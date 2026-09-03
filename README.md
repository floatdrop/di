# di

[![CI](https://github.com/floatdrop/di/actions/workflows/ci.yml/badge.svg)](https://github.com/floatdrop/di/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/floatdrop/di.svg)](https://pkg.go.dev/github.com/floatdrop/di)

A small dependency-injection container for Go 1.27+, built on generic methods.
Services are registered with `s.Provide(...)` and resolved with `s.Get[T]()`.
No reflection over your constructors, no code generation, no dependencies.

> Status: early. The API is small on purpose and may still change.

## Why another one

Go 1.27 added [generic methods](https://go.dev/blog/generic-methods). Before
that, a typed container had to expose package-level functions such as
`do.Invoke[T](injector)`, and every variation (named, transient, must,
with-context) became another function. With generic methods the whole API fits
on one concrete type and reads left to right:

```go
app.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} })
repo := app.Get[*Repo]()
```

Beyond the syntax, the design makes a few deliberate choices:

- **Keys are Go types, not strings.** Services are keyed by `reflect.Type`
  identity plus an optional name, so same-named types in different packages
  never collide and there is no naming scheme to learn.
- **Constructors return `T`, not `(T, error)`.** Inside a constructor,
  `s.Get[X]()` either returns the dependency or unwinds to the enclosing
  `Resolve`/`Start` call, which reports a normal `error` with the full path and
  the file:line where the failing service was registered.
- **Typed lifecycle hooks.** `OnStart`/`OnStop` take `func(context.Context, T)
  error` on the binding. Nothing is discovered by sniffing interfaces.
- **Deterministic shutdown.** `Stop` runs hooks in reverse build order,
  sequentially, and joins every error.
- **Overrides are the test seam.** The last registration of a key wins, so a
  test wires the production graph into a fresh scope and re-registers the one
  thing it wants faked. No mocks of the container are needed.

## Install

```sh
go get github.com/floatdrop/di
```

Requires Go 1.27 or newer.

## Quick start

Every snippet below lives under [`examples/`](examples/) and is compiled in CI.

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

## Registration

Every registration returns a typed `Binding[T]` whose methods refine it.
Call them before the scope is first resolved; afterwards they panic.

| Call | Meaning |
|---|---|
| `s.Provide(func(*di.Scope) T)` | Lazily built singleton. `T` is inferred from the constructor. |
| `s.Value(v)` | An instance you already have. |
| `s.Bind[I, T]()` | Serve requests for interface `I` from `T`'s binding. Checked at registration. |
| `s.Add(func(*di.Scope) T)` | Append to the multi-binding group for `T`. |
| `.Named("replica")` | Register under a name in addition to the type. |
| `.Transient()` | Build a fresh instance on every resolution. |
| `.Scoped()` | One instance per resolving scope, built and stopped there. Not allowed on `Value`. |
| `.Eager()` | Build during `Start`. |
| `.OnStart(f)` / `.OnStop(f)` | Typed lifecycle hooks, `f` is `func(context.Context, T) error`. |
| `.Run(f)` | Long-running function for `T`, run in its own goroutine and cancelled on stop. |
| `.Health(f)` | Health check for `T`, run by `HealthCheck`. |

Later registrations of the same key override earlier ones, which is how a
child scope shadows its parent.

## Resolution

| Call | Meaning |
|---|---|
| `s.Get[T]()` | Resolve `T`. Inside a constructor a failure unwinds to the caller; at top level it panics with the error. |
| `s.Resolve[T]()` | Same, returning `(T, error)`. |
| `s.Lookup(di.Named[T]("replica"))` | Resolve a named binding through a typed key. |
| `s.Maybe[T]()` | `(T, bool)` for optional dependencies. |
| `s.All[T]()` | Every member of the group for `T`, across the scope chain. |
| `s.Must(v, err)` | Inside a constructor: unwrap a `(T, error)` pair, aborting on error. `db := s.Must(sql.Open(...))` |
| `s.Context()` | The context passed to `Start`/`Run`, so constructors can dial with a deadline. |

Errors wrap `di.ErrNotProvided` or `di.ErrCycle` and read like:

```
di: building *app.Repo (provided at /src/app/wire.go:31): *app.DB: not provided (needed by [*app.Repo])
di: building *app.A (provided at ...): di: building *app.B (provided at ...): di: dependency cycle: [*app.A *app.B] -> *app.A
```

## Scopes

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

A child resolves through its parent, reuses the parent's singletons, and owns
the lifecycle of whatever it builds itself. A singleton always builds its
dependencies in the scope that registered it, so a child cannot rewire a
parent singleton. A service that must see child-scoped values is declared
`Scoped()`: one instance per resolving scope, built there.

For tests, wire the production graph into a fresh scope and override before
anything is resolved:

[embedmd]:# (examples/testing/repo_test.go go)
```go
package app

import (
	"context"
	"testing"

	"github.com/floatdrop/di"
)

func TestRepo(t *testing.T) {
	s := di.New()
	Wire(s)
	s.Value(&DB{DSN: "sqlite://memory"}) // later registration wins: replaces the production *DB
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	repo := s.Get[*Repo]() // built against the fake DB
	if repo.DB.DSN != "sqlite://memory" {
		t.Fatalf("got %q", repo.DB.DSN)
	}
}
```

## Lifecycle

`Start` builds every `Eager` binding, then runs `OnStart` hooks in build order.
If a hook fails, the hooks that already ran are rolled back with `OnStop` and
`Start` returns both errors. A service built after `Start`, lazily or in a
child scope, runs its `OnStart` as part of being built, so nothing is ever
handed out unstarted; if that hook fails the resolution fails.

`Stop` stops child scopes first, then runs `OnStop` hooks in reverse build
order and returns all failures joined with `errors.Join`. Only services that
were actually built are stopped. Stopping a child scope also detaches it from
its parent, which is what releases a per-request scope. A stopped scope, and
any scope under it, refuses to resolve anything with `di.ErrStopped`, so a
closed service is never handed out.

Group members registered with `Add` are ordinary bindings: singletons by
default, built once, started and stopped like everything else.

Start hooks must not block. A server binds its listener synchronously so a
busy port fails `Start`, then serves in a goroutine.

## Background workers

A binding's `Run` hook is for anything that loops until told to stop:
consumers, pollers, schedulers.

```go
app.Provide(newMailer).Eager().Run(func(ctx context.Context, m *Mailer) error {
    return m.Loop(ctx) // returns when ctx is cancelled
})
```

The function starts in its own goroutine when the service starts. Its
context is cancelled when the service stops, in reverse build order, and
`Stop` waits for it to return within the stop deadline. Returning a non-nil
error before that calls `Shutdown` with it, so a worker that dies takes the
application down instead of leaving it half alive. `http.Server` does not
take a context, so it keeps using `OnStart` and `OnStop`.

## Health checks

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

`HealthCheck` runs every `Health` hook of the services already built in the
scope and its descendants, concurrently, bounded by the context, and returns
the failures joined. Each failure wraps `di.ErrUnhealthy` and names the
service. Services that were never built are not checked.

## Request scopes

`Middleware` creates a child scope per request, registers the
`*http.Request` in it, attaches it to the request context, and stops it when
the handler returns.

```go
srv := &http.Server{Handler: app.Middleware(mux)}

mux.HandleFunc("GET /hello", func(w http.ResponseWriter, r *http.Request) {
    req, _ := di.FromContext(r.Context())
    user := req.Get[*User]() // built against this request's *http.Request
})
```

Services that depend on the request are declared once, in the root, with
`Scoped()`:

```go
app.Provide(func(s *di.Scope) *User {
    return &User{Name: s.Get[*http.Request]().Header.Get("X-User")}
}).Scoped()
```

A scoped binding is built in the scope that resolves it, so it sees that
request's `*http.Request`, is cached for the rest of the request, and is
stopped with it. Resolving it from the root fails with `ErrNotProvided`
rather than producing an instance built with the wrong data. `di.WithScope`
and `di.FromContext` are the primitives if you are not using `net/http`.

The complete pattern, with a worker, a health endpoint, request scopes, and
graceful shutdown, is in [`examples/app`](examples/app/main.go).

## Graceful shutdown

`Run` is the main-function helper: it starts the scope, blocks until the
context is cancelled, `SIGINT`/`SIGTERM` arrives, or something calls
`Shutdown`, then stops everything with a bounded context (15 seconds by
default, see `di.StopTimeout`). A second signal during the stop cancels that
context so a hung hook cannot keep the process alive.

`Shutdown(err)` can be called from any goroutine, never blocks, and the first
call wins. Use it when a long-running service dies on its own; `Run` returns
the error you passed.

[embedmd]:# (examples/server/main.go go)
```go
// Graceful shutdown of an HTTP server.
//
// Run starts the scope, waits for SIGINT/SIGTERM or a Shutdown call, then
// stops everything in reverse order with a bounded context. The server's
// OnStop calls http.Server.Shutdown, which stops accepting connections and
// waits for in-flight requests until the stop context expires.
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
		OnStop(func(ctx context.Context, srv *http.Server) error {
			log.Println("draining")
			return srv.Shutdown(ctx) // waits for in-flight requests, bounded by StopTimeout
		})

	// Blocks until Ctrl-C, SIGTERM, or app.Shutdown. A second signal cancels
	// the stop context so a hung hook cannot keep the process alive.
	if err := app.Run(context.Background(), di.StopTimeout(10*time.Second)); err != nil {
		log.Fatal(err)
	}
}
```

## Concurrency

Resolution is safe to call from many goroutines. Each singleton is built at
most once; the per-call resolution path is kept off the scope so concurrent
resolutions never share state.

## Benchmarks

The `benchmarks/` directory is a separate module comparing this package with
[samber/do](https://github.com/samber/do) on the same four-service graph.
Indicative numbers on Apple Silicon:

| | Warm resolve | Allocs | Cold register + build |
|---|---|---|---|
| `di` | 42 ns | 3 | 2.5 µs, 46 allocs |
| `do` v2.1 | 119 ns | 6 | 6.1 µs, 120 allocs |

```sh
cd benchmarks && go test -bench . -benchmem
```

## Contributing

README code blocks are generated from `examples/` with
[embedmd](https://github.com/campoy/embedmd). After editing an example run:

```sh
go run github.com/campoy/embedmd@v1.0.0 -w README.md
```

## Limitations

- The dependency graph is only known once constructors run, so a missing
  dependency of a lazy service surfaces on first resolution or at `Start` if
  the service is eager. There is no whole-graph validation yet.
- Generic methods cannot live on interfaces, so `*di.Scope` is a concrete
  type. Use child scopes rather than mocks to substitute dependencies.
- Generic methods are invisible to `reflect`; the container does not rely on
  that, but tooling that discovers methods reflectively will not see them.

## License

MIT
