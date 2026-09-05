# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

`github.com/floatdrop/di` is a dependency-injection container for Go 1.27+ built
on generic methods. The whole library is `di.go`, plus the net/http adapter
in `dihttp/`; everything else is tests, examples, and a separate benchmarks
module.

## Commands

```sh
go test -race -count=1 ./...                  # the suite; always run with -race
go test -race -run '^TestRegressionStartRace$' .   # one test
go test -count=1 -run TestMachineSeeded .     # the seeded operation sweep
go test -run '^$' -fuzz FuzzMachine -fuzztime 2m . # coverage-guided fuzzing
go vet ./... && golangci-lint run ./...       # lint (config in .golangci.yml)
test -z "$(gofmt -l .)"                       # formatting gate
go run github.com/campoy/embedmd@v1.0.0 -d README.md   # README in sync?
go run github.com/campoy/embedmd@v1.0.0 -w README.md   # re-embed after editing examples/
cd benchmarks && go test -bench . -benchmem   # separate module, see below

go test -count=1 -run 'TestMachine|TestConcurrent|TestProperty|FuzzMachine' -coverprofile=gen.out .
go test -count=1 -coverprofile=all.out .
go run scripts/generatorgap.go -floor 85 gen.out all.out   # what only hand-written tests reach
```

Run the full gate as a single `&&` chain before committing, the same way CI
does, so a failing step cannot let a commit through:

```sh
test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./... \
  && golangci-lint run ./... \
  && go run github.com/campoy/embedmd@v1.0.0 -d README.md
```

## Architecture

Reading `di.go` top to bottom does not reveal the model; these are the pieces
that only make sense together.

**Three levels of state.** A `binding` is a registration: its key, lifetime,
hooks, and `build` func. An `instance` is one built value of a binding. A
`state` is a scope's registry and lifecycle bookkeeping. `Scope` is a thin
handle over `*state` plus a `*resolver` carrying the current resolution path;
the `Scope` handed to a constructor is a *view* over the same state with that
path attached, which is how cycle detection and error paths work.

**Which state owns an instance.** A singleton lives in the scope that
registered the binding (`owner`); a `Scoped` instance lives in the scope that
resolves it (`holder`), so it can see that scope's values. `resolve` picks the holder
and the rest of the pipeline works in terms of it.

**The instance phase machine** (`phaseNew` → `Building` → `Built` →
`Starting` → `Started`/`Failed` → `Stopped`) is read and written *only* under
the owning state's mutex. This exists because splitting a start-or-stop
decision across two critical sections produced several bugs. In particular:
`publish` appends to the stop list and *then* `startIfRunning` reads
`running`, while `Start` sets `running` before it drains. That ordering is
what guarantees an instance is started by exactly one path.

`await` is the only way to reach an instance. It claims the build step or waits
for whoever did, and it also waits out `phaseStarting`, which is what stops a
resolution handing back a service whose `OnStart` is still running. `settled`
says the build step is over and `value`/`err` are final; it replaced a
`sync.Once`, which could only tell a second resolution to carry on, never to
wait.

**A waiter blocks on one step, not on the scope.** Each step another goroutine
can be responsible for finishing has a channel closed when it is done:
`settledCh`, `startingCh`, `drainedCh`. One rule covers all three -- the first
goroutine that actually has to wait makes the channel (`waitOn`), and the owner
of the step closes it only if it is there (`wake`). The phase still says
*which* step is outstanding, and phase and channel are read in one critical
section, so a waiter cannot pick up the channel from a later step.

Both halves run under the owning state's mutex, which is what makes the pair
safe in either order: a waiter that got there first is released by the close;
an owner that got there first leaves nil behind, and the phase the waiter then
reads already says the step is done, so it never blocks on a wakeup that has
been and gone. Nil is the ordinary state, not an edge case -- an uncontended
build, an unraced start step and an undisputed drain each allocate nothing.

