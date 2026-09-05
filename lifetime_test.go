package di_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/floatdrop/di"
)

func TestGroupMembersHaveLifecycle(t *testing.T) {
	var builds atomic.Int32
	var log []string
	s := di.New()
	s.Provide(func(*di.Scope) Handler { builds.Add(1); return Handler{"users"} }).Group().
		OnStart(func(context.Context, Handler) error { log = append(log, "start users"); return nil }).
		OnStop(func(context.Context, Handler) error { log = append(log, "stop users"); return nil })
	s.Provide(func(*di.Scope) Handler { builds.Add(1); return Handler{"orders"} }).Group().
		OnStop(func(context.Context, Handler) error { log = append(log, "stop orders"); return nil })

	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.All[Handler]()
	s.All[Handler]()
	if builds.Load() != 2 {
		t.Fatalf("group members rebuilt: %d builds", builds.Load())
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); got != "start users,stop orders,stop users" {
		t.Fatalf("got %q", got)
	}
}

func TestGroupMemberCycle(t *testing.T) {
	s := di.New()
	s.Provide(func(s *di.Scope) Handler { _ = s.All[Handler](); return Handler{} }).Group()
	defer func() {
		if err, _ := recover().(error); !errors.Is(err, di.ErrCycle) {
			t.Fatalf("got %v", err)
		}
	}()
	s.All[Handler]()
}

func TestGetAfterStopFails(t *testing.T) {
	s := di.New()
	s.Value(&DB{})
	child := s.Child("child")
	s.Get[*DB]()
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve[*DB](); !errors.Is(err, di.ErrStopped) {
		t.Fatalf("root: %v", err)
	}
	if _, err := child.Resolve[*DB](); !errors.Is(err, di.ErrStopped) {
		t.Fatalf("child of a stopped scope: %v", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop must stay idempotent: %v", err)
	}
}

func TestGetAfterFailedStartFails(t *testing.T) {
	s := di.New()
	s.Value(&DB{}).Eager().OnStart(func(context.Context, *DB) error { return errors.New("boom") })
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("expected start failure")
	}
	if _, err := s.Resolve[*DB](); !errors.Is(err, di.ErrStopped) {
		t.Fatalf("got %v", err)
	}
}

func TestLifetimeOnValueIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(di.Binding[*DB])
	}{
		{"Scoped", func(b di.Binding[*DB]) { b.Scoped() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := di.New()
			tc.apply(s.Value(&DB{}))
			// Validation happens when the registration batch is committed,
			// so every combination is rejected in one place regardless of
			// the order the builder methods were called in.
			mustPanic(t, tc.name+" is meaningless", func() { _, _ = s.Resolve[*DB]() })
		})
	}
}
