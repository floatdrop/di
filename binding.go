package di

// Registration: what a binding is, the methods that make one, and the typed
// handle that refines it. Nothing here builds anything; a binding's build
// func is called by the resolution in resolve.go.

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"sync/atomic"
)

// binding is one registration: its key, lifetime, hooks and build func.
type binding struct {
	key      key
	site     string
	module   string // the Module this was registered from, or ""
	group    bool
	scoped   bool
	eager    bool
	override bool   // declared to replace an earlier registration of the key
	isValue  bool   // registered with Value: lifetimes do not apply
	wants    []want // the parameters of a Wire constructor; nil for a Provide closure
	build    func(*Scope) any

	// inner is the registration a Wrap composes over, bound when Wrap is
	// called, and innerAt the scope that registered it; both nil otherwise.
	inner   *binding
	innerAt *state

	onStart func(context.Context, any) error
	onDrain func(context.Context, any) error
	onStop  func(context.Context, any) error
	worker  func(context.Context, any) error

	guard // what stops this registration being replaced; see guard

	single *instance // the singleton; scoped bindings keep one instance per state
}

// where names the registration for a message: its site, and the module it was
// registered from when there is one, as in "storage (wire.go:12)".
func (b *binding) where() string {
	if b.module == "" {
		return b.site
	}
	return b.module + " (" + b.site + ")"
}

// once rejects a second registration of a hook the binding already carries:
// a binding has one of each, so assigning over the first would drop work the
// caller asked for. Unlike validate this cannot wait for freeze — the field
// is written here — so it names the site of the second call.
func (b *binding) once(what string, cur func(context.Context, any) error, at string) {
	if cur != nil {
		panic(fmt.Sprintf("di: %s (provided at %s): a second %s is registered at %s; one binding has one %s", b.key, b.where(), what, at, what))
	}
}

// validate rejects lifetime and hook combinations that cannot be honoured. It
// runs at freeze, so the order the builder methods were called in does not
// matter.
func (b *binding) validate() {
	bad := func(what, why string) {
		panic(fmt.Sprintf("di: %s (provided at %s): %s %s", b.key, b.where(), what, why))
	}
	switch {
	case b.eager && b.scoped:
		// Rejected even if a later registration overrides it; whether an
		// override inherits eagerness is decided in deriveEager.
		bad("Eager", "does not apply to a Scoped binding: it is not built once")
	case b.isValue && b.scoped:
		bad("Scoped", "is meaningless for a Value binding: the instance already exists")
	case b.group && b.override:
		bad("Override", "does not apply to a group member: members accumulate rather than replace one another")
	case b.inner != nil && b.group:
		bad("Group", "does not apply to a wrapper: it serves the key it wraps")
	case b.inner != nil && b.override:
		bad("Override", "does not apply to a wrapper: it composes over the registration it wraps rather than replacing it")
	}
}

// Binding is the typed handle returned by Provide, Value, Wire and Wrap. Its
// methods refine the registration; they must be called before the first
// resolution from this scope, and each hook at most once.
type Binding[T any] struct {
	s *Scope
	b *binding
}

// register makes a binding and queues it for the next freeze. init runs
// before the binding is queued: once it is in pending, freeze and teardown
// read it under the scope's mutex, and a field written afterwards races both.
func (s *Scope) register(k key, build func(*Scope) any, init func(*binding)) *binding {
	b := &binding{key: k, site: callsite(2), module: s.module, build: build}
	b.single = &instance{b: b}
	if init != nil {
		init(b)
	}
	s.st.mu.Lock()
	s.st.pending = append(s.st.pending, b)
	s.st.hasPending.Store(true)
	stopped := s.st.stopped.Load()
	s.st.mu.Unlock()
	if stopped && b.inner != nil {
		// teardown collected the scope's wrappers before this one was queued,
		// and a stopped scope never serves the key, so drop the mark here.
		b.inner.unwrap(b)
	}
	return b
}

