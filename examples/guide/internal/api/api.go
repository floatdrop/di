// Package api serves HTTP. The server is a singleton with a lifecycle; what a
// handler needs per request is Scoped, and is built in the request scope
// that dihttp.Middleware opens for each request.
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

// NewServer builds the routes. requests is the middleware that gives each
// request its scope; a handler reaches it through di.FromContext.
func NewServer(cfg config.Config, requests func(http.Handler) http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		req, _ := di.FromContext(r.Context())
		req.Get[*Handler]().ServeHTTP(w, r)
	})
	return &http.Server{Addr: cfg.Addr, Handler: requests(mux)}
}

// Module registers the request-scoped values and the server. The server's
// constructor needs the scope itself, to open a request scope per request,
// so it is a closure rather than a wired constructor.
func Module(s *di.Scope) {
	s.Wire[*Caller](NewCaller).Scoped()
	s.Wire[*Handler](NewHandler).Scoped()
	s.Provide(func(s *di.Scope) *http.Server {
		return NewServer(s.Get[config.Config](), dihttp.Middleware(s))
	}).
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
