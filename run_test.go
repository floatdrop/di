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

	err := s.Start(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if got := strings.Join(log, ","); got != "start db,stop db" {
		t.Fatalf("rollback order %q", got)
	}
	if err := s.Stop(context.Background()); err != nil || len(log) != 2 {
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

	if err := s.Stop(context.Background()); err != nil {
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
	go func() { done <- s.Run(context.Background()) }()
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("OnStop did not run")
	}
}

func TestRunReturnsStartError(t *testing.T) {
	s := di.New()
	boom := errors.New("boom")
	s.Value(&DB{}).Eager().OnStart(func(context.Context, *DB) error { return boom })
	if err := s.Run(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestShutdownFromChildReachesRoot(t *testing.T) {
	s := di.New()
	child := s.Child("worker")
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
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
	err := s.Run(context.Background(), di.StopTimeout(50*time.Millisecond))
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
			io.WriteString(w, "done")
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
	go func() { runErr <- app.Run(context.Background()) }()

	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + <-addr)
		if err != nil {
			body <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
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