// Provide registers a lazily built singleton. T is inferred from the
// constructor's return type; dependencies are pulled with s.Get[...]().
func (s *Scope) Provide[T any](ctor func(*Scope) T) Binding[T] {
	return Binding[T]{s, s.register(s.key(reflect.TypeFor[T]()), func(s *Scope) any { return ctor(s) }, nil)}
}

// Value registers an already-built instance.
func (s *Scope) Value[T any](v T) Binding[T] {
	b := s.register(s.key(reflect.TypeFor[T]()), func(*Scope) any { return v }, func(b *binding) { b.isValue = true })
	return Binding[T]{s, b}
}

// callsite is the file:line skip frames above its caller. register passes 2,
// for itself and the registration method the user called, so every
// registration method must call register directly.
func callsite(skip int) string {
	_, file, line, _ := runtime.Caller(skip + 1)
	return fmt.Sprintf("%s:%d", file, line)
}

// Wire registers a lazily built singleton from a constructor of any arity,
// whose parameters are its dependencies:
//
//	s.Wire[*Server](NewServer) // func NewServer(cfg Config, repo *Repo) *Server
//
// ctor must be a non-variadic function returning T, or T and an error, and is
// read with reflection once, here. Each parameter is resolved as a Provide
// closure would resolve it, so lifetimes, cycles, hooks and error paths are
// the same; what Wire adds is that the dependencies are known at
// registration. A non-nil error from ctor aborts the build as s.Must does.
//
// T is spelled out because it cannot be inferred from an untyped argument. A
// result merely assignable to T is accepted, so a concrete constructor may
// serve an interface key: s.Wire[Repository](NewPGRepo).
func (s *Scope) Wire[T any](ctor any) Binding[T] {
	served := reflect.TypeFor[T]()
	fv, ft, fails := function("Wire["+typeName(served)+"]", "constructor", ctor, served)
	wants := params(ft, 0)
	b := s.register(s.key(served), func(s *Scope) any {
		args := make([]reflect.Value, len(wants))
		s.arguments(wants, args)
		return call(fv, args, fails, served)
	}, func(b *binding) { b.wants = wants })
	return Binding[T]{s, b}
}

// Wrap registers a wrapper over the registration that serves T when Wrap is
// called: the latest one in this scope, or the one an ancestor provides. fn
// takes the value being wrapped first and its other dependencies after it,
// read with reflection as Wire reads a constructor, and returns T, or T and
// an error:
//
//	s.Wrap[Store](func(next Store, c *Cache) Store { return &caching{next, c} })
//
// What is wrapped keeps its registration, hooks and lifetime: it is built
// first, as the wrapper's dependency, and stopped after it. The wrapper serves
// T from this scope down; in a child scope it wraps the parent's value for
// that child alone. Wrappers chain in registration order and take the
// lifetime of what they wrap; Scoped() on the wrapper makes it one per
// resolving scope over a shared inner value. An Override registered afterwards
// replaces the wrapper and everything it wrapped. Nothing to wrap is rejected
// here, and a group cannot be wrapped. A key this scope has already resolved
// is rejected at the next resolution, as an Override is.
func (s *Scope) Wrap[T any](fn any) Binding[T] {
	served := reflect.TypeFor[T]()
	name := "Wrap[" + typeName(served) + "]"
	fv, ft, fails := function(name, "wrapper", fn, served)
	if ft.NumIn() == 0 || !served.AssignableTo(ft.In(0)) {
		panic(fmt.Sprintf("di: %s: wrapper %s must take the %s it wraps as its first parameter", name, ft, typeName(served)))
	}
	k := s.key(served)
	// This scope is read pending batch included, without committing it:
	// committing here would end the batch for every registration so far.
	// Ancestors are looked up as a resolution would look them up.
	inner, at := s.st.current(k)
	if inner == nil && s.st.parent != nil {
		inner, at = (&Scope{st: s.st.parent}).lookup(k)
	}
	if inner == nil {
		panic(fmt.Sprintf("di: %s: nothing provides %s in scope %s or above; a group is read with All and cannot be wrapped", name, k, s.st.name))
	}
	wants := params(ft, 1)
	b := s.register(k, func(s *Scope) any {
		args := make([]reflect.Value, len(wants)+1)
		// Resolved as a dependency, which records the edge, keeps build order
		// and catches a wrapper that reaches back into itself.
		args[0] = argument(s.resolve(inner, at), ft.In(0))
		s.markServed(at, k)
		s.arguments(wants, args[1:])
		return call(fv, args, fails, served)
	}, func(b *binding) {
		b.inner, b.innerAt, b.wants, b.scoped = inner, at, wants, inner.scoped
		inner.addWrapper(b)
	})
	return Binding[T]{s, b}
}

