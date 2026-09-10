# benchmarks

Separate module so the library itself stays dependency-free. Compares
`github.com/floatdrop/di` against `samber/do` and `go.uber.org/dig` on the
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

```sh
cd benchmarks && go test -bench . -benchmem
```
