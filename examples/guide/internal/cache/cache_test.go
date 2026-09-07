package cache

import (
	"context"
	"testing"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/examples/guide/internal/config"
	"github.com/floatdrop/di/examples/guide/internal/storage"
)

// An internal test, since the cache is private: only this package can name
// *cache, so only this package can read its hits.
func TestSecondLookupIsAHit(t *testing.T) {
	s := di.Test(t, config.Module, storage.Module, Module)
	store := s.Get[storage.Store]()
	for range 2 {
		if _, err := store.Find(context.Background(), "42"); err != nil {
			t.Fatal(err)
		}
	}
	if hits := s.Get[*cache]().hits; hits != 1 {
		t.Fatalf("want one hit, got %d", hits)
	}
}
