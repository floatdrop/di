# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

`github.com/floatdrop/di` is a dependency-injection container for Go 1.27+ built
on generic methods. [`docs/DESIGN.md`](docs/DESIGN.md) explains resolution,
lifetimes, phases, cycles and teardown with diagrams; this file is the working
detail behind it, and the two are edited together. The library is `di.go`, the
rendering of the recorded graph in `explain.go`, the check of the declared
graph in `validate.go`, the net/http adapter in `dihttp/` and the slog bridge
for `Observe` in `dislog/`; everything else is tests and two separate modules,
`examples/` and `benchmarks/`.

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

cd site && npm ci && npm run check && npm run build   # the guide site; BASE_PATH=/di for Pages
cd examples && go test ./...                  # separate module, charmbracelet/log lives there
cd examples && go test ./guide -update        # rewrite testdata/ after rewiring the guide app

go test -count=1 -run 'TestMachine|TestConcurrent|TestProperty|FuzzMachine' -coverprofile=gen.out .
go test -count=1 -coverprofile=all.out .
go run scripts/generatorgap.go -floor 90 gen.out all.out   # what only hand-written tests reach
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
resolves it (`holder`), so it can see that scope's values. `resolve` picks the
holder and the rest of the pipeline works in terms of it.

**The instance phase machine** (`phaseNew` → `Building` → `Built` →
`Starting` → `Started`/`Failed` → `Stopped`) is read and written *only* under
the owning state's mutex: splitting a start-or-stop decision across two
critical sections produced several bugs. In particular `publish` appends to
the stop list and *then* `startIfRunning` reads `running`, while `Start` sets
`running` before it drains. That ordering is what guarantees an instance is
started by exactly one path.

`await` is the only way to reach an instance. It claims the build step or
waits for whoever did, and it waits out `phaseStarting`, which is what stops a
resolution handing back a service whose `OnStart` is still running. `settled`
says the build step is over and `value`/`err` are final.

**A waiter blocks on one step, not on the scope.** Each step another goroutine
can be responsible for finishing has a channel closed when it is done:
`settledCh`, `startingCh`, `drainedCh`. One rule covers all three -- the first
goroutine that actually has to wait makes the channel (`waitOn`), and the owner
of the step closes it only if it is there (`wake`). The phase says *which* step
is outstanding, and phase and channel are read in one critical section, so a
waiter cannot pick up the channel from a later step.

Both halves run under the owning state's mutex, which is what makes the pair
safe in either order: a waiter that got there first is released by the close;
an owner that got there first leaves nil behind, and the phase the waiter then
reads already says the step is done. Nil is the ordinary state, not an edge
case -- an uncontended build, an unraced start step and an undisputed drain
each allocate nothing.

Nothing waits on a predicate that mentions `stopped`, so `teardown` closes no
channel after `stopped.Store(true)`. If a resolution is ever found hanging
across a `Stop`, that is the first thing to suspect.

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
resolution follows the parent chain, but nothing joins two containers.

**The resolution path is immutable, and finished nodes stop counting.**
`resolver` is a linked list node, not a slice, because a constructor may
resolve from several goroutines and they all share the `*Scope` it was handed.
A node is identified by binding *and* holder, never by key: that is what
separates a group member from a plain registration of the same type, and one
`Scoped` binding across scopes. It carries no key at all -- `pathStr` reads
`b.key`.

`resolver.done` is the one thing about a node that changes: `resolve` sets it
as it returns, and `onPath` and `descends` *stop the walk* at a node that has
it. The path stays whole for error messages; what goes away is the claim that
the node, or anything above it, is still a dependency. A constructor may keep
the `*Scope` it was handed -- that is how a goroutine it starts resolves later
-- and a resolution made through it afterwards would otherwise meet its own
finished frame, be called a cycle, and have that verdict cached on whatever
instance it was building.

