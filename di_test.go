package di_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/floatdrop/di"
)

type Config struct{ DSN string }
type DB struct{ dsn string }
type Repo struct{ db *DB }
type Reader interface{ Read() string }

func (r *Repo) Read() string { return r.db.dsn }

type Handler struct{ name string }

func newApp() *di.Scope {
	s := di.New()
	s.Value(Config{DSN: "pg://primary"})
	s.Provide(func(s *di.Scope) *DB { return &DB{dsn: s.Get[Config]().DSN} })
	s.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} })
	return s
}

func TestResolveChain(t *testing.T) {
	s := newApp()
	r, err := s.Resolve[*Repo]()
	if err != nil {
		t.Fatal(err)
	}
	if r.Read() != "pg://primary" {
		t.Fatalf("got %q", r.Read())
	}
	if s.Get[*DB]() != r.db {
		t.Fatal("singleton was built twice")
	}
}

func TestNamed(t *testing.T) {
	s := newApp()
	s.Provide(func(*di.Scope) *DB { return &DB{dsn: "pg://replica"} }).Named("replica")
	if got := s.Lookup(di.Named[*DB]("replica")).dsn; got != "pg://replica" {
		t.Fatalf("replica: %q", got)
	}
	if got := s.Get[*DB]().dsn; got != "pg://primary" {
		t.Fatalf("unnamed binding was displaced: %q", got)
	}
}

func TestBind(t *testing.T) {
	s := newApp()
	s.Bind[Reader, *Repo]()
	if s.Get[Reader]().Read() != "pg://primary" {
		t.Fatal("alias did not resolve")
	}
	if any(s.Get[Reader]()) != any(s.Get[*Repo]()) {
		t.Fatal("alias should share the target singleton")
	}
}

func TestBindRejectsNonImplementer(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "does not implement") {
			t.Fatalf("expected implements panic, got %v", r)
		}
	}()
	di.New().Bind[Reader, *DB]()
}

func TestGroups(t *testing.T) {
	s := di.New()
	s.Add(func(*di.Scope) Handler { return Handler{"users"} })
	s.Add(func(*di.Scope) Handler { return Handler{"orders"} })
	child := s.Child("child")
	child.Add(func(*di.Scope) Handler { return Handler{"admin"} })
	got := child.All[Handler]()
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	if len(s.All[Handler]()) != 2 {
		t.Fatal("parent must not see child group members")
	}
}

func TestMaybe(t *testing.T) {
	s := newApp()
	if _, ok := s.Maybe[string](); ok {
		t.Fatal("string is not provided")
	}
	if db, ok := s.Maybe[*DB](); !ok || db.dsn != "pg://primary" {
		t.Fatal("DB should be found")
	}
}

func TestMissingDependency(t *testing.T) {
	s := di.New()
	s.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} })
	_, err := s.Resolve[*Repo]()
	if !errors.Is(err, di.ErrNotProvided) {
		t.Fatalf("want ErrNotProvided, got %v", err)
	}
	for _, want := range []string{"di_test.DB", "needed by", "di_test.Repo", "di_test.go:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestCycle(t *testing.T) {
	type A struct{ b any }
	type B struct{ a any }
	s := di.New()
	s.Provide(func(s *di.Scope) *A { return &A{s.Get[*B]()} })
	s.Provide(func(s *di.Scope) *B { return &B{s.Get[*A]()} })
	_, err := s.Resolve[*A]()
	if !errors.Is(err, di.ErrCycle) {
		t.Fatalf("want ErrCycle, got %v", err)
	}
}

func TestCycleThroughAlias(t *testing.T) {
	s := di.New()
	s.Provide(func(s *di.Scope) *Repo { return &Repo{db: &DB{dsn: s.Get[Reader]().Read()}} })
	s.Bind[Reader, *Repo]()
	_, err := s.Resolve[*Repo]()
	if !errors.Is(err, di.ErrCycle) {
		t.Fatalf("want ErrCycle, got %v", err)
	}
}

func TestTopLevelGetPanicsWithError(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		if err, ok := r.(error); !ok || !errors.Is(err, di.ErrNotProvided) {
			t.Fatalf("panic value should be the error, got %#v", r)
		}
	}()
	di.New().Get[*DB]()
}

