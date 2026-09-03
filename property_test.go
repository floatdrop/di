package di_test

// A property test for the eager set, the part of the container that has
// broken most often. Rather than checking specific cases, it generates
// random registration sequences and asserts the documented invariant:
//
//	For every key, Start builds the binding that owns the key exactly once
//	if any registration for that key was marked Eager, at the position of
//	the first such registration. A group member is its own entry. A binding
//	that owns the key but has a per-scope lifetime is rejected.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/floatdrop/di"
)

type pk1 struct{}
type pk2 struct{}
type pk3 struct{}

var (
	propKinds = []string{"provide", "value", "scoped", "transient", "group"}
	propNames = []string{"pk1", "pk2", "pk3"}
)

type propStep struct {
	key   int
	kind  string
	eager bool
}

func (st propStep) String() string {
	return fmt.Sprintf("%s/%s/eager=%v", propNames[st.key], st.kind, st.eager)
}

func regKind[T any](s *di.Scope, mk func() T, kind string, eager bool) {
	var b di.Binding[T]
	switch kind {
	case "provide":
		b = s.Provide(func(*di.Scope) T { return mk() })
	case "value":
		b = s.Value(mk())
	case "scoped":
		b = s.Provide(func(*di.Scope) T { return mk() }).Scoped()
	case "transient":
		b = s.Provide(func(*di.Scope) T { return mk() }).Transient()
	case "group":
		b = s.Add(func(*di.Scope) T { return mk() })
	}
	if eager {
		b.Eager()
	}
}

func applyStep(s *di.Scope, st propStep) {
	switch st.key {
	case 0:
		regKind(s, func() *pk1 { return &pk1{} }, st.kind, st.eager)
	case 1:
		regKind(s, func() *pk2 { return &pk2{} }, st.kind, st.eager)
	case 2:
		regKind(s, func() *pk3 { return &pk3{} }, st.kind, st.eager)
	}
}

// wantEager derives the expected build sequence, or reports that the
// sequence must be rejected.
func wantEager(steps []propStep) (want []string, wantPanic bool) {
	winner := map[int]string{} // last non-group registration owns the key
	for _, st := range steps {
		if st.kind != "group" {
			winner[st.key] = st.kind
		}
	}
	for _, st := range steps {
		// A registration that marks itself both Eager and per-scope is
		// contradictory on its own terms, override or not.
		if st.eager && (st.kind == "scoped" || st.kind == "transient") {
			return nil, true
		}
		if !st.eager || st.kind == "group" {
			continue
		}
		// Eagerness cannot transfer to a per-scope binding that owns the key.
		if k := winner[st.key]; k == "scoped" || k == "transient" {
			return nil, true
		}
	}
	seen := map[string]bool{}
	for i, st := range steps {
		if !st.eager {
			continue
		}
		id := fmt.Sprintf("k%d", st.key)
		if st.kind == "group" {
			id = fmt.Sprintf("g%d", i) // each member is its own entry
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		want = append(want, propNames[st.key])
	}
	return want, false
}

// runSteps registers the sequence and starts the scope, reporting the build
// order observed and whether the container rejected the sequence.
func runSteps(steps []propStep) (got []string, panicked bool, err error) {
	s := di.New()
	s.Observe(func(ev di.Event) {
		if ev.Kind == di.EventBuild {
			got = append(got, ev.Service[strings.LastIndex(ev.Service, ".")+1:])
		}
	})
	defer func() {
		if r := recover(); r != nil {
			panicked = true
		}
	}()
	for _, st := range steps {
		applyStep(s, st)
	}
	err = s.Start(context.Background())
	return got, false, err
}

func TestPropertyEagerSet(t *testing.T) {
	rng := rand.New(rand.NewPCG(0x5eed, 0xf00d))
	for iter := range 4000 {
		steps := make([]propStep, 1+rng.IntN(6))
		for i := range steps {
			steps[i] = propStep{
				key:   rng.IntN(len(propNames)),
				kind:  propKinds[rng.IntN(len(propKinds))],
				eager: rng.IntN(2) == 0,
			}
		}
		want, wantPanic := wantEager(steps)
		got, panicked, err := runSteps(steps)

		fail := func(format string, args ...any) {
			t.Fatalf("iteration %d, steps %v:\n"+format, append([]any{iter, steps}, args...)...)
		}
		switch {
		case wantPanic && !panicked:
			fail("expected rejection, got build order %v (err %v)", got, err)
		case !wantPanic && panicked:
			fail("unexpected rejection")
		case panicked:
			continue
		case err != nil:
			fail("Start failed: %v", err)
		case !slices.Equal(got, want):
			fail("built %v, want %v", got, want)
		}
	}
}
