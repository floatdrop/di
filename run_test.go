package di_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/floatdrop/di"
)

func TestStartRollsBackOnFailure(t *testing.T) {
	var log []string
	boom := errors.New("boom")
	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).Eager().
		OnStart(func(context.Context, *DB) error { log = append(log, "start db"); return nil }).
		OnStop(func(context.Context, *DB) error { log = append(log, "stop db"); return nil })
	s.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} }).Eager().
		OnStart(func(context.Context, *Repo) error { return boom }).
		OnStop(func(context.Context, *Repo) error { log = append(log, "stop repo"); return nil })

	err := s.Start(t.Context())
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if got := strings.Join(log, ","); got != "start db,stop db" {
		t.Fatalf("rollback order %q", got)
	}
	if err := s.Stop(t.Context()); err != nil || len(log) != 2 {
		t.Fatalf("Stop after failed Start must be a no-op, log=%v err=%v", log, err)
	}
}

func TestStopStopsChildrenFirst(t *testing.T) {
	var log []string
	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).
		OnStop(func(context.Context, *DB) error { log = append(log, "stop db"); return nil })
	child := s.Child("request")
	child.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} }).
		OnStop(func(context.Context, *Repo) error { log = append(log, "stop repo"); return nil })
	child.Get[*Repo]()

	if err := s.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); got != "stop repo,stop db" {
		t.Fatalf("order %q", got)
	}
}

func TestRunReturnsShutdownError(t *testing.T) {
	s := di.New()
	cause := errors.New("listener died")
	done := make(chan error, 1)
	go func() { done <- s.Run(t.Context()) }()
	time.Sleep(10 * time.Millisecond)
	s.Shutdown(cause)
	s.Shutdown(errors.New("ignored")) // first call wins
	select {
	case err := <-done:
		if !errors.Is(err, cause) || strings.Contains(err.Error(), "ignored") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	var stopped bool
	s := di.New()
	s.Value(&DB{}).OnStop(func(context.Context, *DB) error { stopped = true; return nil })
	s.Get[*DB]()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("OnStop did not run")
	}
}

