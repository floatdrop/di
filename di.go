// Package di is a dependency-injection container for Go 1.27+ built on
// generic methods: services are registered with s.Provide(...) and resolved
// with s.Get[T]().
package di

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"time"
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
	ErrUnhealthy   = errors.New("unhealthy")
)

type abort struct{ err error }

// ---- state -----------------------------------------------------------------

type binding struct {
	key       key
	site      string
	group     bool
	transient bool
	scoped    bool
	eager     bool
	build     func(*Scope) any
	onStart   func(context.Context, any) error
	onStop    func(context.Context, any) error
	run       func(context.Context, any) error
	health    func(context.Context, any) error

	single *instance // the singleton; scoped bindings keep one instance per state
}

// instance is one built value of a binding, owned by the state that stops it.
type instance struct {
	b     *binding
	once  sync.Once
	value any
	err   error

	// Run hook bookkeeping, set by start and consumed by stop.
	cancel context.CancelFunc
	done   chan struct{}
	runErr error
}

// start runs OnStart and launches the Run hook. The Run context is detached
// from ctx so the worker is cancelled by Stop, in dependency order, rather
// than the moment the application context is cancelled.
func (in *instance) start(ctx context.Context, owner *state) error {
	b := in.b
	if b.onStart != nil {
		if err := b.onStart(ctx, in.value); err != nil {
			return err
		}
	}
	if b.run != nil {
		rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		in.cancel, in.done = cancel, make(chan struct{})
		go func() {
			defer close(in.done)
			err := b.run(rctx, in.value)
			switch {
			case err == nil:
			case rctx.Err() == nil: // died on its own: stop the application
				(&Scope{state: owner}).Shutdown(fmt.Errorf("di: %s: %w", b.key, err))
			case !errors.Is(err, context.Canceled):
				in.runErr = err
			}
		}()
	}
	return nil
}

// stop cancels the Run hook, waits for it within ctx, then runs OnStop.
func (in *instance) stop(ctx context.Context) error {
	b := in.b
	var errs []error
	if in.cancel != nil {
		in.cancel()
		select {
		case <-in.done:
			if in.runErr != nil {
				errs = append(errs, fmt.Errorf("di: %s: %w", b.key, in.runErr))
			}
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("di: stopping %s: Run hook did not return: %w", b.key, ctx.Err()))
		}
	}
	if b.onStop != nil {
		if err := b.onStop(ctx, in.value); err != nil {
			errs = append(errs, fmt.Errorf("di: stopping %s: %w", b.key, err))
		}
	}
	return errors.Join(errs...)
}

type state struct {
	name   string
	parent *state

	mu       sync.Mutex
	pending  []*binding // registrations not yet indexed
	index    map[key]*binding
	groups   map[reflect.Type][]*binding
	frozen   bool
	started  []*instance            // build order; stopped in reverse
	scoped   map[*binding]*instance // per-scope instances of Scoped bindings
	children []*state

	startCtx context.Context // set by Start; read by Context()
	running  bool            // set once Start reaches the hook phase; enables late OnStart

	shutdownOnce sync.Once
	shutdownCh   chan struct{}
	shutdownErr  error
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
	return &state{
		name:       name,
		parent:     parent,
		index:      map[key]*binding{},
		groups:     map[reflect.Type][]*binding{},
		scoped:     map[*binding]*instance{},
		shutdownCh: make(chan struct{}),
	}
}

// Child creates a scope that resolves through s. Stopping s stops its
// children first.
func (s *Scope) Child(name string) *Scope {
	st := newState(name, s.state)
	s.mu.Lock()
	s.children = append(s.children, st)
	s.mu.Unlock()
	return &Scope{state: st}
}

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

// Scoped makes the binding one-per-scope: each scope that resolves it gets
// its own instance, built in that scope (so it can see that scope's
// values) and stopped with it. Declare request-scoped services once in the
// root and resolve them through the request scope.
func (b Binding[T]) Scoped() Binding[T] { return b.edit(func(b *binding) { b.scoped = true }) }
func (b Binding[T]) Eager() Binding[T]  { return b.edit(func(b *binding) { b.eager = true }) }

// Typed lifecycle hooks: no interface sniffing, no reflection.
func (b Binding[T]) OnStart(f func(context.Context, T) error) Binding[T] {
	return b.edit(func(b *binding) { b.onStart = func(ctx context.Context, v any) error { return f(ctx, v.(T)) } })
}
func (b Binding[T]) OnStop(f func(context.Context, T) error) Binding[T] {
	return b.edit(func(b *binding) { b.onStop = func(ctx context.Context, v any) error { return f(ctx, v.(T)) } })
}

