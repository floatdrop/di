package di_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/floatdrop/di"
)

func TestStoppedChildIsDetached(t *testing.T) {
	stops := 0
	root := di.New()
	for range 100 {
		c := root.Child("request")
		c.Value(&DB{}).OnStop(func(context.Context, *DB) error { stops++; return nil })
		c.Get[*DB]()
		if err := c.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if stops != 100 {
		t.Fatalf("child stops = %d", stops)
	}
	if err := root.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stops != 100 {
		t.Fatalf("stopped children were stopped again by the parent: %d", stops)
	}
}

func TestLateBuiltServiceStarts(t *testing.T) {
	var log []string
	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).
		OnStart(func(context.Context, *DB) error { log = append(log, "start db"); return nil }).
		OnStop(func(context.Context, *DB) error { log = append(log, "stop db"); return nil })
	s.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} }).
		OnStart(func(context.Context, *Repo) error { log = append(log, "start repo"); return nil }).
		OnStop(func(context.Context, *Repo) error { log = append(log, "stop repo"); return nil })

	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(log) != 0 {
		t.Fatalf("nothing is eager, log=%v", log)
	}
	s.Get[*Repo]() // built after Start: dependencies start before dependents
	s.Get[*Repo]() // cached: hooks must not run again
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "start db,start repo,stop repo,stop db"
	if got := strings.Join(log, ","); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLateStartFailureIsAnError(t *testing.T) {
	boom := errors.New("boom")
	stopped := false
	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).
		OnStart(func(context.Context, *DB) error { return boom }).
		OnStop(func(context.Context, *DB) error { stopped = true; return nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve[*DB](); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Resolve[*DB](); !errors.Is(err, boom) {
		t.Fatalf("failure must stick: %v", err)
	}
	if err := s.Stop(context.Background()); err != nil || stopped {
		t.Fatalf("a service that failed to start must not be stopped (stopped=%v err=%v)", stopped, err)
	}
}

func TestChildOfRunningAppStartsLateServices(t *testing.T) {
	started := false
	root := di.New()
	if err := root.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	req := root.Child("request")
	req.Provide(func(*di.Scope) *DB { return &DB{} }).
		OnStart(func(context.Context, *DB) error { started = true; return nil })
	req.Get[*DB]()
	if !started {
		t.Fatal("OnStart did not run for a service built in a child of a running app")
	}
}

func TestStartTwice(t *testing.T) {
	s := di.New()
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("second Start must fail")
	}
}

func TestMustInsideConstructor(t *testing.T) {
	boom := errors.New("dial failed")
	open := func() (*DB, error) { return nil, boom }
	s := di.New()
	s.Provide(func(s *di.Scope) *DB { return s.Must(open()) })
	s.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} })
	_, err := s.Resolve[*Repo]()
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "building *github.com/floatdrop/di_test.DB") {
		t.Fatalf("error should name the failing service: %v", err)
	}
	if v := s.Must(42, nil); v != 42 {
		t.Fatal("Must must pass values through")
	}
}

func TestMustAtTopLevelPanicsWithError(t *testing.T) {
	boom := errors.New("boom")
	defer func() {
		if r := recover(); r != boom {
			t.Fatalf("got %v", r)
		}
	}()
	di.New().Must(0, boom)
}

type ctxKey struct{}

func TestContextInConstructor(t *testing.T) {
	s := di.New()
	if s.Context() != context.Background() {
		t.Fatal("before Start, Context must be Background")
	}
	var seen any
	s.Provide(func(s *di.Scope) *DB { seen = s.Context().Value(ctxKey{}); return &DB{} }).Eager()
	ctx := context.WithValue(context.Background(), ctxKey{}, "from-start")
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if seen != "from-start" {
		t.Fatalf("constructor saw %v", seen)
	}
	child := s.Child("child")
	if child.Context().Value(ctxKey{}) != "from-start" {
		t.Fatal("child must inherit the start context")
	}
}