// StartTimeout bounds the start: a hook that respects its context fails with
// the deadline and the rollback stops what had started. (fx review)
func TestRunStartTimeout(t *testing.T) {
	var log []string
	s := di.New()
	s.Value(&DB{}).Eager().
		OnStop(func(context.Context, *DB) error { log = append(log, "stop db"); return nil })
	s.Value(&Worker{}).Eager().
		OnStart(func(ctx context.Context, _ *Worker) error { <-ctx.Done(); return ctx.Err() })

	err := s.Run(t.Context(), di.StartTimeout(20*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if strings.Join(log, ",") != "stop db" {
		t.Fatalf("rollback ran %v", log)
	}
}

// The phase is checked between steps, so a hook that ignores its context
// bounds the start too. (fx review)
func TestRunStartTimeoutEndsBetweenSteps(t *testing.T) {
	var log []string
	s := di.New()
	s.Value(&DB{}).Eager().
		OnStart(func(context.Context, *DB) error { time.Sleep(40 * time.Millisecond); return nil })
	s.Value(&Worker{}).Eager().
		OnStart(func(context.Context, *Worker) error { log = append(log, "start worker"); return nil })

	err := s.Run(t.Context(), di.StartTimeout(10*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if len(log) != 0 {
		t.Fatalf("the start went on past its deadline: %v", log)
	}
}

// The eager builds are bounded too, between one constructor and the next: a
// constructor reads Scope.Context, so that is the only place the phase can
// end one. (fx review)
func TestRunStartTimeoutEndsBetweenEagerBuilds(t *testing.T) {
	var built []string
	s := di.New()
	s.Provide(func(*di.Scope) *DB {
		time.Sleep(40 * time.Millisecond)
		built = append(built, "db")
		return &DB{}
	}).Eager()
	s.Provide(func(*di.Scope) *Worker { built = append(built, "worker"); return &Worker{} }).Eager()

	err := s.Run(t.Context(), di.StartTimeout(10*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if strings.Join(built, ",") != "db" {
		t.Fatalf("the eager builds went on past the deadline: %v", built)
	}
}

// The deadline is the default, and StartTimeout(0) takes it off. What the
// hook sees is the whole of it: a constructor reads Scope.Context. (fx review)
func TestStartTimeoutDefaultAndOff(t *testing.T) {
	bounded := func(opts ...di.RunOption) bool {
		var deadline bool
		s := di.New()
		s.Value(&DB{}).Eager().OnStart(func(ctx context.Context, _ *DB) error {
			_, deadline = ctx.Deadline()
			s.Shutdown(nil)
			return nil
		})
		if err := s.Run(t.Context(), opts...); err != nil {
			t.Fatal(err)
		}
		return deadline
	}
	if !bounded() {
		t.Error("the start phase is unbounded by default")
	}
	if bounded(di.StartTimeout(0)) {
		t.Error("StartTimeout(0) still bounded the start phase")
	}
}

// The start deadline bounds the phase, not the application: neither the
// scope's context nor a worker's carries it. (fx review)
func TestStartTimeoutDoesNotOutliveTheStart(t *testing.T) {
	cancelled := make(chan struct{})
	s := di.New()
	s.Value(&Worker{}).Eager().Go(func(ctx context.Context, _ *Worker) error {
		<-ctx.Done() // Stop cancels it; the start deadline must not
		close(cancelled)
		return nil
	})

	done := make(chan error, 1)
	go func() { done <- s.Run(t.Context(), di.StartTimeout(20*time.Millisecond)) }()
	time.Sleep(60 * time.Millisecond) // well past the deadline

	select {
	case <-cancelled:
		t.Fatal("the start deadline cancelled the worker")
	default:
	}
	if _, ok := s.Context().Deadline(); ok {
		t.Fatal("the scope kept the start phase's deadline")
	}

	s.Shutdown(nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("Stop did not cancel the worker")
	}
}

func TestRunReturnsStartError(t *testing.T) {
	s := di.New()
	boom := errors.New("boom")
	s.Value(&DB{}).Eager().OnStart(func(context.Context, *DB) error { return boom })
	if err := s.Run(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestShutdownFromChildReachesRoot(t *testing.T) {
	s := di.New()
	child := s.Child("worker")
	done := make(chan error, 1)
	go func() { done <- s.Run(t.Context()) }()
	time.Sleep(10 * time.Millisecond)
	child.Shutdown(nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestStopTimeoutBoundsHooks(t *testing.T) {
	s := di.New()
	s.Value(&DB{}).OnStop(func(ctx context.Context, _ *DB) error { <-ctx.Done(); return ctx.Err() })
	s.Get[*DB]()
	s.Shutdown(nil)
	start := time.Now()
	err := s.Run(t.Context(), di.StopTimeout(50*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("stop did not respect the timeout")
	}
}

// A real HTTP server: Run must wait for the in-flight request to finish.
func TestRunDrainsHTTPServer(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	addr := make(chan string, 1)

	app := di.New()
	app.Provide(func(*di.Scope) *http.Server {
		return &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release
			_, _ = io.WriteString(w, "done")
		})}
	}).Eager().
		OnStart(func(ctx context.Context, srv *http.Server) error {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return err
			}
			addr <- ln.Addr().String()
			go func() {
				if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
					app.Shutdown(err)
				}
			}()
			return nil
		}).
		OnStop(func(ctx context.Context, srv *http.Server) error { return srv.Shutdown(ctx) })

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(t.Context()) }()

	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + <-addr)
		if err != nil {
			body <- "error: " + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()

	<-entered
	app.Shutdown(nil) // graceful: the in-flight request must still complete
	time.Sleep(20 * time.Millisecond)
	close(release)

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if got := <-body; got != "done" {
		t.Fatalf("request was cut short: %q", got)
	}
}