// guard is what stops a registration being replaced once that would leave two
// live values for its key, or a wrapper over a registration nothing else can
// reach: a value served, a resolution in flight, a wrapper in a live scope.
// The fourth guard, a scope that handed the key down from an ancestor, is
// state.served, since it belongs to that scope.
//
// resolve writes used and resolving from whichever scope is resolving, without
// the owner's mutex, which keeps a warm resolution off that mutex; so they are
// atomics. wraps is a small set, replaced whole by compare-and-swap.
type guard struct {
	// used is set once this binding has served a value. A failed resolution
	// built nothing and leaves the key re-registerable.
	used atomic.Bool

	// resolving counts the resolutions of this binding that have not served a
	// value yet, the window used cannot cover: a constructor that registers
	// over its own key and resolves the replacement would otherwise hand the
	// nested call the new value and the outer call the old one.
	resolving atomic.Int32

	// wraps holds the Wraps bound to this binding whose scopes are still
	// alive, in registration order, and whether the binding is retired. Wrap
	// adds itself; the scope that registered a wrapper removes it as it stops.
	// It is a set because sibling scopes wrap one parent registration
	// independently, and one stopping must not release the others.
	//
	// retired is set on a wrapper in a chain an Override replaced. It never
	// serves from its own scope again, but a live wrapper in a descendant may
	// still compose over it, so it keeps its mark on what it wraps until
	// nothing wraps it; see release.
	//
	// Both change rarely, so they are one immutable wrapSet, nil for a binding
	// nobody has wrapped or retired.
	wraps atomic.Pointer[wrapSet]
}

// against says why replacer may not replace or wrap the guarded registration,
// or returns "" when nothing stops it. The reasons read as the end of a
// sentence: "cannot be overridden at wire.go:9: it has already been resolved".
func (g *guard) against(replacer *binding) string {
	switch {
	case g.used.Load():
		return "it has already been resolved"
	case g.resolving.Load() > 0:
		return "it is being resolved"
	}
	if replacer.inner == nil {
		// An Override, not a Wrap: a wrapper over this registration would go
		// on serving a value built from something nothing else can reach.
		if w := g.wrapper(); w != nil {
			return "it is wrapped at " + w.where()
		}
	}
	return ""
}

// wrapSet is a guard's wrappers and retired flag. It is never written after
// it is stored.
type wrapSet struct {
	wrappers []*binding
	retired  bool
}

// update replaces the guard's wrapSet with what f makes of the current one,
// retrying if another update landed first. f must not modify the slice it is
// given. An empty, unretired set is stored as nil.
func (g *guard) update(f func(cur wrapSet) wrapSet) {
	for {
		old := g.wraps.Load()
		var cur wrapSet
		if old != nil {
			cur = *old
		}
		next := f(cur)
		var p *wrapSet
		if len(next.wrappers) > 0 || next.retired {
			p = &next
		}
		if g.wraps.CompareAndSwap(old, p) {
			return
		}
	}
}

// addWrapper records a Wrap bound to the guarded registration.
func (g *guard) addWrapper(w *binding) {
	g.update(func(cur wrapSet) wrapSet {
		cur.wrappers = append(slices.Clip(cur.wrappers), w)
		return cur
	})
}

