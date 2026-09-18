// Package guide is the application the step-by-step guide walks through.
package guide

import (
	"github.com/floatdrop/di"
	"github.com/floatdrop/di/dihttp"
	"github.com/floatdrop/di/examples/guide/internal/api"
	"github.com/floatdrop/di/examples/guide/internal/cache"
	"github.com/floatdrop/di/examples/guide/internal/config"
	"github.com/floatdrop/di/examples/guide/internal/mail"
	"github.com/floatdrop/di/examples/guide/internal/storage"
)

// Modules is the application, in the order it is composed: cmd/api builds it
// and the tests beside this file pin what it declares, so the graph the guide
// shows is the graph that runs. Order matters in one place -- cache wraps what
// serves storage.Store, so its module comes after storage's.
var Modules = []di.Module{
	config.Module,
	storage.Module,
	cache.Module,
	mail.Module,
	dihttp.Module,
	api.Module,
}
