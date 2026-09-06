// Package storage owns the database connection and the store built on it.
package storage

import (
	"context"
	"errors"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/examples/guide/internal/config"
)

// DB is a database connection. Its constructor takes what it needs as
// parameters and knows nothing about the container.
type DB struct{ dsn string }

func NewDB(cfg config.Config) (*DB, error) {
	if cfg.DSN == "" {
		return nil, errors.New("storage: DSN is empty")
	}
	return &DB{dsn: cfg.DSN}, nil
}

func (db *DB) Ping(context.Context) error { return nil }
func (db *DB) Close() error               { return nil }

// Store is what the rest of the application depends on. Handlers take the
// interface; the container serves whatever is registered for it.
type Store interface {
	Find(ctx context.Context, id string) (User, error)
}

type User struct{ ID, Name string }

type PGStore struct{ db *DB }

func NewPGStore(db *DB) *PGStore { return &PGStore{db: db} }

func (s *PGStore) Find(_ context.Context, id string) (User, error) {
	return User{ID: id, Name: "user " + id + " via " + s.db.dsn}, nil
}

// Module registers the package's services. Constructors are handed over as
// they are; the hooks are typed on what they receive. The Store key is
// served by the concrete constructor, whose result is assignable to it.
func Module(s *di.Scope) {
	s.Wire[*DB](NewDB).
		OnStart(func(ctx context.Context, db *DB) error { return db.Ping(ctx) }).
		OnStop(func(_ context.Context, db *DB) error { return db.Close() })
	s.Wire[Store](NewPGStore)
}
