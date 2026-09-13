package di_test

// Regressions in the drain phase: the one teardown phase during which a scope
// still resolves, and the only one two Stop calls can be inside at once.
// Tags are explained in fixtures_test.go.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/dihttp"
)

// A request in flight when Stop begins keeps its scope until the server
// has drained, which is what OnDrain is for.
// (review 1, 11)
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

	srv := httptest.NewUnstartedServer(dihttp.NewMiddleware(app)(mux))
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
// (review 1)
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

// A Stop that reaches a scope whose drain another Stop is running must wait
// for it rather than release underneath the hook still using the value.
// (review 2, 1)
func TestReview2ConcurrentStopWaitsForAncestorDrain(t *testing.T) {
	inDrain := make(chan struct{})
	release := make(chan struct{})
	var overlap atomic.Bool

	root := di.New()
	kid := root.Child("kid")
	kid.Value(&DB{}).Eager().
		OnDrain(func(context.Context, *DB) error { close(inDrain); <-release; return nil }).
		OnStop(func(context.Context, *DB) error {
			select {
			case <-release:
			default:
				overlap.Store(true)
			}
			return nil
		})
	if err := root.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := kid.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	rootStop := make(chan error, 1)
	go func() { rootStop <- root.Stop(context.Background()) }()
	<-inDrain

	kidStop := make(chan error, 1)
	go func() { kidStop <- kid.Stop(context.Background()) }()
	select {
	case err := <-kidStop:
		t.Fatalf("the second Stop walked past a drain in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	for _, ch := range []chan error{kidStop, rootStop} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Stop never returned")
		}
	}
	if overlap.Load() {
		t.Fatal("OnStop ran while OnDrain was still in flight")
	}
}

// A scope must not mark itself stopped while a descendant's drain, owned by
// another Stop, is still running: those hooks resolve.
// (review 2, 2)
func TestReview2AncestorStopWaitsForIndependentChildDrain(t *testing.T) {
	inDrain := make(chan struct{})
	release := make(chan struct{})
	var resolveErr error

	root := di.New()
	root.Provide(func(*di.Scope) *DB { return &DB{dsn: "root"} }).Eager()

	kid := root.Child("kid")
	kid.Value(&Worker{}).Eager().
		OnDrain(func(context.Context, *Worker) error {
			close(inDrain)
			<-release
			_, resolveErr = kid.Resolve[*DB]()
			return nil
		})
	if err := root.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := kid.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	kidStop := make(chan error, 1)
	go func() { kidStop <- kid.Stop(context.Background()) }()
	<-inDrain

	rootStop := make(chan error, 1)
	go func() { rootStop <- root.Stop(context.Background()) }()
	time.Sleep(100 * time.Millisecond) // let root.Stop reach the point of marking itself stopped
	close(release)

	for _, ch := range []chan error{kidStop, rootStop} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Stop never returned")
		}
	}
	if resolveErr != nil {
		t.Fatalf("a drain hook could not reach its dependency: %v", resolveErr)
	}
}

// The scope still resolves while draining, so a service first built by a
// drain hook is drained too.
// (review 2, 3)
func TestReview2LateBuildDuringDrainIsDrained(t *testing.T) {
	var mu sync.Mutex
	var log []string
	note := func(s string) { mu.Lock(); log = append(log, s); mu.Unlock() }

	root := di.New()
	root.Provide(func(*di.Scope) *DB { return &DB{} }).
		OnDrain(func(context.Context, *DB) error { note("drain db"); return nil }).
		OnStop(func(context.Context, *DB) error { note("stop db"); return nil })
	root.Value(&Worker{}).Eager().
		OnDrain(func(context.Context, *Worker) error {
			if _, err := root.Resolve[*DB](); err != nil {
				return err
			}
			note("drain worker")
			return nil
		})
	if err := root.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := root.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"drain worker", "drain db", "stop db"}
	if len(log) != len(want) {
		t.Fatalf("got %v, want %v", log, want)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("got %v, want %v", log, want)
		}
	}
}

// A child scope opened by a drain hook, as an in-flight request opens its
// request scope, is drained before the parent goes stopped, so its own hooks
// can still resolve.
// (review 2, 4)
func TestReview2LateChildDrainCanResolve(t *testing.T) {
	var resolveErr error
	var ran atomic.Bool

	root := di.New()
	root.Provide(func(*di.Scope) *DB { return &DB{dsn: "root"} }).Eager()
	root.Value(&Worker{}).Eager().
		OnDrain(func(context.Context, *Worker) error {
			req := root.Child("req")
			req.Provide(func(*di.Scope) *Repo { return &Repo{} }).
				OnDrain(func(context.Context, *Repo) error {
					ran.Store(true)
					_, resolveErr = req.Resolve[*DB]()
					return nil
				})
			_, err := req.Resolve[*Repo]()
			return err
		})
	if err := root.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := root.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ran.Load() {
		t.Fatal("the scope opened during drain was never drained")
	}
	if resolveErr != nil {
		t.Fatalf("its drain hook could not reach its dependency: %v", resolveErr)
	}
}

