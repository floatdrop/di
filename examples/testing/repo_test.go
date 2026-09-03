package app

import (
	"context"
	"testing"

	"github.com/floatdrop/di"
)

func TestRepo(t *testing.T) {
	s := di.New()
	Wire(s)
	s.Value(&DB{DSN: "sqlite://memory"}) // later registration wins: replaces the production *DB
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	repo := s.Get[*Repo]() // built against the fake DB
	if repo.DB.DSN != "sqlite://memory" {
		t.Fatalf("got %q", repo.DB.DSN)
	}
}