Stopping rather than skipping is the second half, because the frames *above*
the finished one are usually still building: A resolves B, B keeps its scope
and returns, A carries on, and a later resolution through B's scope that needs
A met an active A and was called a cycle when it had only to wait. A finished
node breaks the chain in both directions, so both edges of the wait-for graph
read it the same way. The price is the one case that cannot be told apart
without goroutine-local state: a constructor that blocks on a resolution made
through a finished descendant's scope that leads back to itself now deadlocks
where it used to be reported. That takes a service reaching back into its own
unfinished construction through an escaped scope; the late resolution the
change admits is the documented one.

`Scope.Child` carries the resolver of the scope it is made from, so a child
opened *inside* a constructor is part of that resolution and a cycle through it
is reported instead of deadlocking. A child kept for later needs nothing more,
because its node is `done` by then. `inFlight` is that question -- a path whose
last node has not returned -- and it also decides whether `Get`, `All` and
`Must` convert an `abort` into the plain error panic a top-level call gets: a
scope kept past its resolution has no enclosing call to unwind to.

**Scopes have a stop machine too.** `state.stopOnce` is claimed by the first
`Stop` and settled when its teardown finishes; every later or concurrent `Stop`
waits on it and reports its error. That wait is what keeps dependency order
when a child and its parent are stopped at once, and its cost is the rule in
Invariants below: a hook may not `Stop` its own scope or an ancestor.

The `once` type is that pattern by itself -- claim, settle, ctx-bounded wait --
because the scope has two of them. Its fields are guarded by the state mutex
rather than one of its own, so claiming a phase and recording what the claim
decided (`stopCtx`, for `Stop`) stay one critical section and no third lock
joins the ordering rules. Both callers report the owner's error, including the
scope-wide drain: a request scope ending while the application shuts down is
exactly where a dropped drain failure needed reporting.

**Draining precedes everything, and it has a machine of its own.** `Stop` is
drain, then mark stopped, then children, then this scope's instances. Both
levels of the drain phase are once-with-wait, mirroring the stop phase:
`state.drainOnce` for the scope, `instance.dr` (`drainNone` → `draining` →
`drained`) for one hook. A second `Stop` arriving at either waits instead of
skipping -- recording only that a drain had been *decided* let a concurrent
`Stop` walk past a hook still running and begin releasing what it was using.

`drainRun.sweepAll` sweeps repeatedly rather than once, and `drainRun.visit`
sweeps *every* scope the phase owns on every pass. Draining is the only
teardown phase during which the scope still resolves, so a hook finishing
in-flight work may build a service or open a child scope for the first time;
those owe a drain too, and it has to happen before `stopped` is set. Visiting
each descendant once was enough for a hook that builds into its own scope and
not for one that builds a level along. A pass that does no work ends the phase,
and `ctx` bounds the sweep as well as the hooks.

The sweep is a post-order walk that claims a descendant's phase immediately
before descending into it, not a discovery pass that claims the whole subtree
and a sweep that follows. That ordering is the invariant: while a hook runs,
the only phases the run holds unended are the scope being swept and its
ancestors, which is exactly the set a hook may not `Stop` anyway. Claiming
ahead deadlocks a hook that stops a scope the walk has taken but not yet
reached. A scope another `Stop` already owns is waited for and then left alone,
subtree included: that `Stop`'s run drains it.

A descendant's phase ends when its own sweep does, not when the run does.
Holding it open to the end deadlocks the case the phase exists for: an HTTP
server draining in an outer scope waits for a handler, and that handler is
stopping its request scope. That is also why the scope-level guard is not
enough on its own and `stopIfNeeded` waits out `draining` per instance: an
instance built after its scope's phase ended can be drained by a sweep still
running above it exactly as its own `Stop` arrives. `drainIfNeeded` skips an
instance whose scope is already stopped, because winding something down for
work it can no longer take on is the opposite of what the hook is for.

Three windows stay open, and each is cheaper to accept than to close: an
instance published between the last sweep and `stopped.Store(true)` is not
drained, and closing that would mean holding the state mutex across user hooks;
the same for one built into a scope whose phase another `Stop` already ended;
and a hook running on such a late instance can find its scope stopped mid-hook.
That last one is why the concurrent driver exercises resolution inside drain
hooks but does not assert that it succeeds.

