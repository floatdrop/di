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
- **Child scopes are the test seam.** Registering in a child shadows the
  parent, so a fake is one `Value` call away.

## Install

```sh
go get github.com/floatdrop/di
```

Requires Go 1.27 or newer.

## Quick start

```go
package main

import (
	"context"
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
		OnStop(func(ctx context.Context, db *DB) error { return nil })

	app.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} })

	app.Provide(func(s *di.Scope) *Server { return &Server{repo: s.Get[*Repo]()} }).
		Eager().
		OnStart(func(ctx context.Context, srv *Server) error { return nil }).
		OnStop(func(ctx context.Context, srv *Server) error { return nil })

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Stop(ctx)
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
| `.Eager()` | Build during `Start`. |
| `.OnStart(f)` / `.OnStop(f)` | Typed lifecycle hooks, `f` is `func(context.Context, T) error`. |

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

Errors wrap `di.ErrNotProvided` or `di.ErrCycle` and read like:

```
di: building *app.Repo (provided at /src/app/wire.go:31): *app.DB: not provided (needed by [*app.Repo])
di: building *app.A (provided at ...): di: building *app.B (provided at ...): di: dependency cycle: [*app.A *app.B] -> *app.A
```

## Scopes

```go
req := app.Child("request")
req.Value(currentUser)
req.Provide(func(s *di.Scope) *Handler { return &Handler{user: s.Get[*User]()} })

h := req.Get[*Handler]()   // sees currentUser and everything in app
```

A child resolves through its parent, reuses the parent's singletons, and owns
the lifecycle of whatever it builds itself. The same mechanism gives you test
overrides:

```go
func TestRepo(t *testing.T) {
	s := app.Child("test")
	s.Value(&DB{dsn: "sqlite://memory"})
	t.Cleanup(func() { s.Stop(context.Background()) })

	repo := s.Get[*Repo]()   // built against the fake DB
}
```

## Lifecycle

`Start` builds every `Eager` binding, then runs `OnStart` hooks in build order.
`Stop` runs `OnStop` hooks in reverse build order and returns all failures
joined with `errors.Join`. Only services that were actually built are stopped.

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
