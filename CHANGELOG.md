# Changelog

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

While the major version is 0, a minor bump may change behaviour. Each entry
below says plainly whether an upgrade can break a caller.

## [Unreleased]

Fixes for the six defects of the second September 2026 review, plus the
`Run`-hook overlap reported alongside them. The public API is unchanged. Every
change is behaviour; an upgrade can break a caller only in the way listed
under Changed.

### Fixed

- The drain phase is now coherent with concurrent teardown, which is what the
  phase was added for in 0.4.0. Three defects shared that root cause: a `Stop`
  that reached a scope whose drain another `Stop` was running saw the flag,
  skipped the hook and went on to release what it was still using; a scope
  marked itself stopped while a descendant's drain was in flight, so those
  hooks lost the dependencies they were draining against; and a service or
  child scope first built *by* a drain hook was missed by the phase, so it was
  either stopped without being drained or drained after the parent had already
  stopped. Draining now runs once per scope with later arrivals waiting for
  it, and sweeps the scope until a pass finds no new work. The shape all three
  hit was an HTTP server finishing in-flight requests whose handlers take a
  request scope.
- A constructor may keep the `*Scope` it was handed -- which is how a
  goroutine it starts resolves later -- without a deferred resolution through
  it being reported as a false `ErrCycle`. A finished frame of the path is no
  longer treated as an active dependency. The false cycle was also recorded on
  whatever instance that resolution was building, so one such call poisoned
  that service for the life of the process.
- One `Bind` alias to a `Scoped` target is now a distinct edge in each scope
  that holds an instance of the target, as the target's own node already was.
  Keying the alias hop on the scope the alias was registered in collapsed
  those edges and reported an acyclic graph as `ErrCycle`.
- A scope that has stopped refuses to resolve, including a resolution that was
  already waiting on a constructor running in a live ancestor. Only the
  instance's holder was re-checked after the wait, so a fully stopped child
  could still be handed a value.
- A panicking `OnStart` is a failed start rather than a successful one. The
  instance was left looking started, so a caller that recovered the panic was
  served an initialisation that never finished, and `Stop` paired an `OnStop`
  with it. The panic now reaches `Resolve` as an error, like a panicking
  constructor, which also restores the rule that `Resolve` never panics.
- A drain hook that builds into a scope the phase has already swept no longer
  leaves that instance undrained. The sweep visited each descendant once,
  which was enough for a service first built in the scope doing the draining
  and not for one built a level along, so it could be stopped without ever
  being drained. Found by the drain oracle added to the concurrent driver
  after the review, not by the review.
  Two things come with that fix rather than being separate defects, because
  revisiting a scope is what makes them reachable at all. `Stop` waits for a
  drain hook per instance, not only through the scope-wide phase: that phase
  ends per scope -- it has to, or an outer scope draining an HTTP server would
  deadlock against a handler stopping its request scope -- so a sweep can be
  draining a late instance exactly as that scope's own `Stop` reaches it. And
  an instance whose scope has already stopped is not drained at all, since
  winding a service down for work it can no longer take on is the opposite of
  what `OnDrain` is for.
- `OnStop` no longer runs while a `Run` hook is still using the value. When a
  `Run` hook outlasts `Stop`'s context, `Stop` reports the missed deadline as
  before and the release now follows that hook's own return instead of racing
  it. Service code that only followed the lifecycle API could be reading
  what `OnStop` was closing.

### Testing

- The concurrent driver checks the drain phase instead of merely running it,
  and classifies the errors an operation may panic with rather than accepting
  any of them. Four of the six defects above were invisible to it: its drain
  hooks returned nil and touched nothing, and a false `ErrCycle` out of `Get`
  read as a legitimate failure. It also gained a registration shape that is
  both `Scoped` and draining, without which a drain-owing instance could
  never appear in a child scope, which is what made the seventh and eighth
  defects reachable.

  Two oracles were considered and rejected as unsound rather than added: that
  an instance owing a drain always gets one, and that a resolution inside a
  drain hook always succeeds. Both are true of a drain phase in isolation and
  false once a second `Stop` is tearing the same scope down, so both are
  pinned deterministically instead.

### Changed

- `Stop` can now be slower to return when a drain hook builds something: the
  phase keeps sweeping until nothing new appears, and both the sweep and the
  hooks are bounded by the context passed to `Stop`. A caller that passed a
  context without a deadline and relied on drain being a single pass will
  wait longer.

### Unchanged, deliberately

