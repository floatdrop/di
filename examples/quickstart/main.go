// Quick start: register a few services, start and stop the application.
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
type Server struct{ repo *Repo }

func main() {
	app := di.New()

	app.Value(Config{DSN: "postgres://localhost/app"})

	app.Provide(func(s *di.Scope) *DB { return &DB{dsn: s.Get[Config]().DSN} }).
		OnStop(func(ctx context.Context, db *DB) error { fmt.Println("db closed"); return nil })

	app.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} })

	app.Provide(func(s *di.Scope) *Server { return &Server{repo: s.Get[*Repo]()} }).
		Eager().
		OnStart(func(ctx context.Context, srv *Server) error { fmt.Println("listening"); return nil }).
		OnStop(func(ctx context.Context, srv *Server) error { fmt.Println("server stopped"); return nil })

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer app.Stop(ctx)

	fmt.Println("serving", app.Get[*Server]().repo.db.dsn)
}
