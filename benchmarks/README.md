# benchmarks

Separate module so the library itself stays dependency-free. Compares
`golang.yandex/di` against `samber/do` and `go.uber.org/dig` on the
same four-service graph, each wired the way that container is meant to be
used.

`di` appears twice: `dig.Provide` is reflective, so `Wire` is the comparable
registration, and the `Provide` closure is what the same graph costs when the
dependencies are pulled by hand.

Cold register-and-build is a like-for-like comparison. A warm resolve is not,
for dig: it has no typed accessor, so the nearest thing is `Invoke` with a
function dig reflects over on every call, and an fx application resolves once
at startup rather than per request. The package comment in `bench_test.go`
says the same thing; do not quote the dig warm figure without it.

`parallel_test.go` measures `di` alone under `RunParallel`: a warm `Get` on
the root, the same through a child scope, and a whole request scope. Read it
at several `-cpu` values. A per-op figure that grows with the CPU count is a
lock on the hot path, which the single-goroutine comparisons cannot show.

```sh
cd benchmarks && go test -bench . -benchmem
cd benchmarks && go test -bench Parallel -benchmem -cpu 1,4,8
```