- A service whose start step is in flight when `Stop` runs is still torn down
  by the goroutine running that step, so that teardown can finish just after
  `Stop` returns and its error reaches observers rather than the caller. This
  looks like a defect and is a forced one: the goroutine running the step may
  be the caller of `Stop` itself, because a start hook is allowed to stop the
  scope, and Go offers no way to tell that case from another goroutine's start
  step. Waiting would deadlock exactly those callers. `Stop`'s documentation
  states the consequence; use `Shutdown` from a hook.

## [0.4.0] - 2026-09-04

Fixes for the eleven defects of the September 2026 review, plus the
resolution-during-`Start` gap it noted without counting. The public API gains
one method, `Binding.OnDrain`, and one `EventKind`, `EventDrain`; nothing was
removed or altered. Every other change is behaviour, and an upgrade can break
a caller in the three ways listed under Changed.

### Added

- `Binding.OnDrain` and the matching `EventDrain`. `Stop` now runs a drain
  phase before anything is torn down: `OnDrain` hooks run from the innermost
  scope outwards, in reverse build order, while every scope still resolves.
  It is where a service stops taking new work and waits for what it has. An
  HTTP server belongs here rather than in `OnStop`, because its handlers hold
  request scopes that `OnStop` would be racing.

### Fixed

- Concurrent `Stop` calls no longer break dependency order. A parent that
  finds a child already stopping now waits for that teardown to finish
  instead of seeing an emptied scope and closing what the child's hooks are
  still using. The shape this hit was an HTTP request ending as the
  application shut down.
- `Run` now applies its configured `StopTimeout`, and its signal handling, to
  the rollback of a failed `Start` as well as to an ordinary exit. A correct
  `OnStop` that waits on its context used to hang there forever.
- A constructor may resolve its dependencies from several goroutines. The
  resolution path is now an immutable linked list rather than a shared slice,
  which removes a data race and the false `ErrCycle` two parallel resolutions
  of one singleton could produce.
- A dependency cycle whose halves are built concurrently is reported as
  `ErrCycle` instead of deadlocking, through a wait-for graph between
  in-flight builds.
- A `Transient` constructor goes through the same wrapper as every other one:
  a panic inside it becomes an error rather than escaping `Resolve`, and a
  successful build emits `EventBuild`.
- Shadowing a key a scope has already been served is rejected along the whole
  lookup route, not just at its end. That covers a `Bind` alias owned by an
  outer scope, and any scope between the resolver and the owner of the
  binding; both could previously end up with two live values for one key.
- A nil interface can be registered and resolved. `Get`, `Lookup`, `Resolve`,
  `Maybe`, `All` and the hook adapters no longer panic on it.
- Cycle detection compares bindings rather than keys, so a group member and a
  plain registration of the same type are no longer treated as one node.
  `All` reported a false cycle for that pair.
- A worker that dies in a child scope reaches the root's `Run` even when the
  child is stopped and detached first. The failure is wrapped once and
  reported once, whether it arrives as the cause or as a `Stop` error.
- `Middleware` registers the same `*http.Request` it passes to the handler.
  A router writes path values and the matched pattern into the request it is
  given, so the copy registered before was missing everything the route
  matched, and its context carried no scope.

### Testing

- The concurrent driver checks the drain phase instead of merely running it,
  and classifies the errors an operation may panic with rather than accepting
  any of them. Four of the six defects above were invisible to it: its drain
  hooks returned nil and touched nothing, and a false `ErrCycle` out of `Get`
  read as a legitimate failure. It also gained a registration shape that is
  both `Scoped` and draining, without which a drain-owing instance could
  never appear in a child scope, which is what made the seventh and eighth
  defects reachable.

  Two oracles were considered and rejected as unsound rather than added: that
  an instance owing a drain always gets one, and that a resolution inside a
  drain hook always succeeds. Both are true of a drain phase in isolation and
  false once a second `Stop` is tearing the same scope down, so both are
  pinned deterministically instead.

### Changed

- Once a scope is running, a resolution waits for a start step another
  goroutine is running rather than returning the service unstarted. An
  `OnStart` hook must therefore not resolve a service that depends on the one
  being started, which would be a wait on itself.
- A hook must not call `Stop` on its own scope or an ancestor. Concurrent
  `Stop` calls now wait for the first, so that would be a wait on itself.
  Use `Shutdown`, which never blocks.
- A dying `Run` hook now records its error as the cause `Run` returns, as
  well as reporting it through `Stop`. `Run` recognises the two as one
  failure and reports it once.

## [0.3.0] - 2026-09-03

The public API is unchanged from 0.2.0: no signature was added, removed or
altered. Everything here is behaviour.

### Testing

