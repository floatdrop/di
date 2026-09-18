# CLAUDE.md

Guidance for Claude Code (claude.ai/code) working in this repository.

`github.com/floatdrop/di` is a dependency-injection container for Go 1.27+ built
on generic methods, with no dependency outside the standard library.
[`docs/DESIGN.md`](docs/DESIGN.md) is the model — resolution, lifetimes, phases,
cycles, teardown, with diagrams. This file is the working detail behind it; the
two are edited together.

Library: `di.go` (package doc, keys, events, `Scope`, modules, `Test`),
`binding.go` (registration, the `Binding` handle), `state.go` (a scope's
registry, `freeze`, parent-chain readers), `resolve.go` (resolution path, both
cycle detectors, build step, `Get`), `lifecycle.go` (instance phase machine,
hooks, `Start`, `Stop`), `run.go` (`Run`, `Shutdown`), `explain.go` (renders the
recorded graph), `validate.go` (checks the declared graph). Plus the net/http
adapter `dihttp/`, the slog bridge `dislog/`, tests, and two separate modules,
`examples/` and `benchmarks/`.

## Working rules

- **Target modern Go.** Invoke the `modern-go-guidelines:use-modern-go` skill
  before writing Go here, and follow it: prefer `slices`, `maps`, `cmp`,
  range-over-func, and the rest of what Go 1.27 offers over legacy patterns.
  (If the skill is not listed, install it:
  `/plugin install modern-go-guidelines@goland-claude-marketplace`.)
- **Never reimplement the standard library.** No hand-rolled `max`/`min`,
  `slices.Contains`, `slices.SortFunc`, `maps.Keys`, `cmp.Or`, `errors.Join`,
  `sync.OnceValue`. If a builtin or stdlib function does it, call it.
- **Comments are short.** State what the code does or which invariant it
  carries, in one line where possible. Do not narrate the change, justify the
  edit, compare with the previous version, or record history — the deep
  rationale belongs in `docs/DESIGN.md` and in commit messages.
- **Review every change before reporting it done.** Run the full gate below,
  then run the `code-review` skill over the diff and act on its findings. A
  change is not finished until both pass.

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
go run scripts/og.go                          # redraw the social card after a logo change
cd benchmarks && go test -bench . -benchmem   # separate module
cd examples && go test ./...                  # separate module
cd examples && go test ./guide -update        # rewrite testdata/ after rewiring the guide app
cd site && npm ci && npm run check && npm run build   # guide site; BASE_PATH=/di for Pages

go test -count=1 -run 'TestMachine|TestConcurrent|TestProperty|FuzzMachine' -coverprofile=gen.out .
go test -count=1 -coverprofile=all.out .
go run scripts/generatorgap.go -floor 90 gen.out all.out   # what only hand-written tests reach
```

The full gate, as one chain, the way CI runs it — run this before committing:

```sh
test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./... \
  && golangci-lint run ./... \
  && go run github.com/campoy/embedmd@v1.0.0 -d README.md