// A drain hook may build into a scope the sweep has already visited, and
// that instance owes a drain like any other. Found by the concurrent driver's
// drain oracle rather than the review.
// (review 2, 10)
func TestReview2LateBuildIntoSweptChildIsDrained(t *testing.T) {
	var mu sync.Mutex
	var log []string
	note := func(s string) { mu.Lock(); log = append(log, s); mu.Unlock() }

	root := di.New()
	kid := root.Child("kid")
	kid.Provide(func(*di.Scope) *wQ { return &wQ{} }).
		OnDrain(func(context.Context, *wQ) error { note("drain late"); return nil }).
		OnStop(func(context.Context, *wQ) error { note("stop late"); return nil })

	root.Value(&Worker{}).Eager().
		OnDrain(func(context.Context, *Worker) error {
			note("drain root")
			// The child was swept before this hook ran: it had nothing in it.
			if _, err := kid.Resolve[*wQ](); err != nil {
				t.Errorf("resolve into a child during drain: %v", err)
			}
			return nil
		})

	if err := root.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := root.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"drain root", "drain late", "stop late"}
	if !slices.Equal(log, want) {
		t.Fatalf("got %v, want %v", log, want)
	}
}

// A sweep revisiting a scope whose own phase has ended can be draining a late
// instance there as that scope's Stop reaches it. The scope-wide phase has
// ended, and must have, so Stop waits for the hook per instance.
// (review 2, 11)
func TestReview2LateDrainIsNotReleasedUnderneath(t *testing.T) {
	bDraining := make(chan struct{})
	releaseB := make(chan struct{})
	var overlap atomic.Bool
	stopped := make(chan struct{})

	root := di.New()
	kid := root.Child("kid")

	// Scoped, so resolving it through kid puts the instance in kid.
	root.Provide(func(*di.Scope) *oLate { return &oLate{} }).Scoped().
		OnDrain(func(context.Context, *oLate) error {
			close(bDraining)
			<-releaseB
			return nil
		}).
		OnStop(func(context.Context, *oLate) error {
			select {
			case <-releaseB:
			default:
				overlap.Store(true)
			}
			close(stopped)
			return nil
		})

	root.Value(&oRoot{}).Eager().
		OnDrain(func(context.Context, *oRoot) error {
			// kid was swept already, empty; this puts an instance there.
			if _, err := kid.Resolve[*oLate](); err != nil {
				t.Errorf("resolve into the child during drain: %v", err)
			}
			return nil
		})

	if err := root.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	rootStop := make(chan error, 1)
	go func() { rootStop <- root.Stop(context.Background()) }()

	select {
	case <-bDraining:
	case <-time.After(5 * time.Second):
		t.Fatal("the late instance was never drained")
	}

	kidStop := make(chan error, 1)
	go func() { kidStop <- kid.Stop(context.Background()) }()
	time.Sleep(150 * time.Millisecond) // let kid.Stop reach the release

	if overlap.Load() {
		t.Fatal("OnStop ran while the instance's own OnDrain was still holding it")
	}
	close(releaseB)
	for _, ch := range []chan error{kidStop, rootStop} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("Stop never returned")
		}
	}
	<-stopped
	if overlap.Load() {
		t.Fatal("OnStop ran while the instance's own OnDrain was still holding it")
	}
}

// A drain hook may stop a scope that is neither its own nor an ancestor: the
// sweep must not hold a sibling's phase while the hook runs.
// (review 3, 1)
func TestReview3DrainHookCanStopASiblingScope(t *testing.T) {
	root := di.New()
	request := root.Child("request") // earlier than server, so swept later
	server := root.Child("server")

	var stopErr error
	server.Value(&r3Server{}).
		OnDrain(func(ctx context.Context, _ *r3Server) error {
			stopErr = request.Stop(ctx)
			return stopErr
		})
	if _, err := server.Resolve[*r3Server](); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- root.Stop(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("root.Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("root.Stop deadlocked on the sibling scope its drain hook stopped")
	}
	if stopErr != nil {
		t.Errorf("stopping the sibling from the drain hook: %v", stopErr)
	}
}

// A Stop whose context runs out while another Stop's drain hook holds the
// instance still owes the release: it took the instance off the scope's list,
// so nothing else will reach it.
// (review 3, 2)
func TestReview3LostDrainWaitStillReleases(t *testing.T) {
	root := di.New()
	child := root.Child("child")

	inDrain := make(chan struct{})
	release := make(chan struct{})
	stopped := make(chan struct{})

	child.Provide(func(*di.Scope) *r3Late { return &r3Late{} }).
		OnDrain(func(context.Context, *r3Late) error { close(inDrain); <-release; return nil }).
		OnStop(func(context.Context, *r3Late) error { close(stopped); return nil })

	// Built into the child by the root's drain hook, after the child's own
	// phase has ended.
	root.Value(&r3Drainer{}).
		OnDrain(func(context.Context, *r3Drainer) error {
			_, err := child.Resolve[*r3Late]()
			return err
		})
	if _, err := root.Resolve[*r3Drainer](); err != nil {
		t.Fatal(err)
	}

	rootStop := make(chan error, 1)
	go func() { rootStop <- root.Stop(context.Background()) }()

	<-inDrain
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := child.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("child.Stop: want the missed deadline reported, got %v", err)
	}
	select {
	case <-stopped:
		t.Fatal("OnStop ran while the drain hook still held the value")
	default:
	}

	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("OnStop was never run: the release was dropped with the missed deadline")
	}
	if err := <-rootStop; err != nil {
		t.Errorf("root.Stop: %v", err)
	}
}

