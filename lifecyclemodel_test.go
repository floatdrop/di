package di_test

// A model of the instance lifecycle, checked against the sequential machine.
//
// machine_test.go deliberately predicts nothing: a predictive model of the
// whole container is itself likely to be wrong, and a wrong model that agrees
// with a wrong implementation hides defects rather than finding them. That
// argument holds for registration, where what serves a key depends on
// overrides and the eager rules, and property_test.go models exactly
// that much and no more.
//
// It does not hold for what happens to an instance once it exists. Given that
// a constructor ran, in a scope, for a binding with a known set of hooks, the
// rest is a small state machine that the package documents completely: which
// hooks are owed, in what order, and exactly once. That is also the half of
// the library every review found defects in, and the half no generator was
// checking -- the drivers counted hooks and compared them against each other,
// which cannot see a hook that should have run and did not.
//
// So the model takes builds as given, from the constructors themselves, and
// predicts everything downstream:
//
//	M1  No hook of an instance runs twice.
//	M2  For one instance the order is OnStart, then OnDrain, then OnStop.
//	M3  An instance owes a stop step when its start step succeeded, or when
//	    it was built and has no OnStart to pair with -- either the binding
//	    declares none, or the scope was never started. Every instance that
//	    owes one gets one; one that does not, does not.
//	M4  An instance that owes a drain gets one, under the same predicate.
//	    The concurrent driver cannot check this and says so: an instance
//	    built during the phase may legitimately miss it. Sequentially the
//	    phase has a boundary, and drain hooks here build nothing, so the
//	    question has an exact answer.
//	M5  Instances stop innermost scope first, and in reverse build order
//	    within a scope.
//	M6  An instance built into a running scope runs its start step as part
//	    of being built.
//
// Whether the start step ran is observed rather than predicted, because a
// rollback stops what had started at the moment it failed and predicting that
// would mean predicting the failure. Everything the observation then feeds is
// still a prediction: M3 and M4 are the package's own rule for what is owed.

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// The machine starts each scope with a context naming that scope, so both
// halves of "which Start governs this scope" can be read back off
// Scope.Context: that it was started at all, and which scope's Start it was.
// The model observes this rather than predicting it, because whether a
// rejected Start had already recorded its context depends on which panic came
// first, which the package does not promise -- and because the answer changes
// while a Start is running, which is the window an eager build happens in.
type machineStartKey struct{}

func machineStartCtx(scope int) context.Context {
	return context.WithValue(context.Background(), machineStartKey{}, scope)
}

// governedBy names the scope whose Start this one answers to: the nearest
// scope, itself included, that Start has been called on.
func (l *lifecycle) governedBy(scope int) (int, bool) {
	v := l.m.scopes[scope].Context().Value(machineStartKey{})
	if v == nil {
		return 0, false
	}
	return v.(int), true
}

type hookSet struct{ start, drain, stop bool }

// hooksOfShape says which hooks each registration shape of the sequential
// machine declares. It is the one place the model has to agree with
// regShape, so they are read together.
func hooksOfShape(reg uint8) hookSet {
	switch reg {
	case 0, 3:
		return hookSet{start: true, stop: true}
	case 1, 2, 4:
		return hookSet{stop: true}
	case 7:
		return hookSet{drain: true, stop: true}
	case 8:
		return hookSet{start: true, drain: true, stop: true}
	}
	return hookSet{}
}

type scopePhase int

const (
	scopeNew scopePhase = iota
	scopeStarted
	scopeStopped
)

// modelInstance is what the model believes about one built value.
type modelInstance struct {
	value  any
	scope  int // the scope holding it, which is the one that stops it
	hooks  hookSet
	built  int // global order, so M5 can read reverse build order off it
	ran    map[string]int
	at     map[string]int // hook -> when it ran
	live   bool           // the model expects this instance to be startable
	expect map[string]bool
}

type lifecycle struct {
	m         *machine
	seq       int
	instances []*modelInstance
	byValue   map[any]*modelInstance
	phase     [numScopes]scopePhase
	fails     []string
}

func newLifecycle(m *machine) *lifecycle {
	return &lifecycle{m: m, byValue: map[any]*modelInstance{}}
}

func (l *lifecycle) failf(format string, args ...any) {
	l.fails = append(l.fails, fmt.Sprintf(format, args...))
}

// ancestors reports whether scope a is at or below b.
func under(a, b int) bool {
	for s := a; s >= 0; s = parentOf[s] {
		if s == b {
			return true
		}
	}
	return false
}

// everStarted reports whether Start was called on this scope or an ancestor,
// which is what makes an OnStop a paired teardown rather than a plain
// destructor.
func (l *lifecycle) everStarted(scope int) bool {
	_, ok := l.governedBy(scope)
	return ok
}

// running reports whether the scope's Start has passed its hook phase, which
// is when an instance built later starts as part of being built. A Start that
// returned nil is past it by definition, and that is the only case the model
// claims anything about.
// running reports whether the Start that governs this scope has passed its
// hook phase, which is when an instance built later starts as part of being
// built. The scope that governs is the nearest one Start was called on, and
// that becomes the scope itself the moment its own Start records its context
// -- before it builds its eager bindings. An eager build during that window
// does not start, because that Start has not reached its hook phase yet; it
// starts in the phase that follows, if the call gets there. Walking to the
// nearest *finished* Start instead answered for an ancestor that was already
// running, and predicted a start step for an instance whose own scope's Start
// went on to fail.
func (l *lifecycle) running(scope int) bool {
	s, ok := l.governedBy(scope)
	return ok && l.phase[s] == scopeStarted
}

