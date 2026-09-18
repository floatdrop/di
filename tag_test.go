package di_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/floatdrop/di"
)

// Fixtures for Tag: one database type registered twice under two tags, a
// consumer of one, and a consumer of both through a defined type.

type tDB struct{ dsn string }

type (
	tPrimary struct{}
	tReplica struct{}
)

type tReport struct{ db *tDB }

func newTReport(db *tDB) *tReport { return &tReport{db} }

// tRead is the defined type a consumer of both instances takes for one of
// them, filled from the tag by newTRead.
type tRead struct{ *tDB }

func newTRead(db *tDB) tRead { return tRead{db} }

type tRepo struct{ write, read *tDB }

func newTRepo(w *tDB, r tRead) *tRepo { return &tRepo{write: w, read: r.tDB} }

func TestTagServesTwoInstancesOfOneType(t *testing.T) {
	s := di.New()
	s.Tag[tPrimary]().Wire[*tDB](func() *tDB { return &tDB{"primary"} })
	s.Tag[tReplica]().Wire[*tDB](func() *tDB { return &tDB{"replica"} })
	s.Wire[tRead](newTRead).Needs(di.Tagged[*tDB, tReplica]())
	s.Wire[*tRepo](newTRepo).Needs(di.Tagged[*tDB, tPrimary]())
	repo := s.Get[*tRepo]()
	if repo.write.dsn != "primary" || repo.read.dsn != "replica" {
		t.Fatalf("got write=%s read=%s", repo.write.dsn, repo.read.dsn)
	}
	if s.Tag[tPrimary]().Get[*tDB]() != repo.write {
		t.Fatal("the view does not read the instance the constructor got")
	}
	// The plain key is another key: nothing provides it.
	if _, err := s.Resolve[*tDB](); !errors.Is(err, di.ErrNotProvided) {
		t.Fatalf("plain key: got %v", err)
	}
	if v, err := s.Tag[tReplica]().Resolve[*tDB](); err != nil || v.dsn != "replica" {
		t.Fatalf("Resolve through the view: got %v, %v", v, err)
	}
	if _, ok := s.Tag[tReplica]().Maybe[*tDB](); !ok {
		t.Fatal("Maybe through the view found nothing")
	}
}

// A Tagged need is matched by type like any other, so two parameters of one
// type are rejected rather than told apart by position.
func TestTaggedNeedIsMatchedByType(t *testing.T) {
	s := di.New()
	mustPanic(t, "matches 2 *github.com/floatdrop/di_test.tDB parameters, which type cannot tell apart", func() {
		s.Wire[*tRepo](func(w, r *tDB) *tRepo { return &tRepo{w, r} }).Needs(di.Tagged[*tDB, tPrimary]())
	})
}

// A parameter without a need reads the plain key, beside a tagged one.
func TestTaggedNeedBesideThePlainKey(t *testing.T) {
	s := di.New()
	s.Value(&tDB{"plain"})
	s.Tag[tReplica]().Value(&tDB{"replica"})
	s.Wire[tRead](newTRead).Needs(di.Tagged[*tDB, tReplica]())
	s.Wire[*tRepo](newTRepo)
	repo := s.Get[*tRepo]()
	if repo.write.dsn != "plain" || repo.read.dsn != "replica" {
		t.Fatalf("got write=%s read=%s", repo.write.dsn, repo.read.dsn)
	}
}

// The view tags only what is named through it: not a child, not a module,
// and not what a constructor registered through it resolves.
func TestTagViewDoesNotLeak(t *testing.T) {
	s := di.New()
	s.Value(&tDB{"plain"})
	tagged := s.Tag[tPrimary]()
	tagged.Provide(func(sc *di.Scope) *tReport { return &tReport{sc.Get[*tDB]()} })
	tagged.Use(func(sc *di.Scope) { sc.Value(&tReport{&tDB{"module"}}) })
	if got := tagged.Get[*tReport]().db.dsn; got != "plain" {
		t.Fatalf("constructor through the view resolved %s", got)
	}
	if got := s.Get[*tReport]().db.dsn; got != "module" {
		t.Fatalf("module through the view registered under %s", got)
	}
	if got := tagged.Child("c").Get[*tDB]().dsn; got != "plain" {
		t.Fatalf("child of the view resolved %s", got)
	}
}

