// Package cache puts a cache in front of the store. It replaces nothing:
// the store keeps its registration and hooks, and this wraps it. The package
// exports only its Module.
package cache

import (
	"context"
	"sync"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/examples/guide/internal/storage"
)

type cache struct {
	mu    sync.Mutex
	users map[string]storage.User
	hits  int
}

func newCache() *cache { return &cache{users: map[string]storage.User{}} }

// cachingStore is a Store that asks the one it wraps only on a miss, and
// forwards what it does not change.
type cachingStore struct {
	next  storage.Store
	cache *cache
}

// newCachingStore takes the store it wraps first, then its dependencies.
func newCachingStore(next storage.Store, c *cache) storage.Store {
	return &cachingStore{next: next, cache: c}
}

func (s *cachingStore) Find(ctx context.Context, id string) (storage.User, error) {
	s.cache.mu.Lock()
	user, ok := s.cache.users[id]
	if ok {
		s.cache.hits++
	}
	s.cache.mu.Unlock()
	if ok {
		return user, nil
	}
	user, err := s.next.Find(ctx, id)
	if err != nil {
		return user, err
	}
	s.cache.mu.Lock()
	s.cache.users[id] = user
	s.cache.mu.Unlock()
	return user, nil
}

func (s *cachingStore) Ping(ctx context.Context) error { return s.next.Ping(ctx) }

// Module registers the cache and wraps whatever serves Store by now. Order
// matters: this module comes after storage's.
func Module(s *di.Scope) {
	s.Wire[*cache](newCache)
	s.Wrap[storage.Store](newCachingStore)
}
