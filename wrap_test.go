package di_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/floatdrop/di"
)

// Fixtures for Wrap: a Store interface, one implementation, and wrappers
// that keep a pointer to what they wrap.

type wStore interface{ Kind() string }
type wPG struct{ _ byte } // not zero-size: pointers to distinct zero-size values may compare equal
type wCaching struct {
	next  wStore
	cache *wCache
}
type wTracing struct{ next wStore }
type wCache struct{}

func (*wPG) Kind() string        { return "pg" }
func (c *wCaching) Kind() string { return "caching(" + c.next.Kind() + ")" }
func (t *wTracing) Kind() string { return "tracing(" + t.next.Kind() + ")" }

func newWPG() wStore                            { return &wPG{} }
func newWCaching(next wStore, c *wCache) wStore { return &wCaching{next, c} }
func newWTracing(next wStore) wStore            { return &wTracing{next} }
func newWFailing(wStore) (wStore, error)        { return nil, errors.New("boom") }

func TestWrapComposesOverTheRegistration(t *testing.T) {
	s := di.New()
	s.Value(&wCache{})
	s.Wire[wStore](newWPG)
	s.Wrap[wStore](newWCaching)
	if got := s.Get[wStore]().Kind(); got != "caching(pg)" {
		t.Fatalf("got %s", got)
	}
	if s.Get[wStore]() != s.Get[wStore]() {
		t.Fatal("the wrapper is not a singleton")
	}
}

func TestWrapChainsInRegistrationOrder(t *testing.T) {
	s := di.New()
	s.Value(&wCache{})
	s.Wire[wStore](newWPG)
	s.Wrap[wStore](newWCaching)
	s.Wrap[wStore](newWTracing)
	if got := s.Get[wStore]().Kind(); got != "tracing(caching(pg))" {
		t.Fatalf("got %s", got)
	}
}

// The wrapped registration keeps its hooks and is built first, so it is
// started before the wrapper and stopped after it.
func TestWrapKeepsTheWrappedLifecycle(t *testing.T) {
	var log []string
	note := func(what string) func(context.Context, wStore) error {
		return func(context.Context, wStore) error { log = append(log, what); return nil }
	}
	s := di.New()
	s.Wire[wStore](newWPG).OnStart(note("start pg")).OnStop(note("stop pg"))
	s.Wrap[wStore](newWTracing).Eager().OnStart(note("start tracing")).OnStop(note("stop tracing"))
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	want := "start pg,start tracing,stop tracing,stop pg"
	if got := strings.Join(log, ","); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// A wrapper in a child scope wraps the parent's value for that child and
// its descendants only: the parent and its other children see the original,
// and the wrapped value is the parent's one singleton.
func TestWrapInAChildScopeIsLocalToIt(t *testing.T) {
	root := di.New()
	root.Wire[wStore](newWPG)
	traced := root.Child("traced")
	traced.Wrap[wStore](newWTracing)
	plain := root.Child("plain")

	if got := traced.Get[wStore]().Kind(); got != "tracing(pg)" {
		t.Fatalf("child got %s", got)
	}
	if got := root.Get[wStore]().Kind(); got != "pg" {
		t.Fatalf("root got %s", got)
	}
	if got := plain.Get[wStore]().Kind(); got != "pg" {
		t.Fatalf("sibling got %s", got)
	}
	if traced.Get[wStore]().(*wTracing).next != root.Get[wStore]() {
		t.Fatal("the wrapper should wrap the parent's own instance")
	}
	grandchild := traced.Child("gc")
	if got := grandchild.Get[wStore]().Kind(); got != "tracing(pg)" {
		t.Fatalf("grandchild got %s", got)
	}
}

// A wrapper takes the lifetime of what it wraps, and Scoped() on the wrapper
// puts one wrapper per resolving scope around a shared inner value.
func TestWrapTakesTheLifetimeOfWhatItWraps(t *testing.T) {
	root := di.New()
	root.Wire[wStore](newWPG).Scoped()
	root.Wrap[wStore](newWTracing) // scoped, because the store is
	a, b := root.Child("a"), root.Child("b")
	wa, wb := a.Get[wStore]().(*wTracing), b.Get[wStore]().(*wTracing)
	if wa == wb || wa.next == wb.next {
		t.Fatal("a scoped store must be wrapped once per scope")
	}

	shared := di.New()
	shared.Wire[wStore](newWPG)
	shared.Wrap[wStore](newWTracing).Scoped()
	c, d := shared.Child("c"), shared.Child("d")
	wc, wd := c.Get[wStore]().(*wTracing), d.Get[wStore]().(*wTracing)
	if wc == wd || wc.next != wd.next {
		t.Fatal("a Scoped wrapper is one per scope around the one singleton")
	}
}

func TestWrapConstructorErrorAbortsTheBuild(t *testing.T) {
	s := di.New()
	s.Wire[wStore](newWPG)
	s.Wrap[wStore](newWFailing)
	if _, err := s.Resolve[wStore](); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want the wrapper's error, got %v", err)
	}
}

func TestWrapDependencyOnItsOwnKeyIsACycle(t *testing.T) {
	s := di.New()
	s.Wire[wStore](newWPG)
	s.Wrap[wStore](func(next wStore, again wStore) wStore { return next })
	if _, err := s.Resolve[wStore](); !errors.Is(err, di.ErrCycle) {
		t.Fatalf("want ErrCycle, got %v", err)
	}
	if v := s.Validate(); !errors.Is(v.Err(), di.ErrCycle) {
		t.Fatalf("Validate should see the same cycle, got %v", v.Err())
	}
}

