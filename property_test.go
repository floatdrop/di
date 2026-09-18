package di_test

// A property test for the eager set: random registration sequences checked
// against the documented invariant:
//
//	Lifetime and lifecycle hooks belong to a registration. Eagerness belongs
//	to the key: for every key, Start builds whatever serves that key exactly
//	once if any registration for the key was marked Eager, at the position of
//	the first such registration. A group member is its own entry. A binding
//	that serves an eager key may not have a per-scope lifetime, nor may a
//	single registration combine Eager with one.
//
// And the rule that decides which registration serves a key at all:
//
//	Within one scope, a second registration of a key must be marked Override
//	and then replaces the first; an unmarked one is rejected, and so is an
//	Override with nothing to override. Group members accumulate instead and
//	never override anything. A wrapper composes over what serves the key,
//	takes its lifetime, and builds it first; nothing to wrap is rejected.
//	A registration through a Tag view is a registration of another key, the
//	type tagged, and the rules apply to that key.
//
// The generator starts from a fresh scope each iteration and never touches
// one again after a rejection, so freeze's error paths are left to the
// regression tests.

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

// pkI is an interface key, served by a constructor returning *pk1.
type pkI interface{ marker() }

func (*pk1) marker() {}

var (
	propKinds = []string{"provide", "value", "scoped", "group", "wire", "wrap"}
	propNames = []string{"pk1", "pk2", "pk3", "pkI"}
)

type propStep struct {
	key      int
	kind     string
	eager    bool
	override bool
	tag      bool // registered through s.Tag[pTag](), under the key tagged pTag
}

// pTag is the one tag the generator registers under.
type pTag struct{}

func (st propStep) String() string {
	return fmt.Sprintf("%s/%s/eager=%v/override=%v/tag=%v", propNames[st.key], st.kind, st.eager, st.override, st.tag)
}

func regKind[T any](s *di.Scope, mk func() T, kind string, eager, override, tag bool) {
	if tag {
		s = s.Tag[pTag]()
	}
	var b di.Binding[T]
	switch kind {
	case "provide":
		b = s.Provide(func(*di.Scope) T { return mk() })
	case "value":
		b = s.Value(mk())
	case "scoped":
		b = s.Provide(func(*di.Scope) T { return mk() }).Scoped()
	case "group":
		b = s.Provide(func(*di.Scope) T { return mk() }).Group()
	case "wire":
		b = s.Wire[T](mk) // a singleton like provide, with its dependencies declared
	case "wrap":
		b = s.Wrap[T](func(T) T { return mk() })
	}
	if eager {
		b.Eager()
	}
	if override {
		b.Override()
	}
}

func applyStep(s *di.Scope, st propStep) {
	switch st.key {
	case 0:
		regKind(s, func() *pk1 { return &pk1{} }, st.kind, st.eager, st.override, st.tag)
	case 1:
		regKind(s, func() *pk2 { return &pk2{} }, st.kind, st.eager, st.override, st.tag)
	case 2:
		regKind(s, func() *pk3 { return &pk3{} }, st.kind, st.eager, st.override, st.tag)
	case 3:
		regKind(s, func() pkI { return &pk1{} }, st.kind, st.eager, st.override, st.tag)
	}
}

type propWant struct {
	builds []string
	panics bool
}

// wantEager derives what the container must do with a sequence.
func wantEager(steps []propStep) propWant {
	// A tagged registration is one of another key, so it is modelled as the
	// key four along; propNames is read modulo the plain keys.
	steps = slices.Clone(steps)
	for i, st := range steps {
		if st.tag {
			steps[i].key += len(propNames)
		}
	}
	winner := map[int]string{} // the registration that serves the key
	scoped := map[int]bool{}   // whether that registration is per-scope
	chain := map[int]int{}     // how many registrations build for the key: the winner and what it wraps
	for _, st := range steps {
		switch {
		case st.kind == "group":
			if st.override {
				return propWant{panics: true} // members accumulate; nothing to replace
			}
		case st.kind == "wrap":
			if winner[st.key] == "" || st.override {
				return propWant{panics: true} // nothing to wrap, or a marker a wrapper rejects
			}
			winner[st.key] = "wrap" // takes the wrapped lifetime, so scoped stays
			chain[st.key]++
		case winner[st.key] != "" && !st.override:
			return propWant{panics: true} // a second registration must say so
		case winner[st.key] == "" && st.override:
			return propWant{panics: true} // nothing to override
		default:
			winner[st.key], scoped[st.key], chain[st.key] = st.kind, st.kind == "scoped", 1
		}
	}

	// A registration may not combine Eager with a per-scope lifetime; a
	// wrapper's lifetime is the one it inherited when registered, so it is
	// replayed.
	inherited := map[int]bool{}
	for _, st := range steps {
		switch st.kind {
		case "group":
		case "wrap":
			if st.eager && inherited[st.key] {
				return propWant{panics: true}
			}
		default:
			inherited[st.key] = st.kind == "scoped"
			if st.eager && st.kind == "scoped" {
				return propWant{panics: true}
			}
		}
	}
	for _, st := range steps {
		if !st.eager || st.kind == "group" {
			continue
		}
		// Whatever serves an eager key must be able to honour eagerness.
		if scoped[st.key] {
			return propWant{panics: true}
		}
	}

	var want propWant
	seen := map[string]bool{}
	for i, st := range steps {
		if !st.eager {
			continue
		}
		id := fmt.Sprintf("g%d", i) // a group member is its own entry
		if st.kind != "group" {
			id = fmt.Sprintf("k%d", st.key)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		n := 1
		if st.kind != "group" {
			n = chain[st.key] // a wrapper builds what it wraps first, under the same key
		}
		for range n {
			want.builds = append(want.builds, propNames[st.key%len(propNames)])
		}
	}
	return want
}

// runSteps registers the sequence and starts the scope, reporting the builds
// observed and how the container reacted.
func runSteps(steps []propStep) (got []string, panicked bool, err error) {
	s := di.New()
	s.Observe(func(ev di.Event) {
		if ev.Kind == di.EventBuild {
			name, _, _ := strings.Cut(ev.Service, " tagged ")
			got = append(got, name[strings.LastIndex(name, ".")+1:])
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
	for iter := range 6000 {
		steps := make([]propStep, 1+rng.IntN(6))
		for i := range steps {
			steps[i] = propStep{
				key:      rng.IntN(len(propNames)),
				kind:     propKinds[rng.IntN(len(propKinds))],
				eager:    rng.IntN(2) == 0,
				override: rng.IntN(3) == 0,
				tag:      rng.IntN(4) == 0,
			}
		}
		want := wantEager(steps)
		got, panicked, err := runSteps(steps)

		fail := func(format string, args ...any) {
			t.Fatalf("iteration %d, steps %v:\n"+format, append([]any{iter, steps}, args...)...)
		}
		switch {
		case want.panics && !panicked:
			fail("expected rejection, got builds %v (err %v)", got, err)
		case !want.panics && panicked:
			fail("unexpected rejection")
		case panicked:
			continue
		case err != nil:
			fail("Start failed: %v", err)
		case !slices.Equal(got, want.builds):
			fail("built %v, want %v", got, want.builds)
		}
	}
}
