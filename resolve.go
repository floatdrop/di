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
// node that needed it. The path is a linked list because a constructor may
// resolve from several goroutines at once; a node is never mutated after it
// is made, except done, so branches share nothing.
//
// A node is identified by binding and holder, not by key: a group member and
// a plain registration of the same type are different bindings, and one
// Scoped binding is a different node in each scope that holds an instance.
type resolver struct {
	parent *resolver
	b      *binding // nil on the root node, which resolves nothing itself
	holder *state

	// done marks a node whose resolution has returned. The path stays whole
	// for error messages, but a finished node is no longer a dependency: a
	// constructor may keep its Scope and resolve through it later, and that
	// resolution must not meet its own finished frame and be called a cycle.
	// Written once by the resolution that owns the node, read from any branch.
	done atomic.Bool
}

// topLevel is the root node of every resolution begun outside a constructor.
// It resolves nothing, is never finished and never builds, so one serves all.
var topLevel = &resolver{}

func (r *resolver) child(b *binding, holder *state) *resolver {
	return &resolver{parent: r, b: b, holder: holder}
}

// onPath reports whether this exact binding is still being resolved further
// up the path, which is a dependency cycle within one branch.
//
// The walk stops at the first finished node rather than skipping it: what is
// above it may still be building, but not for this branch, so a resolution
// through a kept Scope has only to wait. The one shape this cannot tell apart
// without goroutine-local state deadlocks instead of being reported: a
// constructor that blocks on a resolution made through a finished
// descendant's Scope that leads back to itself.
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
// blocked resolution points at the instance it waits for (a waitEdge). Its
// mutex is the innermost lock: a state's mutex may be held while taking it,
// never the reverse. One graph per container is as far as a cycle can reach.
type graph struct {
	mu sync.Mutex

	// under indexes every wait by each node of the blocked resolution's path
	// below the root, up to and including the first finished one, as the path
	// stood when it blocked. The root builds nothing, so no search reads it.
	// A node only ever becomes finished, so the set descends can match only
	// shrinks and the index is a superset of it: wait narrows the search to
	// under[builder] and descends still decides.
	under map[*resolver]map[*waitEdge]struct{}
}

// waitEdge is one wait: the blocked resolution, the instance it waits for,
// and the nodes it was indexed under. Each wait has its own, so removing one
// never depends on another wait by the same resolution.
type waitEdge struct {
	r    *resolver
	in   *instance
	path []*resolver
}

// descends reports whether n is anc or was created below it. A branch blocks
// at a leaf of its path, below the node that claimed the build it is holding
// up, so both directions of the graph are matched against whole paths. The
// walk stops at a finished node for the same reason onPath does.
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

// wait records that r is about to wait for in and returns the edge to hand to
// unwait, or nil when waiting would close a wait-for cycle: reaching, through
// builds that are themselves blocked, a build this branch must finish. Called
// with the holder's mutex held; the check and the new edge are one critical
// section, so two branches closing a cycle at once cannot both decide to wait.
func (r *resolver) wait(g *graph, in *instance) *waitEdge {
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
			return nil // waiting on our own branch's work
		}
		for e := range g.under[builder] {
			if !seen[e.in] && descends(e.r, builder) {
				seen[e.in] = true
				stack = append(stack, e.in)
			}
		}
	}
	e := &waitEdge{r: r, in: in}
	for n := r; n != nil && n.b != nil; n = n.parent {
		e.path = append(e.path, n)
		set := g.under[n]
		if set == nil {
			set = map[*waitEdge]struct{}{}
			g.under[n] = set
		}
		set[e] = struct{}{}
		if n.done.Load() {
			break // descends matches a node before asking whether it finished
		}
	}
	return e
}

// unwait removes the wait e recorded.
func (g *graph) unwait(e *waitEdge) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, n := range e.path {
		set := g.under[n]
		delete(set, e)
		if len(set) == 0 {
			delete(g.under, n)
		}
	}
}

