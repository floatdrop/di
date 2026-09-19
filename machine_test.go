package di_test

// A model-based test over sequences of container operations, driven by a
// seeded generator or by the fuzzer.
//
// It predicts nothing. A predictive model of the whole container could be
// wrong in the same way as the code, so each sequence is checked against
// invariants taken from the documented guarantees:
//
//	I1  Resolve never panics, with an error or anything else. Wiring
//	    problems are errors; only a rejected configuration panics, and only
//	    from Register, Get, All or Start.
//	I2  A rejected configuration is rejected identically when the same
//	    read-only operation is repeated. (Different operations on one scope
//	    may legitimately be rejected for different reasons, and a resolve
//	    freezes several scopes, so the check is per operation.)
//	I3  A stopped scope, and everything under it, refuses to resolve: an
//	    operation on it fails, or is rejected, but never succeeds.
//	I4  A singleton is stable: two successful resolutions of a key from one
//	    scope return the identical value.
//	I5  Nothing is stopped more often than it was built.
//	I6  Once the root is stopped, every Go hook has returned.
//	I7  Explain and Graph render whatever state the sequence reached,
//	    panicking only where a resolution from the same scope would, and
//	    never deadlocking against the phase machine they read.
//	I8  Validate builds nothing and is repeatable: two calls from one scope
//	    say the same thing, and the build count is what it was before.
//
// What happens to an instance once it exists is predicted by the model in
// lifecyclemodel_test.go. Recovering a key whose resolution failed needs a
// specific shape and is pinned by a regression test instead.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.yandex/di"
)

// ---- the services under test ----------------------------------------------

type mk1 struct{ dep any }
type mk2 struct{ dep any }
type mk3 struct{ dep any }
type mkI interface{ marker() }

func (*mk1) marker() {}

const (
	numKeys = 4 // mk1, mk2, mk3, mkI
	// root, two children and a grandchild. The grandchild is what lets a
	// scope between a resolver and a binding's owner shadow a key already
	// served through it.
	numScopes = 4
)

// parentOf[i] is the index of scope i's parent, -1 for the root.
var parentOf = [numScopes]int{-1, 0, 0, 1}

var keyNames = []string{"mk1", "mk2", "mk3", "mkI"}

// ---- operations ------------------------------------------------------------

type opKind uint8

const (
	opRegister opKind = iota
	opResolve
	opGet
	opMaybe
	opAll
	opStart
	opStop
	opShutdown
	opRun
	numOpKinds
)

type op struct {
	kind  opKind
	scope uint8
	key   uint8
	reg   uint8 // which registration shape
	// eager is the registration's Eager flag. On every other kind the
	// concurrent driver reads it as a variant: a Stop whose context is far
	// too short for its hooks, which is how the deadline paths are reached.
	eager bool
	// override marks a registration Override(). Without it a repeated key in
	// one scope is a rejection, which is a shallow outcome; with it the
	// sequence goes on to exercise what a replacement does.
	override bool
	// wire registers the shape through Wire instead of Provide, where the
	// shape has a constructor to hand over. The bit was spare, so every
	// corpus entry keeps its meaning.
	wire bool
	// asks makes the constructor resolve the next key optionally, so a
	// sequence reaches a miss recorded against the scopes that answered it.
	// Another spare bit, for the same reason.
	asks bool
}

func (o op) String() string {
	names := []string{"Register", "Resolve", "Get", "Maybe", "All", "Start", "Stop", "Shutdown", "Run"}
	if o.kind == opRegister {
		return fmt.Sprintf("Register(s%d, %s, shape%d, eager=%v, override=%v, wire=%v, asks=%v)", o.scope, keyNames[o.key], o.reg, o.eager, o.override, o.wire, o.asks)
	}
	if o.kind == opStop && o.eager {
		return fmt.Sprintf("Stop(s%d, impatient)", o.scope)
	}
	return fmt.Sprintf("%s(s%d, %s)", names[o.kind], o.scope, keyNames[o.key])
}

