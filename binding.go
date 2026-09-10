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
	override bool  // declared to replace an earlier registration of the key
	isValue  bool  // registered with Value: lifetimes do not apply
	wants    []key // the parameter types of a Wire constructor; nil for a Provide closure
	build    func(*Scope) any

	// inner is the registration a Wrap composes over, bound when Wrap is
	// called, and innerAt the scope that registered it; both nil for any
	// other binding. wrappedBy is set on a binding a Wrap has bound to: an
	// Override that replaced it would leave the wrapper composing over a
	// registration that no longer serves the key.
	inner     *binding
	innerAt   *state
	wrappedBy atomic.Pointer[binding]
	onStart   func(context.Context, any) error
	onDrain   func(context.Context, any) error
	onStop    func(context.Context, any) error
	worker    func(context.Context, any) error

	// used is set once this binding has served a value. From then on the
	// registration cannot be overridden, since that would leave two live
	// instances of one service. A failed resolution built nothing and leaves
	// the key re-registerable; that is how a key whose constructor failed is
	// recovered.
	used atomic.Bool

	// resolving counts the resolutions of this binding that have not served
	// a value yet, the window used cannot cover: a constructor that registers
	// over its own key and resolves the replacement would otherwise hand the
	// nested call the new value and the outer call the old one. It is read
	// only until the first value is served; after that used says the same.
	resolving atomic.Int32

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

// validate rejects lifetime and hook combinations that cannot be honoured.
// It runs at freeze, so the order the builder methods were called in does
// not matter.
func (b *binding) validate() {
	bad := func(what, why string) {
		panic(fmt.Sprintf("di: %s (provided at %s): %s %s", b.key, b.where(), what, why))
	}
	switch {
	case b.eager && b.scoped:
		// Rejected even if a later registration overrides it. Whether an
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
// resolution from this scope.
type Binding[T any] struct {
	s *Scope
	b *binding
}

func (s *Scope) register(k key, build func(*Scope) any) *binding {
	b := &binding{key: k, site: callsite(), module: s.module, build: build}
	b.single = &instance{b: b}
	s.mu.Lock()
	s.pending = append(s.pending, b)
	s.mu.Unlock()
	return b
}

// Provide registers a lazily built singleton. T is inferred from the
// constructor's return type; dependencies are pulled with s.Get[...]().
func (s *Scope) Provide[T any](ctor func(*Scope) T) Binding[T] {
	return Binding[T]{s, s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any { return ctor(s) })}
}

// Value registers an already-built instance.
func (s *Scope) Value[T any](v T) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(*Scope) any { return v })
	b.isValue = true
	return Binding[T]{s, b}
}

func callsite() string {
	_, file, line, _ := runtime.Caller(3)
	return fmt.Sprintf("%s:%d", file, line)
}