// Run registers a long-running function for T, such as a worker loop. It is
// started in its own goroutine once the service starts and its context is
// cancelled when the service stops; Stop waits for it to return. Returning a
// non-nil error before that calls Shutdown with it, stopping the
// application.
func (b Binding[T]) Run(f func(context.Context, T) error) Binding[T] {
	return b.edit(func(b *binding) { b.run = func(ctx context.Context, v any) error { return f(ctx, v.(T)) } })
}

// Health registers a health check for T, run by HealthCheck once the
// service has been built.
func (b Binding[T]) Health(f func(context.Context, T) error) Binding[T] {
	return b.edit(func(b *binding) { b.health = func(ctx context.Context, v any) error { return f(ctx, v.(T)) } })
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

	if b.transient {
		return b.build((&Scope{state: owner}).view(r))
	}

	// Singletons live in the scope that registered them; scoped instances
	// live in the scope that resolves them and are built there.
	holder, in := owner, b.single
	if b.scoped {
		holder = s.state
		holder.mu.Lock()
		in = holder.scoped[b]
		if in == nil {
			in = &instance{b: b}
			holder.scoped[b] = in
		}
		holder.mu.Unlock()
	}
	buildView := (&Scope{state: holder}).view(r)

	in.once.Do(func() {
		defer func() {
			if rec := recover(); rec != nil {
				if a, ok := rec.(abort); ok {
					in.err = fmt.Errorf("di: building %s (provided at %s): %w", k, b.site, a.err)
					return
				}
				in.err = fmt.Errorf("di: building %s (provided at %s): panic: %v", k, b.site, rec)
			}
		}()
		in.value = b.build(buildView)
		if ctx, running := holder.runContext(); running {
			if err := in.start(ctx, holder); err != nil {
				in.err = fmt.Errorf("di: starting %s (provided at %s): %w", k, b.site, err)
				return
			}
		}
		holder.mu.Lock()
		holder.started = append(holder.started, in)
		holder.mu.Unlock()
	})
	if in.err != nil {
		panic(abort{in.err})
	}
	return in.value
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
	if s.r != nil {
		panic(abort{err})
	}
	panic(err)
}