This replaced a `sync.Cond` per scope, where every phase change woke every
waiter in the scope to re-check a predicate that was almost never theirs, and
where bounding a wait by a context needed `awaitPhase` to fake it with a
`context.AfterFunc` that broadcast on expiry. The context-bounded wait in
`drainIfNeeded` is now an ordinary `select`.

The one broadcast with no channel replacing it is the one `teardown` did after
`stopped.Store(true)`. Nothing waits on a predicate that mentions `stopped` --
`await` blocks only on `settled` and `phaseStarting`, and both are advanced by
goroutines a `Stop` does not interrupt -- so it released nobody who was not
about to be released anyway. If a resolution is ever found hanging across a
`Stop`, this is the first thing to suspect.

**Two cycle detectors, because one branch cannot see the other.** Within a
branch, `resolver.onPath` walks the immutable path. Across branches,
`resolver.wait` searches a `*graph` before blocking: instances point at the
resolution building them, blocked resolutions point at what they wait for, and
a branch that blocks does so several nodes below the one holding the build, so
both directions are matched against whole paths (`descends`). The check and the
edge it adds are one critical section, or two branches closing a cycle at once
would both decide to wait. Lock order is state mutex then `graph.mu`, never the
reverse.

There is one graph per container, made by `New` and handed down through
`newState`, so `state.graph` is a field read rather than a walk to the root.
That is exactly the reach a cycle has: a wait crosses scopes, because a
resolution follows the parent chain, but nothing joins two containers. The
graph used to be a package-level map and mutex, which found the same cycles and
made every blocked resolution in the process scan every other container's edges
under one lock to do it.

**The resolution path is immutable, and finished nodes stop counting.**
`resolver` is a linked list node, not a slice, because a constructor may
resolve from several goroutines and they all share the `*Scope` it was handed.
A node is identified by binding *and* holder, never by key: that is what
separates a group member from a plain registration of the same type, and one
`Scoped` binding across scopes. It does not carry a key at all -- `pathStr`
reads `b.key` -- because the only thing that ever put a node on the path whose
key was not its binding's was a `Bind` alias hop.

`resolver.done` is the one thing about a node that changes: `resolve` sets it
as it returns, and `onPath` and `descends` *stop the walk* at a node that has
it. The path stays whole for error messages; what goes away is the claim that
the node, or anything above it, is still a dependency. This exists because a
constructor may keep the `*Scope` it was handed -- that is how a goroutine it
starts resolves later -- and a resolution made through it afterwards would
otherwise meet its own finished frame, be called a cycle, and have that
verdict cached on whatever instance it was building.

Stopping rather than skipping is the second half of that, and it matters
because the frames *above* the finished one are usually still building: A
resolves B, B keeps its scope and returns, A carries on, and a later
resolution through B's scope that needs A met an active A and was called a
cycle even though it had only to wait. A finished node breaks the chain in
both directions -- nothing above it is waiting on what is opened below it
afterwards -- so both edges of the wait-for graph read it the same way. The
price is the one case that cannot be told apart without goroutine-local state,
exactly as with `Stop`'s mid-start handoff: a constructor that blocks on a
resolution made through a finished descendant's scope that leads back to
itself now deadlocks where it used to be reported. It takes a service reaching
back into its own unfinished construction through an escaped scope; the late
resolution the change admits is the documented one.

`Scope.Child` carries the resolver of the scope it is made from, so a child
opened *inside* a constructor is part of that resolution and a cycle through
it is reported instead of deadlocking. Nothing else needs to change for a
child kept for later, because the node it carries is `done` by then and the
rule above makes the path inert. `inFlight` is that question -- a path whose
last node has not returned -- and it is also what decides whether `Get`,
`All` and `Must` convert an `abort` into the plain error panic a top-level
call gets: a scope kept past its resolution has no enclosing call to unwind
to, so it is a top-level caller that happens to know where it came from.

**Scopes have a stop machine too.** `state.stopOnce` is claimed by the first
`Stop` and settled when its teardown finishes; every later or concurrent `Stop`
waits on it and reports its error. That wait is what keeps dependency order
when a child and its parent are stopped at once. The cost is that a hook may
not call `Stop` on its own scope or an ancestor.

