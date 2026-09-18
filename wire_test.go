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
	if err := s.Start(t.Context()); err != nil {
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

// A result assignable to the key but not identical to it, a chan for a
// receive-only key or a []byte for a named slice, is stored as the key's
// type, for Wire and for Wrap, so Get's assertion and a parameter of that
// type both accept it. (issue 35)
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

// Needs fills the two parameters a plain constructor cannot declare: a group
// and an optional dependency. The constructor imports nothing. (fx review)

type wRoute struct{ path string }
type wTracer struct{}
type wRouter struct {
	routes []wRoute
	tracer *wTracer
}

func newWRouter(rs []wRoute, t *wTracer) *wRouter { return &wRouter{rs, t} }

func TestNeedsFillsAGroupAndAnOptional(t *testing.T) {
	s := di.New()
	s.Wire[wRoute](func() wRoute { return wRoute{"/a"} }).Group()
	s.Wire[wRoute](func() wRoute { return wRoute{"/b"} }).Group()
	s.Wire[*wRouter](newWRouter).Needs(di.AllOf[wRoute](), di.Optional[*wTracer]())

	r, err := s.Resolve[*wRouter]()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.routes) != 2 || r.routes[0].path != "/a" || r.routes[1].path != "/b" {
		t.Fatalf("routes: %v", r.routes)
	}
	if r.tracer != nil {
		t.Fatalf("nothing provides *wTracer, so it should be nil: %v", r.tracer)
	}

	// The same graph with the optional provided.
	s2 := di.New()
	s2.Wire[wRoute](func() wRoute { return wRoute{"/a"} }).Group()
	s2.Value(&wTracer{})
	s2.Wire[*wRouter](newWRouter).Needs(di.AllOf[wRoute](), di.Optional[*wTracer]())
	if r, err := s2.Resolve[*wRouter](); err != nil || r.tracer == nil {
		t.Fatalf("the optional was provided: %v %v", r, err)
	}
}

// An empty group is not a failure, and the parameter is the nil slice All
// returns. (fx review)
func TestNeedsAllOfAnEmptyGroup(t *testing.T) {
	s := di.New()
	s.Wire[*wRouter](newWRouter).Needs(di.AllOf[wRoute](), di.Optional[*wTracer]())
	r, err := s.Resolve[*wRouter]()
	if err != nil {
		t.Fatal(err)
	}
	if r.routes != nil {
		t.Fatalf("an empty group should be the nil slice, got %#v", r.routes)
	}
}

// A group parameter is read where the constructor runs, so a Scoped consumer
// sees the members its own scope adds. (fx review)
func TestNeedsAllOfReadsFromTheBuildingScope(t *testing.T) {
	root := di.New()
	root.Wire[wRoute](func() wRoute { return wRoute{"root"} }).Group()
	root.Wire[*wRouter](newWRouter).Scoped().
		Needs(di.AllOf[wRoute](), di.Optional[*wTracer]())

	child := root.Child("child")
	child.Wire[wRoute](func() wRoute { return wRoute{"child"} }).Group()
	r, err := child.Resolve[*wRouter]()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.routes) != 2 {
		t.Fatalf("the child's own member is missing: %v", r.routes)
	}
	if rr, err := root.Resolve[*wRouter](); err != nil || len(rr.routes) != 1 {
		t.Fatalf("the root must not see the child's member: %v %v", rr, err)
	}
}

// Needs is matched by type, so it is rejected when nothing matches, when one
// parameter is described twice, and on a registration with no declared
// parameters at all. (fx review)
func TestNeedsRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		wire func(*di.Scope)
	}{
		{"no such parameter", "matches no *github.com/floatdrop/di_test.wTracer parameter", func(s *di.Scope) {
			s.Wire[*wRepo](newWRepo).Needs(di.Optional[*wTracer]())
		}},
		{"group needs a slice parameter", "matches no []di_test.wRoute parameter", func(s *di.Scope) {
			s.Wire[*wRepo](newWRepo).Needs(di.AllOf[wRoute]())
		}},
		{"twice over", "already resolved as Optional", func(s *di.Scope) {
			s.Wire[*wRouter](newWRouter).
				Needs(di.Optional[*wTracer](), di.Optional[*wTracer]())
		}},
		{"on a closure", "applies to a Wire or Wrap constructor", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *wDB { return &wDB{} }).Needs(di.Optional[*wTracer]())
		}},
		{"on a value", "applies to a Wire or Wrap constructor", func(s *di.Scope) {
			s.Value(&wDB{}).Needs(di.Optional[*wTracer]())
		}},
		{"the zero Need", "the zero Need", func(s *di.Scope) {
			s.Wire[*wRouter](newWRouter).Needs(di.Need{})
		}},
		{"two parameters of one type", "which type cannot tell apart", func(s *di.Scope) {
			s.Wire[*wDB](func(a, b *wTracer) *wDB { return &wDB{} }).
				Needs(di.Optional[*wTracer]())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mustPanic(t, tc.want, func() { tc.wire(di.New()) })
		})
	}
}

// A wrapper's parameters go through the same path, so Needs applies there
// too, past the value being wrapped. (fx review)
func TestNeedsOnAWrapper(t *testing.T) {
	s := di.New()
	s.Wire[wRoute](func() wRoute { return wRoute{"/a"} }).Group()
	s.Value(&wRouter{})
	s.Wrap[*wRouter](func(next *wRouter, rs []wRoute, t *wTracer) *wRouter {
		return &wRouter{append(next.routes, rs...), t}
	}).Needs(di.AllOf[wRoute](), di.Optional[*wTracer]())

	r, err := s.Resolve[*wRouter]()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.routes) != 1 || r.tracer != nil {
		t.Fatalf("the wrapper's needs were not filled: %#v", r)
	}
}
