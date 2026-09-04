package di_test

// Stand-in services for the regression tests: types whose only job is to be a
// distinct key, and the one helper they share. They live together because
// those tests are grouped by the rule they pin rather than by the shapes they
// need, and no shape belongs to one rule. The letter prefixes are historical,
// from the files this set was assembled from.

import (
	"strings"
	"testing"

	"github.com/floatdrop/di"
)

type (
	rA struct{}
	rB struct{}
	rC struct{}
	rD struct{}

	vA struct{}
	vB struct{}
	vT struct{ n int }

	wA struct{ sc *di.Scope }
	wB struct{ a *wA }
	wT struct{}
	wU struct{ i wI }
	wQ struct{}

	oLate struct{}
	oRoot struct{}

	r3A       struct{}
	r3B       struct{ sc *di.Scope }
	r3C       struct{}
	r3Drainer struct{}
	r3Late    struct{}
	r3Plain   struct{}
	r3Self    struct{}
	r3Server  struct{}
	r3T       struct{}
	r3Worker  struct{}

	altReader struct{}
)

// Two interfaces with the same method set, so each satisfies the other.
type (
	readerA interface{ Read() string }
	readerB interface{ Read() string }

	vI interface{ marker() }
	wI interface{ tag() string }
)

func (*altReader) Read() string { return "alt" }
func (*vT) marker()             {}
func (*wT) tag() string         { return "T" }

// mustPanic runs fn and requires it to panic with a string containing want,
// which is how a configuration rejection reports itself.
func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic containing %q", want)
		}
		if msg, _ := r.(string); !strings.Contains(msg, want) {
			t.Fatalf("panic %v does not contain %q", r, want)
		}
	}()
	fn()
}
