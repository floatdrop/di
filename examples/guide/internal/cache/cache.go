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

// hit reads the cache and counts the lookup if it was there. It is a method
// of its own because the lock must be released before the store is asked.
func (c *cache) hit(id string) (storage.User, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	user, ok := c.users[id]
	if ok {
		c.hits++
	}
	return user, ok
}

func (c *cache) put(id string, user storage.User) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.users[id] = user
}

func (s *cachingStore) Find(ctx context.Context, id string) (storage.User, error) {
	if user, ok := s.cache.hit(id); ok {
		return user, nil
	}
	user, err := s.next.Find(ctx, id)
	if err != nil {
		return user, err
	}
	s.cache.put(id, user)
	return user, nil
}

func (s *cachingStore) Ping(ctx context.Context) error { return s.next.Ping(ctx) }

// Module registers the cache and wraps whatever serves Store by now. Order
// matters: this module comes after storage's.
func Module(s *di.Scope) {
	s.Wire[*cache](newCache)
	s.Wrap[storage.Store](newCachingStore)
}
