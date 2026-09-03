# benchmarks

Separate module so the library itself stays dependency-free. Compares
`github.com/floatdrop/di` against `samber/do` on the same four-service graph.

```sh
cd benchmarks && go test -bench . -benchmem
```
