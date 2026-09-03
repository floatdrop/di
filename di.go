// Package di is a dependency-injection container for Go 1.27+ built on
// generic methods.
//
// Services are registered on a [Scope] and resolved from it by type:
//
//	app := di.New()
//	app.Value(Config{DSN: "postgres://localhost/app"})
//	app.Provide(func(s *di.Scope) *DB { return s.Must(sql.Open("postgres", s.Get[Config]().DSN)) }).
//		OnStop(func(ctx context.Context, db *DB) error { return db.Close() })
//	app.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} })
//
//	repo, err := app.Resolve[*Repo]()
//
// Keys are Go types, so there is no naming scheme and no collisions between
// packages. Constructors return T rather than (T, error): inside a
// constructor, [Scope.Get] and [Scope.Must] abort on failure and the error
// surfaces from the enclosing [Scope.Resolve], [Scope.Start] or [Scope.Run]
// with the dependency path and the registration site.
//
// # Lifetimes
//
// A binding is a singleton by default, cached in the scope that registered
// it. [Binding.Scoped] makes it one instance per resolving scope, built
// there so it can see that scope's values, which is how request-scoped
// services are declared once in the root. [Binding.Transient] builds an
// untracked new instance on every resolution. [Scope.Add] and [Scope.All] handle groups,
// [Scope.Bind] aliases an interface to an implementation, and
// [Scope.Maybe] resolves optional dependencies.
//
// # Scopes
//
// [Scope.Child] creates a scope that resolves through its parent, reuses
// the parent's singletons and owns the lifecycle of what it builds. The
// last registration of a key wins, which is the test seam: wire the
// production graph into a fresh scope, then re-register what you want faked
// before anything is resolved ([Test] does the bookkeeping). For HTTP,
// [Scope.Middleware] gives each request a child scope holding the
// *http.Request, reachable through [FromContext].
//
// # Lifecycle
//
// [Binding.OnStart] and [Binding.OnStop] are typed hooks. [Scope.Start]
// builds [Binding.Eager] bindings and runs start hooks in build order,
// rolling back on failure; services built later start as part of being
// built. [Scope.Stop] stops child scopes first, then services in reverse
// build order, and afterwards the scope refuses to resolve anything.
// [Binding.Run] runs a long-lived function that is cancelled on stop, and
// [Binding.Health] feeds [Scope.HealthCheck]. [Scope.Run] ties it together
// for a main function: start, wait for a signal or [Scope.Shutdown], stop
// with a deadline. [Scope.Observe] reports every step for logging and
// metrics.
package di

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
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
	ErrStopped     = errors.New("scope stopped")
)

// ---- observability ---------------------------------------------------------

// EventKind classifies an Event.
type EventKind string

const (
	EventBuild    EventKind = "build"    // a constructor ran
	EventStart    EventKind = "start"    // an OnStart hook ran
	EventStop     EventKind = "stop"     // a Run hook was cancelled and/or an OnStop hook ran
	EventHealth   EventKind = "health"   // a Health hook ran
	EventShutdown EventKind = "shutdown" // Shutdown was called
)

// Event describes one lifecycle step. Observers receive it after the step
// completes, with its duration and error if any.
type Event struct {
	Kind     EventKind
	Service  string // the service, e.g. "*github.com/acme/app.DB" or "...DB#replica"; empty for shutdown
	Scope    string // name of the scope that owns the instance
	Site     string // file:line of the registration; empty for shutdown
	Duration time.Duration
	Err      error
}

// SlogObserver returns an observer that logs every event to l: failures at
// Error level, everything else at Debug.
func SlogObserver(l *slog.Logger) func(Event) {
	return func(ev Event) {
		attrs := []any{"kind", string(ev.Kind), "scope", ev.Scope}
		if ev.Service != "" {
			attrs = append(attrs, "service", ev.Service)
		}
		if ev.Duration > 0 {
			attrs = append(attrs, "duration", ev.Duration)
		}
		if ev.Err != nil {
			l.Error("di: "+string(ev.Kind)+" failed", append(attrs, "err", ev.Err)...)
			return
		}
		l.Debug("di: "+string(ev.Kind), attrs...)
	}
}

