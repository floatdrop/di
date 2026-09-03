// Package di is a dependency-injection container for Go 1.27+ built on
// generic methods: services are registered with s.Provide(...) and resolved
// with s.Get[T]().
package di

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"sync"
)

// ---- keys ------------------------------------------------------------------

// Key identifies a service: its Go type plus an optional name. Keys compare by
// reflect.Type identity, so same-named types in different packages never
// collide (unlike fmt.Sprintf("%T")-derived names).
type Key[T any] struct{ name string }

func Named[T any](name string) Key[T] { return Key[T]{name: name} }

type key struct {
	t    reflect.Type
	name string
}

func (k key) String() string {
	if k.name == "" {
		return typeName(k.t)
	}
	return typeName(k.t) + "#" + k.name
}

func typeName(t reflect.Type) string {
	if t.Kind() == reflect.Pointer {
		return "*" + typeName(t.Elem())
	}
	if t.PkgPath() != "" {
		return t.PkgPath() + "." + t.Name()
	}
	return t.String()
}

// ---- errors ----------------------------------------------------------------

var (
	ErrNotProvided = errors.New("not provided")
	ErrCycle       = errors.New("dependency cycle")
)

type abort struct{ err error }

// ---- state -----------------------------------------------------------------

type binding struct {
	key       key
	site      string
	group     bool
	transient bool
	eager     bool
	build     func(*Scope) any
	onStart   func(context.Context, any) error
	onStop    func(context.Context, any) error

	once  sync.Once
	value any
	err   error
}

type state struct {
	name   string
	parent *state

	mu      sync.Mutex
	pending []*binding // registrations not yet indexed
	index   map[key]*binding
	groups  map[reflect.Type][]*binding
	frozen  bool
	started []*binding // instantiation order; stopped in reverse
}

func (st *state) freeze() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.pending) == 0 {
		return
	}
	st.frozen = true
	for _, b := range st.pending {
		if b.group {
			st.groups[b.key.t] = append(st.groups[b.key.t], b)
		} else {
			st.index[b.key] = b // later registration wins; that is the override rule
		}
	}
	st.pending = nil
}

// resolver is the per-resolution context: the path being built, for cycle
// detection and error messages.
type resolver struct{ stack []key }

// Scope is a container. A Scope value handed to a constructor is a view over
// the same state that carries the current resolution path.
type Scope struct {
	*state
	r *resolver
}

func New() *Scope { return &Scope{state: newState("root", nil)} }

func newState(name string, parent *state) *state {
	return &state{name: name, parent: parent, index: map[key]*binding{}, groups: map[reflect.Type][]*binding{}}
}

func (s *Scope) Child(name string) *Scope { return &Scope{state: newState(name, s.state)} }

func (s *Scope) view(r *resolver) *Scope { return &Scope{state: s.state, r: r} }

func callsite() string {
	_, file, line, _ := runtime.Caller(3)
	return fmt.Sprintf("%s:%d", file, line)
}

// ---- registration (generic methods) ---------------------------------------

// Binding is the typed handle returned by Provide/Value/Bind/Add. Its
// methods refine the registration; they must be called before the first
// resolution from this scope.
type Binding[T any] struct {
	s *Scope
	b *binding
}

func (s *Scope) register(k key, build func(*Scope) any) *binding {
	b := &binding{key: k, site: callsite(), build: build}
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
	return Binding[T]{s, s.register(key{t: reflect.TypeFor[T]()}, func(*Scope) any { return v })}
}

// Bind registers an alias: requests for I are served by T's binding.
// Both parameters are explicit: s.Bind[Reader, *Repo]().
func (s *Scope) Bind[I, T any]() Binding[I] {
	if !reflect.TypeFor[T]().Implements(reflect.TypeFor[I]()) {
		panic(fmt.Sprintf("di: %s does not implement %s", typeName(reflect.TypeFor[T]()), typeName(reflect.TypeFor[I]())))
	}
	return Binding[I]{s, s.register(key{t: reflect.TypeFor[I]()}, func(s *Scope) any { return any(s.Get[T]()).(I) })}
}

// Add appends to the multi-binding group for T; read back with s.All[T]().
func (s *Scope) Add[T any](ctor func(*Scope) T) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any { return ctor(s) })
	b.group = true
	return Binding[T]{s, b}
}

func (b Binding[T]) edit(f func(*binding)) Binding[T] {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	if b.s.frozen && !slices.Contains(b.s.pending, b.b) {
		panic(fmt.Sprintf("di: %s (provided at %s) modified after the scope was first resolved", b.b.key, b.b.site))
	}
	f(b.b)
	return b
}

func (b Binding[T]) Named(name string) Binding[T] {
	return b.edit(func(b *binding) { b.key.name = name })
}
func (b Binding[T]) Transient() Binding[T] { return b.edit(func(b *binding) { b.transient = true }) }
func (b Binding[T]) Eager() Binding[T]     { return b.edit(func(b *binding) { b.eager = true }) }

// Typed lifecycle hooks: no interface sniffing, no reflection.
func (b Binding[T]) OnStart(f func(context.Context, T) error) Binding[T] {
	return b.edit(func(b *binding) { b.onStart = func(ctx context.Context, v any) error { return f(ctx, v.(T)) } })
}
func (b Binding[T]) OnStop(f func(context.Context, T) error) Binding[T] {
	return b.edit(func(b *binding) { b.onStop = func(ctx context.Context, v any) error { return f(ctx, v.(T)) } })
}

