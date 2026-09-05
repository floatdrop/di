package di_test

// A property test for the eager set, the part of the container that has
// broken most often. Rather than checking specific cases it generates random
// registration sequences, including Bind aliases, and asserts the documented
// invariant:
//
//	Lifetime and lifecycle hooks belong to a registration. Eagerness belongs
//	to the key: for every key, Start builds whatever serves that key exactly
//	once if any registration for the key was marked Eager, at the position of
//	the first such registration. A group member is its own entry. An alias
//	serves its target, so it builds the target and shares its instance. A
//	binding that serves an eager key may not have a per-scope lifetime, nor
//	may a single registration combine Eager with one.

// The generator starts from a fresh scope each iteration and never touches
// one again after a rejection, so it cannot see defects in freeze's error
// paths; those are covered by regression tests instead.

import (
	"context"
	"errors"
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

// pkI is served either directly or, via Bind, by *pk1.
type pkI interface{ marker() }

func (*pk1) marker() {}

const bindTarget = 0 // Bind[pkI, *pk1] always redirects to key 0

var (
	propKinds  = []string{"provide", "value", "scoped", "transient", "group"}
	ifaceKinds = []string{"provide", "value", "bind", "group"}
	propNames  = []string{"pk1", "pk2", "pk3", "pkI"}
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
		b = s.Provide(func(*di.Scope) T { return mk() }).Group()
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
	case 3:
		if st.kind == "bind" {
			b := s.Bind[pkI, *pk1]()
			if st.eager {
				b.Eager()
			}
			return
		}
		regKind(s, func() pkI { return &pk1{} }, st.kind, st.eager)
	}
}

type propWant struct {
	builds []string
	panics bool
	errs   bool // Start returns an error rather than building
}

// wantEager derives what the container must do with a sequence.
func wantEager(steps []propStep) propWant {
	perScope := func(kind string) bool { return kind == "scoped" || kind == "transient" }

	winner := map[int]string{} // last non-group registration serves the key
	for _, st := range steps {
		if st.kind != "group" {
			winner[st.key] = st.kind
		}
	}
	// effective returns the key whose binding actually gets built.
	effective := func(k int) int {
		if winner[k] == "bind" {
			return bindTarget
		}
		return k
	}

	for _, st := range steps {
		// A registration may not combine Eager with a per-scope lifetime,
		// override or not.
		if st.eager && perScope(st.kind) {
			return propWant{panics: true}
		}
		if !st.eager || st.kind == "group" {
			continue
		}
		// Whatever serves an eager key must be able to honour eagerness,
		// whether it serves directly or through an alias.
		if perScope(winner[st.key]) || perScope(winner[effective(st.key)]) {
			return propWant{panics: true}
		}
	}
	for _, st := range steps {
		// An alias to a target nothing registers cannot be built.
		if st.eager && st.kind != "group" && winner[st.key] == "bind" && winner[bindTarget] == "" {
			return propWant{errs: true}
		}
	}

	var want propWant
	seen := map[string]bool{}
	for i, st := range steps {
		if !st.eager {
			continue
		}
		id, name := fmt.Sprintf("g%d", i), propNames[st.key] // a group member is its own entry
		if st.kind != "group" {
			eff := effective(st.key)
			id, name = fmt.Sprintf("k%d", eff), propNames[eff] // an alias shares its target's instance
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		want.builds = append(want.builds, name)
	}
	return want
}

// runSteps registers the sequence and starts the scope, reporting the builds
// observed and how the container reacted.
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
	for iter := range 6000 {
		steps := make([]propStep, 1+rng.IntN(6))
		for i := range steps {
			key := rng.IntN(len(propNames))
			kinds := propKinds
			if key == 3 {
				kinds = ifaceKinds
			}
			steps[i] = propStep{key: key, kind: kinds[rng.IntN(len(kinds))], eager: rng.IntN(2) == 0}
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
		case want.errs && !errors.Is(err, di.ErrNotProvided):
			fail("expected ErrNotProvided, got %v (builds %v)", err, got)
		case want.errs:
			continue
		case err != nil:
			fail("Start failed: %v", err)
		case !slices.Equal(got, want.builds):
			fail("built %v, want %v", got, want.builds)
		}
	}
}
