package storage_test

import (
	"context"
	"strings"
	"testing"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/examples/guide/internal/config"
	"github.com/floatdrop/di/examples/guide/internal/storage"
)

// The production modules, with the configuration overridden: the store is
// built against a database that dials the test DSN, and nothing else in the
// wiring changes.
func TestStoreFindsUsers(t *testing.T) {
	s := di.Test(t, config.Module, storage.Module)
	s.Value(config.Config{DSN: "sqlite://memory"}).Override()

	user, err := s.Get[storage.Store]().Find(context.Background(), "42")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(user.Name, "sqlite://memory") {
		t.Fatalf("store was built against the wrong database: %q", user.Name)
	}
}
