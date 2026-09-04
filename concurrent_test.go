package di_test

// A concurrent driver over the same operations as machine_test.go, plus the
// oracles that only mean anything when calls overlap. The sequential machine
// cannot reach these: it never has two goroutines inside the container, so
// every defect that needs one was invisible to it.
//
// Outcomes are not predicted here either, and fewer invariants survive
// concurrency than survive a sequential run. What is checked is:
//
//	C1  No operation panics except with a configuration rejection, or with
//	    one of the errors the package documents.
//	C2  Every operation returns. A driver that hangs is a defect, so each
//	    lane is bounded and the whole run has a deadline.
//	C3  Stop respects scope order: no stop hook of a scope may begin while a
//	    stop hook of one of its descendants is still running. This is the
//	    oracle for a parent and a child being stopped at the same time.
//	C4  Nothing is stopped more often than it was built.
//	C5  A service is built once however many resolutions race for it.
//	C6  No stop hook of an instance begins while that instance's own drain
//	    hook is still running. Drain hooks build into a scope under the one
//	    being drained, which is what puts a late instance where a sweep
//	    above it may still reach it as its own Stop arrives.
//	C7  Drain hooks resolve, so the phase is exercised against a live
//	    registry rather than doing nothing. That the resolution *succeeds*
//	    is deliberately not asserted here: a Stop of the hook's own scope,
//	    issued from another lane, may legitimately stop it mid-hook. The
//	    property belongs to the deterministic tests, which know the phase
//	    boundary instead of guessing at it.
//	C8  One fixed graph, one verdict: two resolutions of the same key from
//	    the same scope never disagree about a cycle or a missing provider.
//
// C6, C7 and C8 exist because the first version of this driver could not see
// four of the six defects of the second September 2026 review. Its drain
// hooks returned nil and touched nothing, so the phase was exercised without
// being checked; and C1 accepted any panicked error, which is how a false
// ErrCycle out of Get reads as a legitimate failure.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floatdrop/di"
)

// ---- the stop-order oracle -------------------------------------------------

// stopOrder records which scopes have a stop hook in flight, so a hook that
// starts while a descendant's is still running can be caught at the moment it
// happens rather than inferred afterwards from an ordering log.
type stopOrder struct {
	mu     sync.Mutex
	live   map[string]int // scope name -> stop hooks currently running
	parent map[string]string
	errs   []string
}

func newStopOrder(parent map[string]string) *stopOrder {
	return &stopOrder{live: map[string]int{}, parent: parent}
}

func (o *stopOrder) enter(scope string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for s, n := range o.live {
		if n > 0 && s != scope && o.descends(s, scope) {
			o.errs = append(o.errs, fmt.Sprintf(
				"a stop hook of %s began while %s, which is under it, was still stopping", scope, s))
		}
	}
	o.live[scope]++
}

func (o *stopOrder) exit(scope string) {
	o.mu.Lock()
	o.live[scope]--
	o.mu.Unlock()
}

// descends reports whether scope is at or below anc.
func (o *stopOrder) descends(scope, anc string) bool {
	for s := scope; s != ""; s = o.parent[s] {
		if s == anc {
			return true
		}
	}
	return false
}

func (o *stopOrder) failures() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.errs...)
}

// ---- the drain oracle ------------------------------------------------------

// drainStage is where one built instance has got to in the drain phase.
type drainStage int

const (
	drainOwed drainStage = iota // the binding has an OnDrain that has not run
	drainRunning
	drainRan
)

// drainWatch tracks that stage per instance, so a stop hook can be caught
// beginning under a drain hook that is still running.
//
// It deliberately does not also fail an OnStop that arrives before any drain
// ran. That looks like the stronger check and is not sound: an instance built
// during the phase, into a scope whose part of the phase has ended, owes no
// drain, and one built into a scope already marked stopped is undone by
// publish without one. Both are documented. The property that every instance
// which owes a drain gets one is pinned deterministically in review2_test.go
// instead, where the phase boundary is known rather than guessed at.
type drainWatch struct {
	mu    sync.Mutex
	stage map[any]drainStage
	errs  []string
}

func newDrainWatch() *drainWatch { return &drainWatch{stage: map[any]drainStage{}} }

func (w *drainWatch) owed(v any) { w.set(v, drainOwed) }

func (w *drainWatch) begin(v any) { w.set(v, drainRunning) }

func (w *drainWatch) end(v any) { w.set(v, drainRan) }

