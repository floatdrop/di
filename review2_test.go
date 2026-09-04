package di_test

// Regression tests for the six defects of the second September 2026 review,
// plus the Run-hook overlap reported alongside them.
//
// Every test here was checked against the commit before the fix (2b8915d) and
// fails there. The drain ones fail either by reporting or by hanging, which is
// why each bounds its own wait instead of relying on the package timeout.

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

type wA struct{ sc *di.Scope }
type wB struct{ a *wA }
type wT struct{}
type wU struct{ i wI }
type wQ struct{}

type wI interface{ tag() string }

func (*wT) tag() string { return "T" }

// 1. A Stop that reaches a scope whose drain another Stop is running must wait
// for that drain, not see the flag, skip it and start releasing underneath the
// hook still using the value.
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

// 2. A scope must not mark itself stopped while a descendant's drain, owned by
// another Stop, is still running: those hooks resolve.
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

// 3. Draining is the phase during which the scope still resolves, so a service
// first built by a drain hook has to be drained too, not stopped undrained.
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

// 4. A child scope opened by a drain hook -- an in-flight request taking its
// request scope -- is drained before the parent goes stopped, so its own hooks
// can still resolve.
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

// 5. A constructor may keep the Scope it was handed, which is how a goroutine
// it starts resolves later. A resolution made through that Scope after the
// constructor returned is not a cycle, and must not poison the instance it
// builds for every later resolution either.
func TestReview2RetainedScopeIsNotACycle(t *testing.T) {
	root := di.New()
	root.Provide(func(sc *di.Scope) *wA { return &wA{sc: sc} })
	root.Provide(func(sc *di.Scope) *wB { return &wB{a: sc.Get[*wA]()} })

	a := root.Get[*wA]()
	b, err := a.sc.Resolve[*wB]()
	if err != nil {
		t.Fatalf("deferred resolve through the retained Scope: %v", err)
	}
	if b.a != a {
		t.Fatal("it did not see the finished instance")
	}
	if _, err := root.Resolve[*wB](); err != nil {
		t.Fatalf("a later clean resolve inherited the failure: %v", err)
	}
}

// 6. One alias to a Scoped target is a different edge in each scope that holds
// an instance of that target, so reaching it twice at two holders is not a
// cycle. wT takes an optional per-scope decoration, present only in the child,
// which is what makes the root's own wT a leaf.
func TestReview2AliasAcrossScopedHolders(t *testing.T) {
	root := di.New()
	root.Provide(func(sc *di.Scope) *wT {
		if _, ok := sc.Maybe[*wQ](); ok {
			_ = sc.Get[*wU]()
		}
		return &wT{}
	}).Scoped()
	root.Bind[wI, *wT]()
	root.Provide(func(sc *di.Scope) *wU { return &wU{i: sc.Get[wI]()} })

	kid := root.Child("kid")
	kid.Value(&wQ{})

	v, err := kid.Resolve[wI]()
	if err != nil {
		t.Fatalf("acyclic graph reported as a cycle: %v", err)
	}
	if v.tag() != "T" {
		t.Fatalf("got %q", v.tag())
	}
}

// 7. A scope that has stopped refuses to resolve, including a resolution that
// was already waiting on a constructor running in a live ancestor.
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

// 8. A panicking start hook is a failed start: the service is not served, the
// panic reaches Resolve as an error, and no OnStop is paired with the OnStart
// that never finished.
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

// 9. Reported alongside the six: OnStop must not run while a Run hook that
// outlasted Stop's context is still using the value. Stop reports the missed
// deadline and the release follows the worker's own return.
func TestReview2OnStopWaitsForALiveRunHook(t *testing.T) {
	runLive := make(chan struct{})
	release := make(chan struct{})
	stopped := make(chan struct{})
	var overlap atomic.Bool

	root := di.New()
	root.Value(&Worker{}).Eager().
		Run(func(context.Context, *Worker) error { close(runLive); <-release; return nil }).
		OnStop(func(context.Context, *Worker) error {
			select {
			case <-release:
			default:
				overlap.Store(true)
			}
			close(stopped)
			return nil
		})
	if err := root.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-runLive

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := root.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "did not return") {
		t.Fatalf("got %v", err)
	}
	select {
	case <-stopped:
		t.Fatal("OnStop ran while the Run hook was still live")
	default:
	}
	close(release)

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the release never happened")
	}
	if overlap.Load() {
		t.Fatal("OnStop ran while the Run hook was still live")
	}
}
