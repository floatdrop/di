// Command api is the application: it composes the modules, checks the graph,
// and runs until a signal arrives.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	charm "github.com/charmbracelet/log"
	"github.com/floatdrop/di"
	"github.com/floatdrop/di/dislog"
	"github.com/floatdrop/di/examples/guide"
)

func main() {
	// charmbracelet/log is an slog handler, so one logger serves both the
	// application and the container.
	logger := slog.New(charm.NewWithOptions(os.Stderr, charm.Options{
		ReportTimestamp: true,
		TimeFormat:      time.Kitchen,
	}))

	app := di.New()

	// dislog.New turns the container's lifecycle events into log lines: one
	// per constructor and per hook, so the application says what it is doing
	// as it builds, starts, drains and stops. Observers see this scope and
	// every scope under it, request scopes included, so register it first and
	// the wiring is logged from the beginning.
	app.Observe(dislog.New(logger))

	// The same logger as a service, so a constructor that wants to log takes
	// a *slog.Logger as a parameter like any other dependency.
	app.Value(logger)

	// guide.Modules is the list, kept beside the tests that pin the graph.
	app.Use(guide.Modules...)

	// Nothing has been built yet. The constructors declared their
	// dependencies, so the graph is checked here, as a request scope holding
	// an *http.Request would resolve it.
	if err := app.Validate(di.Provided[*http.Request]()).Err(); err != nil {
		logger.Error("invalid wiring", "err", err)
		os.Exit(1)
	}

	// Run starts the eager services and their hooks, waits for SIGINT or
	// SIGTERM, then stops everything in reverse order within the timeout.
	if err := app.Run(context.Background(), di.StopTimeout(10*time.Second)); err != nil {
		logger.Error("stopped with failures", "err", err)
		os.Exit(1)
	}
}
