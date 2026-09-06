package di_test

import (
	"context"
	"testing"

	"github.com/floatdrop/di"
)

type bCfg struct{ dsn string }
type bDB struct{ cfg bCfg }
type bRepo struct{ db *bDB }
type bSvc struct{ repo *bRepo }
type bReq struct{ id int }
type bHandler struct {
	repo *bRepo
	req  *bReq
}

func newBDB(c bCfg) *bDB                      { return &bDB{c} }
func newBRepo(db *bDB) *bRepo                 { return &bRepo{db} }
func newBSvc(r *bRepo) *bSvc                  { return &bSvc{r} }
func newBHandler(r *bRepo, q *bReq) *bHandler { return &bHandler{r, q} }

// The same graph registered with Provide closures and with Wire, so the cost
// of building through reflect stays measured.
var benchStyles = []struct {
	name    string
	graph   func(s *di.Scope)
	handler func(s *di.Scope)
}{
	{
		name: "Provide",
		graph: func(s *di.Scope) {
			s.Value(bCfg{"x"})
			s.Provide(func(s *di.Scope) *bDB { return newBDB(s.Get[bCfg]()) })
			s.Provide(func(s *di.Scope) *bRepo { return newBRepo(s.Get[*bDB]()) })
			s.Provide(func(s *di.Scope) *bSvc { return newBSvc(s.Get[*bRepo]()) })
		},
		handler: func(s *di.Scope) {
			s.Provide(func(s *di.Scope) *bHandler { return newBHandler(s.Get[*bRepo](), s.Get[*bReq]()) }).Scoped()
		},
	},
	{
		name: "Wire",
		graph: func(s *di.Scope) {
			s.Value(bCfg{"x"})
			s.Wire[*bDB](newBDB)
			s.Wire[*bRepo](newBRepo)
			s.Wire[*bSvc](newBSvc)
		},
		handler: func(s *di.Scope) { s.Wire[*bHandler](newBHandler).Scoped() },
	},
}

// Cold build: register the graph and resolve its root once.
func BenchmarkWireColdBuild(b *testing.B) {
	for _, st := range benchStyles {
		b.Run(st.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				s := di.New()
				st.graph(s)
				_ = s.Get[*bSvc]()
			}
		})
	}
}

// Per-request build: a Scoped handler with two dependencies, built in a
// fresh child each iteration. This is where a reflect.Call recurs, once per
// request per scoped service. Subtract BenchmarkWireScopedBaseline for the
// build alone.
func BenchmarkWireScopedBuild(b *testing.B) {
	for _, st := range benchStyles {
		b.Run(st.name, func(b *testing.B) {
			s := di.New()
			st.graph(s)
			st.handler(s)
			_ = s.Get[*bSvc]()
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				c := s.Child("req")
				c.Value(&bReq{1})
				_ = c.Get[*bHandler]()
				_ = c.Stop(ctx)
			}
		})
	}
}

// The child lifecycle with nothing resolved in it.
func BenchmarkWireScopedBaseline(b *testing.B) {
	s := di.New()
	benchStyles[0].graph(s)
	_ = s.Get[*bSvc]()
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		c := s.Child("req")
		c.Value(&bReq{1})
		_ = c.Stop(ctx)
	}
}
