package di_test

// A concurrent driver over the same operations as machine_test.go, plus the
// oracles that only mean anything when calls overlap. The sequential machine
// cannot reach these: it never has two goroutines inside the container, so
// every defect that needs one was invisible to it.
//
// Outcomes are not predicted here either, and fewer invariants survive
// concurrency than survive a sequential run. What is checked is:
//
//	C1  No operation panics except with a configuration rejection.
//	C2  Every operation returns. A driver that hangs is a defect, so each
//	    lane is bounded and the whole run has a deadline.
//	C3  Stop respects scope order: no stop hook of a scope may begin while a
//	    stop hook of one of its descendants is still running. This is the
//	    oracle for a parent and a child being stopped at the same time.
//	C4  Nothing is stopped more often than it was built.
//	C5  A service is built once however many resolutions race for it.

import (
	"context"
	"fmt"
	"math/rand/v2"
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
	owner sync.Map // built value -> the name of the scope holding it

	scopes []*di.Scope
	names  []string

	mu     sync.Mutex
	builds map[string]int
	stops  map[string]int
	fails  []string
}

func newCMachine(t *testing.T, ops []op) *cmachine {
	m := &cmachine{
		t: t, ops: ops,
		builds: map[string]int{}, stops: map[string]int{},
		order: newStopOrder(map[string]string{"c1": "root", "c2": "root", "gc": "c1", "request": "root"}),
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
	for i, s := range m.scopes {
		s.Value(scopeName{m.names[i]})
	}
	return m
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
			// Get reports failure this way at top level; not a defect.
		default:
			m.fail("%s panicked with %T: %v", what, v, v)
		}
	}()
	f()
}

// register wires the shapes the concurrent driver uses. It stays simpler than
// the sequential machine's ten: what matters here is overlap, not variety, and
// every hook has to cooperate with the stop-order oracle.
func (m *cmachine) register(s *di.Scope, o op) {
	stop := func(_ context.Context, v any) error {
		name := m.holderOf(v)
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
	// own records which scope the constructor ran in, which for every
	// lifetime is the scope that holds the instance and will stop it.
	own := func(sc *di.Scope, v T) T {
		m.owner.Store(any(v), sc.Get[scopeName]().name)
		return v
	}
	build := func(sc *di.Scope) T { return own(sc, plain()) }
	switch o.reg % 5 {
	case 0:
		s.Provide(build).OnStop(down)
	case 1:
		s.Provide(build).Scoped().OnStop(down)
	case 2:
		s.Provide(func(sc *di.Scope) T { return own(sc, dep(sc)) }).OnStop(down)
	case 3:
		// OnDrain deliberately stays out of the stop-order oracle: draining
		// runs before anything is torn down and releases nothing, so it is
		// not ordered against a concurrently stopping sibling. Drain order
		// itself is pinned deterministically in review_test.go.
		s.Provide(build).
			OnDrain(func(context.Context, T) error { return nil }).
			Run(func(ctx context.Context, _ T) error { <-ctx.Done(); return nil }).
			OnStop(down)
	default:
		s.Provide(build).
			OnStart(func(context.Context, T) error { return nil }).
			OnStop(down)
	}
}

func (m *cmachine) resolve(s *di.Scope, o op) {
	switch o.key {
	case 0:
		_, _ = s.Resolve[*mk1]()
	case 1:
		_, _ = s.Resolve[*mk2]()
	case 2:
		_, _ = s.Resolve[*mk3]()
	default:
		_, _ = s.Resolve[mkI]()
	}
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

	for _, phase := range [][]op{warm, up, down} {
		m.parallel(phase)
	}

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
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, n := range m.stops { // C4
		if n > m.builds[id] {
			m.fails = append(m.fails, fmt.Sprintf("%s: stopped %d times but built %d", id, n, m.builds[id]))
		}
	}
	if len(m.fails) > 0 {
		m.t.Fatalf("%s\n  sequence: %v", strings.Join(m.fails, "\n  "), m.ops)
	}
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
	}
	for i, data := range seeds {
		t.Run(fmt.Sprintf("seed%d", i), func(t *testing.T) { newCMachine(t, decode(data)).run() })
	}
}

// FuzzMachineConcurrent is the coverage-guided driver for the same oracles.
func FuzzMachineConcurrent(f *testing.F) {
	f.Add([]byte{0, 1, 0, 1, 0, 1, 3, 0, 0, 0, 6, 1, 0, 0, 0, 6, 0, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 3, 1, 5, 0, 0, 0, 0, 6, 1, 0, 0, 0, 6, 0, 0, 0, 0})
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
