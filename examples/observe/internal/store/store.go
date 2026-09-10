// Package store is a service in a package of its own, so the log lines show
// what dislog does with an import path and with a module.
package store

import (
	"context"
	"errors"

	"github.com/floatdrop/di"
)

type Config struct{ DSN string }

type DB struct{ dsn string }

func New(cfg Config) *DB { return &DB{cfg.DSN} }

// Module registers the DB with the hooks that open and close it.
func Module(s *di.Scope) {
	s.Wire[*DB](New).
		Eager().
		OnStart(func(context.Context, *DB) error { return nil }).
		// A hook that fails is logged at error level, with the registration
		// site, since that is what a failure is read with.
		OnStop(func(context.Context, *DB) error { return errors.New("connection reset") })
}
