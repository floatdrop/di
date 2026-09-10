package di

// Resolution: the path a resolution walks, the two cycle detectors, the
// build step of an instance, and the entry points that resolve by type.

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// abort is the panic a wiring failure unwinds with. It reaches the nearest
// Resolve, Start or Run and becomes that call's error; a top-level Get panics
// with the plain error instead.
type abort struct{ err error }

// resolver is one node of a resolution path: what is being resolved and the
// node that needed it. The path is a linked list, not a slice, because a
// constructor may resolve from several goroutines at once; a node is never
// mutated after it is made (except done), so branches share nothing and each
// carries the whole path for cycle detection and error messages.
//
// A node is identified by binding and holder, not by key: a group member and
// a plain registration of the same type are different bindings, and one
// Scoped binding is a different node in each scope that holds an instance of
// it.
type resolver struct {
	parent *resolver
	b      *binding // nil on the root node, which resolves nothing itself
	holder *state

	// done marks a node whose resolution has returned. The path stays whole
	// for error messages, but a finished node is no longer a dependency: a
	// constructor may keep the Scope it was handed and resolve through it
	// later, and that resolution must not meet its own finished frame and be
	// called a cycle. Written once by the resolution that owns the node, read
	// from any branch.
	done atomic.Bool
}

func (r *resolver) child(b *binding, holder *state) *resolver {
	return &resolver{parent: r, b: b, holder: holder}
}

// onPath reports whether this exact binding is still being resolved further
// up the path, which is a dependency cycle within one branch.
//
// The walk stops at the first finished node rather than skipping it. Only a
// constructor that kept its Scope can put one on a live path, and a
// resolution made through that Scope afterwards is a new branch: what is
// above the finished node may still be building, but not for it, so it has
// only to wait. The one shape this cannot tell apart without goroutine-local
// state deadlocks instead of being reported: a constructor that blocks on a
// resolution made through a finished descendant's Scope that leads back to
// itself.
func (r *resolver) onPath(b *binding, holder *state) bool {
	for n := r; n != nil; n = n.parent {
		if n.done.Load() {
			return false
		}
		if n.b == b && n.holder == holder {
			return true
		}
	}
	return false
}

func (r *resolver) path() string {
	if r == nil || r.b == nil {
		return ""
	}
	return " (needed by " + r.pathStr() + ")"
}

func (r *resolver) pathStr() string {
	var parts []string
	for n := r; n != nil; n = n.parent {
		if n.b != nil {
			parts = append(parts, n.b.key.String())
		}
	}
	slices.Reverse(parts)
	return fmt.Sprint(parts)
}

// graph is one container's wait-for graph, with two kinds of edge: an
// instance points at the resolution building it (instance.builder), and a
// blocked resolution points at the instance it waits for (blockedFor). Its
// mutex is the innermost lock: a state's mutex may be held while taking it,
// never the reverse, so the graph can be read across scopes without ordering
// state mutexes against each other.
//
// New makes one graph per container and every scope under that root shares
// it. That is as far as a cycle can reach: a resolution follows the parent
// chain, so a wait can cross scopes, but nothing joins two containers.
type graph struct {
	mu         sync.Mutex
	blockedFor map[*resolver]*instance
}

// descends reports whether n is anc or was created below it. A branch blocks
// at a leaf of its path, several nodes below the one that claimed the build it
// is holding up, so both directions of the graph are matched against whole
// paths rather than single nodes.
//
// The walk stops at a node whose resolution has returned, for the same reason
// onPath does: nothing above a finished node is waiting for what is opened
// below it later.
func descends(n, anc *resolver) bool {
	for ; n != nil; n = n.parent {
		if n == anc {
			return true
		}
		if n.done.Load() {
			return false
		}
	}
	return false
}

// wait records that r is about to wait for in, unless that would close a
// wait-for cycle by reaching, through builds that are themselves blocked, a
// build this branch is responsible for finishing. Called with the holder's
// mutex held; the check and the new edge are one critical section, so two
// branches closing a cycle at once cannot both decide to wait.
func (r *resolver) wait(g *graph, in *instance) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	seen := map[*instance]bool{in: true}
	for stack := []*instance{in}; len(stack) > 0; {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		builder := cur.builder
		if builder == nil {
			continue // nobody is building it: whoever holds it will settle it
		}
		if descends(r, builder) {
			return false // waiting on our own branch's work
		}
		for n, j := range g.blockedFor {
			if descends(n, builder) && !seen[j] {
				seen[j] = true
				stack = append(stack, j)
			}
		}
	}
	g.blockedFor[r] = in
	return true
}

