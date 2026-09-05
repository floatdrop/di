# Changelog

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

While the major version is 0, a minor bump may change behaviour. Each entry
below says plainly whether an upgrade can break a caller.

## [Unreleased]

### Fixed

- `Scope.Run` reports a cause published through `Shutdown` while a failed
  `Start` was rolling back, not only one published during an ordinary stop. A
  rollback runs the drain and stop hooks, so a worker can die there exactly as
  it can during a shutdown; the error branch returned before the cause was
  ever read.
- A `Stop` reports the failure of a drain hook of its own scope even when an
  ancestor's `Stop` owned the phase and ran the hook. The waiter dropped the
  owner's error on the grounds that it reached the caller through the `Stop`
  that owned the drain -- true when that is the same call, and false for a
  request scope ending while the application shuts down, which is exactly
  where the failure needed reporting. Each scope's drain errors now settle
  into that scope's phase and reach the aggregate through its own `Stop`, so
  they are reported to every caller that should hear them and still appear
  once in any one error.
- A key cannot be overridden while a resolution of it is in flight. `used` is
  set only once a value has been served, so a constructor could register over
  its own key and resolve the replacement: the nested call was served the new
  value and the outer call returned the old one -- two live values for one
  key, from a single goroutine, past a guard meant to prevent exactly that.
  Re-registering after a *failed* resolution still works, which is how a key
  whose constructor failed is recovered.

Nine cuts to the API surface, made before the graph-validation work of
issue #3 so that it lands on a smaller and more regular API. Each one
breaks a caller that used the removed name, and each has a one-line
migration.

### Removed

- **`Binding.Named`, `Scope.Lookup`, `Key` and `di.Named`**, the one
  stringly-typed corner of the API. A second binding of one type is declared
  as a distinct type instead -- `type ReplicaDB struct{ *DB }` -- which the
  compiler checks at every reference, where a misspelt name was a missing key
  at runtime. The README described `.Named` as registering the key "also"
  under a name; it registered it under the name only.
- **`Scope.Add`.** Group membership is a property of a binding, like its
  lifetime, so it is now `Binding.Group`: `s.Add(ctor)` becomes
  `s.Provide(ctor).Group()`. `Value(v).Group()` adds a pre-built member,
  which `Add` could not express; `Bind` followed by `Group` is rejected at
  freeze, since an alias is never a group member.
- **`Scope.Middleware`** moved to the `dihttp` package as
  `dihttp.Middleware(s)`, with the usual `func(http.Handler) http.Handler`
  shape so a router's `Use` accepts it: `app.Middleware(mux)` becomes
  `dihttp.Middleware(app)(mux)`. The core package no longer imports net/http
  and no longer registers anything on a caller's behalf. `WithScope` and
  `FromContext` stay where they were.
- **`Scope.Bind`.** An interface is served by a constructor that returns the
  implementation: `s.Bind[Reader, *Repo]()` becomes
  `s.Provide(func(s *di.Scope) Reader { return s.Get[*Repo]() })`, declared
  `Scoped()` too when the target is. The closure is checked by the compiler
  where `Bind` checked `Implements` at registration, shares the target's
  instance because it returns the same pointer, and is an ordinary binding,
  so the alias machinery goes with it: the route marking for hops, the alias
  cycle detector and the eager-through-alias rules.
- **`Binding.Transient`.** A per-resolution value is a factory,
  `s.Provide(func(s *di.Scope) func() *X { ... })`, or a constructor called
  directly. Transient instances had no hooks and no tracking, which made
  them a lifetime in name only, and every teardown oracle carried an
  exemption for them.
- **`Binding.Health`, `Scope.HealthCheck`, `ErrUnhealthy` and
  `EventHealth`.** A health endpoint is a `Group` of checkers in user code;
  the README's "Health checks" section is now that recipe and
  `examples/app` uses it. What is lost is `HealthCheck` skipping services
  not yet built: `All` builds a checker's target instead, which is what an
  endpoint usually wants.
- **`Signals`.** `Run` exits on `os.Interrupt` and `SIGTERM`, which is what
  every caller used. `StopTimeout` stays as the one `RunOption`.
- **`SlogObserver`.** Four lines in user code, and the one place the package
  imported `log/slog`.

### Changed

- **`Binding.Run` is now `Binding.Worker`.** It shared a name with
  `Scope.Run`, the main-function helper, for an unrelated thing. Behaviour
  is unchanged; the error `Stop` returns for a hook that outlasts the
  deadline now reads "Worker hook did not return".

### Testing

The fourth review's findings were all in places the generators still could not
reach, so each fix comes with the shape that would have caught it:

- Drain hooks can fail now, and C11 holds a `Stop` to reporting the failure of
  a hook of its own scope. Every drain hook in the driver returned nil until
  now, so a scope that swallowed its own hook's failure looked exactly like
  one with nothing to report.
- The machines drive `Scope.Run`, with a context that is already cancelled so
  it starts and stops again. `Run` was the largest block of code only the
  hand-written tests reached, and it is where a worker's failure and a stop's
  errors are joined. Generator coverage: 82.5% to 88.5%.
