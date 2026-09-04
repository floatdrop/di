package di_test

// Regressions in starting and stopping: rollback, the mid-start handoff, and
// the rule that a stopped scope serves nothing.
//
// One test per defect, named for the rule it pins. The tag at the end of a
// comment says where the defect came from. (review 1, 3) is the third defect
// of the first September 2026 review, checked against 12dba3c; review 2 was
// checked against 2b8915d and review 3 against 9ace680. (pass 4) is the
// fourth of the seven narrower passes that preceded those reviews, each
// checked against the code before the instance-phase refactor, and (alias
// refactor) the sweep that made lookup follow Bind chains. An untagged test
// comes from the first of those passes, or from the generators, which its own
// comment says. Several fail by hanging rather than by reporting, which is why
// each bounds its own wait instead of relying on the package timeout.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floatdrop/di"
)

// An instance built concurrently with Start must be started by exactly one
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

// Rollback must not run OnStop for a service whose OnStart never ran.
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

// Rollback must stop child scopes too.
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

// Rollback must wait for Run hooks even when the caller's context is done.
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

// A build finishing after Stop must undo itself within Stop's deadline.
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

// An instance whose start step is in flight when Stop runs is torn down
// before Stop returns. Stop waits for the step; it used to hand the teardown
// to the goroutine running it, which finished after Stop had returned.
// (pass 2)
func TestStopWaitsForAnInFlightStartStep(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var stopped atomic.Bool
	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).
		OnStart(func(context.Context, *DB) error { close(entered); <-release; return nil }).
		OnStop(func(context.Context, *DB) error { stopped.Store(true); return nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() { _, err := s.Resolve[*DB](); res <- err }()
	<-entered

	go func() { time.Sleep(20 * time.Millisecond); close(release) }()
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !stopped.Load() {
		t.Fatal("Stop returned before the in-flight instance was torn down")
	}
	if err := <-res; !errors.Is(err, di.ErrStopped) {
		t.Fatalf("a stopped scope handed out the value: %v", err)
	}
}

// A failing eager constructor rolls back like a failing start hook.
// (pass 2)
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

// A hook may not call Stop on its own scope: it would be waiting for the step
// it is itself running. A hook that passes on the context it was given is
// told so rather than left to wait, and Shutdown is the call that does work
// from inside a hook.
// (pass 3)
func TestStopFromAHookIsReported(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire func(*di.Scope, chan<- error)
	}{
		{"OnStart", func(s *di.Scope, got chan<- error) {
			s.Value(&DB{}).Eager().
				OnStart(func(ctx context.Context, _ *DB) error { got <- s.Stop(ctx); return nil })
		}},
		{"OnDrain", func(s *di.Scope, got chan<- error) {
			s.Value(&DB{}).Eager().
				OnDrain(func(ctx context.Context, _ *DB) error { got <- s.Stop(ctx); return nil })
		}},
		{"OnStop", func(s *di.Scope, got chan<- error) {
			s.Value(&DB{}).Eager().
				OnStop(func(ctx context.Context, _ *DB) error { got <- s.Stop(ctx); return nil })
		}},
		{"Run", func(s *di.Scope, got chan<- error) {
			s.Value(&DB{}).Eager().
				Run(func(ctx context.Context, _ *DB) error { got <- s.Stop(ctx); <-ctx.Done(); return nil })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan error, 1)
			s := di.New()
			tc.wire(s, got)
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { defer close(done); _ = s.Stop(context.Background()) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Stop deadlocked")
			}
			select {
			case err := <-got:
				if err == nil || !strings.Contains(err.Error(), "call Shutdown instead") {
					t.Fatalf("the hook's Stop returned %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("the hook never ran")
			}
		})
	}
}

// A hook that passes a context of its own is not recognised, and then the
// wait is the caller's to bound: with a deadline it is reported, and only a
// hook that waits for ever on a background context can hang.
// (pass 3)
func TestStopFromAHookWithItsOwnContextIsBounded(t *testing.T) {
	got := make(chan error, 1)
	s := di.New()
	s.Value(&DB{}).Eager().
		OnStart(func(context.Context, *DB) error {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			got <- s.Stop(ctx)
			return nil
		})
	// The Stop is a real one: it claims the scope, so Start reports that the
	// scope stopped under it. What is being checked is that the hook's own
	// call came back at all.
	if err := s.Start(context.Background()); !errors.Is(err, di.ErrStopped) {
		t.Fatalf("Start: %v", err)
	}
	select {
	case err := <-got:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want the missed deadline, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop from the hook never returned")
	}
}

// A Stop whose deadline expires while a start step is in flight must not
// orphan the instance: its Run hook is cancelled and its OnStop runs. Stop
// waits for the step, so this is the deadline ending the caller's wait rather
// than the teardown, which finishes on a goroutine of its own.
// (pass 3)
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

// A scope whose ancestor is already stopped must refuse to resolve, even
// before its own stopped flag is set.
// (pass 3)
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

// Start must not report success for a scope that was stopped while its hook
// phase was running. The stop used to come from the hook itself; a hook may
// no longer do that, so it comes from another goroutine, which is the shape
// that remains.
// (pass 4)
func TestStartReportsAScopeStoppedUnderIt(t *testing.T) {
	entered := make(chan struct{})
	s := di.New()
	s.Value(&DB{}).Eager().
		OnStart(func(context.Context, *DB) error { close(entered); time.Sleep(50 * time.Millisecond); return nil })
	go func() { <-entered; _ = s.Stop(context.Background()) }()
	if err := s.Start(context.Background()); !errors.Is(err, di.ErrStopped) {
		t.Fatalf("got %v, want ErrStopped", err)
	}
}

// The teardown of an instance whose start step was in flight is reported to
// the caller of Stop, not only to observers: Stop waits for the step, so the
// failure is its own to report.
// (pass 4)
func TestStopReportsTheTeardownItWaitedFor(t *testing.T) {
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
	go func() { time.Sleep(20 * time.Millisecond); close(release) }()

	if err := s.Stop(context.Background()); !errors.Is(err, flush) {
		t.Fatalf("Stop returned %v, want the teardown failure it waited for", err)
	}
	select {
	case err := <-seen:
		if !errors.Is(err, flush) {
			t.Fatalf("observer saw %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the teardown was never reported")
	}
}

// The first Stop owns the context of every teardown its scope still owes,
// including the one that undoes a build finishing after the scope stopped. A
// later Stop, which has nothing left to tear down, must not substitute its
// own, possibly cancelled, context.
// (pass 5)
func TestFirstStopOwnsALateTeardownsContext(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	stopRan := make(chan struct{})
	var sawCancelled atomic.Bool

	s := di.New()
	s.Provide(func(*di.Scope) *DB { close(entered); <-release; return &DB{} }).
		OnStop(func(ctx context.Context, _ *DB) error {
			sawCancelled.Store(ctx.Err() != nil)
			close(stopRan)
			return nil
		})
	go func() { _, _ = s.Resolve[*DB]() }() // still constructing when Stop runs
	<-entered

	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_ = s.Stop(cancelled) // nothing left to stop, must not change the owned context

	close(release)
	select {
	case <-stopRan:
	case <-time.After(5 * time.Second):
		t.Fatal("the build that finished after Stop was never undone")
	}
	if sawCancelled.Load() {
		t.Fatal("the undo ran with a later Stop's cancelled context")
	}
}

// A child scope stopping concurrently with its parent must finish before
// the parent closes what that child's stop hooks are still using. The shape
// is an HTTP request ending just as the application shuts down.
// (review 1, 1)
func TestReviewConcurrentChildStopPreservesDependencyOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	root := di.New()
	root.Provide(func(*di.Scope) *DB { return &DB{} }).
		OnStop(func(context.Context, *DB) error { note("db"); return nil })
	entered := make(chan struct{})
	release := make(chan struct{})
	root.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} }).Scoped().
		OnStop(func(context.Context, *Repo) error { close(entered); <-release; note("repo"); return nil })

	child := root.Child("request")
	child.Get[*Repo]()

	childDone := make(chan struct{})
	go func() { _ = child.Stop(context.Background()); close(childDone) }()
	<-entered

	rootDone := make(chan struct{})
	go func() { _ = root.Stop(context.Background()); close(rootDone) }()
	time.Sleep(50 * time.Millisecond) // long enough for a racing root to run ahead
	close(release)

	for _, ch := range []chan struct{}{childDone, rootDone} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("Stop did not return")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "repo,db" {
		t.Fatalf("stop order %v: the parent closed a dependency the child was still using", order)
	}
}

