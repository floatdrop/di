// Inspecting the graph: what a service was built from, and what needed it.
//
// Dependencies are recorded as constructors resolve them, so Explain and
// Graph describe what actually happened rather than what was registered.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/floatdrop/di"
)

type Config struct{ DSN string }
type DB struct{ dsn string }
type Repo struct{ db *DB }
type Cache struct{ db *DB }
type Server struct {
	repo  *Repo
	cache *Cache
}

func main() {
	app := di.New()

	app.Value(Config{DSN: "postgres://localhost/app"})
	app.Provide(func(s *di.Scope) *DB { return &DB{dsn: s.Get[Config]().DSN} })
	app.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} })
	app.Provide(func(s *di.Scope) *Cache { return &Cache{db: s.Get[*DB]()} })
	app.Provide(func(s *di.Scope) *Server {
		return &Server{repo: s.Get[*Repo](), cache: s.Get[*Cache]()}
	}).Eager()

	if err := app.Start(context.Background()); err != nil {
		log.Fatal(err)
	}
	defer func() { _ = app.Stop(context.Background()) }()

	// What the server was built from. *DB is reached through both the repo
	// and the cache, and is expanded once.
	fmt.Print(app.Explain[*Server]())

	// And the other direction: what needed the database.
	fmt.Println()
	fmt.Print(app.Explain[*DB]())

	// Everything built so far, as Graphviz DOT: dot -Tsvg > graph.svg
	fmt.Println()
	fmt.Print(app.Graph())
}
