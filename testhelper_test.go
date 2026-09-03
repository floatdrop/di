package di_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/floatdrop/di"
)

type fakeTB struct {
	testing.TB // panics if anything else is called
	cleanups   []func()
	errors     []string
}

func (f *fakeTB) Helper()                           {}
func (f *fakeTB) Cleanup(fn func())                 { f.cleanups = append(f.cleanups, fn) }
func (f *fakeTB) Errorf(format string, args ...any) { f.errors = append(f.errors, format) }
func (f *fakeTB) runCleanups() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

func wireProd(s *di.Scope) {
	s.Provide(func(*di.Scope) *DB { return &DB{dsn: "prod"} })
	s.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} })
}

func TestTestWiresOverridesAndStops(t *testing.T) {
	tb := &fakeTB{}
	stopped := false
	s := di.Test(tb, wireProd)
	s.Value(&DB{dsn: "fake"}).OnStop(func(context.Context, *DB) error { stopped = true; return nil })
	if got := s.Get[*Repo]().db.dsn; got != "fake" {
		t.Fatalf("override not applied: %q", got)
	}
	tb.runCleanups()
	if !stopped {
		t.Fatal("scope not stopped at cleanup")
	}
	if _, err := s.Resolve[*Repo](); !errors.Is(err, di.ErrStopped) {
		t.Fatalf("scope should be stopped: %v", err)
	}
}

func TestTestReportsStopErrors(t *testing.T) {
	tb := &fakeTB{}
	s := di.Test(tb)
	s.Value(&DB{}).OnStop(func(context.Context, *DB) error { return errors.New("close failed") })
	s.Get[*DB]()
	tb.runCleanups()
	if len(tb.errors) != 1 || !strings.Contains(tb.errors[0], "stopping test scope") {
		t.Fatalf("stop error not reported: %v", tb.errors)
	}
}

func TestTestWithRealT(t *testing.T) {
	s := di.Test(t, wireProd)
	if s.Get[*Repo]().db.dsn != "prod" {
		t.Fatal("wiring not applied")
	}
}
