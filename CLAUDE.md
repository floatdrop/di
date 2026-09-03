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
in the resolving scope and is not tracked at all. `instanceFor` makes this
choice, and the rest of the pipeline works in terms of `holder`.

**The instance phase machine** (`phaseNew` → `Built` → `Starting` →
`Started`/`Failed` → `Stopped`) is read and written *only* under the owning
state's mutex. This exists because splitting a start-or-stop decision across
two critical sections produced several bugs. In particular: `publish` appends
to the stop list and *then* `startIfRunning` reads `running`, while `Start`
sets `running` before it drains. That ordering is what guarantees an instance
is started by exactly one path.

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
  stop, health) across a root and two children, checked against invariants
  taken from documented guarantees rather than predicted values. This is the
  layer that catches error-path and cross-scope bugs.
- `FuzzMachine` — the same invariants under coverage-guided search. Corpus in
  `testdata/fuzz/` is committed; CI runs 90s in its own job.

Neither generator explores goroutine interleavings; concurrency is covered by
stress loops under `-race` (see `TestRegressionStartRace`).

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