// as unwraps a stored value. A nil interface is a legitimate service, and a
// nil any cannot be asserted back to the interface type it was stored as, so
// it becomes T's zero value. Every hand-back of a stored value goes through
// here.
func as[T any](v any) T {
	if v == nil {
		var zero T
		return zero
	}
	return v.(T)
}

// lookup finds the binding registered for k in this scope or an ancestor,
// and the scope that owns it, or nil.
func (st *state) lookup(k key) (*binding, *state) {
	for ; st != nil; st = st.parent {
		st.freeze()
		if b, ok := st.reg.Load().index[k]; ok {
			return b, st
		}
	}
	return nil, nil
}

// inFlight reports whether this Scope is a live view of a resolution: it
// carries a path whose last node has not returned. A Scope kept past that
// point makes top-level calls.
func (s *Scope) inFlight() bool { return s.r != nil && !s.r.done.Load() }

// enter returns a view carrying a resolver, starting a new resolution unless
// one is in flight.
func (s *Scope) enter() *Scope {
	if s.inFlight() {
		return s
	}
	return s.view(topLevel)
}

// get resolves k. Outside a constructor the internal abort becomes a panic
// carrying the plain error; inside one it unwinds to the enclosing
// Resolve/Start call.
func (s *Scope) get(k key) any {
	if !s.inFlight() {
		defer unwrapAbort()
		return s.enter().get(k)
	}
	for {
		b, owner := s.st.lookup(k)
		if b != nil && !s.st.claimed(k, owner) {
			s.refuseIfStopped(k)
			b, owner = s.st.claim(k)
		}
		if b == nil {
			panic(abort{fmt.Errorf("di: %s: %w%s", k, ErrNotProvided, s.r.path())})
		}
		if v, ok := s.resolveBy(b, owner, true); ok {
			return v
		}
	}
}

// refuseIfStopped aborts a resolution of k from a stopped scope.
func (s *Scope) refuseIfStopped(k key) {
	if s.st.isStopped() {
		panic(abort{fmt.Errorf("di: %s: %w%s", k, ErrStopped, s.r.path())})
	}
}