func (w *drainWatch) set(v any, st drainStage) {
	w.mu.Lock()
	w.stage[v] = st
	w.mu.Unlock()
}

// stopping is called at the top of every stop hook: C6.
func (w *drainWatch) stopping(v any, scope string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stage[v] == drainRunning {
		w.errs = append(w.errs, fmt.Sprintf(
			"a stop hook in %s began while the same instance's OnDrain was still running", scope))
	}
}

func (w *drainWatch) failures() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.errs...)
}

// ---- the concurrent machine ------------------------------------------------

// scopeName is registered as a Value in every scope, so a constructor can
// name the scope it is running in. That is the scope that will stop the
// instance, which is not the scope the binding was registered in: a Scoped
// binding declared in the root is held, and torn down, by whichever scope
// resolved it. Labelling a hook by its registration would have the oracle
// blame the wrong scope.
type scopeName struct{ name string }

type cmachine struct {
	t     *testing.T
	ops   []op
	order *stopOrder
	drain *drainWatch
	owner sync.Map // built value -> the name of the scope holding it
	late  sync.Map // built value -> built during the teardown phase

	// tearing marks the teardown phase, so a build that happens inside it can
	// be told from one that came before. See stopOrder.
	tearing atomic.Bool

	scopes     []*di.Scope
	names      []string
	byName     map[string]*di.Scope   // so a hook can resolve from the scope holding its value
	childrenOf map[string][]*di.Scope // and from one below it

	mu       sync.Mutex
	builds   map[string]int
	stops    map[string]int
	verdicts map[string]map[string]bool // scope/key -> the verdicts resolution gave
	fails    []string
}

func newCMachine(t *testing.T, ops []op) *cmachine {
	m := &cmachine{
		t: t, ops: ops,
		builds: map[string]int{}, stops: map[string]int{},
		verdicts: map[string]map[string]bool{},
		order:    newStopOrder(map[string]string{"c1": "root", "c2": "root", "gc": "c1", "request": "root"}),
		drain:    newDrainWatch(),
	}
	root := di.New()
	root.Observe(func(ev di.Event) {
		m.mu.Lock()
		defer m.mu.Unlock()
		id := ev.Scope + "/" + ev.Service
		switch ev.Kind {
		case di.EventBuild:
			if ev.Err == nil {
				m.builds[id]++
			}
		case di.EventStop:
			m.stops[id]++
		}
	})
	c1 := root.Child("c1")
	m.scopes = []*di.Scope{root, c1, root.Child("c2"), c1.Child("gc")}
	m.names = []string{"root", "c1", "c2", "gc"}
	m.byName = map[string]*di.Scope{}
	for i, s := range m.scopes {
		s.Value(scopeName{m.names[i]})
		m.byName[m.names[i]] = s
	}
	m.childrenOf = map[string][]*di.Scope{}
	for i, p := range parentOf {
		if p >= 0 {
			m.childrenOf[m.names[p]] = append(m.childrenOf[m.names[p]], m.scopes[i])
		}
	}
	return m
}

// builtLate reports whether v came into being during the teardown phase. Such
// a value may be undone by publish rather than by a teardown -- the scope was
// stopped by the time its constructor returned -- and that release runs on
// whichever goroutine was resolving, not in any scope's stop order. It is one
// of the three teardowns the package documents as finishing outside Stop, so
// C3 has to model it rather than report it.
func (m *cmachine) builtLate(v any) bool {
	_, ok := m.late.Load(v)
	return ok
}

// holderOf names the scope that built v, defaulting to the root so a value
// the oracle never saw built cannot silently disable the check.
func (m *cmachine) holderOf(v any) string {
	if name, ok := m.owner.Load(v); ok {
		return name.(string)
	}
	return "root"
}

func (m *cmachine) fail(format string, args ...any) {
	m.mu.Lock()
	m.fails = append(m.fails, fmt.Sprintf(format, args...))
	m.mu.Unlock()
}

// call enforces C1: only a configuration rejection may panic.
func (m *cmachine) call(what string, f func()) {
	defer func() {
		switch v := recover().(type) {
		case nil:
		case string:
			if !strings.HasPrefix(v, "di: ") {
				m.fail("%s panicked with an unexpected string: %q", what, v)
			}
		case error:
			// Get reports failure this way at top level, but only with the
			// errors the package documents. Accepting any error is what let
			// two false-ErrCycle defects read as legitimate failures here.
			if !isDocumentedFailure(v) {
				m.fail("%s panicked with an undocumented error: %v", what, v)
			}
		default:
			m.fail("%s panicked with %T: %v", what, v, v)
		}
	}()
	f()
}

