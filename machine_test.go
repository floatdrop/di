package di_test

// A model-based test over sequences of container OPERATIONS, not just
// registrations, driven either by a seeded generator or by the fuzzer.
//
// It deliberately does not predict outcomes. A predictive model of a
// container this size is itself likely to be wrong, and a wrong model that
// happens to agree with a wrong implementation hides bugs rather than
// finding them. Instead each sequence is checked against invariants taken
// from the documented guarantees, which hold whatever the sequence is:
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
//	I6  Once the root is stopped, every Run hook has returned.
//
// Alias identity, and recovering a key whose resolution failed, are pinned
// by regression tests instead: they need a specific shape rather than a
// random one.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floatdrop/di"
)

// ---- the services under test ----------------------------------------------

type mk1 struct{ dep any }
type mk2 struct{ dep any }
type mk3 struct{ dep any }
type mkI interface{ marker() }

func (*mk1) marker() {}

const (
	numKeys  = 4 // mk1, mk2, mk3, mkI
	ifaceKey = 3
	// root, two children and a grandchild. The grandchild is what lets a
	// scope between a resolver and the owner of a binding shadow a key that
	// has already been served through it.
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
	opHealth
	opShutdown
	numOpKinds
)

type op struct {
	kind  opKind
	scope uint8
	key   uint8
	reg   uint8 // which registration shape
	// eager is the registration's Eager flag. On every other kind it is a
	// spare bit the concurrent driver reads as a variant: a Stop whose
	// context is far too short for the hooks it will run, which is how the
	// deadline paths are reached at all.
	eager bool
}

func (o op) String() string {
	names := []string{"Register", "Resolve", "Get", "Maybe", "All", "Start", "Stop", "Health", "Shutdown"}
	if o.kind == opRegister {
		return fmt.Sprintf("Register(s%d, %s, shape%d, eager=%v)", o.scope, keyNames[o.key], o.reg, o.eager)
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
			kind:  opKind(data[i] % uint8(numOpKinds)),
			scope: data[i+1] % numScopes,
			key:   data[i+2] % numKeys,
			reg:   data[i+3] % 10,
			eager: data[i+4]&1 == 1,
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

	runsLive atomic.Int32 // Run hooks currently executing

	// values seen per (scope, key), to check singleton stability
	seen map[string]any

	failedResolve map[string]bool
	unstable      map[int]bool // keys that need not resolve to a stable value
	aliased       bool         // mkI has been aliased to *mk1 at some point
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
		unstable: map[int]bool{},
		stopped:  make([]bool, numScopes),
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
	return m
}

// outcome classifies what an operation did.
type outcome struct {
	value    any
	err      error
	panicked bool   // the operation panicked rather than returning
	rejected string // a configuration rejection, which panics by design
}

// call runs f, classifying panics. A panic carrying a string is a rejected
// configuration. A panic carrying an error is how Get reports failure at top
// level. Anything else is a defect.
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
	m.finish()
}

func (m *machine) step(i int, o op) {
	s := m.scopes[o.scope]
	label := fmt.Sprintf("op %d %v", i, o)

	switch o.kind {
	case opRegister:
		m.call(label, func() (any, error) { m.register(s, o); return nil, nil })

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
		m.call(label, func() (any, error) { return nil, s.Start(context.Background()) })

	case opStop:
		out := m.call(label, func() (any, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return nil, s.Stop(ctx)
		})
		if out.rejected == "" {
			m.markStopped(int(o.scope))
		}
		_ = out

	case opHealth:
		m.call(label, func() (any, error) { return nil, s.HealthCheck(context.Background()) })

	case opShutdown:
		// Sequentially this only records a cause; it is here so the operation
		// exists in the shared encoding, and because Shutdown is what a hook
		// must call now that it may not call Stop.
		m.call(label, func() (any, error) { s.Shutdown(errShutdown); return nil, nil })
	}
}

var errShutdown = errors.New("shutdown from the machine")

// checkRepeatable enforces I2. It re-runs the identical operation and
// requires the same rejection. Only read-only operations are re-run:
// Register and Start mutate, so repeating them is not the same operation.
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

// checkStopped enforces I3. A resolve from a stopped tree must not succeed:
// reporting only on the errors it did return would accept the very case the
// invariant exists to rule out.
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

// checkStable enforces I4: a key resolves to the same value each time.
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
	if prev != v && !m.unstable[int(o.key)] {
		m.fail("%s: %s resolved to a different value than before", label, keyNames[o.key])
	}
}

