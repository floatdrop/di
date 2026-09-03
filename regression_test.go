package di_test

// Regression tests for the twelve defects found in the September 2026 review.
// Each one reproduced against the code before the instance-phase refactor.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floatdrop/di"
)

type rA struct{}
type rB struct{}
type rC struct{}
type rD struct{}

func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic containing %q", want)
		}
		if msg, _ := r.(string); !strings.Contains(msg, want) {
			t.Fatalf("panic %v does not contain %q", r, want)
		}
	}()
	fn()
}

// 1. An instance built concurrently with Start must be started by exactly one
// of the two paths, never neither.
func TestRegressionStartRace(t *testing.T) {
	for i := range 3000 {
		s := di.New()
		var started, stopped atomic.Int32
		up := func(context.Context, any) error { started.Add(1); return nil }
		down := func(context.Context, any) error { stopped.Add(1); return nil }
		s.Provide(func(*di.Scope) *rA { return &rA{} }).
			OnStart(func(ctx context.Context, v *rA) error { return up(ctx, v) }).
			OnStop(func(ctx context.Context, v *rA) error { return down(ctx, v) })
		s.Provide(func(*di.Scope) *rB { return &rB{} }).
			OnStart(func(ctx context.Context, v *rB) error { return up(ctx, v) }).
			OnStop(func(ctx context.Context, v *rB) error { return down(ctx, v) })
		s.Provide(func(*di.Scope) *rC { return &rC{} }).
			OnStart(func(ctx context.Context, v *rC) error { return up(ctx, v) }).
			OnStop(func(ctx context.Context, v *rC) error { return down(ctx, v) })
		s.Provide(func(*di.Scope) *rD { return &rD{} }).
			OnStart(func(ctx context.Context, v *rD) error { return up(ctx, v) }).
			OnStop(func(ctx context.Context, v *rD) error { return down(ctx, v) })

		var wg sync.WaitGroup
		wg.Add(5)
		go func() { defer wg.Done(); _ = s.Start(context.Background()) }()
		go func() { defer wg.Done(); _, _ = s.Resolve[*rA]() }()
		go func() { defer wg.Done(); _, _ = s.Resolve[*rB]() }()
		go func() { defer wg.Done(); _, _ = s.Resolve[*rC]() }()
		go func() { defer wg.Done(); _, _ = s.Resolve[*rD]() }()
		wg.Wait()

		nstarted := started.Load()
		_ = s.Stop(context.Background())
		if nstopped := stopped.Load(); nstopped > nstarted {
			t.Fatalf("iteration %d: %d instances stopped but only %d started", i, nstopped, nstarted)
		}
	}
}

// 2. Bind must serve the target's own instance, keeping the target's lifetime.
func TestRegressionBindKeepsTargetLifetime(t *testing.T) {
	s := di.New()
	var builds atomic.Int32
	s.Provide(func(*di.Scope) *Repo { builds.Add(1); return &Repo{} }).Transient()
	s.Bind[Reader, *Repo]()
	a, b := s.Get[Reader](), s.Get[Reader]()
	if a == b || builds.Load() != 2 {
		t.Fatalf("alias must not cache a transient target: builds=%d", builds.Load())
	}

	// A Scoped target stays per resolving scope through the alias.
	s2 := di.New()
	s2.Provide(func(sc *di.Scope) *Repo { return &Repo{} }).Scoped()
	s2.Bind[Reader, *Repo]()
	c1, c2 := s2.Child("one"), s2.Child("two")
	if c1.Get[Reader]() == c2.Get[Reader]() {
		t.Fatal("alias to a Scoped target must not collapse to one instance")
	}
	if c1.Get[Reader]() != any(c1.Get[*Repo]()) {
		t.Fatal("alias and target must share one instance within a scope")
	}
}

// 3. A Run hook that dies on its own is reported by Stop, not only by Run.
func TestRegressionRunHookErrorReachesStop(t *testing.T) {
	boom := errors.New("queue disconnected")
	s := di.New()
	s.Value(&Worker{}).Eager().Run(func(context.Context, *Worker) error { return boom })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := s.Stop(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Stop must report the dead worker, got %v", err)
	}
}

// 4. Rollback must not run OnStop for a service whose OnStart never ran.
func TestRegressionRollbackSkipsNeverStarted(t *testing.T) {
	var log []string
	s := di.New()
	s.Provide(func(*di.Scope) *rA { return &rA{} }).
		OnStart(func(context.Context, *rA) error { log = append(log, "startA"); return nil }).
		OnStop(func(context.Context, *rA) error { log = append(log, "stopA"); return nil })
	s.Provide(func(*di.Scope) *rB { return &rB{} }).
		OnStart(func(context.Context, *rB) error { return errors.New("boom") })
	s.Provide(func(*di.Scope) *rC { return &rC{} }).
		OnStart(func(context.Context, *rC) error { log = append(log, "startC"); return nil }).
		OnStop(func(context.Context, *rC) error { log = append(log, "stopC"); return nil })
	s.Get[*rA]()
	s.Get[*rB]()
	s.Get[*rC]()
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("expected a start failure")
	}
	if got := strings.Join(log, ","); got != "startA,stopA" {
		t.Fatalf("got %q, want only the started service rolled back", got)
	}
}

