package di_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/floatdrop/di"
)

// Fixtures for the Wire prototype: a four-service graph with one interface
// dependency, built from plain constructors that know nothing about di.

type wCfg struct{ dsn string }
type wDB struct{ cfg wCfg }
type wRepo struct{ db *wDB }
type wLogger interface{ Log(string) }
type wStdLogger struct{ db *wDB }
type wSvc struct {
	repo *wRepo
	log  wLogger
}
type wReq struct{ id int }
type wHandler struct {
	repo *wRepo
	req  *wReq
}

func (*wStdLogger) Log(string) {}

func newWDB(cfg wCfg) *wDB                         { return &wDB{cfg} }
func newWRepo(db *wDB) *wRepo                      { return &wRepo{db} }
func newWLogger(db *wDB) wLogger                   { return &wStdLogger{db} }
func newWSvc(r *wRepo, l wLogger) *wSvc            { return &wSvc{r, l} }
func newWHandler(r *wRepo, q *wReq) *wHandler      { return &wHandler{r, q} }
func newWSvcErr(*wRepo, wLogger) (*wSvc, error)    { return nil, errors.New("boom") }
func newWSvcOK(r *wRepo, l wLogger) (*wSvc, error) { return &wSvc{r, l}, nil }

// wireStyles registers the same graph through each registration form.
var wireStyles = []struct {
	name    string
	graph   func(s *di.Scope)
	failing func(s *di.Scope)
	handler func(s *di.Scope)
}{
	{
		name: "typed",
		graph: func(s *di.Scope) {
			s.Value(wCfg{"pg"})
			s.Wire1(newWDB)
			s.Wire1(newWRepo)
			s.Wire1(newWLogger)
			s.Wire2E(newWSvcOK)
		},
		failing: func(s *di.Scope) { s.Wire2E(newWSvcErr) },
		handler: func(s *di.Scope) { s.Wire2(newWHandler).Scoped() },
	},
	{
		name: "reflective",
		graph: func(s *di.Scope) {
			s.Value(wCfg{"pg"})
			s.Wire[*wDB](newWDB)
			s.Wire[*wRepo](newWRepo)
			s.Wire[wLogger](newWLogger)
			s.Wire[*wSvc](newWSvcOK)
		},
		failing: func(s *di.Scope) { s.Wire[*wSvc](newWSvcErr) },
		handler: func(s *di.Scope) { s.Wire[*wHandler](newWHandler).Scoped() },
	},
}

func TestWireBuildsTheGraph(t *testing.T) {
	for _, st := range wireStyles {
		t.Run(st.name, func(t *testing.T) {
			s := di.New()
			st.graph(s)
			svc := s.Get[*wSvc]()
			if svc.repo == nil || svc.repo.db == nil || svc.repo.db.cfg.dsn != "pg" {
				t.Fatalf("dependencies not threaded: %+v", svc)
			}
			if l, ok := svc.log.(*wStdLogger); !ok || l.db != svc.repo.db {
				t.Fatalf("interface dependency not shared: %#v", svc.log)
			}
			if s.Get[*wSvc]() != svc {
				t.Fatal("not a singleton")
			}
		})
	}
}

func TestWireConstructorErrorAbortsTheBuild(t *testing.T) {
	for _, st := range wireStyles {
		t.Run(st.name, func(t *testing.T) {
			s := di.New()
			s.Value(wCfg{"pg"})
			s.Wire1(newWDB)
			s.Wire1(newWRepo)
			s.Wire1(newWLogger)
			st.failing(s)
			_, err := s.Resolve[*wSvc]()
			if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "building") {
				t.Fatalf("want a build error carrying the constructor's error, got %v", err)
			}
		})
	}
}

func TestWireScopedBuildsPerChild(t *testing.T) {
	for _, st := range wireStyles {
		t.Run(st.name, func(t *testing.T) {
			s := di.New()
			st.graph(s)
			st.handler(s)
			var hs [2]*wHandler
			for i := range hs {
				c := s.Child("req")
				c.Value(&wReq{i})
				hs[i] = c.Get[*wHandler]()
				if hs[i].req.id != i {
					t.Fatalf("handler %d saw request %d", i, hs[i].req.id)
				}
			}
			if hs[0] == hs[1] || hs[0].repo != hs[1].repo {
				t.Fatal("scoped handler must differ per child and share the singleton repo")
			}
		})
	}
}

func TestWireNilInterfaceDependency(t *testing.T) {
	for _, st := range wireStyles {
		t.Run(st.name, func(t *testing.T) {
			s := di.New()
			s.Value(wCfg{"pg"})
			s.Wire1(newWDB)
			s.Wire1(newWRepo)
			s.Provide(func(*di.Scope) wLogger { return nil })
			if st.name == "typed" {
				s.Wire2(newWSvc)
			} else {
				s.Wire[*wSvc](newWSvc)
			}
			if svc := s.Get[*wSvc](); svc.log != nil {
				t.Fatalf("want nil logger, got %#v", svc.log)
			}
		})
	}
}

func TestWireHooksCompose(t *testing.T) {
	for _, st := range wireStyles {
		t.Run(st.name, func(t *testing.T) {
			s := di.New()
			s.Value(wCfg{"pg"})
			s.Wire1(newWDB)
			s.Wire1(newWRepo)
			s.Wire1(newWLogger)
			started := false
			var b di.Binding[*wSvc]
			if st.name == "typed" {
				b = s.Wire2(newWSvc)
			} else {
				b = s.Wire[*wSvc](newWSvc)
			}
			b.Eager().OnStart(func(_ context.Context, v *wSvc) error {
				started = v.repo != nil
				return nil
			})
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !started {
				t.Fatal("OnStart did not run against the typed value")
			}
		})
	}
}

func TestReflectiveWireRejectsBadShapes(t *testing.T) {
	cases := map[string]func(s *di.Scope){
		"not a function":    func(s *di.Scope) { s.Wire[*wDB](42) },
		"nil":               func(s *di.Scope) { s.Wire[*wDB](nil) },
		"variadic":          func(s *di.Scope) { s.Wire[*wDB](func(...wCfg) *wDB { return nil }) },
		"no result":         func(s *di.Scope) { s.Wire[*wDB](func(wCfg) {}) },
		"three results":     func(s *di.Scope) { s.Wire[*wDB](func() (*wDB, error, bool) { return nil, nil, false }) },
		"wrong result type": func(s *di.Scope) { s.Wire[*wDB](newWRepo) },
		"second not error":  func(s *di.Scope) { s.Wire[*wDB](func() (*wDB, bool) { return nil, false }) },
	}
	for name, reg := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				msg, ok := recover().(string)
				if !ok || !strings.HasPrefix(msg, "di: Wire[") {
					t.Fatalf("want a di: configuration panic, got %v", msg)
				}
			}()
			reg(di.New())
			t.Fatal("registration accepted")
		})
	}
}

func TestReflectiveWireAcceptsAssignableResult(t *testing.T) {
	s := di.New()
	s.Value(wCfg{"pg"})
	s.Wire[*wDB](newWDB)
	s.Wire[wLogger](func(db *wDB) *wStdLogger { return &wStdLogger{db} }) // concrete result for an interface key
	if _, ok := s.Get[wLogger]().(*wStdLogger); !ok {
		t.Fatal("interface not served by the concrete constructor")
	}
}