**Who hears a drain failure is decided by who owns the teardown, not by who
ran the hook.** A sweep settles a descendant's failures into that descendant's
phase, so the descendant's own `Stop` reports them, and an ancestor inherits
them by stopping that descendant and joining what its `Stop` returns. When a
second `Stop` of that descendant is already in flight, that call owns the
teardown, hears the failure, and detaches the scope as it finishes -- so
whether the ancestor also hears it depends on whether the detach beats the
ancestor's read of its child list. Both orders are correct, because the failure
always reaches the caller that owned the teardown, and `EventDrain` carries it
to observers either way.

**Do not write an ordering oracle, or a test, that requires the ancestor to
hear it**: one did, and failed about one run in eight.
`TestReview4ChildStopReportsItsOwnDrainFailure` is that test with the assertion
narrowed, and `TestReview5RootStopReportsAChildsDrainFailure` pins the half
that is fixed -- an ancestor that does own the teardown. Taking the child list
before the drain as well as after would make the ancestor hear it always, and
it is the wrong trade: the ancestor would then also inherit a deadline the
*other* caller set, so a `Stop` that waited properly and released everything
would report `context.DeadlineExceeded`. `TestReview3LostDrainWaitStillReleases`
and `TestConcurrentImpatientStopStillReleases` are the guard on that.

Both levels wait out `phaseStarting` rather than stepping around it, which is
what the no-`Stop`-from-a-hook rule buys: the goroutine running that start step
can no longer be this one. `drainIfNeeded` waits for it because a service that
is starting owes a drain as soon as it has started, and leaving it undecided
for a later pass meant a start step that outlasted the phase was never drained.

**`freeze` is transactional.** Registrations queue in `pending` and commit in
one batch. The batch is validated against *prospective copies* of
`index`/`groups`/`all`, so a rejected registration leaves the scope untouched
and keeps being rejected identically. Do not move a mutation of the real maps
before validation.