type abort struct{ err error }

// ---- state -----------------------------------------------------------------

type binding struct {
	key       key
	site      string
	group     bool
	transient bool
	scoped    bool
	eager     bool
	isValue   bool // registered with Value: lifetimes do not apply
	alias     *key // Bind: serve this key from another binding, lifetime included
	build     func(*Scope) any
	onStart   func(context.Context, any) error
	onStop    func(context.Context, any) error
	run       func(context.Context, any) error
	health    func(context.Context, any) error

	// used is set once this binding has successfully served a value, after
	// which the registration can no longer be overridden: doing so would
	// leave two live instances of one service. A resolution that failed
	// built nothing, so it leaves the key re-registerable, which is the
	// only way to recover a key whose constructor failed.
	used atomic.Bool

	single *instance // the singleton; scoped bindings keep one instance per state
}

// validate rejects lifetime and hook combinations that cannot be honoured.
// It runs at freeze, so the order the builder methods were called in does
// not matter.
func (b *binding) validate() {
	bad := func(what, why string) {
		panic(fmt.Sprintf("di: %s (provided at %s): %s %s", b.key, b.site, what, why))
	}
	hooks := b.onStart != nil || b.onStop != nil || b.run != nil || b.health != nil
	switch {
	case b.transient && b.scoped:
		bad("Transient and Scoped", "are mutually exclusive")
	case b.transient && hooks:
		bad("lifecycle hooks", "do not apply to a Transient binding: its instances are not tracked")
	case b.eager && lifetimeName(b) != "":
		// Contradictory on its own terms, so rejected even if a later
		// registration overrides it. Whether an override can inherit
		// eagerness is a separate question, settled in deriveEager.
		bad("Eager", "does not apply to a "+lifetimeName(b)+" binding: it is not built once")
	case b.alias != nil && (b.transient || b.scoped || hooks):
		// Eager is allowed: it says the key exists by the time Start
		// returns, which an alias honours by building its target.
		bad("lifetimes and lifecycle hooks", "belong on the target binding, not on a Bind alias")
	}
}

// lifetimeName names a binding's per-scope lifetime, or "" for a singleton.
func lifetimeName(b *binding) string {
	switch {
	case b.scoped:
		return "Scoped"
	case b.transient:
		return "Transient"
	}
	return ""
}

// phase is an instance's position in the build/start/stop sequence. It is
// read and written only under the owning state's mutex, so the decision of
// who starts or stops an instance is never split across two critical
// sections.
type phase int8

const (
	phaseNew      phase = iota // no value yet
	phaseBuilt                 // constructor ran; the start step has not
	phaseStarting              // a goroutine has claimed the start step
	phaseStarted               // the start step succeeded
	phaseFailed                // the start step failed
	phaseStopped               // the stop step ran, or was skipped for good
)

// instance is one built value of a binding, owned by the state that stops it.
type instance struct {
	b     *binding
	once  sync.Once
	ph    phase // guarded by the owning state's mutex
	value any
	err   error

	stopWanted bool            // Stop arrived mid-start; the claimer tears it down
	stopCtx    context.Context // the context of the Stop that queued the handoff

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
		t0 := time.Now()
		err := b.onStart(ctx, in.value)
		owner.emit(Event{Kind: EventStart, Service: b.key.String(), Scope: owner.name, Site: b.site, Duration: time.Since(t0), Err: err})
		if err != nil {
			return err
		}
	}
	if b.run != nil {
		rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		in.cancel, in.done = cancel, make(chan struct{})
		go func() {
			defer close(in.done)
			err := b.run(rctx, in.value)
			if err == nil {
				return
			}
			if rctx.Err() != nil && errors.Is(err, context.Canceled) {
				return // we cancelled it and it reported just that
			}
			// Record it whoever cancelled, so Stop reports it in every
			// driving mode, then wake a waiting Run if it died on its own.
			in.runErr = err
			if rctx.Err() == nil {
				(&Scope{state: owner}).shutdown(fmt.Errorf("di: %s: %w", b.key, err), false)
			}
		}()
	}
	return nil
}