// 5. Rollback must stop child scopes too.
func TestRegressionRollbackStopsChildren(t *testing.T) {
	childStopped := false
	s := di.New()
	child := s.Child("child")
	child.Value(&DB{}).OnStop(func(context.Context, *DB) error { childStopped = true; return nil })
	child.Get[*DB]()
	s.Value(&Worker{}).Eager().OnStart(func(context.Context, *Worker) error { return errors.New("boom") })
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("expected a start failure")
	}
	if !childStopped {
		t.Fatal("child scope was left running after rollback")
	}
}

// 6. Rollback must wait for Run hooks even when the caller's context is done.
func TestRegressionRollbackAwaitsRunHook(t *testing.T) {
	returned := make(chan struct{})
	s := di.New()
	s.Value(&Worker{}).Eager().
		Run(func(ctx context.Context, w *Worker) error {
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond)
			close(returned)
			return nil
		})
	s.Provide(func(*di.Scope) *rA { return &rA{} }).Eager().
		OnStart(func(ctx context.Context, _ *rA) error { return errors.New("boom") })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Start(ctx); err == nil {
		t.Fatal("expected a start failure")
	}
	select {
	case <-returned:
	default:
		t.Fatal("rollback returned without awaiting the Run hook")
	}
}

// 7. A build finishing after Stop must undo itself within Stop's deadline.
func TestRegressionLateUndoHonoursDeadline(t *testing.T) {
	building := make(chan struct{})
	release := make(chan struct{})
	s := di.New()
	s.Provide(func(*di.Scope) *Worker { close(building); <-release; return &Worker{} }).
		Run(func(ctx context.Context, w *Worker) error { time.Sleep(3 * time.Second); return nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _, _ = s.Resolve[*Worker](); close(done) }()
	<-building
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = s.Stop(ctx)
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the racing resolve ignored Stop's deadline")
	}
}

// 8. Eager on a group member builds it at Start.
func TestRegressionEagerGroupMember(t *testing.T) {
	var builds, starts atomic.Int32
	s := di.New()
	s.Add(func(*di.Scope) Handler { builds.Add(1); return Handler{} }).Eager().
		OnStart(func(context.Context, Handler) error { starts.Add(1); return nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if builds.Load() != 1 || starts.Load() != 1 {
		t.Fatalf("builds=%d starts=%d, want 1 and 1", builds.Load(), starts.Load())
	}
	if got := s.All[Handler](); len(got) != 1 {
		t.Fatalf("All returned %d members", len(got))
	}
	if builds.Load() != 1 {
		t.Fatal("All rebuilt the eager member")
	}
}

// 9. Combinations that cannot be honoured are rejected, in either call order.
func TestRegressionInvalidCombinations(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		wire func(*di.Scope)
	}{
		{"transient with hooks", "do not apply to a Transient binding", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *rA { return &rA{} }).Transient().OnStart(func(context.Context, *rA) error { return nil })
		}},
		{"hooks then transient", "do not apply to a Transient binding", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *rA { return &rA{} }).OnStop(func(context.Context, *rA) error { return nil }).Transient()
		}},
		{"eager transient", "does not apply to a Transient binding", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *rA { return &rA{} }).Transient().Eager()
		}},
		{"eager scoped", "does not apply to a Scoped binding", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *rA { return &rA{} }).Scoped().Eager()
		}},
		{"transient and scoped", "mutually exclusive", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *rA { return &rA{} }).Scoped().Transient()
		}},
		{"hooks on an alias", "belong on the target binding", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *Repo { return &Repo{} })
			s.Bind[Reader, *Repo]().OnStop(func(context.Context, Reader) error { return nil })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := di.New()
			tc.wire(s)
			mustPanic(t, tc.want, func() { _ = s.Start(context.Background()) })
		})
	}
}

// 10. Eager services build in registration order, not map order.
func TestRegressionEagerOrderIsDeterministic(t *testing.T) {
	for range 50 {
		var log []string
		s := di.New()
		s.Provide(func(*di.Scope) *rA { log = append(log, "A"); return &rA{} }).Eager()
		s.Provide(func(*di.Scope) *rB { log = append(log, "B"); return &rB{} }).Eager()
		s.Provide(func(*di.Scope) *rC { log = append(log, "C"); return &rC{} }).Eager()
		s.Provide(func(*di.Scope) *rD { log = append(log, "D"); return &rD{} }).Eager()
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(log, ""); got != "ABCD" {
			t.Fatalf("build order %q, want ABCD", got)
		}
	}
}

// 11. A key cannot be re-registered once it has been resolved.
func TestRegressionOverrideAfterResolveRejected(t *testing.T) {
	s := di.New()
	s.Value(&DB{dsn: "a"})
	s.Get[*DB]()
	s.Value(&DB{dsn: "b"})
	mustPanic(t, "cannot be overridden", func() { s.Get[*DB]() })

	// Overriding before the key is resolved stays legal: that is how tests
	// and child scopes substitute dependencies.
	ok := di.New()
	ok.Value(&DB{dsn: "a"})
	ok.Value(&DB{dsn: "b"})
	if got := ok.Get[*DB]().dsn; got != "b" {
		t.Fatalf("override before resolution must win, got %q", got)
	}
}

// 12. A transient is built in the scope that resolves it, like Scoped.
func TestRegressionTransientBuildsInResolvingScope(t *testing.T) {
	app := di.New()
	app.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} }).Transient()
	req := app.Child("request")
	req.Value(&DB{dsn: "request-scoped"})
	got, err := req.Resolve[*Repo]()
	if err != nil {
		t.Fatal(err)
	}
	if got.db.dsn != "request-scoped" {
		t.Fatalf("transient saw %q", got.db.dsn)
	}
}
