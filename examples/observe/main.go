// Observe: the container's lifecycle as log lines. dislog.New turns a
// *slog.Logger into the observer Observe takes, so the application says what
// it is doing as it builds, starts and stops.
package main

import (
	"context"
	"log/slog"
	"os"

	charm "github.com/charmbracelet/log"
	"github.com/floatdrop/di"
	"github.com/floatdrop/di/dislog"
	"github.com/floatdrop/di/examples/observe/internal/store"
)

type Repo struct{ db *store.DB }

func NewRepo(db *store.DB) *Repo { return &Repo{db} }

func main() {
	// dislog imports only log/slog, so any handler will do. This one is
	// charmbracelet/log, which is an slog handler that colours its output.
	logger := slog.New(charm.New(os.Stderr))

	app := di.New()

	// Observers see this scope and every scope under it, so registering first
	// means the whole wiring is logged.
	app.Observe(dislog.New(logger))

	app.Value(store.Config{DSN: "postgres://localhost/app"})
	app.Use(store.Module)
	app.Wire[*Repo](NewRepo)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		logger.Error("start", "err", err)
		os.Exit(1)
	}

	// Built after Start, so this build is logged here, between the two
	// phases, and its OnStart would run as it is handed out. It is a type in
	// main, which has no import path to lift out, so it gets no pkg.
	_ = app.Get[*Repo]()

	// Stop returns what the hooks reported as well as logging it, so a
	// caller that wants to act on a teardown failure still can.
	if err := app.Stop(ctx); err != nil {
		logger.Warn("stopped with failures", "err", err)
	}
}
