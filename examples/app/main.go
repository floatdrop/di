// A complete service: request scopes through middleware, a background
// worker with a Worker hook, a health endpoint, and graceful shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/dihttp"
)

type DB struct{ dsn string }

func (db *DB) Ping(ctx context.Context) error { return nil }

type User struct{ Name string }

type Mailer struct{ queue chan string }

func main() {
	app := di.New()

	app.Provide(func(s *di.Scope) *DB {
		ctx, cancel := context.WithTimeout(s.Context(), 5*time.Second) // dial with the start deadline
		defer cancel()
		db := &DB{dsn: "postgres://localhost/app"}
		return s.Must(db, db.Ping(ctx))
	}).
		Health(func(ctx context.Context, db *DB) error { return db.Ping(ctx) }).
		OnStop(func(ctx context.Context, db *DB) error { log.Println("db closed"); return nil })

	// A worker: started with the app, cancelled by Stop, awaited before the
	// DB it depends on is closed. Returning an error stops the application.
	app.Provide(func(s *di.Scope) *Mailer { _ = s.Get[*DB](); return &Mailer{queue: make(chan string, 16)} }).
		Eager().
		Worker(func(ctx context.Context, m *Mailer) error {
			for {
				select {
				case msg := <-m.queue:
					log.Println("sent", msg)
				case <-ctx.Done():
					log.Println("mailer drained")
					return nil
				}
			}
		})

	// Request-scoped: declared once here, built once per request scope
	// created by Middleware, where the *http.Request exists.
	app.Provide(func(s *di.Scope) *User {
		return &User{Name: s.Get[*http.Request]().Header.Get("X-User")}
	}).Scoped()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", func(w http.ResponseWriter, r *http.Request) {
		req, _ := di.FromContext(r.Context())
		user := req.Get[*User]()
		req.Get[*Mailer]().queue <- "welcome " + user.Name
		fmt.Fprintln(w, "hello", user.Name)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := app.HealthCheck(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})

	app.Provide(func(s *di.Scope) *http.Server {
		return &http.Server{Addr: ":8080", Handler: dihttp.Middleware(app)(mux)}
	}).
		Eager().
		OnStart(func(ctx context.Context, srv *http.Server) error {
			ln, err := net.Listen("tcp", srv.Addr)
			if err != nil {
				return err
			}
			go func() {
				if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
					app.Shutdown(err)
				}
			}()
			return nil
		}).
		// Draining happens before anything is stopped, so a handler still
		// in flight keeps its request scope and everything it depends on.
		// In OnStop this would race the teardown of the very scopes those
		// handlers are using.
		OnDrain(func(ctx context.Context, srv *http.Server) error { return srv.Shutdown(ctx) }).
		OnStop(func(ctx context.Context, srv *http.Server) error { return srv.Close() })

	if err := app.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
