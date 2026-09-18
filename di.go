// Package di is a dependency-injection container for Go 1.27+ built on
// generic methods.
//
// Services are registered on a [Scope] and resolved from it by type. A
// constructor is a plain function, and its parameters are its dependencies:
//
//	app := di.New()
//	app.Value(Config{DSN: "postgres://localhost/app"})
//	app.Wire[*DB](NewDB). // func NewDB(Config) (*DB, error)
//		OnStop(func(ctx context.Context, db *DB) error { return db.Close() })
//	app.Wire[*Repo](NewRepo) // func NewRepo(*DB) *Repo
//
//	repo, err := app.Resolve[*Repo]()
//
// Keys are Go types, so there is no naming scheme and no collisions between
// packages. [Scope.Wire] reads a constructor's signature once, at
// registration, so the graph is declared before anything is built:
// [Scope.Validate] checks every declared dependency without running a
// constructor, and a constructor that returns an error fails the enclosing
// [Scope.Resolve], [Scope.Start] or [Scope.Run] with the dependency path and
// the registration site.
//
// [Scope.Provide] takes a closure over the scope instead, for the rare
// constructor that needs the scope itself, such as middleware that opens a
// child scope per request. Inside it, [Scope.Get] and [Scope.Must] pull
// dependencies and abort on failure, with the same error. A closure's
// dependencies are known only once it runs, so Validate and [Scope.Modules]
// list it as unchecked, and [Scope.Explain] can show what it needed only
// after it has been built.
//
// # Lifetimes
//
// A binding is a singleton by default, cached in the scope that registered
// it. [Binding.Scoped] makes it one instance per resolving scope, built there
// so it can see that scope's values, which is how request-scoped services are
// declared once in the root. [Binding.Group] and [Scope.All] handle groups,
// and [Scope.Maybe] resolves an optional dependency from inside a closure.
// [Binding.Needs] says that a parameter of a Wire constructor is one of those
// two — [AllOf] for a []T holding the group for T, [Optional] for a T that may
// go unprovided — so a constructor that takes either is still a plain function
// and its dependencies are still declared. An interface is served by a
// constructor that returns the implementation, since Wire accepts any result
// assignable to the key: s.Wire[Reader](NewRepo). Two instances of one type
// are two keys: [Scope.Tag] is a view through which a type names the key
// tagged with another type, for registering and resolving alike, and
// [Tagged] says which parameter of a Wire constructor takes it.
//
// # Scopes
//
// [Scope.Child] creates a scope that resolves through its parent, reuses the
// parent's singletons and owns the lifecycle of what it builds. A child may
// shadow a key its parent provides; within one scope, a second registration
// of a key must be marked [Binding.Override], or the next resolution rejects
// it naming both sites. That marker is the test seam: wire the production
// graph into a fresh scope, then override what you want faked before anything
// is resolved ([Test] does the bookkeeping). For HTTP,
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
// [Binding.OnStart], [Binding.OnDrain] and [Binding.OnStop] are typed hooks,
// one of each per binding. [Scope.Start] builds [Binding.Eager] bindings and
// runs start hooks in build order, rolling back on failure; services built
// later start as part of being built. [Scope.Stop] first drains, which lets
// work already in flight finish while the scope still resolves, then stops
// child scopes, then services in reverse build order, and afterwards the scope
// refuses to resolve anything. [Binding.Go] runs a worker, a long-lived
// function in a goroutine of its own, cancelled on stop. [Scope.Run] ties it
// together for a main function: start within [StartTimeout], wait for a signal
// or [Scope.Shutdown], stop within [StopTimeout].
// [Scope.Observe] reports every step for logging and metrics.
//
// # Inspecting the graph
//
// A constructor's dependencies are recorded as it resolves them, so the graph
// is known for whatever has been built. [Scope.Explain] renders one service's
// dependency tree, with the registration site, lifetime and scope of each
// node, and what needed it. [Scope.Graph] renders everything built in a scope
// and its descendants as Graphviz DOT.
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

// key identifies a service: its Go type, and the tag Binding.Tag registered
// it under, if any. Keys compare by reflect.Type identity, so same-named
// types in different packages never collide. There is no string name: a
// second binding of one type is declared under a distinct type, or through
// Scope.Tag under a tag, which is a type too, so a mistaken reference is a
// compile error.
type key struct {
	t   reflect.Type
	tag reflect.Type // nil for a plain registration
}