The `once` type is that pattern by itself -- claim, settle, ctx-bounded wait --
because the scope has two of them and they were previously two hand-written
copies that disagreed in ways only a comment recorded. Its fields are guarded
by the state mutex rather than one of its own, so claiming a phase and
recording what the claim decided (`stopCtx`, for `Stop`) stay one critical
section and no third lock joins the ordering rules. The waiter's contract is
the one thing the two callers still differ on, and it is now visible at the
call site: both report the owner's error. The scope-wide drain used to drop
it, on the reasoning that a drain's failures reach the caller through the
`Stop` that owns them -- true when that is the same call, and false for a
request scope ending while the application shuts down, which is where the
failure needed reporting. The fourth review found it.

**Draining precedes everything, and it has a machine of its own.** `Stop` is
drain, then mark stopped, then children, then this scope's instances. Both
levels of the drain phase are once-with-wait, mirroring the stop phase:
`state.drainOnce` for the scope, `instance.dr` (`drainNone` → `draining` →
`drained`) for one hook. A second `Stop` arriving at either waits instead of
skipping. An earlier version recorded only that a drain had been *decided*,
which let a concurrent `Stop` see the flag, walk past a hook still running and
begin releasing what it was using; and it skipped a child whose `Stop` had
begun, which let this scope mark itself stopped while that child's hooks were
still resolving through it.

`drainRun.sweepAll` sweeps repeatedly rather than once, and `drainRun.visit`
sweeps *every* scope the phase owns on every pass. Draining is the only
teardown phase during which the scope still resolves, so a hook finishing
in-flight work may build a service or open a child scope for the first time;
those owe a drain too, and it has to happen before `stopped` is set or their
own hooks find nothing to resolve. Visiting each descendant once was enough
for a hook that builds into its own scope and not for one that builds a level
along — the same defect, found by the drain oracle after the review that fixed
the first half. A pass that does no work ends the phase, and `ctx` bounds the
sweep as well as the hooks.

The sweep is a post-order walk that claims a descendant's phase immediately
before descending into it, not a discovery pass that claims the whole subtree
and a sweep that follows. That ordering is the invariant: while a hook runs,
the only phases the run holds unended are the scope being swept and its
ancestors, which is exactly the set a hook may not call `Stop` on anyway.
Claiming ahead is tidier and it deadlocks a hook that stops a scope the walk
has taken but not yet reached — a server draining in one child and stopping a
request scope in another waits for a phase only its own blocked walk can end.
That is the trap of the paragraph below, one branch along rather than one
level up. A scope another `Stop` already owns is waited for and then left
alone, subtree included: that `Stop`'s run drains it.

A descendant's phase ends when its own sweep does, not when the run does.
Holding it open to the end is tidier, since an ancestor's hook can still build
into it, and it deadlocks the case the phase exists for: an HTTP server
draining in an outer scope waits for a handler, and that handler is stopping
its request scope, which would then be waiting for the run. That is why the
scope-level guard is not enough on its own and `stopIfNeeded` waits out
`draining` per instance: an instance built after its scope's phase ended can
be drained by a sweep still running above it exactly as its own `Stop`
arrives, and only the per-instance wait keeps the release off a value the hook
still holds. `drainIfNeeded` also skips an instance whose scope is already
stopped, because winding something down for work it can no longer take on is
the opposite of what the hook is for.

Three windows stay open, and each is cheaper to accept than to close: an
instance published between the last sweep and `stopped.Store(true)` is not
drained, and closing that would mean holding the state mutex across user
hooks; the same for one built into a scope whose phase another `Stop` already
ended; and a hook running on such a late instance can find its scope stopped
mid-hook, because the scope became stopped after the decision to drain it.
That last one is why the concurrent driver exercises resolution inside drain
hooks but does not assert that it succeeds.

