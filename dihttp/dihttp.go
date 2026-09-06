// Package dihttp connects a di.Scope to net/http.
//
// A [Middleware] gives every request its own child scope holding the
// *http.Request, so services that depend on the request are declared once in
// the application scope as Scoped and built per request. Handlers reach the
// scope through [di.FromContext]. [Module] registers the middleware as a
// service, so a server's constructor takes it as a parameter; [NewMiddleware]
// makes one directly.
package dihttp

import (
	"context"
	"net/http"

	"github.com/floatdrop/di"
)

// Middleware gives every request its own child scope of the application
// scope: the *http.Request is registered in it, the scope is attached to the
// request context, and it is stopped (and detached) when the handler returns.
// Stop failures reach the application scope's observers as EventStop with Err
// set.
//
// It has the usual middleware shape, so it wraps a handler directly or goes
// into a router's Use. Take it as a dependency after Module has registered
// it, or make one with NewMiddleware.
type Middleware func(http.Handler) http.Handler

// Module registers a Middleware over the scope it is applied to, so that a
// constructor wired into that scope can take one as a parameter:
//
//	app.Use(dihttp.Module, api.Module)
//
//	func NewServer(cfg Config, mw dihttp.Middleware) *http.Server {
//		return &http.Server{Addr: cfg.Addr, Handler: mw(mux)}
//	}
//
// The middleware needs the scope itself, to open a child per request, which
// is why this is the one closure in the package rather than a wired
// constructor.
func Module(s *di.Scope) {
	s.Provide(func(s *di.Scope) Middleware { return NewMiddleware(s) })
}

// NewMiddleware makes a Middleware whose request scopes are children of s.
func NewMiddleware(s *di.Scope) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req := s.Child("request")
			// Attach the scope, then register that same request and hand it
			// on. The handler and the constructors must see one
			// *http.Request: routers write path values and the matched
			// pattern into the request they are given, so a copy registered
			// here would miss them.
			r = r.WithContext(di.WithScope(r.Context(), req))
			req.Value(r)
			defer func() { _ = req.Stop(context.WithoutCancel(r.Context())) }()
			next.ServeHTTP(w, r)
		})
	}
}