- The concurrent driver checks the drain phase instead of merely running it,
  and classifies the errors an operation may panic with rather than accepting
  any of them. Four of the six defects above were invisible to it: its drain
  hooks returned nil and touched nothing, and a false `ErrCycle` out of `Get`
  read as a legitimate failure. It also gained a registration shape that is
  both `Scoped` and draining, without which a drain-owing instance could
  never appear in a child scope, which is what made the seventh and eighth
  defects reachable.

  Two oracles were considered and rejected as unsound rather than added: that
  an instance owing a drain always gets one, and that a resolution inside a
  drain hook always succeeds. Both are true of a drain phase in isolation and
  false once a second `Stop` is tearing the same scope down, so both are
  pinned deterministically instead.

### Changed

- Combinations that were silently ignored are now rejected when the
  registration batch is committed: lifecycle hooks or `Eager` on a
  `Transient` binding, `Eager` on a `Scoped` one, lifetimes or hooks on a
  `Bind` alias, and `Scoped` or `Transient` on a `Value`.
- Re-registering a key that has already served a value panics, including
  through an alias and in a scope that resolved the key from an outer
  scope. Overriding before the key is resolved is unchanged, so child
  scopes and `di.Test` are unaffected. A resolution that failed built
  nothing, so it leaves the key re-registerable.
- Group members registered with `Add` are ordinary bindings: built once,
  started and stopped like anything else. `All` no longer re-runs their
  constructors on every call.
- A stopped scope, and every scope under it, refuses to resolve with
  `ErrStopped`.
- `OnStop` runs only when it is owed: `OnStart` succeeded, or the binding
  has no `OnStart` to pair with, in which case `OnStop` is a plain
  destructor. A service built but never started is not torn down.
- `Bind` serves its target's own instance and lifetime, so aliasing a
  transient no longer caches it and aliasing a scoped one stays per scope.
  `Eager` may now be declared on an alias.
- `Transient` builds in the scope that resolves it, like `Scoped`.
- Eagerness belongs to the key, so it transfers to whichever binding owns
  it, and eager services build in registration order rather than map order.
- `Stop` may return just before a service whose start step was in flight
  finishes being torn down; that teardown reports to observers.

### Fixed

- A service built concurrently with `Start` could be started by neither
  path yet stopped anyway.
- `Start`'s rollback ran `OnStop` on services that never started, skipped
  child scopes, and abandoned live `Run` hooks when the caller's context
  was already cancelled.
- A `Run` hook that died on its own reported to nobody unless the scope was
  driven by `Scope.Run`.
- `Stop` called from inside a start hook deadlocked.
- A rejected registration batch was partly applied, so a retried `Start`
  silently succeeded with the invalid configuration dropped.
- `Maybe` reported an alias with a missing target as present, then failed
  inside `Get`; a looping alias chain had no cycle detection.
- An eager key served through an alias panicked with a nil dereference.
- One constructor panic permanently bricked its key, because the failed
  instance caches its error and re-registration was refused.

### Added

- A property test over random registration sequences, an operation-level
  model test checked against documented invariants rather than predicted
  values, and `FuzzMachine` over the same invariants, run in CI.
- Thirty-five regression tests, each verified to fail against the commit
  that preceded its fix.

## [0.2.0] - 2026-09-03

### Added

- `di.Test`: a test scope wired from functions, stopped at cleanup, failing
  the test if a stop hook errors.
- Package overview documentation.

### Testing

- The concurrent driver checks the drain phase instead of merely running it,
  and classifies the errors an operation may panic with rather than accepting
  any of them. Four of the six defects above were invisible to it: its drain
  hooks returned nil and touched nothing, and a false `ErrCycle` out of `Get`
  read as a legitimate failure. It also gained a registration shape that is
  both `Scoped` and draining, without which a drain-owing instance could
  never appear in a child scope, which is what made the seventh and eighth
  defects reachable.

  Two oracles were considered and rejected as unsound rather than added: that
  an instance owing a drain always gets one, and that a resolution inside a
  drain hook always succeeds. Both are true of a drain phase in isolation and
  false once a second `Stop` is tearing the same scope down, so both are
  pinned deterministically instead.

### Changed

- Transient bindings documented as untracked, so `OnStop`, `Run` and
  `Health` do not apply to them.
- CI uses golangci-lint-action v9.

## [0.1.0] - 2026-09-03

First tagged release of a Go 1.27 generic-method dependency injection
container: typed registration and resolution, child and request scopes,
`Scoped` and `Transient` lifetimes, groups, typed lifecycle hooks with
rollback and deterministic stop order, `Run` hooks for workers, health
checks, `Run` and `Shutdown` for graceful termination, and observability
events.

[Unreleased]: https://github.com/floatdrop/di/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/floatdrop/di/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/floatdrop/di/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/floatdrop/di/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/floatdrop/di/releases/tag/v0.1.0