// Run's rollback of a failed Start must honour StopTimeout. Without it a
// well-behaved OnStop that waits on its context hangs the process.
// (review 1, 2)
func TestReviewRunRollbackHonoursStopTimeout(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).
		OnStart(func(context.Context, *DB) error { return nil }).
		OnStop(func(ctx context.Context, _ *DB) error { <-ctx.Done(); return ctx.Err() })
	s.Provide(func(sc *di.Scope) *Repo { return &Repo{db: sc.Get[*DB]()} }).Eager().
		OnStart(func(context.Context, *Repo) error { return errors.New("boom") })

	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background(), di.StopTimeout(10*time.Millisecond)) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected the start failure")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the rollback ignored StopTimeout")
	}
}

// The gap the review noted without counting: after Start, a resolution must
// not hand out a service whose OnStart is still running.
// (review 1)
func TestReviewResolveWaitsForInFlightStart(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var started atomic.Bool

	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).Eager().
		OnStart(func(context.Context, *DB) error {
			close(entered)
			<-release
			started.Store(true)
			return nil
		})

	go func() { _ = s.Start(context.Background()) }()
	<-entered

	got := make(chan bool, 1)
	go func() {
		_, err := s.Resolve[*DB]()
		got <- err == nil && started.Load()
	}()

	select {
	case <-got:
		t.Fatal("Resolve returned while the start step was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("Resolve returned a service whose OnStart had not finished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Resolve never returned")
	}
	_ = s.Stop(context.Background())
}