func (r *resolver) unwait(g *graph) {
	g.mu.Lock()
	delete(g.blockedFor, r)
	g.mu.Unlock()
}

// as unwraps a stored value. A nil interface is a legitimate service, and a
// nil any cannot be asserted back to the interface type it was stored as, so
// it becomes T's zero value rather than a panic. Every hand-back of a stored
// value goes through here.
func as[T any](v any) T {
	if v == nil {
		var zero T
		return zero
	}
	return v.(T)
}

// lookup finds the binding registered for k in this scope or an ancestor,
// and the scope that owns it. A nil binding means nothing is registered for
// k anywhere in the chain.
func (s *Scope) lookup(k key) (*binding, *state) {
	for st := s.state; st != nil; st = st.parent {
		st.freeze()
		st.mu.Lock()
		b, ok := st.index[k]
		st.mu.Unlock()
		if ok {
			return b, st
		}
	}
	return nil, nil
}

// inFlight reports whether this Scope is a live view of a resolution: it
// carries a path whose last node has not returned. A Scope kept past that
// point, by a constructor or by a Child made in one, is not in flight, and
// calls through it are top-level calls.
func (s *Scope) inFlight() bool { return s.r != nil && !s.r.done.Load() }

// enter returns a view carrying a resolver, starting a new resolution unless
// one is in flight.
func (s *Scope) enter() *Scope {
	if s.inFlight() {
		return s
	}
	return s.view(&resolver{})
}

// get resolves k. Outside a constructor the internal abort is converted into
// a panic carrying the plain error; inside one it unwinds to the enclosing
// Resolve/Start call.
func (s *Scope) get(k key) any {
	if !s.inFlight() {
		defer unwrapAbort()
		return s.enter().get(k)
	}
	b, owner := s.lookup(k)
	if b == nil {
		panic(abort{fmt.Errorf("di: %s: %w%s", k, ErrNotProvided, s.r.path())})
	}
	v := s.resolve(b, owner)
	s.markServed(owner, k)
	return v
}

// markServed records that k was served to this scope from owner, in every
// scope between the two. binding.used protects the owner; the scopes in
// between each handed out a value for k as well, and registering k in one of
// them afterwards would give the key two live values there.
func (s *Scope) markServed(owner *state, k key) {
	for st := s.state; st != nil && st != owner; st = st.parent {
		st.mu.Lock()
		if st.served == nil {
			st.served = make(map[key]bool, 4)
		}
		st.served[k] = true
		st.mu.Unlock()
	}
}

// resolve produces b's value for the resolving scope s, honouring the
// binding's lifetime and starting the instance when the scope is running.
func (s *Scope) resolve(b *binding, owner *state) any {
	if s.isStopped() {
		panic(abort{fmt.Errorf("di: %s: %w%s", b.key, ErrStopped, s.r.path())})
	}
	// The holder owns the instance's lifecycle: a singleton lives in the
	// scope that registered the binding, a scoped one in the scope that
	// resolves it, so it can see that scope's values.
	holder := owner
	if b.scoped {
		holder = s.state
	}
	if s.r.onPath(b, holder) {
		panic(abort{fmt.Errorf("di: %w: %s -> %s", ErrCycle, s.r.pathStr(), b.key)})
	}
	if !b.used.Load() {
		// Hold the key against an override for as long as this resolution
		// runs, so a constructor cannot replace the registration it is
		// itself being built from. used is set before this is dropped, so
		// the two guards never leave a gap between them.
		b.resolving.Add(1)
		defer b.resolving.Add(-1)
	}
	sc := s.view(s.r.child(b, holder))
	// The node stops being a dependency when this resolution returns, however
	// it returns. See resolver.done.
	defer sc.r.done.Store(true)

	in := holder.instanceFor(b)
	v, err := sc.await(in, holder)
	if err != nil {
		panic(abort{err})
	}
	b.used.Store(true)
	// The edge belongs to the node that asked, and only a node with a binding
	// has an instance to record it on: a top-level Get starts a path whose
	// first node has none, and so does a Scope kept past its resolution. The
	// test is here rather than in dependOn so that the warm path pays a
	// pointer comparison instead of a call. A failed resolution records
	// nothing; the path it failed on is in the error.
	if s.r.b != nil {
		s.r.dependOn(in, holder)
	}
	return v
}

