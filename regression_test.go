package di_test

// Regression tests for the twelve defects found in the September 2026 review.
// Each one reproduced against the code before the instance-phase refactor.

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// The four below are regressions introduced by the fixes above and caught by
// a second review pass.

// 13. An eager binding that a later registration overrode must not be built.
func TestRegressionShadowedEagerNotBuilt(t *testing.T) {
	var log []string
	s := di.New()
	s.Provide(func(*di.Scope) *DB { log = append(log, "real"); return &DB{dsn: "real"} }).Eager().
		OnStart(func(context.Context, *DB) error { log = append(log, "startReal"); return nil })
	s.Provide(func(*di.Scope) *DB { log = append(log, "fake"); return &DB{dsn: "fake"} }).Eager().
		OnStart(func(context.Context, *DB) error { log = append(log, "startFake"); return nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); got != "fake,startFake" {
		t.Fatalf("got %q, want only the winning registration built", got)
	}
	if got := s.Get[*DB]().dsn; got != "fake" {
		t.Fatalf("Get returned %q", got)
	}
}

// 14. An instance whose start step is in flight when Stop runs must still be
// torn down. Stop hands that teardown to the goroutine running the step, so
// it completes just after Stop returns rather than being skipped.
func TestRegressionStopHandsOffInFlightStart(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	stopped := make(chan struct{})
	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).
		OnStart(func(context.Context, *DB) error { close(entered); <-release; return nil }).
		OnStop(func(context.Context, *DB) error { close(stopped); return nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() { _, err := s.Resolve[*DB](); res <- err }()
	<-entered

	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight instance was never torn down")
	}
	if err := <-res; !errors.Is(err, di.ErrStopped) {
		t.Fatalf("a stopped scope handed out the value: %v", err)
	}
}

// 15. A failing eager constructor rolls back like a failing start hook.
func TestRegressionEagerConstructorFailureRollsBack(t *testing.T) {
	boom := errors.New("dial failed")
	var stops []string
	s := di.New()
	child := s.Child("child")
	child.Value(&Worker{}).OnStop(func(context.Context, *Worker) error { stops = append(stops, "child"); return nil })
	child.Get[*Worker]()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).Eager().
		OnStop(func(context.Context, *DB) error { stops = append(stops, "db"); return nil })
	s.Provide(func(sc *di.Scope) *Repo { return sc.Must((*Repo)(nil), boom) }).Eager()

	err := s.Start(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if !slices.Contains(stops, "db") || !slices.Contains(stops, "child") {
		t.Fatalf("rollback stopped %v, want the built service and the child scope", stops)
	}
	if _, err := s.Resolve[*DB](); !errors.Is(err, di.ErrStopped) {
		t.Fatalf("scope must be stopped after a failed Start, got %v", err)
	}
}

// 16. A worker dying with an error that wraps context.Canceled, while its own
// context is alive, is still reported.
func TestRegressionRunErrorWrappingCanceled(t *testing.T) {
	s := di.New()
	s.Value(&Worker{}).Eager().
		Run(func(ctx context.Context, _ *Worker) error { return fmt.Errorf("upstream dial: %w", context.Canceled) })
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "upstream dial") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a dead worker did not stop the application")
	}
}

// The four below are regressions introduced by the previous fixes and caught
// by a third review pass.

// 17. Stop called from inside a start step must not deadlock. The teardown
// is handed to the goroutine running the step, which is this one.
func TestRegressionStopInsideStartHookDoesNotDeadlock(t *testing.T) {
	stopped := false
	s := di.New()
	s.Value(&DB{}).Eager().
		OnStart(func(ctx context.Context, _ *DB) error { return s.Stop(context.Background()) }).
		OnStop(func(context.Context, *DB) error { stopped = true; return nil })

	done := make(chan error, 1)
	go func() { done <- s.Start(context.Background()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start deadlocked")
	}
	if !stopped {
		t.Fatal("the service was never torn down")
	}
	if _, err := s.Resolve[*DB](); !errors.Is(err, di.ErrStopped) {
		t.Fatalf("scope should be stopped: %v", err)
	}
}

// 18. A Stop whose deadline expires while a start step is in flight must not
// orphan the instance: its Run hook is cancelled and its OnStop runs.
func TestRegressionExpiredStopDoesNotOrphan(t *testing.T) {
	entered := make(chan struct{})
	runCancelled := make(chan struct{})
	stopped := make(chan struct{})
	s := di.New()
	s.Provide(func(*di.Scope) *Worker { return &Worker{} }).
		OnStart(func(context.Context, *Worker) error { close(entered); time.Sleep(150 * time.Millisecond); return nil }).
		Run(func(ctx context.Context, _ *Worker) error { <-ctx.Done(); close(runCancelled); return nil }).
		OnStop(func(context.Context, *Worker) error { close(stopped); return nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = s.Resolve[*Worker]() }()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_ = s.Stop(ctx)

	for _, ch := range []chan struct{}{runCancelled, stopped} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("the instance was orphaned: its Run hook or OnStop never ran")
		}
	}
}

