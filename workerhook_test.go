package di_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/floatdrop/di"
)

type Worker struct{}

func TestWorkerHookLifecycle(t *testing.T) {
	var log []string
	stopped := make(chan struct{})
	s := di.New()
	s.Provide(func(*di.Scope) *Worker { return &Worker{} }).Eager().
		Worker(func(ctx context.Context, w *Worker) error {
			log = append(log, "run")
			<-ctx.Done()
			close(stopped)
			return ctx.Err() // context.Canceled is not reported as a failure
		}).
		OnStop(func(context.Context, *Worker) error { log = append(log, "stop"); return nil })

	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("Stop returned before the Worker hook was cancelled")
	}
	if got := strings.Join(log, ","); got != "run,stop" {
		t.Fatalf("order %q", got)
	}
}

func TestWorkerHookFailureStopsApplication(t *testing.T) {
	boom := errors.New("queue disconnected")
	s := di.New()
	s.Provide(func(*di.Scope) *Worker { return &Worker{} }).Eager().
		Worker(func(ctx context.Context, w *Worker) error { return boom })
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), "di_test.Worker") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after the worker failed")
	}
}

func TestWorkerHookErrorAfterCancelIsReportedByStop(t *testing.T) {
	flushFailed := errors.New("flush failed")
	s := di.New()
	s.Value(&Worker{}).Eager().Worker(func(ctx context.Context, w *Worker) error { <-ctx.Done(); return flushFailed })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background()); !errors.Is(err, flushFailed) {
		t.Fatalf("got %v", err)
	}
}

func TestWorkerHookIgnoringCancelHitsStopTimeout(t *testing.T) {
	s := di.New()
	s.Value(&Worker{}).Eager().Worker(func(ctx context.Context, w *Worker) error {
		time.Sleep(2 * time.Second)
		return nil
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := s.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "did not return") {
		t.Fatalf("got %v", err)
	}
}

func TestWorkerHookStartsForLateBuiltService(t *testing.T) {
	running := make(chan struct{})
	s := di.New()
	s.Provide(func(*di.Scope) *Worker { return &Worker{} }).
		Worker(func(ctx context.Context, w *Worker) error { close(running); <-ctx.Done(); return nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Get[*Worker]()
	select {
	case <-running:
	case <-time.After(time.Second):
		t.Fatal("Worker hook not started for a service built after Start")
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A worker may fail on its own, keep flushing until it is told to stop, and
// only then report what went wrong. That error owes nothing to the
// cancellation it arrived after, so Run must still learn the cause -- which
// no reading of the run context can establish, since by then it is cancelled
// either way. The flaky sibling of this test,
// TestReviewDetachedChildWorkerFailureReachesRun, only ever failed because
// the answer was read from the context twice.
func TestWorkerHookFailureDecidedBeforeCancelReachesRun(t *testing.T) {
	boom := errors.New("queue disconnected")
	failed := make(chan struct{})

	root := di.New()
	child := root.Child("c")
	child.Provide(func(*di.Scope) *Worker { return &Worker{} }).Eager().
		Worker(func(ctx context.Context, _ *Worker) error {
			close(failed) // the failure is decided here
			<-ctx.Done()  // the worker flushes while the scope winds down
			return boom   // and is reported here
		})

	runDone := make(chan error, 1)
	go func() { runDone <- root.Run(context.Background(), di.StopTimeout(time.Second)) }()
	if err := child.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-failed
	_ = child.Stop(context.Background()) // detaches, and discards what it reports

	select {
	case err := <-runDone:
		if !errors.Is(err, boom) {
			t.Fatalf("root.Run returned %v, want the worker failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("root.Run did not return")
	}
}

// A worker cancelled by an orderly Stop, reporting only that, is not a
// failure: nothing calls Shutdown and Stop returns cleanly. This is the case
// the surviving guard is for, and the one an unconditional Shutdown would get
// wrong.
func TestWorkerHookCancellationIsNotAFailure(t *testing.T) {
	root := di.New()
	root.Value(&Worker{}).Eager().
		Worker(func(ctx context.Context, _ *Worker) error { <-ctx.Done(); return ctx.Err() })
	if err := root.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := root.Stop(context.Background()); err != nil {
		t.Fatalf("a cancelled worker was reported as a failure: %v", err)
	}
}
