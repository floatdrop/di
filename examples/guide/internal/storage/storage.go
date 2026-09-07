// Package storage owns the database connection and the store built on it.
// It exports its contract, Store and User, and its Module; the connection
// and the implementation are private. Keys are types, so a type only this
// package can name is a service only this package can resolve.
package storage

import (
	"context"
	"errors"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/examples/guide/internal/config"
)

// db is the database connection: private to the package, with a lifecycle
// the container runs. Its constructor takes what it needs as parameters and
// knows nothing about the container.
type db struct{ dsn string }

func newDB(cfg config.Config) (*db, error) {
	if cfg.DSN == "" {
		return nil, errors.New("storage: DSN is empty")
	}
	return &db{dsn: cfg.DSN}, nil
}

func (db *db) Ping(context.Context) error { return nil }
func (db *db) Close() error               { return nil }

// Store is what the rest of the application depends on. Handlers take the
// interface; the container serves whatever is registered for it.
type Store interface {
	Find(ctx context.Context, id string) (User, error)
	Ping(ctx context.Context) error
}

type User struct{ ID, Name string }

type pgStore struct{ db *db }

func newPGStore(db *db) *pgStore { return &pgStore{db: db} }

func (s *pgStore) Find(_ context.Context, id string) (User, error) {
	return User{ID: id, Name: "user " + id + " via " + s.db.dsn}, nil
}

func (s *pgStore) Ping(ctx context.Context) error { return s.db.Ping(ctx) }

// Module registers the package's services. Constructors are handed over as
// they are; the hooks are typed on what they receive. The Store key is
// served by the private constructor, whose result is assignable to it.
func Module(s *di.Scope) {
	s.Wire[*db](newDB).
		OnStart(func(ctx context.Context, db *db) error { return db.Ping(ctx) }).
		OnStop(func(_ context.Context, db *db) error { return db.Close() })
	s.Wire[Store](newPGStore)
}