// everStarted reports whether Start was called on this scope or an ancestor.
func (st *state) everStarted() bool {
	for ; st != nil; st = st.parent {
		st.mu.Lock()
		started := st.startCtx != nil
		st.mu.Unlock()
		if started {
			return true
		}
	}
	return false
}

// claim takes the start step for this goroutine, returning false if another
// one already has it or the instance is past starting.
func (in *instance) claim(owner *state) bool {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if in.ph != phaseBuilt {
		return false
	}
	in.ph = phaseStarting
	return true
}

// startClaimed runs the start step of an instance already in phaseStarting.
// The phase is settled even if the hook panics, and a Stop that arrived
// while the step was in flight is honoured here, on this goroutine.
func (in *instance) startClaimed(ctx context.Context, owner *state) error {
	ok := false
	defer func() {
		owner.mu.Lock()
		if ok {
			in.ph = phaseStarted
		} else {
			in.ph = phaseFailed
		}
		wanted, stopCtx := in.stopWanted, in.stopCtx
		owner.mu.Unlock()
		if wanted {
			if stopCtx == nil {
				stopCtx = owner.stopContext()
			}
			_ = in.stopIfNeeded(stopCtx, owner)
		}
	}()
	err := in.start(ctx, owner)
	ok = err == nil
	return err
}

// stopContext returns the context Stop was called with, or a background one
// if the scope was stopped without recording it.
func (st *state) stopContext() context.Context {
	for ; st != nil; st = st.parent {
		st.mu.Lock()
		ctx := st.stopCtx
		st.mu.Unlock()
		if ctx != nil {
			return ctx
		}
	}
	return context.Background()
}

// stopIfNeeded runs the stop step once, and only when it is owed. It is owed
// when the start step succeeded, and when the instance was merely built but
// its OnStop is not paired with anything: either the binding declares no
// OnStart, or the scope was never started at all, in which case OnStop is a
// plain destructor for what the constructor acquired. An instance whose
// OnStart was skipped by a rollback, or failed, is not stopped.
func (in *instance) stopIfNeeded(ctx context.Context, owner *state) error {
	paired := in.b.onStart != nil && owner.everStarted()
	owner.mu.Lock()
	if in.ph == phaseStarting {
		// The start step is in flight. Hand the teardown to the goroutine
		// running it rather than waiting here: that goroutine may be this
		// one, if a start hook called Stop, and waiting would deadlock.
		in.stopWanted = true
		in.stopCtx = ctx // this Stop's context, not whichever is current later
		owner.mu.Unlock()
		return nil
	}
	owed := in.ph == phaseStarted || (in.ph == phaseBuilt && !paired)
	in.ph = phaseStopped
	owner.mu.Unlock()
	if !owed {
		return nil
	}
	return in.stop(ctx, owner)
}

// stop cancels the Run hook, waits for it within ctx, then runs OnStop.
func (in *instance) stop(ctx context.Context, owner *state) error {
	b := in.b
	if in.cancel == nil && b.onStop == nil {
		return nil
	}
	t0 := time.Now()
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
	err := errors.Join(errs...)
	owner.emit(Event{Kind: EventStop, Service: b.key.String(), Scope: owner.name, Site: b.site, Duration: time.Since(t0), Err: err})
	return err
}

type state struct {
	name   string
	parent *state

	mu        sync.Mutex
	pending   []*binding // registrations not yet indexed
	index     map[key]*binding
	groups    map[reflect.Type][]*binding
	frozen    bool
	all       []*binding             // every binding, in registration order
	eager     []*binding             // derived by deriveEager: what Start builds
	started   []*instance            // build order; stopped in reverse
	scoped    map[*binding]*instance // per-scope instances of Scoped bindings
	served    map[key]bool           // keys this scope resolved from an outer scope; lazily made
	children  []*state
	observers []func(Event)

	stopped  atomic.Bool     // set by Stop or a failed Start; resolution then fails with ErrStopped
	stopCtx  context.Context // the context Stop was called with
	startCtx context.Context // set by Start; read by Context()
	running  bool            // set once Start reaches the hook phase; enables late OnStart

	shutdownOnce sync.Once
	shutdownCh   chan struct{}
	shutdownErr  error
}

