package di_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/floatdrop/di"
)

type Session struct{ ID string }

func ExampleBinding_Scoped() {
	app := di.New()
	// Declared once in the root, built once per scope that resolves it.
	app.Provide(func(s *di.Scope) *Session { return &Session{ID: s.Get[string]()} }).Scoped()

	for _, id := range []string{"a1", "b2"} {
		req := app.Child("request")
		req.Value(id)
		first, again := req.Get[*Session](), req.Get[*Session]()
		fmt.Println(first.ID, first == again)
		_ = req.Stop(context.Background())
	}
	// Output:
	// a1 true
	// b2 true
}

type Queue struct{ jobs chan string }

func ExampleBinding_Worker() {
	app := di.New()
	done := make(chan string, 1)
	app.Provide(func(*di.Scope) *Queue { return &Queue{jobs: make(chan string, 1)} }).Eager().
		Worker(func(ctx context.Context, q *Queue) error {
			for {
				select {
				case job := <-q.jobs:
					done <- "processed " + job
				case <-ctx.Done():
					return nil // cancelled by Stop
				}
			}
		})

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		panic(err)
	}
	app.Get[*Queue]().jobs <- "email"
	fmt.Println(<-done)
	fmt.Println("stop:", app.Stop(ctx)) // waits for the worker to return
	// Output:
	// processed email
	// stop: <nil>
}

type Cache struct{}

func ExampleScope_Observe() {
	app := di.New()
	app.Observe(func(ev di.Event) {
		fmt.Println(ev.Kind, ev.Service, ev.Err)
	})
	app.Provide(func(*di.Scope) *Cache { return &Cache{} }).
		OnStop(func(context.Context, *Cache) error { return errors.New("flush failed") })

	app.Get[*Cache]()
	_ = app.Stop(context.Background())
	// Output:
	// build *github.com/floatdrop/di_test.Cache <nil>
	// stop *github.com/floatdrop/di_test.Cache di: stopping *github.com/floatdrop/di_test.Cache: flush failed
}