// 19. Overriding an Eager binding keeps the key eager: the replacement is
// built at Start, which is what the test seam relies on.
func TestRegressionOverrideKeepsKeyEager(t *testing.T) {
	var log []string
	s := di.New()
	s.Provide(func(*di.Scope) *DB { log = append(log, "real"); return &DB{dsn: "real"} }).Eager().
		OnStart(func(context.Context, *DB) error { log = append(log, "startReal"); return nil })
	s.Value(&DB{dsn: "fake"}).
		OnStart(func(context.Context, *DB) error { log = append(log, "startFake"); return nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); got != "startFake" {
		t.Fatalf("got %q, want the replacement started and the original never built", got)
	}
	if got := s.Get[*DB]().dsn; got != "fake" {
		t.Fatalf("Get returned %q", got)
	}
}

// 20. A scope whose ancestor is already stopped must refuse to resolve, even
// before its own stopped flag is set.
func TestRegressionAncestorStoppedRejectsChild(t *testing.T) {
	root := di.New()
	root.Provide(func(*di.Scope) *Repo { return &Repo{} }).Scoped()
	child := root.Child("child")
	if _, err := child.Resolve[*Repo](); err != nil {
		t.Fatal(err)
	}
	grand := child.Child("grand")
	if err := root.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, sc := range map[string]*di.Scope{"child": child, "grandchild": grand} {
		if _, err := sc.Resolve[*Repo](); !errors.Is(err, di.ErrStopped) {
			t.Fatalf("%s resolved from a stopped tree: %v", name, err)
		}
	}
}

// The three below were found by a fourth review pass over the third pass's
// own fixes.

// 21. Eagerness transfers to whichever binding owns the key, so a per-scope
// winner must be rejected rather than built once in the declaring scope.
func TestRegressionEagerCannotTransferToPerScopeLifetime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(di.Binding[*DB]) di.Binding[*DB]
	}{
		{"Scoped", func(b di.Binding[*DB]) di.Binding[*DB] { return b.Scoped() }},
		{"Transient", func(b di.Binding[*DB]) di.Binding[*DB] { return b.Transient() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			built := false
			s := di.New()
			s.Provide(func(*di.Scope) *DB { return &DB{dsn: "real"} }).Eager()
			tc.apply(s.Provide(func(*di.Scope) *DB { built = true; return &DB{dsn: "fake"} }))
			mustPanic(t, "eagerness cannot transfer", func() { _ = s.Start(context.Background()) })
			if built {
				t.Fatalf("a %s binding was built at Start", tc.name)
			}
		})
	}
}

// 22. Start must not report success for a scope that a start hook stopped.
func TestRegressionStartReportsSelfStop(t *testing.T) {
	s := di.New()
	s.Value(&DB{}).Eager().OnStart(func(context.Context, *DB) error { return s.Stop(context.Background()) })
	if err := s.Start(context.Background()); !errors.Is(err, di.ErrStopped) {
		t.Fatalf("got %v, want ErrStopped", err)
	}
}

// 23. A teardown handed off after Stop returned still reaches observers.
func TestRegressionHandoffStopErrorReachesObservers(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	seen := make(chan error, 4)
	flush := errors.New("flush failed")
	s := di.New()
	s.Observe(func(ev di.Event) {
		if ev.Kind == di.EventStop {
			seen <- ev.Err
		}
	})
	s.Provide(func(*di.Scope) *DB { return &DB{} }).
		OnStart(func(context.Context, *DB) error { close(entered); <-release; return nil }).
		OnStop(func(context.Context, *DB) error { return flush })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = s.Resolve[*DB]() }()
	<-entered
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-seen:
		if !errors.Is(err, flush) {
			t.Fatalf("observer saw %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the late teardown was never reported")
	}
}

// The two below were found by a fifth review pass, run as two independent
// reviewers after the review agent repeatedly failed on server errors.

