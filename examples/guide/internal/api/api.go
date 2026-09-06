// Package api serves HTTP. The server is a singleton with a lifecycle. A
// handler type covers one resource, with a method per route; it is Scoped
// when it needs the request, and built in the request scope the dihttp
// middleware opens, or a plain singleton when it does not.
package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/dihttp"
	"github.com/floatdrop/di/examples/guide/internal/config"
	"github.com/floatdrop/di/examples/guide/internal/mail"
	"github.com/floatdrop/di/examples/guide/internal/storage"
)

// Caller is who is making the request. It depends on the *http.Request, which
// only a request scope provides.
type Caller struct{ Name string }

func NewCaller(r *http.Request) *Caller { return &Caller{Name: r.Header.Get("X-User")} }

// Users is built once per request, from singletons and request-scoped values
// alike, and serves every route about users.
type Users struct {
	store  storage.Store
	mail   *mail.Mailer
	caller *Caller
}

func NewUsers(store storage.Store, m *mail.Mailer, caller *Caller) *Users {
	return &Users{store: store, mail: m, caller: caller}
}

func (u *Users) Show(w http.ResponseWriter, r *http.Request) {
	user, err := u.store.Find(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	fmt.Fprintln(w, user.Name)
}

func (u *Users) Greet(w http.ResponseWriter, r *http.Request) {
	user, err := u.store.Find(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	u.mail.Send(u.caller.Name + " greets " + user.Name)
	w.WriteHeader(http.StatusAccepted)
}

// Health needs nothing from the request, so it is an ordinary singleton.
type Health struct{ db *storage.DB }

func NewHealth(db *storage.DB) *Health { return &Health{db: db} }

func (h *Health) Check(w http.ResponseWriter, r *http.Request) {
	if err := h.db.Ping(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

// NewServer builds the routes. Each one resolves its handler from the
// request scope the middleware opens; dihttp.Module provides the middleware.
func NewServer(cfg config.Config, mw dihttp.Middleware) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /users/{id}", dihttp.Handle((*Users).Show))
	mux.Handle("POST /users/{id}/greet", dihttp.Handle((*Users).Greet))
	mux.Handle("GET /healthz", dihttp.Handle((*Health).Check))
	return &http.Server{Addr: cfg.Addr, Handler: mw(mux)}
}

// Module registers the request-scoped values, the handlers and the server,
// with the hooks that bind, drain and close it.
func Module(s *di.Scope) {
	s.Wire[*Caller](NewCaller).Scoped()
	s.Wire[*Users](NewUsers).Scoped()
	s.Wire[*Health](NewHealth)
	s.Wire[*http.Server](NewServer).
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
