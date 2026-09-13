// Parallel shapes, di only. The comparisons in bench_test.go run on one
// goroutine, which hides contention: a warm resolution that takes a mutex in
// the scope that owns the service costs nothing extra there and serialises
// across cores. Read these at several -cpu values; a per-op figure that grows
// with the CPU count is a lock on the hot path.
package bench

import (
	"context"
	"net/http"
	"testing"

	"github.com/floatdrop/di"
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

// The whole request: open a child, register the request, build the Scoped
// handler over a root singleton, stop the child.
func BenchmarkDI_Parallel_RequestScope(b *testing.B) {
	s := parallelApp()
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	ctx := context.Background()
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
