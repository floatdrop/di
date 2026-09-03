package di_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

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

func ExampleBinding_Run() {
	app := di.New()
	done := make(chan string, 1)
	app.Provide(func(*di.Scope) *Queue { return &Queue{jobs: make(chan string, 1)} }).Eager().
		Run(func(ctx context.Context, q *Queue) error {
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

type Cache struct{ ok bool }

func ExampleBinding_Health() {
	app := di.New()
	app.Provide(func(*di.Scope) *Cache { return &Cache{} }).
		Health(func(ctx context.Context, c *Cache) error {
			if !c.ok {
				return errors.New("not connected")
			}
			return nil
		})

	fmt.Println("before build:", app.HealthCheck(context.Background()))
	app.Get[*Cache]()
	err := app.HealthCheck(context.Background())
	fmt.Println("unhealthy:", errors.Is(err, di.ErrUnhealthy))
	fmt.Println(err)
	// Output:
	// before build: <nil>
	// unhealthy: true
	// di: *github.com/floatdrop/di_test.Cache unhealthy: not connected
}

type Caller struct{ Name string }

func ExampleScope_Middleware() {
	app := di.New()
	app.Provide(func(s *di.Scope) *Caller {
		return &Caller{Name: s.Get[*http.Request]().Header.Get("X-Caller")}
	}).Scoped()

	h := app.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := di.FromContext(r.Context())
		fmt.Fprintln(w, "hello", req.Get[*Caller]().Name)
	}))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Caller", "ada")
	h.ServeHTTP(rec, r)
	fmt.Print(rec.Body.String())
	// Output:
	// hello ada
}

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
