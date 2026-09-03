package di_test

import (
	"context"
	"errors"
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

func TestScopedIsOnePerScope(t *testing.T) {
	stops := 0
	root := di.New()
	root.Provide(func(*di.Scope) *DB { return &DB{} })
	root.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} }).Scoped().
		OnStop(func(context.Context, *Repo) error { stops++; return nil })

	a, b := root.Child("a"), root.Child("b")
	first, second := a.Get[*Repo](), a.Get[*Repo]()
	if first != second {
		t.Fatal("scoped instance must be cached within its scope")
	}
	if a.Get[*Repo]() == b.Get[*Repo]() {
		t.Fatal("scoped instances must differ between scopes")
	}
	if a.Get[*Repo]().db != b.Get[*Repo]().db {
		t.Fatal("scoped instances still share root singletons")
	}
	if err := a.Stop(context.Background()); err != nil || stops != 1 {
		t.Fatalf("scoped instance must stop with its scope: stops=%d err=%v", stops, err)
	}
	if err := root.Stop(context.Background()); err != nil || stops != 2 {
		t.Fatalf("root Stop must stop b's instance only: stops=%d err=%v", stops, err)
	}
}

func TestScopedBuildsInResolvingScope(t *testing.T) {
	root := di.New()
	root.Provide(func(s *di.Scope) *User { return &User{Name: s.Get[string]()} }).Scoped()

	req := root.Child("request")
	req.Value("ada")
	if req.Get[*User]().Name != "ada" {
		t.Fatal("scoped service must see the resolving scope's values")
	}
	// From the root the request value does not exist: a clear error, not a
	// captive instance built with the wrong data.
	if _, err := root.Resolve[*User](); !errors.Is(err, di.ErrNotProvided) {
		t.Fatalf("got %v", err)
	}
}

func TestMiddlewareGivesEachRequestAScope(t *testing.T) {
	stops := 0
	app := di.New()
	app.Provide(func(*di.Scope) *DB { return &DB{dsn: "shared"} })
	// Declared once in the root, built once per request scope, where the
	// *http.Request exists.
	app.Provide(func(s *di.Scope) *User {
		return &User{Name: s.Get[*http.Request]().Header.Get("X-User")}
	}).Scoped().OnStop(func(context.Context, *User) error { stops++; return nil })

	var seen []string
	h := app.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, ok := di.FromContext(r.Context())
		if !ok {
			t.Fatal("no scope in request context")
		}
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