- A registration shape whose constructor registers, and one that registers
  over its own key, so the registry being mutable during a resolution is
  something the generators exercise rather than something reviews find.
- `scripts/generatorgap.go` keyed coverage blocks by line number and merged
  the eighteen lines of `di.go` that carry more than one -- a hook registered
  and its body declared in a single expression -- so reaching the registration
  made the body look covered. It now keys on the full block, and CI checks its
  arithmetic against `go tool cover -func`, because a tool that measures a gap
  has to be measured itself.


## [0.6.0] - 2026-09-04

The six defects of the third September 2026 review, and then the question of
why three reviews in a row had each found about six. Measuring it gave a
number: the generators reached 78% of statements against the suite's 97%, and
the whole gap was the lifecycle -- no `Run` hook, no `Shutdown`, no context
expiring inside `Stop`, and every `Start` kept before every `Stop`. Every
defect all three reviews found lived in that gap. Most of this release is that
gap closed, in the tests and in the one place `di.go` had to change to make
closing it possible.

The public API is unchanged -- `go doc -all` is identical to 0.5.0's -- so
every change here is behaviour, and an upgrade can break a caller in the one
way listed under Changed.

### Changed

- **`Stop` is synchronous.** It now waits out whatever another goroutine is
  running for a service it is tearing down -- a start step in flight, a drain
  hook another `Stop` began -- so when it returns, the teardown has happened
  and its failures are in the error it returns rather than only in the event
  stream. A teardown outlives the call in one case now, when `Stop`'s own
  context expires first. Every review so far has reported the old asymmetry as
  a defect.

  The rule that buys it: **a lifecycle hook must not call `Stop` on its own
  scope or an ancestor** -- call `Shutdown`, which never blocks. That was
  already the documented contract, but `OnStart` was a working exception,
  since the mid-start teardown was handed to the goroutine running the step
  rather than waited for. It no longer is. Stopping a sibling scope, or one
  below the hook's own, is still allowed.

  A hook that passes on the context it was given now gets an error naming
  `Shutdown` instead of waiting: hook contexts carry the scope they belong to.
  A hook that calls `Stop` with a context of its own cannot be recognised, and
  waits until that context expires, so an unbounded one hangs where it used to
  work. This is the upgrade note: if a start hook stops its own scope, replace
  it with `Shutdown`.

### Fixed

- A drain hook may stop a scope that is neither its own nor an ancestor of it
  -- a sibling, or anything else outside its own line. The sweep used to claim
  the drain phase of every descendant before running a single hook, so such a
  `Stop` waited for a phase that only the walk it had just blocked could end:
  with a deadline it failed, with `context.Background` it hung. The sweep now
  claims a scope's phase immediately before it sweeps that scope, so the only
  phases held while a hook runs belong to the hook's own scope and its
  ancestors, which a hook may not stop anyway. Whether the old code deadlocked
  depended on the order the two scopes were created in.
- A `Stop` whose context runs out while another `Stop`'s `OnDrain` still holds
  an instance no longer drops that instance's `OnStop`. It had already taken
  the instance off its scope's list, so nothing else would ever reach it and
  the service was never released -- a second `Stop` replayed the stored error
  and released nothing either. The missed deadline is still reported, and the
  release now follows the drain hook's own return, exactly as it already did
  for a `Run` hook that outlasts the same deadline.
- `Scope.Run` reports a failure published through `Shutdown` while the stop was
  already running. `Run` read the cause once, before `Stop`, so a worker in a
  child scope that died after a cancelled context -- with its own scope stopped
  and detached by a hook that handled that error itself -- returned nothing to
  the caller. `Run` now re-reads the cause on the way out, and the existing
  de-duplication keeps a failure that arrived by both routes from being
  reported twice.
- A resolution made through the `*Scope` a finished constructor kept is no
  longer a false `ErrCycle` when a constructor *above* it is still building.
  0.5.0 stopped counting the finished frame itself; the frames above it were
  still counted, so `A` building, `B` returning and keeping its scope, and an
  independent resolution through that scope needing `A` was reported as a
  cycle -- and the verdict was cached, so the service stayed poisoned after
  `A` had long succeeded. A finished frame now ends the walk in both the path
  check and the wait-for graph.

  This is a trade, not a free fix, and it is the same one `Stop` makes for a
  start step in flight: without goroutine-local state there is no way to tell
  an independent late resolution from the constructor's own goroutine reaching
  back through an escaped scope into its own unfinished construction. The
  second now deadlocks where it used to be reported. It takes a service
  reaching into itself through a scope that escaped a nested constructor; the
  first is the documented pattern.
- A child scope created *inside* a constructor keeps that constructor's
  resolution, so a request through it that leads back to the service being
  built is reported as `ErrCycle`. `Child` used to hand back a scope with no
  path at all, which started a fresh resolution that then waited for the build
  it was part of: neither the path check nor the wait-for graph could connect
  the two halves. A child kept for later is unaffected -- its path is already
  finished, so it resolves as its own branch, and failures from it panic with
  a plain error as any top-level call does.