func TestTagKeepsHooksAndMarkers(t *testing.T) {
	var log []string
	s := di.New()
	s.Tag[tPrimary]().Provide(func(*di.Scope) *tDB { return &tDB{"primary"} }).
		OnStart(func(_ context.Context, db *tDB) error { log = append(log, "start "+db.dsn); return nil }).
		OnStop(func(_ context.Context, db *tDB) error { log = append(log, "stop "+db.dsn); return nil }).
		Eager()
	s.Tag[tReplica]().Provide(func(*di.Scope) *tDB { return &tDB{"scoped"} }).Scoped()
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	c1, c2 := s.Child("c1"), s.Child("c2")
	if c1.Tag[tReplica]().Get[*tDB]() == c2.Tag[tReplica]().Get[*tDB]() {
		t.Fatal("Scoped did not survive the tag")
	}
	if err := s.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ", "); got != "start primary, stop primary" {
		t.Fatalf("got %s", got)
	}
}

// A tagged group is read through the same view.
func TestTagGroups(t *testing.T) {
	s := di.New()
	s.Value(&tDB{"plain"}).Group()
	s.Tag[tReplica]().Value(&tDB{"r1"}).Group()
	s.Tag[tReplica]().Value(&tDB{"r2"}).Group()
	if n := len(s.All[*tDB]()); n != 1 {
		t.Fatalf("plain group has %d members", n)
	}
	if n := len(s.Tag[tReplica]().All[*tDB]()); n != 2 {
		t.Fatalf("tagged group has %d members", n)
	}
}

func TestTagOverridesAndCollidesUnderTheTag(t *testing.T) {
	s := di.New()
	s.Tag[tPrimary]().Value(&tDB{"prod"})
	s.Tag[tPrimary]().Value(&tDB{"fake"}).Override()
	if got := s.Tag[tPrimary]().Get[*tDB]().dsn; got != "fake" {
		t.Fatalf("got %s", got)
	}
	s2 := di.New()
	s2.Value(&tDB{"plain"})
	s2.Tag[tPrimary]().Value(&tDB{"a"})
	s2.Tag[tPrimary]().Value(&tDB{"b"})
	mustPanic(t, "*github.com/floatdrop/di_test.tDB tagged github.com/floatdrop/di_test.tPrimary is provided at", func() { s2.Tag[tPrimary]().Get[*tDB]() })
	// Override with only the plain key to override is nothing to override.
	s3 := di.New()
	s3.Value(&tDB{"plain"})
	s3.Tag[tPrimary]().Value(&tDB{"fake"}).Override()
	mustPanic(t, "is marked Override() but nothing in scope root provides it", func() { s3.Get[*tDB]() })
}

func TestTagWraps(t *testing.T) {
	s := di.New()
	s.Value(&tDB{"plain"})
	s.Tag[tPrimary]().Value(&tDB{"primary"})
	s.Tag[tPrimary]().Wrap[*tDB](func(next *tDB) *tDB { return &tDB{"wrapped " + next.dsn} })
	if got := s.Tag[tPrimary]().Get[*tDB]().dsn; got != "wrapped primary" {
		t.Fatalf("got %s", got)
	}
	if got := s.Get[*tDB]().dsn; got != "plain" {
		t.Fatalf("the plain key was wrapped: %s", got)
	}
	s2 := di.New()
	s2.Value(&tDB{"plain"})
	mustPanic(t, "nothing provides *github.com/floatdrop/di_test.tDB tagged github.com/floatdrop/di_test.tReplica", func() {
		s2.Tag[tReplica]().Wrap[*tDB](func(next *tDB) *tDB { return next })
	})
}

