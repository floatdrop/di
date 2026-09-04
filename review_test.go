package di_test

// Regression tests for the eleven defects of the September 2026 review, plus
// the resolution-during-Start gap that review noted but did not count.
//
// Every test here was checked against the commit before the fix (12dba3c) and
// fails there. The three concurrency ones fail by hanging rather than by
// reporting, which is why each bounds its own wait instead of relying on the
// package timeout.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floatdrop/di"
)

type vA struct{}
type vB struct{}
type vT struct{ n int }
type vI interface{ marker() }

func (*vT) marker() {}

// 1. A child scope stopping concurrently with its parent must finish before
// the parent closes what that child's stop hooks are still using. The shape
// is an HTTP request ending just as the application shuts down.
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

// 2. Run's rollback of a failed Start must honour StopTimeout. Without it a
// well-behaved OnStop that waits on its context hangs the process.
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

// 3. A constructor may resolve its dependencies from several goroutines. The
// resolution path must not be shared mutable state.
func TestReviewParallelDependenciesInsideConstructor(t *testing.T) {
	for range 50 {
		s := di.New()
		s.Provide(func(*di.Scope) *vA { return &vA{} })
		s.Provide(func(*di.Scope) *vB { return &vB{} })
		s.Provide(func(sc *di.Scope) *Repo {
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					if _, err := sc.Resolve[*vA](); err != nil {
						t.Errorf("parallel resolve of vA: %v", err)
					}
				})
				wg.Go(func() {
					if _, err := sc.Resolve[*vB](); err != nil {
						t.Errorf("parallel resolve of vB: %v", err)
					}
				})
			}
			wg.Wait()
			return &Repo{}
		})
		if _, err := s.Resolve[*Repo](); err != nil {
			t.Fatalf("false failure: %v", err)
		}
	}
}

// 3b. Two goroutines asking for the same singleton from inside one
// constructor is not a cycle. It reported one deterministically.
func TestReviewParallelSameDependencyIsNotACycle(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *vA { return &vA{} })
	s.Provide(func(sc *di.Scope) *Repo {
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Go(func() { _, errs[i] = sc.Resolve[*vA]() })
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Errorf("false cycle: %v", err)
			}
		}
		return &Repo{}
	})
	if _, err := s.Resolve[*Repo](); err != nil {
		t.Fatal(err)
	}
}

// 4. A cycle entered from both ends at once must be reported, not deadlock.
// Neither branch has the repeated key on its own path, so it is only visible
// through the wait-for graph.
func TestReviewConcurrentCycle(t *testing.T) {
	s := di.New()
	inA, inB := make(chan struct{}), make(chan struct{})
	both := make(chan struct{})
	s.Provide(func(sc *di.Scope) *vA { close(inA); <-both; sc.Get[*vB](); return &vA{} })
	s.Provide(func(sc *di.Scope) *vB { close(inB); <-both; sc.Get[*vA](); return &vB{} })

	errs := make(chan error, 2)
	go func() { _, err := s.Resolve[*vA](); errs <- err }()
	go func() { _, err := s.Resolve[*vB](); errs <- err }()
	<-inA
	<-inB
	close(both)

	var reported int
	for range 2 {
		select {
		case err := <-errs:
			if errors.Is(err, di.ErrCycle) {
				reported++
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a concurrent cycle deadlocked instead of reporting ErrCycle")
		}
	}
	if reported == 0 {
		t.Fatal("neither branch reported ErrCycle")
	}
}

// 5. A Transient constructor goes through the same wrapper as any other: a
// panic becomes an error and a successful build is observed.
func TestReviewTransientPanicIsAnError(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *vA { panic("boom") }).Transient()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Resolve panicked instead of returning an error: %v", r)
		}
	}()
	if _, err := s.Resolve[*vA](); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v", err)
	}
}

func TestReviewTransientIsObserved(t *testing.T) {
	var builds int
	s := di.New()
	s.Observe(func(ev di.Event) {
		if ev.Kind == di.EventBuild {
			builds++
		}
	})
	s.Provide(func(*di.Scope) *vA { return &vA{} }).Transient()
	s.Get[*vA]()
	s.Get[*vA]()
	if builds != 2 {
		t.Fatalf("EventBuild fired %d times for two transient builds", builds)
	}
}