// isDocumentedFailure reports whether err is one of the failures the package
// says a resolution can report. A wiring failure that wraps none of them is a
// defect, not a legitimate outcome.
func isDocumentedFailure(err error) bool {
	for _, sentinel := range []error{di.ErrStopped, di.ErrNotProvided, di.ErrCycle, di.ErrUnhealthy} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// register wires the shapes the concurrent driver uses. It stays simpler than
// the sequential machine's ten: what matters here is overlap, not variety, and
// every hook has to cooperate with the stop-order oracle.
func (m *cmachine) register(s *di.Scope, o op) {
	stop := func(_ context.Context, v any) error {
		name := m.holderOf(v)
		m.drain.stopping(v, name) // C6
		if m.builtLate(v) {
			return nil // C3 cannot order a release that no teardown issued
		}
		m.order.enter(name)
		time.Sleep(time.Millisecond) // widen the window a bad ordering needs
		m.order.exit(name)
		return nil
	}
	switch o.key {
	case 0:
		reg(m, s, o, stop, func() *mk1 { return &mk1{} }, func(sc *di.Scope) *mk1 { return &mk1{dep: sc.Get[*mk2]()} })
	case 1:
		reg(m, s, o, stop, func() *mk2 { return &mk2{} }, func(sc *di.Scope) *mk2 { return &mk2{dep: sc.Get[*mk3]()} })
	case 2:
		reg(m, s, o, stop, func() *mk3 { return &mk3{} }, func(sc *di.Scope) *mk3 { return &mk3{dep: sc.Get[*mk1]()} })
	default:
		reg(m, s, o, stop, func() mkI { return &mk1{} }, func(sc *di.Scope) mkI { _ = sc.Get[*mk2](); return &mk1{} })
	}
}

func reg[T any](m *cmachine, s *di.Scope, o op, stop func(context.Context, any) error, plain func() T, dep func(*di.Scope) T) {
	down := func(ctx context.Context, v T) error { return stop(ctx, v) }
	// owe records that this instance's binding declares OnDrain, which is
	// what makes C6 hold it to one.
	owe := func(sc *di.Scope, v T) T {
		m.drain.owed(any(v))
		return v
	}
	// The drain hook of every shape that has one. It records the interval it
	// occupies (C6), resolves from the scope holding the value, which the
	// phase promises still works (C7), and resolves its own key from a scope
	// below that one, which is what can put a late instance somewhere the
	// sweep has already been.
	drainHook := func(_ context.Context, v T) error {
		m.drain.begin(any(v))
		holder := m.holderOf(any(v))
		m.resolveDuringDrain(holder)
		m.buildIntoChild(holder, o.key)
		time.Sleep(time.Millisecond) // widen the window a bad ordering needs
		m.drain.end(any(v))
		return nil
	}
	// own records which scope the constructor ran in, which for every
	// lifetime is the scope that holds the instance and will stop it.
	own := func(sc *di.Scope, v T) T {
		m.owner.Store(any(v), sc.Get[scopeName]().name)
		if m.tearing.Load() {
			m.late.Store(any(v), true)
		}
		return v
	}
	build := func(sc *di.Scope) T { return own(sc, plain()) }
	switch o.reg % 6 {
	case 0:
		s.Provide(build).OnStop(down)
	case 1:
		s.Provide(build).Scoped().OnStop(down)
	case 2:
		s.Provide(func(sc *di.Scope) T { return own(sc, dep(sc)) }).OnStop(down)
	case 3:
		// OnDrain stays out of the stop-order oracle, because draining is not
		// ordered against a concurrently stopping sibling: it releases
		// nothing. It has its own two oracles instead. The hook records the
		// interval it occupies, so a stop hook of the same instance can be
		// caught inside it (C6), and it resolves from the scope holding the
		// value, which the phase promises still works (C7). A hook that
		// returned nil and touched nothing is how three drain defects
		// survived this driver.
		s.Provide(func(sc *di.Scope) T { return owe(sc, build(sc)) }).
			OnDrain(drainHook).
			Run(func(ctx context.Context, _ T) error { <-ctx.Done(); return nil }).
			OnStop(down)
	case 4:
		s.Provide(build).
			OnStart(func(context.Context, T) error { return nil }).
			OnStop(down)
	default:
		// Scoped *and* draining. Without this shape the driver could not put
		// a drain-owing instance into a child scope at all: every other
		// OnDrain shape is a plain singleton, so resolving it from a child
		// hands back the instance the owner already holds. A whole class of
		// drain defect was unreachable for want of one registration.
		s.Provide(func(sc *di.Scope) T { return owe(sc, build(sc)) }).Scoped().
			OnDrain(drainHook).
			OnStop(down)
	}
}

// resolveDuringDrain is C7: it puts a resolution inside the drain phase, so
// the phase runs against a live registry under -race instead of returning nil
// and touching nothing, which is how three drain defects survived this driver.
//
// The outcome is not asserted, and cannot be. The property is that a scope
// still resolves while it is being drained, but a Stop of that scope from
// another lane may have moved past draining by the time the hook reaches this
// call, and then ErrStopped is the honest answer. What the phase guarantees
// about resolution is pinned by TestReview2AncestorStopWaitsForIndependentChildDrain
// and TestReview2LateChildDrainCanResolve, where there is a known boundary to
// check against.
func (m *cmachine) resolveDuringDrain(name string) {
	if sc := m.byName[name]; sc != nil {
		_, _ = sc.Resolve[scopeName]()
	}
}

// buildIntoChild resolves a key from a scope below the one being drained, so
// that an instance can first come into being in a scope the sweep has already
// visited. That is a different shape from a late build in the scope doing the
// draining, and the phase owes it a drain just the same.
//
// The outcome is deliberately not checked: this is a generator, not an
// oracle. A child whose own Stop has already finished legitimately refuses to
// resolve, and whether the key has a provider or a cycle is a property of the
// random wiring. C6 is what turns the shape into a verdict: the key is the
// drained binding's own, so a Scoped one yields a new instance in the child.
func (m *cmachine) buildIntoChild(name string, key uint8) {
	for _, sc := range m.childrenOf[name] {
		switch key {
		case 0:
			_, _ = sc.Resolve[*mk1]()
		case 1:
			_, _ = sc.Resolve[*mk2]()
		case 2:
			_, _ = sc.Resolve[*mk3]()
		default:
			_, _ = sc.Resolve[mkI]()
		}
	}
}

func (m *cmachine) resolve(s *di.Scope, o op) {
	var err error
	switch o.key {
	case 0:
		_, err = s.Resolve[*mk1]()
	case 1:
		_, err = s.Resolve[*mk2]()
	case 2:
		_, err = s.Resolve[*mk3]()
	default:
		_, err = s.Resolve[mkI]()
	}
	m.verdict(o, err)
}

// verdict records how one key resolved, for C8. Every registration happens
// before any resolution, so the graph is fixed by the time this runs and the
// answer is a property of the wiring: two resolutions of the same key from
// the same scope must not disagree. ErrStopped is left out because it is the
// one answer that legitimately depends on when the call was made.
func (m *cmachine) verdict(o op, err error) {
	var v string
	switch {
	case err == nil:
		v = "ok"
	case errors.Is(err, di.ErrStopped):
		return
	case errors.Is(err, di.ErrCycle):
		v = "a cycle"
	case errors.Is(err, di.ErrNotProvided):
		v = "no provider"
	default:
		v = "an undocumented error: " + err.Error()
	}
	id := fmt.Sprintf("%s/key%d", m.names[o.scope], o.key)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.verdicts[id] == nil {
		m.verdicts[id] = map[string]bool{}
	}
	m.verdicts[id][v] = true
}

func (m *cmachine) step(i int, o op) {
	s := m.scopes[o.scope]
	label := fmt.Sprintf("op %d %v", i, o)
	m.call(label, func() {
		switch o.kind {
		case opRegister:
			m.register(s, o)
		case opStart:
			_ = s.Start(context.Background())
		case opStop:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.Stop(ctx)
		case opHealth:
			_ = s.HealthCheck(context.Background())
		default:
			m.resolve(s, o)
		}
	})
}

// run executes the sequence in three phases: wiring sequentially, then the
// resolutions in parallel lanes, then the lifecycle calls in parallel lanes.
//
// The phases are what give the oracles something to work with. Racing the
// registrations only exercises the freeze path, which the sequential machine
// already covers. And a Stop that races a Resolve of a scope holding nothing
// tears down an empty scope, so the interesting orderings only exist once the
// resolutions have run: separating them is what lets two overlapping Stop
// calls actually have hooks to get wrong.
func (m *cmachine) run() {
	var wired, warm, up, down []op
	for _, o := range m.ops {
		switch o.kind {
		case opRegister:
			wired = append(wired, o)
		case opStart, opHealth:
			up = append(up, o)
		case opStop:
			down = append(down, o)
		default:
			warm = append(warm, o)
		}
	}
	for i, o := range wired {
		m.step(i, o)
	}

	// Stops go in their own phase, after the starts. A start step that is
	// still in flight when Stop arrives is handed to the goroutine running
	// it and may finish just after Stop returns, which is documented and
	// deliberate but is indistinguishable, from outside, from a parent
	// running ahead of its child. Keeping the two apart lets C3 be checked
	// without an exemption that would also excuse the real defect. Start
	// racing Stop has its own regression tests.
	//
	// Every scope is stopped at the end anyway, so a sequence that generated
	// no Stop still gets one overlapping pair to check.
	if len(down) == 0 {
		down = []op{{kind: opStop, scope: 1}, {kind: opStop, scope: 0}}
	}

	for _, phase := range [][]op{warm, up} {
		m.parallel(phase)
	}
	m.tearing.Store(true)
	m.parallel(down)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m.call("final Stop", func() { _ = m.scopes[0].Stop(ctx) })

	m.check()
}

// parallel runs one phase's operations in lanes released together, so they
// overlap instead of interleaving one at a time.
func (m *cmachine) parallel(ops []op) {
	const lanes = 4
	start := make(chan struct{})
	done := make(chan struct{})
	var wg sync.WaitGroup
	for lane := range lanes {
		wg.Go(func() {
			<-start
			for i, o := range ops {
				if i%lanes == lane {
					m.step(i, o)
				}
			}
		})
	}
	close(start)
	go func() { wg.Wait(); close(done) }()

	// C2: an operation that never returns is a defect, not a slow test.
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		m.t.Fatalf("a concurrent operation never returned\n  sequence: %v", m.ops)
	}
}

