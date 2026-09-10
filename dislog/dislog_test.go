package dislog_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/dislog"
)

// logger writes text lines into buf with the parts that move -- the
// timestamp, the measured duration and the absolute path of a site --
// replaced, so a line can be compared as a string.
func logger(buf *bytes.Buffer, level slog.Level) *slog.Logger {
	h := slog.NewTextHandler(buf, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey:
				return slog.Attr{}
			case "duration":
				return slog.String("duration", "1ms")
			case "site":
				return slog.String("site", filepath.Base(a.Value.String()))
			}
			return a
		},
	})
	return slog.New(h)
}

func lines(buf *bytes.Buffer) []string {
	return strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
}

type DB struct{}

func TestNewEventShapes(t *testing.T) {
	site := "/home/dev/app/wire.go:12"
	for _, tc := range []struct {
		name string
		ev   di.Event
		opts []dislog.Option
		want string
	}{
		{
			name: "the import path is a separate attribute",
			ev:   di.Event{Kind: di.EventBuild, Service: "*github.com/acme/app/internal/mail.Mailer", Scope: "root", Duration: time.Millisecond},
			want: `level=INFO msg=build service=*mail.Mailer pkg=github.com/acme/app/internal/mail scope=root duration=1ms`,
		},
		{
			name: "a build names the service, its scope and how long it took",
			ev:   di.Event{Kind: di.EventBuild, Service: "*app.DB", Scope: "root", Site: site, Duration: time.Millisecond},
			want: `level=INFO msg=build service=*app.DB scope=root duration=1ms`,
		},
		{
			name: "a module is named when the registration carried one",
			ev:   di.Event{Kind: di.EventStart, Service: "*app.DB", Scope: "root", Module: "app.Storage", Site: site, Duration: time.Millisecond},
			want: `level=INFO msg=start service=*app.DB scope=root module=app.Storage duration=1ms`,
		},
		{
			name: "a failure is an error, with the site to read it at",
			ev:   di.Event{Kind: di.EventStop, Service: "*app.DB", Scope: "root", Site: site, Duration: time.Millisecond, Err: errors.New("boom")},
			want: `level=ERROR msg=stop service=*app.DB scope=root duration=1ms site=wire.go:12 err=boom`,
		},
		{
			name: "a shutdown names no service and took no time",
			ev:   di.Event{Kind: di.EventShutdown, Scope: "root"},
			want: `level=INFO msg=shutdown scope=root`,
		},
		{
			name: "a shutdown with a cause is an error",
			ev:   di.Event{Kind: di.EventShutdown, Scope: "root", Err: errors.New("serve: closed")},
			want: `level=ERROR msg=shutdown scope=root err="serve: closed"`,
		},
		{
			name: "Site logs it on a step that succeeded too",
			ev:   di.Event{Kind: di.EventBuild, Service: "*app.DB", Scope: "root", Site: site, Duration: time.Millisecond},
			opts: []dislog.Option{dislog.Site()},
			want: `level=INFO msg=build service=*app.DB scope=root duration=1ms site=wire.go:12`,
		},
		{
			name: "Level moves a step that succeeded",
			ev:   di.Event{Kind: di.EventBuild, Service: "*app.DB", Scope: "root", Site: site, Duration: time.Millisecond},
			opts: []dislog.Option{dislog.Level(slog.LevelDebug)},
			want: `level=DEBUG msg=build service=*app.DB scope=root duration=1ms`,
		},
		{
			name: "Level does not move a failure",
			ev:   di.Event{Kind: di.EventBuild, Service: "*app.DB", Scope: "root", Site: site, Duration: time.Millisecond, Err: errors.New("boom")},
			opts: []dislog.Option{dislog.Level(slog.LevelDebug)},
			want: `level=ERROR msg=build service=*app.DB scope=root duration=1ms site=wire.go:12 err=boom`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			dislog.New(logger(&buf, slog.LevelDebug), tc.opts...)(tc.ev)
			if got := strings.TrimSuffix(buf.String(), "\n"); got != tc.want {
				t.Errorf("\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// A level the handler discards costs nothing beyond the check: the event is
// not formatted, and a failure still gets through.
func TestNewRespectsTheHandlerLevel(t *testing.T) {
	var buf bytes.Buffer
	obs := dislog.New(logger(&buf, slog.LevelInfo), dislog.Level(slog.LevelDebug))
	obs(di.Event{Kind: di.EventBuild, Service: "*app.DB", Scope: "root"})
	if buf.Len() != 0 {
		t.Fatalf("want nothing logged, got %s", buf.String())
	}
	obs(di.Event{Kind: di.EventBuild, Service: "*app.DB", Scope: "root", Err: errors.New("boom")})
	if !strings.Contains(buf.String(), "level=ERROR") {
		t.Fatalf("want the failure logged, got %q", buf.String())
	}
}

// The observer is registered on a real scope, so what it logs is what the
// container emits, in the order the container emits it.
func TestNewOnARunningScope(t *testing.T) {
	var buf bytes.Buffer
	app := di.New()
	app.Observe(dislog.New(logger(&buf, slog.LevelDebug)))
	app.Wire[*DB](func() *DB { return &DB{} }).
		Eager().
		OnStart(func(context.Context, *DB) error { return nil }).
		OnStop(func(context.Context, *DB) error { return nil })

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	// The import path is lifted out of the name, so the service reads the way
	// it is written in Go.
	const svc = "service=*dislog_test.DB pkg=github.com/floatdrop/di/dislog_test"
	want := []string{
		`level=INFO msg=build ` + svc + ` scope=root duration=1ms`,
		`level=INFO msg=start ` + svc + ` scope=root duration=1ms`,
		`level=INFO msg=stop ` + svc + ` scope=root duration=1ms`,
	}
	if got := lines(&buf); !slices.Equal(got, want) {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
}

// A child scope's events reach an observer registered on the parent, and name
// the scope they happened in.
func TestNewSeesChildScopes(t *testing.T) {
	var buf bytes.Buffer
	app := di.New()
	app.Observe(dislog.New(logger(&buf, slog.LevelDebug)))
	app.Wire[*DB](func() *DB { return &DB{} }).Scoped()

	req := app.Child("request")
	req.Get[*DB]()
	if err := req.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"msg=build", "scope=request"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("want %s in %q", want, buf.String())
		}
	}
}

// split is what keeps a service name short. Its job is to take off an import
// path and to leave alone everything that only looks like one.
func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		service, name, pkg string
	}{
		{"github.com/acme/app.DB", "app.DB", "github.com/acme/app"},
		{"*github.com/acme/app.DB", "*app.DB", "github.com/acme/app"},
		{"**github.com/acme/app.DB", "**app.DB", "github.com/acme/app"},
		{"*net/http.Server", "*http.Server", "net/http"},
		{"github.com/acme/app.Cache[github.com/acme/app.Key]", "app.Cache[github.com/acme/app.Key]", "github.com/acme/app"},
		// Nothing to take off: a main type, an unnamed type reflect already
		// wrote short, a builtin. Each keeps its whole name and gets no pkg.
		{"main.Config", "main.Config", ""},
		{"*main.Config", "*main.Config", ""},
		{"[]app.DB", "[]app.DB", ""},
		{"map[string]app.DB", "map[string]app.DB", ""},
		{"int", "int", ""},
		{"[]byte", "[]byte", ""},
		{"", "", ""},
	} {
		name, pkg := dislog.Split(tc.service)
		if name != tc.name || pkg != tc.pkg {
			t.Errorf("split(%q) = %q, %q; want %q, %q", tc.service, name, pkg, tc.name, tc.pkg)
		}
	}
}