// claimed reports whether owner, found by a lookup from this scope, is this
// scope or the owner of its recorded route. Every scope on a recorded route is
// marked and registers nothing for k, so a lookup that found that owner,
// however long ago, still would; one that found another is stale.
func (st *state) claimed(k key, owner *state) bool {
	if owner == st {
		return true
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return owner.descendsFrom(st.served[k])
}

// claim marks every scope from this one up to the owner of k, checking each
// for its own registration in the critical section freeze commits under. It
// ends at a registration, or at a scope with a recorded route, above which it
// looks k up once that record is read; then it records the owner on every
// scope it marked. The caller's lookup froze the scopes up to the owner.
func (st *state) claim(k key) (*binding, *state) {
	var b *binding
	var owner, end *state
	for at := st; at != nil && b == nil; at = at.parent {
		if b = at.reg.Load().index[k]; b != nil { // the owner is not locked
			owner, end = at, at
			break
		}
		at.mu.Lock()
		to := at.served[k]
		if b = at.reg.Load().index[k]; b == nil {
			at.markLocked(k)
		}
		at.mu.Unlock()
		switch {
		case b != nil:
			owner, end = at, at
		case to != nil:
			b, owner = at.parent.lookup(k)
			end = at
		}
	}
	for at := st; at != end; at = at.parent {
		at.record(k, owner)
	}
	return b, owner
}

// markLocked marks k as handed down from this scope, keeping a recorded owner.
// Called with the scope's mutex held.
func (st *state) markLocked(k key) {
	if _, ok := st.served[k]; ok {
		return
	}
	if st.served == nil {
		st.served = make(map[key]*state, 4)
	}
	st.served[k] = nil
}

// record notes that the route from this scope to owner is marked.
func (st *state) record(k key, owner *state) {
	st.mu.Lock()
	if !owner.descendsFrom(st.served[k]) {
		st.markLocked(k)
		st.served[k] = owner
	}
	st.mu.Unlock()
}

// markBound marks the scopes from this one up to, not including, owner for a
// Wrap's bound route, passing over any that registers k itself, committed or
// pending. It records the route here only if it passed over none, since a
// record promises a route with no registration of k on it.
func (st *state) markBound(owner *state, k key) {
	if st == owner {
		return
	}
	whole := true
	for at := st; at != nil && at != owner; at = at.parent {
		at.mu.Lock()
		if owner.descendsFrom(at.served[k]) {
			at.mu.Unlock()
			if at == st {
				return
			}
			break
		}
		if b, _ := at.currentLocked(k); b == nil {
			at.markLocked(k)
		} else {
			whole = false
		}
		at.mu.Unlock()
	}
	if whole {
		st.record(k, owner)
	}
}

// decline is one optional key a build asked for and did not find, and the
// scope it asked from, which is the holder or a descendant of it.
type decline struct {
	k    key
	from *state
}

// declined records the miss on the instance whose constructor asked, as
// dependOn records an edge and readGroup records a group read. It guards
// nothing: whether a key is provided is a question about the chain as it
// stands, which a child scope may answer differently anyway, so a key
// registered afterwards is reported by Explain rather than rejected.
func (r *resolver) declined(k key, from *state) {
	r.holder.mu.Lock()
	defer r.holder.mu.Unlock()
	asker := r.holder.instanceAt(r.b)
	if asker == nil || slices.Contains(asker.declines, decline{k, from}) {
		return
	}
	asker.declines = append(asker.declines, decline{k, from})
}

// groupRead is one group a build read: the key, the scope it read from, and
// the members it got. A member registered into that chain afterwards is not
// in seen, and the value built from the read does not have it.
type groupRead struct {
	k    key
	from *state
	seen []*binding
}

// readGroup records the read on the instance whose constructor made it, as
// declined records a miss. A group is read-time and scope-dependent by
// contract too, so a later member is likewise reported rather than rejected.
// A second read of the same group from the same scope merges into the first,
// so a constructor that reads in a loop keeps one record, and a member any of
// its reads saw is one it has.
func (r *resolver) readGroup(k key, from *state, seen []*binding) {
	r.holder.mu.Lock()
	defer r.holder.mu.Unlock()
	asker := r.holder.instanceAt(r.b)
	if asker == nil {
		return
	}
	if i := slices.IndexFunc(asker.reads, func(o groupRead) bool { return o.k == k && o.from == from }); i >= 0 {
		for _, b := range seen {
			if !slices.Contains(asker.reads[i].seen, b) {
				asker.reads[i].seen = append(asker.reads[i].seen, b)
			}
		}
		return
	}
	asker.reads = append(asker.reads, groupRead{k: k, from: from, seen: seen})
}

// resolve produces b's value for the resolving scope s, honouring the
// binding's lifetime and starting the instance when the scope is running.
func (s *Scope) resolve(b *binding, owner *state) any {
	v, _ := s.resolveBy(b, owner, false)
	return v
}

// resolveBy is resolve for a binding found by key when byKey is set: it
// reports false, having built nothing, when an Override replaced b after it
// was looked up.
func (s *Scope) resolveBy(b *binding, owner *state, byKey bool) (any, bool) {
	s.refuseIfStopped(b.key)
	// The holder owns the instance: the registering scope for a singleton,
	// the resolving scope for a Scoped binding.
	holder := owner
	if b.scoped {
		holder = s.st
	}
	if s.r.onPath(b, holder) {
		panic(abort{fmt.Errorf("di: %w: %s -> %s", ErrCycle, s.r.pathStr(), b.key)})
	}
	used := b.used.Load()
	if !used {
		// Hold the key against an override while this resolution runs, so a
		// constructor cannot replace the registration it is built from. used
		// is set before this is dropped, and a freeze claims before it reads
		// used, so the two guards leave no gap.
		if !b.hold(owner, byKey) {
			return nil, false
		}
		defer b.resolving.Add(-1)
	}
	in := holder.instanceFor(b)
	var v any
	if used && in.ready.Load() && !s.st.isStopped() {
		// The warm path, without the holder's mutex and without a node: the
		// build is settled, no start step is owed or in flight, and the value
		// is final. ready is written under that mutex at every change that
		// could make the answer differ (see refresh), so a load that sees it
		// set is ordered before any such change. Nothing it does writes a
		// line other cores read.
		v = in.value
	} else {
		sc := s.view(s.r.child(b, holder))
		// The node stops being a dependency when this resolution returns,
		// however it returns. See resolver.done.
		defer sc.r.done.Store(true)
		var err error
		if v, err = sc.await(in, holder); err != nil {
			panic(abort{err})
		}
		if !b.used.Load() {
			b.used.Store(true)
		}
	}
	// The edge belongs to the node that asked, and only a node with a binding
	// has an instance to record it on: a top-level Get, or a Scope kept past
	// its resolution, has none. The test is here rather than in dependOn so
	// the warm path pays a pointer comparison instead of a call. A failed
	// resolution records nothing.
	if s.r.b != nil {
		s.r.dependOn(in, holder)
	}
	return v, true
}

// dependOn records that the resolution at this node needed in, once per
// distinct dependency. The scan is over one constructor's own dependencies
// and runs only while it is building.
func (r *resolver) dependOn(in *instance, holder *state) {
	r.holder.mu.Lock()
	defer r.holder.mu.Unlock()
	// The asking instance exists, since resolve made it before running the
	// constructor; the nil check guards the recording only, because a mistake
	// here must not break resolution.
	asker := r.holder.instanceAt(r.b)
	if asker == nil || slices.ContainsFunc(asker.deps, func(d dep) bool { return d.in == in }) {
		return
	}
	asker.deps = append(asker.deps, dep{in: in, holder: holder})
}

// await returns the instance's value: this branch builds it if it gets there
// first, and otherwise waits for whoever did. It waits for the start step as
// well, so a running scope never hands out a service whose OnStart is in
// flight. A wait that would close a cycle is reported as ErrCycle.
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
		// closes the channel under the same mutex.
		var ready chan struct{}
		if in.settled {
			ready = waitOn(&in.startingCh) // settled, so OnStart is outstanding
		} else {
			ready = waitOn(&in.settledCh)
		}
		e := s.r.wait(holder.graph, in)
		if e == nil {
			holder.mu.Unlock()
			return nil, fmt.Errorf("di: %w: %s -> %s", ErrCycle, s.r.parent.pathStr(), in.b.key)
		}
		holder.mu.Unlock()
		<-ready
		holder.graph.unwait(e)
		holder.mu.Lock()
	}
	value, err := in.value, in.err
	if err == nil && s.st.isStopped() {
		// The scope stopped while this branch was building or waiting. The
		// check is on the resolving scope, which covers the holder: a stopped
		// scope must refuse whether or not the value is still alive above it.
		value, err = nil, fmt.Errorf("di: %s: %w", in.b.key, ErrStopped)
	}
	holder.mu.Unlock()
	return value, err
}

