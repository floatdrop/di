package di

import (
	"reflect"
	"strings"
	"testing"
)

// wrapBy registers in s a wrapper over inner, found in at, as Wrap does once
// it has looked inner up, and returns the panic register raised, if any.
func wrapBy(s *Scope, inner *binding, at *state) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			var ok bool
			if msg, ok = r.(string); !ok {
				panic(r)
			}
		}
	}()
	s.register(inner.key, func(*Scope) any { return &graph{} }, func(b *binding) {
		b.inner, b.innerAt = inner, at
	})
	return ""
}

// A Wrap whose target is overridden between its lookup and its mark is
// rejected before it is queued, and holds no mark, whether the target was a
// registration or a wrapper chain the Override retired, and whether it lives
// in an ancestor or in the Wrap's own scope. Wrap's two steps are taken by
// hand to put the Override between them. (#52)
func TestWrapLosingARaceToAnOverrideIsRejected(t *testing.T) {
	k := key{t: reflect.TypeFor[*graph]()}
	for _, shape := range []string{"registration", "wrapper chain", "own scope"} {
		t.Run(shape, func(t *testing.T) {
			root := New()
			root.Provide(func(*Scope) *graph { return &graph{} })
			if shape == "wrapper chain" {
				root.Wrap[*graph](func(g *graph) *graph { return g })
			}
			wrapping := root.Child("child")
			if shape == "own scope" {
				wrapping = root
			}
			inner, at := root.st.lookup(k) // Wrap's lookup
			override := root.Provide(func(*Scope) *graph { return &graph{} }).Override()
			if shape != "own scope" {
				root.st.freeze() // committed before the mark
			}
			msg := wrapBy(wrapping, inner, at)
			if !strings.Contains(msg, "while the Wrap was being registered") || !strings.Contains(msg, override.b.site) {
				t.Fatalf("got %q, want a rejection naming the Override", msg)
			}
			for x := inner; x != nil; x = x.inner {
				if x.wrapper() != nil {
					t.Fatalf("%s kept a mark after the rejection", x.where())
				}
			}
			if _, err := wrapping.Resolve[*graph](); err != nil {
				t.Fatalf("the override serves: %v", err)
			}
		})
	}
}

// A Wrap whose target another Wrap has wrapped since the lookup still finds
// it serving the key, through that chain, and is queued. (#52)
func TestWrapOverATargetWrappedSinceIsQueued(t *testing.T) {
	k := key{t: reflect.TypeFor[*graph]()}
	root := New()
	root.Provide(func(*Scope) *graph { return &graph{} })
	child := root.Child("child")
	inner, at := root.st.lookup(k)
	root.Wrap[*graph](func(g *graph) *graph { return g })
	root.st.freeze()
	if msg := wrapBy(child, inner, at); msg != "" {
		t.Fatalf("rejected: %s", msg)
	}
}

// A Wrap of an ancestor's registration is rejected when its own scope has
// registered the key since the lookup, rather than displacing that
// registration unseen. (#52)
func TestWrapRejectsARegistrationLandingInItsOwnScope(t *testing.T) {
	k := key{t: reflect.TypeFor[*graph]()}
	root := New()
	root.Provide(func(*Scope) *graph { return &graph{} })
	child := root.Child("child")
	inner, at := root.st.lookup(k)
	own := child.Provide(func(*Scope) *graph { return &graph{} })
	if msg := wrapBy(child, inner, at); !strings.Contains(msg, own.b.site) {
		t.Fatalf("got %q, want a rejection naming the child's registration", msg)
	}
	if inner.wrapper() != nil {
		t.Fatal("the rejected wrapper kept its mark on the root's registration")
	}
}

// A Wrap does not lose to an ancestor's Override that is still pending: that
// Override is the one to be refused, as it sees the Wrap. (#52)
func TestWrapIgnoresAPendingOverrideInAnAncestor(t *testing.T) {
	k := key{t: reflect.TypeFor[*graph]()}
	root := New()
	root.Provide(func(*Scope) *graph { return &graph{} })
	child := root.Child("child")
	inner, at := root.st.lookup(k)
	root.Provide(func(*Scope) *graph { return &graph{} }).Override() // pending
	if msg := wrapBy(child, inner, at); msg != "" {
		t.Fatalf("rejected: %s", msg)
	}
	func() {
		defer func() {
			if msg, _ := recover().(string); !strings.Contains(msg, "it is wrapped at") {
				t.Fatalf("got %q, want the Override refused for the wrapper", msg)
			}
		}()
		root.st.freeze()
	}()
}

// Two Wraps racing in one scope over the same target cannot both be queued
// over it: the second would drop the first from the chain unseen. The second
// is rejected naming the first, whether the target is in that scope or an
// ancestor's. (#52)
func TestWrapRacingAWrapInItsScopeIsRejected(t *testing.T) {
	k := key{t: reflect.TypeFor[*graph]()}
	for _, shape := range []string{"own scope", "ancestor"} {
		t.Run(shape, func(t *testing.T) {
			root := New()
			root.Provide(func(*Scope) *graph { return &graph{} })
			wrapping := root
			if shape == "ancestor" {
				wrapping = root.Child("child")
			}
			inner, at := root.st.lookup(k) // both Wraps looked up the same target
			if msg := wrapBy(wrapping, inner, at); msg != "" {
				t.Fatalf("the first Wrap was rejected: %s", msg)
			}
			first := wrapping.st.pending[len(wrapping.st.pending)-1]
			if msg := wrapBy(wrapping, inner, at); !strings.Contains(msg, first.site) {
				t.Fatalf("got %q, want the second Wrap rejected naming the first", msg)
			}
		})
	}
}