// 24. A Bind alias may end up owning an eager key. Start must build the
// target it redirects to, not the alias's own absent constructor.
func TestRegressionEagerAliasBuildsTarget(t *testing.T) {
	var builds int
	s := di.New()
	s.Provide(func(*di.Scope) Reader { return &Repo{db: &DB{dsn: "direct"}} }).Eager()
	s.Provide(func(*di.Scope) *Repo { builds++; return &Repo{db: &DB{dsn: "target"}} })
	s.Bind[Reader, *Repo]() // now owns the Reader key, and inherits its eagerness

	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if builds != 1 {
		t.Fatalf("target built %d times, want 1", builds)
	}
	if got := s.Get[Reader]().Read(); got != "target" {
		t.Fatalf("Reader resolved to %q", got)
	}
	if any(s.Get[Reader]()) != any(s.Get[*Repo]()) {
		t.Fatal("alias and target must share one instance")
	}
}

// 25. The Stop call that queues a handoff owns that teardown's context. A
// later Stop, which has nothing left to tear down, must not substitute its
// own, possibly cancelled, context.
func TestRegressionHandoffKeepsQueueingStopsContext(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	stopRan := make(chan struct{})
	var sawCancelled atomic.Bool

	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).Eager().
		OnStart(func(context.Context, *DB) error { close(entered); <-release; return nil }).
		OnStop(func(ctx context.Context, _ *DB) error {
			sawCancelled.Store(ctx.Err() != nil)
			close(stopRan)
			return nil
		})

	go func() { _ = s.Start(context.Background()) }()
	<-entered

	if err := s.Stop(context.Background()); err != nil { // queues the handoff
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_ = s.Stop(cancelled) // nothing left to stop, must not change the queued context

	close(release)
	select {
	case <-stopRan:
	case <-time.After(5 * time.Second):
		t.Fatal("the handed-off teardown never ran")
	}
	if sawCancelled.Load() {
		t.Fatal("OnStop ran with a later Stop's cancelled context")
	}
}

// The four below cover the alias refactor: lookup now follows Bind aliases,
// so resolve can never receive one and callers cannot forget the redirect.

// 26. Maybe must report absent for an alias whose target is missing, rather
// than reporting present and then failing inside Get.
func TestRegressionMaybeFollowsAliases(t *testing.T) {
	s := di.New()
	s.Bind[Reader, *Repo]() // target never registered
	if _, ok := s.Maybe[Reader](); ok {
		t.Fatal("Maybe reported an alias to a missing target as present")
	}
	s.Provide(func(*di.Scope) *Repo { return &Repo{db: &DB{dsn: "x"}} })
	if v, ok := s.Maybe[Reader](); !ok || v.Read() != "x" {
		t.Fatalf("Maybe = %v, %v once the target exists", v, ok)
	}
}

// 27. An alias chain that loops is a cycle, not a hang or a stack overflow.
// Two interfaces with the same method set each satisfy the other, so they
// can be bound to one another.
func TestRegressionAliasCycle(t *testing.T) {
	s := di.New()
	s.Bind[readerA, readerB]()
	s.Bind[readerB, readerA]() // readerA -> readerB -> readerA
	if _, err := s.Resolve[readerA](); !errors.Is(err, di.ErrCycle) {
		t.Fatalf("got %v, want ErrCycle", err)
	}
	if _, ok := s.Maybe[readerA](); ok {
		t.Fatal("Maybe reported a looping alias chain as present")
	}
}

type readerA interface{ Read() string }
type readerB interface{ Read() string }

// 27b. Bind's first type parameter must be an interface, and saying so
// beats surfacing a raw reflect panic.
func TestRegressionBindRejectsNonInterface(t *testing.T) {
	mustPanic(t, "must be an interface", func() { di.New().Bind[*Repo, *Repo]() })
}

// 28. An eager key served through an alias to a per-scope target cannot
// honour eagerness, exactly as a direct per-scope winner cannot.
func TestRegressionEagerAliasToPerScopeTargetRejected(t *testing.T) {
	built := false
	s := di.New()
	s.Provide(func(*di.Scope) Reader { return &Repo{db: &DB{}} }).Eager()
	s.Provide(func(*di.Scope) *Repo { built = true; return &Repo{db: &DB{}} }).Scoped()
	s.Bind[Reader, *Repo]()
	mustPanic(t, "eagerness cannot transfer", func() { _ = s.Start(context.Background()) })
	if built {
		t.Fatal("the scoped target was built at Start")
	}
}

