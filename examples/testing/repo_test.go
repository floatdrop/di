package app

import (
	"testing"

	"github.com/floatdrop/di"
)

func TestRepo(t *testing.T) {
	s := di.Test(t, Production)                     // production graph, stopped when the test ends
	s.Value(&DB{DSN: "sqlite://memory"}).Override() // replaces the production *DB, and says so

	repo := s.Get[*Repo]() // built against the fake DB
	if repo.DB.DSN != "sqlite://memory" {
		t.Fatalf("got %q", repo.DB.DSN)
	}
}