// claimBuild takes the build step for this resolution. Called with the
// holder's mutex held.
func (in *instance) claimBuild(holder *state, r *resolver) {
	in.ph = phaseBuilding
	in.refresh()
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
	in.refresh()
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
	in.refresh()
	holder.mu.Unlock()
}

// materialise builds an instance, once. A failure is recorded on the instance
// rather than unwound, so every later resolution reports it identically, and
// the instance is settled on the way out whatever happened.
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
	in.value = b.build(&Scope{st: holder, r: s.r, module: b.module})
	return nil
}

// publish adds the instance to its owner's stop list. If the scope stopped
// while the constructor ran, Stop's snapshot did not include the instance,
// so it is undone here and reported as ErrStopped.
func (in *instance) publish(owner *state) bool {
	owner.mu.Lock()
	stopped := owner.isStopped()
	in.ph = phaseBuilt
	in.refresh()
	if !stopped {
		owner.started = append(owner.started, in)
	}
	owner.mu.Unlock()
	if !stopped && !owner.announce() {
		return true
	}
	err := errors.Join(fmt.Errorf("di: %s: %w", in.b.key, ErrStopped), in.stopIfNeeded(owner.stopContext(), owner))
	owner.mu.Lock()
	in.err = err
	in.refresh()
	owner.mu.Unlock()
	return false
}

