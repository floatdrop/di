package di_test

// Regressions in Run hooks and Shutdown: how a worker's own failure reaches
// the caller, and what may still be holding the value when OnStop wants it.
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
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floatdrop/di"
)

// A Run hook that dies on its own is reported by Stop, not only by Run.
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

// A worker dying with an error that wraps context.Canceled, while its own
// context is alive, is still reported.
// (pass 2)
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

// A worker that dies in a child scope must reach the root's Run even when
// the child is stopped and detached before the root reacts.
// (review 1, 9)
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

// The same failure reported by both routes is still reported once. This
// one passes before the fix as well: a dying worker did not record a cause
// there, so there was nothing to duplicate. It guards the fix for 9, which
// makes it record one, rather than covering a defect of its own.
// (review 1, 9b)
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

// Reported alongside the six: OnStop must not run while a Run hook that
// outlasted Stop's context is still using the value. Stop reports the missed
// deadline and the release follows the worker's own return.
// (review 2, 9)
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

// Shutdown records the cause Run should return. A worker that dies during
// the stop publishes it after Run has already woken for a cancelled context,
// so Run reads it once more on the way out rather than leaving it to whichever
// Stop observed the failure.
// (review 3, 3)
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

// Run joins a cause published through Shutdown on the way out of a failed
// Start, not only on the way out of an ordinary stop. A rollback runs the
// drain and stop hooks, so a worker can die there exactly as it can during a
// shutdown, and the error branch used to return before the cause was read.
// (review 4, 1)
func TestReview4RunReportsACausePublishedDuringRollback(t *testing.T) {
	fail := errors.New("worker died")
	boom := errors.New("start failed")

	root := di.New()
	child := root.Child("worker")
	child.Value(&Worker{}).Run(func(ctx context.Context, _ *Worker) error {
		<-ctx.Done()
		root.Shutdown(fail)
		return fail
	})
	if _, err := child.Resolve[*Worker](); err != nil {
		t.Fatal(err)
	}
	if err := child.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A drain hook that stops the child and handles its error itself, so the
	// failure reaches Run only as the published cause.
	root.Value(&DB{}).Eager().
		OnDrain(func(ctx context.Context, _ *DB) error { _ = child.Stop(ctx); return nil })
	root.Value(&Repo{}).Eager().
		OnStart(func(context.Context, *Repo) error { return boom })

	err := root.Run(context.Background(), di.StopTimeout(5*time.Second))
	if !errors.Is(err, boom) {
		t.Fatalf("Run: want the start failure, got %v", err)
	}
	if !errors.Is(err, fail) {
		t.Fatalf("Run dropped the worker failure published during the rollback: %v", err)
	}
}
