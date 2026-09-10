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
// services are declared once in the root. [Binding.Group] and [Scope.All]
// handle groups, and [Scope.Maybe] resolves optional dependencies. An
// interface is served by a constructor that returns the implementation:
// s.Provide(func(s *Scope) Reader { return s.Get[*Repo]() }).
//
// # Scopes
//
// [Scope.Child] creates a scope that resolves through its parent, reuses
// the parent's singletons and owns the lifecycle of what it builds. A child
// may shadow a key its parent provides; within one scope, a second
// registration of a key must be marked [Binding.Override], or the next
// resolution rejects it naming both sites. That marker is the test seam: wire
// the production graph into a fresh scope, then override what you want faked
// before anything is resolved ([Test] does the bookkeeping). For HTTP,
// [github.com/floatdrop/di/dihttp.Middleware] gives each request a child
// scope holding the *http.Request, reachable through [FromContext].
//
// # Modules
//
// A [Module] is a function that registers into a scope, and [Scope.Use]
// applies modules in order. Registrations are attributed to the module that
// made them, so a collision between two modules is reported as one: "*app.DB
// is provided at storage (wire.go:12) and again at caching (cache.go:8)".
//
// # Lifecycle
//
// [Binding.OnStart], [Binding.OnDrain] and [Binding.OnStop] are typed hooks.
// [Scope.Start] builds [Binding.Eager] bindings and runs start hooks in build
// order, rolling back on failure; services built later start as part of being
// built. [Scope.Stop] first drains, which lets work already in flight finish
// while the scope still resolves, then stops child scopes, then services in
// reverse build order, and afterwards the scope refuses to resolve anything.
// [Binding.Worker] runs a long-lived function that is cancelled on stop.
// [Scope.Run] ties it together
// for a main function: start, wait for a signal or [Scope.Shutdown], stop
// with a deadline. [Scope.Observe] reports every step for logging and
// metrics.
//
// # Inspecting the graph
//
// A constructor's dependencies are recorded as it resolves them, so the
// graph is known for whatever has been built. [Scope.Explain] renders one
// service's dependency tree, with the registration site, lifetime and scope
// of each node, and what needed it. [Scope.Graph] renders everything built
// in a scope and its descendants as Graphviz DOT.
//
// # Concurrency
//
// A [Scope] is safe to use from many goroutines, including from goroutines a
// constructor starts for itself: the resolution path is immutable, so
// branches that run in parallel share nothing. A constructor may also keep
// the Scope it was handed and resolve through it later, once its own service
// is built; the finished part of that path is no longer a dependency, so such
// a resolution is not a cycle. A service is built once however many
// resolutions race for it, and a resolution of a running scope returns only a
// service whose start step has finished. A cycle is reported as [ErrCycle]
// even when the two halves are being built concurrently.
//
// Three re-entrancy limits apply. A goroutine started by a constructor must
// use [Scope.Resolve] rather than [Scope.Get], because Get reports failure by
// panicking and that panic cannot unwind to the enclosing call from another
// goroutine. An [Binding.OnStart] hook must not resolve a service that
// depends on the one being started: the hook already holds the value, and
// waiting for itself cannot make progress. And no hook may call [Scope.Stop]
// on its own scope or an ancestor, because Stop waits for the very step the
// hook is running; call [Scope.Shutdown], which never blocks.
package di

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"time"
)

// key identifies a service: its Go type. Keys compare by reflect.Type
// identity, so same-named types in different packages never collide (unlike
// fmt.Sprintf("%T")-derived names). There is no name alongside the type: a
// second binding of one type is declared as a distinct type instead, which
// makes a mistaken reference a compile error rather than a missing key.
type key struct{ t reflect.Type }

func (k key) String() string { return typeName(k.t) }