**Who hears a drain failure is decided by who owns the teardown, not by who
ran the hook.** A sweep settles a descendant's failures into that
descendant's phase, so the descendant's own `Stop` reports them, and an
ancestor inherits them by stopping that descendant and joining what its
`Stop` returns. When a second `Stop` of that descendant is already in flight,
that call owns the teardown, hears the failure, and detaches the scope as it
finishes -- so whether the ancestor also hears it depends on whether the
detach beats the ancestor's read of its child list. Both orders are correct,
because the failure always reaches the caller that owned the teardown, and
`EventDrain` carries it to observers either way. Do not write an ordering
oracle, or a test, that requires the ancestor to hear it: one did, and failed
about one run in eight. `TestReview4ChildStopReportsItsOwnDrainFailure` is
that test with the assertion narrowed, and
`TestReview5RootStopReportsAChildsDrainFailure` pins the half that is fixed --
an ancestor that does own the teardown.

Taking the child list before the drain as well as after it would make the
ancestor hear it always, and it is the wrong trade: the ancestor would then
also inherit a deadline the *other* caller set, so a `Stop` that waited
properly and released everything would report `context.DeadlineExceeded`.
`TestReview3LostDrainWaitStillReleases` and
`TestConcurrentImpatientStopStillReleases` both fail that way; they are the
guard on this paragraph.

Both levels now wait out `phaseStarting` rather than stepping around it,
which is what the no-`Stop`-from-a-hook rule buys: the goroutine running that
start step can no longer be this one. `drainIfNeeded` waits for it because a
service that is starting owes a drain as soon as it has started, and leaving
it undecided for a later pass meant a start step that outlasted the phase was
never drained at all.

**`freeze` is transactional.** Registrations queue in `pending` and commit in
one batch. The batch is validated against *prospective copies* of
`index`/`groups`/`all`, so a rejected registration leaves the scope untouched
and keeps being rejected identically. Do not move a mutation of the real maps
before validation.

**Two levels of registration semantics.** Lifetime and hooks belong to one
registration, because they are typed on that value. Eagerness belongs to the
*key*: it means the service exists by the time `Start` returns, so it transfers
to whichever binding owns the key. `deriveEager` is the single place that
decides what `Eager` means, and it validates in the same loop so the derived
set and its rules cannot drift apart.

**A key is served to a whole route, not just to its destination.**
`binding.used` protects the owner; `markServed` records the key in every scope
between the resolver and that owner. Both halves matter: an earlier version
marked only the endpoint, and a scope in the middle could then shadow a key
it had already handed out. `Bind` aliases used to add hops to the route and
a cycle detector of their own; an interface is now served by a constructor
returning the implementation, which is an ordinary binding.

**A stopped scope refuses to serve, and that is checked twice.** `resolve`
checks on the way in, and `await` checks again after the wait, because the
scope can stop while a resolution is parked on someone else's build. The
second check is on the *resolving* scope, not the instance's holder: the
holder is always that scope or an ancestor, so checking the resolver covers
both, and a stopped child must refuse the request whether or not what it asked
for is still alive above it.

**Errors versus panics.** A *wiring* failure (missing dependency, cycle, failed
constructor) is an internal `abort{err}` panic that unwinds to the nearest
`Resolve`/`Start`/`Run` and becomes an `error`. A *configuration* rejection
(contradictory lifetimes, re-registering a resolved key) is a plain `panic`
with a string prefixed `di: `. So `Resolve` never panics, `Get` panics with an
`error` at top level, and config errors panic with a string. Tests and the fuzz
harness classify panics by that rule.

**When a stop is owed.** `OnStop` runs when `OnStart` succeeded, or when there
is no `OnStart` to pair with (or the scope was never started), making `OnStop` a
plain destructor. A service built but never started is *not* torn down. Only a
start hook that *returned* counts as succeeded: `startClaimed` recovers a
panicking hook into a failed start, or the instance would sit at
`phaseStarted`, be served to a caller that recovered the panic, and be paired
with an `OnStop` for an `OnStart` that never finished.
`binding.used` is set only when a resolution actually served a value, and
`state.served` records keys a scope resolved from an *outer* scope, because
`used` lives on the outer binding and cannot protect the inner scope.

