package di_test

// Regression tests for the six defects of the third September 2026 review.
//
// Every test here was checked against the commit before the fix (9ace680) and
// fails there, except TestReview3ChildKeptPastTheConstructorIsIndependent,
// which passes both before and after: it guards the other half of the Child
// change rather than a defect. Three of them fail there by hanging rather than
// by reporting, which is why each bounds its own wait instead of relying on
// the package timeout.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/floatdrop/di"
)

type r3Server struct{}
type r3Late struct{}
type r3Worker struct{}
type r3Drainer struct{}
type r3A struct{}
type r3B struct{ sc *di.Scope }
type r3C struct{}
type r3T struct{}
type r3Self struct{}
type r3Plain struct{}

// 1. A drain hook may stop a scope that is neither its own nor an ancestor of
// it. The sweep used to claim every descendant's drain phase before running a
// single hook, so a hook that stopped a sibling waited for a phase only its
// own blocked walk could end.
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

// 2. A Stop whose context runs out while another Stop's drain hook holds the
// instance still owes the release: it took the instance off the scope's list,
// so nothing else will reach it. The release is finished off the hook's own
// return, as it is for a Run hook that outlasts the same deadline.
func TestReview3LostDrainWaitStillReleases(t *testing.T) {
	root := di.New()
	child := root.Child("child")

	inDrain := make(chan struct{})
	release := make(chan struct{})
	stopped := make(chan struct{})

	child.Provide(func(*di.Scope) *r3Late { return &r3Late{} }).
		OnDrain(func(context.Context, *r3Late) error { close(inDrain); <-release; return nil }).
		OnStop(func(context.Context, *r3Late) error { close(stopped); return nil })

	// Built into the child by a hook of the root's own drain, so it appears
	// after the child's drain phase has already ended and is drained by a
	// later sweep of the run above it.
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

// 3. Shutdown records the cause Run should return. A worker that dies during
// the stop publishes it after Run has already woken for a cancelled context,
// so Run reads it once more on the way out rather than leaving it to whichever
// Stop observed the failure.
func TestReview3RunReportsAShutdownPublishedDuringStop(t *testing.T) {
	fail := errors.New("worker died")

	root := di.New()
	child := root.Child("worker")
	child.Value(&r3Worker{}).
		Run(func(ctx context.Context, _ *r3Worker) error {
			<-ctx.Done()
			root.Shutdown(fail)
			return fail
		})
	if _, err := child.Resolve[*r3Worker](); err != nil {
		t.Fatal(err)
	}
	// A hook that stops the child itself and handles its error: the failure
	// then reaches nothing Run looks at except the shutdown cause.
	root.Value(&r3Drainer{}).Eager().
		OnDrain(func(ctx context.Context, _ *r3Drainer) error {
			_ = child.Stop(ctx)
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := root.Run(ctx, di.StopTimeout(5*time.Second)); !errors.Is(err, fail) {
		t.Errorf("Run: want the published worker failure, got %v", err)
	}
}

// 4. A resolution made through the Scope a finished constructor kept is a new
// branch: the ancestors above that constructor are still building, but not for
// it, so it has only to wait for them. Reporting a cycle also cached the
// verdict on whatever it was building, which outlived the timing that caused
// it.
func TestReview3LateResolutionThroughAFinishedConstructorIsNotACycle(t *testing.T) {
	root := di.New()
	built := make(chan struct{})
	finishA := make(chan struct{})
	var saved *di.Scope

	root.Provide(func(s *di.Scope) *r3A {
		s.Get[*r3B]()
		close(built) // B's constructor has returned; A is still building
		<-finishA
		return &r3A{}
	})
	root.Provide(func(s *di.Scope) *r3B { saved = s; return &r3B{sc: s} })
	root.Provide(func(s *di.Scope) *r3C { s.Get[*r3A](); return &r3C{} })

	a := make(chan error, 1)
	go func() { _, err := root.Resolve[*r3A](); a <- err }()
	<-built

	c := make(chan error, 1)
	go func() { _, err := saved.Resolve[*r3C](); c <- err }()

	select {
	case err := <-c:
		t.Fatalf("C resolved without waiting for A: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finishA)

	select {
	case err := <-c:
		if err != nil {
			t.Errorf("C through the kept Scope: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("C never returned")
	}
	if err := <-a; err != nil {
		t.Fatalf("A: %v", err)
	}
	if _, err := root.Resolve[*r3C](); err != nil {
		t.Errorf("second resolve of C: %v", err) // the verdict used to be cached
	}
}

// 5. A Transient constructor that finishes after its scope has stopped must
// not hand the value back: the transient branch skipped the check await makes
// after its wait.
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

// 6. A child scope made inside a constructor carries that constructor's
// resolution, so a request through it that leads back to the service being
// built is a cycle. Starting a fresh path there left the two halves waiting
// for each other with nothing to report it.
func TestReview3ChildMadeInAConstructorKeepsTheCyclePath(t *testing.T) {
	root := di.New()
	inner := make(chan error, 1)
	root.Provide(func(s *di.Scope) *r3Self {
		_, err := s.Child("request").Resolve[*r3Self]()
		inner <- err
		return &r3Self{}
	})

	done := make(chan struct{})
	go func() { defer close(done); _, _ = root.Resolve[*r3Self]() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resolving through a child made in the constructor deadlocked")
	}
	if err := <-inner; !errors.Is(err, di.ErrCycle) {
		t.Errorf("want ErrCycle, got %v", err)
	}
}

// The other half of that: a child kept past the constructor resolves as an
// independent branch, and reports failure the way a top-level call does rather
// than unwinding to a call that has long returned.
func TestReview3ChildKeptPastTheConstructorIsIndependent(t *testing.T) {
	root := di.New()
	var kept *di.Scope
	root.Provide(func(s *di.Scope) *r3Self { kept = s.Child("request"); return &r3Self{} })
	root.Value(&r3Plain{})
	if _, err := root.Resolve[*r3Self](); err != nil {
		t.Fatal(err)
	}

	if _, err := kept.Resolve[*r3Plain](); err != nil {
		t.Errorf("resolving through the kept child: %v", err)
	}
	if v := kept.Get[*r3Plain](); v == nil {
		t.Error("Get through the kept child returned nothing")
	}
	func() {
		defer func() {
			rec := recover()
			if err, ok := rec.(error); !ok || !errors.Is(err, di.ErrNotProvided) {
				t.Errorf("want a panic carrying ErrNotProvided, got %#v", rec)
			}
		}()
		kept.Get[*r3C]()
	}()
}