// dropWrapper forgets a Wrap that can no longer serve. A wrapper is added
// before anything can drop it, so one not in the set was dropped already.
func (g *guard) dropWrapper(w *binding) {
	if s := g.wraps.Load(); s == nil || !slices.Contains(s.wrappers, w) {
		return
	}
	g.update(func(cur wrapSet) wrapSet {
		cur.wrappers = slices.DeleteFunc(slices.Clone(cur.wrappers), func(x *binding) bool { return x == w })
		return cur
	})
}

// retire marks the guarded registration as a wrapper in a chain an Override
// replaced.
func (g *guard) retire() {
	g.update(func(cur wrapSet) wrapSet {
		cur.retired = true
		return cur
	})
}

// isRetired reports whether retire was called.
func (g *guard) isRetired() bool {
	s := g.wraps.Load()
	return s != nil && s.retired
}

// unwrap forgets w, a wrapper bound to b that can no longer serve, and lets b
// go too if b was retired and w was the last thing keeping it.
func (b *binding) unwrap(w *binding) {
	b.dropWrapper(w)
	b.release()
}

// release drops the mark a retired binding holds on what it wraps once nothing
// live wraps it, and carries on down the chain while each link is retired and
// unwrapped in turn. A link still wrapped keeps its mark: the wrapper over it
// composes over everything below.
func (b *binding) release() {
	for r := b; r.inner != nil && r.isRetired() && r.wrapper() == nil; r = r.inner {
		r.inner.dropWrapper(r)
	}
}

// wrapper returns the first live Wrap bound to the guarded registration, or
// nil.
func (g *guard) wrapper() *binding {
	if s := g.wraps.Load(); s != nil && len(s.wrappers) > 0 {
		return s.wrappers[0]
	}
	return nil
}

var errorType = reflect.TypeFor[error]()

// function checks that fn is a non-variadic function returning served, or
// served and an error, and returns it with its type and whether it declares
// the error. name and role label the message: "di: Wire[*app.Server]: constructor
// must be a function".
func function(name, role string, fn any, served reflect.Type) (fv reflect.Value, ft reflect.Type, fails bool) {
	fv = reflect.ValueOf(fn)
	if !fv.IsValid() || fv.Kind() != reflect.Func {
		panic(fmt.Sprintf("di: %s: %s must be a function, got %T", name, role, fn))
	}
	ft = fv.Type()
	switch {
	case ft.IsVariadic():
		panic(fmt.Sprintf("di: %s: %s %s is variadic", name, role, ft))
	case ft.NumOut() == 0 || ft.NumOut() > 2:
		panic(fmt.Sprintf("di: %s: %s %s must return T or (T, error)", name, role, ft))
	case !ft.Out(0).AssignableTo(served):
		panic(fmt.Sprintf("di: %s: %s %s returns %s", name, role, ft, typeName(ft.Out(0))))
	case ft.NumOut() == 2 && ft.Out(1) != errorType:
		panic(fmt.Sprintf("di: %s: %s %s must return T or (T, error)", name, role, ft))
	}
	return fv, ft, ft.NumOut() == 2
}

// want is one parameter of a Wire constructor: the key it resolves, the
// parameter's own type, and how the parameter is filled. Binding.Needs is the
// only thing that changes a kind.
type want struct {
	k     key          // the dependency; for a group, the member type
	param reflect.Type // the parameter's type, which for a group is a slice of k.t
	kind  wantKind
}

// wantKind says how a parameter is resolved: as Get, as Maybe, or as All.
type wantKind uint8

const (
	wantValue wantKind = iota
	wantOptional
	wantGroup
)

// params lists the parameter types of ft from index from on, each resolved by
// key until Needs says otherwise.
func params(ft reflect.Type, from int) []want {
	wants := make([]want, ft.NumIn()-from)
	for i := range wants {
		t := ft.In(i + from)
		wants[i] = want{k: key{t: t}, param: t}
	}
	return wants
}

