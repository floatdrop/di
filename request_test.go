package di_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/floatdrop/di"
)

type User struct{ Name string }

func TestWithScopeRoundTrip(t *testing.T) {
	if _, ok := di.FromContext(context.Background()); ok {
		t.Fatal("empty context must have no scope")
	}
	s := di.New()
	got, ok := di.FromContext(di.WithScope(context.Background(), s))
	if !ok || got != s {
		t.Fatal("scope not round-tripped")
	}
}

func TestMiddlewareGivesEachRequestAScope(t *testing.T) {
	app := di.New()
	app.Provide(func(*di.Scope) *DB { return &DB{dsn: "shared"} })
	// Registered in the root but request-dependent: resolved per request
	// because the *http.Request only exists in the child scope.
	stops := 0
	app.Provide(func(s *di.Scope) *User {
		return &User{Name: s.Get[*http.Request]().Header.Get("X-User")}
	})

	var seen []string
	h := app.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, ok := di.FromContext(r.Context())
		if !ok {
			t.Fatal("no scope in request context")
		}
		req.Provide(func(s *di.Scope) *User {
			return &User{Name: s.Get[*http.Request]().Header.Get("X-User")}
		}).OnStop(func(context.Context, *User) error { stops++; return nil })
		seen = append(seen, req.Get[*User]().Name)
		if req.Get[*DB]() != app.Get[*DB]() {
			t.Fatal("request scope must share root singletons")
		}
	}))

	for _, name := range []string{"ada", "linus"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-User", name)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if len(seen) != 2 || seen[0] != "ada" || seen[1] != "linus" {
		t.Fatalf("seen %v", seen)
	}
	if stops != 2 {
		t.Fatalf("request scopes stopped %d times", stops)
	}
	if err := app.Stop(context.Background()); err != nil || stops != 2 {
		t.Fatalf("stopped request scopes must be detached: stops=%d err=%v", stops, err)
	}
}
