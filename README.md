# di

[![CI](https://github.com/floatdrop/di/actions/workflows/ci.yml/badge.svg)](https://github.com/floatdrop/di/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/floatdrop/di.svg)](https://pkg.go.dev/github.com/floatdrop/di)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A dependency-injection container for Go 1.27+, built on
[generic methods](https://go.dev/blog/generic-methods). Register services with
`s.Provide(...)` or hand over a plain constructor with `s.Wire[T](NewT)`, and
resolve them with `s.Get[T]()`. No code generation, no dependencies, and
reflection only where you ask for it: `Wire` reads a constructor's signature
so that `Validate` can check the graph before anything is built.

[**The guide**](https://floatdrop.github.io/di/) walks through one application
top to bottom, a file at a time: how it is structured and what it looks like.

```go
app := di.New()
app.Provide(func(s *di.Scope) *DB { return s.Must(sql.Open("postgres", dsn)) }).
    OnStop(func(ctx context.Context, db *DB) error { return db.Close() })
app.Wire[*Repo](NewRepo) // func NewRepo(db *DB) *Repo

repo, err := app.Resolve[*Repo]()
```

- **Keys are Go types.** No naming scheme, no string collisions between
  packages.
- **Constructors return `T`, not `(T, error)`.** A missing dependency or a
  failed constructor unwinds to the enclosing `Resolve` or `Start` as an
  `error` that names the full dependency path and the registration site.
- **Typed lifecycle.** `OnStart`, `OnDrain`, `OnStop` and `Worker` hooks are
  typed on the service. Nothing is discovered by sniffing interfaces.
- **Deterministic shutdown.** Reverse build order, child scopes first, every
  error reported.
- **Scopes for requests and tests.** A child scope sees its parent and can
  shadow it.
- **Explicit overrides.** Replacing a registration is `Override()`. A second
  registration without it is rejected, naming both, so one module cannot
  rewire another unnoticed. Composing over one is `Wrap`, which keeps what it
  wraps, hooks and lifetime included.
- **The graph is inspectable.** Dependencies are recorded as constructors
  resolve them, so `Explain[T]` prints what a service was built from and what
  needed it, and `Graph` exports the whole thing as Graphviz DOT.
- **The graph can be checked before it is built.** A constructor handed to
  `Wire` declares its dependencies by its parameters, and `Validate` walks
  them: a dependency nothing provides, a cycle, or a singleton that would
  build a request-scoped service in the wrong scope is reported without a
  constructor running.

## Installation

```sh
go get github.com/floatdrop/di
```

Requires Go 1.27 or newer. Editor support for generic methods needs gopls
v0.23 or newer.

## Quick start

Every embedded example in this document is a compiled program under
[`examples/`](examples/), built in CI.

[embedmd]:# (examples/quickstart/main.go go)
```go
// Quick start: register a few services, start and stop the application.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/floatdrop/di"
)

type Config struct{ DSN string }
type DB struct{ dsn string }
type Repo struct{ db *DB }
type Server struct{ repo *Repo }

// Plain constructors: their parameters are their dependencies.
func NewDB(cfg Config) *DB         { return &DB{dsn: cfg.DSN} }
func NewRepo(db *DB) *Repo         { return &Repo{db: db} }
func NewServer(repo *Repo) *Server { return &Server{repo: repo} }

func main() {
	app := di.New()

	app.Value(Config{DSN: "postgres://localhost/app"})

	app.Wire[*DB](NewDB).
		OnStop(func(ctx context.Context, db *DB) error { fmt.Println("db closed"); return nil })

	app.Wire[*Repo](NewRepo)

	app.Wire[*Server](NewServer).
		Eager().
		OnStart(func(ctx context.Context, srv *Server) error { fmt.Println("listening"); return nil }).
		OnStop(func(ctx context.Context, srv *Server) error { fmt.Println("server stopped"); return nil })

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := app.Stop(ctx); err != nil {
			log.Println(err)
		}
	}()

	fmt.Println("serving", app.Get[*Server]().repo.db.dsn)
}
```

## Guide

### Registration

Each registration returns a typed `Binding[T]`. Its methods refine the
registration and must be called before the scope is first resolved.

| Call | Registers |
|---|---|
| `s.Provide(func(*di.Scope) T)` | A lazily built singleton. `T` is inferred. |
| `s.Wire[T](NewT)` | A lazily built singleton from a plain constructor. Its parameters are its dependencies; see [Wiring plain constructors](#wiring-plain-constructors). |
| `s.Wrap[T](fn)` | A wrapper over what serves `T`: `fn` takes that value first, then its dependencies; see [Wrapping a service](#wrapping-a-service). |
| `s.Value(v)` | An instance you already have. |
| `s.Use(mods...)` | What the modules register, attributed to them by name. |

| Method | Effect |
|---|---|
| `.Scoped()` | One instance per resolving scope, built and stopped there. |
| `.Group()` | A member of the group for `T`, read back with `s.All[T]()`. |
| `.Eager()` | Build during `Start`, in registration order. |
| `.Override()` | Replace an earlier registration of `T` in this scope; a second one without it is rejected. |
| `.OnStart(f)`, `.OnStop(f)` | Lifecycle hooks, `f` is `func(context.Context, T) error`. |
| `.OnDrain(f)` | Runs before anything is stopped, while the scope still resolves. |
| `.Worker(f)` | A long-running function, cancelled on stop. |

An interface is served by a constructor that returns the implementation:

```go
app.Provide(func(s *di.Scope) Reader { return s.Get[*Repo]() })
app.Wire[Reader](NewRepo) // the same, when NewRepo returns *Repo
```

The compiler checks that `*Repo` satisfies `Reader`, and the two keys share
one instance because the constructor returns the same pointer. Declare it
`Scoped()` as well when the target is. With `Wire` the constructor's result
need only be assignable to the key, so `app.Wire[Reader](NewRepo)` serves the
interface directly; a constructor that does not is rejected at registration.

Rules the container enforces:

- A child scope may shadow a key its parent provides. Within one scope, a
  second registration of a key must be marked `Override()`; it then serves the
  key and inherits its eagerness. An unmarked duplicate, or an `Override()`
  with nothing to override, is rejected at the next resolution, naming the
  registrations involved. The marker is how a test substitutes a fake.
- Once a key has served a value it can no longer be replaced, in the scope
  that owns it or in any scope that resolved through it. Replacing it would
  leave one key with two live values, so it panics instead. A resolution that
  failed built nothing and leaves the key re-registerable.
- Combinations that cannot be honoured are rejected when the scope is first
  resolved, whatever order the methods were called in: `Eager` on a scoped
  binding, and `Scoped` on a `Value`.

### Wiring plain constructors

`Provide` takes a closure, which pulls its dependencies with `s.Get` and can
do anything else it likes; the container learns what it needed by watching it
run. `Wire` takes a constructor as it is written, `func(A, B) T` or
`func(A, B) (T, error)`, and reads its parameters as the dependencies. The
build resolves each one exactly as the closure would have, from the same
scope, so lifetimes, hooks, cycles and error paths are unchanged. What changes
is that the dependencies are known at registration, and `Validate` can walk
them with nothing built:

[embedmd]:# (examples/wire/main.go go)
```go
// Wire: plain constructors whose parameters are their dependencies, and a
// graph that is checked before anything is built.
package main

import (
	"fmt"
	"net/http"

	"github.com/floatdrop/di"
)

type Config struct{ DSN string }
type DB struct{ dsn string }
type Repo struct{ db *DB }
type User struct{ name string }
type Handler struct {
	repo *Repo
	user *User
}
type Mailer struct{ user *User }

// The constructors know nothing about di.
func NewDB(cfg Config) *DB                       { return &DB{dsn: cfg.DSN} }
func NewRepo(db *DB) *Repo                       { return &Repo{db: db} }
func NewUser(r *http.Request) *User              { return &User{name: r.Header.Get("X-User")} }
func NewHandler(repo *Repo, user *User) *Handler { return &Handler{repo: repo, user: user} }
func NewMailer(user *User) *Mailer               { return &Mailer{user: user} }

func main() {
	app := di.New()
	app.Value(Config{DSN: "postgres://localhost/app"})
	app.Wire[*DB](NewDB)
	app.Wire[*Repo](NewRepo)
	app.Wire[*User](NewUser).Scoped() // one per request scope, where the *http.Request is
	app.Wire[*Handler](NewHandler).Scoped()

	// Nothing has been built, but the constructors declared their edges, so
	// Explain draws them, dashed, down to what only a request scope provides.
	fmt.Print(app.Explain[*Handler]())
	fmt.Println()

	// Validate walks the same edges. From the application scope, *User needs
	// an *http.Request that only a request scope provides: owed, not wrong.
	v := app.Validate()
	fmt.Println("errors:", v.Err())
	fmt.Println("owed:  ", v.Owed)

	// Told what a request scope holds, the check is the one that scope
	// would make, and nothing is owed.
	fmt.Println("request scopes:", app.Validate(di.Provided[*http.Request]()).Err())

	// A singleton depending on a request-scoped service would be built in
	// app, where there is no request. A closure would fail on first use;
	// the declared graph fails here.
	app.Wire[*Mailer](NewMailer)
	fmt.Println(app.Validate().Err())
}
```

```
*main.Handler: scoped in root, not built (provided at main.go:35)
├╌╌ *main.Repo: singleton in root, not built (provided at main.go:33)
│   └╌╌ *main.DB: singleton in root, not built (provided at main.go:32)
│       └╌╌ main.Config: value in root, not built (provided at main.go:31)
└╌╌ *main.User: scoped in root, not built (provided at main.go:34)
    └╌╌ *net/http.Request: not provided

errors: <nil>
owed:   [*net/http.Request: needed by *main.User (scoped, provided at main.go:34)]
request scopes: <nil>
di: *net/http.Request: not provided in scope root (needed by [*main.Mailer *main.User]; *main.User is Scoped, so the singleton *main.Mailer would build it there)
```

| Call | Returns |
|---|---|
| `s.Validate()` | A `Validation`. `Err()` joins `Errors`, the failures the declared graph proves. `Owed` lists what a `Scoped` binding needs that this scope does not provide, left to the scope that resolves it. `Unchecked` lists the `Provide` closures. |
| `s.Validate(di.Provided[T]()...)` | The same check as the resolving scope would make it, told that it holds a `T`. With stubs nothing is owed: what neither the scope nor the stubs provide is an error. |

A singleton is checked against the scope that registered it, since that is
where it is built. A `Scoped` binding is built in whichever scope resolves it,
so it is checked as if resolved from the scope calling `Validate`, and what
that scope does not provide is owed rather than wrong: a descendant may
provide it, as request scopes provide the request. Call `Validate` from that
descendant, or say what it will hold with `di.Provided[T]()` stubs, to have
those checked.

`Wire` reads the signature with reflection once, at registration, and a
constructor of the wrong shape is rejected there with the other configuration
errors. The build calls it through `reflect.Call`, which costs about 150ns and
two allocations per build over a closure; a warm `Get` is the same code for
both. A slice parameter is a key like any other, not the group for its
element type, and a constructor that needs the scope itself, for `s.Context()`
or a conditional dependency, stays a `Provide` closure.

### Wrapping a service

`Wrap[T]` composes over whatever serves `T` when it is called: the latest
registration in this scope, or the one an ancestor provides. Its function
takes the value being wrapped first and its other dependencies after it, and
returns `T` or `(T, error)`, read as `Wire` reads a constructor. The wrapped
registration keeps its hooks and lifetime; it is built first, as the
wrapper's dependency, and so stopped after it. Wrappers chain in registration
order. This is what uber/fx calls `Decorate`.

[embedmd]:# (examples/wrap/main.go go)
```go
// Wrap: compose over a service without replacing it. The wrapped
// registration keeps its hooks and lifetime, and a wrapper in a child scope
// applies to that scope alone.
package main

import (
	"context"
	"fmt"

	"github.com/floatdrop/di"
)

type Store interface{ Get(key string) string }

type PGStore struct{}
type Cache struct{ hits int }
type CachingStore struct {
	next  Store
	cache *Cache
}
type TracingStore struct{ next Store }

func (*PGStore) Get(key string) string        { return "row " + key }
func (c *CachingStore) Get(key string) string { c.cache.hits++; return c.next.Get(key) }
func (t *TracingStore) Get(key string) string { return "traced(" + t.next.Get(key) + ")" }

func NewPGStore() *PGStore                       { return &PGStore{} }
func NewCachingStore(next Store, c *Cache) Store { return &CachingStore{next: next, cache: c} }
func NewTracingStore(next Store) Store           { return &TracingStore{next: next} }

func main() {
	app := di.New()
	app.Value(&Cache{})
	app.Wire[Store](NewPGStore).
		OnStop(func(context.Context, Store) error { fmt.Println("pg closed"); return nil })

	// The first parameter is the value being wrapped; the rest are
	// dependencies. The store keeps its OnStop, and is stopped after the
	// wrapper, since it was built first.
	app.Wrap[Store](NewCachingStore)

	// A wrapper in a child scope wraps the parent's value for that scope and
	// its descendants; the parent and its other children are untouched.
	debug := app.Child("debug")
	debug.Wrap[Store](NewTracingStore)

	fmt.Println("app:  ", app.Get[Store]().Get("1"))
	fmt.Println("debug:", debug.Get[Store]().Get("1"))
	fmt.Print(app.Explain[Store]())
	_ = app.Stop(context.Background())
}
```

```
app:   row 1
debug: traced(row 1)
main.Store: singleton wrapper in root, built (provided at main.go:40)
├── main.Store: singleton in root, built (provided at main.go:34)
└── *main.Cache: value in root, built (provided at main.go:33)
needed by: main.Store in debug
pg closed
```

Rules the container enforces:

- A wrapper takes the lifetime of what it wraps: over a `Scoped` service it is
  one per resolving scope. `Scoped()` on the wrapper itself puts one wrapper
  per resolving scope around a shared singleton.
- A wrapper in a child scope wraps the parent's value for that child and its
  descendants. The parent and its other children keep the original.
- Nothing to wrap is rejected when `Wrap` is called, and a group cannot be
  wrapped: its members are read with `All`. A key this scope has already
  resolved cannot be wrapped afterwards, as it cannot be overridden, since
  callers hold the unwrapped value.
- `Override()` after a wrapper replaces it and everything it wrapped. A
  registration some wrapper composes over cannot be overridden while that
  wrapper stands, in its scope or a descendant's.

### Resolution

| Call | Returns |
|---|---|
| `s.Get[T]()` | `T`. Inside a constructor a failure unwinds to the caller; at top level it panics with the error. |
| `s.Resolve[T]()` | `(T, error)`. Never panics on a wiring problem. |
| `s.Maybe[T]()` | `(T, bool)`, for optional dependencies. |
| `s.All[T]()` | Every member of the group for `T`, across the scope chain. |
| `s.Must(v, err)` | `v`, or aborts the constructor with `err`. |
| `s.Context()` | The context passed to `Start`, so constructors can dial with a deadline. |

Errors wrap `di.ErrNotProvided`, `di.ErrCycle` or `di.ErrStopped`:

```
di: building *app.Repo (provided at app/wire.go:31): *app.DB: not provided (needed by [*app.Repo])
di: building *app.A (provided at ...): di: building *app.B (provided at ...): di: dependency cycle: [*app.A *app.B] -> *app.A
```

### Scopes

A child scope resolves through its parent, reuses the parent's singletons, and
owns the lifecycle of what it builds itself.

[embedmd]:# (examples/scopes/main.go go)
```go
// Scopes: a child scope sees everything in its parent and can shadow it.
package main

import (
	"fmt"

	"github.com/floatdrop/di"
)

type DB struct{ dsn string }
type User struct{ Name string }
type Handler struct {
	db   *DB
	user *User
}

func NewHandler(db *DB, user *User) *Handler { return &Handler{db: db, user: user} }

func main() {
	app := di.New()
	app.Wire[*DB](func() *DB { return &DB{dsn: "postgres://localhost/app"} })

	// One child per request: request-scoped values live here, shared
	// singletons such as *DB are reused from app.
	req := app.Child("request")
	req.Value(&User{Name: "ada"})
	req.Wire[*Handler](NewHandler)

	h := req.Get[*Handler]()
	fmt.Println(h.user.Name, "->", h.db.dsn)
	fmt.Println("same db:", h.db == app.Get[*DB]())
}
```

A singleton is built in the scope that registered it, so a child cannot rewire
a parent's singleton. A service that must see child-scoped values is declared
`Scoped()`: one instance per resolving scope, built there.

### Groups

`Group()` makes a registration a member of the group for its type rather than
the binding for it, and `All` resolves every member across the scope chain. A
health endpoint is the usual case: a group of checkers, and a handler that
decides what healthy means.

```go
type Checker interface{ Check(ctx context.Context) error }

app.Wire[Checker](func(db *DB) Checker { return db }).Group()

mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
    for _, c := range app.All[Checker]() {
        if err := c.Check(r.Context()); err != nil {
            http.Error(w, err.Error(), http.StatusServiceUnavailable)
            return
        }
    }
    fmt.Fprintln(w, "ok")
})
```

`All` builds any member not built yet, so a checker's target is up by the
time it is asked. Members keep their own lifetime and hooks, and a plain
registration of the same type is neither shadowed by the group nor part of
it.

### More than one instance of a type

A key is a Go type, so two live instances of one type need two types. While
the set is fixed — a primary and a replica, two HTTP clients with different
timeouts — a defined type names each one, and embedding keeps the methods:
`type Primary struct{ *DB }`. The surrogate appears in constructor signatures
and nowhere else. `Wire` resolves it like any other parameter, so nothing has
to be annotated to say which instance feeds which argument, and `Validate`
and `Explain` see two distinct services rather than one type registered
twice.

When the set comes from configuration there are no types to write. Register
the instances as a group, fold them into a registry, and let the scope that
resolves the key say which one it wants: the selector is an ordinary
constructor whose parameter is the key, and `Scoped()` leaves the choice to
the resolving scope.

[embedmd]:# (examples/instances/main.go go)
```go
// More than one instance of a type: a defined type names each one while the
// set is fixed, and a Scoped selector picks one when the set comes from
// configuration.
package main

import (
	"context"
	"fmt"

	"github.com/floatdrop/di"
)

type DB struct{ dsn string }

func (d *DB) Query() string { return "query " + d.dsn }

// A fixed set: one defined type per instance. Embedding promotes the methods,
// so only the constructor below mentions the surrogate.
type Primary struct{ *DB }
type Replica struct{ *DB }

type Repo struct{ read, write *DB }

func NewRepo(p Primary, r Replica) *Repo { return &Repo{write: p.DB, read: r.DB} }

// A configured set: the shards are not known until the config is read, so no
// type can name them. They are registered as a group, folded into a registry,
// and picked by a value the resolving scope provides.
type Config struct{ Shards []string }

type Shard struct {
	Name string
	DB   *DB
}

type Shards map[string]*DB

type ShardName string

func selectShard(want ShardName, all Shards) (*DB, error) {
	db, ok := all[string(want)]
	if !ok {
		return nil, fmt.Errorf("no shard %q", want)
	}
	return db, nil
}

func main() {
	cfg := Config{Shards: []string{"eu-1", "us-1"}}

	app := di.New()
	app.Value(cfg)

	// Two databases, told apart by type. Each keeps its own hooks.
	app.Value(Primary{&DB{dsn: "primary"}}).
		OnStop(func(context.Context, Primary) error { fmt.Println("primary closed"); return nil })
	app.Value(Replica{&DB{dsn: "replica"}}).
		OnStop(func(context.Context, Replica) error { fmt.Println("replica closed"); return nil })
	app.Wire[*Repo](NewRepo)

	repo := app.Get[*Repo]()
	fmt.Println("writes:", repo.write.Query())
	fmt.Println("reads: ", repo.read.Query())

	// One binding per configured shard, read back together as a registry.
	for _, name := range cfg.Shards {
		app.Value(Shard{Name: name, DB: &DB{dsn: name}}).Group()
	}
	app.Provide(func(s *di.Scope) Shards {
		m := Shards{}
		for _, sh := range s.All[Shard]() {
			m[sh.Name] = sh.DB
		}
		return m
	})

	// The selector is an ordinary constructor: its ShardName parameter is the
	// key, and Scoped() leaves the choice to the scope that resolves it.
	app.Wire[*DB](selectShard).Scoped()

	for _, tenant := range cfg.Shards {
		req := app.Child(tenant)
		req.Value(ShardName(tenant))
		fmt.Println(tenant, "->", req.Get[*DB]().Query())
		_ = req.Stop(context.Background())
	}

	// The root does not provide ShardName, so Validate reports it as owed by
	// whichever scope resolves the shard rather than as a failure.
	fmt.Println("owed:", app.Validate().Owed)

	_ = app.Stop(context.Background())
}
```

Rules and traps:

- Use `Wire` for the selector rather than `Provide`. Its parameter declares
  the key, so `Validate` reports the key in `Owed` — the obligation the
  resolving scope carries — and `Validate(di.Provided[T]())` discharges it
  as a request scope would. A `Provide` closure behaves identically at run time and
  declares nothing, so the missing key is found by the first request that
  needs it.
- One key resolves to one value per scope. A caller that needs two shards at
  once reads the registry, or opens a scope for each.
- A type alias is not a new key. `type CacheDB = *DB` is the same
  `reflect.Type`, and the second registration of it is rejected as a
  duplicate. A surrogate has to be a defined type.
- A surrogate that embeds an interface satisfies that interface, so
  `type Cold struct{ Store }` compiles wherever a `Store` is wanted. It is
  still a separate key: registering `Cold` does not serve `Store`.

### Modules

A module is a function that registers into a scope. `Use` applies modules in
order and attributes each registration to the module that made it:

```go
func Storage(s *di.Scope) {
    s.Provide(func(*di.Scope) *DB { return open(storageDSN) })
    s.Wire[*Repo](NewRepo)
}

func Caching(s *di.Scope) {
    s.Provide(func(*di.Scope) *DB { return open(cacheDSN) }) // also a *DB
    s.Wire[*Cache](NewCache)
}

app := di.New()
app.Use(Storage, Caching)
app.Get[*Repo]()
// di: *app.DB is provided at app.Storage (storage.go:12) and again at
// app.Caching (caching.go:8): a second registration of a key must be marked
// Override() to replace the first
```

Without the rule the second `*DB` would have won silently and rewired
`Storage`'s `*Repo` to `Caching`'s database. Two modules that each need a `*DB`
of their own declare distinct types (`type CacheDB struct{ *DB }`); a module
that means to replace another's registration says `Override()`.

### Lifecycle

`Start` builds every `Eager` binding, then runs `OnStart` hooks in build order.
If a constructor or hook fails, `Start` stops the scope and returns both
errors; that rolls back exactly the services that started, child scopes
included. A service built after `Start` runs its `OnStart` as part of being
built, so nothing is handed out unstarted.

`Stop` runs in three phases. It drains, then stops child scopes, then runs
`OnStop` hooks in reverse build order, and returns every failure joined. A
service is stopped only when its stop is owed: its `OnStart` succeeded, or it
has no `OnStart` to pair with, in which case `OnStop` is a plain destructor.
Afterwards the scope and everything under it refuses to resolve, with
`di.ErrStopped`.

`Stop` is idempotent and safe to call concurrently: only the first call tears
the scope down, and the others wait for it and report its result. A hook must
therefore not call `Stop` on its own scope or an ancestor, which would be a
wait on itself; call `Shutdown`, which never blocks.

`OnStart` should return once the service is ready rather than run it: a server
binds its listener in the hook, so a busy port fails `Start`, and serves in a
goroutine.

### Draining

`OnDrain` runs before anything is stopped, from the innermost scope outwards
and in reverse build order, while every scope still resolves normally. It is
where a service stops taking new work and waits for the work it already has.

```go
app.Wire[*http.Server](newServer).Eager().
    OnDrain(func(ctx context.Context, srv *http.Server) error { return srv.Shutdown(ctx) }).
    OnStop(func(ctx context.Context, srv *http.Server) error { return srv.Close() })
```

An HTTP server is the case that needs it. Its handlers hold request scopes
under the application scope, so shutting the server down from `OnStop` would
race the teardown of the scopes those handlers still use, and a request in
flight would fail with `di.ErrStopped` before the server finished waiting for
it. Draining first keeps handlers' scopes and dependencies alive until they
return.

### Workers

`Worker` is for anything that loops until told to stop: consumers, pollers,
schedulers.

```go
app.Wire[*Mailer](newMailer).Eager().Worker(func(ctx context.Context, m *Mailer) error {
    return m.Loop(ctx) // returns when ctx is cancelled
})
```

The function runs in its own goroutine from the moment the service starts.
Its context is cancelled when the service stops, and `Stop` waits for it
within the stop deadline. Returning an error calls `Shutdown`, so a worker
that dies takes the application down rather than leaving it half alive, even
if it reports the failure only once shutdown is under way. The one error that
does not count is `context.Canceled` after cancellation, which means only that
the worker stopped.

### Request scopes

A `dihttp.Middleware` gives each request a child scope holding the
`*http.Request`, attaches it to the request context, and stops it when the
handler returns. It has the usual `func(http.Handler) http.Handler` shape,
so a router's `Use` accepts it too. `dihttp.Module` registers one, so a
server's constructor takes it as a dependency like any other:

```go
import "github.com/floatdrop/di/dihttp"

app.Use(dihttp.Module, api.Module)

func NewServer(cfg Config, mw dihttp.Middleware) *http.Server {
    mux := http.NewServeMux()
    mux.Handle("GET /users/{id}", dihttp.Handle((*Users).Show))
    mux.Handle("GET /healthz", dihttp.Handle((*Health).Check))
    return &http.Server{Addr: cfg.Addr, Handler: mw(mux)}
}
```

`dihttp.Handle` resolves a handler type from the request's scope and calls
the method; a method expression names both, so one type per resource with a
method per route keeps its dependencies in one place. The type is declared
`Scoped()` when it needs the request and once for the application when it
does not, and `Handle` follows either. A handler written by hand reaches
the scope with `di.FromContext(r.Context())`. Outside the container,
`dihttp.NewMiddleware(app)` makes a middleware directly.

Services that depend on the request are declared once, in the root, as
`Scoped()`; they are built per request, cached for its duration, and stopped
with it:

```go
app.Wire[*User](func(r *http.Request) *User { return &User{Name: r.Header.Get("X-User")} }).Scoped()
```

`di.WithScope` and `di.FromContext` are the primitives if you are not using
`net/http`. A complete service with a worker, a health endpoint, request scopes
and graceful shutdown is in [`examples/app`](examples/app/main.go).

### Graceful shutdown

`Run` is the main-function helper. It starts the scope, blocks until the
context is cancelled, `SIGINT` or `SIGTERM` arrives, or `Shutdown` is called,
then stops everything with a bounded context. A second signal during the stop
cancels that context, so a hung hook cannot keep the process alive.

[embedmd]:# (examples/server/main.go go)
```go
// Graceful shutdown of an HTTP server.
//
// Run starts the scope, waits for SIGINT/SIGTERM or a Shutdown call, then
// stops everything in reverse order with a bounded context. The server's
// OnDrain calls http.Server.Shutdown, which stops accepting connections and
// waits for in-flight requests until the stop context expires. Draining runs
// before anything is torn down, so those requests still have their scopes.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/floatdrop/di"
)

type DB struct{ dsn string }

func main() {
	app := di.New()

	app.Wire[*DB](func() *DB { return &DB{dsn: "postgres://localhost/app"} }).
		OnStop(func(ctx context.Context, db *DB) error { log.Println("db closed"); return nil })

	app.Wire[http.Handler](func(db *DB) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * time.Second) // simulate slow work that must not be cut short
			fmt.Fprintln(w, "served by", db.dsn)
		})
	})

	app.Wire[*http.Server](func(h http.Handler) *http.Server { return &http.Server{Addr: ":8080", Handler: h} }).
		Eager().
		OnStart(func(ctx context.Context, srv *http.Server) error {
			// Bind synchronously so a busy port fails Start; serve in the background.
			ln, err := net.Listen("tcp", srv.Addr)
			if err != nil {
				return err
			}
			log.Println("listening on", ln.Addr())
			go func() {
				if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
					app.Shutdown(err) // the listener died: stop the whole application
				}
			}()
			return nil
		}).
		// OnDrain runs before anything is stopped, so handlers that are
		// still running keep their scopes and dependencies.
		OnDrain(func(ctx context.Context, srv *http.Server) error {
			log.Println("draining")
			return srv.Shutdown(ctx) // waits for in-flight requests, bounded by StopTimeout
		}).
		OnStop(func(ctx context.Context, srv *http.Server) error { return srv.Close() })

	// Blocks until Ctrl-C, SIGTERM, or app.Shutdown. A second signal cancels
	// the stop context so a hung hook cannot keep the process alive.
	if err := app.Run(context.Background(), di.StopTimeout(10*time.Second)); err != nil {
		log.Fatal(err)
	}
}
```

### Observability

```go
app.Observe(func(ev di.Event) {
    if ev.Kind == di.EventBuild {
        buildDuration.WithLabelValues(ev.Service).Observe(ev.Duration.Seconds())
    }
})
```

Observers receive an `Event` for every constructor and every `OnStart`,
`OnDrain` and `OnStop` hook in the scope and its descendants, plus one per
`Shutdown`. Each event names the service, its scope, its module if it has one,
the registration site, the duration and the error, if any.

### Inspecting the graph

A `Provide` closure's dependencies are learned by watching it resolve them,
and a `Wire` constructor's are declared. What has been built has a recorded
graph, what was wired has a declared one, and two methods render them.

`Explain[T]` is the dependency tree of one service, with each node's lifetime,
its scope, how far through its lifecycle it is and where it was registered,
followed by what needed it:

```
*main.Server: singleton in root, eager, started (provided at main.go:36)
├── *main.Repo: singleton in root, started (provided at main.go:34)
│   └── *main.DB: singleton in root, started (provided at main.go:33)
│       └── main.Config: value in root, started (provided at main.go:32)
└── *main.Cache: singleton in root, started (provided at main.go:35)
    └── *main.DB: see above

*main.DB: singleton in root, started (provided at main.go:33)
└── main.Config: value in root, started (provided at main.go:32)
needed by: *main.Repo in root, *main.Cache in root
```

A dependency reached twice is expanded once, so a diamond is drawn as one.
Registration sites are absolute paths; they are shortened above.

`Graph` renders everything built in a scope and its descendants as Graphviz
DOT, one cluster per scope:

```sh
go run ./examples/explain | dot -Tsvg > graph.svg
```

Neither builds anything. A service that has not been resolved is reported
with its registration and left alone; if it was registered with `Wire`, the
dependencies it declares are drawn under it with dashed edges, each continuing
as a recorded tree where it has been built and as a declared one where it has
not, and `declared by:` names the unbuilt services that declare it. A closure
that has not run ends its branch, which is the other half of the
[known limitation](#design-notes) below: for closures the graph is what ran,
not what could run.

```
*main.Handler: scoped in root, not built (provided at main.go:35)
├╌╌ *main.Repo: singleton in root, not built (provided at main.go:33)
│   └╌╌ *main.DB: singleton in root, not built (provided at main.go:32)
│       └╌╌ main.Config: value in root, not built (provided at main.go:31)
└╌╌ *main.User: scoped in root, not built (provided at main.go:34)
    └╌╌ *net/http.Request: not provided
```

[embedmd]:# (examples/explain/main.go go)
```go
// Inspecting the graph: what a service was built from, and what needed it.
//
// Dependencies are recorded as constructors resolve them, so Explain and
// Graph describe what actually happened rather than what was registered.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/floatdrop/di"
)

type Config struct{ DSN string }
type DB struct{ dsn string }
type Repo struct{ db *DB }
type Cache struct{ db *DB }
type Server struct {
	repo  *Repo
	cache *Cache
}

func NewDB(cfg Config) *DB                       { return &DB{dsn: cfg.DSN} }
func NewRepo(db *DB) *Repo                       { return &Repo{db: db} }
func NewCache(db *DB) *Cache                     { return &Cache{db: db} }
func NewServer(repo *Repo, cache *Cache) *Server { return &Server{repo: repo, cache: cache} }

func main() {
	app := di.New()

	app.Value(Config{DSN: "postgres://localhost/app"})
	app.Wire[*DB](NewDB)
	app.Wire[*Repo](NewRepo)
	app.Wire[*Cache](NewCache)
	app.Wire[*Server](NewServer).Eager()

	if err := app.Start(context.Background()); err != nil {
		log.Fatal(err)
	}
	defer func() { _ = app.Stop(context.Background()) }()

	// What the server was built from. *DB is reached through both the repo
	// and the cache, and is expanded once.
	fmt.Print(app.Explain[*Server]())

	// And the other direction: what needed the database.
	fmt.Println()
	fmt.Print(app.Explain[*DB]())

	// Everything built so far, as Graphviz DOT: dot -Tsvg > graph.svg
	fmt.Println()
	fmt.Print(app.Graph())
}
```

### Testing your application

`di.Test` wires the production graph into a fresh scope and stops it when the
test ends, failing the test if a stop hook errors. Override what you need
before anything is resolved, marking it `Override()`.

[embedmd]:# (examples/testing/repo_test.go go)
```go
package app

import (
	"testing"

	"github.com/floatdrop/di"
)

func TestRepo(t *testing.T) {
	s := di.Test(t, Production)                     // production graph, stopped when the test ends
	s.Value(&DB{DSN: "sqlite://memory"}).Override() // replaces the production *DB, and says so

	repo := s.Get[*Repo]() // built against the fake DB
	if repo.DB.DSN != "sqlite://memory" {
		t.Fatalf("got %q", repo.DB.DSN)
	}
}
```

## Design notes

**Why generic methods.** Before Go 1.27 a typed container had to expose
package-level functions such as `do.Invoke[T](injector)`, and every variation
became another function. With generic methods the whole API lives on one
concrete type and reads left to right. The trade-off is that generic methods
cannot appear on interfaces, so `*di.Scope` is concrete; substitute
dependencies through scopes rather than by mocking the container.

**Concurrency.** Resolution is safe from many goroutines, including
goroutines a constructor starts for itself. Each singleton is built at most
once however many resolutions race for it, and the resolution path is an
immutable linked list, so parallel branches share nothing. Once the scope is
running, a resolution returns only a service whose `OnStart` has finished,
waiting if another goroutine is starting it. A cycle is reported as
`di.ErrCycle` even when the two halves are being built concurrently, which
needs a wait-for graph rather than the per-branch path alone.

Three re-entrancy limits apply. In a goroutine a constructor started, use
`Resolve` rather than `Get`: `Get` reports failure by panicking, and that
panic has no enclosing call to unwind to from another goroutine. An
`OnStart` hook must not resolve a service that depends on the one being
started, which would be a wait on itself. And no hook may call `Stop` on its
own scope or an ancestor, for the same reason; `Shutdown` never blocks.

**Known limitations.** A `Provide` closure's dependencies are only known once
it runs, so a missing dependency of a lazy service surfaces on first
resolution, or at `Start` if the service is eager. `Validate` checks what
`Wire` declares and lists the closures as unchecked
([#3](https://github.com/floatdrop/di/issues/3)). `Stop` is synchronous with
one exception: when its own context expires while a hook is still running,
the missed deadline is reported to the caller and the release finishes on a
goroutine of its own, reaching observers. A build that completes after its
scope stopped is undone the same way.

## Performance

[`benchmarks/`](benchmarks/) is a separate module comparing this package with
[samber/do](https://github.com/samber/do) on the same four-service graph, so
the library itself stays dependency-free. On an Apple M3 Max:

| | Warm resolve | Cold register and build |
|---|---|---|
| `di` | 39 ns, 64 B, 2 allocs | 3.7 µs, 4.3 kB, 70 allocs |
| `do` v2.1 | 130 ns, 192 B, 6 allocs | 6.5 µs, 11.5 kB, 120 allocs |

The cold figure counts the registration-site strings, so its byte total moves
with how deep the source sits on disk; compare allocation counts across
checkouts, not bytes.

```sh
cd benchmarks && go test -bench . -benchmem
```

## Versioning

While the major version is 0, a minor bump may change behaviour. Every entry
in [CHANGELOG.md](CHANGELOG.md) and on the
[releases page](https://github.com/floatdrop/di/releases) says whether an
upgrade can break a caller.

## Contributing

Alongside one regression test per historical defect, generative suites guard
the parts that proved easiest to get wrong: a property test over random
registration sequences, a model-based test over random operation sequences
checked against documented invariants and a lifecycle model, the same
operations run in parallel lanes under the race detector, and fuzz targets
over both.

```sh
go test -race ./...
go test -run '^$' -fuzz 'FuzzMachine$' -fuzztime 2m .
go test -race -run '^$' -fuzz FuzzMachineConcurrent -fuzztime 2m .
```

The code blocks in this README are embedded from `examples/` with
[embedmd](https://github.com/campoy/embedmd). After editing an example:

```sh
gofmt -w examples/ && go run github.com/campoy/embedmd@v1.0.0 -w README.md
```

## License

[MIT](LICENSE)