// pkgPath is the import path of the named type k stands for, walking through
// pointers as typeName does, since a pointer type is unnamed and carries no
// path of its own. It is empty for a type that has no path to report, which
// is exactly the set typeName writes with reflect's own short spelling.
func (k key) pkgPath() string {
	t := k.t
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.PkgPath()
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

var (
	ErrNotProvided = errors.New("not provided")
	ErrCycle       = errors.New("dependency cycle")
	ErrStopped     = errors.New("scope stopped")
)

// EventKind classifies an Event.
type EventKind string

const (
	EventBuild    EventKind = "build"    // a constructor ran
	EventStart    EventKind = "start"    // an OnStart hook ran
	EventDrain    EventKind = "drain"    // an OnDrain hook ran
	EventStop     EventKind = "stop"     // a Worker hook was cancelled and/or an OnStop hook ran
	EventShutdown EventKind = "shutdown" // Shutdown was called
)

// Event describes one lifecycle step. Observers receive it after the step
// completes, with its duration and error if any.
type Event struct {
	Kind    EventKind
	Service string // the service, e.g. "*github.com/acme/app.DB"; empty for shutdown
	// Package is the import path of the type Service names, e.g.
	// "github.com/acme/app", so an observer can shorten or group by it
	// without parsing Service. It is empty for a shutdown, and for a key
	// whose type is unnamed -- a []byte, a map[string]int -- since reflect
	// already writes those with a short package name.
	Package  string
	Scope    string // name of the scope that owns the instance
	Site     string // file:line of the registration; empty for shutdown
	Module   string // the Module the service was registered from; empty when none
	Duration time.Duration
	Err      error
}

// Scope is a container. A Scope value handed to a constructor is a view over
// the same state that carries the current resolution path.
type Scope struct {
	*state
	r      *resolver
	module string // the Module registering through this handle, or ""
}

// New creates a root scope: a container with no parent.
func New() *Scope { return &Scope{state: newState("root", nil)} }

func newState(name string, parent *state) *state {
	st := &state{
		name:       name,
		parent:     parent,
		index:      map[key]*binding{},
		groups:     map[key][]*binding{},
		scoped:     map[*binding]*instance{},
		shutdownCh: make(chan struct{}),
	}
	// One graph per container, shared by every scope under the root.
	if parent != nil {
		st.graph = parent.graph
	} else {
		st.graph = &graph{blockedFor: map[*resolver]*instance{}}
	}
	return st
}

// Child creates a scope that resolves through s. Stopping s stops its
// children first.
//
// A child made inside a constructor carries that constructor's resolution
// path, so a cycle through it is reported rather than deadlocking. The path
// goes inert when the constructor returns (see resolver.done), so a child kept
// for later, such as a request scope, resolves as an independent branch.
func (s *Scope) Child(name string) *Scope {
	st := newState(name, s.state)
	s.mu.Lock()
	s.children = append(s.children, st)
	s.mu.Unlock()
	return &Scope{state: st, r: s.r, module: s.module}
}

func (s *Scope) view(r *resolver) *Scope { return &Scope{state: s.state, r: r, module: s.module} }

// A Module is a unit of wiring: a function that registers into a scope.
// Modules compose by ordinary function composition, and [Scope.Use] applies
// them in order. Every registration a module makes is attributed to it, so a
// collision between two modules is reported as one.
type Module func(*Scope)

// Use applies each module to this scope. A registration made while a module
// runs -- directly, or from a child the module opens, or later from a
// constructor the module registered -- carries that module's name, which is
// the name of the function: register modules as named functions rather than
// closures, or the name is the enclosing function's.
func (s *Scope) Use(mods ...Module) {
	for _, m := range mods {
		m(&Scope{state: s.state, r: s.r, module: moduleName(m)})
	}
}

// moduleName is the function's name, package-qualified and without the import
// path: "app.Storage" for a function Storage in package app.
func moduleName(m Module) string {
	if m == nil {
		return ""
	}
	fn := runtime.FuncForPC(reflect.ValueOf(m).Pointer())
	if fn == nil {
		return ""
	}
	name := fn.Name()
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// Observe registers fn to receive lifecycle events from this scope and every
// scope under it. Use it for logging and metrics.
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

// report emits the event for one lifecycle step of b, owned by this scope,
// that began at t0 and ended with err.
func (st *state) report(kind EventKind, b *binding, t0 time.Time, err error) {
	st.emit(Event{
		Kind: kind, Service: b.key.String(), Package: b.key.pkgPath(),
		Scope: st.name, Site: b.site, Module: b.module,
		Duration: time.Since(t0), Err: err,
	})
}

// TB is the subset of testing.TB that Test needs.
type TB interface {
	Helper()
	Cleanup(func())
	Errorf(format string, args ...any)
}

// Test returns a scope for a test: the modules register the graph under test,
// and the scope is stopped when the test ends, failing it if a stop hook
// errors. Override what you need faked after wiring and before resolving,
// saying so:
//
//	s := di.Test(t, app.Production)
//	s.Value(&DB{DSN: "sqlite://memory"}).Override()
//	repo := s.Get[*Repo]()
func Test(tb TB, wire ...Module) *Scope {
	tb.Helper()
	s := New()
	s.Use(wire...)
	tb.Cleanup(func() {
		if err := s.Stop(context.Background()); err != nil {
			tb.Errorf("di: stopping test scope: %v", err)
		}
	})
	return s
}

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