// arguments resolves each of wants from s into the corresponding slot of
// args, by the kind Needs left on it.
func (s *Scope) arguments(wants []want, args []reflect.Value) {
	for i, w := range wants {
		switch w.kind {
		case wantOptional:
			v, _ := s.maybe(w.k)
			args[i] = argument(v, w.param)
		case wantGroup:
			members := s.all(w.k)
			if len(members) == 0 {
				args[i] = reflect.Zero(w.param) // the nil slice All returns
				continue
			}
			slice := reflect.MakeSlice(w.param, len(members), len(members))
			for j, v := range members {
				slice.Index(j).Set(argument(v, w.param.Elem()))
			}
			args[i] = slice
		default:
			args[i] = argument(s.get(w.k), w.param)
		}
	}
}

// current is the registration serving k in this scope as of now, pending or
// committed, read without committing anything.
func (st *state) current(k key) (*binding, *state) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, b := range slices.Backward(st.pending) {
		if b.key == k && !b.group {
			return b, st
		}
	}
	if b, ok := st.reg.Load().index[k]; ok {
		return b, st
	}
	return nil, nil
}

// argument makes a stored value into an argument of type t. A nil interface
// is a legitimate service, and reflect.ValueOf(nil) is not a value of any
// type; see as.
func argument(v any, t reflect.Type) reflect.Value {
	if v == nil {
		return reflect.Zero(t)
	}
	return reflect.ValueOf(v)
}

// call runs a constructor through reflect and turns a declared, returned
// error into the abort s.Must would raise. The value is stored as the
// registered type, not the result type: registration accepted any assignable
// result, and a chan int stored for a <-chan int key would pass every check
// until Get asserted it. An interface key needs no conversion.
func call(fv reflect.Value, args []reflect.Value, fails bool, served reflect.Type) any {
	out := fv.Call(args)
	if fails && !out[1].IsNil() {
		panic(abort{out[1].Interface().(error)})
	}
	v := out[0]
	if v.Type() != served && served.Kind() != reflect.Interface {
		v = v.Convert(served)
	}
	return v.Interface()
}

// edit applies a builder method to the binding, rejecting one made after the
// scope committed the registration.
func (b Binding[T]) edit(f func(*binding)) Binding[T] {
	b.s.st.mu.Lock()
	defer b.s.st.mu.Unlock()
	if b.s.st.frozen && !slices.Contains(b.s.st.pending, b.b) {
		panic(fmt.Sprintf("di: %s (provided at %s) modified after the scope was first resolved", b.b.key, b.b.where()))
	}
	f(b.b)
	return b
}

// Group makes the binding a member of the multi-binding group for T instead
// of the binding for T: it neither shadows nor is shadowed by another
// registration of T, and the members are read back together with s.All[T]().
// A member keeps its own lifetime and hooks.
func (b Binding[T]) Group() Binding[T] {
	return b.edit(func(b *binding) { b.group = true })
}

// Override declares that this registration replaces an earlier one of the same
// key in the same scope. Without it a second registration of a key is rejected
// at the next resolution, naming both sites. With it the later registration
// serves the key and inherits its eagerness, which is the test seam:
//
//	s := di.Test(t, app.Production)
//	s.Value(&DB{DSN: "sqlite://memory"}).Override()
//
// There must be something to override in this scope, or that is rejected too,
// since a fake for a renamed service would otherwise be a registration nobody
// resolves. A child scope shadows its parent without Override. A key that has
// already served a value cannot be overridden at all.
func (b Binding[T]) Override() Binding[T] {
	return b.edit(func(b *binding) { b.override = true })
}

// Need says how one parameter of a Wire constructor is resolved, for
// Binding.Needs. Make one with Optional, AllOf or Tagged.
type Need struct {
	k    key
	kind wantKind
}

// String names the need the way it was written, for a message.
func (n Need) String() string {
	switch {
	case n.k.t == nil:
		return "the zero Need"
	case n.kind == wantGroup:
		return "AllOf[" + n.k.String() + "]"
	case n.k.tag != nil:
		return "Tagged[" + typeName(n.k.t) + ", " + typeName(n.k.tag) + "]"
	}
	return "Optional[" + n.k.String() + "]"
}