// freeze commits the pending registrations. It is transactional: the batch
// is validated against a prospective copy of the registry and only then
// committed, so a rejected registration leaves the scope exactly as it was.
// A scope that holds an invalid registration therefore keeps rejecting,
// consistently, instead of appearing to succeed on a second attempt with
// the offending entry silently dropped.
func (st *state) freeze() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.pending) == 0 {
		return
	}

	index := maps.Clone(st.index)
	groups := maps.Clone(st.groups)
	all := slices.Clone(st.all)
	for _, b := range st.pending {
		b.validate()
		if b.group {
			groups[b.key.t] = append(slices.Clone(groups[b.key.t]), b)
		} else {
			// Later registration wins, which is how overrides work, but only
			// until the key has been resolved: replacing it afterwards would
			// leave two live instances of one service.
			if prev, ok := index[b.key]; ok && prev.used.Load() {
				panic(fmt.Sprintf("di: %s (provided at %s) cannot be overridden at %s: it has already been resolved",
					b.key, prev.site, b.site))
			}
			if st.served[b.key] {
				// This scope already handed out a value for the key from an
				// outer scope, so shadowing it now would give one key two
				// live values within this scope.
				panic(fmt.Sprintf("di: %s cannot be registered at %s: this scope has already resolved it from an outer scope",
					b.key, b.site))
			}
			index[b.key] = b
		}
		all = append(all, b)
	}
	eager := deriveEager(all, index)

	st.index, st.groups, st.all, st.eager = index, groups, all, eager
	st.pending, st.frozen = nil, true
}

// deriveEager returns the ordered set of bindings Start builds. It is the
// only place that decides what Eager means, so the set and the rules it must
// satisfy cannot drift apart:
//
// For every key, the set holds the binding that serves the key exactly once
// if any registration for that key was marked Eager, at the position of the
// first such registration. A group member is its own entry. A binding that
// serves the key but has a per-scope lifetime cannot honour eagerness and is
// rejected here, whether that combination was declared directly or arrived
// through an override. A Bind alias may serve an eager key: Start then
// builds the target it redirects to.
func deriveEager(all []*binding, index map[key]*binding) []*binding {
	var eager []*binding
	seen := make(map[*binding]bool, len(all))
	for _, b := range all {
		if !b.eager {
			continue
		}
		w := b
		if !b.group {
			if w = index[b.key]; w == nil {
				continue
			}
		}
		if seen[w] {
			continue
		}
		if kind := lifetimeName(w); kind != "" {
			// b itself is caught by validate, so w is an override here.
			panic(fmt.Sprintf("di: %s is Eager (provided at %s), but the %s registration at %s owns the key: eagerness cannot transfer to a per-scope lifetime",
				b.key, b.site, kind, w.site))
		}
		seen[w] = true
		eager = append(eager, w)
	}
	return eager
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

// Observe registers fn to receive lifecycle events from this scope and every
// scope under it. Use it for logging and metrics; see SlogObserver.
func (s *Scope) Observe(fn func(Event)) {
	s.mu.Lock()
	s.observers = append(s.observers, fn)
	s.mu.Unlock()
}

// emit delivers ev to the observers of st and its ancestors.
func (st *state) emit(ev Event) {
	for ; st != nil; st = st.parent {
		st.mu.Lock()
		obs := slices.Clone(st.observers)
		st.mu.Unlock()
		for _, fn := range obs {
			fn(ev)
		}
	}
}

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
	b := s.register(key{t: reflect.TypeFor[T]()}, func(*Scope) any { return v })
	b.isValue = true
	return Binding[T]{s, b}
}

