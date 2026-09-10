// Package dislog logs a di.Scope's lifecycle events through log/slog.
//
// [New] turns a *slog.Logger into the function [di.Scope.Observe] takes,
// so an application says what it is doing as it builds, starts, drains and
// stops:
//
//	app.Observe(dislog.New(slog.Default()))
//
// It imports nothing outside the standard library, so any slog handler will
// do, including one that colours its output. Events arrive on the goroutine
// that did the work, one per constructor and per hook, so a slow handler slows
// the application down; a *slog.Logger is safe to share, and events do arrive
// from several goroutines at once.
package dislog

import (
	"context"
	"log/slog"
	"path"
	"strings"

	"github.com/floatdrop/di"
)

// New returns a function for [di.Scope.Observe] that logs each event
// through l. The message is the event's kind -- "build", "start", "drain",
// "stop" or "shutdown" -- and the attributes are the service, its scope, the
// module it was registered from when there is one, and how long the step took.
//
// A service is named the way it is written in Go rather than the way an event
// carries it: "service=*mail.Mailer" with the import path alongside as
// "pkg=github.com/acme/app/internal/mail", since the path is most of the
// length and none of the meaning. The two come from [di.Event.Service] and
// [di.Event.Package], so nothing is parsed; a key whose type is unnamed
// reports no package and keeps its whole name.
//
// An event carrying an error is logged at [slog.LevelError] with an "err"
// attribute and the registration site, since that is what a failure is read
// with; anything else is logged at [slog.LevelInfo], or at the level [Level]
// sets. Pass [Site] to log the site every time.
func New(l *slog.Logger, opts ...Option) func(di.Event) {
	cfg := options{level: slog.LevelInfo}
	for _, o := range opts {
		o(&cfg)
	}
	return func(ev di.Event) {
		level := cfg.level
		if ev.Err != nil {
			level = slog.LevelError
		}
		if !l.Enabled(context.Background(), level) {
			return
		}
		attrs := make([]slog.Attr, 0, 7)
		if ev.Service != "" { // a shutdown names no service
			attrs = append(attrs, slog.String("service", short(ev.Service, ev.Package)))
			if ev.Package != "" {
				attrs = append(attrs, slog.String("pkg", ev.Package))
			}
		}
		attrs = append(attrs, slog.String("scope", ev.Scope))
		if ev.Module != "" {
			attrs = append(attrs, slog.String("module", ev.Module))
		}
		if ev.Kind != di.EventShutdown {
			attrs = append(attrs, slog.Duration("duration", ev.Duration))
		}
		if ev.Site != "" && (cfg.site || ev.Err != nil) {
			attrs = append(attrs, slog.String("site", ev.Site))
		}
		if ev.Err != nil {
			attrs = append(attrs, slog.Any("err", ev.Err))
		}
		l.LogAttrs(context.Background(), level, string(ev.Kind), attrs...)
	}
}

// short is the service name with its import path taken off:
// "*github.com/acme/app.DB" and "github.com/acme/app" become "*app.DB".
//
// The event carries both, so there is nothing to guess. An empty pkg is a
// type with no path to take off -- an unnamed type, which reflect already
// writes short, or a shutdown, which names no service -- and its name is
// returned as it came. A generic instantiation keeps its type arguments,
// because what is trimmed is the prefix rather than everything after a dot.
func short(service, pkg string) string {
	if pkg == "" {
		return service
	}
	stars := 0
	for stars < len(service) && service[stars] == '*' {
		stars++
	}
	name, ok := strings.CutPrefix(service[stars:], pkg+".")
	if !ok {
		return service // not the shape the pair promises; report it whole
	}
	return service[:stars] + path.Base(pkg) + "." + name
}

// Option configures the observer New returns.
type Option func(*options)

type options struct {
	level slog.Level
	site  bool
}

// Level sets the level an event that succeeded is logged at. Building every
// service is worth a line while an application is being wired and noise once
// it works, so [slog.LevelDebug] is the usual second choice. A failure is
// logged at [slog.LevelError] whatever this says.
func Level(lv slog.Level) Option { return func(o *options) { o.level = lv } }

// Site includes the registration site -- the file:line the service was
// registered at -- on every event, rather than only on the ones that failed.
func Site() Option { return func(o *options) { o.site = true } }
