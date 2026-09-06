// Command api is the application: it composes the modules, checks the graph,
// and runs until a signal arrives.
package main

import (
	"context"
	"log"
	"time"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/dihttp"
	"github.com/floatdrop/di/examples/guide/internal/api"
	"github.com/floatdrop/di/examples/guide/internal/cache"
	"github.com/floatdrop/di/examples/guide/internal/config"
	"github.com/floatdrop/di/examples/guide/internal/mail"
	"github.com/floatdrop/di/examples/guide/internal/storage"
)

func main() {
	app := di.New()
	app.Use(config.Module, storage.Module, cache.Module, mail.Module, api.Module)

	// Nothing has been built yet. The constructors declared their
	// dependencies, so the graph is checked here, request scopes included.
	if err := dihttp.Validate(app); err != nil {
		log.Fatal(err)
	}

	// Run starts the eager services and their hooks, waits for SIGINT or
	// SIGTERM, then stops everything in reverse order within the timeout.
	if err := app.Run(context.Background(), di.StopTimeout(10*time.Second)); err != nil {
		log.Fatal(err)
	}
}