// 6a. An alias owned by the root whose target is local to a child still
// serves the child that key: registering it there afterwards would give the
// child two live values.
func TestReviewAliasTargetLocalShadow(t *testing.T) {
	root := di.New()
	root.Bind[vI, *vT]()
	child := root.Child("c")
	child.Provide(func(*di.Scope) *vT { return &vT{n: 1} })
	child.Get[vI]()

	defer func() {
		if recover() == nil {
			t.Fatal("shadowing a key the scope already served through an alias was accepted")
		}
	}()
	child.Provide(func(*di.Scope) vI { return &vT{n: 2} })
	child.Get[vI]()
}

// 6b. A scope between the resolver and the owner served the key too, so it
// cannot shadow it afterwards either.
func TestReviewIntermediateScopeShadow(t *testing.T) {
	root := di.New()
	root.Provide(func(*di.Scope) *vT { return &vT{n: 1} })
	mid := root.Child("mid")
	gc := mid.Child("gc")
	gc.Get[*vT]()

	defer func() {
		if recover() == nil {
			t.Fatal("an intermediate scope shadowed a key its descendant had already been served")
		}
	}()
	mid.Provide(func(*di.Scope) *vT { return &vT{n: 2} })
	gc.Get[*vT]()
}

// 7. A nil interface is a service like any other. Every hand-back path has
// to survive it, not just the one that stores it.
func TestReviewNilInterfaceValue(t *testing.T) {
	s := di.New()
	s.Value[error](nil)
	s.Add(func(*di.Scope) error { return nil })

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a nil interface value panicked: %v", r)
		}
	}()
	if v, err := s.Resolve[error](); err != nil || v != nil {
		t.Fatalf("Resolve: v=%v err=%v", v, err)
	}
	if v := s.Get[error](); v != nil {
		t.Fatalf("Get: %v", v)
	}
	if v, ok := s.Maybe[error](); !ok || v != nil {
		t.Fatalf("Maybe: %v %v", v, ok)
	}
	if all := s.All[error](); len(all) != 1 || all[0] != nil {
		t.Fatalf("All: %v", all)
	}
}

func TestReviewNilInterfaceReachesHooks(t *testing.T) {
	ran := false
	s := di.New()
	s.Value[error](nil).OnStop(func(_ context.Context, v error) error {
		ran = v == nil
		return nil
	})
	_ = s.Get[error]()
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("OnStop did not receive the nil value")
	}
}

// 8. A group member and a plain registration of the same type are different
// bindings. Cycle detection must compare bindings, not keys.
func TestReviewGroupAndDirectSameType(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) vI { return &vT{n: 1} })
	s.Add(func(sc *di.Scope) vI { _ = sc.Get[vI](); return &vT{n: 2} })

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("All reported a false cycle: %v", r)
		}
	}()
	if got := s.All[vI](); len(got) != 1 {
		t.Fatalf("got %d group members", len(got))
	}
}

// 9. A worker that dies in a child scope must reach the root's Run even when
// the child is stopped and detached before the root reacts.
func TestReviewDetachedChildWorkerFailureReachesRun(t *testing.T) {
	root := di.New()
	child := root.Child("c")
	failed := make(chan struct{})
	child.Provide(func(*di.Scope) *Worker { return &Worker{} }).Eager().
		Run(func(context.Context, *Worker) error { defer close(failed); return errors.New("worker died") })

	runDone := make(chan error, 1)
	go func() { runDone <- root.Run(context.Background(), di.StopTimeout(time.Second)) }()
	if err := child.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-failed
	_ = child.Stop(context.Background()) // detaches before the root gets there

	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "worker died") {
			t.Fatalf("root.Run returned %v, want the worker failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("root.Run did not return")
	}
}