## Invariants that are easy to break

- Never set a phase or read `running`/`stopped` for a decision outside the
  owning state's mutex.
- **`Stop` is synchronous, and that rests on one rule: no hook may call `Stop`
  on its own scope or an ancestor.** `stopIfNeeded` waits out every step
  another goroutine owns for the instance -- `phaseStarting`, then `draining`
  -- so a teardown outlives `Stop` only when `ctx` expired. The old asymmetry
  (a mid-start teardown handed off via `stopWanted`) existed because a start
  hook was allowed to call `Stop` and Go cannot tell that goroutine from any
  other; every review reported the asymmetry as a bug. The rule is now the
  documented one, and `Stop` reports the misuse instead of waiting whenever it
  can see it: hook contexts carry their scope (`inHook`/`hookOwner`), so a
  hook that passes on the context it was given gets an error naming
  `Shutdown`. A hook that passes a context of its own is invisible and waits,
  which is why the fallback still has to be a bounded wait rather than a
  promise.
- Nothing in the teardown path may run a user hook against a value another
  hook still holds. That is one rule with three instances: `OnStop` after
  `OnDrain`, `OnStop` after a `Worker` hook (deferred to `releaseAfterWorker` when
  `ctx` expires rather than run alongside it), and a parent's hooks after a
  child's.
- `Start`'s rollback goes through `Stop` with `context.WithoutCancel`, so it
  stops child scopes and waits for `Worker` hooks.
- Whichever `Stop` call queues a handoff owns that teardown's context; a later
  `Stop` must not clobber it.

## Testing strategy

Four layers, each catching a different class:

- The regression files — one test per historical defect, grouped by the part
  of the library the defect lived in rather than by the review that found it:
  `cycles_test.go` (the resolution path), `wiring_test.go` (registration and
  lookup), `teardown_test.go` (start, stop, rollback), `drain_test.go` and
  `worker_test.go` (`Worker` hooks and `Shutdown`), with the stand-in types they
  share in `fixtures_test.go`. A new one goes wherever its rule lives.

  Provenance moved into a tag on each test -- `(review 2, 5)`, `(pass 4)` --
  because grouping by it put three files between two tests of the same
  machine. Each file's header lists the tags and the commit each review was
  checked against. **Verify a new test fails against the commit that preceded
  the fix**, e.g. by restoring the old `di.go` from git and running just that
  test, and tag it. Several tests here turned out to pass both before and
  after; say so rather than implying coverage.
- `property_test.go` — random *registration* sequences checked against a model
  of the eager rules. A predictive model can be wrong in the same way as
  the code, so treat it as needing its own scrutiny.
- `machine_test.go` — random *operation* sequences (register, resolve, start,
  stop, shutdown) across a root, two children and a grandchild,
  checked against invariants taken from documented guarantees rather than
  predicted values. This is the layer that catches error-path and cross-scope
  bugs. I4 has no exemptions now that every lifetime is tracked. The old
  exemption for aliased keys is what once hid a scope handing out two live
  values for one interface, so do not reintroduce one lightly.
- `lifecyclemodel_test.go` — the one place that *does* predict, because the
  argument against predicting does not hold for it. What serves a key depends
  on overrides and the eager rules, and modelling that would be
  modelling the code twice; what happens to an instance *once it exists* is a
  small state machine the package documents completely, and it is the half
  every review found defects in. So the model takes builds as given -- the
  constructors report themselves -- and predicts the rest: which hooks are
  owed, in what order, exactly once (M1-M6). It is what lets the sequential
  layer check the thing the concurrent driver says it cannot: that an instance
  owing a drain gets one.

  Two facts are observed rather than predicted, and both are marked in the
  file. Whether a start step succeeded, because a rollback stops what had
  started at the moment it failed. And whether `Start` was ever called on a
  scope, read back through `Scope.Context`, because whether a rejected `Start`
  had already recorded its context depends on which panic came first, which
  is not a documented guarantee.

  Mutation-tested, and the result is worth keeping in mind: stopping in build
  order instead of reverse is caught by the fuzzer in 0.06s and *not* by the
  400 seeded sequences. The seeded sweep is thinner than the accumulated fuzz
  corpus; when a model check finds nothing, run the fuzzer before believing
  it.
