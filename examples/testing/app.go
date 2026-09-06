// Package app shows how to substitute dependencies in tests: wire the
// production graph into a fresh scope, then override before resolving.
package app

import "github.com/floatdrop/di"

type DB struct{ DSN string }
type Repo struct{ DB *DB }

// Production registers the production graph into s.
func Production(s *di.Scope) {
	s.Wire[*DB](func() *DB { return &DB{DSN: "postgres://localhost/app"} })
	s.Wire[*Repo](func(db *DB) *Repo { return &Repo{DB: db} })
}