```

## Architecture

Reading the library top to bottom does not reveal the model. These are the
pieces that only make sense together; `docs/DESIGN.md` has the long form.

**Three levels of state.** A `binding` is a registration (key, lifetime, hooks,
`build`). An `instance` is one built value. A `state` is a scope's registry and
lifecycle bookkeeping. `Scope` is a handle over `*state` (field `st`, not an
embedding) plus a `*resolver` carrying the current resolution path; the `Scope`
handed to a constructor is a *view* over the same state with that path attached.

**Who owns an instance.** A singleton lives in the scope that registered the
binding (`owner`); a `Scoped` instance lives in the scope that resolves it
(`holder`). `resolve` picks the holder; the rest of the pipeline works in terms
of it.

**The instance phase machine** (`phaseNew` → `Building` → `Built` → `Starting` →
`Started`/`Failed` → `Stopped`) is read and written *only* under the owning
state's mutex. Splitting a start-or-stop decision across two critical sections
has produced several bugs. `publish` appends to the stop list *before*
`startIfRunning` reads `running`, and `Start` sets `running` before it drains:
that ordering is what makes exactly one path start an instance.

`await` is the only way to reach an instance. It claims the build step or waits
for whoever did, and waits out `phaseStarting`, so no resolution hands back a
service whose `OnStart` is still running. `settled` means `value`/`err` are
final.

**The warm path takes no lock in the owning scope.** A top-level resolution of a
built singleton reads three atomics: `hasPending` and `reg` per scope looked
through, and `instance.ready`. The two remaining locks belong to the resolving
side (`instanceFor` for a `Scoped` binding, `dependOn` while a constructor
builds). `ready` summarises `ph`/`err`/`settled` and is recomputed by `refresh`
in the same critical section as every change to them; it is set exactly when
`await`'s locked loop would return at once. `startCtx` and `running` are atomics
for the same reason. `benchmarks/parallel_test.go` is the record of what this
bought.

**A waiter blocks on one step, not on the scope.** Each step another goroutine
may finish has a channel closed when it is done: `settledCh`, `startingCh`,
`drainedCh`, and a scope's `sealCh`. The first goroutine that must wait makes
the channel (`waitOn`); the step's owner closes it only if present (`wake`).
Phase and channel are read in one critical section, so a waiter cannot pick up a
later step's channel. Nil is the ordinary state — an uncontended step allocates
nothing. Nothing waits on a predicate mentioning `stopped`, so `teardown` closes
no channel after `stopped.Store(true)`; suspect that first if a resolution hangs
across a `Stop`.

**Two cycle detectors, because one branch cannot see the other.** Within a
branch, `resolver.onPath` walks the immutable path. Across branches,
`resolver.wait` searches a `*graph` before blocking, matching whole paths
(`descends`) in both directions; `graph.under` indexes each blocked resolution
by its path's nodes so a search reads only the waits beneath one builder. Each
wait is its own `waitEdge`, taken back by `unwait`. The check and the edge it
adds are one critical section. Lock order is state mutex then `graph.mu`, never
the reverse. One graph per container, made by `New` and passed through
`newState` — a wait crosses scopes, never containers.

**The resolution path is immutable, and finished nodes stop counting.**
`resolver` is a linked-list node, identified by binding *and* holder, never by
key. `resolver.done` is the only mutable part: `resolve` sets it as it returns,
and `onPath`/`descends` *stop the walk* there, in both directions. That is what
lets a constructor keep its `*Scope` and resolve later without meeting its own
frame as a cycle. `Scope.Child` carries the resolver it was made from, so a
child opened inside a constructor is part of that resolution. `inFlight` — a
path whose last node has not returned — also decides whether `Get`/`All`/`Must`
convert an `abort` into a plain error panic.

**Scopes have a stop machine too.** `state.stopOnce` is claimed by the first
`Stop` and settled when its teardown finishes; later or concurrent `Stop` calls
wait on it and report its error. The `once` type is that pattern by itself
(claim, settle, ctx-bounded wait); its fields are guarded by the state mutex, so
no third lock joins the ordering rules.

**Draining precedes everything.** `Stop` is drain, mark stopped, children, then
this scope's instances. Both levels are once-with-wait: `state.drainOnce` per
scope, `instance.dr` (`drainNone` → `draining` → `drained`) per hook — a second
arrival waits rather than skipping. `drainRun.sweepAll` sweeps repeatedly and
`visit` sweeps every scope the phase owns on every pass, because the scope still
resolves during a drain and a hook may build or open a child. The sweep is a
post-order walk that claims a descendant's phase immediately before descending,
and a descendant's phase ends with its own sweep, not with the run: claiming
ahead, or holding phases open, deadlocks a hook stopping a scope the walk has
taken. `stopIfNeeded` waits out `draining` per instance for the same reason.
`drainIfNeeded` skips an instance whose scope is already stopped.

**The end of the drain phase is sealed, not guessed.** A build published into
the subtree or a start step claimed there can create drain work after a sweep
decided. `announce` bumps `drainGen` on the scope and every ancestor, then reads
`sealed` up the chain; `seal` stores `sealed`, then reads `drainGen`, storing
`stopped` only if it did not move. Neither side can miss the other: the sweep
goes round again, or the announcer learns the scope stopped and undoes its work
(`publish` undoes the build, `gateStart` undoes the claim and calls `refresh`).
`claimNext` returns nil for a stopped scope. `drainGen` is per subtree, so an
unrelated request cannot force a re-sweep — the price is that a subtree which
never quiets holds the phase open until `ctx` expires.

**Who hears a drain failure is decided by who owns the teardown**, not by who
ran the hook. A sweep settles a descendant's failures into that descendant's
phase; an ancestor inherits them by stopping it and joining its error. When
another `Stop` of that descendant is already in flight, whether the ancestor
also hears it depends on whether the detach beats the ancestor's read of its
child list. Both orders are correct. **Do not write an oracle or test requiring
the ancestor to hear it** — one did, and failed about one run in eight
(`TestReview4ChildStopReportsItsOwnDrainFailure` is it, narrowed;
`TestReview5RootStopReportsAChildsDrainFailure` pins the fixed half;
`TestReview3LostDrainWaitStillReleases` and
`TestConcurrentImpatientStopStillReleases` guard the deadline trade).

**`freeze` is transactional.** Registrations queue in `pending` and commit in
one batch, validated against *prospective copies* of `index`/`groups`/`all`, so
a rejection leaves the scope untouched and keeps being rejected identically.
Never mutate the real maps before validation. The copies commit as a new
`registry` behind an atomic pointer and are never written again — that is what
lets `lookup`, `All` and the renderers read without the mutex.

**A key names one live value per scope, and four guards close the ways to get
two.** A second registration of a key in one scope needs `override`, or it is a
collision rejected naming both sites; an `override` with nothing in *this* scope
to override is rejected too (a child shadows a parent without the marker). Then
`used`, `resolving` and live `wrappers` — one embedded `guard` with one `against`
check returning the end of the rejection sentence — and `served`, which stays on
the scope because it is a fact about the scope that handed the key down. The
nothing-to-override check is same-scope only: no two state mutexes are ever
ordered against each other.

**Two levels of registration semantics.** Lifetime and hooks belong to one
registration. Eagerness belongs to the *key* — it means the service exists by
the time `Start` returns, so an `Override()` inherits it. `deriveEager` is the
single place that decides it, and validates in the same loop.

**A module is a label on the handle, not a scope.** `Use` calls each `Module`
through a `Scope` view carrying the function's name; `register` stamps it on the
binding, and `view`/`Child`/`construct` propagate it. Lookup is unchanged:
modules are attribution for messages and events. Privacy, if ever wanted, must
be a namespace *within* a scope, never a child scope with exports: teardown is
children-first, so an exported dependency in a child would be torn down before
its dependants in the parent (#6).

**The graph is recorded by watching, only while a constructor runs.** `resolve`
appends the instance it produced to `deps` on the instance of `s.r`, the node
that asked. A node with no binding is a top-level call, tested at the call site
so a warm `Get` pays a pointer comparison. `deps` is guarded by the *asking*
instance's holder mutex. A resolution through a kept `Scope` records nothing, and
so does a failed one. Only `Explain` and `Graph` read `deps`.

**`Wire` declares what `Provide` reveals.** `Wire[T](ctor any)` reflects over the
signature once at registration and stores the parameter types as `wants`; the
build calls `s.get` for each, then `reflect.Call`, so everything below `register`
is shared with `Provide`. A result merely assignable to `T` is accepted. Every
registration method calls `register` directly, because `callsite` skips the two
frames `register` tells it to — `TestRegistrationSiteNamesTheCaller` guards that
count.

`Explain` draws `wants` under an unbuilt node with dashed edges (`declaredInto`)
and switches to the recorded tree wherever a declared dependency is built;
`declaredBy` reads only committed registrations (`peek`), so a root `Explain`
cannot reject a child's pending batch. `Graph` lists built instances only.
`Modules` groups live bindings by `binding.module`, resolving each binding's
`wants` from its holder, with `shortName` keys; its dedupe set is keyed by
section as well as line. The guide pins both in `examples/guide/testdata/`.

**`Wrap` binds at registration.** `Wrap[T]` finds what serves `T` when called —
`state.current` reads this scope's pending batch and index *without* freezing,
then `lookup` freezes ancestors as a resolution would — and stores it as
`binding.inner`/`innerAt`. The build resolves the inner by binding, not by key,
and calls `markServed`; edge, build order and cycle check fall out of that. A
child's wrapper is that child's registration of the key over the parent's
instance, which is fx's module-scoped `Decorate` with no new mechanism.

`freeze` exempts a wrapper from the collision rule, applies the
used/resolving/served guards as to an override, sets `scoped` from the inner
there (the inner's own `Scoped()` may come later in the batch), and rejects an
`Override` of a binding that still has `wrappers`. `wrappers` is a set, because
sibling scopes wrap one parent registration independently; `teardown` removes
the scope's own after storing `stopped`. A rejected batch keeps its wrappers on
purpose. A committed `Override` retires every link of the chain registered in
that scope down to the first wrapping an ancestor; a retired link releases its
mark only once nothing live wraps it (`release`), and `unwrap` carries that down
the chain. The set and `retired` are one immutable `wrapSet` behind an atomic
pointer, replaced by CAS. One window stays open: a descendant's `Wrap` racing an
ancestor's `Override` can mark a link the `Override` already checked — closing
it would order two scopes' commits, which no two state mutexes ever are.
`validate.go`'s `live` follows `inner` chains so the wrapped registration gets
its own turn; `declared` puts the inner edge first, bound rather than looked up.

**`Validate` walks `wants`.** Its node is a binding *in the scope it would be
built in*, so one `Scoped` binding under two holders is two nodes — the memo
(`done`) and the cycle path (`step`) are keyed by both (#35). Three modes say
what a missing dependency means: `strict` for a singleton on its own turn;
`lenient` for a `Scoped` binding as the validating scope would resolve it, where
what is missing is `Owed` rather than an error, unless `Provided` stubs say the
scope is a leaf; `cyclesOnly` for a singleton reached from anything else, since
its own turn reports what it misses. A stub is honoured on the `lenient` path
only. A `Scoped` dependency is walked in the caller's mode under the same
holder. Cycles are reported once, keyed by their members.

**A key is served to a whole route.** `binding.used` protects the owner;
`markServed` records the key in every scope between the resolver and that owner,
so a scope in the middle cannot shadow a key it already handed out. An interface
is served by a constructor returning the implementation — `Bind` aliases are
gone.

**A stopped scope refuses to serve, checked twice.** `resolve` checks on the way
in; `await` checks again after the wait, because the scope can stop while a
resolution is parked on someone else's build. The second check is on the
*resolving* scope, which covers the holder too.

**Errors versus panics.** A *wiring* failure (missing dependency, cycle, failed
constructor) is an internal `abort{err}` panic that unwinds to the nearest
`Resolve`/`Start`/`Run` and becomes an `error`. A *configuration* rejection
(contradictory lifetimes, re-registering a resolved key) is a plain `panic` with
a string prefixed `di: `. So `Resolve` never panics, `Get` panics with an `error`
at top level, and config errors panic with a string. Tests and the fuzz harness
classify panics by that rule.

**When a stop is owed.** `OnStop` runs when `OnStart` succeeded, or when there is
no `OnStart` to pair with (or the scope was never started), making it a plain
destructor. A service built but never started is *not* torn down. Only a start
hook that *returned* counts as succeeded — `callHook` turns a panicking hook
into a failed start. `instance.paired` and `instance.owes` are the one statement
of that predicate, shared by the drain and stop steps.

## Invariants that are easy to break

- Never set a phase outside the owning state's mutex, and never change `ph`,
  `err` or `settled` without calling `refresh` in the same critical section: the
  warm path trusts `ready` without the lock. `running` and `stopped` are atomics;
  decisions on them are sound because of their order against `publish`, not
  because of a lock.
- **`Stop` is synchronous, and that rests on one rule: no hook may `Stop` its own
  scope or an ancestor.** `stopIfNeeded` waits out every step another goroutine
  owns (`phaseStarting`, then `draining`), so a teardown outlives `Stop` only
  when `ctx` expired. Hook contexts carry their scope (`inHook`/`hookOwner`), so
  a hook that passes its context on gets an error naming `Shutdown`; one that
  passes a context of its own is invisible and waits, which is why the fallback
  must stay a bounded wait.
- Every user function goes through `callHook`, the `Go` worker included, and
  every step is reported through `state.report`, so a panicking hook is observed
  like a failing one. A cancelled worker's return is dropped only when it says
  nothing beyond `context.Canceled` (`onlyCancellation` walks the error tree;
  `errors.Is` matched `errors.Join(ctx.Err(), failure)` and dropped the failure
  with it, #35). Letting a panic escape `Stop` leaves `stopOnce` claimed and
  never settled — a hang for every later `Stop`.
- Nothing in the teardown path may run a user hook against a value another hook
  still holds: `OnStop` after `OnDrain`, `OnStop` after a `Go` worker (deferred
  to `releaseAfterWorker` when `ctx` expires), and a parent's hooks after a
  child's.
- `Start`'s rollback goes through `Stop` with `context.WithoutCancel`, so it
  stops child scopes and waits for workers.
- Whichever `Stop` queues a handoff owns that teardown's context; a later `Stop`
  must not clobber it.

## Testing strategy

Tests take their context from `t.Context()`. Two exceptions, both deliberate:
`Test`'s `tb.Cleanup` in `di.go` uses `context.Background()`, because
`t.Context()` is cancelled *before* cleanups run and a dead context turns every
test teardown into the impatient path that defers releases; and
`lifecycle_test.go` compares `s.Context()` against `context.Background()` by
identity, which is the documented pre-`Start` default.

Five layers, each catching a different class.

**Regression files** — one test per historical defect, grouped by the part of the
library the defect lived in: `cycles_test.go` (resolution path),
`wiring_test.go` (registration and lookup), `teardown_test.go` (start, stop,
rollback), `drain_test.go`, `worker_test.go` (`Worker` hooks, `Shutdown`), with
shared stand-ins in `fixtures_test.go`. Provenance is a tag on the test —
`(review 2, 5)`, `(pass 4)`; each file's header explains the tags and the commit
each review was checked against. **Verify a new test fails against the commit
before the fix** (restore the old library files from git and run just that test),
and tag it. If a test passes both before and after, say so.

**`property_test.go`** — random *registration* sequences against a model of the
eager rules and of which registration serves a key. A predictive model can be
wrong the same way the code is; the override half was mutation-tested.

**`machine_test.go`** — random *operation* sequences (register, resolve, start,
stop, shutdown) across a root, two children and a grandchild, checked against
invariants taken from documented guarantees rather than predicted values. This
layer catches error-path and cross-scope bugs. I4 has no exemptions; the old one
for aliased keys hid a scope handing out two live values for one interface, so
do not reintroduce one lightly. `op.wire` is a spare bit in the fifth byte (so
the corpus kept its meaning): when set, shapes with a constructor register it
through `Wire`, putting `reflect.Call` under `-race` and giving `Validate`
declared edges. `Validate` runs at the end of every sequence (I8: builds
nothing, repeatable); what it *says* is pinned by `validate_test.go`.

**`lifecyclemodel_test.go`** — the one place that *does* predict, because what
happens to an instance once it exists is a small documented state machine. Builds
are taken as given (constructors report themselves) and the rest predicted: which
hooks are owed, in what order, exactly once (M1–M6). Two facts are observed, not
predicted, and marked in the file: whether a start step succeeded, and whether
`Start` was ever called on a scope.

**`concurrent_test.go`** — the same operations in parallel lanes under `-race`,
in two phases (wire, then everything else), checking the ten oracles listed at
the top of the file: only a configuration rejection may panic (C1), every
operation returns (C2), `Stop` respects scope order (C3), nothing is stopped more
often than built (C4), one build however many resolutions race (C5), no stop hook
begins while that instance's drain hook runs (C6), drain hooks resolve (C7), one
graph gives one cycle verdict (C8), every instance owing a stop gets exactly one
by quiescence (C9), and a resolution begun after `Stop` returned fails (C10).
`settle` defines quiescence by polling until no hook runs and nothing owed is
unreleased. Driver hooks can panic, so every piece of bookkeeping after a hook's
first line must be deferred.

**The exemptions these oracles need are the most dangerous part of them.** C3
cannot order a release a missed deadline deferred, so it is off for such scopes —
switching C6 off with it once silently disabled the drain/stop overlap check for
the only shape that needs it. C6 holds however impatient the `Stop` was.
Mutation-test an exemption before believing it.

`scheduler_test.go` makes the interleaving an input: hooks and operations park at
scheduling points and a seed picks who goes next, so `TestMachineScheduled`
replays one sequence under many orderings with every oracle live. It explores
rather than verifies. `TestMachineConcurrentShapes` builds op sequences directly,
since a byte seed must survive four modulos to reach one interleaving; its three
shapes are what the coverage gap said no random sequence reached. Delete either
deferred release in `lifecycle.go` and C9 fails.

`FuzzMachine` and `FuzzMachineConcurrent` run the same invariants under
coverage-guided search; the corpus in `testdata/fuzz/` is committed and CI runs
90s in its own job. Run the concurrent one with `-race` or it checks almost
nothing.

The rendering is generated against too: the sequential machine renders every
scope and explains every key at the end of a sequence (I7), and the concurrent
driver renders inside its lanes — the only way a generator meets an instance
mid-build and the only thing putting the renderers' reads under `-race`. Both
tolerate a configuration rejection from `Explain` and nothing else.

`scripts/generatorgap.go` maps what only hand-written tests reach, which is where
the next review will dig: every defect the September 2026 reviews found lived on
such a line. CI runs it with a floor of 90%; move the floor up when it moves. CI
also checks its arithmetic against `go tool cover -func`, because it has been
wrong twice — keying blocks by line number (eighteen library lines carry more
than one), and attributing a block to a function in the wrong file. A tool that
measures a gap has to be measured itself.

**The recurring lesson is about *shapes*, not oracles.** Adding C6 changed nothing
until the driver gained a registration that is `Scoped` **and** draining: one
missing registration made a whole class of defect unreachable. Likewise C1 once
accepted *any* `panic(error)` as legitimate, and the drain hooks once returned nil
and touched nothing. **When an oracle or model check finds nothing, suspect the
generator before believing the code** — and run the fuzzer, since the seeded sweep
is thinner than the accumulated corpus (stopping in build order instead of
reverse: caught by the fuzzer in 0.06s, missed by 400 seeded sequences).

## Repo conventions

- **README code blocks are generated** from `examples/` with embedmd markers.
  `gofmt -w` the example *before* re-embedding, or CI fails the sync check.
- **Coverage is published with the site, not to a service.** `pages.yml` writes
  `go tool cover -html` and a shields endpoint JSON into `site/build` before
  uploading, so the report and its badge are one deploy. It covers `di`, `dihttp`
  and `dislog`. That is why the site workflow's path filter includes `**/*.go` —
  narrowing it back to `site/` would freeze the badge with nothing failing.
- **`examples/` and `benchmarks/` are separate modules** with `replace ../`, so
  the root module keeps zero requires. A root `go test ./...` does not cover the
  examples and `golangci-lint run ./...` does not lint them; CI runs them in
  their own step. `gofmt -l .` still walks both.
- **`dislog/` is the slog bridge for `Observe`** and imports nothing but
  `log/slog` and the library. `dislog.New` returns the `func(di.Event)` that
  `Observe` takes, not a `slog.Handler`. Failed steps log at Error with the site
  attached; successful steps at Info, or wherever `Level` puts them. It shortens
  a service name by composing `Event.Service` with `Event.Package`, which exists
  because there is no correct way to parse a qualified type name back apart
  (`path.Base` drops a pointer's `*` and cuts a generic name at the wrong dot).
  `key.pkgPath` walks through pointers, mirroring `typeName`; the two must keep
  agreeing.
- **`benchmarks/` is where `samber/do` and `go.uber.org/dig` are dependencies.**
  Two of the three comparisons are like-for-like and one is not: dig has no typed
  accessor, so its warm number is `Invoke` reflecting on every call. Do not quote
  the dig warm figure without that caveat. `di` is measured twice (`Provide` and
  `Wire`) as the check on the claim that a warm `Get` is one code path.
- **`site/` is the landing page and guide** at https://floatdrop.github.io/di/ —
  React with Gravity UI, prerendered to static HTML in English at `/` and
  Russian, Chinese, Japanese at `/ru/`, `/zh/`, `/ja/`, each one value of a
  `Content` type so a missing translation is a compile error. It ships no React.
  Code blocks are the files of `examples/guide` imported as raw text at build
  time; the `Explain` tree and `Modules` report are `examples/guide/testdata/`,
  pinned by golden tests with `-update`. Code is never translated.
  **`site/README.md` has the rest, including traps that read as botched design
  rather than missing files** — read it before believing a rendering bug.
- **The social card is generated; uploading it is manual.** `go run scripts/og.go`
  reads `docs/assets/logo.svg` and writes `docs/assets/og.svg` and `og.png`
  (1280x640), rasterising with `resvg` or `rsvg-convert`. Both files are
  committed — the same rule `site/src/components/Logo.tsx` and
  `site/public/favicon.svg` follow. GitHub has **no API for a repository's social
  preview**: the PNG must be uploaded by hand under Settings → General → Social
  preview. Type is whatever the system resolves `og.go`'s families to, which is
  why the PNG is committed rather than built in CI.
- **`examples/guide` is a multi-package application** (config, storage, cache,
  mail, api) whose `cmd/api` blocks on signals; its tests start it on a random
  port. It uses no `Provide` closure: the request-scope middleware comes from
  `dihttp.Module` as a `dihttp.Middleware` dependency, and routes resolve handler
  types through `dihttp.Handle((*Users).Show)`. Packages export only their
  contract and `Module` — keys are types, so an unexported type is a private
  service, and that is the whole privacy model.
- **`examples/app` and `examples/server` block on signals.** Build and run them
  with output going to the terminal, not redirected — this harness loses a
  backgrounded server's startup output when redirected, which once produced a
  false failure report.
- **A teardown finishes after `Stop` returns only when `Stop`'s context expired**
  (waiting for a worker, a start step or a drain hook), plus the one undoing a
  build that completed after the scope stopped. The deadline bounds how long
  `Stop` waits, never whether the release is owed; the handoff goroutine
  re-enters `stopIfNeeded` with `context.WithoutCancel`, so it cannot recurse.
  Any ordering oracle must model these.
- **CHANGELOG is enforced, and it is the release notes.** `release.yml` fails a
  tag push when `CHANGELOG.md` has no `## [<version>]` section, then publishes
  the release with that section as its body. Re-running edits the notes; a version
  containing `-` is a prerelease; the section ends at the next release heading or
  at the link definitions. The CHANGELOG tracks library behaviour — a docs- or
  site-only change adds no entry. The public API has been stable across tags;
  verify with `go doc -all` diffed between tags before choosing a version.

## Tooling caveats

- Generic methods need gopls **v0.23.0+**. v0.21.1 rejects the code with
  `method must have no type parameters`, then reports cascading phantom errors.
  golangci-lint v2.13.1+ handles them.
- `.golangci.yml` excludes staticcheck QF1011: `var get func() *DB = s.Get` is
  not redundant — the declared type drives Go 1.27 inference for a generic method
  value.
- It also disables SA4023, because golangci-lint v2.13.1's staticcheck *crashes*
  on this package (`index out of range [1] with length 1`, in its nilness
  analysis), taking the whole lint job down with no partial result. The trigger
  moves as the test package grows. Drop the exclusion once upstream is fixed and
  see what SA4023 has to say.
- Bisecting a lint crash needs care: reverting one file can break the build, and
  golangci-lint then reports "0 issues" for a package it never analysed. Check
  the package still compiles at each step.