// 9b. The same failure reported by both routes is still reported once. This
// one passes before the fix as well: a dying worker did not record a cause
// there, so there was nothing to duplicate. It guards the fix for 9, which
// makes it record one, rather than covering a defect of its own.
func TestReviewWorkerFailureIsNotDuplicated(t *testing.T) {
	boom := errors.New("queue disconnected")
	s := di.New()
	s.Value(&Worker{}).Eager().Run(func(context.Context, *Worker) error { return boom })
	err := s.Run(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if n := strings.Count(err.Error(), boom.Error()); n != 1 {
		t.Fatalf("the worker failure is listed %d times:\n%v", n, err)
	}
}

// 10. The handler and the injected *http.Request must be one request, so a
// scoped constructor sees what the router matched.
func TestReviewMiddlewareInjectedRequestRouting(t *testing.T) {
	var gotID, gotScope atomic.Value
	gotID.Store("")
	gotScope.Store(false)

	app := di.New()
	app.Provide(func(s *di.Scope) *Handler {
		r := s.Get[*http.Request]()
		gotID.Store(r.PathValue("id"))
		_, ok := di.FromContext(r.Context())
		gotScope.Store(ok)
		return &Handler{name: r.PathValue("id")}
	}).Scoped()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		sc, _ := di.FromContext(r.Context())
		sc.Get[*Handler]()
	})
	srv := httptest.NewServer(app.Middleware(mux))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/users/42")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if id := gotID.Load().(string); id != "42" {
		t.Fatalf("the injected request has PathValue(id)=%q, want 42", id)
	}
	if !gotScope.Load().(bool) {
		t.Fatal("the injected request's context has no scope")
	}
}

// 11. A request in flight when Stop begins keeps its scope until the server
// has drained, which is what OnDrain is for.
func TestReviewRequestSurvivesDrain(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	resolved := make(chan error, 1)

	app := di.New()
	app.Provide(func(s *di.Scope) *Handler {
		return &Handler{name: s.Get[*http.Request]().URL.Path}
	}).Scoped()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		sc, _ := di.FromContext(r.Context())
		_, err := sc.Resolve[*Handler]()
		resolved <- err
	})

	srv := httptest.NewUnstartedServer(app.Middleware(mux))
	srv.Start()
	defer srv.Close()
	app.Value(srv.Config).
		OnDrain(func(ctx context.Context, s *http.Server) error { return s.Shutdown(ctx) })
	app.Get[*http.Server]()

	go func() {
		resp, err := http.Get(srv.URL + "/slow")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered

	stopped := make(chan error, 1)
	go func() {
		close(release)
		stopped <- app.Stop(context.Background())
	}()

	select {
	case err := <-resolved:
		if err != nil {
			t.Fatalf("the in-flight request lost its scope during drain: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never finished")
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
}

// Drain runs before anything is stopped, innermost scope first, and only for
// services that are live enough to owe a teardown.
func TestReviewDrainOrder(t *testing.T) {
	var log []string
	root := di.New()
	root.Value(&DB{}).
		OnDrain(func(context.Context, *DB) error { log = append(log, "drain db"); return nil }).
		OnStop(func(context.Context, *DB) error { log = append(log, "stop db"); return nil })
	root.Get[*DB]()

	child := root.Child("c")
	child.Value(&Worker{}).
		OnDrain(func(context.Context, *Worker) error { log = append(log, "drain worker"); return nil }).
		OnStop(func(context.Context, *Worker) error { log = append(log, "stop worker"); return nil })
	child.Get[*Worker]()

	if err := root.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "drain worker,drain db,stop worker,stop db"
	if got := strings.Join(log, ","); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestReviewDrainSkipsUnbuiltAndRunsOnce(t *testing.T) {
	drains := 0
	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).
		OnDrain(func(context.Context, *DB) error { drains++; return nil })
	s.Provide(func(*di.Scope) *Worker { t.Fatal("an unbuilt service was drained"); return nil }).
		OnDrain(func(context.Context, *Worker) error { return nil })
	s.Get[*DB]()
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if drains != 1 {
		t.Fatalf("OnDrain ran %d times", drains)
	}
}

func TestReviewDrainFailureIsReported(t *testing.T) {
	boom := errors.New("drain failed")
	s := di.New()
	s.Value(&DB{}).OnDrain(func(context.Context, *DB) error { return boom })
	s.Get[*DB]()
	if err := s.Stop(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestReviewDrainRejectedOnTransient(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).Transient().
		OnDrain(func(context.Context, *DB) error { return nil })
	mustPanic(t, "do not apply to a Transient binding", func() { s.Get[*DB]() })
}

// The gap the review noted without counting: after Start, a resolution must
// not hand out a service whose OnStart is still running.
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
