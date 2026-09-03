// Package app shows how to substitute dependencies in tests: wire the
// production graph into a fresh scope, then override before resolving.
package app

import "github.com/floatdrop/di"

type DB struct{ DSN string }
type Repo struct{ DB *DB }

// Wire registers the production graph into s.
func Wire(s *di.Scope) {
	s.Provide(func(*di.Scope) *DB { return &DB{DSN: "postgres://localhost/app"} })
	s.Provide(func(s *di.Scope) *Repo { return &Repo{DB: s.Get[*DB]()} })
}