func decode(data []byte) []op {
	var ops []op
	for i := 0; i+4 < len(data) && len(ops) < 24; i += 5 {
		ops = append(ops, op{
			kind:     opKind(data[i] % uint8(numOpKinds)),
			scope:    data[i+1] % numScopes,
			key:      data[i+2] % numKeys,
			reg:      data[i+3] % 12,
			eager:    data[i+4]&1 == 1,
			override: data[i+4]&2 == 2,
			wire:     data[i+4]&4 == 4,
			asks:     data[i+4]&8 == 8,
		})
	}
	return ops
}

// ---- the harness -----------------------------------------------------------

type machine struct {
	t       *testing.T
	ops     []op
	scopes  []*di.Scope
	names   []string
	stopped []bool // scope index -> Stop has been called

	// observed lifecycle, keyed by "scope/service"
	builds map[string]int
	starts map[string]int
	stops  map[string]int

	runsLive atomic.Int32 // Go hooks currently executing

	// values seen per (scope, key), to check singleton stability
	seen map[string]any

	// registeredFrom remembers the scopes a constructor has already
	// registered into, so the registering shape never overrides itself.
	registeredFrom map[int]bool

	lc *lifecycle

	failedResolve map[string]bool
}

// fail reports a violation together with the sequence that produced it.
func (m *machine) fail(format string, args ...any) {
	m.t.Helper()
	m.t.Fatalf(format+"\n  sequence: %v", append(args, m.ops)...)
}

