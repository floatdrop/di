// Values that change: a live value is held by a service that does not
// change. A long-lived service reads the source when it needs the value; a
// per-request service takes a snapshot, Scoped, so one request sees one
// configuration and the next request sees the current one.
package main

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"

	"golang.yandex/di"
)

type Config struct{ RateLimit int }

// Source holds the current configuration. Current is what a long-lived
// service calls at the moment it needs the value. A real source follows a
// file, a signal or a remote endpoint and calls Reload: that watcher is a Go
// hook, and wants Eager() so it starts with the application rather than on
// the first resolution.
type Source struct{ cur atomic.Pointer[Config] }

func NewSource() *Source {
	s := &Source{}
	s.cur.Store(&Config{RateLimit: 100}) // the initial read
	return s
}

func (s *Source) Current() Config { return *s.cur.Load() }

func (s *Source) Reload(cfg Config) {
	s.cur.Store(&cfg)
	fmt.Println("reloaded: ", cfg.RateLimit, "per minute")
}

// A singleton lives as long as the application, so it takes the source and
// reads it on every use. Taking Config instead would capture one snapshot
// when the limiter is built, whatever lifetime Config had.
type Limiter struct{ src *Source }

func NewLimiter(src *Source) *Limiter { return &Limiter{src: src} }
func (l *Limiter) Limit() int         { return l.src.Current().RateLimit }

// A per-request service takes Config as a plain parameter. Its snapshot is
// built with the request scope and lives as long as it does.
type Handler struct{ cfg Config }

func NewHandler(cfg Config) *Handler { return &Handler{cfg: cfg} }

func main() {
	app := di.New()
	app.Wire[*Source](NewSource)
	app.Wire[Config](func(src *Source) Config { return src.Current() }).Scoped()
	app.Wire[*Limiter](NewLimiter)
	app.Wire[*Handler](NewHandler).Scoped()

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer func() { _ = app.Stop(ctx) }()

	limiter := app.Get[*Limiter]()
	fmt.Println("limiter:  ", limiter.Limit(), "per minute")

	a := app.Child("request a")
	fmt.Println("request a:", a.Get[*Handler]().cfg.RateLimit, "per minute")

	app.Get[*Source]().Reload(Config{RateLimit: 200})

	// The limiter reads the new value; request a keeps the snapshot it was
	// built with; a new request is built with the current one.
	fmt.Println("limiter:  ", limiter.Limit(), "per minute")
	fmt.Println("request a:", a.Get[*Handler]().cfg.RateLimit, "per minute")
	b := app.Child("request b")
	fmt.Println("request b:", b.Get[*Handler]().cfg.RateLimit, "per minute")

	_ = a.Stop(ctx)
	_ = b.Stop(ctx)
}