// ---- resolution ------------------------------------------------------------

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

// enter returns a view carrying a resolver, starting a new resolution when
// called outside a constructor.
func (s *Scope) enter() *Scope {
	if s.r != nil {
		return s
	}
	return s.view(&resolver{})
}

// get resolves k. Outside a constructor the internal abort is converted into
// a panic carrying the plain error; inside one it unwinds to the enclosing
// Resolve/Start call.
func (s *Scope) get(k key) any {
	if s.r == nil {
		defer func() {
			if rec := recover(); rec != nil {
				if a, ok := rec.(abort); ok {
					panic(a.err)
				}
				panic(rec)
			}
		}()
		return s.enter().get(k)
	}
	r := s.r
	b, owner := s.lookup(k)
	if b == nil {
		panic(abort{fmt.Errorf("di: %s: %w%s", k, ErrNotProvided, r.path())})
	}
	if slices.Contains(r.stack, k) {
		panic(abort{fmt.Errorf("di: %w: %s -> %s", ErrCycle, r.pathStr(), k)})
	}
	r.stack = append(r.stack, k)
	defer func() { r.stack = r.stack[:len(r.stack)-1] }()

	ownerView := (&Scope{state: owner}).view(r)
	if b.transient {
		return b.build(ownerView)
	}
	b.once.Do(func() {
		defer func() {
			if rec := recover(); rec != nil {
				if a, ok := rec.(abort); ok {
					b.err = fmt.Errorf("di: building %s (provided at %s): %w", k, b.site, a.err)
					return
				}
				b.err = fmt.Errorf("di: building %s (provided at %s): panic: %v", k, b.site, rec)
			}
		}()
		b.value = b.build(ownerView)
		owner.mu.Lock()
		owner.started = append(owner.started, b)
		owner.mu.Unlock()
	})
	if b.err != nil {
		panic(abort{b.err})
	}
	return b.value
}

func (r *resolver) path() string {
	if len(r.stack) == 0 {
		return ""
	}
	return " (needed by " + r.pathStr() + ")"
}
func (r *resolver) pathStr() string {
	parts := make([]string, len(r.stack))
	for i, k := range r.stack {
		parts[i] = k.String()
	}
	return fmt.Sprint(parts)
}

// Get resolves T. Inside a constructor, failure unwinds to the enclosing
// Resolve/Start call and becomes an error; at top level it panics.
func (s *Scope) Get[T any]() T { return s.get(key{t: reflect.TypeFor[T]()}).(T) }

// Lookup resolves a named key: s.Lookup(di.Named[*DB]("replica")).
func (s *Scope) Lookup[T any](k Key[T]) T {
	return s.get(key{t: reflect.TypeFor[T](), name: k.name}).(T)
}

// Maybe resolves T if it is provided anywhere in the scope chain.
func (s *Scope) Maybe[T any]() (T, bool) {
	if b, _ := s.lookup(key{t: reflect.TypeFor[T]()}); b == nil {
		var zero T
		return zero, false
	}
	return s.Get[T](), true
}

// All resolves the multi-binding group for T across the scope chain.
func (s *Scope) All[T any]() []T {
	if s.r == nil {
		defer func() {
			if rec := recover(); rec != nil {
				if a, ok := rec.(abort); ok {
					panic(a.err)
				}
				panic(rec)
			}
		}()
		return s.enter().All[T]()
	}
	var out []T
	for st := s.state; st != nil; st = st.parent {
		st.freeze()
		st.mu.Lock()
		bs := slices.Clone(st.groups[reflect.TypeFor[T]()])
		st.mu.Unlock()
		for _, b := range bs {
			out = append(out, b.build((&Scope{state: st}).view(s.r)).(T))
		}
	}
	return out
}

// Resolve is the error-returning entry point.
func (s *Scope) Resolve[T any]() (v T, err error) {
	defer recoverAbort(&err)
	return s.enter().get(key{t: reflect.TypeFor[T]()}).(T), nil
}

func recoverAbort(err *error) {
	if rec := recover(); rec != nil {
		if a, ok := rec.(abort); ok {
			*err = a.err
			return
		}
		panic(rec)
	}
}

// ---- lifecycle -------------------------------------------------------------

// Start builds eager bindings, then runs OnStart hooks in build order.
func (s *Scope) Start(ctx context.Context) (err error) {
	defer recoverAbort(&err)
	s.freeze()
	s.mu.Lock()
	var eager []*binding
	for _, b := range s.index {
		if b.eager {
			eager = append(eager, b)
		}
	}
	s.mu.Unlock()
	for _, b := range eager {
		s.enter().get(b.key)
	}
	s.mu.Lock()
	started := slices.Clone(s.started)
	s.mu.Unlock()
	for _, b := range started {
		if b.onStart != nil {
			if err := b.onStart(ctx, b.value); err != nil {
				return fmt.Errorf("di: starting %s: %w", b.key, err)
			}
		}
	}
	return nil
}

// Stop runs OnStop hooks in reverse build order (dependents first) and
// reports every failure.
func (s *Scope) Stop(ctx context.Context) error {
	s.mu.Lock()
	started := s.started
	s.started = nil
	s.mu.Unlock()
	var errs []error
	for _, b := range slices.Backward(started) {
		if b.onStop != nil {
			if err := b.onStop(ctx, b.value); err != nil {
				errs = append(errs, fmt.Errorf("di: stopping %s: %w", b.key, err))
			}
		}
	}
	return errors.Join(errs...)
}