// 29. Eagerness is a property of the key, so an alias may declare it: the
// target is built at Start.
func TestRegressionEagerDeclaredOnAlias(t *testing.T) {
	var builds int
	s := di.New()
	s.Provide(func(*di.Scope) *Repo { builds++; return &Repo{db: &DB{dsn: "t"}} })
	s.Bind[Reader, *Repo]().Eager()
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if builds != 1 {
		t.Fatalf("target built %d times, want 1", builds)
	}
	if got := s.Get[Reader]().Read(); got != "t" {
		t.Fatalf("got %q", got)
	}
}

// The three below were found by a sixth review pass, which also audited the
// property test's own model rather than trusting it.

// 30. A rejected registration must be rejected every time. freeze used to
// clear pending before deriving the eager set, so a panic there left the
// batch consumed and a retried Start silently succeeded with the invalid
// configuration dropped.
func TestRegressionRejectionIsRepeatable(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).Eager()
	s.Provide(func(*di.Scope) *Repo { return &Repo{} }).Eager()
	s.Provide(func(*di.Scope) *Repo { return &Repo{} }).Scoped() // eagerness cannot transfer

	for attempt := range 3 {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("attempt %d: Start was accepted, the invalid config was dropped", attempt)
				}
			}()
			_ = s.Start(context.Background())
		}()
	}
}

// 31. A rejected batch must leave the scope as it was, so the rejection is
// the same on every subsequent operation rather than a half-applied registry.
func TestRegressionRejectedBatchIsNotHalfApplied(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} })
	s.Provide(func(*di.Scope) *Repo { return &Repo{} }).Scoped().Transient() // invalid

	var msgs []string
	for range 3 {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					msgs = append(msgs, "accepted")
					return
				}
				msgs = append(msgs, r.(string))
			}()
			_, _ = s.Resolve[*DB]()
		}()
	}
	for i, m := range msgs {
		if !strings.Contains(m, "mutually exclusive") {
			t.Fatalf("attempt %d: %s", i, m)
		}
	}
	if msgs[0] != msgs[1] || msgs[1] != msgs[2] {
		t.Fatalf("rejection was not identical across attempts: %v", msgs)
	}
}

// 32. An alias key counts as resolved once something has been served through
// it, so it cannot be re-registered either.
func TestRegressionAliasKeyCannotBeRebound(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *Repo { return &Repo{db: &DB{dsn: "first"}} })
	s.Bind[Reader, *Repo]()
	if got := s.Get[Reader]().Read(); got != "first" {
		t.Fatalf("got %q", got)
	}
	s.Provide(func(*di.Scope) Reader { return &Repo{db: &DB{dsn: "second"}} })
	mustPanic(t, "cannot be overridden", func() { _ = s.Get[Reader]() })
}

// The two below were found by a seventh, narrow pass over the transactional
// freeze and the alias used-marking.

// 33. A key whose constructor failed built nothing, so it can be
// re-registered. Because a failed instance caches its error for good, this
// is the only way to recover such a key.
func TestRegressionFailedResolveLeavesKeyReRegisterable(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *DB { panic("boom") })
	if _, err := s.Resolve[*DB](); err == nil {
		t.Fatal("expected the constructor panic to surface")
	}
	s.Provide(func(*di.Scope) *DB { return &DB{dsn: "recovered"} })
	got, err := s.Resolve[*DB]()
	if err != nil {
		t.Fatalf("re-registration should recover the key: %v", err)
	}
	if got.dsn != "recovered" {
		t.Fatalf("got %q", got.dsn)
	}
}

// 34. The same for an alias: redirecting the interface to a working
// implementation is the whole point of Bind, so a failed target must not
// foreclose it.
func TestRegressionFailedAliasTargetLeavesKeyReAliasable(t *testing.T) {
	s := di.New()
	s.Bind[Reader, *Repo]()
	s.Provide(func(*di.Scope) *Repo { panic("boom") })
	if _, err := s.Resolve[Reader](); err == nil {
		t.Fatal("expected the target's panic to surface")
	}

	// Redirect the interface at a different, untouched implementation.
	s.Bind[Reader, *altReader]()
	s.Provide(func(*di.Scope) *altReader { return &altReader{} })
	got, err := s.Resolve[Reader]()
	if err != nil {
		t.Fatalf("re-aliasing should be allowed: %v", err)
	}
	if got.Read() != "alt" {
		t.Fatalf("got %q", got.Read())
	}
}

type altReader struct{}

func (*altReader) Read() string { return "alt" }