**A key names one live value per scope, and four guards each close one way of
getting two.** In `freeze`: a second registration of a key in the same scope
must carry `override`, or it is a collision and is rejected naming both sites
-- last-wins used to be silent, which let one module reroute another's wiring
without a word said (#6). An `override` with nothing in *this scope* to
override is rejected too, because a fake for a renamed service is otherwise a
registration nobody resolves; a child shadows its parent without the marker,
since that is a different registry. Then `used` (the key has served),
`resolving` (a resolution is in flight) and `served` (this scope handed the key
down from an ancestor) each reject a replacement the marker cannot excuse. The
"nothing to override" check is deliberately same-scope only: checking ancestors
from inside `freeze` would mean taking a parent's mutex while holding the
child's, and no two state mutexes are ever ordered against each other.

**Two levels of registration semantics.** Lifetime and hooks belong to one
registration, because they are typed on that value. Eagerness belongs to the
*key*: it means the service exists by the time `Start` returns, so it transfers
to whichever binding owns the key -- an `Override()` inherits it. `deriveEager`
is the single place that decides what `Eager` means, and it validates in the
same loop so the derived set and its rules cannot drift apart.

**A module is a label on the handle, not a scope.** `Use` calls each `Module`
through a `Scope` view carrying the function's name; `register` stamps it on
the binding, `view` and `Child` propagate it, and `construct` hands a
constructor a view labelled with its own binding's module, so a registration
made from inside a constructor is attributed to the module that registered the
constructor. Nothing about lookup changes: modules are attribution for messages
and events, and privacy -- if it is ever wanted -- must be a namespace *within*
a scope, never a child scope with exports, because teardown is children-first
and an exported dependency in a child would be torn down before its dependants
in the parent (#6, the analysis).

**The graph is recorded by watching, and only while a constructor runs.**
`resolve` appends the instance it just produced to `deps` on the instance of
the node that asked for it, which is `s.r` -- the node this resolution hangs
off, not the one it just made. A node with no binding is a top-level call, and
the test for that is at the call site rather than inside `dependOn`, so a warm
`Get` pays a pointer comparison instead of a function call; that difference is
measurable in the resolve benchmark. `deps` is guarded by the *asking*
instance's holder mutex, because a constructor may resolve from several
goroutines that share the `*Scope` it was handed.

Two consequences fall out of using the resolution path rather than a registry.
A resolution made through a `Scope` a constructor kept is a new path, whose
first node has no binding, so it records nothing -- the same rule that stops it
being called a cycle. And a failed resolution records nothing either, since it
produced no value and its path is already in the error. Only `Explain` and
`Graph` read `deps`; nothing in the build, start or stop machine does, so a
mistake here cannot break resolution.

**`Wire` declares what `Provide` reveals.** `Wire[T](ctor any)` reads the
constructor's signature with reflection once, at registration, and stores the
parameter types on the binding as `wants`; the build is an ordinary `build`
func that calls `s.get` for each and then `reflect.Call`, so everything below
`register` is shared with `Provide` and the machine never sees the difference.
`T` is spelled out because it cannot be inferred from `any`, and a result
merely assignable to `T` is accepted, which is how a concrete constructor
serves an interface key. (A typed `Wire0..Wire6` with `E` variants was
prototyped beside it and dropped: fourteen methods to save a repeated type
argument, with the compiler checking only an arity the reflective form cannot
get wrong.) Every registration method calls `register` directly, because
`callsite` counts a fixed number of frames and a `Wire` that went through
`Provide` would record a site inside `di.go`.

`Explain` draws `wants` under an unbuilt node with dashed edges
(`declaredInto`), switching back to the recorded tree wherever a declared
dependency has been built; `declaredBy` is the reverse direction and reads only
committed registrations (`peek`, a lookup without the freeze), because it walks
every scope of the container and a root `Explain` must not be the call that
rejects a child's pending batch. `Graph` lists built instances only, and a
built `Wire` instance's recorded edges are its declared ones. `Modules` is the
third renderer, grouping live bindings by `binding.module` and resolving each
binding's `wants` from its holder to name the serving module; keys use
`shortName`, package-qualified rather than import-path-qualified, because a
module report is read beside module labels of the same shape. Its dedupe set is
keyed by section as well as line, since a closure's key is listed under
"provides" and again under "unchecked". The guide pins its report in
`examples/guide/testdata/modules.txt` next to the Explain golden.

**`Wrap` binds at registration.** `Wrap[T]` finds what serves `T` when it is
called -- `state.current`, which reads this scope's pending batch and index
*without* freezing, because a freeze here would end the batch for every
registration made so far and make a later `Eager()` on one of them a "modified
after the scope was first resolved" panic; then `lookup` from the parent, which
freezes ancestors as a resolution would -- and stores it as
`binding.inner`/`innerAt`. The build resolves the inner through
`resolve(inner, innerAt)` rather than by key, since the key now names the
wrapper, and calls `markServed` as `get` would; the edge, the build order and
the cycle check all fall out of that. The wrapper serves the key from its scope
down, so a child's wrapper is that child's registration of the key over the
parent's instance, which is what makes it fx's module-scoped `Decorate` without
a new mechanism.

Three guards: `freeze` exempts a wrapper from the collision rule and applies
the used/resolving/served guards to it as to an override, sets `scoped` from
the inner there rather than at registration because the inner's own `Scoped()`
may come later in the batch, and rejects an `Override` of a binding whose
`wrappedBy` is set -- the cross-scope case, where a parent's later override
would leave a child's wrapper composing over a registration nothing else can
reach. `validate.go`'s `live` follows `inner` chains so the wrapped
registration gets its own turn though it is no longer in `index`; `declared`
puts the inner edge first, bound rather than looked up, for both `Validate` and
`Explain`.

**`Validate` walks `wants`.** Its node is a binding *in the scope it would be
built in*, because a `Scoped` binding built in one scope looks its dependencies
up from there, so the same binding under two holders is two nodes and the memo
(`done`) is keyed by both -- and so is the path used for cycle detection
(`step`), which once compared bindings alone and called a valid graph that
visits one `Scoped` binding from a child and again from the root a cycle (#35).

Three modes say what a missing dependency means: a singleton on its own turn is
`strict`; a `Scoped` binding as the validating scope would resolve it is
`lenient`, and what is missing is `Owed` rather than an error, because a
descendant may provide it and no scope-position rule can tell an intermediate
scope from a leaf -- only the caller knows it is one, and says so with
`Provided` stubs, which name what the resolving scope will hold and make
everything else it lacks an error (`leaf`); a stub is honoured on the `lenient`
path only, since a singleton builds in its own scope where a descendant's
values are not; a singleton reached from anything else is `cyclesOnly`, since
its own turn reports what it misses. A `Scoped` dependency is walked in the
caller's mode under the same holder, which is how a singleton that would build
a `Scoped` service its scope cannot satisfy becomes an error -- the one
definite failure a singleton-captures-`Scoped` shape has; a capture that is
satisfiable works at runtime and is not reported. Cycles are reported once,
keyed by their members, whichever turn finds them.

**A key is served to a whole route, not just to its destination.**
`binding.used` protects the owner; `markServed` records the key in every scope
between the resolver and that owner. Both halves matter: an earlier version
marked only the endpoint, and a scope in the middle could then shadow a key it
had already handed out. An interface is served by a constructor returning the
implementation, which is an ordinary binding -- `Bind` aliases, which added
hops to the route and needed a cycle detector of their own, are gone.

**A stopped scope refuses to serve, and that is checked twice.** `resolve`
checks on the way in, and `await` checks again after the wait, because the
scope can stop while a resolution is parked on someone else's build. The second
check is on the *resolving* scope, not the instance's holder: the holder is
always that scope or an ancestor, so checking the resolver covers both, and a
stopped child must refuse the request whether or not what it asked for is still
alive above it.

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
with an `OnStop` for an `OnStart` that never finished. `binding.used` is set
only when a resolution actually served a value, and `state.served` records keys
a scope resolved from an *outer* scope, because `used` lives on the outer
binding and cannot protect the inner scope.

## Invariants that are easy to break

- Never set a phase or read `running`/`stopped` for a decision outside the
  owning state's mutex.
- **`Stop` is synchronous, and that rests on one rule: no hook may call `Stop`
  on its own scope or an ancestor.** `stopIfNeeded` waits out every step
  another goroutine owns for the instance -- `phaseStarting`, then `draining`
  -- so a teardown outlives `Stop` only when `ctx` expired. The old asymmetry
  (a mid-start teardown handed off via `stopWanted`) existed because a start
  hook was allowed to call `Stop` and Go cannot tell that goroutine from any
  other; every review reported the asymmetry as a bug. `Stop` reports the
  misuse instead of waiting whenever it can see it: hook contexts carry their
  scope (`inHook`/`hookOwner`), so a hook that passes on the context it was
  given gets an error naming `Shutdown`. A hook that passes a context of its
  own is invisible and waits, which is why the fallback still has to be a
  bounded wait rather than a promise.
- Every user hook is called through `callHook` (or `startClaimed`'s
  equivalent), which turns a panic into that hook's error. A cancelled
  `Worker`'s return is dropped only when it says nothing beyond
  `context.Canceled` (`onlyCancellation` walks the error tree); `errors.Is`
  matched `errors.Join(ctx.Err(), failure)` and dropped the failure with it
  (#35). A hook can panic by resolving something whose registration is rejected
  -- the rejection is a panic, and `Resolve` re-panics anything that is not an
  `abort`. Letting one escape `Stop` leaves `stopOnce` claimed and never
  settled, which is a hang for every later `Stop` and a leak of everything
  behind it.
- Nothing in the teardown path may run a user hook against a value another hook
  still holds. That is one rule with three instances: `OnStop` after `OnDrain`,
  `OnStop` after a `Worker` hook (deferred to `releaseAfterWorker` when `ctx`
  expires rather than run alongside it), and a parent's hooks after a child's.
- `Start`'s rollback goes through `Stop` with `context.WithoutCancel`, so it
  stops child scopes and waits for `Worker` hooks.
- Whichever `Stop` call queues a handoff owns that teardown's context; a later
  `Stop` must not clobber it.

## Testing strategy

Five layers, each catching a different class.

**The regression files** — one test per historical defect, grouped by the part
of the library the defect lived in rather than by the review that found it:
`cycles_test.go` (the resolution path), `wiring_test.go` (registration and
lookup), `teardown_test.go` (start, stop, rollback), `drain_test.go` and
`worker_test.go` (`Worker` hooks and `Shutdown`), with the stand-in types they
share in `fixtures_test.go`. A new one goes wherever its rule lives.
Provenance is a tag on each test -- `(review 2, 5)`, `(pass 4)` -- because
grouping by it put three files between two tests of the same machine; each
file's header explains the tags and the commit each review was checked against.
**Verify a new test fails against the commit that preceded the fix**, e.g. by
restoring the old `di.go` from git and running just that test, and tag it.
Several tests here turned out to pass both before and after; say so rather than
implying coverage.

**`property_test.go`** — random *registration* sequences checked against a model
of the eager rules and of which registration serves a key: a repeat must be
marked `Override`, an `Override` needs a target, a group member does neither. A
predictive model can be wrong in the same way as the code, so treat it as
needing its own scrutiny; the override half was mutation-tested when added.

**`machine_test.go`** — random *operation* sequences (register, resolve, start,
stop, shutdown) across a root, two children and a grandchild, checked against
invariants taken from documented guarantees rather than predicted values. This
is the layer that catches error-path and cross-scope bugs. I4 has no exemptions
now that every lifetime is tracked; the old exemption for aliased keys is what
once hid a scope handing out two live values for one interface, so do not
reintroduce one lightly.

`op.wire` is a bit that was spare in the fifth byte, so adding it kept every
corpus entry's meaning: when set, the shapes that have a constructor to hand
over register it through `Wire` instead of `Provide`, in both this machine and
the concurrent driver, which is what puts `reflect.Call` under `-race` and
gives `Validate` declared edges to walk. `Validate` is checked at the end of
every sequence (I8: builds nothing, repeatable) and inside the concurrent
render lane; what it *says* is pinned by `validate_test.go`, because predicting
it here would model the lookup rules a second time. The same bit turns shape 1
(machine) and shape 2 (concurrent) into a wrapper over whatever serves the key,
or a registration-time rejection; the property model tracks a per-key chain
length, since an eager wrapped key builds every registration in its chain under
one name.

**`lifecyclemodel_test.go`** — the one place that *does* predict, because the
argument against predicting does not hold for it. What serves a key depends on
overrides and the eager rules, and modelling that would be modelling the code
twice; what happens to an instance *once it exists* is a small state machine
the package documents completely, and it is the half every review found defects
in. So the model takes builds as given -- the constructors report themselves --
and predicts the rest: which hooks are owed, in what order, exactly once
(M1-M6). It is what lets the sequential layer check the thing the concurrent
driver says it cannot: that an instance owing a drain gets one.

Two facts are observed rather than predicted, and both are marked in the file:
whether a start step succeeded, because a rollback stops what had started at
the moment it failed; and whether `Start` was ever called on a scope, read back
through `Scope.Context`, because whether a rejected `Start` had already
recorded its context depends on which panic came first, which is not a
documented guarantee.

**`concurrent_test.go`** — the same operations run in parallel lanes under
`-race`, in two phases (wire, then everything else). It checks only what
survives concurrency, as ten oracles listed at the top of the file: only a
configuration rejection may panic (C1), every operation returns (C2), `Stop`
respects scope order (C3, the one that catches a parent running ahead of a
child), nothing is stopped more often than built (C4), a service is built once
however many resolutions race for it (C5), no stop hook of an instance begins
while that instance's own drain hook runs (C6), drain hooks resolve (C7), one
fixed graph gives one verdict so two resolutions of a key never disagree about
a cycle (C8), every instance owing a stop step gets exactly one by quiescence
(C9), and a resolution *begun* after its scope's `Stop` returned fails (C10).
C9 needed a definition of quiescence, since a release deferred past a missed
deadline lands after every `Stop` has returned: `settle` polls until no hook is
running and nothing owed is unreleased.

The driver's hooks can panic -- a drain hook resolves through child scopes, and
one left with a permanently rejected registration meets the rejection as a
panic out of `Resolve`, which the container recovers into the hook's error. So
every piece of bookkeeping a hook does after its first line must be deferred,
or an oracle reads a hook that panicked as one that never ended. C6 reported
exactly that once.

**The exemptions these oracles need are the most dangerous part of them.** C3
cannot order a release that a missed deadline deferred, so it is switched off
for scopes where a `Stop` reported one -- and switching C6 off with it, which
looked like the same exemption, silently disabled the drain/stop overlap check
for the one shape that needs it. C6 holds however impatient the `Stop` was: a
missed deadline defers a release, it never runs one early. Mutation-test an
exemption before believing it.

`scheduler_test.go` makes the interleaving an input: every hook and every
operation parks at a scheduling point and a seed decides which parked goroutine
goes next, so `TestMachineScheduled` replays one sequence under many orderings
with every oracle live. It explores rather than verifies -- a released
goroutine can block inside the container where no test can see it -- and the
loop waits ~200µs for goroutines to gather before it chooses, because releasing
each one as it arrives leaves the seed nothing to decide.
`TestMachineConcurrentShapes` builds op sequences directly rather than from
bytes, because a byte seed has to survive four modulos to reach a particular
interleaving; its three shapes are what the coverage gap said no random
sequence was reaching. Delete either deferred release in `di.go` and C9 fails.

`FuzzMachine` and `FuzzMachineConcurrent` run the same invariants under
coverage-guided search; the corpus in `testdata/fuzz/` is committed and CI runs
90s in its own job. Run the concurrent one with `-race` or it checks almost
nothing. The sequential generators do not explore goroutine interleavings; that
is what `concurrent_test.go` and the stress loops under `-race` are for (see
`TestRegressionStartRace`).

The rendering is generated against too: the sequential machine renders every
scope and explains every key at the end of a sequence (I7), and the concurrent
driver renders inside its lanes, which is the only way a generator meets an
instance mid-build or mid-start and the only thing that puts the rendering's
reads under `-race`. Both tolerate a configuration rejection from `Explain`,
which looks a key up and so commits the pending batch exactly as a resolution
would, and nothing else.

`scripts/generatorgap.go` is the map of what only the hand-written tests reach,
which is the map of where the next review will dig: every defect the September
2026 reviews found lived on such a line. CI runs it with a floor of 90%
generator coverage; when the floor moves, move it up. CI also checks the
script's arithmetic against `go tool cover -func`, because it has been wrong
twice -- once keying coverage blocks by line number, when `di.go` has eighteen
lines carrying more than one block, and once attributing a block to a function
in the wrong file when `explain.go` arrived. Both answers looked plausible,
which is the dangerous kind of wrong. A tool that measures a gap has to be
measured itself.

**Where the defects came from.** Five reviews in September 2026, preceded by
seven narrower passes. The first found eleven defects plus a gap it did not
count; the second six plus the `Worker`-hook overlap, and then two more the
tightened driver found on its own; the third six that were all cross-phase or
cross-branch -- a drain hook stopping a sibling scope, a release dropped with a
missed deadline, a shutdown cause published after `Run` had read it, a false
cycle through a finished frame, a `Transient` skipping the stopped check, and a
child scope made in a constructor starting a fresh path. No generator reaches
any of the third review's six.

That is the recurring lesson, and it is about *shapes*, not oracles. Adding C6
changed nothing until the driver gained a registration that is `Scoped` **and**
draining: every other draining shape is a plain singleton, so resolving one
from a child hands back the instance the owner already holds, and a
drain-owing instance could never appear in a child scope at all. One missing
registration made a whole class of defect unreachable. Two other things had
been missing and are now fixed: C1 accepted *any* `panic(error)` as
legitimate, because that is how `Get` reports failure, so a false `ErrCycle`
read as normal; and the drain hooks returned nil and touched nothing, so the
phase was exercised without being checked. **When an oracle or a model check
finds nothing, suspect the generator before believing the code** -- and run the
fuzzer, since the seeded sweep is thinner than the accumulated corpus.
Mutation-testing showed the difference: stopping in build order instead of
reverse is caught by the fuzzer in 0.06s and *not* by the 400 seeded sequences.

## Repo conventions

- **README code blocks are generated.** They are embedded from `examples/` with
  embedmd markers. Run `gofmt -w` on an example *before* re-embedding, or CI
  fails on the sync check.
- **`examples/` and `benchmarks/` are separate modules**, each with a
  `replace ../` directive, so the root module keeps zero requires and the
  library's "no dependency outside the standard library" claim stays true.
  Each module path is its directory's old import path, so nothing an example
  imports changed when it moved out. The consequence to remember: a root
  `go test ./...` no longer covers the examples, and `golangci-lint run ./...`
  no longer lints them -- CI runs `go vet`, the tests and the runnable examples
  from `examples/` in their own step. `gofmt -l .` still walks both, since
  gofmt does not stop at a module boundary.
- **`dislog/` is the slog bridge for `Observe`**, and it imports nothing but
  `log/slog` and the library, which is what keeps the root module clean:
  `charmbracelet/log` is an slog handler, so it is a dependency of `examples/`
  only. `dislog.New` returns the `func(di.Event)` that `Observe` takes -- not a
  `slog.Handler`, despite the shape of the name. A failed step logs at Error
  with the site attached; a step that succeeded logs at Info, or wherever
  `Level` puts it. `samber/do` and `go.uber.org/dig` are
  dependencies there only. Two of the three comparisons are like-for-like and
  one is not: dig has no typed accessor, so its warm number is `Invoke` with a
  function reflected over on every call, which is not how an fx application
  resolves. The file says so, and so does the README; do not quote the dig warm
  figure without it. `di` is measured twice, because dig's `Provide` is
  reflective like `Wire` and not like a `Provide` closure -- and the warm
  figures for the two are the same to within noise, which is the check on the
  claim that a warm `Get` is one code path.
- **`site/` is the landing page and guide** at https://floatdrop.github.io/di/,
  a React project built with Gravity UI, prerendered to static HTML in English
  at `/` and Russian, Chinese and Japanese at `/ru/`, `/zh/` and `/ja/`, each
  one value of a `Content` type so a missing translation is a compile error
  (though not a stale one), and deployed by `.github/workflows/pages.yml`
  on pushes to `main` that touch it or `examples/guide/`. It ships no React;
  the only JavaScript is one inlined script. Its code blocks are the files of
  `examples/guide`, imported as raw text at build time, so the guide cannot
  drift from code the Go CI compiles and tests; the `Explain` tree and the
  `Modules` report it shows are `examples/guide/testdata/`, pinned by golden
  tests with an `-update` flag. Code is never translated. **`site/README.md`
  has the rest**, including the traps -- every one of them produces a page that
  reads as a botched design rather than a missing file, so read it before
  believing a rendering bug.
- **`examples/guide` is a multi-package application** (config, storage, cache,
  mail, api) whose `cmd/api` blocks on signals like the other servers; its
  tests start it on a random port instead. It uses no `Provide` closure: the one
  thing that needs the scope, the request-scope middleware, comes from
  `dihttp.Module` as a `dihttp.Middleware` dependency, and routes resolve their
  handler types through `dihttp.Handle((*Users).Show)`. Its packages export only
  their contract and `Module`: keys are types, so an unexported type is a
  private service, which is the whole privacy model (the `Private()` marker and
  child-scope exports were considered and are not needed; see the Modules
  section of the README).
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
  `stopIfNeeded` with `context.WithoutCancel`, whose `Done` is nil, so it waits
  properly and cannot recurse again. Any ordering oracle has to model them or it
  will report them as defects; the concurrent driver does it by running every
  `Start` before any `Stop`, so no start step is ever in flight.
- **CHANGELOG is enforced.** `.github/workflows/release.yml` fails a tag push
  when `CHANGELOG.md` has no `## [<version>]` section. It tracks library
  behaviour: a docs- or site-only change adds no entry. The public API has been
  stable across tags; verify with `go doc -all` diffed between tags before
  choosing a version number.

## Tooling caveats

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