// Wire registers a lazily built singleton from a constructor of any arity,
// whose parameters are its dependencies:
//
//	s.Wire[*Server](NewServer) // func NewServer(cfg Config, repo *Repo) *Server
//
// ctor must be a non-variadic function returning T, or T and an error, and is
// read with reflection once, here. Each parameter type is resolved from the
// same scope view a Provide closure would see, so lifetimes, cycles, hooks and
// error paths are unchanged; what Wire adds is that the dependencies are known
// at registration, before anything is built. A non-nil error from ctor aborts
// the build exactly as s.Must does.
//
// T cannot be inferred from an untyped argument, so it is spelled out, and a
// constructor whose result is not assignable to T is rejected here, with the
// other configuration errors. A concrete constructor may therefore serve an
// interface key directly: s.Wire[Repository](NewPGRepo). The build calls ctor
// through reflect, which costs about 150ns and two allocations per build over
// a Provide closure; a warm Get is the same code for both.
func (s *Scope) Wire[T any](ctor any) Binding[T] {
	want := reflect.TypeFor[T]()
	fv, ft, fails := function("Wire["+typeName(want)+"]", "constructor", ctor, want)
	wants := params(ft, 0)
	b := s.register(key{t: want}, func(s *Scope) any {
		args := make([]reflect.Value, len(wants))
		s.arguments(wants, args)
		return call(fv, args, fails, want)
	})
	b.wants = wants
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
// first, as the wrapper's dependency, and so stopped after it. The wrapper
// serves T from this scope down. In a child scope it wraps the parent's value
// for that child and its descendants and leaves the parent and its other
// children as they were, which is what uber/fx calls Decorate. Wrappers
// chain in registration order, and a wrapper takes the lifetime of what it
// wraps; Scoped() on the wrapper makes it one per resolving scope over a
// shared inner value. An Override registered afterwards replaces the wrapper
// and everything it wrapped. Nothing to wrap is rejected here, and a group
// cannot be wrapped: its members are read with All. A key this scope has
// already resolved is rejected at the next resolution, as an Override is,
// since callers already hold the unwrapped value.
func (s *Scope) Wrap[T any](fn any) Binding[T] {
	want := reflect.TypeFor[T]()
	name := "Wrap[" + typeName(want) + "]"
	fv, ft, fails := function(name, "wrapper", fn, want)
	if ft.NumIn() == 0 || !want.AssignableTo(ft.In(0)) {
		panic(fmt.Sprintf("di: %s: wrapper %s must take the %s it wraps as its first parameter", name, ft, typeName(want)))
	}
	k := key{t: want}
	// This scope is read as it is, pending batch included, because committing
	// the batch here would end it for every registration made so far.
	// Ancestors are looked up as a resolution would look them up.
	inner, at := s.current(k)
	if inner == nil && s.parent != nil {
		inner, at = (&Scope{state: s.parent}).lookup(k)
	}
	if inner == nil {
		panic(fmt.Sprintf("di: %s: nothing provides %s in scope %s or above; a group is read with All and cannot be wrapped", name, k, s.name))
	}
	wants := params(ft, 1)
	b := s.register(k, func(s *Scope) any {
		args := make([]reflect.Value, len(wants)+1)
		// The wrapped value is resolved as a dependency, which records the
		// edge, keeps build order and catches a wrapper that reaches back
		// into itself; served is marked as get would mark it.
		args[0] = argument(s.resolve(inner, at), ft.In(0))
		s.markServed(at, k)
		s.arguments(wants, args[1:])
		return call(fv, args, fails, want)
	})
	b.inner, b.innerAt, b.wants, b.scoped = inner, at, wants, inner.scoped
	inner.wrappedBy.Store(b)
	return Binding[T]{s, b}
}

var errorType = reflect.TypeFor[error]()

// function checks that fn is a non-variadic function returning want, or want
// and an error, and returns it with its type and whether it declares the
// error. name and role label the message: "di: Wire[*app.Server]: constructor
// must be a function".
func function(name, role string, fn any, want reflect.Type) (fv reflect.Value, ft reflect.Type, fails bool) {
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
	case !ft.Out(0).AssignableTo(want):
		panic(fmt.Sprintf("di: %s: %s %s returns %s", name, role, ft, typeName(ft.Out(0))))
	case ft.NumOut() == 2 && ft.Out(1) != errorType:
		panic(fmt.Sprintf("di: %s: %s %s must return T or (T, error)", name, role, ft))
	}
	return fv, ft, ft.NumOut() == 2
}

// params lists the parameter types of ft from index from on, as keys.
func params(ft reflect.Type, from int) []key {
	wants := make([]key, ft.NumIn()-from)
	for i := range wants {
		wants[i] = key{t: ft.In(i + from)}
	}
	return wants
}