// built is called by every constructor the machine registers, with the scope
// the constructor ran in -- which for every lifetime is the scope that holds
// the instance and will stop it.
func (l *lifecycle) built(scope int, reg uint8, v any) {
	if _, seen := l.byValue[v]; seen {
		l.failf("the same value was built twice")
		return
	}
	l.seq++
	in := &modelInstance{
		value: v, scope: scope, hooks: hooksOfShape(reg), built: l.seq,
		ran: map[string]int{}, at: map[string]int{}, expect: map[string]bool{},
		live: l.phase[scope] != scopeStopped,
	}
	// M6: a scope that is already running starts what it builds, as part of
	// building it. A constructor that runs while its own scope is stopping is
	// undone instead, which the machine cannot reach sequentially.
	if in.live && in.hooks.start && l.running(scope) {
		in.expect["OnStart"] = true
	}
	l.instances = append(l.instances, in)
	l.byValue[v] = in
}

// hookRan is called by every hook the machine registers.
func (l *lifecycle) hookRan(v any, hook string) {
	in := l.byValue[v]
	if in == nil {
		l.failf("%s ran for a value no constructor reported", hook)
		return
	}
	l.seq++
	in.ran[hook]++
	if in.ran[hook] > 1 { // M1
		l.failf("%s ran %d times for one instance in %s", hook, in.ran[hook], l.m.names[in.scope])
	}
	if _, seen := in.at[hook]; !seen {
		in.at[hook] = l.seq
	}
}

// started records the outcome of a Start. A failed one rolls back, which the
// model hears about as a stop.
func (l *lifecycle) started(scope int, err error) {
	if err != nil {
		// A failed Start rolls back through Stop, with one exception: a
		// second Start is refused before anything happens, so it changes
		// nothing at all.
		if !strings.Contains(err.Error(), "Start called twice") {
			l.stopping(scope)
		}
		return
	}
	if l.phase[scope] == scopeNew {
		l.phase[scope] = scopeStarted
	}
	// Everything alive in the subtree has now had its start step run, in
	// build order, whether it was built before or during the call.
	for _, in := range l.instances {
		if in.live && in.hooks.start && under(in.scope, scope) {
			in.expect["OnStart"] = true
		}
	}
}

// stopping records a Stop of a scope, and settles what its subtree owed.
func (l *lifecycle) stopping(scope int) {
	if l.phase[scope] == scopeStopped {
		return
	}
	for s := range numScopes {
		if under(s, scope) {
			l.phase[s] = scopeStopped
		}
	}
	for _, in := range l.instances {
		if !in.live || !under(in.scope, scope) {
			continue
		}
		in.live = false
		// The package's own rule: a stop step is owed when the start step
		// succeeded, and when the instance was merely built but its OnStop
		// has nothing to pair with -- no OnStart, or a scope that was never
		// started, in which case OnStop is a plain destructor.
		paired := in.hooks.start && l.everStarted(in.scope)
		owed := in.ran["OnStart"] > 0 || !paired
		in.expect["OnStop"] = in.hooks.stop && owed
		in.expect["OnDrain"] = in.hooks.drain && owed
	}
}

// check reports what the model expected and the container did not do, or did
// and should not have.
func (l *lifecycle) check() []string {
	for _, in := range l.instances {
		where := l.m.names[in.scope]
		for _, hook := range []string{"OnStart", "OnDrain", "OnStop"} {
			want, got := in.expect[hook], in.ran[hook] > 0
			switch {
			case want && !got: // M3, M4, M6
				l.failf("an instance in %s owed %s and never got one", where, hook)
			case !want && got:
				l.failf("an instance in %s ran %s when none was owed", where, hook)
			}
		}
		// M2: one instance's hooks run in lifecycle order.
		for _, pair := range [][2]string{{"OnStart", "OnDrain"}, {"OnDrain", "OnStop"}, {"OnStart", "OnStop"}} {
			a, b := in.at[pair[0]], in.at[pair[1]]
			if a > 0 && b > 0 && a > b {
				l.failf("an instance in %s ran %s before %s", where, pair[1], pair[0])
			}
		}
	}
	// M5: innermost scope first, and reverse build order within a scope.
	stopped := make([]*modelInstance, 0, len(l.instances))
	for _, in := range l.instances {
		if in.at["OnStop"] > 0 {
			stopped = append(stopped, in)
		}
	}
	slices.SortFunc(stopped, func(a, b *modelInstance) int { return a.at["OnStop"] - b.at["OnStop"] })
	for i, in := range stopped {
		for _, later := range stopped[i+1:] {
			if in.scope == later.scope && in.built < later.built {
				l.failf("%s stopped two instances in build order, not in reverse", l.m.names[in.scope])
			}
			if in.scope != later.scope && under(later.scope, in.scope) {
				l.failf("%s stopped before %s, which is under it",
					l.m.names[in.scope], l.m.names[later.scope])
			}
		}
	}
	return l.fails
}

func (l *lifecycle) report() {
	if msgs := l.check(); len(msgs) > 0 {
		l.m.fail("the lifecycle model disagrees:\n  %s", strings.Join(msgs, "\n  "))
	}
}
