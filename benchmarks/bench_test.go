package bench

import (
	"testing"

	"github.com/floatdrop/di"
	"github.com/samber/do/v2"
)

type Cfg struct{ dsn string }
type DB struct{ cfg Cfg }
type Repo struct{ db *DB }
type Svc struct{ repo *Repo }

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
