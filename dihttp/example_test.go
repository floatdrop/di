package dihttp_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/dihttp"
)

type Caller struct{ Name string }

func ExampleMiddleware() {
	app := di.New()
	// Declared once in the application scope, built once per request scope,
	// where the *http.Request exists.
	app.Provide(func(s *di.Scope) *Caller {
		return &Caller{Name: s.Get[*http.Request]().Header.Get("X-Caller")}
	}).Scoped()

	h := dihttp.Middleware(app)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := di.FromContext(r.Context())
		fmt.Fprintln(w, "hello", req.Get[*Caller]().Name)
	}))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Caller", "ada")
	h.ServeHTTP(rec, r)
	fmt.Print(rec.Body.String())
	// Output:
	// hello ada
}