- A `Transient` constructor that finishes after its scope has stopped reports
  `ErrStopped` instead of handing the value back. Every other lifetime was
  re-checked after its wait; the transient branch returned without one, so a
  scope that had finished stopping could still serve a value it had no way to
  tear down.

### Testing

The gap above, closed in three pieces:

- The concurrent driver has all four of those now, and two new oracles: every
  instance that owes a stop step gets exactly one by quiescence, and a
  resolution begun after its scope's `Stop` returned fails. Generator coverage
  is 82.5%, and `scripts/generatorgap.go` prints what only the hand-written
  tests reach, which is the map of where the next review will dig. CI fails
  below 80%.
- The instance lifecycle has a model (`lifecyclemodel_test.go`). Registration
  is still checked against invariants rather than predictions, for the reason
  the tests have always given; what happens to an instance once it exists is a
  small documented state machine, and predicting it is what catches a hook
  that should have run and did not.
- The interleaving is an input (`scheduler_test.go`): hooks and operations
  park at scheduling points and a seed decides who goes next.

Each of these was mutation-tested rather than trusted. That is also how the
one defect in the *oracles* turned up: an exemption written for the ordering
oracle had switched off the drain/stop overlap check for exactly the shape it
exists for.

## [0.5.0] - 2026-09-04

Fixes for the six defects of the second September 2026 review, the `Run`-hook
overlap reported alongside them, two more that the tightened concurrent driver
found in those fixes, and the half of the first review's ninth defect that its
fix had left open. Almost all of it is the teardown path: draining, and how a
failure gets out of a worker.

The public API is unchanged -- `go doc -all` differs from 0.4.0 only in one
parameter name -- so every change is behaviour, and an upgrade can break a
caller in the two ways listed under Changed.

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
- A `Run` hook's failure always reaches `Shutdown`. Whether it did used to
  depend on a race: the worker goroutine asked the run context whether *we*
  had cancelled it, and a `Stop` landing between two readings of that context
  flipped the verdict, so a worker that died of its own error could be written
  off as one we had stopped. Its failure then reached only the `Stop` that
  cancelled it, and a caller who discarded that -- or a child scope that had
  already detached -- left `Scope.Run` waiting for a signal that never came.
  This is the half of the September 2026 review's ninth defect that its fix
  left open, and it is why
  `TestReviewDetachedChildWorkerFailureReachesRun` could fail on timing alone.

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

- A `Run` hook that returns a non-nil error now calls `Shutdown` even when the
  scope was already stopping. Only `context.Canceled` from a hook we had
  cancelled is still treated as no failure at all. When an error surfaces says
  nothing about what caused it -- a worker may fail, flush what it has while
  the scope winds down, and only then report -- so the timing test that used to
  gate this was unsound in both directions. A caller who does not want a
  worker's shutdown-time error to take the application down should return nil
  from the hook: previously that error was silently confined to whichever
  `Stop` cancelled it.
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

- A `Run` hook's failure always reaches `Shutdown`. Whether it did used to
  depend on a race: the worker goroutine asked the run context whether *we*
  had cancelled it, and a `Stop` landing between two readings of that context
  flipped the verdict, so a worker that died of its own error could be written
  off as one we had stopped. Its failure then reached only the `Stop` that
  cancelled it, and a caller who discarded that -- or a child scope that had
  already detached -- left `Scope.Run` waiting for a signal that never came.
  This is the half of the September 2026 review's ninth defect that its fix
  left open, and it is why
  `TestReviewDetachedChildWorkerFailureReachesRun` could fail on timing alone.

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

- A `Run` hook's failure always reaches `Shutdown`. Whether it did used to
  depend on a race: the worker goroutine asked the run context whether *we*
  had cancelled it, and a `Stop` landing between two readings of that context
  flipped the verdict, so a worker that died of its own error could be written
  off as one we had stopped. Its failure then reached only the `Stop` that
  cancelled it, and a caller who discarded that -- or a child scope that had
  already detached -- left `Scope.Run` waiting for a signal that never came.
  This is the half of the September 2026 review's ninth defect that its fix
  left open, and it is why
  `TestReviewDetachedChildWorkerFailureReachesRun` could fail on timing alone.

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

- A `Run` hook's failure always reaches `Shutdown`. Whether it did used to
  depend on a race: the worker goroutine asked the run context whether *we*
  had cancelled it, and a `Stop` landing between two readings of that context
  flipped the verdict, so a worker that died of its own error could be written
  off as one we had stopped. Its failure then reached only the `Stop` that
  cancelled it, and a caller who discarded that -- or a child scope that had
  already detached -- left `Scope.Run` waiting for a signal that never came.
  This is the half of the September 2026 review's ninth defect that its fix
  left open, and it is why
  `TestReviewDetachedChildWorkerFailureReachesRun` could fail on timing alone.

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

[Unreleased]: https://github.com/floatdrop/di/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/floatdrop/di/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/floatdrop/di/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/floatdrop/di/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/floatdrop/di/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/floatdrop/di/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/floatdrop/di/releases/tag/v0.1.0
