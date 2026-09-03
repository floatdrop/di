package di_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/floatdrop/di"
)

type Worker struct{ ticks int }

func TestRunHookLifecycle(t *testing.T) {
	var log []string
	stopped := make(chan struct{})
	s := di.New()
	s.Provide(func(*di.Scope) *Worker { return &Worker{} }).Eager().
		Run(func(ctx context.Context, w *Worker) error {
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
		t.Fatal("Stop returned before the Run hook was cancelled")
	}
	if got := strings.Join(log, ","); got != "run,stop" {
		t.Fatalf("order %q", got)
	}
}

func TestRunHookFailureStopsApplication(t *testing.T) {
	boom := errors.New("queue disconnected")
	s := di.New()
	s.Provide(func(*di.Scope) *Worker { return &Worker{} }).Eager().
		Run(func(ctx context.Context, w *Worker) error { return boom })
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

func TestRunHookErrorAfterCancelIsReportedByStop(t *testing.T) {
	flushFailed := errors.New("flush failed")
	s := di.New()
	s.Value(&Worker{}).Eager().Run(func(ctx context.Context, w *Worker) error { <-ctx.Done(); return flushFailed })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background()); !errors.Is(err, flushFailed) {
		t.Fatalf("got %v", err)
	}
}

func TestRunHookIgnoringCancelHitsStopTimeout(t *testing.T) {
	s := di.New()
	s.Value(&Worker{}).Eager().Run(func(ctx context.Context, w *Worker) error {
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

func TestRunHookStartsForLateBuiltService(t *testing.T) {
	running := make(chan struct{})
	s := di.New()
	s.Provide(func(*di.Scope) *Worker { return &Worker{} }).
		Run(func(ctx context.Context, w *Worker) error { close(running); <-ctx.Done(); return nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Get[*Worker]()
	select {
	case <-running:
	case <-time.After(time.Second):
		t.Fatal("Run hook not started for a service built after Start")
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
