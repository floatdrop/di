package di_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/floatdrop/di"
)

// A four-service graph with one interface dependency, built from plain
// constructors that know nothing about di.

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

func wireGraph(s *di.Scope) {
	s.Value(wCfg{"pg"})
	s.Wire[*wDB](newWDB)
	s.Wire[*wRepo](newWRepo)
	s.Wire[wLogger](newWLogger)
}

func TestWireBuildsTheGraph(t *testing.T) {
	s := di.New()
	wireGraph(s)
	s.Wire[*wSvc](newWSvcOK)
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
}

func TestWireZeroDependencies(t *testing.T) {
	s := di.New()
	s.Wire[*wDB](func() *wDB { return &wDB{wCfg{"none"}} })
	if s.Get[*wDB]().cfg.dsn != "none" {
		t.Fatal("zero-arity constructor not called")
	}
}

func TestWireConstructorErrorAbortsTheBuild(t *testing.T) {
	s := di.New()
	wireGraph(s)
	s.Wire[*wSvc](newWSvcErr)
	_, err := s.Resolve[*wSvc]()
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "building") {
		t.Fatalf("want a build error carrying the constructor's error, got %v", err)
	}
}

func TestWireMissingDependencyNamesThePath(t *testing.T) {
	s := di.New()
	s.Wire[*wRepo](newWRepo) // *wDB is not provided
	_, err := s.Resolve[*wRepo]()
	if !errors.Is(err, di.ErrNotProvided) || !strings.Contains(err.Error(), "wDB") {
		t.Fatalf("want ErrNotProvided naming *wDB, got %v", err)
	}
}

func TestWireScopedBuildsPerChild(t *testing.T) {
	s := di.New()
	wireGraph(s)
	s.Wire[*wHandler](newWHandler).Scoped()
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
}

func TestWireNilInterfaceDependency(t *testing.T) {
	s := di.New()
	s.Value(wCfg{"pg"})
	s.Wire[*wDB](newWDB)
	s.Wire[*wRepo](newWRepo)
	s.Provide(func(*di.Scope) wLogger { return nil })
	s.Wire[*wSvc](newWSvc)
	if svc := s.Get[*wSvc](); svc.log != nil {
		t.Fatalf("want nil logger, got %#v", svc.log)
	}
}

func TestWireHooksCompose(t *testing.T) {
	s := di.New()
	wireGraph(s)
	started := false
	s.Wire[*wSvc](newWSvc).Eager().OnStart(func(_ context.Context, v *wSvc) error {
		started = v.repo != nil
		return nil
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("OnStart did not run against the typed value")
	}
}

func TestWireRejectsBadShapes(t *testing.T) {
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

func TestWireServesAnInterfaceFromAConcreteConstructor(t *testing.T) {
	s := di.New()
	s.Value(wCfg{"pg"})
	s.Wire[*wDB](newWDB)
	s.Wire[wLogger](func(db *wDB) *wStdLogger { return &wStdLogger{db} })
	if _, ok := s.Get[wLogger]().(*wStdLogger); !ok {
		t.Fatal("interface not served by the concrete constructor")
	}
}

type wBytes []byte

// A result that is assignable to the key but not identical to it, a chan for
// a receive-only key or a []byte for a named slice, passed registration and
// then failed the assertion in Get, because the stored value kept the
// constructor's type. It is stored as the key's type now, for Wire and for
// Wrap, and it can then be handed to a parameter of that type. (issue 35)
func TestWireStoresAnAssignableResultAsTheKey(t *testing.T) {
	s := di.New()
	s.Wire[<-chan int](func() chan int { return make(chan int, 1) })
	s.Wire[wBytes](func() []byte { return []byte{42} })
	s.Wire[*wSink](func(ch <-chan int, b wBytes) *wSink { return &wSink{} })

	ch, err := s.Resolve[<-chan int]()
	if err != nil || ch == nil {
		t.Fatalf("receive-only channel: %v", err)
	}
	if b, err := s.Resolve[wBytes](); err != nil || len(b) != 1 {
		t.Fatalf("named slice: %v %v", b, err)
	}
	if _, err := s.Resolve[*wSink](); err != nil {
		t.Fatalf("as a parameter: %v", err)
	}

	w := di.New()
	w.Wire[wBytes](func() []byte { return []byte{1} })
	w.Wrap[wBytes](func(b wBytes) []byte { return append(b, 2) })
	if b, err := w.Resolve[wBytes](); err != nil || len(b) != 2 {
		t.Fatalf("a wrapper's result is converted too: %v %v", b, err)
	}
}

type wSink struct{}
