package di_test

// Stand-in services for the regression tests, types whose only job is to be
// a distinct key, and the one helper they share. The tests are grouped by the
// rule they pin rather than by the shapes they need, so the shapes live here.
//
// Those files hold one test per defect, named for the rule it pins, with a
// tag at the end of its comment saying where the defect came from. (review 1,
// 3) is the third defect of the first September 2026 review, checked against
// 12dba3c; review 2 was checked against 2b8915d and review 3 against 9ace680.
// (pass 4) is the fourth of the seven narrower passes that preceded those
// reviews, each checked against the code before the instance-phase refactor.
// (fx review) is the comparison with uber/fx, checked against 3728d76.
// An untagged test comes from the first of those passes, or from the
// generators, which its own comment says. Several fail by hanging rather than
// by reporting, which is why each bounds its own wait instead of relying on
// the package timeout.

import (
	"strings"
	"testing"

	"golang.yandex/di"
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
	r3Worker  struct{}
)

type vI interface{ marker() }

func (*vT) marker() {}

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
