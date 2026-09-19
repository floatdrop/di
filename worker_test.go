package di_test

// Regressions in Go hooks and Shutdown: how a worker's own failure reaches
// the caller, and what may still be holding the value when OnStop wants it.
// Tags are explained in fixtures_test.go.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.yandex/di"
)

// A Go hook that dies on its own is reported by Stop, not only by Run.
func TestRegressionWorkerHookErrorReachesStop(t *testing.T) {
	boom := errors.New("queue disconnected")
	s := di.New()
	s.Value(&Worker{}).Eager().Go(func(context.Context, *Worker) error { return boom })
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := s.Stop(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("Stop must report the dead worker, got %v", err)
	}
}

// A worker dying with an error that wraps context.Canceled, while its own
// context is alive, is still reported.
// (pass 2)
func TestRegressionRunErrorWrappingCanceled(t *testing.T) {
	s := di.New()
	s.Value(&Worker{}).Eager().
		Go(func(ctx context.Context, _ *Worker) error { return fmt.Errorf("upstream dial: %w", context.Canceled) })
	done := make(chan error, 1)
	go func() { done <- s.Run(t.Context()) }()
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
		Go(func(context.Context, *Worker) error { defer close(failed); return errors.New("worker died") })

	runDone := make(chan error, 1)
	go func() { runDone <- root.Run(t.Context(), di.StopTimeout(time.Second)) }()
	if err := child.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-failed
	_ = child.Stop(t.Context()) // detaches before the root gets there

	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "worker died") {
			t.Fatalf("root.Run returned %v, want the worker failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("root.Run did not return")
	}
}

// The same failure reported by both routes is reported once. This passes
// before the fix as well: a dying worker recorded no cause then, so there was
// nothing to duplicate; it guards the fix for 9 rather than a defect.
// (review 1, 9b)
func TestReviewWorkerFailureIsNotDuplicated(t *testing.T) {
	boom := errors.New("queue disconnected")
	s := di.New()
	s.Value(&Worker{}).Eager().Go(func(context.Context, *Worker) error { return boom })
	err := s.Run(t.Context())
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if n := strings.Count(err.Error(), boom.Error()); n != 1 {
		t.Fatalf("the worker failure is listed %d times:\n%v", n, err)
	}
}

// OnStop must not run while a Go hook that outlasted Stop's context is still
// using the value: Stop reports the missed deadline and the release follows
// the worker's return.
// (review 2, 9)
func TestReview2OnStopWaitsForALiveWorkerHook(t *testing.T) {
	runLive := make(chan struct{})
	release := make(chan struct{})
	stopped := make(chan struct{})
	var overlap atomic.Bool

	root := di.New()
	root.Value(&Worker{}).Eager().
		Go(func(context.Context, *Worker) error { close(runLive); <-release; return nil }).
		OnStop(func(context.Context, *Worker) error {
			select {
			case <-release:
			default:
				overlap.Store(true)
			}
			close(stopped)
			return nil
		})
	if err := root.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-runLive

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err := root.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "did not return") {
		t.Fatalf("got %v", err)
	}
	select {
	case <-stopped:
		t.Fatal("OnStop ran while the Go hook was still live")
	default:
	}
	close(release)

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the release never happened")
	}
	if overlap.Load() {
		t.Fatal("OnStop ran while the Go hook was still live")
	}
}

// A worker that dies during the stop publishes its cause after Run has woken
// for a cancelled context, so Run reads it once more on the way out.
// (review 3, 3)
func TestReview3RunReportsAShutdownPublishedDuringStop(t *testing.T) {
	fail := errors.New("worker died")

	root := di.New()
	child := root.Child("worker")
	child.Value(&r3Worker{}).
		Go(func(ctx context.Context, _ *r3Worker) error {
			<-ctx.Done()
			root.Shutdown(fail)
			return fail
		})
	if _, err := child.Resolve[*r3Worker](); err != nil {
		t.Fatal(err)
	}
	// The hook handles the child's error itself, so the failure reaches Run
	// only as the shutdown cause.
	root.Value(&r3Drainer{}).Eager().
		OnDrain(func(ctx context.Context, _ *r3Drainer) error {
			_ = child.Stop(ctx)
			return nil
		})

	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := root.Run(ctx, di.StopTimeout(5*time.Second)); !errors.Is(err, fail) {
		t.Errorf("Run: want the published worker failure, got %v", err)
	}
}