// startIfRunning runs the start step when the scope is already running.
// publish precedes the read of running, and Start sets running before it
// drains, so either this starts the instance or Start's drain finds it.
func (in *instance) startIfRunning(owner *state) {
	if sctx, running := owner.runContext(); running && in.claim(owner) && in.gateStart(owner) {
		_ = in.startClaimed(sctx, owner)
	}
	stopped := owner.isStopped()
	owner.mu.Lock()
	if in.err == nil && stopped {
		// Stop waited for the start step and tore the instance down.
		in.err = fmt.Errorf("di: %s: %w", in.b.key, ErrStopped)
		in.refresh()
	}
	owner.mu.Unlock()
}

// Get resolves T. Inside a constructor, failure unwinds to the enclosing
// Resolve/Start call and becomes an error; at top level it panics. In a
// goroutine a constructor started, use Resolve instead: that panic has no
// enclosing call to unwind to.
func (s *Scope) Get[T any]() T { return as[T](s.get(key{t: reflect.TypeFor[T]()})) }

// Maybe resolves T if it is provided anywhere in the scope chain.
//
// Whether T is provided is a question about the chain as it stands, and a
// scope below may answer it differently, so registering T afterwards is not
// rejected. A constructor's miss is recorded, though: Scope.Explain of T
// names the services that were built without it, which is where a dependency
// wired too late shows up. A miss outside a constructor records nothing,
// since no value was built on the answer.
func (s *Scope) Maybe[T any]() (T, bool) {
	v, ok := s.maybe(key{t: reflect.TypeFor[T]()})
	return as[T](v), ok
}

// maybe is Maybe by key, shared with the Optional parameter of a Wire
// constructor.
func (s *Scope) maybe(k key) (any, bool) {
	if b, _ := s.st.lookup(k); b == nil {
		if s.inFlight() && s.r.b != nil {
			s.r.declined(k, s.st)
		}
		return nil, false
	}
	return s.get(k), true
}

// All resolves the multi-binding group for T across the scope chain. Members
// have the same lifetimes and lifecycle as any other binding.
func (s *Scope) All[T any]() []T {
	var out []T // nil for a group with no members, as this has always returned
	for _, v := range s.all(key{t: reflect.TypeFor[T]()}) {
		out = append(out, as[T](v))
	}
	return out
}

// all is All by key, shared with the AllOf parameter of a Wire constructor.
func (s *Scope) all(k key) []any {
	if !s.inFlight() {
		defer unwrapAbort()
		return s.enter().all(k)
	}
	var out []any
	var members []*binding
	for st := s.st; st != nil; st = st.parent {
		st.freeze()
		for _, b := range st.reg.Load().groups[k] { // immutable: freeze appends to a copy
			out = append(out, s.resolve(b, st))
			members = append(members, b)
		}
	}
	if s.r.b != nil {
		// Inside a build, so the value keeps these members however the group
		// grows afterwards; Explain reports one that arrives too late.
		s.r.readGroup(k, s.st, members)
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
// what a top-level Get or All reports.
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