// A scope that has stopped refuses to resolve, including a resolution that
// was already waiting on a constructor running in a live ancestor.
// (review 2, 7)
func TestReview2StoppedScopeServesNothingAfterWaiting(t *testing.T) {
	inCtor := make(chan struct{})
	release := make(chan struct{})

	root := di.New()
	root.Provide(func(*di.Scope) *DB { close(inCtor); <-release; return &DB{} })
	kid := root.Child("kid")

	res := make(chan error, 1)
	go func() { _, err := kid.Resolve[*DB](); res <- err }()
	<-inCtor

	if err := kid.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)

	select {
	case err := <-res:
		if !errors.Is(err, di.ErrStopped) {
			t.Fatalf("a stopped scope handed out a value: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting resolution never returned")
	}
	if _, err := root.Resolve[*DB](); err != nil {
		t.Fatalf("the live root lost the instance too: %v", err)
	}
}

// A panicking start hook is a failed start: the service is not served, the
// panic reaches Resolve as an error, and no OnStop is paired with the OnStart
// that never finished.
// (review 2, 8)
func TestReview2PanickingStartHookIsAFailure(t *testing.T) {
	var starts, stops atomic.Int32

	root := di.New()
	root.Provide(func(*di.Scope) *DB { return &DB{} }).
		OnStart(func(context.Context, *DB) error { starts.Add(1); panic("boom") }).
		OnStop(func(context.Context, *DB) error { stops.Add(1); return nil })
	if err := root.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := root.Resolve[*DB]()
	if err == nil {
		t.Fatal("a service whose OnStart panicked was served")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v", err)
	}
	if _, err := root.Resolve[*DB](); err == nil {
		t.Fatal("a later resolve was served the unstarted service")
	}
	if err := root.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := stops.Load(); got != 0 {
		t.Fatalf("OnStop ran %d times for a service that never started", got)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("OnStart ran %d times", got)
	}
}

// A Transient constructor that finishes after its scope has stopped must
// not hand the value back: the transient branch skipped the check await makes
// after its wait.
// (review 3, 5)
func TestReview3TransientFinishingAfterStopIsRejected(t *testing.T) {
	root := di.New()
	building := make(chan struct{})
	release := make(chan struct{})
	root.Provide(func(*di.Scope) *r3T { close(building); <-release; return &r3T{} }).Transient()

	out := make(chan error, 1)
	go func() { _, err := root.Resolve[*r3T](); out <- err }()
	<-building
	if err := root.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-out; !errors.Is(err, di.ErrStopped) {
		t.Errorf("want ErrStopped, got %v", err)
	}
}