// Run joins a cause published through Shutdown on the way out of a failed
// Start too: a rollback runs the hooks, so a worker can die there.
// (review 4, 1)
func TestReview4RunReportsACausePublishedDuringRollback(t *testing.T) {
	fail := errors.New("worker died")
	boom := errors.New("start failed")

	root := di.New()
	child := root.Child("worker")
	child.Value(&Worker{}).Go(func(ctx context.Context, _ *Worker) error {
		<-ctx.Done()
		root.Shutdown(fail)
		return fail
	})
	if _, err := child.Resolve[*Worker](); err != nil {
		t.Fatal(err)
	}
	if err := child.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The hook handles the child's error itself, so the failure reaches Run
	// only as the published cause.
	root.Value(&DB{}).Eager().
		OnDrain(func(ctx context.Context, _ *DB) error { _ = child.Stop(ctx); return nil })
	root.Value(&Repo{}).Eager().
		OnStart(func(context.Context, *Repo) error { return boom })

	err := root.Run(t.Context(), di.StopTimeout(5*time.Second))
	if !errors.Is(err, boom) {
		t.Fatalf("Run: want the start failure, got %v", err)
	}
	if !errors.Is(err, fail) {
		t.Fatalf("Run dropped the worker failure published during the rollback: %v", err)
	}
}

// A worker cancelled by Stop may report a failure joined with the
// cancellation; only an error that says nothing beyond the cancellation is
// dropped. (issue 35)
func TestWorkerFailureJoinedWithCancellationIsReported(t *testing.T) {
	failure := errors.New("flush failed")
	s := di.New()
	s.Value(&Worker{}).Eager().Go(func(ctx context.Context, _ *Worker) error {
		<-ctx.Done()
		return errors.Join(ctx.Err(), failure)
	})
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := s.Stop(ctx); !errors.Is(err, failure) {
		t.Fatalf("Stop dropped the worker's own failure: %v", err)
	}

	// A wrapped cancellation with nothing else in it is nothing to report.
	quiet := di.New()
	quiet.Value(&Worker{}).Eager().Go(func(ctx context.Context, _ *Worker) error {
		<-ctx.Done()
		return fmt.Errorf("loop: %w", ctx.Err())
	})
	if err := quiet.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := quiet.Stop(ctx); err != nil {
		t.Fatalf("a bare cancellation is not a failure: %v", err)
	}
}

// A worker that panics is a worker that failed: the panic becomes its error,
// Shutdown receives it, Run returns it, and OnStop still runs. Checked against
// c44198f.
func TestPanickingWorkerIsAFailure(t *testing.T) {
	var stops atomic.Int32
	var events []di.Event
	s := di.New()
	s.Observe(func(ev di.Event) {
		if ev.Kind == di.EventStop {
			events = append(events, ev)
		}
	})
	s.Value(&Worker{}).Eager().
		Go(func(context.Context, *Worker) error { panic("consumer exploded") }).
		OnStop(func(context.Context, *Worker) error { stops.Add(1); return nil })
	done := make(chan error, 1)
	go func() { done <- s.Run(t.Context()) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a panicking worker did not stop the application")
	}
	if err == nil || !strings.Contains(err.Error(), "consumer exploded") {
		t.Fatalf("Run must report the panic as the worker's failure, got %v", err)
	}
	if stops.Load() != 1 {
		t.Fatalf("OnStop ran %d times, want 1", stops.Load())
	}
	if len(events) != 1 || events[0].Err == nil {
		t.Fatalf("stop event %+v, want one carrying the failure", events)
	}
}
