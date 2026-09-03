package app

import (
	"testing"

	"github.com/floatdrop/di"
)

func TestRepo(t *testing.T) {
	s := di.Test(t, Wire)                // production graph, stopped when the test ends
	s.Value(&DB{DSN: "sqlite://memory"}) // later registration wins: replaces the production *DB

	repo := s.Get[*Repo]() // built against the fake DB
	if repo.DB.DSN != "sqlite://memory" {
		t.Fatalf("got %q", repo.DB.DSN)
	}
}
