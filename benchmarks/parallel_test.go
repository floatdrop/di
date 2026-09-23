// Parallel shapes, di only. The comparisons in bench_test.go run on one
// goroutine, which hides contention: a warm resolution that takes a mutex in
// the scope that owns the service costs nothing extra there and serialises
// across cores. Read these at several -cpu values; a per-op figure that grows
// with the CPU count is a lock on the hot path.
package bench

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"golang.yandex/di"
)

// Handler is per request: it needs a root singleton and the request itself.
type Handler struct {
	svc *Svc
	req *http.Request
}

func newHandler(s *Svc, r *http.Request) *Handler { return &Handler{s, r} }

// parallelApp is the four-service graph plus a Scoped handler, warmed.
func parallelApp() *di.Scope {
	s := di.New()
	s.Wire[Cfg](newCfg)
	s.Wire[*DB](newDB)
	s.Wire[*Repo](newRepo)
	s.Wire[*Svc](newSvc)
	s.Wire[*Handler](newHandler).Scoped()
	s.Get[*Svc]()
	return s
}

// A warm Get of a root singleton, made directly on the root.
func BenchmarkDI_Parallel_RootGet(b *testing.B) {
	s := parallelApp()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = s.Get[*Svc]()
		}
	})
}

// A warm Get of a root singleton through a child scope held per goroutine,
// which is what a handler resolving an application service does.
func BenchmarkDI_Parallel_ChildGet(b *testing.B) {
	s := parallelApp()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		c := s.Child("request")
		c.Get[*Svc]()
		for pb.Next() {
			_ = c.Get[*Svc]()
		}
	})
}

// A warm Get of a root singleton through a child scope held per goroutine
// under one shared middle scope, as a tenant or module scope between the
// application and its requests would be.
func BenchmarkDI_Parallel_NestedGet(b *testing.B) {
	s := parallelApp()
	mid := s.Child("tenant")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		c := mid.Child("request")
		c.Get[*Svc]()
		for pb.Next() {
			_ = c.Get[*Svc]()
		}
	})
}

// The whole request: open a child, register the request, build the Scoped
// handler over a root singleton, stop the child.
func BenchmarkDI_Parallel_RequestScope(b *testing.B) {
	s := parallelApp()
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	ctx := b.Context()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c := s.Child("request")
			c.Value(req)
			_ = c.Get[*Handler]()
			if err := c.Stop(ctx); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

// A herd of resolutions arriving at one slow constructor. Each one that has
// to wait searches the container's wait-for graph for a cycle before it
// blocks, under the container's one lock, so what matters is how that search
// grows with the number already waiting. The sleep lets the herd gather
// before the constructor returns and is the same in every run; read the
// figures against each other, not as the cost of one resolution.
// The
// smallest herd mostly measures the sleep.
func benchmarkHerd(b *testing.B, n int) {
	for b.Loop() {
		s := di.New()
		release := make(chan struct{})
		var entered sync.WaitGroup
		entered.Add(1)
		s.Provide(func(*di.Scope) *DB { entered.Done(); <-release; return &DB{} })
		var wg sync.WaitGroup
		wg.Go(func() { _, _ = s.Resolve[*DB]() })
		entered.Wait()
		for range n - 1 {
			wg.Go(func() { _, _ = s.Resolve[*DB]() })
		}
		time.Sleep(2 * time.Millisecond)
		close(release)
		wg.Wait()
	}
}

func BenchmarkDI_Herd_256(b *testing.B)  { benchmarkHerd(b, 256) }
func BenchmarkDI_Herd_2048(b *testing.B) { benchmarkHerd(b, 2048) }
func BenchmarkDI_Herd_8192(b *testing.B) { benchmarkHerd(b, 8192) }
