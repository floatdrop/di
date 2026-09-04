# di

[![CI](https://github.com/floatdrop/di/actions/workflows/ci.yml/badge.svg)](https://github.com/floatdrop/di/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/floatdrop/di.svg)](https://pkg.go.dev/github.com/floatdrop/di)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A dependency-injection container for Go 1.27+, built on
[generic methods](https://go.dev/blog/generic-methods). Register services with
`s.Provide(...)`, resolve them with `s.Get[T]()`. No reflection over your
constructors, no code generation, no dependencies.

```go
app := di.New()
app.Provide(func(s *di.Scope) *DB { return s.Must(sql.Open("postgres", dsn)) }).
    OnStop(func(ctx context.Context, db *DB) error { return db.Close() })
app.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} })

repo, err := app.Resolve[*Repo]()
```

- **Keys are Go types.** No naming scheme, no string collisions between packages.
- **Constructors return `T`, not `(T, error)`.** A missing dependency or a
  failed constructor unwinds to the enclosing `Resolve` or `Start` as an
  `error` that names the full dependency path and the registration site.
- **Typed lifecycle.** `OnStart`, `OnStop`, `Run` and `Health` hooks are typed
  on the service. Nothing is discovered by sniffing interfaces.
- **Deterministic shutdown.** Reverse build order, child scopes first, every
  error reported.
- **Scopes for requests and tests.** Child scopes shadow their parent; the
  last registration of a key wins until that key has served a value.

## Installation

```sh
go get github.com/floatdrop/di
```

Requires Go 1.27 or newer. Editor support for generic methods needs gopls
v0.23 or newer.

## Quick start

Every example in this document is a compiled program under
[`examples/`](examples/) and is run in CI.

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

func main() {
	app := di.New()

	app.Value(Config{DSN: "postgres://localhost/app"})

	app.Provide(func(s *di.Scope) *DB { return &DB{dsn: s.Get[Config]().DSN} }).
		OnStop(func(ctx context.Context, db *DB) error { fmt.Println("db closed"); return nil })

	app.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} })

	app.Provide(func(s *di.Scope) *Server { return &Server{repo: s.Get[*Repo]()} }).
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
| `s.Value(v)` | An instance you already have. |
| `s.Bind[I, T]()` | An alias: `I` is served by `T`'s binding, lifetime and hooks included. |
| `s.Add(func(*di.Scope) T)` | A member of the group for `T`. |

| Method | Effect |
|---|---|
| `.Named("replica")` | Also register under a name. |
| `.Scoped()` | One instance per resolving scope, built and stopped there. |
| `.Transient()` | A new, untracked instance on every resolution. |
| `.Eager()` | Build during `Start`, in registration order. |
| `.OnStart(f)`, `.OnStop(f)` | Lifecycle hooks, `f` is `func(context.Context, T) error`. |
| `.Run(f)` | A long-running function, cancelled on stop. |
| `.Health(f)` | A health check, run by `HealthCheck`. |

Rules the container enforces:

- The last registration of a key wins. That is how a child scope shadows its
  parent and how a test substitutes a fake.
- Once a key has served a value it can no longer be replaced, in the scope
  that owns it or in any scope that resolved through it. Replacing it would
  leave one key with two live values, so it panics instead. A resolution that
  failed built nothing and leaves the key re-registerable.
- Combinations that cannot be honoured are rejected when the scope is first
  resolved, whatever order the methods were called in: hooks or `Eager` on a
  transient, `Eager` on a scoped binding, a lifetime on a `Value`, and
  lifetimes or hooks on an alias.

### Resolution

| Call | Returns |
|---|---|
| `s.Get[T]()` | `T`. Inside a constructor a failure unwinds to the caller; at top level it panics with the error. |
| `s.Resolve[T]()` | `(T, error)`. Never panics on a wiring problem. |
| `s.Lookup(di.Named[T]("replica"))` | A named binding, through a typed key. |
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

func main() {
	app := di.New()
	app.Provide(func(*di.Scope) *DB { return &DB{dsn: "postgres://localhost/app"} })

	// One child per request: request-scoped values live here, shared
	// singletons such as *DB are reused from app.
	req := app.Child("request")
	req.Value(&User{Name: "ada"})
	req.Provide(func(s *di.Scope) *Handler {
		return &Handler{db: s.Get[*DB](), user: s.Get[*User]()}
	})

	h := req.Get[*Handler]()
	fmt.Println(h.user.Name, "->", h.db.dsn)
	fmt.Println("same db:", h.db == app.Get[*DB]())
}
```

A singleton is built in the scope that registered it, so a child cannot rewire
a parent's singleton. A service that must see child-scoped values is declared
`Scoped()`: one instance per resolving scope, built there.

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

Start hooks must not block. A server binds its listener synchronously, so a
busy port fails `Start`, then serves in a goroutine.

### Draining

`OnDrain` runs before anything is stopped, from the innermost scope outwards
and in reverse build order, while every scope still resolves normally. It is
where a service stops taking new work and waits for the work it already has.

```go
app.Provide(newServer).Eager().
    OnDrain(func(ctx context.Context, srv *http.Server) error { return srv.Shutdown(ctx) }).
    OnStop(func(ctx context.Context, srv *http.Server) error { return srv.Close() })
```

An HTTP server is the case that needs it. Its handlers hold request scopes
that are children of the application scope, so shutting the server down from
`OnStop` would be racing the teardown of the scopes those handlers are still
using: a request in flight would start failing with `di.ErrStopped` before the
server had finished waiting for it. Draining first gives handlers their scopes
and dependencies until they return.

### Workers

`Run` is for anything that loops until told to stop: consumers, pollers,
schedulers.

```go
app.Provide(newMailer).Eager().Run(func(ctx context.Context, m *Mailer) error {
    return m.Loop(ctx) // returns when ctx is cancelled
})
```

The function runs in its own goroutine from the moment the service starts.
Its context is cancelled when the service stops, and `Stop` waits for it
within the stop deadline. Returning an error before that calls `Shutdown`, so
a worker that dies takes the application down rather than leaving it half
alive.

### Health checks

```go
app.Provide(newDB).Health(func(ctx context.Context, db *DB) error { return db.Ping(ctx) })

mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
    if err := app.HealthCheck(r.Context()); err != nil {
        http.Error(w, err.Error(), http.StatusServiceUnavailable)
        return
    }
    fmt.Fprintln(w, "ok")
})
```

`HealthCheck` runs the `Health` hook of every service already built in the
scope and its descendants, concurrently and bounded by the context. Each
failure wraps `di.ErrUnhealthy` and names the service.

### Request scopes

`Middleware` gives each request a child scope holding the `*http.Request`,
attaches it to the request context, and stops it when the handler returns.

```go
srv := &http.Server{Handler: app.Middleware(mux)}