// Tagged is a Need for the parameter of type T, which takes the instance
// registered through Scope.Tag under Tag rather than the one under T.
func Tagged[T any, Tag any]() Need {
	return Need{k: key{t: reflect.TypeFor[T](), tag: reflect.TypeFor[Tag]()}}
}

// Optional is a Need for a parameter of type T that may go unprovided: the
// constructor is given T if anything provides it and the zero value if
// nothing does, as Scope.Maybe resolves it.
func Optional[T any]() Need {
	return Need{k: key{t: reflect.TypeFor[T]()}, kind: wantOptional}
}

// AllOf is a Need for a []T parameter holding the group for T, as Scope.All
// resolves it: every member across the scope chain, in build order, and
// nothing at all when the group is empty.
func AllOf[T any]() Need {
	return Need{k: key{t: reflect.TypeFor[T]()}, kind: wantGroup}
}

// Needs says how to fill the parameters that are not plain dependencies, so
// that a constructor stays a function anyone can call:
//
//	// func NewRouter(rs []Route, t *Tracer) *Router
//	s.Wire[*Router](NewRouter).Needs(di.AllOf[Route](), di.Optional[*Tracer]())
//
// Each Need is matched to the parameter it describes by type — Optional[T] to
// a T, AllOf[T] to a []T, Tagged[T, Tag] to a T — so the order of the
// parameters is the constructor's business and only the type has to agree.
// A Need matching no parameter is rejected here, as is one matching two,
// which type cannot tell apart, a second Need for one parameter, and Needs
// on anything but a Wire or Wrap registration: a closure resolves what it
// needs itself.
//
// What this buys over Scope.Maybe and Scope.All is that the dependency stays
// declared: Scope.Validate checks it, Scope.Explain draws it before anything
// is built, and Scope.Modules lists it.
func (b Binding[T]) Needs(needs ...Need) Binding[T] {
	at := callsite(1)
	return b.edit(func(b *binding) {
		if b.wants == nil {
			panic(fmt.Sprintf("di: %s (provided at %s): Needs at %s applies to a Wire or Wrap constructor, whose parameters are known; a closure resolves what it needs itself",
				b.key, b.where(), at))
		}
		for _, n := range needs {
			b.need(n, at)
		}
	})
}

// need applies one Need to the parameter it describes. A Need is matched by
// type, so it must match exactly one: two parameters of one type cannot be
// told apart here, and they would get one value anyway.
func (b *binding) need(n Need, at string) {
	bad := func(why string, args ...any) {
		panic(fmt.Sprintf("di: %s (provided at %s): Needs at %s %s", b.key, b.where(), at, fmt.Sprintf(why, args...)))
	}
	if n.k.t == nil {
		bad("was given the zero Need; make one with Optional, AllOf or Tagged")
	}
	param := n.k.t
	if n.kind == wantGroup {
		param = reflect.SliceOf(param)
	}
	matches := 0
	for _, w := range b.wants {
		if w.param == param {
			matches++
		}
	}
	i := slices.IndexFunc(b.wants, func(w want) bool { return w.param == param })
	switch {
	case i < 0:
		bad("(%s) matches no %s parameter of the constructor", n, typeName(param))
	case matches > 1:
		bad("(%s) matches %d %s parameters, which type cannot tell apart; a key has one value, so take it once",
			n, matches, typeName(param))
	case b.wants[i].kind != wantValue || b.wants[i].k.tag != nil:
		bad("(%s) would change the %s parameter, already resolved as %s", n, typeName(param), b.wants[i].how())
	}
	b.wants[i] = want{k: n.k, param: param, kind: n.kind}
}

// how names the Need that set a want, for a message.
func (w want) how() string {
	if w.k.tag != nil {
		return "Tagged"
	}
	return w.kind.String()
}

// String names a kind the way the Need that set it was written.
func (k wantKind) String() string {
	switch k {
	case wantOptional:
		return "Optional"
	case wantGroup:
		return "AllOf"
	}
	return "a dependency"
}