func (k key) String() string { return k.name(typeName) }

// name renders the key with f naming each type, "*app.DB tagged app.Primary"
// for a tagged one.
func (k key) name(f func(reflect.Type) string) string {
	if k.tag == nil {
		return f(k.t)
	}
	return f(k.t) + " tagged " + f(k.tag)
}

// pkgPath is the import path of the named type k stands for, walking through
// pointers as typeName does, since a pointer type is unnamed. It is empty for
// exactly the types typeName writes with reflect's own short spelling.
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
	EventStop     EventKind = "stop"     // a worker was cancelled and/or an OnStop hook ran
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
	// whose type is unnamed, since reflect already writes those short.
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
	st     *state // what this handle is a view over
	r      *resolver
	module string       // the Module registering through this handle, or ""
	tag    reflect.Type // what a Tag view adds to the keys named through it; nil otherwise
}

// Tag is a view of the scope through which T names the key T tagged with
// Tag, a second key for the type, so several instances of one type coexist,
// each named by a type:
//
//	type Primary struct{}
//	s.Tag[Primary]().Wire[*DB](newPrimary)
//	s.Tag[Replica]().Wire[*DB](newReplica)
//	s.Wire[*Report](NewReport).Needs(di.Tagged[*DB, Replica]()) // func NewReport(db *DB) *Report
//	db := s.Tag[Primary]().Get[*DB]()
//
// Every method that names a key applies the tag: the registration methods
// and Wrap on their way in, Get, Resolve, Maybe, All and Explain on their way
// out. A Wire constructor takes a plain *DB and says which instance fills it
// with [Binding.Needs]; a constructor that takes two *DB is not told apart by
// type, so one of them takes a defined type, filled from the tag by a
// one-line constructor. The tag is a compile-time name, and an unexported
// one keeps the instance private to its package.
//
// The view is for the calls made through it: Child, Use and the Scope a
// constructor is handed carry no tag, so a kept view cannot tag what a child
// or a constructor resolves.
func (s *Scope) Tag[Tag any]() *Scope {
	return &Scope{st: s.st, r: s.r, module: s.module, tag: reflect.TypeFor[Tag]()}
}

// key is the key t names through this handle: tagged through a Tag view.
func (s *Scope) key(t reflect.Type) key { return key{t: t, tag: s.tag} }

// New creates a root scope: a container with no parent.
func New() *Scope { return &Scope{st: newState("root", nil)} }

func newState(name string, parent *state) *state {
	st := &state{
		name:       name,
		parent:     parent,
		scoped:     map[*binding]*instance{},
		shutdownCh: make(chan struct{}),
	}
	st.reg.Store(emptyRegistry)
	// One graph per container, shared by every scope under the root.
	if parent != nil {
		st.graph = parent.graph
	} else {
		st.graph = &graph{under: map[*resolver]map[*waitEdge]struct{}{}}
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
	st := newState(name, s.st)
	s.st.mu.Lock()
	s.st.children = append(s.st.children, st)
	s.st.mu.Unlock()
	return &Scope{st: st, r: s.r, module: s.module}
}

func (s *Scope) view(r *resolver) *Scope { return &Scope{st: s.st, r: r, module: s.module} }

// A Module is a unit of wiring: a function that registers into a scope.
// Modules compose by ordinary function composition, and [Scope.Use] applies
// them in order. Every registration a module makes is attributed to it, so a
// collision between two modules is reported as one.
type Module func(*Scope)

// Use applies each module to this scope. A registration made while a module
// runs, directly, from a child the module opens, or later from a constructor
// the module registered, carries that module's name, which is the name of the
// function: register modules as named functions rather than closures, or the
// name is the enclosing function's.
func (s *Scope) Use(mods ...Module) {
	for _, m := range mods {
		m(&Scope{st: s.st, r: s.r, module: moduleName(m)})
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
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	var obs []func(Event)
	if p := s.st.observers.Load(); p != nil {
		obs = slices.Clone(*p)
	}
	obs = append(obs, fn)
	s.st.observers.Store(&obs)
}

// emit delivers ev to the observers of st and its ancestors. It takes no
// lock: every build in every request scope reports through the root.
func (st *state) emit(ev Event) {
	for ; st != nil; st = st.parent {
		if p := st.observers.Load(); p != nil {
			for _, fn := range *p {
				fn(ev)
			}
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