// Bind registers an alias: requests for I are served by T's binding, with
// T's lifetime, instance and hooks. Both parameters are explicit:
// s.Bind[Reader, *Repo](). Lifetimes and lifecycle hooks belong on the
// target binding, not on the alias.
func (s *Scope) Bind[I, T any]() Binding[I] {
	it, tt := reflect.TypeFor[I](), reflect.TypeFor[T]()
	if it.Kind() != reflect.Interface {
		panic(fmt.Sprintf("di: Bind's first type parameter must be an interface, got %s", typeName(it)))
	}
	if !tt.Implements(it) {
		panic(fmt.Sprintf("di: %s does not implement %s", typeName(tt), typeName(it)))
	}
	b := s.register(key{t: it}, nil)
	target := key{t: tt}
	b.alias = &target
	return Binding[I]{s, b}
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

// Transient builds a new instance on every resolution, in the scope that
// resolves it. Instances are not tracked, so lifecycle hooks and Eager are
// rejected on a transient binding: it must own its own cleanup.
func (b Binding[T]) Transient() Binding[T] {
	return b.edit(func(b *binding) { b.lifetimeCheck("Transient"); b.transient = true })
}

// Scoped makes the binding one-per-scope: each scope that resolves it gets
// its own instance, built in that scope (so it can see that scope's
// values) and stopped with it. Declare request-scoped services once in the
// root and resolve them through the request scope.
func (b Binding[T]) Scoped() Binding[T] {
	return b.edit(func(b *binding) { b.lifetimeCheck("Scoped"); b.scoped = true })
}

func (b *binding) lifetimeCheck(what string) {
	if b.isValue {
		panic(fmt.Sprintf("di: %s is meaningless for a Value binding (%s, provided at %s)", what, b.key, b.site))
	}
}

// Eager builds the service during Start rather than on first use.
//
// Lifetime and lifecycle hooks belong to a registration, because they are
// typed on that particular value, but eagerness belongs to the key: it means
// the service exists by the time Start returns. So overriding an eager
// binding keeps the key eager and builds the replacement, while a
// replacement with a per-scope lifetime, which cannot be built once at
// Start, is rejected.
func (b Binding[T]) Eager() Binding[T] { return b.edit(func(b *binding) { b.eager = true }) }

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

// found is what lookup located for a key: the binding that actually serves
// it, the scope that owns that binding, the key it is registered under, and
// the alias keys traversed on the way. An alias binding is never reported,
// so resolve cannot receive one.
type found struct {
	b       *binding
	owner   *state
	at      key        // the key b is registered under, after following aliases
	aliases []*binding // alias bindings traversed: their keys are the route
	cycle   bool       // the alias chain looped back on itself
}

// lookupKey finds the binding registered for k in this scope or an ancestor,
// without following aliases.
func (s *Scope) lookupKey(k key) (*binding, *state) {
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

// lookup resolves k through any Bind aliases to the binding that serves it.
// A nil b means either nothing is registered for at, or cycle is set.
func (s *Scope) lookup(k key) found {
	f := found{at: k}
	for {
		b, st := s.lookupKey(f.at)
		if b == nil {
			return f
		}
		if b.alias == nil {
			f.b, f.owner = b, st
			return f
		}
		f.aliases = append(f.aliases, b)
		next := *b.alias
		if slices.ContainsFunc(f.aliases, func(a *binding) bool { return a.key == next }) {
			f.cycle, f.at = true, next
			return f
		}
		f.at = next
	}
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
	f := s.lookup(k)

	// Alias keys stay on the stack so a cycle through an alias is detected
	// and the error path names the whole route. Skipped entirely when the
	// key is served directly, which is the common case.
	if len(f.aliases) > 0 {
		for _, ab := range f.aliases {
			if slices.Contains(r.stack, ab.key) {
				panic(abort{fmt.Errorf("di: %w: %s -> %s", ErrCycle, r.pathStr(), ab.key)})
			}
			r.stack = append(r.stack, ab.key)
		}
		defer func() { r.stack = r.stack[:len(r.stack)-len(f.aliases)] }()
	}

	switch {
	case f.cycle:
		panic(abort{fmt.Errorf("di: %w: %s -> %s", ErrCycle, r.pathStr(), f.at)})
	case f.b == nil:
		panic(abort{fmt.Errorf("di: %s: %w%s", f.at, ErrNotProvided, r.path())})
	}
	v := s.resolve(f.b, f.owner, f.at)
	// An alias key counts as resolved once a value has actually been served
	// through it. resolve panics on failure, so a failed resolution leaves
	// the alias re-registerable, which is what lets a caller redirect the
	// interface to a working implementation.
	for _, ab := range f.aliases {
		ab.used.Store(true)
	}
	if f.owner != s.state {
		// Served from an outer scope, where binding.used cannot protect this
		// scope, so record the keys here instead.
		s.mu.Lock()
		if s.served == nil {
			s.served = make(map[key]bool, 4)
		}
		s.served[k], s.served[f.at] = true, true
		for _, ab := range f.aliases {
			s.served[ab.key] = true
		}
		s.mu.Unlock()
	}
	return v
}

// resolve produces b's value for the resolving scope s, honouring the
// binding's lifetime and starting the instance when the scope is running.
func (s *Scope) resolve(b *binding, owner *state, k key) any {
	if b.alias != nil {
		// lookup never reports an alias and groups never are one, so this
		// is a programming error in the container, not in the caller's wiring.
		panic("di: internal: alias binding reached resolve")
	}
	r := s.r
	if s.isStopped() {
		panic(abort{fmt.Errorf("di: %s: %w%s", k, ErrStopped, r.path())})
	}
	if slices.Contains(r.stack, k) {
		panic(abort{fmt.Errorf("di: %w: %s -> %s", ErrCycle, r.pathStr(), k)})
	}
	r.stack = append(r.stack, k)
	defer func() { r.stack = r.stack[:len(r.stack)-1] }()

	if b.transient {
		// Built in the resolving scope, like Scoped: nothing is cached, so
		// it cannot become captive, and it can see this scope's values.
		v := b.build((&Scope{state: s.state}).view(r))
		b.used.Store(true)
		return v
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
		t0 := time.Now()
		func() {
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
		}()
		holder.emit(Event{Kind: EventBuild, Service: k.String(), Scope: holder.name, Site: b.site, Duration: time.Since(t0), Err: in.err})
		if in.err != nil {
			return
		}
		holder.mu.Lock()
		if holder.isStopped() {
			// Stop ran while we were building, so its snapshot did not
			// include us: undo with the context Stop was given, and fail.
			in.ph = phaseBuilt
			holder.mu.Unlock()
			in.err = errors.Join(fmt.Errorf("di: %s: %w", k, ErrStopped), in.stopIfNeeded(holder.stopContext(), holder))
			return
		}
		in.ph = phaseBuilt
		holder.started = append(holder.started, in)
		holder.mu.Unlock()

		// The append above strictly precedes this read, and Start sets
		// running before it drains, so either we see running and start the
		// instance ourselves, or Start's drain finds it in the list.
		if sctx, running := holder.runContext(); running && in.claim(holder) {
			if sctx == nil {
				sctx = context.Background()
			}
			if err := in.startClaimed(sctx, holder); err != nil {
				in.err = fmt.Errorf("di: starting %s (provided at %s): %w", k, b.site, err)
			}
		}
		if in.err == nil && holder.isStopped() {
			// Stop ran while we were starting; it waits for the start step,
			// so the instance is torn down. Do not hand it out.
			in.err = fmt.Errorf("di: %s: %w", k, ErrStopped)
		}
	})
	if in.err != nil {
		panic(abort{in.err})
	}
	b.used.Store(true)
	return in.value
}

func (st *state) isStopped() bool {
	for ; st != nil; st = st.parent {
		if st.stopped.Load() {
			return true
		}
	}
	return false
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
	// lookup follows aliases, so an alias whose target is missing, or whose
	// chain loops, reports absent rather than reporting present and then
	// failing inside Get.
	if f := s.lookup(key{t: reflect.TypeFor[T]()}); f.b == nil {
		var zero T
		return zero, false
	}
	return s.Get[T](), true
}

// All resolves the multi-binding group for T across the scope chain. Members
// are singletons (or Scoped/Transient if so marked) with the same lifecycle
// as any other binding.
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
	k := key{t: reflect.TypeFor[T]()}
	var out []T
	for st := s.state; st != nil; st = st.parent {
		st.freeze()
		st.mu.Lock()
		bs := slices.Clone(st.groups[k.t])
		st.mu.Unlock()
		for _, b := range bs {
			out = append(out, s.resolve(b, st, k).(T))
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

// Start builds every Eager binding in registration order, then runs the
// start step of everything built so far, in build order. If a constructor
// or a start step fails, the scope is stopped, which rolls back exactly the
// services that did start, child scopes included. A service that was built
// but never started is not stopped, so acquire resources in OnStart rather
// than in the constructor when the binding declares one. After Start returns, a service built later
// runs its start step as part of being built, so lazily resolved services
// start too. Start may be called once. It builds this scope's own Eager
// bindings; a child scope's are built by that child's Start.
func (s *Scope) Start(ctx context.Context) (err error) {
	defer recoverAbort(&err)
	s.freeze()
	s.mu.Lock()
	if s.startCtx != nil {
		s.mu.Unlock()
		return errors.New("di: Start called twice")
	}
	s.startCtx = ctx
	eager := slices.Clone(s.eager) // derived at freeze; clone so a later freeze cannot truncate it
	s.mu.Unlock()

	// F3: a failing eager constructor must roll back like a failing hook.
	if err := s.buildEager(eager); err != nil {
		return errors.Join(err, s.Stop(context.WithoutCancel(ctx)))
	}

	s.mu.Lock()
	s.running = true
	s.mu.Unlock()

	// Drain: anything built before the flag was set is still waiting here,
	// and starting one service may build more.
	for {
		in, owner := s.claimNext()
		if in == nil {
			if s.isStopped() {
				// A start hook stopped the scope; nothing is running.
				return fmt.Errorf("di: Start: %w", ErrStopped)
			}
			return nil
		}
		if err := in.startClaimed(ctx, owner); err != nil {
			err = fmt.Errorf("di: starting %s: %w", in.b.key, err)
			return errors.Join(err, s.Stop(context.WithoutCancel(ctx)))
		}
	}
}

// buildEager builds the eager bindings, turning a constructor failure into
// an error rather than letting it unwind past Start's rollback.
func (s *Scope) buildEager(eager []*binding) (err error) {
	defer recoverAbort(&err)
	for _, b := range eager {
		if b.group {
			// A group member is not reachable by key: resolve it directly.
			s.enter().resolve(b, s.state, b.key)
			continue
		}
		// By key, so a Bind alias that owns the key builds its target
		// rather than its own absent constructor. The target must be able
		// to honour eagerness, exactly as a direct winner must.
		if f := s.lookup(b.key); f.b != nil && lifetimeName(f.b) != "" {
			panic(fmt.Sprintf("di: %s is Eager (provided at %s), but it resolves through an alias to the %s binding at %s: eagerness cannot transfer to a per-scope lifetime",
				b.key, b.site, lifetimeName(f.b), f.b.site))
		}
		s.enter().get(b.key)
	}
	return nil
}

// claimNext claims the start step of the next built-but-unstarted instance
// in this scope or a descendant, in build order.
func (st *state) claimNext() (*instance, *state) {
	st.mu.Lock()
	for _, in := range st.started {
		if in.ph == phaseBuilt {
			in.ph = phaseStarting
			st.mu.Unlock()
			return in, st
		}
	}
	children := slices.Clone(st.children)
	st.mu.Unlock()
	for _, c := range children {
		if in, owner := c.claimNext(); in != nil {
			return in, owner
		}
	}
	return nil, nil
}

// Stop stops child scopes first, then runs OnStop hooks in reverse build
// order (dependents first). A service is stopped only if it actually
// started, or if it declares no OnStart, in which case OnStop is a plain
// destructor. A service whose start step is in flight is torn down by the
// goroutine running it, which may finish just after Stop returns; that
// teardown's error is reported to observers rather than returned here.
// Every failure is reported; Stop is idempotent.
// Afterwards the scope and its descendants refuse to resolve anything, with
// ErrStopped, so a closed service can never be handed out.
// Stopping a child scope also detaches it from its parent, so per-request
// scopes are released once stopped.
func (s *Scope) Stop(ctx context.Context) error {
	s.mu.Lock()
	children := slices.Clone(s.children)
	started := s.started
	s.started = nil
	if s.stopCtx == nil {
		s.stopCtx = ctx // the first Stop owns it; a later call must not clobber it
	}
	s.stopped.Store(true)
	s.mu.Unlock()

	var errs []error
	for _, c := range children {
		errs = append(errs, (&Scope{state: c}).Stop(ctx))
	}
	errs = append(errs, stopAll(ctx, s.state, started))

	if p := s.parent; p != nil {
		p.mu.Lock()
		p.children = slices.DeleteFunc(p.children, func(c *state) bool { return c == s.state })
		p.mu.Unlock()
	}
	return errors.Join(errs...)
}

func stopAll(ctx context.Context, owner *state, started []*instance) error {
	var errs []error
	for _, in := range slices.Backward(started) {
		errs = append(errs, in.stopIfNeeded(ctx, owner))
	}
	return errors.Join(errs...)
}

// HealthCheck runs every Health hook of the services built in this scope and
// its descendants, concurrently, and returns the failures joined. Each
// failure wraps ErrUnhealthy and names the service.
func (s *Scope) HealthCheck(ctx context.Context) error {
	type checked struct {
		in    *instance
		owner *state
	}
	var ins []checked
	var collect func(st *state)
	collect = func(st *state) {
		st.mu.Lock()
		for _, in := range st.started {
			if in.b.health != nil {
				ins = append(ins, checked{in, st})
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
	for i, c := range ins {
		wg.Go(func() {
			in, b := c.in, c.in.b
			t0 := time.Now()
			err := b.health(ctx, in.value)
			c.owner.emit(Event{Kind: EventHealth, Service: b.key.String(), Scope: c.owner.name, Site: b.site, Duration: time.Since(t0), Err: err})
			if err != nil {
				errs[i] = fmt.Errorf("di: %s %w: %w", b.key, ErrUnhealthy, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// ---- testing ---------------------------------------------------------------

// TB is the subset of testing.TB that Test needs.
type TB interface {
	Helper()
	Cleanup(func())
	Errorf(format string, args ...any)
}

// Test returns a scope for a test: the wire functions register the graph
// under test, and the scope is stopped when the test ends, failing it if
// a stop hook errors. Override what you need faked after wiring and before
// resolving; the last registration wins.
//
//	s := di.Test(t, app.Wire)
//	s.Value(&DB{DSN: "sqlite://memory"})
//	repo := s.Get[*Repo]()
func Test(tb TB, wire ...func(*Scope)) *Scope {
	tb.Helper()
	s := New()
	for _, w := range wire {
		w(s)
	}
	tb.Cleanup(func() {
		if err := s.Stop(context.Background()); err != nil {
			tb.Errorf("di: stopping test scope: %v", err)
		}
	})
	return s
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
		// Stop failures are reported to observers as EventStop with Err set.
		defer func() { _ = req.Stop(context.WithoutCancel(r.Context())) }()
		next.ServeHTTP(w, r.WithContext(WithScope(r.Context(), req)))
	})
}

// Shutdown asks a running Run to stop, optionally recording why. It never
// blocks, may be called from any goroutine, and the first call wins. The
// request propagates to ancestor scopes, so a service in a child scope can
// stop the application.
func (s *Scope) Shutdown(err error) { s.shutdown(err, true) }

// shutdown wakes any waiting Run. record says whether the cause should be
// returned by Run; a dying Run hook passes false because Stop reports its
// error instead, which keeps it reported in every driving mode.
func (s *Scope) shutdown(cause error, record bool) {
	first := false
	for st := s.state; st != nil; st = st.parent {
		st.shutdownOnce.Do(func() {
			if record {
				st.shutdownErr = cause
			}
			close(st.shutdownCh)
			first = first || st == s.state
		})
	}
	if first {
		s.emit(Event{Kind: EventShutdown, Scope: s.name, Err: cause})
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
