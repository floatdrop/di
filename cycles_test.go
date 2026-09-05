package di_test

// Regressions in the resolution path: what counts as a cycle, what only looks
// like one, and what two branches racing for the same build must agree on.
//
// One test per defect, named for the rule it pins. The tag at the end of a
// comment says where the defect came from. (review 1, 3) is the third defect
// of the first September 2026 review, checked against 12dba3c; review 2 was
// checked against 2b8915d and review 3 against 9ace680. (pass 4) is the
// fourth of the seven narrower passes that preceded those reviews, each
// checked against the code before the instance-phase refactor, and (alias
// refactor) the sweep that made lookup follow Bind chains. An untagged test
// comes from the first of those passes, or from the generators, which its own
// comment says. Several fail by hanging rather than by reporting, which is why
// each bounds its own wait instead of relying on the package timeout.

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/floatdrop/di"
)

// An alias chain that loops is a cycle, not a hang or a stack overflow.
// Two interfaces with the same method set each satisfy the other, so they
// can be bound to one another.
// (alias refactor)
func TestRegressionAliasCycle(t *testing.T) {
	s := di.New()
	s.Bind[readerA, readerB]()
	s.Bind[readerB, readerA]() // readerA -> readerB -> readerA
	if _, err := s.Resolve[readerA](); !errors.Is(err, di.ErrCycle) {
		t.Fatalf("got %v, want ErrCycle", err)
	}
	if _, ok := s.Maybe[readerA](); ok {
		t.Fatal("Maybe reported a looping alias chain as present")
	}
}

// A constructor may resolve its dependencies from several goroutines. The
// resolution path must not be shared mutable state.
// (review 1, 3)
func TestReviewParallelDependenciesInsideConstructor(t *testing.T) {
	for range 50 {
		s := di.New()
		s.Provide(func(*di.Scope) *vA { return &vA{} })
		s.Provide(func(*di.Scope) *vB { return &vB{} })
		s.Provide(func(sc *di.Scope) *Repo {
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					if _, err := sc.Resolve[*vA](); err != nil {
						t.Errorf("parallel resolve of vA: %v", err)
					}
				})
				wg.Go(func() {
					if _, err := sc.Resolve[*vB](); err != nil {
						t.Errorf("parallel resolve of vB: %v", err)
					}
				})
			}
			wg.Wait()
			return &Repo{}
		})
		if _, err := s.Resolve[*Repo](); err != nil {
			t.Fatalf("false failure: %v", err)
		}
	}
}

// Two goroutines asking for the same singleton from inside one
// constructor is not a cycle. It reported one deterministically.
// (review 1, 3b)
func TestReviewParallelSameDependencyIsNotACycle(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *vA { return &vA{} })
	s.Provide(func(sc *di.Scope) *Repo {
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Go(func() { _, errs[i] = sc.Resolve[*vA]() })
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Errorf("false cycle: %v", err)
			}
		}
		return &Repo{}
	})
	if _, err := s.Resolve[*Repo](); err != nil {
		t.Fatal(err)
	}
}

// A cycle entered from both ends at once must be reported, not deadlock.
// Neither branch has the repeated key on its own path, so it is only visible
// through the wait-for graph.
// (review 1, 4)
func TestReviewConcurrentCycle(t *testing.T) {
	s := di.New()
	inA, inB := make(chan struct{}), make(chan struct{})
	both := make(chan struct{})
	s.Provide(func(sc *di.Scope) *vA { close(inA); <-both; sc.Get[*vB](); return &vA{} })
	s.Provide(func(sc *di.Scope) *vB { close(inB); <-both; sc.Get[*vA](); return &vB{} })

	errs := make(chan error, 2)
	go func() { _, err := s.Resolve[*vA](); errs <- err }()
	go func() { _, err := s.Resolve[*vB](); errs <- err }()
	<-inA
	<-inB
	close(both)

	var reported int
	for range 2 {
		select {
		case err := <-errs:
			if errors.Is(err, di.ErrCycle) {
				reported++
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a concurrent cycle deadlocked instead of reporting ErrCycle")
		}
	}
	if reported == 0 {
		t.Fatal("neither branch reported ErrCycle")
	}
}

// A group member and a plain registration of the same type are different
// bindings. Cycle detection must compare bindings, not keys.
// (review 1, 8)
func TestReviewGroupAndDirectSameType(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) vI { return &vT{n: 1} })
	s.Provide(func(sc *di.Scope) vI { _ = sc.Get[vI](); return &vT{n: 2} }).Group()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("All reported a false cycle: %v", r)
		}
	}()
	if got := s.All[vI](); len(got) != 1 {
		t.Fatalf("got %d group members", len(got))
	}
}