- `concurrent_test.go` — the same operations run in parallel lanes under
  `-race`, in two phases (wire, then everything else). It checks only what
  survives concurrency: nothing panics unexpectedly, every operation returns,
  nothing is stopped more often than built, and the stop-order oracle, which
  is the layer that catches a parent running ahead of a child. Five more
  oracles cover the drain phase and the classes C1 cannot see: no stop hook of
  an instance begins inside or before that instance's drain hook (C6), a drain
  hook can still resolve (C7), one fixed graph gives one verdict, so two
  resolutions of a key never disagree about a cycle (C8), every instance that
  owes a stop step gets exactly one by quiescence (C9), and a resolution
  *begun* after its scope's Stop returned fails (C10).

  C9 is the one that needed a definition of quiescence, since a release
  deferred past a missed deadline lands after every Stop has returned:
  `settle` polls until no hook is running and nothing owed is unreleased.
  Starts and stops share one phase now that `Stop` waits for a start step;
  keeping them apart was a workaround for the handoff.

  `scheduler_test.go` makes the interleaving an input. Every hook and every
  operation parks at a scheduling point, and a seed decides which parked
  goroutine goes next, so `TestMachineScheduled` replays one sequence under
  many orderings with every oracle live. It explores rather than verifies: a
  released goroutine can block inside the container where no test can see it,
  and the next release then happens on a timer, so two runs of a seed can
  still differ. The loop waits ~200µs for goroutines to gather before it
  chooses, because releasing each one as it arrives leaves nothing to decide
  and the seed decides nothing -- that single change took a sample sequence
  from two distinct schedules across twelve seeds to six.

  `TestMachineConcurrentShapes` builds op sequences directly rather than from
  bytes. A byte seed has to survive four modulos to reach a particular
  interleaving, and the three shapes there -- an impatient `Stop` under an
  ancestor's drain, the same a level deeper, and an impatient `Stop` of a
  running worker -- are what the coverage gap said no random sequence was
  reaching. Both were checked by mutation: delete either deferred release in
  `di.go` and C9 fails.
- The three September 2026 reviews are the source of most of those tests: the
  first found eleven defects plus a gap it did not count, the second six plus
  the `Worker`-hook overlap and then two the tightened driver found on its own,
  and the third six that were all cross-phase or cross-branch -- a drain hook
  stopping a sibling scope, a release dropped with a missed deadline, a
  shutdown cause published after `Run` had read it, a false cycle through a
  finished frame, a `Transient` skipping the stopped check, and a child scope
  made in a constructor starting a fresh path. No generator reaches any of
  the third review's six, which is the same lesson as the second: they need
  shapes the driver does not build.

  The exemptions these oracles need are the most dangerous part of them. C3
  cannot order a release that a missed deadline deferred, so it is switched
  off for scopes where a `Stop` reported one -- and switching C6 off with it,
  which looked like the same exemption, silently disabled the drain/stop
  overlap check for the one shape that needs it. C6 holds however impatient
  the `Stop` was: a missed deadline defers a release, it never runs one early.
  Mutation-test an exemption before believing it.

  Every defect in both reviews lived in a gap the generators could not see,
  and closing those gaps found a seventh. Two things had been missing, both
  now fixed: C1 accepted *any* `panic(error)` as legitimate, because that is
  how `Get` reports failure, so a false `ErrCycle` read as normal; and the
  drain hooks returned nil and touched nothing, so the phase was exercised
  without being checked.

  The lesson is about *shapes*, not oracles. Adding C6 changed nothing until
  the driver gained a registration that is `Scoped` **and** draining: every
  other draining shape is a plain singleton, so resolving one from a child
  hands back the instance the owner already holds, and a drain-owing instance
  could never appear in a child scope at all. One missing registration made a
  whole class of defect unreachable. When an oracle finds nothing, suspect the
  generator before believing the code.
