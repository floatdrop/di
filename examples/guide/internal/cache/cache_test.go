package cache_test

import (
	"context"
	"testing"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/examples/guide/internal/cache"
	"github.com/floatdrop/di/examples/guide/internal/config"
	"github.com/floatdrop/di/examples/guide/internal/storage"
)

func TestSecondLookupIsAHit(t *testing.T) {
	s := di.Test(t, config.Module, storage.Module, cache.Module)
	store := s.Get[storage.Store]()
	for range 2 {
		if _, err := store.Find(context.Background(), "42"); err != nil {
			t.Fatal(err)
		}
	}
	if hits := s.Get[*cache.Cache]().Hits; hits != 1 {
		t.Fatalf("want one hit, got %d", hits)
	}
}
