package di_test

// Modules and overrides: composition is plain function composition, and the
// rule that makes it safe is that a second registration of a key within one
// scope has to say so.

import (
	"context"
	"strings"
	"testing"

	"github.com/floatdrop/di"
)

type modDB struct{ tag string }
type modRepo struct{ db *modDB }
type modCache struct{}
type modMarker struct{}

func storageModule(s *di.Scope) {
	s.Provide(func(*di.Scope) *modDB { return &modDB{tag: "storage"} })
	s.Provide(func(sc *di.Scope) *modRepo { return &modRepo{db: sc.Get[*modDB]()} })
}

func cachingModule(s *di.Scope) {
	s.Provide(func(*di.Scope) *modDB { return &modDB{tag: "caching"} }) // collides with storage's
	s.Provide(func(*di.Scope) *modCache { return &modCache{} })
}

// Two modules providing the same key is a collision, reported with both
// modules named. It used to be a silent recapture: storage's *modRepo was wired
// to storage's *modDB and quietly rewired to caching's.
func TestModulesCollideLoudly(t *testing.T) {
	s := di.New()
	s.Use(storageModule, cachingModule)
	mustPanic(t, "is provided at di_test.storageModule", func() { s.Get[*modRepo]() })
	mustPanic(t, "and again at di_test.cachingModule", func() { s.Get[*modRepo]() })
	mustPanic(t, "Override()", func() { s.Get[*modRepo]() })
}

// A module can replace another module's registration by saying so, and the
// replacement is what dependents see.
func TestOverrideReplacesWithinAScope(t *testing.T) {
	s := di.New()
	s.Use(storageModule)
	s.Value(&modDB{tag: "fake"}).Override()
	if got := s.Get[*modRepo]().db.tag; got != "fake" {
		t.Fatalf("Repo sees %q, want the override", got)
	}
}

// An Override with nothing to override is a fake for a service that has been
// renamed or removed, and would otherwise be a registration nobody resolves.
func TestOverrideNeedsSomethingToOverride(t *testing.T) {
	s := di.New()
	s.Use(storageModule)
	s.Value(&modCache{}).Override()
	mustPanic(t, "nothing in scope root provides it", func() { s.Get[*modRepo]() })
}

// A child shadows its parent without Override: that is a different registry,
// not a replacement. Saying Override there is the error, since there is nothing
// in the child to replace.
func TestChildShadowsWithoutOverride(t *testing.T) {
	root := di.New()
	root.Use(storageModule)
	child := root.Child("request")
	child.Value(&modDB{tag: "child"})
	if got := child.Get[*modDB]().tag; got != "child" {
		t.Fatalf("child sees %q", got)
	}
	if got := root.Get[*modDB]().tag; got != "storage" {
		t.Fatalf("parent sees %q, its own registration must stand", got)
	}

	other := root.Child("other")
	other.Value(&modDB{tag: "marked"}).Override()
	mustPanic(t, "a child scope shadows its parent without Override", func() { other.Get[*modDB]() })
}

// Group members accumulate; there is nothing for Override to replace.
func TestOverrideOnAGroupMemberIsRejected(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *modCache { return &modCache{} }).Group().Override()
	mustPanic(t, "does not apply to a group member", func() { s.All[*modCache]() })
}

// A key that has served a value cannot be overridden even with the marker; the
// marker says what the caller means, not that two live values are acceptable.
func TestOverrideAfterResolutionIsStillRejected(t *testing.T) {
	s := di.New()
	s.Use(storageModule)
	s.Get[*modDB]()
	s.Value(&modDB{tag: "late"}).Override()
	mustPanic(t, "it has already been resolved", func() { s.Get[*modDB]() })
}

// Registrations carry the module that made them, directly, from a child the
// module opens, and from a constructor the module registered.
func TestUseAttributesRegistrations(t *testing.T) {
	registering := func(s *di.Scope) {
		s.Provide(func(sc *di.Scope) *modCache {
			sc.Value(modMarker{}) // a constructor registering on its module's behalf
			return &modCache{}
		})
		s.Child("sub").Value(&modDB{tag: "sub"})
	}
	modules := map[string]string{}
	s := di.New()
	s.Observe(func(ev di.Event) {
		if ev.Kind == di.EventBuild {
			modules[ev.Service[strings.LastIndex(ev.Service, ".")+1:]] = ev.Module
		}
	})
	s.Use(storageModule, registering)
	s.Get[*modRepo]()
	s.Get[*modCache]()
	s.Get[modMarker]()

	for service, want := range map[string]string{
		"modDB":     "di_test.storageModule",
		"modRepo":   "di_test.storageModule",
		"modCache":  "di_test.TestUseAttributesRegistrations.func1",
		"modMarker": "di_test.TestUseAttributesRegistrations.func1",
	} {
		if got := modules[service]; got != want {
			t.Errorf("%s attributed to %q, want %q", service, got, want)
		}
	}
	if len(modules) != 4 {
		t.Errorf("saw builds for %v", modules)
	}
}

// A registration made outside any module has no module, and its messages read
// as they always did.
func TestRegistrationsOutsideAModuleAreUnattributed(t *testing.T) {
	var got string
	s := di.New()
	s.Observe(func(ev di.Event) {
		if ev.Kind == di.EventBuild {
			got = ev.Module
		}
	})
	s.Value(&modDB{})
	s.Get[*modDB]()
	if got != "" {
		t.Fatalf("module %q on a registration made directly on the scope", got)
	}
}

// Test applies its wire functions as modules, so the collision report inside a
// test names the production module and the test's own site.
func TestTestAppliesWireFunctionsAsModules(t *testing.T) {
	tb := &fakeTB{}
	s := di.Test(tb, storageModule)
	s.Provide(func(*di.Scope) *modDB { return &modDB{tag: "unmarked"} })
	mustPanic(t, "is provided at di_test.storageModule", func() { s.Get[*modDB]() })
	tb.runCleanups()
}

var _ = context.Background