// dependOn records that the resolution at this node needed in, once per
// distinct dependency: a constructor that asks for the same service twice
// gets one edge. The scan is over one constructor's own dependencies and runs
// only while it is building.
func (r *resolver) dependOn(in *instance, holder *state) {
	r.holder.mu.Lock()
	defer r.holder.mu.Unlock()
	// resolve made the asking instance before running its constructor, so
	// the nil check is a guard on the recording only: a mistake here must
	// not break resolution.
	asker := r.holder.instanceAt(r.b)
	if asker == nil || slices.ContainsFunc(asker.deps, func(d dep) bool { return d.in == in }) {
		return
	}
	asker.deps = append(asker.deps, dep{in: in, holder: holder})
}

// await returns the instance's value: this branch builds it if it gets there
// first, and otherwise waits for whoever did. It waits for the start step as
// well, so a resolution of a running scope never hands out a service whose
// OnStart is still in flight. A wait that would close a cycle between two
// concurrent builds is reported as ErrCycle rather than deadlocking.
func (s *Scope) await(in *instance, holder *state) (any, error) {
	holder.mu.Lock()
	for in.ph == phaseNew || !in.settled || in.ph == phaseStarting {
		if in.ph == phaseNew {
			in.claimBuild(holder, s.r)
			holder.mu.Unlock()
			s.materialise(in, holder)
			holder.mu.Lock()
			continue
		}
		// The phase says which step is outstanding and so which channel to
		// block on. Both are read in this critical section, and the owner
		// closes the channel under the same mutex, so it cannot be closed
		// between the choice and the block.
		var ready chan struct{}
		if in.settled {
			ready = waitOn(&in.startingCh) // settled, so OnStart is outstanding
		} else {
			ready = waitOn(&in.settledCh)
		}
		if !s.r.wait(holder.graph, in) {
			holder.mu.Unlock()
			return nil, fmt.Errorf("di: %w: %s -> %s", ErrCycle, s.r.parent.pathStr(), in.b.key)
		}
		holder.mu.Unlock()
		<-ready
		s.r.unwait(holder.graph)
		holder.mu.Lock()
	}
	value, err := in.value, in.err
	if err == nil && s.isStopped() {
		// The scope stopped while this branch was building or waiting;
		// resolve's check was before the wait. The check is on the resolving
		// scope, which covers the holder (always that scope or an ancestor):
		// a stopped scope must refuse the request whether or not the value is
		// still alive above it.
		value, err = nil, fmt.Errorf("di: %s: %w", in.b.key, ErrStopped)
	}
	holder.mu.Unlock()
	return value, err
}

// claimBuild takes the build step for this resolution. Called with the
// holder's mutex held.
func (in *instance) claimBuild(holder *state, r *resolver) {
	in.ph = phaseBuilding
	g := holder.graph
	g.mu.Lock()
	in.builder = r
	g.mu.Unlock()
}

// settle publishes the outcome of the build step and wakes every resolution
// waiting for this instance.
func (in *instance) settle(holder *state) {
	holder.mu.Lock()
	in.settled = true
	g := holder.graph
	g.mu.Lock()
	in.builder = nil
	g.mu.Unlock()
	wake(in.settledCh)
	holder.mu.Unlock()
}

// fail records a build failure, which is terminal for the instance.
func (in *instance) fail(holder *state, err error) {
	holder.mu.Lock()
	in.ph, in.err = phaseFailed, err
	holder.mu.Unlock()
}

// materialise builds an instance, once. A failure is recorded on the instance
// rather than unwound, so every later resolution reports it identically, and
// the instance is settled on the way out so waiters are released whatever
// happened.
func (s *Scope) materialise(in *instance, holder *state) {
	defer in.settle(holder)
	if err := s.construct(in, holder); err != nil {
		in.fail(holder, err)
		return
	}
	if !in.publish(holder) {
		return
	}
	in.startIfRunning(holder)
}

// construct runs the constructor, turning a panic or an abort from a nested
// resolution into an error, and reports the attempt to observers either way.
func (s *Scope) construct(in *instance, holder *state) (err error) {
	b := in.b
	t0 := time.Now()
	defer func() {
		if rec := recover(); rec != nil {
			if a, ok := rec.(abort); ok {
				err = fmt.Errorf("di: building %s (provided at %s): %w", b.key, b.where(), a.err)
			} else {
				err = fmt.Errorf("di: building %s (provided at %s): panic: %v", b.key, b.where(), rec)
			}
		}
		holder.report(EventBuild, b, t0, err)
	}()
	in.value = b.build(&Scope{state: holder, r: s.r, module: b.module})
	return nil
}