func (m *cmachine) check() {

	for _, msg := range m.order.failures() { // C3
		m.fail("%s", msg)
	}
	for _, msg := range m.drain.failures() { // C6
		m.fail("%s", msg)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, n := range m.stops { // C4
		if n > m.builds[id] {
			m.fails = append(m.fails, fmt.Sprintf("%s: stopped %d times but built %d", id, n, m.builds[id]))
		}
	}
	for id, seen := range m.verdicts { // C8
		if len(seen) > 1 {
			m.fails = append(m.fails, fmt.Sprintf("%s resolved to %s in one run", id, strings.Join(sortedKeys(seen), " and to ")))
		}
	}
	if len(m.fails) > 0 {
		m.t.Fatalf("%s\n  sequence: %v", strings.Join(m.fails, "\n  "), m.ops)
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestMachineConcurrent runs the operation sequences with the lanes
// overlapping, under -race.
func TestMachineConcurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("concurrent sweep")
	}
	rng := rand.New(rand.NewPCG(0x5EED, 0xFACE))
	for iter := range 400 {
		n := 2 + rng.IntN(10)
		data := make([]byte, n*5)
		for i := range data {
			data[i] = byte(rng.UintN(256))
		}
		ops := decode(data)
		t.Run(fmt.Sprintf("iter%d", iter), func(t *testing.T) { newCMachine(t, ops).run() })
		if t.Failed() {
			t.Fatalf("failing sequence: %v", ops)
		}
	}
}

// The shapes worth seeding by hand, because a random sequence reaches them
// rarely and they are where the concurrency defects lived.
func TestMachineConcurrentSeeds(t *testing.T) {
	seeds := [][]byte{
		{0, 1, 0, 1, 0, 1, 3, 0, 0, 0, 6, 1, 0, 0, 0, 6, 0, 0, 0, 0},    // scoped in gc, then stop c1 and root
		{0, 0, 0, 2, 1, 5, 0, 0, 0, 0, 1, 1, 0, 0, 0, 6, 0, 0, 0, 0},    // start racing a resolve from a child
		{0, 0, 0, 3, 1, 5, 0, 0, 0, 0, 6, 1, 0, 0, 0, 6, 0, 0, 0, 0},    // a Run hook, then overlapping stops
		{0, 0, 0, 4, 1, 1, 3, 0, 0, 0, 1, 1, 0, 0, 0, 5, 0, 0, 0, 0},    // late build racing Start's hook phase
		{0, 3, 0, 0, 0, 1, 3, 0, 0, 0, 6, 3, 0, 0, 0, 6, 1, 0, 0, 0, 6}, // stop the grandchild and its ancestors
		{0, 1, 0, 3, 0, 1, 1, 0, 0, 0, 6, 1, 0, 0, 0, 6, 0, 0, 0, 0},    // a drain hook in c1, then c1 and root stopped at once
		{0, 3, 1, 3, 0, 1, 3, 1, 0, 0, 6, 3, 0, 0, 0, 6, 0, 0, 0, 0},    // the same in the grandchild, stopped against the root
	}
	for i, data := range seeds {
		// Repeated, because the orderings these seeds exist for are races:
		// one run of the shape proves nothing about the interleaving.
		t.Run(fmt.Sprintf("seed%d", i), func(t *testing.T) {
			for range 20 {
				newCMachine(t, decode(data)).run()
				if t.Failed() {
					return
				}
			}
		})
	}
}

// FuzzMachineConcurrent is the coverage-guided driver for the same oracles.
func FuzzMachineConcurrent(f *testing.F) {
	f.Add([]byte{0, 1, 0, 1, 0, 1, 3, 0, 0, 0, 6, 1, 0, 0, 0, 6, 0, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 3, 1, 5, 0, 0, 0, 0, 6, 1, 0, 0, 0, 6, 0, 0, 0, 0})
	f.Add([]byte{0, 1, 0, 3, 0, 1, 1, 0, 0, 0, 6, 1, 0, 0, 0, 6, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		ops := decode(data)
		if len(ops) == 0 {
			return
		}
		newCMachine(t, ops).run()
	})
}

// ---- targeted concurrency properties ---------------------------------------

// A stop hook must never overlap with one from a scope beneath it, however
// the two Stop calls were issued. This is the oracle of C3 on the shape it
// exists for: a request scope ending exactly as the application shuts down.
func TestConcurrentStopKeepsScopeOrder(t *testing.T) {
	for range 200 {
		order := newStopOrder(map[string]string{"request": "root"})
		root := di.New()
		root.Value(&DB{}).OnStop(func(context.Context, *DB) error {
			order.enter("root")
			time.Sleep(200 * time.Microsecond)
			order.exit("root")
			return nil
		})
		root.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} }).Scoped().
			OnStop(func(context.Context, *Repo) error {
				order.enter("request")
				time.Sleep(200 * time.Microsecond)
				order.exit("request")
				return nil
			})
		req := root.Child("request")
		req.Get[*Repo]()

		var wg sync.WaitGroup
		wg.Go(func() { _ = req.Stop(context.Background()) })
		wg.Go(func() { _ = root.Stop(context.Background()) })
		wg.Wait()

		if msgs := order.failures(); len(msgs) > 0 {
			t.Fatal(strings.Join(msgs, "\n"))
		}
	}
}