// Context returns the context passed to Start (or Run) on this scope or the
// nearest started ancestor, so constructors can dial with a deadline. Before
// Start it returns context.Background().
func (s *Scope) Context() context.Context {
	if ctx, _ := s.runContext(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// runContext walks up the scope chain to the nearest state that Start was
// called on. running reports whether that Start has passed its hook phase,
// which is when bindings built later must start themselves.
func (st *state) runContext() (ctx context.Context, running bool) {
	for ; st != nil; st = st.parent {
		st.mu.Lock()
		ctx, running = st.startCtx, st.running
		st.mu.Unlock()
		if ctx != nil {
			return ctx, running
		}
	}
	return nil, false
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

// Start builds every Eager binding, then runs OnStart hooks in build order.
// If a hook fails, the hooks that already ran are rolled back with OnStop in
// reverse order and the scope is left with nothing to stop. After Start
// returns, a binding built later runs its OnStart as part of being built, so
// lazily resolved services start too. Start may be called once.
func (s *Scope) Start(ctx context.Context) (err error) {
	defer recoverAbort(&err)
	s.freeze()
	s.mu.Lock()
	if s.startCtx != nil {
		s.mu.Unlock()
		return errors.New("di: Start called twice")
	}
	s.startCtx = ctx
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

	// Everything built from here on starts itself inside get.
	s.mu.Lock()
	started := slices.Clone(s.started)
	s.running = true
	s.mu.Unlock()

	for _, in := range started {
		if err := in.start(ctx, s.state); err != nil {
			err = fmt.Errorf("di: starting %s: %w", in.b.key, err)
			s.mu.Lock()
			built := s.started
			s.started = nil
			s.running = false
			s.mu.Unlock()
			return errors.Join(err, stopAll(ctx, slices.DeleteFunc(built, func(x *instance) bool { return x == in })))
		}
	}
	return nil
}

// Stop stops child scopes first, then runs OnStop hooks in reverse build
// order (dependents first). Every failure is reported; Stop is idempotent.
// Stopping a child scope also detaches it from its parent, so per-request
// scopes are released once stopped.
func (s *Scope) Stop(ctx context.Context) error {
	s.mu.Lock()
	children := slices.Clone(s.children)
	started := s.started
	s.started = nil
	s.mu.Unlock()

	var errs []error
	for _, c := range children {
		errs = append(errs, (&Scope{state: c}).Stop(ctx))
	}
	errs = append(errs, stopAll(ctx, started))

	if p := s.parent; p != nil {
		p.mu.Lock()
		p.children = slices.DeleteFunc(p.children, func(c *state) bool { return c == s.state })
		p.mu.Unlock()
	}
	return errors.Join(errs...)
}

func stopAll(ctx context.Context, started []*instance) error {
	var errs []error
	for _, in := range slices.Backward(started) {
		errs = append(errs, in.stop(ctx))
	}
	return errors.Join(errs...)
}

// HealthCheck runs every Health hook of the services built in this scope and
// its descendants, concurrently, and returns the failures joined. Each
// failure wraps ErrUnhealthy and names the service.
func (s *Scope) HealthCheck(ctx context.Context) error {
	var ins []*instance
	var collect func(st *state)
	collect = func(st *state) {
		st.mu.Lock()
		for _, in := range st.started {
			if in.b.health != nil {
				ins = append(ins, in)
			}
		}
		children := slices.Clone(st.children)
		st.mu.Unlock()
		for _, c := range children {
			collect(c)
		}
	}
	collect(s.state)

	errs := make([]error, len(ins))
	var wg sync.WaitGroup
	for i, in := range ins {
		wg.Go(func() {
			if err := in.b.health(ctx, in.value); err != nil {
				errs[i] = fmt.Errorf("di: %s %w: %w", in.b.key, ErrUnhealthy, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// ---- request scopes --------------------------------------------------------

type ctxKey struct{}

// WithScope attaches s to ctx so handlers and their callees can reach it
// with FromContext.
func WithScope(ctx context.Context, s *Scope) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext returns the scope attached with WithScope, if any.
func FromContext(ctx context.Context) (*Scope, bool) {
	s, ok := ctx.Value(ctxKey{}).(*Scope)
	return s, ok
}

// Middleware gives every request its own child scope: the *http.Request is
// registered in it, the scope is attached to the request context, and it is
// stopped (and detached) when the handler returns.
func (s *Scope) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := s.Child("request")
		req.Value(r)
		defer req.Stop(context.WithoutCancel(r.Context()))
		next.ServeHTTP(w, r.WithContext(WithScope(r.Context(), req)))
	})
}

// Shutdown asks a running Run to stop, optionally recording why. It never
// blocks, may be called from any goroutine, and the first call wins. The
// request propagates to ancestor scopes, so a service in a child scope can
// stop the application.
func (s *Scope) Shutdown(err error) {
	for st := s.state; st != nil; st = st.parent {
		st.shutdownOnce.Do(func() {
			st.shutdownErr = err
			close(st.shutdownCh)
		})
	}
}

// RunOption configures Run.
type RunOption func(*runConfig)

type runConfig struct {
	stopTimeout time.Duration
	signals     []os.Signal
}

// StopTimeout bounds how long Stop may take once Run decides to exit.
// The default is 15 seconds.
func StopTimeout(d time.Duration) RunOption { return func(c *runConfig) { c.stopTimeout = d } }

// Signals replaces the signals that make Run exit. The default is
// os.Interrupt and SIGTERM.
func Signals(sig ...os.Signal) RunOption { return func(c *runConfig) { c.signals = sig } }

// Run starts the scope and blocks until ctx is cancelled, a termination
// signal arrives, or Shutdown is called. It then stops the scope with a
// bounded context; a second signal during the stop cancels that context so
// a hung hook cannot keep the process alive. Run returns the Start error, the
// error passed to Shutdown, and any Stop errors, joined.
func (s *Scope) Run(ctx context.Context, opts ...RunOption) error {
	cfg := runConfig{stopTimeout: 15 * time.Second, signals: []os.Signal{os.Interrupt, syscall.SIGTERM}}
	for _, o := range opts {
		o(&cfg)
	}

	// Register before Start so a signal during a slow start is not lost.
	sigCtx, cancelSig := signal.NotifyContext(ctx, cfg.signals...)
	defer cancelSig()

	if err := s.Start(ctx); err != nil {
		return err
	}

	var cause error
	select {
	case <-sigCtx.Done():
	case <-s.shutdownCh:
		cause = s.shutdownErr
	}

	stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), cfg.stopTimeout)
	defer cancelStop()
	forceCtx, cancelForce := signal.NotifyContext(stopCtx, cfg.signals...)
	defer cancelForce()

	return errors.Join(cause, s.Stop(forceCtx))
}
