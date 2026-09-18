// More than one instance of a type: while the set is fixed, a tag names each
// one, and constructors take plain parameters.
package main

import (
	"context"
	"fmt"

	"github.com/floatdrop/di"
)

type DB struct{ dsn string }

func (d *DB) Query() string { return "query " + d.dsn }

// One tag per instance. A tag is a type, so it is checked where it is used
// and appears in no signature.
type Primary struct{}
type Replica struct{}

// Most consumers want one of the instances, and say which with Needs.
type Report struct{ db *DB }

func NewReport(db *DB) *Report { return &Report{db} }

// A consumer of both cannot be told apart by type, so one side takes a
// defined type, filled from its tag by a one-line constructor.
type ReadDB struct{ *DB }

type Repo struct{ write, read *DB }

func NewRepo(w *DB, r ReadDB) *Repo { return &Repo{write: w, read: r.DB} }

func main() {
	app := di.New()

	// Two databases, registered through a tag view. Each keeps its own hooks.
	app.Tag[Primary]().Value(&DB{dsn: "primary"}).
		OnStop(func(context.Context, *DB) error { fmt.Println("primary closed"); return nil })
	app.Tag[Replica]().Value(&DB{dsn: "replica"}).
		OnStop(func(context.Context, *DB) error { fmt.Println("replica closed"); return nil })

	app.Wire[*Report](NewReport).Needs(di.Tagged[*DB, Replica]())
	app.Wire[ReadDB](func(db *DB) ReadDB { return ReadDB{db} }).Needs(di.Tagged[*DB, Replica]())
	app.Wire[*Repo](NewRepo).Needs(di.Tagged[*DB, Primary]())

	repo := app.Get[*Repo]()
	fmt.Println("writes:", repo.write.Query())
	fmt.Println("reads: ", repo.read.Query())
	fmt.Println("report:", app.Get[*Report]().db.Query())

	// The same view reads an instance back.
	fmt.Println("primary:", app.Tag[Primary]().Get[*DB]().Query())

	_ = app.Stop(context.Background())
}
