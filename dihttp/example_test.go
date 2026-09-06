package dihttp_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/dihttp"
)

type Caller struct{ Name string }

func ExampleNewMiddleware() {
	app := di.New()
	// Declared once in the application scope, built once per request scope,
	// where the *http.Request exists.
	app.Provide(func(s *di.Scope) *Caller {
		return &Caller{Name: s.Get[*http.Request]().Header.Get("X-Caller")}
	}).Scoped()

	h := dihttp.NewMiddleware(app)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

type Server struct{ http.Handler }

// NewServer is a plain constructor: the middleware arrives as a dependency.
func NewServer(mw dihttp.Middleware) *Server {
	return &Server{mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := di.FromContext(r.Context())
		fmt.Fprintln(w, "hello", req.Get[*Caller]().Name)
	}))}
}

func ExampleModule() {
	app := di.New()
	app.Use(dihttp.Module)
	app.Wire[*Caller](func(r *http.Request) *Caller { return &Caller{Name: r.Header.Get("X-Caller")} }).Scoped()
	app.Wire[*Server](NewServer)

	// The graph is checked as a request scope would resolve it: the
	// middleware is provided by the module, the request by each request.
	fmt.Println(app.Validate(di.Provided[*http.Request]()).Err())

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Caller", "ada")
	app.Get[*Server]().ServeHTTP(rec, r)
	fmt.Print(rec.Body.String())
	// Output:
	// <nil>
	// hello ada
}