// A constructor may keep the Scope it was handed, which is how a goroutine
// it starts resolves later. A resolution made through that Scope after the
// constructor returned is not a cycle, and must not poison the instance it
// builds for every later resolution either.
// (review 2, 5)
func TestReview2RetainedScopeIsNotACycle(t *testing.T) {
	root := di.New()
	root.Provide(func(sc *di.Scope) *wA { return &wA{sc: sc} })
	root.Provide(func(sc *di.Scope) *wB { return &wB{a: sc.Get[*wA]()} })

	a := root.Get[*wA]()
	b, err := a.sc.Resolve[*wB]()
	if err != nil {
		t.Fatalf("deferred resolve through the retained Scope: %v", err)
	}
	if b.a != a {
		t.Fatal("it did not see the finished instance")
	}
	if _, err := root.Resolve[*wB](); err != nil {
		t.Fatalf("a later clean resolve inherited the failure: %v", err)
	}
}

// One alias to a Scoped target is a different edge in each scope that holds
// an instance of that target, so reaching it twice at two holders is not a
// cycle. wT takes an optional per-scope decoration, present only in the child,
// which is what makes the root's own wT a leaf.
// (review 2, 6)
func TestReview2AliasAcrossScopedHolders(t *testing.T) {
	root := di.New()
	root.Provide(func(sc *di.Scope) *wT {
		if _, ok := sc.Maybe[*wQ](); ok {
			_ = sc.Get[*wU]()
		}
		return &wT{}
	}).Scoped()
	root.Bind[wI, *wT]()
	root.Provide(func(sc *di.Scope) *wU { return &wU{i: sc.Get[wI]()} })

	kid := root.Child("kid")
	kid.Value(&wQ{})

	v, err := kid.Resolve[wI]()
	if err != nil {
		t.Fatalf("acyclic graph reported as a cycle: %v", err)
	}
	if v.tag() != "T" {
		t.Fatalf("got %q", v.tag())
	}
}

// A resolution made through the Scope a finished constructor kept is a new
// branch: the ancestors above that constructor are still building, but not for
// it, so it has only to wait for them. Reporting a cycle also cached the
// verdict on whatever it was building, which outlived the timing that caused
// it.
// (review 3, 4)
func TestReview3LateResolutionThroughAFinishedConstructorIsNotACycle(t *testing.T) {
	root := di.New()
	built := make(chan struct{})
	finishA := make(chan struct{})
	var saved *di.Scope

	root.Provide(func(s *di.Scope) *r3A {
		s.Get[*r3B]()
		close(built) // B's constructor has returned; A is still building
		<-finishA
		return &r3A{}
	})
	root.Provide(func(s *di.Scope) *r3B { saved = s; return &r3B{sc: s} })
	root.Provide(func(s *di.Scope) *r3C { s.Get[*r3A](); return &r3C{} })

	a := make(chan error, 1)
	go func() { _, err := root.Resolve[*r3A](); a <- err }()
	<-built

	c := make(chan error, 1)
	go func() { _, err := saved.Resolve[*r3C](); c <- err }()

	select {
	case err := <-c:
		t.Fatalf("C resolved without waiting for A: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finishA)

	select {
	case err := <-c:
		if err != nil {
			t.Errorf("C through the kept Scope: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("C never returned")
	}
	if err := <-a; err != nil {
		t.Fatalf("A: %v", err)
	}
	if _, err := root.Resolve[*r3C](); err != nil {
		t.Errorf("second resolve of C: %v", err) // the verdict used to be cached
	}
}

// A child scope made inside a constructor carries that constructor's
// resolution, so a request through it that leads back to the service being
// built is a cycle. Starting a fresh path there left the two halves waiting
// for each other with nothing to report it.
// (review 3, 6)
func TestReview3ChildMadeInAConstructorKeepsTheCyclePath(t *testing.T) {
	root := di.New()
	inner := make(chan error, 1)
	root.Provide(func(s *di.Scope) *r3Self {
		_, err := s.Child("request").Resolve[*r3Self]()
		inner <- err
		return &r3Self{}
	})

	done := make(chan struct{})
	go func() { defer close(done); _, _ = root.Resolve[*r3Self]() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resolving through a child made in the constructor deadlocked")
	}
	if err := <-inner; !errors.Is(err, di.ErrCycle) {
		t.Errorf("want ErrCycle, got %v", err)
	}
}

// The other half of that: a child kept past the constructor resolves as an
// independent branch, and reports failure the way a top-level call does rather
// than unwinding to a call that has long returned. This one passes before the
// fix as well: it guards the other half of the Child change rather than a
// defect.
// (review 3)
func TestReview3ChildKeptPastTheConstructorIsIndependent(t *testing.T) {
	root := di.New()
	var kept *di.Scope
	root.Provide(func(s *di.Scope) *r3Self { kept = s.Child("request"); return &r3Self{} })
	root.Value(&r3Plain{})
	if _, err := root.Resolve[*r3Self](); err != nil {
		t.Fatal(err)
	}

	if _, err := kept.Resolve[*r3Plain](); err != nil {
		t.Errorf("resolving through the kept child: %v", err)
	}
	if v := kept.Get[*r3Plain](); v == nil {
		t.Error("Get through the kept child returned nothing")
	}
	func() {
		defer func() {
			rec := recover()
			if err, ok := rec.(error); !ok || !errors.Is(err, di.ErrNotProvided) {
				t.Errorf("want a panic carrying ErrNotProvided, got %#v", rec)
			}
		}()
		kept.Get[*r3C]()
	}()
}
