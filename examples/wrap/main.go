// Wrap: compose over a service without replacing it. The wrapped
// registration keeps its hooks and lifetime, and a wrapper in a child scope
// applies to that scope alone.
package main

import (
	"context"
	"fmt"

	"github.com/floatdrop/di"
)

type Store interface{ Get(key string) string }

type PGStore struct{}
type Cache struct{ hits int }
type CachingStore struct {
	next  Store
	cache *Cache
}
type TracingStore struct{ next Store }

func (*PGStore) Get(key string) string        { return "row " + key }
func (c *CachingStore) Get(key string) string { c.cache.hits++; return c.next.Get(key) }
func (t *TracingStore) Get(key string) string { return "traced(" + t.next.Get(key) + ")" }

func NewPGStore() *PGStore                       { return &PGStore{} }
func NewCachingStore(next Store, c *Cache) Store { return &CachingStore{next: next, cache: c} }
func NewTracingStore(next Store) Store           { return &TracingStore{next: next} }

func main() {
	app := di.New()
	app.Value(&Cache{})
	app.Wire[Store](NewPGStore).
		OnStop(func(context.Context, Store) error { fmt.Println("pg closed"); return nil })

	// The first parameter is the value being wrapped; the rest are
	// dependencies. The store keeps its OnStop, and is stopped after the
	// wrapper, since it was built first.
	app.Wrap[Store](NewCachingStore)

	// A wrapper in a child scope wraps the parent's value for that scope and
	// its descendants; the parent and its other children are untouched.
	debug := app.Child("debug")
	debug.Wrap[Store](NewTracingStore)

	fmt.Println("app:  ", app.Get[Store]().Get("1"))
	fmt.Println("debug:", debug.Get[Store]().Get("1"))
	fmt.Print(app.Explain[Store]())
	_ = app.Stop(context.Background())
}
