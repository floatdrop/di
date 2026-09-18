// More than one instance of a type: while the set is fixed, a defined type
// names each one.
package main

import (
	"context"
	"fmt"

	"github.com/floatdrop/di"
)

type DB struct{ dsn string }

func (d *DB) Query() string { return "query " + d.dsn }

// One defined type per instance. Embedding promotes the methods, so only the
// constructor below mentions the surrogate.
type Primary struct{ *DB }
type Replica struct{ *DB }

type Repo struct{ read, write *DB }

func NewRepo(p Primary, r Replica) *Repo { return &Repo{write: p.DB, read: r.DB} }

func main() {
	app := di.New()

	// Two databases, told apart by type. Each keeps its own hooks.
	app.Value(Primary{&DB{dsn: "primary"}}).
		OnStop(func(context.Context, Primary) error { fmt.Println("primary closed"); return nil })
	app.Value(Replica{&DB{dsn: "replica"}}).
		OnStop(func(context.Context, Replica) error { fmt.Println("replica closed"); return nil })
	app.Wire[*Repo](NewRepo)

	repo := app.Get[*Repo]()
	fmt.Println("writes:", repo.write.Query())
	fmt.Println("reads: ", repo.read.Query())

	_ = app.Stop(context.Background())
}
