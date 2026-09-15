<p align="center">
  <img src="docs/assets/logo.svg" width="280" alt="di — a Go gopher joining two coral cable connectors" />
</p>

# di

[![CI](https://github.com/floatdrop/di/actions/workflows/ci.yml/badge.svg)](https://github.com/floatdrop/di/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/floatdrop/di.svg)](https://pkg.go.dev/github.com/floatdrop/di)
[![coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Ffloatdrop.github.io%2Fdi%2Fcoverage.json)](https://floatdrop.github.io/di/coverage.html)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Awesome Go](https://raw.githubusercontent.com/floatdrop/awesome-go/main/badges/floatdrop--di.svg)](https://floatdrop.github.io/awesome-go/#floatdrop--di)

A dependency-injection container for Go 1.27+. Constructors are plain
functions, keys are Go types, and the container builds, starts and stops
services in dependency order.

```go
app := di.New()
app.Value(Config{DSN: "postgres://localhost/app"})
app.Wire[*DB](NewDB)     // func NewDB(Config) (*DB, error)
app.Wire[*Repo](NewRepo) // func NewRepo(*DB) *Repo

repo := app.Get[*Repo]() // builds Config, then DB, then Repo, each once
```

There is no code generation and no dependency outside the standard library.
`Wire` reads a constructor's signature once, with reflection, which is how
`Validate` can check the whole graph before a single constructor runs.
`Provide` takes a closure for the rare constructor that needs the scope
itself.

[**The guide**](https://floatdrop.github.io/di/) walks through one application
file by file. [**How it works**](docs/DESIGN.md) explains what happens between
`Get` and a value, with diagrams.

## Installation

```sh
go get github.com/floatdrop/di
```

Requires Go 1.27 or newer. Editor support for generic methods needs gopls
v0.23 or newer.

## Quick start

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

The sections follow the order an application comes together. Every
embedded example is a compiled program under [`examples/`](examples/), built
in CI.

### Registering and resolving

Each registration returns a `Binding[T]`. Its methods must be called before
the scope is first resolved.

| Call | Registers |
|---|---|
| `s.Provide(func(*di.Scope) T)` | A lazily built singleton. `T` is inferred. |
| `s.Wire[T](NewT)` | A lazily built singleton from a plain constructor. Its parameters are its dependencies; see [Validate](#validate). |
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
| `.Go(f)` | A worker: a long-running function in a goroutine of its own, cancelled on stop. |

To get a service back:

| Call | Returns |
|---|---|
| `s.Get[T]()` | `T`. Inside a constructor a failure unwinds to the caller; at top level it panics with the error. |
| `s.Resolve[T]()` | `(T, error)`. Never panics on a wiring problem. |
| `s.Maybe[T]()` | `(T, bool)`; see [Optional dependencies](#optional-dependencies). |
| `s.All[T]()` | Every member of the group for `T`, across the scope chain. |
| `s.Must(v, err)` | `v`, or aborts the constructor with `err`. |
| `s.Context()` | The context passed to `Start`, so constructors can dial with a deadline. |

Errors wrap `di.ErrNotProvided`, `di.ErrCycle` or `di.ErrStopped`:

```
di: building *app.Repo (provided at app/wire.go:31): *app.DB: not provided (needed by [*app.Repo])
di: building *app.A (provided at ...): di: building *app.B (provided at ...): di: dependency cycle: [*app.A *app.B] -> *app.A
```

`Provide` takes a closure that pulls its dependencies with `s.Get`. `Wire`
takes a plain constructor, `func(A, B) T` or `func(A, B) (T, error)`; its
parameters are its dependencies, so `Validate` can check them before
anything runs, and a wrong shape is rejected at registration. A slice
parameter is a key, not a group. A constructor that needs the scope itself
stays a closure.

An interface is served by a constructor that returns the implementation:

```go
app.Provide(func(s *di.Scope) Reader { return s.Get[*Repo]() })
app.Wire[Reader](NewRepo) // the same, when NewRepo returns *Repo
```

Both keys share one instance. Mark it `Scoped()` too when the target is.
`Wire` accepts any result assignable to the key.

Three rules, checked when the scope is next resolved:

- A second registration of a key in one scope must say `Override()`, or it
  is rejected naming both sites; so is an `Override()` with nothing to
  override. It inherits the key's eagerness. A child shadows its parent's
  key without the marker.
- A key that has served a value cannot be replaced, in its own scope or in
  any scope that resolved through it.
- `Eager` on a `Scoped` binding, and `Scoped` on a `Value`, are rejected.

#### Optional dependencies

There is no `optional` marker: a dependency nothing provides is an error. A
service that can do without something provides the absence instead.

```go
app.Value[*Cache](nil)           // a nil default: provided, checked, overridden where there is one
app.Wire[Metrics](NewNopMetrics) // a null object: nothing downstream has a branch
app.Provide(func(s *di.Scope) *Report {
    _, traced := s.Maybe[*Tracer]() // presence, for a key nothing registers at all
    return NewReport(s.Get[*Store](), traced)
})
```

A nil default makes the key present, so `Maybe` reports it provided, with
nil in it. `Maybe` records nothing when the key is absent, so a later
registration is not rejected and a constructor that already ran will not
see it; only a closure can ask, so `Validate` lists it as unchecked. A
pointer parameter is not optional on its own; that is fx's `optional:"true"`
tag, registered rather than tagged.

### Lifecycle

`Start` builds every `Eager` binding, then runs `OnStart` hooks in build
order. If one fails, `Start` stops what had started, child scopes included,
and returns both errors. A service built after `Start` runs its `OnStart` as
it is built.

`Stop` drains, stops the child scopes, then runs `OnStop` hooks in reverse
build order, and joins every failure into its error. `OnStop` runs when
`OnStart` succeeded or when there is none. After `Stop`, the scope and
everything under it resolves with `di.ErrStopped`.

`Stop` is safe to call twice or concurrently: later calls wait for the first
and report its result. So a hook must not call `Stop` on its own scope or an
ancestor; it calls `Shutdown`, which never blocks.

`OnStart` returns when the service is ready. A server binds its listener in
the hook, so a busy port fails `Start`, and serves in a goroutine.

#### Draining

`OnDrain` runs before anything is stopped, innermost scope first, while
every scope still resolves. It is where a service stops taking work and
finishes what it has.

```go
app.Wire[*http.Server](newServer).Eager().
    OnDrain(func(ctx context.Context, srv *http.Server) error { return srv.Shutdown(ctx) }).
    OnStop(func(ctx context.Context, srv *http.Server) error { return srv.Close() })
```

An HTTP server's handlers hold request scopes, and shutting it down from
`OnStop` would race their teardown. Draining first keeps them alive until
the handlers return.

#### Workers

`Go` registers a worker: a function that loops until told to stop, in a
goroutine of its own. It is errgroup's `Go`, applied to a service.

```go
app.Wire[*Mailer](newMailer).Eager().Go(func(ctx context.Context, m *Mailer) error {
    return m.Loop(ctx) // returns when ctx is cancelled
})
```

The context is cancelled when the service stops, and `Stop` waits for the
function within its deadline. A worker that returns an error calls
`Shutdown`; `context.Canceled` after cancellation means nothing.

#### Run and Shutdown

`Run` is the helper for `main`: start, block until the context is cancelled,
`SIGINT` or `SIGTERM` arrives, or `Shutdown` is called, then stop with a
bounded context. A second signal cancels that context.

<details>
<summary><code>examples/server/main.go</code>, an HTTP server with OnStart, OnDrain and OnStop, run with a stop timeout</summary>

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

</details>

### Scopes

A child scope resolves through its parent, shares its singletons, and owns
what it builds itself.

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

A singleton is built in the scope that registered it. A service that has
to see a child's values is `Scoped()`: one instance per resolving scope,
built there.

#### Request scopes

`dihttp.Middleware` gives each request a child scope holding the
`*http.Request`, puts it in the request context, and stops it when the
handler returns. It has the `func(http.Handler) http.Handler` shape.
`dihttp.Module` registers one:

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
the method; mark the type `Scoped()` when it needs the request.
`di.FromContext(r.Context())` reaches the scope by hand, and
`dihttp.NewMiddleware(app)` makes a middleware outside the container.
Services that depend on the request are declared once, in the root, as
`Scoped()`:

```go
app.Wire[*User](func(r *http.Request) *User { return &User{Name: r.Header.Get("X-User")} }).Scoped()
```

`di.WithScope` and `di.FromContext` are the primitives without `net/http`.
[`examples/app`](examples/app/main.go) is a complete service.

#### Values that change

There is no transient lifetime. A value made fresh on every use is a
function, registered and resolved like any service. Its type is the key, so
it is named:

```go
type NewID func(n int) string

app.Wire[NewID](func(cfg Config) NewID {
    return func(n int) string { return fmt.Sprintf("%s-%d", cfg.Prefix, n) }
})
```

The container builds the factory once and never sees what it makes. A value
that needs a lifecycle of its own is `Scoped()` and resolved from a child
opened for one unit of work, which stops it.

A value that changes while the application runs, such as reloaded
configuration, is held by a service that does not. A lifetime would not
help: a singleton's constructor is called once and captures one snapshot
regardless.

<details>
<summary><code>examples/reload/main.go</code>, the program that prints the output below</summary>

[embedmd]:# (examples/reload/main.go go)
```go
// Values that change: a live value is held by a service that does not
// change. A long-lived service reads the source when it needs the value; a
// per-request service takes a snapshot, Scoped, so one request sees one
// configuration and the next request sees the current one.
package main

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"

	"github.com/floatdrop/di"
)

type Config struct{ RateLimit int }

// Source holds the current configuration. Current is what a long-lived
// service calls at the moment it needs the value; Watch is the worker that
// applies reloads as they arrive. Here they arrive on a channel; a real
// source follows a file, a signal or a remote endpoint.
type Source struct {
	cur     atomic.Pointer[Config]
	reloads chan Config
	applied chan struct{}
}

func NewSource() *Source {
	s := &Source{reloads: make(chan Config), applied: make(chan struct{})}
	s.cur.Store(&Config{RateLimit: 100}) // the initial read
	return s
}

func (s *Source) Current() Config { return *s.cur.Load() }

func (s *Source) Watch(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case cfg := <-s.reloads:
			s.cur.Store(&cfg)
			fmt.Println("reloaded:", cfg.RateLimit, "per minute")
			s.applied <- struct{}{}
		}
	}
}

// Reload hands a new configuration to the watcher and returns once it has
// been applied, which keeps this program's output in order.
func (s *Source) Reload(cfg Config) { s.reloads <- cfg; <-s.applied }

// A singleton lives as long as the application, so it takes the source and
// reads it on every use. Taking Config instead would capture one snapshot
// when the limiter is built, whatever lifetime Config had.
type Limiter struct{ src *Source }

func NewLimiter(src *Source) *Limiter { return &Limiter{src: src} }
func (l *Limiter) Limit() int         { return l.src.Current().RateLimit }

// A per-request service takes Config as a plain parameter. Its snapshot is
// built with the request scope and lives as long as it does.
type Handler struct{ cfg Config }

func NewHandler(cfg Config) *Handler { return &Handler{cfg: cfg} }

func main() {
	app := di.New()
	app.Wire[*Source](NewSource).Eager().
		Go(func(ctx context.Context, src *Source) error { return src.Watch(ctx) })
	app.Wire[Config](func(src *Source) Config { return src.Current() }).Scoped()
	app.Wire[*Limiter](NewLimiter)
	app.Wire[*Handler](NewHandler).Scoped()

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer func() { _ = app.Stop(ctx) }()

	limiter := app.Get[*Limiter]()
	fmt.Println("limiter:  ", limiter.Limit(), "per minute")

	a := app.Child("request a")
	fmt.Println("request a:", a.Get[*Handler]().cfg.RateLimit, "per minute")

	app.Get[*Source]().Reload(Config{RateLimit: 200})

	// The limiter reads the new value; request a keeps the snapshot it was
	// built with; a new request is built with the current one.
	fmt.Println("limiter:  ", limiter.Limit(), "per minute")
	fmt.Println("request a:", a.Get[*Handler]().cfg.RateLimit, "per minute")
	b := app.Child("request b")
	fmt.Println("request b:", b.Get[*Handler]().cfg.RateLimit, "per minute")

	_ = a.Stop(ctx)
	_ = b.Stop(ctx)
}
```

</details>

```
limiter:   100 per minute
request a: 100 per minute
reloaded: 200 per minute
limiter:   200 per minute
request a: 100 per minute
request b: 200 per minute
```

A long-lived service takes the source and reads `Current()` when it needs
the value. A per-request service takes `Config`: a `Scoped()` snapshot
wired from the source, built once per request scope. Resolve it from
request scopes; one resolved from the application scope is cached there. A
service that must react to a change subscribes to the source.

#### Groups

`Group()` makes a registration a member of the group for its type, and
`All` resolves every member across the scope chain. The usual case is a
health endpoint:

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

`All` builds members not built yet. Members keep their own lifetimes and
hooks. A plain registration of the same type is neither shadowed by the
group nor part of it.

#### More than one instance of a type

A key is a Go type, so two live instances of one type need two types. While
the set is fixed, a defined type names each one and embedding keeps the
methods: `type Primary struct{ *DB }`. The surrogate appears in constructor
signatures and nowhere else; `Wire` resolves it like any parameter, and
`Validate` and `Explain` see two services.

When the set comes from configuration, register the instances as a group,
fold them into a registry, and let the resolving scope pick: the selector is
a constructor whose parameter is the key, marked `Scoped()`.

<details>
<summary><code>examples/instances/main.go</code>, a primary and a replica by type, and configured shards by a scoped selector</summary>

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

</details>

Rules and traps:

- Use `Wire` for the selector. Its parameter declares the key, so `Validate`
  reports it in `Owed` and `Validate(di.Provided[T]())` discharges it. A
  `Provide` closure declares nothing, so the missing key is found by the
  first request.
- One key resolves to one value per scope. Two shards at once means the
  registry, or a scope each.
- A type alias is not a new key: `type CacheDB = *DB` is the same
  `reflect.Type`. A surrogate is a defined type.
- A surrogate that embeds an interface satisfies it but is still a separate
  key: registering `Cold` does not serve `Store`.

### Composing modules

A module is a function that registers into a scope. `Use` applies modules
in order and records which made each registration:

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

Without that rule the second `*DB` would silently rewire `Storage`'s
`*Repo`. Two modules that each need their own `*DB` declare distinct types;
a module that means to replace another's registration says `Override()`.

Keys are types, so an unexported type can be named, and so resolved,
overridden or wrapped, only by its own package. That is the privacy model: a
module exports its contract and its `Module` function.

```go
type db struct{ dsn string }               // only this package can say Get[*db]()

func Module(s *di.Scope) {
    s.Wire[*db](newDB).OnStop(func(_ context.Context, db *db) error { return db.Close() })
    s.Wire[Store](newPGStore)                  // the exported contract
}
```

`Explain`, `Graph` and `Validate` still see private services. A test
overrides what is exported.

#### Wrapping a service

`Wrap[T]` composes over whatever serves `T` when it is called: the latest
registration in this scope, or an ancestor's. The function takes the wrapped
value first, then its dependencies, and returns `T` or `(T, error)`. The
wrapped registration keeps its hooks and lifetime, is built first and
stopped after. Wrappers chain in registration order. uber/fx calls this
`Decorate`.

<details>
<summary><code>examples/wrap/main.go</code>, the program that prints the output below</summary>

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

</details>

```
app:   row 1
debug: traced(row 1)
main.Store: singleton wrapper in root, built (provided at main.go:40)
├── main.Store: singleton in root, built (provided at main.go:34)
└── *main.Cache: value in root, built (provided at main.go:33)
needed by: main.Store in debug
pg closed
```

Three rules:

- A wrapper takes the lifetime of what it wraps; `Scoped()` on the wrapper
  puts one per scope around a shared singleton.
- A wrapper in a child scope applies to that child and its descendants.
- `Override()` after a wrapper replaces the whole chain, and a wrapped
  registration cannot be overridden while the wrapper stands. Nothing to
  wrap, a group, and a key already resolved are rejected.

#### Testing

`di.Test` wires the production graph into a fresh scope, stops it when the
test ends, and fails the test if a stop hook errors. Override what you need
before anything is resolved, and mark it `Override()`.

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

### Checking and inspecting

#### Validate

A wired constructor's dependencies are known at registration, so the graph
can be checked with nothing built. `Validate` reports what would fail: a
missing dependency, a cycle, or a singleton that would build a
request-scoped service in the wrong scope. `Provide` closures are listed as
unchecked.

<details>
<summary><code>examples/wire/main.go</code>, the program that prints the output below</summary>

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

</details>

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

A `Scoped` binding is checked as the calling scope would resolve it, so what
that scope does not provide is owed rather than wrong. Call `Validate` from
the scope that will provide it, or say what it holds with
`di.Provided[T]()`.

#### Explain, Graph and Modules

Three renderers: `Explain` for one service, `Graph` for everything built,
`Modules` for what each module provides and needs. What was built has a
recorded graph; what was wired has a declared one.

`Explain[T]` prints one service's dependency tree with lifetime, scope,
phase and registration site, then what needed it:

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

A dependency reached twice is expanded once.

`Graph` renders everything built in a scope and its descendants as Graphviz
DOT, one cluster per scope:

```sh
go run ./examples/explain | dot -Tsvg > graph.svg
```

`Modules` reports what each module provides, needs and wraps, and which
module serves each need. The guide application's report, before anything is
built:

[embedmd]:# (examples/guide/testdata/modules.txt)
```txt
config.Module
  provides   config.Config
storage.Module
  provides   *storage.db, storage.Store
  needs      config.Config ← config.Module
cache.Module
  provides   *cache.cache
  wraps      storage.Store ← storage.Module
mail.Module
  provides   *mail.Mailer
dihttp.Module
  provides   dihttp.Middleware
  unchecked  dihttp.Middleware (closures: needs known when they run)
api.Module
  provides   *api.Caller, *api.Users, *api.Health, *http.Server
  needs      *http.Request ← owed to a resolving scope
             storage.Store ← cache.Module
             *mail.Mailer ← mail.Module
             config.Config ← config.Module
             dihttp.Middleware ← dihttp.Module
```

None of the three builds anything: an unbuilt wired service shows its
declared dependencies dashed, as under `Validate`, and a closure that has
not run ends its branch
([known limitation](docs/DESIGN.md#known-limitations)).

<details>
<summary><code>examples/explain/main.go</code>, the program that prints the first two trees in this section</summary>

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

</details>

#### Observability

```go
app.Observe(func(ev di.Event) {
    if ev.Kind == di.EventBuild {
        buildDuration.WithLabelValues(ev.Service).Observe(ev.Duration.Seconds())
    }
})
```

Observers receive an `Event` for every constructor and hook in the scope
and its descendants, and one per `Shutdown`: service, package, scope,
module, site, duration and error. [`dislog`](dislog/) is that function
written against `log/slog`:

```go
app.Observe(dislog.New(slog.Default()))
```

A failed step is logged at `slog.LevelError` with the site, anything else
at `slog.LevelInfo` or the level `dislog.Level` sets; `dislog.Site()` logs
the site every time.

<details>
<summary><code>examples/observe/main.go</code>, the program that prints the output below</summary>

[embedmd]:# (examples/observe/main.go go)
```go
// Observe: the container's lifecycle as log lines. dislog.New turns a
// *slog.Logger into the observer Observe takes, so the application says what
// it is doing as it builds, starts and stops.
package main

import (
	"context"
	"log/slog"
	"os"

	charm "github.com/charmbracelet/log"
	"github.com/floatdrop/di"
	"github.com/floatdrop/di/dislog"
	"github.com/floatdrop/di/examples/observe/internal/store"
)

type Repo struct{ db *store.DB }

func NewRepo(db *store.DB) *Repo { return &Repo{db} }

func main() {
	// dislog imports only log/slog, so any handler will do. This one is
	// charmbracelet/log, which is an slog handler that colours its output.
	logger := slog.New(charm.New(os.Stderr))

	app := di.New()

	// Observers see this scope and every scope under it, so registering first
	// means the whole wiring is logged.
	app.Observe(dislog.New(logger))

	app.Value(store.Config{DSN: "postgres://localhost/app"})
	app.Use(store.Module)
	app.Wire[*Repo](NewRepo)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		logger.Error("start", "err", err)
		os.Exit(1)
	}

	// Built after Start, so this build is logged here, between the two
	// phases, and its OnStart would run as it is handed out. It is a type in
	// main, which has no import path to lift out, so it gets no pkg.
	_ = app.Get[*Repo]()

	// Stop returns what the hooks reported as well as logging it, so a
	// caller that wants to act on a teardown failure still can.
	if err := app.Stop(ctx); err != nil {
		logger.Warn("stopped with failures", "err", err)
	}
}
```

</details>

```
INFO build service=store.Config pkg=github.com/acme/app/internal/store scope=root duration=25.667µs
INFO build service=*store.DB pkg=github.com/acme/app/internal/store scope=root module=store.Module duration=286.333µs
INFO start service=*store.DB pkg=github.com/acme/app/internal/store scope=root module=store.Module duration=792ns
INFO build service=*main.Repo scope=root duration=26.417µs
ERRO stop service=*store.DB pkg=github.com/acme/app/internal/store scope=root module=store.Module duration=4.709µs site=store.go:20 err="di: stopping *store.DB: connection reset"
WARN stopped with failures err="di: stopping *store.DB: connection reset"
```

Observers see the scope they are registered on and everything under it.
Events arrive on the goroutine that did the work, so a slow handler slows
the application.

## Performance

[`benchmarks/`](benchmarks/) is a separate module comparing this package with
[samber/do](https://github.com/samber/do) and
[uber-go/dig](https://github.com/uber-go/dig) on the same four-service graph,
so the library itself stays dependency-free. On an Apple M3 Pro:

| | Warm resolve | Cold register and build |
|---|---|---|
| `di`, `Provide` closure | 38 ns, 64 B, 2 allocs | 3.7 µs, 4.5 kB, 70 allocs |
| `di`, `Wire` | 38 ns, 64 B, 2 allocs | 4.2 µs, 4.9 kB, 77 allocs |
| `do` v2.1 | 125 ns, 192 B, 6 allocs | 6.1 µs, 11.5 kB, 120 allocs |
| `dig` v1.19 | 445 ns, 768 B, 24 allocs | 16.4 µs, 24.3 kB, 302 allocs |

`di` is measured twice because `dig.Provide` is reflective like `Wire`
rather than like a closure; the warm figures are the same, and the cold
difference is the signature read and `reflect.Call`.

**The `dig` warm figure needs a caveat.** dig has no typed accessor, so the
nearest thing to a resolve is `Invoke` with a function dig reflects over on
every call, and an fx application invokes once at startup. The cold
comparison is the fair one; read the warm number as what dig costs on a
request path, which it does not set out to serve.

```sh
cd benchmarks && go test -bench . -benchmem
```

## Versioning

While the major version is 0, a minor bump may change behaviour. Every entry
in [CHANGELOG.md](CHANGELOG.md) and on the
[releases page](https://github.com/floatdrop/di/releases) says whether an
upgrade can break a caller.

## Contributing

There is one regression test per historical defect, and generative suites
for the parts that proved easiest to get wrong: a property test over random
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
