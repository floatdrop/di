// Package api serves HTTP. The server is a singleton with a lifecycle; what a
// handler needs per request is Scoped, and is built in the request scope
// that the dihttp middleware opens for each request.
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

// Handler is built once per request, from singletons and request-scoped
// values alike.
type Handler struct {
	store  storage.Store
	mail   *mail.Mailer
	caller *Caller
}

func NewHandler(store storage.Store, m *mail.Mailer, caller *Caller) *Handler {
	return &Handler{store: store, mail: m, caller: caller}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, err := h.store.Find(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	h.mail.Send(h.caller.Name + " looked at " + user.Name)
	fmt.Fprintln(w, user.Name)
}

// NewServer builds the routes. The middleware gives each request its scope,
// which a handler reaches through di.FromContext; dihttp.Module provides it.
func NewServer(cfg config.Config, mw dihttp.Middleware) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		req, _ := di.FromContext(r.Context())
		req.Get[*Handler]().ServeHTTP(w, r)
	})
	return &http.Server{Addr: cfg.Addr, Handler: mw(mux)}
}

// Module registers the request-scoped values and the server, with the hooks
// that bind, drain and close it.
func Module(s *di.Scope) {
	s.Wire[*Caller](NewCaller).Scoped()
	s.Wire[*Handler](NewHandler).Scoped()
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