mux.HandleFunc("GET /hello", func(w http.ResponseWriter, r *http.Request) {
    req, _ := di.FromContext(r.Context())
    user := req.Get[*User]()
})
```

Services that depend on the request are declared once, in the root, as
`Scoped()`; they are built per request, cached for its duration, and stopped
with it:

```go
app.Provide(func(s *di.Scope) *User {
    return &User{Name: s.Get[*http.Request]().Header.Get("X-User")}
}).Scoped()
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

	app.Provide(func(*di.Scope) *DB { return &DB{dsn: "postgres://localhost/app"} }).
		OnStop(func(ctx context.Context, db *DB) error { log.Println("db closed"); return nil })

	app.Provide(func(s *di.Scope) http.Handler {
		db := s.Get[*DB]()
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * time.Second) // simulate slow work that must not be cut short
			fmt.Fprintln(w, "served by", db.dsn)
		})
	})

	app.Provide(func(s *di.Scope) *http.Server {
		return &http.Server{Addr: ":8080", Handler: s.Get[http.Handler]()}
	}).
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
app.Observe(di.SlogObserver(slog.Default()))

app.Observe(func(ev di.Event) {
    if ev.Kind == di.EventBuild {
        buildDuration.WithLabelValues(ev.Service).Observe(ev.Duration.Seconds())
    }
})
```

Observers receive an `Event` for every constructor and every `OnStart`,
`OnStop` and `Health` hook in the scope and its descendants, plus one per
`Shutdown`. Each event names the service, its scope, the registration site,
the duration and the error, if any.

### Testing your application

`di.Test` wires the production graph into a fresh scope and stops it when the
test ends, failing the test if a stop hook errors. Override what you need
before anything is resolved.

[embedmd]:# (examples/testing/repo_test.go go)
```go
package app

import (
	"testing"

	"github.com/floatdrop/di"
)

func TestRepo(t *testing.T) {
	s := di.Test(t, Wire)                // production graph, stopped when the test ends
	s.Value(&DB{DSN: "sqlite://memory"}) // later registration wins: replaces the production *DB

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

Two re-entrancy limits apply. In a goroutine a constructor started, use
`Resolve` rather than `Get`: `Get` reports failure by panicking, and that
panic has no enclosing call to unwind to from another goroutine. And an
`OnStart` hook must not resolve a service that depends on the one being
started, which would be a wait on itself.

**Known limitations.** The dependency graph is only known once constructors
run, so a missing dependency of a lazy service surfaces on first resolution,
or at `Start` if the service is eager; there is no whole-graph validation.
Transient instances are not tracked and get no lifecycle hooks. A teardown
that `Stop` handed off, because a start step was in flight, may finish just
after `Stop` returns; the same is true of a build that completed after the
scope stopped and undid itself.

## Performance

[`benchmarks/`](benchmarks/) is a separate module comparing this package with
[samber/do](https://github.com/samber/do) on the same four-service graph, so
the library itself stays dependency-free. On Apple Silicon:

| | Warm resolve | Cold register and build |
|---|---|---|
| `di` | 53 ns, 2 allocs | 3.5 µs, 64 allocs |
| `do` v2.1 | 127 ns, 6 allocs | 6.2 µs, 120 allocs |

```sh
cd benchmarks && go test -bench . -benchmem
```

## Versioning

While the major version is 0, a minor bump may change behaviour. Every entry
in [CHANGELOG.md](CHANGELOG.md) and on the
[releases page](https://github.com/floatdrop/di/releases) says whether an
upgrade can break a caller.

## Contributing

Alongside ordinary tests, three generative suites guard the parts that proved
easiest to get wrong: a property test over random registration sequences, a
model-based test over random sequences of operations checked against
documented invariants, and a fuzz target over the same invariants.

```sh
go test -race ./...
go test -run '^$' -fuzz FuzzMachine -fuzztime 2m .
```

The code blocks in this README are embedded from `examples/` with
[embedmd](https://github.com/campoy/embedmd). After editing an example:

```sh
gofmt -w examples/ && go run github.com/campoy/embedmd@v1.0.0 -w README.md
```

## License

[MIT](LICENSE)
