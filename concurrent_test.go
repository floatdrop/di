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
//	C9  Every instance that owes a stop step gets exactly one, by the time
//	    everything has gone quiet. This is the oracle for a release dropped
//	    on a path nobody returns from -- a missed deadline, a scope that
//	    detached -- which C4, counting only excess stops, cannot see.
//	C10 A resolution begun after its scope's Stop has returned fails. Begun,
//	    not finished: a resolution already in flight may legitimately return
//	    a value decided before the scope stopped.
//	C11 A Stop reports the failure of a drain hook of its own scope, whether
//	    it ran that hook or waited for another Stop that owned the phase.
//	    Until the fourth review, every drain hook here returned nil, so a
//	    scope whose Stop swallowed its own hook's failure looked identical
//	    to one that had nothing to report.
//
// C6, C7 and C8 exist because the first version of this driver could not see
// four of the six defects of the second September 2026 review. Its drain
// hooks returned nil and touched nothing, so the phase was exercised without
// being checked; and C1 accepted any panicked error, which is how a false
// ErrCycle out of Get reads as a legitimate failure.
//
// C9 and C10 exist because the third review found two more the same way. The
// coverage the generators reached was 78% against the suite's 97%, and the
// whole gap was the lifecycle: no Worker hook, no Shutdown, no context expiring
// mid-Stop, and starts kept strictly before stops. Everything the reviews
// found lived in that gap. It is closed here.

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
// which owes a drain gets one is pinned deterministically in drain_test.go
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

	// inHook counts the user hooks currently running, so the run can wait for
	// quiescence before the oracles that only hold once everything the
	// container still owed has happened. A release deferred past a missed
	// deadline finishes on a goroutine of its own, after Stop returned --
	// which is also why this is a counter and not a WaitGroup: such a hook
	// starts after the run has begun waiting, and a WaitGroup may not have
	// Add called on a zero counter concurrently with Wait.
	inHook atomic.Int32

	// impatient records the scopes where a Stop reported a missed deadline.
	// Both ordering oracles are switched off for those: the releases such a
	// Stop leaves behind run outside its ordering by design, and an oracle
	// that cannot tell them apart from a defect must not guess.
	impatient sync.Map // scope name -> struct{}
	// stopReturned records the scopes whose Stop has come back, for C10.
	stopReturned sync.Map // scope name -> struct{}

	// drainFailed records the scopes whose own drain hook returned an error;
	// stopReports, below, holds what every patient Stop of a scope returned.
	// C11 is the two compared.
	drainFailed sync.Map // scope name -> error

	// sched, when set, decides the order the hooks and operations run in.
	// Nil is the ordinary driver, where the Go scheduler decides.
	sched *scheduler

	// owed holds every instance whose binding declares an OnStop and no
	// OnStart, so that being built is the whole of owing a release; released
	// counts the releases that actually ran, per instance. C9 is the two
	// compared once everything has gone quiet.
	owed sync.Map // built value -> the name of the scope that owes its release

	mu          sync.Mutex
	builds      map[string]int
	starts      map[string]int
	stops       map[string]int
	released    map[any]int
	stopReports map[string][]error // scope -> what each patient Stop of it returned
	// drainAt and stopAt order the two against each other, which is what a
	// C11 failure has to be read against: a Stop that returned before the
	// hook began is a different story from one that returned after it.
	drainAt  map[string]int   // scope -> when its failing drain hook began
	stopAt   map[string][]int // scope -> when each patient Stop of it returned
	clock    int
	verdicts map[string]map[string]bool // scope/key -> the verdicts resolution gave
	fails    []string
}