// C5, and the guarantee the README makes: a singleton is built once however
// many goroutines race for it, including goroutines a constructor started.
func TestConcurrentResolveFromConstructorGoroutines(t *testing.T) {
	var deep, wide atomic.Int32
	s := di.New()
	s.Provide(func(*di.Scope) *mk3 { deep.Add(1); return &mk3{} })
	s.Provide(func(sc *di.Scope) *mk2 {
		wide.Add(1)
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				if _, err := sc.Resolve[*mk3](); err != nil {
					t.Errorf("nested resolve: %v", err)
				}
			})
		}
		wg.Wait()
		return &mk2{}
	})

	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if _, err := s.Resolve[*mk2](); err != nil {
				t.Errorf("resolve: %v", err)
			}
		})
	}
	wg.Wait()
	if deep.Load() != 1 || wide.Load() != 1 {
		t.Fatalf("built %d and %d times, want one each", wide.Load(), deep.Load())
	}
}

// Concurrent resolutions of a graph with a cycle must all report it, and none
// may hang, whichever end each one enters from.
func TestConcurrentCycleAlwaysReports(t *testing.T) {
	for range 100 {
		s := di.New()
		s.Provide(func(sc *di.Scope) *mk1 { return &mk1{dep: sc.Get[*mk2]()} })
		s.Provide(func(sc *di.Scope) *mk2 { return &mk2{dep: sc.Get[*mk1]()} })

		errs := make([]error, 8)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Go(func() {
				if i%2 == 0 {
					_, errs[i] = s.Resolve[*mk1]()
				} else {
					_, errs[i] = s.Resolve[*mk2]()
				}
			})
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent resolutions of a cycle deadlocked")
		}
		for i, err := range errs {
			if err == nil {
				t.Fatalf("resolution %d returned a value from a cyclic graph", i)
			}
		}
	}
}