// arguments resolves each of wants from s into the corresponding slot of
// args.
func (s *Scope) arguments(wants []key, args []reflect.Value) {
	for i, k := range wants {
		args[i] = argument(s.get(k), k.t)
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
	if b, ok := st.index[k]; ok {
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

// call runs a constructor through reflect and turns its error, if it
// declared one and returned it, into the abort that s.Must would raise. The
// value is stored as the registered type, not the constructor's result type:
// registration accepted any result assignable to the key, and a chan int
// stored for a <-chan int key would pass every check until Get asserted it.
// An interface key needs no conversion, since the assertion to an interface
// is what accepts the concrete value.
func call(fv reflect.Value, args []reflect.Value, fails bool, want reflect.Type) any {
	out := fv.Call(args)
	if fails && !out[1].IsNil() {
		panic(abort{out[1].Interface().(error)})
	}
	v := out[0]
	if v.Type() != want && want.Kind() != reflect.Interface {
		v = v.Convert(want)
	}
	return v.Interface()
}

// edit applies a builder method to the binding, rejecting one made after the
// scope committed the registration.
func (b Binding[T]) edit(f func(*binding)) Binding[T] {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	if b.s.frozen && !slices.Contains(b.s.pending, b.b) {
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
// at the next resolution, naming both sites, because a duplicate that wins
// silently is how one module reroutes another module's wiring without anyone
// noticing. With it the later registration serves the key, and inherits its
// eagerness, which is the test seam:
//
//	s := di.Test(t, app.Production)
//	s.Value(&DB{DSN: "sqlite://memory"}).Override()
//
// There must be something to override in this scope, or that is rejected too:
// a fake for a service that has since been renamed would otherwise be a
// registration nobody resolves, and the test would pass against production
// wiring. A child scope shadows its parent without Override, since that is a
// different registry rather than a replacement. A key that has already served
// a value cannot be overridden at all.
func (b Binding[T]) Override() Binding[T] {
	return b.edit(func(b *binding) { b.override = true })
}

// Scoped makes the binding one-per-scope: each scope that resolves it gets
// its own instance, built in that scope (so it can see that scope's
// values) and stopped with it. Declare request-scoped services once in the
// root and resolve them through the request scope.
func (b Binding[T]) Scoped() Binding[T] {
	return b.edit(func(b *binding) { b.scoped = true })
}

// Eager builds the service during Start rather than on first use.
//
// Eagerness belongs to the key, not the registration: it means the service
// exists by the time Start returns. Overriding an eager binding therefore
// keeps the key eager and builds the replacement; a replacement with a
// per-scope lifetime, which cannot be built once at Start, is rejected.
func (b Binding[T]) Eager() Binding[T] { return b.edit(func(b *binding) { b.eager = true }) }

// OnStart runs once the service is built, and only a hook that returns
// normally starts it: one that panics fails the start step, like a panicking
// constructor, and the service is never served. The hooks are typed: no
// interface sniffing, no reflection.
func (b Binding[T]) OnStart(f func(context.Context, T) error) Binding[T] {
	return b.edit(func(b *binding) { b.onStart = func(ctx context.Context, v any) error { return f(ctx, as[T](v)) } })
}

// OnDrain runs before anything is stopped: Stop drains the whole tree, from
// the innermost scope outwards and in reverse build order, while every scope
// still resolves normally. It is where a service stops accepting new work and
// waits for the work it already has, such as an HTTP server that must finish
// in-flight requests whose handlers still need their request scope. Anything
// those handlers build, including a request scope of their own, is drained
// before the phase ends. Use OnStop for the release that follows.
func (b Binding[T]) OnDrain(f func(context.Context, T) error) Binding[T] {
	return b.edit(func(b *binding) { b.onDrain = func(ctx context.Context, v any) error { return f(ctx, as[T](v)) } })
}

// OnStop releases the service, in reverse build order, once its drain step
// and its child scopes are done. It runs when OnStart succeeded, or when
// there is no OnStart to pair with, in which case it is a plain destructor;
// a service whose start step failed is not stopped.
func (b Binding[T]) OnStop(f func(context.Context, T) error) Binding[T] {
	return b.edit(func(b *binding) { b.onStop = func(ctx context.Context, v any) error { return f(ctx, as[T](v)) } })
}

// Go registers a worker for T: a long-running function, such as a consumer
// loop, that runs in a goroutine of its own for as long as the service does.
// It is the contract of errgroup's Go, applied to a service. The worker
// starts once the service has started; its context is cancelled when the
// service stops, and Stop waits for it to return, bounded by Stop's own
// context. A worker that outlasts that deadline is reported by Stop, and
// OnStop then waits for it rather than releasing the value underneath a
// worker still reading it.
//
// Returning a non-nil error calls Shutdown with it, stopping the application,
// even if the scope was already stopping: a worker may fail, flush while the
// scope winds down, and only then report. The exception is context.Canceled
// from a worker that was already cancelled, which is a worker reporting the
// cancellation and nothing else. A worker that wants to stay quiet during
// shutdown should return nil.
func (b Binding[T]) Go(f func(context.Context, T) error) Binding[T] {
	return b.edit(func(b *binding) { b.worker = func(ctx context.Context, v any) error { return f(ctx, as[T](v)) } })
}