// Scoped makes the binding one-per-scope: each scope that resolves it gets
// its own instance, built in that scope (so it can see that scope's values)
// and stopped with it. Declare request-scoped services once in the root and
// resolve them through the request scope.
func (b Binding[T]) Scoped() Binding[T] {
	return b.edit(func(b *binding) { b.scoped = true })
}

// Eager builds the service during Start rather than on first use.
//
// Eagerness belongs to the key, not the registration: it means the service
// exists by the time Start returns. Overriding an eager binding keeps the key
// eager and builds the replacement; a replacement with a per-scope lifetime
// is rejected.
func (b Binding[T]) Eager() Binding[T] { return b.edit(func(b *binding) { b.eager = true }) }

// OnStart runs once the service is built. Only a hook that returns normally
// starts it: one that panics fails the start step, like a panicking
// constructor, and the service is never served.
//
// The hook's context bounds the start: under Run it expires with
// StartTimeout, so a hook that keeps a context for work outliving the start
// should take Scope.Context instead.
//
// A binding has one OnStart, and a second is rejected: put the whole start in
// one hook.
func (b Binding[T]) OnStart(f func(context.Context, T) error) Binding[T] {
	at := callsite(1)
	return b.edit(func(b *binding) {
		b.once("OnStart hook", b.onStart, at)
		b.onStart = func(ctx context.Context, v any) error { return f(ctx, as[T](v)) }
	})
}

// OnDrain runs before anything is stopped: Stop drains the whole tree, from
// the innermost scope outwards and in reverse build order, while every scope
// still resolves. It is where a service stops accepting new work and waits
// for the work it already has, such as an HTTP server finishing in-flight
// requests whose handlers still need their request scope. Anything those
// handlers build, including a request scope, is drained before the phase
// ends. Use OnStop for the release that follows.
//
// A binding has one OnDrain, and a second is rejected.
func (b Binding[T]) OnDrain(f func(context.Context, T) error) Binding[T] {
	at := callsite(1)
	return b.edit(func(b *binding) {
		b.once("OnDrain hook", b.onDrain, at)
		b.onDrain = func(ctx context.Context, v any) error { return f(ctx, as[T](v)) }
	})
}

// OnStop releases the service, in reverse build order, once its drain step
// and its child scopes are done. It runs when OnStart succeeded, or when there
// is no OnStart to pair with, in which case it is a plain destructor; a
// service whose start step failed is not stopped.
//
// A binding has one OnStop, and a second is rejected: release everything the
// service holds in one hook, so the order within it is the caller's.
func (b Binding[T]) OnStop(f func(context.Context, T) error) Binding[T] {
	at := callsite(1)
	return b.edit(func(b *binding) {
		b.once("OnStop hook", b.onStop, at)
		b.onStop = func(ctx context.Context, v any) error { return f(ctx, as[T](v)) }
	})
}

// Go registers a worker for T: a long-running function, such as a consumer
// loop, that runs in a goroutine of its own for as long as the service does,
// as errgroup's Go does for a group. The worker starts once the service has
// started; its context is cancelled when the service stops, and Stop waits
// for it to return, bounded by Stop's own context. A worker that outlasts
// that deadline is reported by Stop, and OnStop then waits for it rather
// than releasing the value underneath it.
//
// Returning a non-nil error calls Shutdown with it, stopping the application,
// even if the scope was already stopping. The exception is context.Canceled
// from a worker that was already cancelled. A worker that wants to stay quiet
// during shutdown should return nil.
//
// A binding has one worker, and a second Go is rejected: start the other
// goroutines from within the one worker, which is where their lifetime is
// already tied to the service's.
func (b Binding[T]) Go(f func(context.Context, T) error) Binding[T] {
	at := callsite(1)
	return b.edit(func(b *binding) {
		b.once("Go worker", b.worker, at)
		b.worker = func(ctx context.Context, v any) error { return f(ctx, as[T](v)) }
	})
}