func TestConstructorPanicBecomesError(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *DB { panic("boom") })
	_, err := s.Resolve[*DB]()
	if err == nil || !strings.Contains(err.Error(), "panic: boom") {
		t.Fatalf("got %v", err)
	}
}

func TestChildOverride(t *testing.T) {
	s := newApp()
	req := s.Child("request")
	req.Value(&DB{dsn: "sqlite://memory"})
	req.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} })
	if got := req.Get[*Repo]().Read(); got != "sqlite://memory" {
		t.Fatalf("child: %q", got)
	}
	if got := s.Get[*Repo]().Read(); got != "pg://primary" {
		t.Fatalf("root: %q", got)
	}
}

func TestChildSeesParentSingletons(t *testing.T) {
	s := newApp()
	child := s.Child("child")
	if child.Get[*DB]() != s.Get[*DB]() {
		t.Fatal("child must reuse the parent's singleton")
	}
}

func TestTransient(t *testing.T) {
	s := di.New()
	var n atomic.Int32
	s.Provide(func(*di.Scope) *DB { n.Add(1); return &DB{} }).Transient()
	a, b := s.Get[*DB](), s.Get[*DB]()
	if a == b || n.Load() != 2 {
		t.Fatal("transient must build a new instance per Get")
	}
}

func TestLifecycleOrder(t *testing.T) {
	var log []string
	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).
		OnStart(func(context.Context, *DB) error { log = append(log, "start db"); return nil }).
		OnStop(func(context.Context, *DB) error { log = append(log, "stop db"); return nil })
	s.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} }).Eager().
		OnStart(func(context.Context, *Repo) error { log = append(log, "start repo"); return nil }).
		OnStop(func(context.Context, *Repo) error { log = append(log, "stop repo"); return nil })

	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "start db,start repo,stop repo,stop db"
	if got := strings.Join(log, ","); got != want {
		t.Fatalf("order %q, want %q", got, want)
	}
}

func TestStopCollectsErrors(t *testing.T) {
	s := di.New()
	e1, e2 := errors.New("e1"), errors.New("e2")
	s.Provide(func(*di.Scope) *DB { return &DB{} }).OnStop(func(context.Context, *DB) error { return e1 })
	s.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} }).OnStop(func(context.Context, *Repo) error { return e2 })
	s.Get[*Repo]()
	err := s.Stop(context.Background())
	if !errors.Is(err, e1) || !errors.Is(err, e2) {
		t.Fatalf("want both errors, got %v", err)
	}
}

func TestStartReportsMissingEagerDependency(t *testing.T) {
	s := di.New()
	s.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} }).Eager()
	if err := s.Start(context.Background()); !errors.Is(err, di.ErrNotProvided) {
		t.Fatalf("got %v", err)
	}
}

func TestModifyAfterFreezePanics(t *testing.T) {
	s := di.New()
	b := s.Value(&DB{})
	s.Get[*DB]()
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "modified after") {
			t.Fatalf("got %v", r)
		}
	}()
	b.Named("late")
}

func TestLateRegistrationAfterFreeze(t *testing.T) {
	s := di.New()
	s.Value(&DB{dsn: "a"})
	s.Get[*DB]()
	s.Value(Config{DSN: "b"})
	if s.Get[Config]().DSN != "b" {
		t.Fatal("registration after first resolve must be visible")
	}
}

func TestMethodValueInference(t *testing.T) {
	s := di.New()
	s.Value(&DB{dsn: "x"})
	var get func() *DB = s.Get // Go 1.27 infers T from the target function type
	if get().dsn != "x" {
		t.Fatal("bad")
	}
	var resolve func() (*DB, error) = s.Resolve
	if _, err := resolve(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentResolutionBuildsOnce(t *testing.T) {
	s := di.New()
	var builds atomic.Int32
	s.Provide(func(*di.Scope) *DB { builds.Add(1); return &DB{} })
	s.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} })
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			if _, err := s.Resolve[*Repo](); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if builds.Load() != 1 {
		t.Fatalf("built %d times", builds.Load())
	}
}