func rejected(t *testing.T, want string, f func()) {
	t.Helper()
	defer func() {
		msg, ok := recover().(string)
		if !ok || !strings.HasPrefix(msg, "di: ") || !strings.Contains(msg, want) {
			t.Fatalf("want a di: rejection mentioning %q, got %v", want, msg)
		}
	}()
	f()
	t.Fatal("accepted")
}

func TestWrapNothingToWrapIsRejectedAtRegistration(t *testing.T) {
	rejected(t, "nothing provides", func() { di.New().Wrap[wStore](newWTracing) })
	// A group does not serve the key.
	s := di.New()
	s.Wire[wStore](newWPG).Group()
	rejected(t, "cannot be wrapped", func() { s.Wrap[wStore](newWTracing) })
}

func TestWrapRejectsBadShapes(t *testing.T) {
	s := di.New()
	s.Wire[wStore](newWPG)
	cases := map[string]func(){
		"not a function":    func() { s.Wrap[wStore](42) },
		"variadic":          func() { s.Wrap[wStore](func(wStore, ...int) wStore { return nil }) },
		"no parameters":     func() { s.Wrap[wStore](func() wStore { return nil }) },
		"wrong first param": func() { s.Wrap[wStore](func(*wCache) wStore { return nil }) },
		"wrong result":      func() { s.Wrap[wStore](func(wStore) *wCache { return nil }) },
		"second not error":  func() { s.Wrap[wStore](func(wStore) (wStore, bool) { return nil, false }) },
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) { rejected(t, "Wrap[", f) })
	}
}

func TestWrapAfterResolutionIsRejected(t *testing.T) {
	s := di.New()
	s.Wire[wStore](newWPG)
	_ = s.Get[wStore]()
	s.Wrap[wStore](newWTracing) // registers; the rejection lands at the next resolution
	rejected(t, "cannot be wrapped", func() { _, _ = s.Resolve[wStore]() })
}

func TestOverrideAfterWrapReplacesTheChain(t *testing.T) {
	s := di.New()
	s.Wire[wStore](newWPG)
	s.Wrap[wStore](newWTracing)
	s.Wire[wStore](func() wStore { return &wPG{} }).Override()
	if got := s.Get[wStore]().Kind(); got != "pg" {
		t.Fatalf("the override should replace the wrapper too, got %s", got)
	}
}

// A parent registration a child has wrapped cannot be overridden: the
// wrapper would go on serving a value built from a registration nothing else
// can reach.
func TestOverrideOfAWrappedRegistrationIsRejected(t *testing.T) {
	root := di.New()
	root.Wire[wStore](newWPG)
	child := root.Child("child")
	child.Wrap[wStore](newWTracing)
	root.Wire[wStore](func() wStore { return &wPG{} }).Override()
	rejected(t, "it is wrapped at", func() { _, _ = root.Resolve[wStore]() })
}

func TestWrapRejectsGroupAndOverrideMarkers(t *testing.T) {
	s := di.New()
	s.Wire[wStore](newWPG)
	s.Wrap[wStore](newWTracing).Group()
	rejected(t, "does not apply to a wrapper", func() { _, _ = s.Resolve[wStore]() })

	o := di.New()
	o.Wire[wStore](newWPG)
	o.Wrap[wStore](newWTracing).Override()
	rejected(t, "does not apply to a wrapper", func() { _, _ = o.Resolve[wStore]() })
}

func TestWrapWithAValueAndAScopeThatServedTheKey(t *testing.T) {
	s := di.New()
	s.Value(wStore(&wPG{}))
	s.Wrap[wStore](newWTracing)
	if got := s.Get[wStore]().Kind(); got != "tracing(pg)" {
		t.Fatalf("a Value wraps too, got %s", got)
	}

	root := di.New()
	root.Wire[wStore](newWPG)
	mid := root.Child("mid")
	leaf := mid.Child("leaf")
	_ = leaf.Get[wStore]() // mid handed the key down
	mid.Wrap[wStore](newWTracing)
	rejected(t, "already resolved it from an outer scope", func() { _, _ = mid.Resolve[wStore]() })
}

// Explain names a wrapper as one, and draws what it wraps beneath it, built
// or declared.
func TestExplainShowsTheWrappedChain(t *testing.T) {
	s := di.New()
	s.Value(&wCache{})
	s.Wire[wStore](newWPG)
	s.Wrap[wStore](newWCaching)
	wantExplain(t, s.Explain[wStore](), `wStore: singleton wrapper in root, not built
├╌╌ wStore: singleton in root, not built
└╌╌ *wCache: value in root, not built
`)
	_ = s.Get[wStore]()
	wantExplain(t, s.Explain[wStore](), `wStore: singleton wrapper in root, built
├── wStore: singleton in root, built
└── *wCache: value in root, built
`)
}

// Validate walks the chain: what a wrapper wraps has its own turn, and the
// wrapper's other dependencies are checked like any constructor's.
func TestValidateChecksAWrappedChain(t *testing.T) {
	s := di.New()
	s.Wire[wStore](func(*wCache) wStore { return &wPG{} }) // *wCache missing
	s.Wrap[wStore](newWTracing)
	v := s.Validate()
	if len(v.Errors) != 1 || !errors.Is(v.Err(), di.ErrNotProvided) || !strings.Contains(v.Err().Error(), "wCache") {
		t.Fatalf("the wrapped registration's missing dependency should be reported once, got %v", v.Errors)
	}
	s2 := di.New()
	s2.Wire[wStore](newWPG)
	s2.Wrap[wStore](newWCaching) // *wCache missing for the wrapper itself
	if v := s2.Validate(); len(v.Errors) != 1 || !strings.Contains(v.Err().Error(), "wCache") {
		t.Fatalf("the wrapper's missing dependency should be reported, got %v", v.Errors)
	}
}
