# Changelog

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

While the major version is 0, a minor bump may change behaviour. Each entry
below says plainly whether an upgrade can break a caller.

## [Unreleased]

## [0.3.0] - 2026-09-03

The public API is unchanged from 0.2.0: no signature was added, removed or
altered. Everything here is behaviour.

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

[Unreleased]: https://github.com/floatdrop/di/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/floatdrop/di/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/floatdrop/di/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/floatdrop/di/releases/tag/v0.1.0