// markTransient and markAliased maintain the set of keys exempt from I4.
// The exemption is deliberately narrow: an alias resolves to its target's own
// instance, so it is only unstable when that target is. Exempting every
// aliased key instead is what hid a scope handing out two live values for one
// interface, so the model must not give that case away.
func (m *machine) markTransient(k int) {
	m.unstable[k] = true
	if k == 0 && m.aliased {
		m.unstable[ifaceKey] = true
	}
}

func (m *machine) markAliased() {
	m.aliased = true
	if m.unstable[0] {
		m.unstable[ifaceKey] = true
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return nil, m.scopes[0].Stop(ctx)
	})
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
		m.fail("%d Run hooks still executing after the root was stopped", n)
	}
}

// ---- registration shapes ---------------------------------------------------

// regShape registers one of ten shapes for T, chosen by op.reg, so a random
// sequence exercises lifetimes, hooks, groups, failures and dependencies.
func regShape[T any](m *machine, s *di.Scope, o op, plain func() T, dep func(*di.Scope) T) {
	var b di.Binding[T]
	noop := func(context.Context, T) error { return nil }
	switch o.reg {
	case 0:
		b = s.Provide(func(*di.Scope) T { return plain() }).OnStart(noop).OnStop(noop)
	case 1:
		b = s.Value(plain()).OnStop(noop)
	case 2:
		b = s.Provide(func(*di.Scope) T { return plain() }).Scoped().OnStop(noop)
	case 3:
		b = s.Provide(func(*di.Scope) T { return plain() }).Transient()
		m.markTransient(int(o.key))
	case 4:
		b = s.Add(func(*di.Scope) T { return plain() }).OnStart(noop).OnStop(noop)
	case 6:
		b = s.Provide(func(*di.Scope) T { return plain() }).
			Run(func(ctx context.Context, _ T) error {
				m.runsLive.Add(1)
				defer m.runsLive.Add(-1)
				<-ctx.Done()
				return nil
			}).OnStop(noop)
	case 7:
		b = s.Provide(func(*di.Scope) T { return plain() }).Health(noop)
	case 8:
		// A constructor that fails. resolve turns this into an error, so it
		// exercises the failure paths rather than escaping as a panic.
		b = s.Provide(func(*di.Scope) T { panic("injected constructor failure") })
	case 9:
		b = s.Provide(dep) // depends on another key, so chains and cycles arise
	default:
		b = s.Provide(func(*di.Scope) T { return plain() })
	}
	if o.eager {
		b.Eager()
	}
}

func (m *machine) register(s *di.Scope, o op) {
	switch o.key {
	case 0:
		regShape(m, s, o,
			func() *mk1 { return &mk1{} },
			func(sc *di.Scope) *mk1 { return &mk1{dep: sc.Get[*mk2]()} })
	case 1:
		regShape(m, s, o,
			func() *mk2 { return &mk2{} },
			func(sc *di.Scope) *mk2 { return &mk2{dep: sc.Get[*mk3]()} })
	case 2:
		regShape(m, s, o,
			func() *mk3 { return &mk3{} },
			func(sc *di.Scope) *mk3 { return &mk3{dep: sc.Get[*mk1]()} })
	default:
		if o.reg == 5 { // only the interface key can be aliased
			m.markAliased()
			bd := s.Bind[mkI, *mk1]()
			if o.eager {
				bd.Eager()
			}
			return
		}
		regShape(m, s, o,
			func() mkI { return &mk1{} },
			func(sc *di.Scope) mkI { _ = sc.Get[*mk2](); return &mk1{} })
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
	f.Add([]byte{0, 0, 3, 5, 1, 1, 0, 3, 0, 0, 6, 0, 3, 0, 0})                // alias, resolve, stop
	f.Add([]byte{0, 0, 0, 9, 0, 0, 0, 1, 9, 0, 0, 0, 2, 9, 0, 1, 0, 0, 0, 0}) // a dependency cycle
	f.Add([]byte{0, 0, 0, 8, 1, 5, 0, 0, 0, 0, 0, 0, 0, 0, 0})                // failing constructor, eager
	f.Fuzz(func(t *testing.T, data []byte) {
		ops := decode(data)
		if len(ops) == 0 {
			return
		}
		newMachine(t, ops).run()
	})
}
