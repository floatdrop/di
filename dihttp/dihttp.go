// Package dihttp connects a di.Scope to net/http.
//
// [Middleware] gives every request its own child scope holding the
// *http.Request, so services that depend on the request are declared once in
// the application scope as Scoped and built per request. Handlers reach the
// scope through [di.FromContext].
package dihttp

import (
	"context"
	"net/http"

	"github.com/floatdrop/di"
)

// Middleware returns a middleware that gives every request its own child
// scope of s: the *http.Request is registered in it, the scope is attached to
// the request context, and it is stopped (and detached from s) when the
// handler returns. Stop failures reach s's observers as EventStop with Err set.
//
// The returned function has the usual middleware shape, so it wraps a handler
// directly or goes into a router's Use.
func Middleware(s *di.Scope) func(http.Handler) http.Handler {
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
