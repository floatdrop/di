// The same four-service graph in each container, wired the way that container
// is meant to be used.
//
// Two comparisons are fair and one needs a caveat. Cold register-and-build is
// what every container pays at startup, and every one of them is doing the
// same work. A warm resolve is what di and samber/do are also built for, since
// a request scope resolves on the hot path; dig has no typed accessor, so the
// nearest thing is Invoke with a function it reflects over on every call, and
// fx applications resolve once at startup and never again. Read the dig warm
// number as the cost of using dig for something it does not set out to do.
//
// di appears twice because dig's Provide is reflective: Wire is the comparable
// registration, and the Provide closure is what the same graph costs when the
// dependencies are pulled by hand.
package bench

import (
	"testing"

	"github.com/floatdrop/di"
	"github.com/samber/do/v2"
	"go.uber.org/dig"
)

type Cfg struct{ dsn string }
type DB struct{ cfg Cfg }
type Repo struct{ db *DB }
type Svc struct{ repo *Repo }

// Plain constructors, for the reflective registrations.
func newCfg() Cfg          { return Cfg{"x"} }
func newDB(cfg Cfg) *DB    { return &DB{cfg} }
func newRepo(db *DB) *Repo { return &Repo{db} }
func newSvc(r *Repo) *Svc  { return &Svc{r} }

func digContainer(b *testing.B) *dig.Container {
	c := dig.New()
	for _, ctor := range []any{newCfg, newDB, newRepo, newSvc} {
		if err := c.Provide(ctor); err != nil {
			b.Fatal(err)
		}
	}
	return c
}

func BenchmarkDI_Resolve(b *testing.B) {
	s := di.New()
	s.Value(Cfg{"x"})
	s.Provide(func(s *di.Scope) *DB { return &DB{s.Get[Cfg]()} })
	s.Provide(func(s *di.Scope) *Repo { return &Repo{s.Get[*DB]()} })
	s.Provide(func(s *di.Scope) *Svc { return &Svc{s.Get[*Repo]()} })
	s.Get[*Svc]()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.Resolve[*Svc](); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDI_Get(b *testing.B) {
	s := di.New()
	s.Value(Cfg{"x"})
	s.Provide(func(s *di.Scope) *DB { return &DB{s.Get[Cfg]()} })
	s.Provide(func(s *di.Scope) *Repo { return &Repo{s.Get[*DB]()} })
	s.Provide(func(s *di.Scope) *Svc { return &Svc{s.Get[*Repo]()} })
	s.Get[*Svc]()
	b.ReportAllocs()
	for b.Loop() {
		_ = s.Get[*Svc]()
	}
}

func BenchmarkDI_Wire_Get(b *testing.B) {
	s := di.New()
	s.Wire[Cfg](newCfg)
	s.Wire[*DB](newDB)
	s.Wire[*Repo](newRepo)
	s.Wire[*Svc](newSvc)
	s.Get[*Svc]()
	b.ReportAllocs()
	for b.Loop() {
		_ = s.Get[*Svc]()
	}
}

func BenchmarkDo_Invoke(b *testing.B) {
	i := do.New()
	do.ProvideValue(i, Cfg{"x"})
	do.Provide(i, func(i do.Injector) (*DB, error) { return &DB{do.MustInvoke[Cfg](i)}, nil })
	do.Provide(i, func(i do.Injector) (*Repo, error) { return &Repo{do.MustInvoke[*DB](i)}, nil })
	do.Provide(i, func(i do.Injector) (*Svc, error) { return &Svc{do.MustInvoke[*Repo](i)}, nil })
	do.MustInvoke[*Svc](i)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := do.Invoke[*Svc](i); err != nil {
			b.Fatal(err)
		}
	}
}

// dig has no typed accessor: the value comes back through a function dig
// reflects over. The function is hoisted, so this is the cheapest form.
func BenchmarkDig_Invoke(b *testing.B) {
	c := digContainer(b)
	var sink *Svc
	fn := func(s *Svc) { sink = s }
	if err := c.Invoke(fn); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := c.Invoke(fn); err != nil {
			b.Fatal(err)
		}
	}
	_ = sink
}

func BenchmarkDI_ColdBuild(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		s := di.New()
		s.Value(Cfg{"x"})
		s.Provide(func(s *di.Scope) *DB { return &DB{s.Get[Cfg]()} })
		s.Provide(func(s *di.Scope) *Repo { return &Repo{s.Get[*DB]()} })
		s.Provide(func(s *di.Scope) *Svc { return &Svc{s.Get[*Repo]()} })
		_ = s.Get[*Svc]()
	}
}

func BenchmarkDI_Wire_ColdBuild(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		s := di.New()
		s.Wire[Cfg](newCfg)
		s.Wire[*DB](newDB)
		s.Wire[*Repo](newRepo)
		s.Wire[*Svc](newSvc)
		_ = s.Get[*Svc]()
	}
}

func BenchmarkDo_ColdBuild(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		i := do.New()
		do.ProvideValue(i, Cfg{"x"})
		do.Provide(i, func(i do.Injector) (*DB, error) { return &DB{do.MustInvoke[Cfg](i)}, nil })
		do.Provide(i, func(i do.Injector) (*Repo, error) { return &Repo{do.MustInvoke[*DB](i)}, nil })
		do.Provide(i, func(i do.Injector) (*Svc, error) { return &Svc{do.MustInvoke[*Repo](i)}, nil })
		do.MustInvoke[*Svc](i)
	}
}

func BenchmarkDig_ColdBuild(b *testing.B) {
	var sink *Svc
	fn := func(s *Svc) { sink = s }
	b.ReportAllocs()
	for b.Loop() {
		c := digContainer(b)
		if err := c.Invoke(fn); err != nil {
			b.Fatal(err)
		}
	}
	_ = sink
}
