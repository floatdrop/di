// Package cache puts a cache in front of the store. It replaces nothing:
// the store keeps its registration and hooks, and this wraps it.
package cache

import (
	"context"
	"sync"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/examples/guide/internal/storage"
)

type Cache struct {
	mu    sync.Mutex
	users map[string]storage.User
	Hits  int
}

func New() *Cache { return &Cache{users: map[string]storage.User{}} }

// CachingStore is a Store that asks the one it wraps only on a miss.
type CachingStore struct {
	next  storage.Store
	cache *Cache
}

// NewCachingStore takes the store it wraps first, then its dependencies.
func NewCachingStore(next storage.Store, c *Cache) storage.Store {
	return &CachingStore{next: next, cache: c}
}

func (s *CachingStore) Find(ctx context.Context, id string) (storage.User, error) {
	s.cache.mu.Lock()
	user, ok := s.cache.users[id]
	if ok {
		s.cache.Hits++
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

// Module registers the cache and wraps whatever serves Store by now. Order
// matters: this module comes after storage's.
func Module(s *di.Scope) {
	s.Wire[*Cache](New)
	s.Wrap[storage.Store](NewCachingStore)
}
