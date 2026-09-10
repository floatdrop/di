package di_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/dihttp"
)

func kinds(evs []di.Event) string {
	var out []string
	for _, ev := range evs {
		k := string(ev.Kind)
		if ev.Err != nil {
			k += "!"
		}
		out = append(out, k)
	}
	return strings.Join(out, ",")
}

func TestObserveSeesWholeLifecycle(t *testing.T) {
	var evs []di.Event
	s := di.New()
	s.Observe(func(ev di.Event) { evs = append(evs, ev) })
	s.Provide(func(*di.Scope) *DB { return &DB{} }).Eager().
		OnStart(func(context.Context, *DB) error { return nil }).
		OnStop(func(context.Context, *DB) error { return errors.New("close failed") })

	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Shutdown(nil)
	_ = s.Stop(context.Background())

	if got := kinds(evs); got != "build,start,shutdown,stop!" {
		t.Fatalf("got %q", got)
	}
	if evs[0].Service != "*github.com/floatdrop/di_test.DB" || evs[0].Scope != "root" || !strings.Contains(evs[0].Site, "observe_test.go") {
		t.Fatalf("build event %+v", evs[0])
	}
}

func TestObserveOnRootSeesChildAndMiddlewareStopErrors(t *testing.T) {
	var evs []di.Event
	closeFailed := errors.New("close failed")
	app := di.New()
	app.Observe(func(ev di.Event) { evs = append(evs, ev) })
	app.Provide(func(s *di.Scope) *User { return &User{} }).Scoped().
		OnStop(func(context.Context, *User) error { return closeFailed })

	h := dihttp.NewMiddleware(app)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := di.FromContext(r.Context())
		req.Get[*User]()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	if got := kinds(evs); got != "build,stop!" {
		t.Fatalf("got %q", got)
	}
	if evs[1].Scope != "request" || !errors.Is(evs[1].Err, closeFailed) {
		t.Fatalf("middleware stop error not surfaced: %+v", evs[1])
	}
}

// A build that completes after Stop took its snapshot must be rejected with
// ErrStopped and never handed out. Whether its OnStop runs follows the same
// pairing rule as a rollback: a declared OnStart that never ran means the
// OnStop is not owed, while an unpaired OnStop is a destructor and runs.
func TestBuildRacingStopIsUndone(t *testing.T) {
	for _, tc := range []struct {
		name      string
		withStart bool
		wantStops int
	}{
		{"paired OnStart is not owed a stop", true, 0},
		{"unpaired OnStop is a destructor", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			building := make(chan struct{})
			release := make(chan struct{})
			stops := 0
			s := di.New()
			b := s.Provide(func(*di.Scope) *DB { close(building); <-release; return &DB{} })
			if tc.withStart {
				b = b.OnStart(func(context.Context, *DB) error { return nil })
			}
			b.OnStop(func(context.Context, *DB) error { stops++; return nil })
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}

			result := make(chan error, 1)
			go func() { _, err := s.Resolve[*DB](); result <- err }()
			<-building
			if err := s.Stop(context.Background()); err != nil { // snapshot is empty: nothing to stop yet
				t.Fatal(err)
			}
			close(release)

			if err := <-result; !errors.Is(err, di.ErrStopped) {
				t.Fatalf("got %v", err)
			}
			if stops != tc.wantStops {
				t.Fatalf("stops = %d, want %d", stops, tc.wantStops)
			}
		})
	}
}

// Event.Package is the import path of the type Service names, which is what
// lets an observer shorten or group by it without parsing Service. The case
// worth pinning is the pointer: a pointer type is unnamed, so its own
// PkgPath is empty and the path has to come from what it points at.
func TestEventPackage(t *testing.T) {
	var evs []di.Event
	s := di.New()
	s.Observe(func(ev di.Event) { evs = append(evs, ev) })
	s.Value(&DB{})                  // *di_test.DB, a pointer to a named type
	s.Value(map[string]int{"a": 1}) // unnamed: no path to report
	s.Get[*DB]()
	s.Get[map[string]int]()
	s.Shutdown(nil) // names no service, so no package either

	const pkg = "github.com/floatdrop/di_test"
	want := map[string]string{
		"*" + pkg + ".DB": pkg,
		"map[string]int":  "",
		"":                "",
	}
	got := map[string]string{}
	for _, ev := range evs {
		got[ev.Service] = ev.Package
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for service, pkgPath := range want {
		if got[service] != pkgPath {
			t.Errorf("%q: package %q, want %q", service, got[service], pkgPath)
		}
	}
}
