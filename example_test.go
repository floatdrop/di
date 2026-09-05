package di_test

import (
	"context"
	"fmt"

	"github.com/floatdrop/di"
)

type Server struct{ repo *Repo }

func Example() {
	app := di.New()
	app.Value(Config{DSN: "pg://primary"})
	app.Provide(func(s *di.Scope) *DB { return &DB{dsn: s.Get[Config]().DSN} }).
		OnStop(func(ctx context.Context, db *DB) error { fmt.Println("close", db.dsn); return nil })
	app.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} })
	app.Provide(func(s *di.Scope) Reader { return s.Get[*Repo]() })
	app.Provide(func(s *di.Scope) *Server { return &Server{repo: s.Get[*Repo]()} }).Eager().
		OnStart(func(context.Context, *Server) error { fmt.Println("listening"); return nil }).
		OnStop(func(context.Context, *Server) error { fmt.Println("stopped"); return nil })

	if err := app.Start(context.Background()); err != nil {
		panic(err)
	}
	fmt.Println("read:", app.Get[Reader]().Read())
	_ = app.Stop(context.Background())
	// Output:
	// listening
	// read: pg://primary
	// stopped
	// close pg://primary
}
