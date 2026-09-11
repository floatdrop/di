// Factories: a value made fresh on every use is a function, and one that
// also needs a lifecycle is a scope.
package main

import (
	"context"
	"fmt"

	"github.com/floatdrop/di"
)

type Config struct{ Prefix string }

// The function's type is the key, so it is named: two factories with the
// same signature would otherwise collide, and the name is what Explain shows
// and what a wired constructor asks for.
type NewID func(n int) string

type Job struct{ id string }

func NewIDs(cfg Config) NewID {
	return func(n int) string { return fmt.Sprintf("%s-%d", cfg.Prefix, n) }
}
func NewJob(newID NewID) *Job                 { return &Job{id: newID(3)} }
func release(_ context.Context, j *Job) error { fmt.Println("released", j.id); return nil }

func main() {
	app := di.New()
	app.Value(Config{Prefix: "job"})
	app.Wire[NewID](NewIDs)
	// A value with a lifecycle of its own is Scoped, resolved from a child
	// scope opened for one unit of work and stopped with it.
	app.Wire[*Job](NewJob).Scoped().OnStop(release)

	// The container built the factory once and never sees what it makes;
	// the values are the caller's.
	newID := app.Get[NewID]()
	fmt.Println(newID(1), newID(2))

	work := app.Child("work")
	fmt.Println("running", work.Get[*Job]().id)
	_ = work.Stop(context.Background())
}