func newCMachine(t *testing.T, ops []op) *cmachine {
	m := &cmachine{
		t: t, ops: ops,
		builds: map[string]int{}, starts: map[string]int{}, stops: map[string]int{},
		released:    map[any]int{},
		stopReports: map[string][]error{},
		drainAt:     map[string]int{},
		stopAt:      map[string][]int{},
		verdicts:    map[string]map[string]bool{},
		order:       newStopOrder(map[string]string{"c1": "root", "c2": "root", "gc": "c1", "request": "root"}),
		drain:       newDrainWatch(),
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
		case di.EventStart:
			if ev.Err == nil {
				m.starts[id]++
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

// unordered reports whether the teardown of anything in scope is outside the
// ordering the oracles check, because a Stop there, or above it, reported a
// missed deadline. Such a Stop finishes its releases on goroutines of its
// own, after it has returned; that is documented, and an ordering oracle that
// modelled it would be modelling the clock.
func (m *cmachine) unordered(scope string) bool {
	for s := scope; s != ""; s = m.order.parent[s] {
		if _, ok := m.impatient.Load(s); ok {
			return true
		}
	}
	return false
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
	for _, sentinel := range []error{di.ErrStopped, di.ErrNotProvided, di.ErrCycle} {
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
		m.inHook.Add(1)
		defer m.inHook.Add(-1)
		name := m.holderOf(v)
		m.sched.pause("OnStop in " + name)
		defer m.sched.pause("OnStop returns in " + name)
		m.mu.Lock()
		m.released[v]++ // C9: counted before any check can bail out
		m.mu.Unlock()
		// C6 holds however impatient the Stop was. A missed deadline defers
		// a release; it never runs one early, and the release still waits
		// for the drain hook that holds the value. Exempting it here along
		// with the scope ordering, which a deferred release really is
		// outside of, switched this check off for the one shape that needs
		// it: an ancestor's sweep inside the hook as the holder's own Stop
		// gives up on waiting.
		m.drain.stopping(v, name)
		if m.builtLate(v) || m.unordered(name) {
			return nil // C3 cannot order a release no Stop issued in order
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

// errWorker is a Worker hook failing of its own accord rather than because it
// was cancelled, which is what makes the container hand it to Shutdown. A
// worker that only ever returns nil leaves that whole path unreached.
var errWorker = errors.New("worker failed")

// errDrain is a drain hook reporting that it could not finish its work, which
// its own scope's Stop has to pass on.
var errDrain = errors.New("drain failed")

func reg[T any](m *cmachine, s *di.Scope, o op, stop func(context.Context, any) error, plain func() T, dep func(*di.Scope) T) {
	down := func(ctx context.Context, v T) error { return stop(ctx, v) }
	// Every shape gets a worker, so that any instance the sequence starts is
	// one Stop has to cancel and wait for. Only the shapes with a worker
	// reach the release that finishes after a missed deadline, and while one
	// shape in six had one, the sequence had to put that shape, a Start and
	// an impatient Stop together before anything was exercised at all.
	work := func(ctx context.Context, _ T) error {
		<-ctx.Done()
		m.sched.pause("worker cancelled")
		time.Sleep(2 * time.Millisecond) // outlast an impatient Stop
		if o.reg%3 == 0 {
			return errWorker // its own failure, not the cancellation
		}
		return nil
	}
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
	drainHook := func(ctx context.Context, v T) error {
		m.inHook.Add(1)
		defer m.inHook.Add(-1)
		m.drain.begin(any(v))
		holder := m.holderOf(any(v))
		m.sched.pause("OnDrain in " + holder)
		defer m.sched.pause("OnDrain returns in " + holder)
		m.resolveDuringDrain(holder)
		m.buildIntoChild(holder, o.key)
		time.Sleep(2 * time.Millisecond) // widen the window a bad ordering needs
		m.drain.end(any(v))
		if o.key%2 == 1 {
			// Half the draining shapes fail. A hook that always returns nil
			// exercises the phase without checking what it does with an
			// error, which is how a child Stop that dropped its own hook's
			// failure survived three drivers.
			m.mu.Lock()
			m.clock++
			m.drainAt[holder] = m.clock
			m.mu.Unlock()
			if !m.builtLate(any(v)) {
				// C11 holds only for an instance that existed before the
				// teardown began. One built during the phase can be drained
				// by a sweep running above a scope whose own Stop is already
				// past its drain, and that Stop has nowhere to put the
				// error: its phase was over before the instance existed.
				// The same exemption C3 needs, for the same reason.
				m.drainFailed.Store(holder, errDrain)
			}
			return errDrain
		}
		return nil
	}
	// own records which scope the constructor ran in, which for every
	// lifetime is the scope that holds the instance and will stop it, and
	// what that instance owes: for a shape with no OnStart, "built" and
	// "owes a release" are the same thing, so C9 holds it to exactly one
	// stop step from here. The shape that has one records itself in the
	// hook, once the start step has actually succeeded.
	own := func(sc *di.Scope, v T) T {
		name := sc.Get[scopeName]().name
		m.owner.Store(any(v), name)
		if m.tearing.Load() {
			m.late.Store(any(v), true)
		}
		if o.reg%6 != 4 {
			m.owed.Store(any(v), name)
		}
		return v
	}
	build := func(sc *di.Scope) T { return own(sc, plain()) }
	switch o.reg % 6 {
	case 0:
		s.Provide(build).Worker(work).OnStop(down)
	case 1:
		s.Provide(build).Scoped().Worker(work).OnStop(down)
	case 2:
		s.Provide(func(sc *di.Scope) T { return own(sc, dep(sc)) }).
			Worker(work).
			OnStop(func(ctx context.Context, v T) error {
				// Slow enough that an impatient Stop misses its deadline
				// here, which is the only way this driver reaches the
				// release that finishes after Stop has returned.
				time.Sleep(2 * time.Millisecond)
				return down(ctx, v)
			})
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
			Worker(work).
			OnStop(down)
	case 4:
		// The one shape with an OnStart, so its stop step is owed only when
		// the start step succeeded. Its Worker hook is what puts a worker under
		// a Stop that has to cancel it and wait.
		s.Provide(build).
			OnStart(func(_ context.Context, v T) error {
				// The one shape whose release is owed only once the start
				// step has succeeded, so the hook itself is what tells C9.
				m.owed.Store(any(v), m.holderOf(any(v)))
				return nil
			}).
			Worker(work).
			OnStop(down)
	default:
		// Scoped *and* draining. Without this shape the driver could not put
		// a drain-owing instance into a child scope at all: every other
		// OnDrain shape is a plain singleton, so resolving it from a child
		// hands back the instance the owner already holds. A whole class of
		// drain defect was unreachable for want of one registration.
		s.Provide(func(sc *di.Scope) T { return owe(sc, build(sc)) }).Scoped().
			OnDrain(drainHook).
			Worker(work).
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
	// C10 is decided here, before the call: a resolution already in flight
	// may return a value the container settled on before the scope stopped,
	// but one begun afterwards has nothing legitimate to hand back.
	dead := m.stoppedBefore(m.names[o.scope])
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
	if dead && err == nil {
		m.fail("%s resolved from %s after its Stop had returned", keyNames[o.key], m.names[o.scope])
	}
	m.verdict(o, err)
}

// stoppedBefore reports whether a Stop of this scope, or of one above it, had
// already returned when the caller looked.
func (m *cmachine) stoppedBefore(scope string) bool {
	for s := scope; s != ""; s = m.order.parent[s] {
		if _, ok := m.stopReturned.Load(s); ok {
			return true
		}
	}
	return false
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
	m.sched.pause(label)
	// Rendering runs in the lane with everything else, so it reads the
	// phase machine while other lanes are writing it. That is what -race
	// is for here, and it is also the only way a generator reaches an
	// instance rendered mid-build or mid-start.
	defer m.render(label, s)
	m.call(label, func() {
		switch o.kind {
		case opRegister:
			m.register(s, o)
		case opStart:
			_ = s.Start(context.Background())
		case opStop:
			// An impatient Stop cannot finish the hooks it starts, which is
			// how the driver reaches the release that outlives its caller.
			// The scope is marked before the call, not after: the releases
			// that Stop leaves behind may begin while it is still running.
			d := 5 * time.Second
			if o.eager {
				// Two flavours, because they reach different code. A context
				// that has already expired makes every wait inside Stop take
				// its deadline branch, which is the only way this driver
				// reaches the release that finishes after Stop returned. A
				// very short one races the hooks instead.
				d = time.Millisecond
				if o.key%2 == 0 {
					d = 0
				}
				m.impatient.Store(m.names[o.scope], struct{}{})
			}
			ctx, cancel := context.WithTimeout(context.Background(), d)
			defer cancel()
			err := s.Stop(ctx)
			m.stopReturned.Store(m.names[o.scope], struct{}{})
			if !o.eager {
				// Only a patient Stop is held to C11: one that ran out of
				// context may return the missed deadline instead of what the
				// phase went on to report.
				m.mu.Lock()
				m.clock++
				m.stopReports[m.names[o.scope]] = append(m.stopReports[m.names[o.scope]], err)
				m.stopAt[m.names[o.scope]] = append(m.stopAt[m.names[o.scope]], m.clock)
				m.mu.Unlock()
			}
			if err != nil && !o.eager && !isDocumentedFailure(err) &&
				!errors.Is(err, context.DeadlineExceeded) &&
				!errors.Is(err, errWorker) && !errors.Is(err, errDrain) {
				m.fail("%s: Stop reported an undocumented failure: %v", label, err)
			}
		case opShutdown:
			s.Shutdown(errShutdown)
		case opRun:
			// Run with a context that is already cancelled: it starts the
			// scope and stops it again. Without this case the op fell through
			// to the default and quietly resolved instead, so the driver
			// never ran Run at all while its sequences said it did.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_ = s.Run(ctx, di.StopTimeout(5*time.Second))
			m.stopReturned.Store(m.names[o.scope], struct{}{})
		default:
			m.resolve(s, o)
		}
	})
}

// render reads the graph while the rest of the lanes are changing it. A
// configuration rejection is legitimate, since Explain commits the pending
// batch the way a resolution does; nothing else is.
func (m *cmachine) render(label string, s *di.Scope) {
	defer func() {
		if r := recover(); r != nil {
			if _, rejected := r.(string); !rejected {
				m.fail("%s: rendering panicked with %v", label, r)
			}
		}
	}()
	if g := s.Graph(); !strings.HasPrefix(g, "digraph di {") {
		m.fail("%s: Graph rendered %q", label, g)
	}
	_ = s.Explain[*mk1]()
	_ = s.Explain[mkI]()
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
		case opStart, opShutdown, opRun:
			// Run belongs with the lifecycle calls, not with the resolutions.
			// It fell to the default and was classified as one, so it ran
			// before the teardown phase was marked -- and the instances its
			// drain hooks built were then not recorded as late, which is what
			// C11's exemption rests on.
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

	// Starts and stops now run in one phase, overlapping. They were kept
	// apart while a start step in flight was handed to the goroutine running
	// it: that teardown finished after Stop returned and was, from outside,
	// indistinguishable from a parent running ahead of its child. Stop waits
	// for the step now, so the ordering it promises holds even when the two
	// race, and this is the interleaving three reviews' worth of defects
	// lived in.
	//
	// Every scope is stopped at the end anyway, so a sequence that generated
	// no Stop still gets one overlapping pair to check. The child's is the
	// impatient flavour: a Stop that runs out of context while an ancestor's
	// sweep is inside a drain hook of one of its instances is the shape that
	// defers a release, and waiting for a random sequence to produce both
	// halves left that path unreached.
	if len(down) == 0 {
		down = []op{{kind: opStop, scope: 1, eager: true, key: 1}, {kind: opStop, scope: 0}}
	}

	m.parallel(warm)
	m.tearing.Store(true)
	m.parallel(append(up, down...))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m.call("final Stop", func() {
		err := m.scopes[0].Stop(ctx)
		m.mu.Lock()
		m.stopReports["root"] = append(m.stopReports["root"], err)
		m.mu.Unlock()
	})
	for _, name := range m.names {
		m.stopReturned.Store(name, struct{}{})
	}

	// C9 and C4 only hold once what the container still owed has happened: a
	// release whose Stop ran out of context finishes on a goroutine of its
	// own, afterwards. Waiting for the hooks is what makes "afterwards"
	// something the oracles can stand on.
	// Nothing may be parked once the operations are done: the releases a
	// missed deadline deferred still have to run, and the oracles are about
	// to read what they did.
	m.sched.close()
	m.settle()
	m.check()
}

// settle waits for the container to go quiet: every hook returned, and every
// release it still owed has happened.
//
// The second half cannot be a WaitGroup. A release deferred past a missed
// deadline is issued by a goroutine that first waits for the Worker hook to
// return, so between that hook finishing and the release starting there is a
// moment when no hook is running and the work is still owed. Polling for the
// owed set to empty is what closes that gap, and it keeps C9 to the property
// that actually holds -- every owed release happens *eventually* -- rather
// than to a deadline of the driver's own invention. A release that never
// comes still fails, just after this waits for it.
func (m *cmachine) settle() {
	deadline := time.Now().Add(10 * time.Second)
	for {
		if m.inHook.Load() == 0 && m.owedButUnreleased() == 0 {
			return
		}
		if time.Now().After(deadline) {
			return // C9 reports what is still owed, with the sequence
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (m *cmachine) owedButUnreleased() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	m.owed.Range(func(v, _ any) bool {
		if m.released[v] == 0 {
			n++
		}
		return true
	})
	return n
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
	m.owed.Range(func(v, scope any) bool { // C9
		switch n := m.released[v]; {
		case n == 0:
			m.fails = append(m.fails, fmt.Sprintf(
				"an instance held by %s owed a stop step and never got one", scope))
		case n > 1:
			m.fails = append(m.fails, fmt.Sprintf(
				"an instance held by %s was released %d times", scope, n))
		}
		return true
	})
	m.drainFailed.Range(func(scope, want any) bool { // C11
		for i, got := range m.stopReports[scope.(string)] {
			if errors.Is(got, context.DeadlineExceeded) {
				// A Stop that ran out of context reports that, and what the
				// phase went on to conclude is not its answer. The impatient
				// flavour is not the only way to get here: a patient Stop can
				// time out too, waiting on a phase another Stop is holding
				// while a hook of its own resolves.
				continue
			}
			if !errors.Is(got, want.(error)) {
				m.fails = append(m.fails, fmt.Sprintf(
					"a drain hook of %s failed and Stop #%d of that scope reported %v\n    all of them: %v\n    the hook began at %d, those Stops returned at %v",
					scope, i, got, m.stopReports[scope.(string)], m.drainAt[scope.(string)], m.stopAt[scope.(string)]))
			}
		}
		return true
	})
	for id, seen := range m.verdicts { // C8
		if len(seen) > 1 {
			m.fails = append(m.fails, fmt.Sprintf("%s resolved to %s in one run", id, strings.Join(sortedKeys(seen), " and to ")))
		}
	}
	if len(m.fails) > 0 {
		m.t.Fatalf("%s\n  sequence: %v\n  %s", strings.Join(m.fails, "\n  "), m.ops, m.sched.history())
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
		{0, 0, 0, 3, 1, 5, 0, 0, 0, 0, 6, 1, 0, 0, 0, 6, 0, 0, 0, 0},    // a Worker hook, then overlapping stops
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

// The shapes that need saying outright. A byte seed is a blunt instrument for
// reaching a particular interleaving -- it has to survive four modulos -- and
// the coverage map after the third review showed which paths no random
// sequence was reaching at all. These build the operations directly, and run
// them through the same driver, so every oracle is live rather than only the
// assertions a targeted test happens to make.
func TestMachineConcurrentShapes(t *testing.T) {
	shapes := []struct {
		name string
		ops  []op
	}{{
		// A Scoped, draining instance held by a child, with the parent's
		// sweep inside its drain hook as the child's own Stop runs out of
		// context. That is the release stopIfNeeded defers to a goroutine of
		// its own, and the one the third review found dropped.
		name: "impatient child Stop under the parent's drain",
		ops: []op{
			{kind: opRegister, scope: 0, key: 0, reg: 5},
			{kind: opResolve, scope: 1, key: 0},
			{kind: opStop, scope: 1, key: 0, eager: true},
			{kind: opStop, scope: 0},
		},
	}, {
		// The same, one level deeper, so the sweep that owns the phase is two
		// scopes above the one that gives up on it.
		name: "impatient grandchild Stop under the root's drain",
		ops: []op{
			{kind: opRegister, scope: 0, key: 1, reg: 5},
			{kind: opResolve, scope: 3, key: 1},
			{kind: opStop, scope: 3, key: 0, eager: true},
			{kind: opStop, scope: 0},
		},
	}, {
		// A started worker cancelled by a Stop that cannot wait for it, which
		// is the release that follows the hook's own return.
		name: "impatient Stop of a running worker",
		ops: []op{
			{kind: opRegister, scope: 0, key: 2, reg: 4},
			{kind: opResolve, scope: 0, key: 2},
			{kind: opStart, scope: 0},
			{kind: opStop, scope: 0, key: 0, eager: true},
		},
	}}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			for range 40 {
				newCMachine(t, sh.ops).run()
				if t.Failed() {
					return
				}
			}
		})
	}
}

// TestMachineScheduled runs the driver with the interleaving as an input
// rather than as a matter of luck. Each sequence is replayed under a series
// of seeds, and every oracle is live throughout: what changes is only which
// parked goroutine goes next.
//
// The shapes are the ones whose orderings matter -- two Stops meeting at one
// scope, an impatient Stop under an ancestor's drain, a worker being
// cancelled while its scope is torn down -- because a schedule is only worth
// exploring where there is something to order.
func TestMachineScheduled(t *testing.T) {
	if testing.Short() {
		t.Skip("scheduled sweep")
	}
	shapes := [][]op{{
		{kind: opRegister, scope: 0, key: 0, reg: 5},
		{kind: opRegister, scope: 1, key: 1, reg: 3},
		{kind: opResolve, scope: 3, key: 0},
		{kind: opResolve, scope: 1, key: 1},
		{kind: opStop, scope: 1},
		{kind: opStop, scope: 0},
	}, {
		{kind: opRegister, scope: 0, key: 2, reg: 4},
		{kind: opResolve, scope: 0, key: 2},
		{kind: opStart, scope: 0},
		{kind: opStop, scope: 1, key: 0, eager: true},
		{kind: opStop, scope: 0},
	}, {
		{kind: opRegister, scope: 1, key: 0, reg: 3},
		{kind: opRegister, scope: 0, key: 1, reg: 5},
		{kind: opResolve, scope: 1, key: 0},
		{kind: opResolve, scope: 3, key: 1},
		{kind: opStart, scope: 1},
		{kind: opStop, scope: 3, key: 0, eager: true},
		{kind: opStop, scope: 1},
		{kind: opStop, scope: 0},
	}}
	for i, ops := range shapes {
		t.Run(fmt.Sprintf("shape%d", i), func(t *testing.T) {
			for seed := range 25 {
				m := newCMachine(t, ops)
				m.sched = newScheduler(uint64(seed))
				m.run()
				m.sched.close()
				if t.Failed() {
					t.Fatalf("failing seed: %d", seed)
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

// A Stop whose context is already spent still owes every release, and the
// coverage map says a random sequence reaches this shape rarely: the drain of
// one instance has to be running, under another Stop, exactly as an impatient
// Stop of the scope holding it arrives. That is the wait stopIfNeeded gives
// up on and hands to a goroutine of its own, and the release it defers is the
// one the third review found dropped.
func TestConcurrentImpatientStopStillReleases(t *testing.T) {
	for range 50 {
		inDrain := make(chan struct{})
		release := make(chan struct{})
		stopped := make(chan struct{})

		root := di.New()
		child := root.Child("child")
		root.Provide(func(*di.Scope) *Repo { return &Repo{} }).Scoped().
			OnDrain(func(context.Context, *Repo) error {
				select {
				case <-inDrain:
				default:
					close(inDrain)
				}
				<-release
				return nil
			}).
			OnStop(func(context.Context, *Repo) error { close(stopped); return nil })
		if _, err := child.Resolve[*Repo](); err != nil {
			t.Fatal(err)
		}

		rootDone := make(chan error, 1)
		go func() { rootDone <- root.Stop(context.Background()) }()
		<-inDrain

		spent, cancel := context.WithTimeout(context.Background(), 0)
		if err := child.Stop(spent); err == nil {
			t.Fatal("a Stop with a spent context reported success")
		}
		cancel()
		select {
		case <-stopped:
			t.Fatal("the release ran while the drain hook still held the value")
		default:
		}

		close(release)
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("the release the impatient Stop deferred never happened")
		}
		if err := <-rootDone; err != nil {
			t.Fatalf("root.Stop: %v", err)
		}
	}
}

// The drain phase waits for a start step in flight rather than stepping
// around it. An instance still starting when the sweep reaches it owes a
// drain as soon as it has started, and leaving it undecided for a later pass
// meant a step that outlasted the phase was never drained at all.
func TestConcurrentDrainWaitsForAnInFlightStartStep(t *testing.T) {
	for range 50 {
		starting := make(chan struct{})
		release := make(chan struct{})
		var drained, stoppedAfterDrain atomic.Bool

		root := di.New()
		root.Provide(func(*di.Scope) *DB { return &DB{} }).
			OnStart(func(context.Context, *DB) error { close(starting); <-release; return nil }).
			OnDrain(func(context.Context, *DB) error { drained.Store(true); return nil }).
			OnStop(func(context.Context, *DB) error { stoppedAfterDrain.Store(drained.Load()); return nil })
		if err := root.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = root.Resolve[*DB]() }()
		<-starting

		go func() { time.Sleep(2 * time.Millisecond); close(release) }()
		if err := root.Stop(context.Background()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if !drained.Load() {
			t.Fatal("an instance whose start step was in flight was never drained")
		}
		if !stoppedAfterDrain.Load() {
			t.Fatal("OnStop ran before OnDrain")
		}
	}
}