// publish adds the instance to its owner's stop list so it will be torn
// down. If Stop ran while the constructor was in flight its snapshot did not
// include this instance, so undo it here and report ErrStopped instead.
func (in *instance) publish(owner *state) bool {
	owner.mu.Lock()
	stopped := owner.isStopped()
	in.ph = phaseBuilt
	if !stopped {
		owner.started = append(owner.started, in)
	}
	owner.mu.Unlock()
	if !stopped {
		return true
	}
	err := errors.Join(fmt.Errorf("di: %s: %w", in.b.key, ErrStopped), in.stopIfNeeded(owner.stopContext(), owner))
	owner.mu.Lock()
	in.err = err
	owner.mu.Unlock()
	return false
}

// startIfRunning runs the start step when the scope is already running.
// publish strictly precedes the read below, and Start sets running before it
// drains, so either this starts the instance or Start's drain finds it.
// startClaimed records its failure on the instance, so a resolution that
// waited for the step reports it too.
func (in *instance) startIfRunning(owner *state) {
	if sctx, running := owner.runContext(); running && in.claim(owner) {
		_ = in.startClaimed(sctx, owner)
	}
	stopped := owner.isStopped()
	owner.mu.Lock()
	if in.err == nil && stopped {
		// Stop ran while we were starting and waited for the step, so the
		// instance is torn down: do not hand it out.
		in.err = fmt.Errorf("di: %s: %w", in.b.key, ErrStopped)
	}
	owner.mu.Unlock()
}

// Get resolves T. Inside a constructor, failure unwinds to the enclosing
// Resolve/Start call and becomes an error; at top level it panics. In a
// goroutine a constructor started, use Resolve instead: that panic has no
// enclosing call to unwind to and would take the process down.
func (s *Scope) Get[T any]() T { return as[T](s.get(key{t: reflect.TypeFor[T]()})) }

// Maybe resolves T if it is provided anywhere in the scope chain.
func (s *Scope) Maybe[T any]() (T, bool) {
	if b, _ := s.lookup(key{t: reflect.TypeFor[T]()}); b == nil {
		var zero T
		return zero, false
	}
	return s.Get[T](), true
}

// All resolves the multi-binding group for T across the scope chain. Members
// are singletons (or Scoped if so marked) with the same lifecycle
// as any other binding.
func (s *Scope) All[T any]() []T {
	if !s.inFlight() {
		defer unwrapAbort()
		return s.enter().All[T]()
	}
	k := key{t: reflect.TypeFor[T]()}
	var out []T
	for st := s.state; st != nil; st = st.parent {
		st.freeze()
		st.mu.Lock()
		bs := slices.Clone(st.groups[k])
		st.mu.Unlock()
		for _, b := range bs {
			out = append(out, as[T](s.resolve(b, st)))
		}
	}
	return out
}

// Must unwraps a (value, error) pair inside a constructor:
//
//	db := s.Must(sql.Open("postgres", dsn))
//
// A non-nil error aborts the constructor and surfaces from the enclosing
// Resolve, Start or Run. Outside a constructor it panics with the error.
func (s *Scope) Must[T any](v T, err error) T {
	if err == nil {
		return v
	}
	if s.inFlight() {
		panic(abort{err})
	}
	panic(err)
}

// Resolve resolves T, reporting a wiring failure as an error rather than a
// panic. It is the entry point for a goroutine a constructor started.
func (s *Scope) Resolve[T any]() (v T, err error) {
	defer recoverAbort(&err)
	return as[T](s.enter().get(key{t: reflect.TypeFor[T]()})), nil
}

// unwrapAbort turns an abort into a panic carrying the plain error, which is
// what a top-level Get or All reports. Deferred only by an entry point that
// is not already inside a resolution.
func unwrapAbort() {
	if rec := recover(); rec != nil {
		if a, ok := rec.(abort); ok {
			panic(a.err)
		}
		panic(rec)
	}
}

// recoverAbort turns an abort into *err and re-panics anything else.
func recoverAbort(err *error) {
	if rec := recover(); rec != nil {
		if a, ok := rec.(abort); ok {
			*err = a.err
			return
		}
		panic(rec)
	}
}
