// Package api serves HTTP. The server is a singleton with a lifecycle. A
// handler type covers one resource, with a method per route; it is Scoped
// when it needs the request, and built in the request scope the dihttp
// middleware opens, or a plain singleton when it does not.
//
// Nothing here is exported but Module. Keys are types, so these handlers are
// services only this package can name, let alone resolve.
package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"golang.yandex/di"
	"golang.yandex/di/dihttp"
	"golang.yandex/di/examples/guide/internal/config"
	"golang.yandex/di/examples/guide/internal/mail"
	"golang.yandex/di/examples/guide/internal/storage"
)

// caller is who is making the request. It depends on the *http.Request, which
// only a request scope provides.
type caller struct{ name string }

func newCaller(r *http.Request) *caller { return &caller{name: r.Header.Get("X-User")} }

// users is built once per request, from singletons and request-scoped values
// alike, and serves every route about users.
type users struct {
	store  storage.Store
	mail   *mail.Mailer
	caller *caller
}

func newUsers(store storage.Store, m *mail.Mailer, c *caller) *users {
	return &users{store: store, mail: m, caller: c}
}

func (u *users) show(w http.ResponseWriter, r *http.Request) {
	user, err := u.store.Find(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	fmt.Fprintln(w, user.Name)
}

func (u *users) greet(w http.ResponseWriter, r *http.Request) {
	user, err := u.store.Find(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	u.mail.Send(u.caller.name + " greets " + user.Name)
	w.WriteHeader(http.StatusAccepted)
}

// health needs nothing from the request, so it is an ordinary singleton. It
// asks the store, which is the storage package's contract; the connection
// behind it is that package's own business.
type health struct{ store storage.Store }

func newHealth(store storage.Store) *health { return &health{store: store} }

func (h *health) check(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Ping(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

// newServer builds the routes. Each one resolves its handler from the
// request scope the middleware opens; dihttp.Module provides the middleware.
func newServer(cfg config.Config, mw dihttp.Middleware) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /users/{id}", dihttp.Handle((*users).show))
	mux.Handle("POST /users/{id}/greet", dihttp.Handle((*users).greet))
	mux.Handle("GET /healthz", dihttp.Handle((*health).check))
	return &http.Server{Addr: cfg.Addr, Handler: mw(mux)}
}

// Module registers the request-scoped values, the handlers and the server,
// with the hooks that bind, drain and close it.
func Module(s *di.Scope) {
	s.Wire[*caller](newCaller).Scoped()
	s.Wire[*users](newUsers).Scoped()
	s.Wire[*health](newHealth)
	s.Wire[*http.Server](newServer).
		Eager().
		OnStart(func(_ context.Context, srv *http.Server) error {
			// Bind synchronously, so a busy port fails Start; serve in the
			// background, and take the application down if serving stops.
			ln, err := net.Listen("tcp", srv.Addr)
			if err != nil {
				return err
			}
			go func() {
				if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
					s.Shutdown(err)
				}
			}()
			return nil
		}).
		// Draining runs before anything is stopped, so requests still in
		// flight keep their scopes and everything those depend on.
		OnDrain(func(ctx context.Context, srv *http.Server) error { return srv.Shutdown(ctx) }).
		OnStop(func(_ context.Context, srv *http.Server) error { return srv.Close() })
}