func TestTaggedNeedRejections(t *testing.T) {
	t.Run("no parameter of the type", func(t *testing.T) {
		s := di.New()
		mustPanic(t, "(Tagged[*github.com/floatdrop/di_test.tDB, github.com/floatdrop/di_test.tPrimary]) matches no *github.com/floatdrop/di_test.tDB parameter", func() {
			s.Wire[*tRepo](func() *tRepo { return nil }).Needs(di.Tagged[*tDB, tPrimary]())
		})
	})
	t.Run("a second tag for the parameter", func(t *testing.T) {
		s := di.New()
		mustPanic(t, "would change the *github.com/floatdrop/di_test.tDB parameter, already resolved as Tagged", func() {
			s.Wire[*tReport](newTReport).Needs(di.Tagged[*tDB, tPrimary](), di.Tagged[*tDB, tReplica]())
		})
	})
	t.Run("optional over a tagged parameter", func(t *testing.T) {
		s := di.New()
		mustPanic(t, "already resolved as Tagged", func() {
			s.Wire[*tReport](newTReport).Needs(di.Tagged[*tDB, tPrimary](), di.Optional[*tDB]())
		})
	})
	t.Run("missing at build", func(t *testing.T) {
		s := di.New()
		s.Value(&tDB{"plain"})
		s.Wire[*tReport](newTReport).Needs(di.Tagged[*tDB, tPrimary]())
		_, err := s.Resolve[*tReport]()
		if !errors.Is(err, di.ErrNotProvided) || !strings.Contains(err.Error(), "tDB tagged github.com/floatdrop/di_test.tPrimary: not provided") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestTagExplainsAndReports(t *testing.T) {
	s := di.New()
	s.Tag[tPrimary]().Value(&tDB{"primary"}) // the site is this line
	s.Tag[tPrimary]().Get[*tDB]()
	if out := s.Explain[*tDB](); !strings.Contains(out, "not provided") {
		t.Fatalf("plain key: got %s", out)
	}
	out := s.Tag[tPrimary]().Explain[*tDB]()
	if !strings.Contains(out, "*github.com/floatdrop/di_test.tDB tagged github.com/floatdrop/di_test.tPrimary: value in root, built") || !strings.Contains(out, "tag_test.go:") {
		t.Fatalf("tagged key: got %s", out)
	}
	if out := s.Modules(); !strings.Contains(out, "*di_test.tDB tagged di_test.tPrimary") {
		t.Fatalf("modules: got %s", out)
	}
}

func TestTagIsValidated(t *testing.T) {
	s := di.New()
	s.Tag[tPrimary]().Wire[*tDB](func() *tDB { return &tDB{"primary"} })
	s.Wire[tRead](newTRead).Needs(di.Tagged[*tDB, tReplica]())
	s.Wire[*tRepo](newTRepo).Needs(di.Tagged[*tDB, tPrimary]())
	r := s.Validate()
	if len(r.Errors) != 1 || !errors.Is(r.Errors[0], di.ErrNotProvided) ||
		!strings.Contains(r.Errors[0].Error(), "tDB tagged github.com/floatdrop/di_test.tReplica: not provided") {
		t.Fatalf("got %+v", r)
	}
	// A stub names a tagged key the resolving scope will hold.
	s2 := di.New()
	s2.Wire[*tReport](newTReport).Needs(di.Tagged[*tDB, tReplica]()).Scoped()
	if err := s2.Validate(di.Provided[*tDB]()).Err(); err == nil {
		t.Fatal("a plain stub satisfied a tagged dependency")
	}
	if err := s2.Validate(di.Provided[*tDB]().Tag[tReplica]()).Err(); err != nil {
		t.Fatalf("tagged stub: %v", err)
	}
}
