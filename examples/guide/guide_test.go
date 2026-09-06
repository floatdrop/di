// Package guide is the application the step-by-step guide walks through.
// This test pins two things the guide shows: that the wiring validates, and
// what Explain says about it before anything is built.
package guide

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/dihttp"
	"github.com/floatdrop/di/examples/guide/internal/api"
	"github.com/floatdrop/di/examples/guide/internal/cache"
	"github.com/floatdrop/di/examples/guide/internal/config"
	"github.com/floatdrop/di/examples/guide/internal/mail"
	"github.com/floatdrop/di/examples/guide/internal/storage"
)

var update = flag.Bool("update", false, "rewrite testdata/explain.txt from the current wiring")

func wire(app *di.Scope) {
	app.Use(config.Module, storage.Module, cache.Module, mail.Module, api.Module)
}

func TestWiringValidates(t *testing.T) {
	app := di.New()
	wire(app)
	if err := dihttp.Validate(app); err != nil {
		t.Fatal(err)
	}
}

// The tree the site shows is this file's output, so the two cannot drift.
func TestExplainMatchesTheGuide(t *testing.T) {
	app := di.New()
	wire(app)
	got := relative(app.Explain[storage.Store]())
	path := filepath.Join("testdata", "explain.txt")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("Explain output changed; run go test ./examples/guide -update\n%s", got)
	}
}

// Start builds the eager services and runs their hooks against a random
// port; Stop cancels the worker, drains the server and closes the database.
func TestStartAndStop(t *testing.T) {
	t.Setenv("ADDR", "127.0.0.1:0")
	app := di.New()
	wire(app)
	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

var modulePath = regexp.MustCompile(`github\.com/floatdrop/di/examples/guide/internal/`)

// relative strips the machine-specific directory and the module path from
// registration sites, leaving internal/storage/storage.go:41.
func relative(s string) string {
	_, file, _, _ := runtime.Caller(0)
	s = strings.ReplaceAll(s, filepath.Dir(file)+"/", "")
	return modulePath.ReplaceAllString(s, "")
}