- `FuzzMachine` — the same invariants under coverage-guided search. Corpus in
  `testdata/fuzz/` is committed; CI runs 90s in its own job.
- `scripts/generatorgap.go` — the map of what only the hand-written tests
  reach, which is the map of where the next review will dig: every defect the
  four September 2026 reviews found lived on such a line. CI runs it with a
  floor of 85% generator coverage. When the floor moves, move it up.

  CI also checks the script's arithmetic against `go tool cover -func`,
  because the fourth review found the script wrong: it keyed coverage blocks
  by line number, and `di.go` has eighteen lines carrying more than one block
  -- a hook registered and its body declared in a single expression. Reaching
  the registration made the body look covered. A tool that measures a gap has
  to be measured itself.

`FuzzMachineConcurrent` is the coverage-guided driver for the concurrent
oracles; run it with `-race` or it checks almost nothing.

The sequential generators do not explore goroutine interleavings. That is what
`concurrent_test.go` and the stress loops under `-race` are for (see
`TestRegressionStartRace`).

## Repo conventions

- **README code blocks are generated.** They are embedded from `examples/` with
  embedmd markers. Run `gofmt -w` on an example *before* re-embedding, or CI
  fails on the sync check.
- **`benchmarks/` is a separate module** with a `replace ../` directive, so the
  library itself stays dependency-free. `samber/do` is a dependency there only.
- **`examples/app` and `examples/server` block on signals.** To exercise them,
  build and run with output going to the terminal, not redirected to a file —
  this harness loses a backgrounded server's startup output when redirected,
  which once produced a false failure report.
- **A teardown finishes after `Stop` returns only when `Stop`'s context
  expired** -- waiting for a `Worker` hook, a start step or a drain hook -- plus
  the one that undoes a build completing after the scope stopped, which no
  `Stop` issued. The deadline bounds how long `Stop` waits, never whether the
  release is owed, and `Stop` has already taken the instance off its scope's
  list, so nothing else will reach it: the handoff goroutine re-enters
  `stopIfNeeded` with `context.WithoutCancel`, whose `Done` is nil, so it
  waits properly and cannot recurse again. Any ordering oracle has to model them
  or it will report them as defects; the concurrent driver does it by running
  every `Start` before any `Stop`, so no start step is ever in flight.
- **CHANGELOG is enforced.** `.github/workflows/release.yml` fails a tag push
  when `CHANGELOG.md` has no `## [<version>]` section. The public API has been
  stable across tags; verify with `go doc -all` diffed between tags before
  choosing a version number.

## Tooling caveat

Generic methods need gopls **v0.23.0 or newer**. v0.21.1 rejects the code with
`method must have no type parameters` and then reports cascading phantom type
errors across the examples. golangci-lint v2.13.1+ handles them correctly.
`.golangci.yml` excludes staticcheck QF1011 because `var get func() *DB = s.Get`
is not redundant: the declared type is what drives Go 1.27 inference for a
generic method value.

golangci-lint v2.13.1's staticcheck (honnef.co/go/tools v0.8.0) *crashes*
rather than reports on this package: `SA4023: index out of range [1] with
length 1`, inside its nilness analysis. It takes the whole lint job down, so
there is no partial result to work from, and the trigger moves as the test
package grows -- it first appeared on a helper comparing an error parameter to
nil in an `||`, and came back later on an unrelated switch case. `.golangci.yml`
disables SA4023 for that reason, with the default check list otherwise intact.
Drop the exclusion when the upstream crash is fixed and see whether SA4023 has
anything to say.

Bisecting a lint crash needs care: reverting one file to find the trigger can
break the build, and golangci-lint then reports "0 issues" for a package it
never analysed. Check that the package still compiles at each step.