func newMachine(t *testing.T, ops []op) *machine {
	m := &machine{
		t: t, ops: ops,
		builds: map[string]int{}, starts: map[string]int{}, stops: map[string]int{},
		seen: map[string]any{}, failedResolve: map[string]bool{},
		registeredFrom: map[int]bool{},
		stopped:        make([]bool, numScopes),
	}
	root := di.New()
	root.Observe(func(ev di.Event) {
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
	m.lc = newLifecycle(m)
	for i, sc := range m.scopes {
		sc.Value(scopeName{m.names[i]})
	}
	return m
}

// scopeOf names the scope a constructor is running in, which for every
// lifetime is the scope that holds the instance and will stop it.
func (m *machine) scopeOf(sc *di.Scope) int { return m.indexOf(sc.Get[scopeName]().name) }

func (m *machine) indexOf(name string) int {
	for i, n := range m.names {
		if n == name {
			return i
		}
	}
	return 0
}

// reported is the build report of a Wire constructor, which has no scope
// handle: the scope's name arrives as a dependency.
func reported[T any](m *machine, o op, sn scopeName, v T) T {
	m.lc.built(m.indexOf(sn.name), o.reg, any(v))
	return v
}

// reportedHere is reported for a Wire constructor, which has no scope handle:
// a singleton is built in the scope that registered it.
func reportedHere[T any](m *machine, o op, v T) T {
	m.lc.built(int(o.scope), o.reg, any(v))
	return v
}

// outcome classifies what an operation did.
type outcome struct {
	value    any
	err      error
	panicked bool   // the operation panicked rather than returning
	rejected string // a configuration rejection, which panics by design
}

// call runs f, classifying panics. A panic carrying a string is a rejected
// configuration; one carrying an error is how Get reports failure at top
// level; anything else is a defect.
func (m *machine) call(what string, f func() (any, error)) outcome {
	var out outcome
	func() {
		defer func() {
			r := recover()
			switch v := r.(type) {
			case nil:
			case string:
				if !strings.HasPrefix(v, "di: ") {
					m.fail("%s panicked with an unexpected string: %q", what, v)
				}
				out.rejected = v
			case error:
				out.err, out.panicked = v, true
			default:
				m.fail("%s panicked with %T: %v", what, r, r)
			}
		}()
		out.value, out.err = f()
	}()
	return out
}

func (m *machine) run() {
	for i, o := range m.ops {
		m.step(i, o)
	}
	m.render()
	m.finish()
}

// render enforces I7 against the state the sequence ended in: every scope's
// graph, and every key explained from every scope. Once per sequence, since
// the shapes a rendering can meet are decided by the registrations, not by
// where in the sequence it is asked.
func (m *machine) render() {
	for i, s := range m.scopes {
		g := s.Graph()
		if !strings.HasPrefix(g, "digraph di {") || !strings.HasSuffix(g, "}\n") {
			m.fail("Graph(s%d) rendered %q", i, g)
		}
		for k := range numKeys {
			m.explain(s, i, k)
		}
		m.validate(s, i)
		m.modules(s, i)
	}
}

// modules renders the module report. A configuration rejection is legitimate
// for the reason explain gives; anything else is a defect.
func (m *machine) modules(s *di.Scope, scope int) {
	defer func() {
		if r := recover(); r != nil {
			if _, rejected := r.(string); !rejected {
				m.fail("Modules(s%d) panicked with %v", scope, r)
			}
		}
	}()
	if out := s.Modules(); out != "" && !strings.HasSuffix(out, "\n") {
		m.fail("Modules(s%d) rendered %q", scope, out)
	}
}

// validate enforces I8. What Validate says is pinned by validate_test.go,
// since predicting it here would model the lookup rules a second time.
func (m *machine) validate(s *di.Scope, scope int) {
	defer func() {
		if r := recover(); r != nil {
			if _, rejected := r.(string); !rejected {
				m.fail("Validate(s%d) panicked with %v", scope, r)
			}
		}
	}()
	before := m.totalBuilds()
	for _, stubs := range [][]di.Stub{nil, {di.Provided[*mk2](), di.Provided[mkI]()}} {
		first := s.Validate(stubs...)
		second := s.Validate(stubs...)
		if m.totalBuilds() != before {
			m.fail("Validate(s%d) built something", scope)
		}
		if fmt.Sprint(first.Err()) != fmt.Sprint(second.Err()) ||
			len(first.Owed) != len(second.Owed) || len(first.Unchecked) != len(second.Unchecked) {
			m.fail("Validate(s%d) was not repeatable:\n  %+v\n  %+v", scope, first, second)
		}
		if len(stubs) > 0 && len(first.Owed) > 0 {
			m.fail("Validate(s%d) with stubs owes nothing by definition, got %v", scope, first.Owed)
		}
	}
}

func (m *machine) totalBuilds() int {
	n := 0
	for _, c := range m.builds {
		n += c
	}
	return n
}

// explain renders one key from one scope. A configuration rejection is
// legitimate, because Explain commits the pending batch as a resolution from
// this scope would; any other panic is a defect.
func (m *machine) explain(s *di.Scope, scope, k int) {
	defer func() {
		if r := recover(); r != nil {
			if _, rejected := r.(string); !rejected {
				m.fail("Explain(s%d, %s) panicked with %v", scope, keyNames[k], r)
			}
		}
	}()
	var out string
	switch k {
	case 0:
		out = s.Explain[*mk1]()
	case 1:
		out = s.Explain[*mk2]()
	case 2:
		out = s.Explain[*mk3]()
	default:
		out = s.Explain[mkI]()
	}
	if !strings.HasSuffix(out, "\n") {
		m.fail("Explain(s%d, %s) rendered %q", scope, keyNames[k], out)
	}
}

func (m *machine) step(i int, o op) {
	s := m.scopes[o.scope]
	label := fmt.Sprintf("op %d %v", i, o)

	switch o.kind {
	case opRegister:
		// Through Use, so every binding carries a module name and the
		// collision rejections name it.
		m.call(label, func() (any, error) { s.Use(func(sc *di.Scope) { m.register(sc, o) }); return nil, nil })

	case opResolve:
		f := func() (any, error) { return m.resolve(s, o) }
		out := m.call(label, f)
		if out.panicked {
			m.fail("%s: Resolve panicked with an error instead of returning it: %v", label, out.err)
		}
		if out.err == nil && out.rejected == "" {
			m.checkStable(label, o, out.value)
		}
		if out.err != nil {
			m.failedResolve[m.vkey(o)] = true
		}
		m.checkRepeatable(label, out, f)
		m.checkStopped(label, o, out)

	case opGet:
		f := func() (any, error) { return m.get(s, o) }
		out := m.call(label, f)
		if out.err == nil && out.rejected == "" {
			m.checkStable(label, o, out.value)
		}
		m.checkRepeatable(label, out, f)

	case opMaybe:
		f := func() (any, error) { return m.maybe(s, o) }
		m.checkRepeatable(label, m.call(label, f), f)

	case opAll:
		f := func() (any, error) { return m.all(s, o) }
		m.checkRepeatable(label, m.call(label, f), f)

	case opStart:
		out := m.call(label, func() (any, error) { return nil, s.Start(machineStartCtx(int(o.scope))) })
		if out.rejected == "" {
			m.lc.started(int(o.scope), out.err)
		}

	case opStop:
		out := m.call(label, func() (any, error) {
			ctx, cancel := context.WithTimeout(m.t.Context(), 2*time.Second)
			defer cancel()
			return nil, s.Stop(ctx)
		})
		if out.rejected == "" {
			m.markStopped(int(o.scope))
			m.lc.stopping(int(o.scope))
		}
		_ = out

	case opRun:
		// Run with a context that is already cancelled: it starts the scope,
		// finds nothing to wait for, and stops again. Run is where a worker's
		// failure and a Stop's errors are joined.
		ctx, cancel := context.WithCancel(machineStartCtx(int(o.scope)))
		cancel()
		out := m.call(label, func() (any, error) {
			return nil, s.Run(ctx, di.StopTimeout(2*time.Second))
		})
		if out.rejected == "" {
			m.lc.ranAndStopped(int(o.scope), out.err)
			if out.err == nil || !strings.Contains(out.err.Error(), "Start called twice") {
				m.markStopped(int(o.scope))
			}
		}

	case opShutdown:
		// Sequentially this only records a cause; it is here so the operation
		// exists in the shared encoding.
		m.call(label, func() (any, error) { s.Shutdown(errShutdown); return nil, nil })
	}
}

var errShutdown = errors.New("shutdown from the machine")

// marker is what the registering shape registers: its own type, so it can
// never override anything else, and registered once per scope so it never
// overrides itself.
type marker struct{}

func (m *machine) registerFrom(sc *di.Scope, scope int) {
	if m.registeredFrom[scope] {
		return
	}
	m.registeredFrom[scope] = true
	sc.Value(marker{})
}

// checkRepeatable enforces I2 by re-running the identical operation. Only
// read-only operations are re-run: Register and Start mutate.
func (m *machine) checkRepeatable(label string, out outcome, again func() (any, error)) {
	if out.rejected == "" {
		return
	}
	second := m.call(label+" (repeat)", again)
	if second.rejected != out.rejected {
		m.fail("%s: rejection was not repeatable\n  first:  %s\n  second: %q err=%v",
			label, out.rejected, second.rejected, second.err)
	}
}

// checkStopped enforces I3. A resolve from a stopped tree must not succeed;
// checking only the errors it returned would accept the very case the
// invariant rules out.
func (m *machine) checkStopped(label string, o op, out outcome) {
	if !m.stoppedTree(int(o.scope)) || out.rejected != "" {
		return
	}
	if out.err == nil {
		m.fail("%s: resolving from a stopped scope succeeded", label)
	}
	if !errors.Is(out.err, di.ErrStopped) && !errors.Is(out.err, di.ErrNotProvided) && !errors.Is(out.err, di.ErrCycle) {
		m.fail("%s: resolving from a stopped scope gave %v", label, out.err)
	}
}

// checkStable enforces I4.
func (m *machine) checkStable(label string, o op, v any) {
	if v == nil {
		return
	}
	k := m.vkey(o)
	prev, ok := m.seen[k]
	if !ok {
		m.seen[k] = v
		return
	}
	if prev != v {
		m.fail("%s: %s resolved to a different value than before", label, keyNames[o.key])
	}
}

func (m *machine) vkey(o op) string { return fmt.Sprintf("s%d/%s", o.scope, keyNames[o.key]) }

// markStopped records that scope i was stopped, along with its descendants.
func (m *machine) markStopped(i int) {
	m.stopped[i] = true
	for j := range m.stopped {
		for a := j; a >= 0; a = parentOf[a] {
			if a == i {
				m.stopped[j] = true
				break
			}
		}
	}
}

// stoppedTree reports whether scope i or any ancestor has been stopped.
func (m *machine) stoppedTree(i int) bool {
	for a := i; a >= 0; a = parentOf[a] {
		if m.stopped[a] {
			return true
		}
	}
	return false
}

// finish enforces I5 and I6.
func (m *machine) finish() {
	_ = m.call("final Stop", func() (any, error) {
		ctx, cancel := context.WithTimeout(m.t.Context(), 5*time.Second)
		defer cancel()
		return nil, m.scopes[0].Stop(ctx)
	})
	m.lc.stopping(0)
	m.lc.report()
	for id, n := range m.stops {
		if n > m.builds[id] {
			m.fail("%s: stopped %d times but built %d", id, n, m.builds[id])
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for m.runsLive.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := m.runsLive.Load(); n != 0 {
		m.fail("%d Go hooks still executing after the root was stopped", n)
	}
}

// ---- registration shapes ---------------------------------------------------

// regShape registers one of the shapes for T, chosen by op.reg, so a random
// sequence exercises lifetimes, hooks, groups, failures and dependencies.
func regShape[T any](m *machine, s *di.Scope, o op, plain func() T, dep func(*di.Scope) T, wire, wireScoped, wireNeeds any, needs []di.Need) {
	var b di.Binding[T]
	// Every modelled shape reports its own build and its own hooks, so the
	// model knows which instance is which without predicting what serves a
	// key. A Value binding has no constructor to report from and stays
	// outside the model.
	builtIn := func(scope int, v T) T {
		m.lc.built(scope, o.reg, any(v))
		return v
	}
	built := func(sc *di.Scope, v T) T {
		if o.asks {
			// Inside the build, so the miss is recorded and a later
			// registration of that key in a scope that answered is rejected.
			askOptional(sc, o.key)
		}
		return builtIn(m.scopeOf(sc), v)
	}
	hook := func(name string) func(context.Context, T) error {
		return func(_ context.Context, v T) error {
			m.lc.hookRan(any(v), name)
			return nil
		}
	}
	noop := func(context.Context, T) error { return nil }
	switch o.reg {
	case 0:
		if o.wire {
			// A Wire constructor has no scope handle; a singleton is built in
			// the scope that registered it. With asks it declares the two
			// reads with Needs rather than making them from a scope it has
			// not got, which is the only way the wire path reaches them.
			ctor := any(func() T { return builtIn(int(o.scope), plain()) })
			if o.asks {
				ctor = wireNeeds
			}
			b = s.Wire[T](ctor).OnStart(hook("OnStart")).OnStop(hook("OnStop"))
			if o.asks {
				b.Needs(needs...)
			}
		} else {
			b = s.Provide(func(sc *di.Scope) T { return built(sc, plain()) }).
				OnStart(hook("OnStart")).OnStop(hook("OnStop"))
		}
	case 1:
		if o.wire {
			// A wrapper over whatever serves the key, or a registration-time
			// rejection when nothing does. It reports its build in the scope
			// that resolves it, which for a scoped chain is not the
			// registering one.
			b = s.Wrap[T](func(_ T, sn scopeName) T { return reported(m, o, sn, plain()) }).
				OnStop(hook("OnStop"))
		} else {
			b = s.Value(plain()).OnStop(noop)
		}
	case 2:
		if o.wire {
			// Scoped through Wire, with a declared dependency: the one shape
			// whose dependency Validate has to leave to a descendant.
			b = s.Wire[T](wireScoped).Scoped().
				OnStop(hook("OnStop"))
		} else {
			b = s.Provide(func(sc *di.Scope) T { return built(sc, plain()) }).Scoped().
				OnStop(hook("OnStop"))
		}
	case 3:
		b = s.Provide(func(sc *di.Scope) T { return built(sc, plain()) }).Group().
			OnStart(hook("OnStart")).OnStop(hook("OnStop"))
	case 4:
		b = s.Provide(func(sc *di.Scope) T { return built(sc, plain()) }).
			Go(func(ctx context.Context, _ T) error {
				m.runsLive.Add(1)
				defer m.runsLive.Add(-1)
				<-ctx.Done()
				return nil
			}).OnStop(hook("OnStop"))
	case 5:
		if o.wire {
			// A constructor of the wrong shape, one per key, rejected at
			// registration.
			switch o.key {
			case 0:
				b = s.Wire[T](42)
			case 1:
				b = s.Wire[T](func(...int) T { return plain() })
			case 2:
				b = s.Wire[T](func() {})
			default:
				b = s.Wire[T](func() (T, bool) { return plain(), false })
			}
			break
		}
		// A constructor that fails, which resolve turns into an error. With
		// asks it is also the shape whose decline must not be recorded.
		b = s.Provide(func(sc *di.Scope) T {
			if o.asks {
				askOptional(sc, o.key)
			}
			panic("injected constructor failure")
		})
	case 6:
		// Depends on another key, so chains and cycles arise; through Wire
		// the dependency is declared, so Validate has something to walk.
		if o.wire {
			b = s.Wire[T](wire)
		} else {
			// The one shape where an optional ask meets a dependency of its
			// own, so a decline is recorded under a chain rather than a leaf.
			b = s.Provide(func(sc *di.Scope) T {
				if o.asks {
					askOptional(sc, o.key)
				}
				return dep(sc)
			})
		}
	case 7:
		// Draining, where the phase's boundary is known.
		b = s.Provide(func(sc *di.Scope) T { return built(sc, plain()) }).
			OnDrain(hook("OnDrain")).OnStop(hook("OnStop"))
	case 8:
		b = s.Provide(func(sc *di.Scope) T { return built(sc, plain()) }).
			OnStart(hook("OnStart")).OnDrain(hook("OnDrain")).OnStop(hook("OnStop"))
	case 9:
		// A constructor that registers, and resolves what it registered, so
		// freeze runs inside a nested lookup with a resolution in flight.
		b = s.Provide(func(sc *di.Scope) T {
			m.registerFrom(sc, int(o.scope))
			_, _ = sc.Resolve[marker]()
			return built(sc, plain())
		}).OnStop(hook("OnStop"))
	case 10:
		// The same, over its own key, which has to be rejected: the nested
		// resolve would be served the replacement while this one returns the
		// old value.
		b = s.Provide(func(sc *di.Scope) T {
			sc.Provide(func(*di.Scope) T { return plain() })
			_, _ = sc.Resolve[T]()
			return built(sc, plain())
		}).OnStop(hook("OnStop"))
	default:
		b = s.Provide(func(*di.Scope) T { return plain() })
	}
	if o.eager {
		b.Eager()
	}
	if o.override {
		b.Override()
	}
}

// askOptional makes the two reads that leave a fact behind on the key after
// this one: an optional resolution, whose miss is recorded against the scope
// that answered, and a group read, whose members Explain compares with the
// group as it stands. The next key rather than its own, since a constructor
// that resolves its own key is a cycle rather than a miss.
func askOptional(sc *di.Scope, key uint8) {
	switch (key + 1) % numKeys {
	case 0:
		_, _ = sc.Maybe[*mk1]()
		_ = sc.All[*mk1]()
	case 1:
		_, _ = sc.Maybe[*mk2]()
		_ = sc.All[*mk2]()
	case 2:
		_, _ = sc.Maybe[*mk3]()
		_ = sc.All[*mk3]()
	default:
		_, _ = sc.Maybe[mkI]()
		_ = sc.All[mkI]()
	}
}

func (m *machine) register(s *di.Scope, o op) {
	// The declared pair, for op.asks on the wire path: a constructor whose
	// parameters are the optional and the group for the next key, and the
	// Needs that say so. Both parameters name one key and two types, which is
	// how Needs matches them.
	switch o.key {
	case 0:
		regShape(m, s, o,
			func() *mk1 { return &mk1{} },
			func(sc *di.Scope) *mk1 { return &mk1{dep: sc.Get[*mk2]()} },
			func(d *mk2) *mk1 { return &mk1{dep: d} },
			func(sn scopeName, d *mk2) *mk1 { return reported(m, o, sn, &mk1{dep: d}) },
			func(opt *mk2, group []*mk2) *mk1 { return reportedHere(m, o, &mk1{dep: opt}) },
			[]di.Need{di.Optional[*mk2](), di.AllOf[*mk2]()})
	case 1:
		regShape(m, s, o,
			func() *mk2 { return &mk2{} },
			func(sc *di.Scope) *mk2 { return &mk2{dep: sc.Get[*mk3]()} },
			func(d *mk3) *mk2 { return &mk2{dep: d} },
			func(sn scopeName, d *mk3) *mk2 { return reported(m, o, sn, &mk2{dep: d}) },
			func(opt *mk3, group []*mk3) *mk2 { return reportedHere(m, o, &mk2{dep: opt}) },
			[]di.Need{di.Optional[*mk3](), di.AllOf[*mk3]()})
	case 2:
		regShape(m, s, o,
			func() *mk3 { return &mk3{} },
			func(sc *di.Scope) *mk3 { return &mk3{dep: sc.Get[*mk1]()} },
			func(d *mk1) *mk3 { return &mk3{dep: d} },
			func(sn scopeName, d *mk1) *mk3 { return reported(m, o, sn, &mk3{dep: d}) },
			func(opt *mk1, group []*mk1) *mk3 { return reportedHere(m, o, &mk3{dep: opt}) },
			[]di.Need{di.Optional[*mk1](), di.AllOf[*mk1]()})
	default:
		regShape(m, s, o,
			func() mkI { return &mk1{} },
			func(sc *di.Scope) mkI { _ = sc.Get[*mk2](); return &mk1{} },
			func(*mk2) mkI { return &mk1{} },
			func(sn scopeName, _ *mk2) mkI { return reported(m, o, sn, mkI(&mk1{})) },
			func(opt *mk1, group []*mk1) mkI { return reportedHere(m, o, mkI(&mk1{})) },
			[]di.Need{di.Optional[*mk1](), di.AllOf[*mk1]()})
	}
}

// ---- resolution dispatch ---------------------------------------------------

func (m *machine) resolve(s *di.Scope, o op) (any, error) {
	switch o.key {
	case 0:
		v, err := s.Resolve[*mk1]()
		return v, err
	case 1:
		v, err := s.Resolve[*mk2]()
		return v, err
	case 2:
		v, err := s.Resolve[*mk3]()
		return v, err
	default:
		v, err := s.Resolve[mkI]()
		return v, err
	}
}

func (m *machine) get(s *di.Scope, o op) (any, error) {
	switch o.key {
	case 0:
		return s.Get[*mk1](), nil
	case 1:
		return s.Get[*mk2](), nil
	case 2:
		return s.Get[*mk3](), nil
	default:
		return s.Get[mkI](), nil
	}
}

func (m *machine) maybe(s *di.Scope, o op) (any, error) {
	switch o.key {
	case 0:
		v, _ := s.Maybe[*mk1]()
		return v, nil
	case 1:
		v, _ := s.Maybe[*mk2]()
		return v, nil
	case 2:
		v, _ := s.Maybe[*mk3]()
		return v, nil
	default:
		v, _ := s.Maybe[mkI]()
		return v, nil
	}
}

func (m *machine) all(s *di.Scope, o op) (any, error) {
	switch o.key {
	case 0:
		return len(s.All[*mk1]()), nil
	case 1:
		return len(s.All[*mk2]()), nil
	case 2:
		return len(s.All[*mk3]()), nil
	default:
		return len(s.All[mkI]()), nil
	}
}

// ---- drivers ---------------------------------------------------------------

// TestMachineSeeded runs a deterministic sweep, so CI is fast and any
// failure is reproducible from the seed alone.
func TestMachineSeeded(t *testing.T) {
	rng := rand.New(rand.NewPCG(0xC0FFEE, 0xBEEF))
	for iter := range 3000 {
		n := 1 + rng.IntN(12)
		data := make([]byte, n*5)
		for i := range data {
			data[i] = byte(rng.UintN(256))
		}
		ops := decode(data)
		t.Run(fmt.Sprintf("iter%d", iter), func(t *testing.T) {
			newMachine(t, ops).run()
		})
		if t.Failed() {
			t.Fatalf("failing sequence: %v", ops)
		}
	}
}

// FuzzMachine is the coverage-guided driver over the same invariants. Run it
// with: go test -fuzz FuzzMachine -fuzztime 2m
func FuzzMachine(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 1, 5, 0, 0, 0, 0})                               // register eager, then Start
	f.Add([]byte{0, 0, 0, 6, 0, 1, 0, 0, 0, 0, 6, 0, 0, 0, 0})                // a dependency on an unprovided key, then stop
	f.Add([]byte{0, 0, 0, 6, 0, 0, 0, 1, 6, 0, 0, 0, 2, 6, 0, 1, 0, 0, 0, 0}) // a dependency cycle
	f.Add([]byte{0, 0, 0, 5, 1, 5, 0, 0, 0, 0, 0, 0, 0, 0, 0})                // failing constructor, eager
	// The wired shapes a random sequence rarely combines: a cycle of three
	// Wire singletons; a Scoped Wire binding whose dependency the root cannot
	// provide; a singleton that would build such a binding in its own scope;
	// and the four rejected constructor shapes.
	f.Add([]byte{0, 0, 0, 6, 4, 0, 0, 1, 6, 4, 0, 0, 2, 6, 4, 2, 1, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 2, 4, 2, 1, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 2, 4, 0, 1, 1, 0, 4, 2, 1, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 6, 4, 0, 0, 1, 2, 4, 1, 0, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 5, 4, 0, 0, 1, 5, 4, 0, 0, 2, 5, 4, 0, 0, 3, 5, 4})
	// Wrappers: over a singleton, started eagerly; over the parent's value
	// from a child; over a scoped chain; and over nothing, which is rejected.
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 1, 5, 5, 0, 0, 0, 0, 6, 0, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 1, 0, 1, 4, 2, 1, 0, 0, 0, 2, 0, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 2, 4, 0, 0, 1, 0, 0, 0, 0, 0, 1, 4, 2, 3, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 1, 4})
	// A wrapper chain an Override replaces while a grandchild still wraps its
	// first link, then the grandchild stops: retiring the chain, release
	// stopping at a wrapped link, and the cascade when that scope stops are
	// reached by nothing else a generator builds.
	f.Add([]byte{0, 0, 0, 0, 0, 0, 1, 0, 1, 4, 0, 3, 0, 1, 4, 0, 1, 0, 1, 4, 0, 1, 0, 0, 2, 1, 1, 0, 0, 0, 6, 3, 0, 0, 0, 6, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		ops := decode(data)
		if len(ops) == 0 {
			return
		}
		newMachine(t, ops).run()
	})
}
