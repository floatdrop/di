# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

`github.com/floatdrop/di` is a dependency-injection container for Go 1.27+ built
on generic methods. The whole library is `di.go`; everything else is tests,
examples, and a separate benchmarks module.

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
resolves it (`holder`), so it can see that scope's values; a `Transient` builds
in the resolving scope and is not tracked at all. `resolve` picks the holder
and the rest of the pipeline works in terms of it.

**The instance phase machine** (`phaseNew` → `Building` → `Built` →
`Starting` → `Started`/`Failed` → `Stopped`) is read and written *only* under
the owning state's mutex. This exists because splitting a start-or-stop
decision across two critical sections produced several bugs. In particular:
`publish` appends to the stop list and *then* `startIfRunning` reads
`running`, while `Start` sets `running` before it drains. That ordering is
what guarantees an instance is started by exactly one path.

`await` is the only way to reach an instance. It claims the build step or
waits on the holder's `sync.Cond` for whoever did, and it also waits out
`phaseStarting`, which is what stops a resolution handing back a service
whose `OnStart` is still running. `settled` says the build step is over and
`value`/`err` are final; it replaced a `sync.Once`, which could only tell a
second resolution to carry on, never to wait.

**Two cycle detectors, because one branch cannot see the other.** Within a
branch, `resolver.onPath` walks the immutable path. Across branches,
`resolver.wait` searches a wait-for graph under `graphMu` before blocking:
instances point at the resolution building them, blocked resolutions point at
what they wait for, and a branch that blocks does so several nodes below the
one holding the build, so both directions are matched against whole paths
(`descends`). The check and the edge it adds are one critical section, or two
branches closing a cycle at once would both decide to wait. Lock order is
state mutex then `graphMu`, never the reverse.

**The resolution path is immutable.** `resolver` is a linked list node, not a
slice, because a constructor may resolve from several goroutines and they all
share the `*Scope` it was handed. A node is identified by binding *and*
holder, never by key: that is what separates a group member from a plain
registration of the same type, and one `Scoped` binding across scopes.

**Scopes have a stop machine too.** `stopDone` is made by the first `Stop`
and closed when its teardown finishes; every later or concurrent `Stop` waits
on it and reports `stopErr`. That wait is what keeps dependency order when a
child and its parent are stopped at once. The cost is that a hook may not call
`Stop` on its own scope or an ancestor.

**Draining precedes everything.** `Stop` is drain, then mark stopped, then
children, then this scope's instances. `drain` skips a child whose `Stop` has
already begun, because that `Stop` drained it; without the skip, `OnDrain`
could run on an instance the child is tearing down. Draining is not otherwise
ordered against such a child, and does not need to be, since it releases
nothing.

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
between the resolver and that owner, and does the same for each alias hop.
Both halves matter: an earlier version marked only the endpoint, and a scope
in the middle, or one holding an alias's target, could then shadow a key it
had already handed out.

**Aliases are resolved in `lookup`, not by callers.** `lookup` follows `Bind`
chains and reports the binding that actually serves a key, never an alias.
`resolve` has a defensive panic if it ever receives one. This is structural: an
earlier version relied on three call sites remembering to follow aliases, and
one forgot.

**Errors versus panics.** A *wiring* failure (missing dependency, cycle, failed
constructor) is an internal `abort{err}` panic that unwinds to the nearest
`Resolve`/`Start`/`Run` and becomes an `error`. A *configuration* rejection
(contradictory lifetimes, re-registering a resolved key) is a plain `panic`
with a string prefixed `di: `. So `Resolve` never panics, `Get` panics with an
`error` at top level, and config errors panic with a string. Tests and the fuzz
harness classify panics by that rule.

**When a stop is owed.** `OnStop` runs when `OnStart` succeeded, or when there
is no `OnStart` to pair with (or the scope was never started), making `OnStop` a
plain destructor. A service built but never started is *not* torn down.
`binding.used` is set only when a resolution actually served a value, and
`state.served` records keys a scope resolved from an *outer* scope, because
`used` lives on the outer binding and cannot protect the inner scope.

## Invariants that are easy to break

- Never set a phase or read `running`/`stopped` for a decision outside the
  owning state's mutex.
- `Stop` must not wait on anything the current goroutine could be responsible
  for finishing. A start hook may call `Stop`; that is why a mid-start teardown
  is handed off via `stopWanted` rather than waited on.
- `Start`'s rollback goes through `Stop` with `context.WithoutCancel`, so it
  stops child scopes and waits for `Run` hooks.
- Whichever `Stop` call queues a handoff owns that teardown's context; a later
  `Stop` must not clobber it.

## Testing strategy

Four layers, each catching a different class:

- `regression_test.go` — one test per historical defect. **Verify a new one
  fails against the commit that preceded the fix**, e.g. by restoring the old
  `di.go` from git and running just that test. Several tests written here
  turned out to pass both before and after; say so rather than implying
  coverage.
- `property_test.go` — random *registration* sequences checked against a model
  of the eager/alias rules. A predictive model can be wrong in the same way as
  the code, so treat it as needing its own scrutiny.
- `machine_test.go` — random *operation* sequences (register, resolve, start,
  stop, health) across a root, two children and a grandchild, checked against
  invariants taken from documented guarantees rather than predicted values.
  This is the layer that catches error-path and cross-scope bugs. Keep the
  I4 exemptions narrow: exempting every aliased key from value stability,
  rather than only one whose target is `Transient`, is what hid a scope
  handing out two live values for one interface.
- `concurrent_test.go` — the same operations run in parallel lanes under
  `-race`, in phases (wire, resolve, start, stop). It checks only what
  survives concurrency: nothing panics unexpectedly, every operation returns,
  nothing is stopped more often than built, and the stop-order oracle, which
  is the layer that catches a parent running ahead of a child.
- `review_test.go` — one test per defect of the September 2026 review.
- `FuzzMachine` — the same invariants under coverage-guided search. Corpus in
  `testdata/fuzz/` is committed; CI runs 90s in its own job.

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
- **Two teardowns may finish after `Stop` returns**: one handed off because a
  start step was in flight, and one undoing a build that completed after the
  scope stopped. Both are deliberate. Any ordering oracle has to model them
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