// A Stop reports the failure of a drain hook of its own scope even when an
// ancestor's Stop owned the phase and ran the hook: a request scope ending
// while the application shuts down is where that matters.
// (review 4, 2)
func TestReview4ChildStopReportsItsOwnDrainFailure(t *testing.T) {
	drainFailed := errors.New("drain failed")
	for range 20 {
		inDrain := make(chan struct{})
		release := make(chan struct{})

		root := di.New()
		child := root.Child("child")
		root.Provide(func(*di.Scope) *Repo { return &Repo{} }).Scoped().
			OnDrain(func(context.Context, *Repo) error {
				close(inDrain)
				<-release
				return drainFailed
			})
		if _, err := child.Resolve[*Repo](); err != nil {
			t.Fatal(err)
		}

		rootErr := make(chan error, 1)
		go func() { rootErr <- root.Stop(context.Background()) }()
		<-inDrain
		childErr := make(chan error, 1)
		go func() { childErr <- child.Stop(context.Background()) }()
		time.Sleep(20 * time.Millisecond) // let the child reach the phase
		close(release)

		if err := <-childErr; !errors.Is(err, drainFailed) {
			t.Fatalf("child.Stop hid its own drain failure: %v", err)
		}
		// Whether the root reports it too depends on whether the child's
		// Stop, released by the settled failure, detaches the child before
		// the root reads its child list. Both orders are correct, and
		// asserting the root hears it failed about one run in eight. The
		// test below pins the case where the root owns the teardown.
		if err := <-rootErr; err != nil && !errors.Is(err, drainFailed) {
			t.Fatalf("root.Stop: %v", err)
		}
	}
}

// A Stop that owns a child scope's teardown reports the failure of a drain
// hook that ran in that child. This is the half of the rule above that does
// not depend on an interleaving.
// (review 5, 1)
func TestReview5RootStopReportsAChildsDrainFailure(t *testing.T) {
	drainFailed := errors.New("drain failed")
	root := di.New()
	child := root.Child("child")
	// Scoped, so resolving it through the child puts the instance there.
	root.Provide(func(*di.Scope) *Repo { return &Repo{} }).Scoped().
		OnDrain(func(context.Context, *Repo) error { return drainFailed })
	if _, err := child.Resolve[*Repo](); err != nil {
		t.Fatal(err)
	}
	if err := root.Stop(context.Background()); !errors.Is(err, drainFailed) {
		t.Fatalf("root.Stop: want the child's drain failure, got %v", err)
	}
}

// A service built and waiting for Start to claim its start step owes a drain
// as soon as it starts, and the sweep must not decide otherwise for good.
// Start's loop is held on an earlier OnStart while a concurrent Stop drains,
// and a drain hook further down the sweep lets it go.
// (review 6)
func TestReview6ServiceStartedDuringDrainIsDrained(t *testing.T) {
	releaseA := make(chan struct{})
	aEntered := make(chan struct{})
	bStarted := make(chan struct{})
	var bDrained, bStopped atomic.Int32

	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).Eager().
		OnDrain(func(context.Context, *DB) error {
			close(releaseA)
			select {
			case <-bStarted:
			case <-time.After(5 * time.Second):
				return errors.New("the waiting service never started")
			}
			return nil
		})
	s.Provide(func(*di.Scope) *Repo { return &Repo{} }).Eager().
		OnStart(func(context.Context, *Repo) error { close(aEntered); <-releaseA; return nil })
	s.Provide(func(*di.Scope) *Worker { return &Worker{} }).Eager().
		OnStart(func(context.Context, *Worker) error { close(bStarted); return nil }).
		OnDrain(func(context.Context, *Worker) error { bDrained.Add(1); return nil }).
		OnStop(func(context.Context, *Worker) error {
			if bDrained.Load() == 0 {
				return errors.New("OnStop ran before OnDrain")
			}
			bStopped.Add(1)
			return nil
		})

	startErr := make(chan error, 1)
	go func() { startErr <- s.Start(context.Background()) }()
	select {
	case <-aEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("Start never reached the blocking OnStart")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-startErr:
	case <-time.After(5 * time.Second):
		t.Fatal("Start never returned")
	}
	if bDrained.Load() != 1 || bStopped.Load() != 1 {
		t.Fatalf("drained=%d stopped=%d, want 1 and 1", bDrained.Load(), bStopped.Load())
	}
}
