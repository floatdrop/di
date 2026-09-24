package di_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"golang.yandex/di"
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
	ctx := t.Context()
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

// A registration marked a group member after a wrapper bound to it is
// rejected as if it had been a member when Wrap looked. (review 7, 1)
func TestWrapOfALaterGroupMemberIsRejected(t *testing.T) {
	s := di.New()
	b := s.Wire[wStore](newWPG)
	s.Wrap[wStore](newWTracing)
	b.Group()
	rejected(t, "does not apply to a group member", func() { _, _ = s.Resolve[wStore]() })
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

// A mark records the owner it was made toward. A Wrap is bound to what it
// wraps when registered, so its build's route runs past a nearer owner that
// marked a scope first, and a walk that stopped at any mark would leave the
// scopes above that nearer owner free to shadow what they handed down. It
// passes on 0f4fe52 too, which marked every scope on every walk: it pins what
// the early stop has to keep.
func TestServedMarksReachTheOwnerAWrapperIsBoundTo(t *testing.T) {
	root := di.New()
	root.Wire[wStore](newWPG)
	z := root.Child("z")
	m := z.Child("m")
	x := m.Child("x")
	w := x.Child("w")
	w.Wrap[wStore](newWTracing) // bound to root's registration
	m.Wire[wStore](newWPG)      // nothing has been served through m yet
	if got := x.Child("s").Get[wStore]().Kind(); got != "pg" {
		t.Fatalf("x's child got %s, want m's", got)
	}
	if got := w.Child("c").Get[wStore]().Kind(); got != "tracing(pg)" {
		t.Fatalf("w's child got %s", got)
	}
	z.Wire[wStore](newWPG)
	rejected(t, "already resolved it from an outer scope", func() { _, _ = z.Resolve[wStore]() })
}

// The same race through a Scoped wrapper: the route from the resolving scope
// to the wrapper is looked up, so a scope on it that registers the key first
// is seen, however far past it the wrapper's own route runs. (review 7, 3)
func TestWrapRouteBeingResolvedSeesARegistrationOnIt(t *testing.T) {
	servedAny := false
	for i := range 1000 {
		root := di.New()
		root.Wire[wStore](newWPG)
		c := root.Child("c")
		c.Wrap[wStore](newWTracing).Scoped()
		x := c.Child("x")
		g := x.Child("g")
		got := raceGetAgainst(func() wStore { return g.Get[wStore]() }, func() {
			x.Wire[wStore](newWPG)
			_, _ = x.Resolve[wStore]()
		})
		if later := served(func() wStore { return g.Get[wStore]() }); got != nil && later != nil && later != got {
			t.Fatalf("iteration %d: g was served %s, then %s", i, got.Kind(), later.Kind())
		}
		servedAny = servedAny || got != nil
	}
	if !servedAny {
		t.Fatal("no iteration served g a value, so none was checked")
	}
}

// A wrapper's route reads the pending registrations of the scopes on it as
// well as their committed ones, so a registration still pending when the
// wrapper is first built is passed over exactly as a committed one is.
// (review 7, 3)
func TestWrapRoutePassesOverAPendingRegistration(t *testing.T) {
	root := di.New()
	root.Wire[wStore](newWPG)
	m := root.Child("m")
	x := m.Child("x")
	w := x.Child("w")
	w.Wrap[wStore](newWTracing) // bound to root's registration
	m.Wire[wStore](newWPG)      // pending until something freezes m
	if got := w.Child("c").Get[wStore]().Kind(); got != "tracing(pg)" {
		t.Fatalf("w's child got %s", got)
	}
	if got := x.Child("s").Get[wStore]().Kind(); got != "pg" {
		t.Fatalf("x's child got %s, want m's", got)
	}
}

// A wrapper's route records itself only when every scope it passed over has
// committed its own registration: a pending one can still become a group
// member, leaving that scope neither registering the key nor marked. It
// passes on e7b5447 too, which recorded no routes: it guards what a record
// promises. (review 7, 3)
func TestWrapRouteRecordsNothingOverAPendingRegistration(t *testing.T) {
	root := di.New()
	root.Provide(func(*di.Scope) *vT { return &vT{n: 1} })
	m := root.Child("m")
	x := m.Child("x")
	w := x.Child("w")
	w.Wrap[*vT](func(v *vT) *vT { return &vT{n: 10 + v.n} })
	pending := m.Provide(func(*di.Scope) *vT { return &vT{n: 2} })
	if got := w.Get[*vT]().n; got != 11 {
		t.Fatalf("the wrapper served n=%d", got)
	}
	pending.Group()
	s := x.Child("s")
	first := s.Get[*vT]()
	m.Provide(func(*di.Scope) *vT { return &vT{n: 3} })
	if later := served(func() *vT { return s.Get[*vT]() }); later != nil && later != first {
		t.Fatalf("s was served n=%d, then n=%d", first.n, later.n)
	}
}

// A wrapper in a direct child of the scope that owns what it wraps marks
// nothing in that owner, which can still wrap or override its own key while
// nothing has been served from it. It passes on e7b5447 too, which marked no
// scope from a wrapper's build: it guards where the route starts.
// (review 7, 3)
func TestWrapInAChildLeavesTheOwnerUnmarked(t *testing.T) {
	root := di.New()
	root.Wire[*vT](func() (*vT, error) { return nil, errors.New("down") })
	c := root.Child("c")
	c.Wrap[*vT](func(v *vT) *vT { return v })
	if _, err := c.Resolve[*vT](); err == nil {
		t.Fatal("a failing constructor was served")
	}
	root.Wrap[*vT](func(v *vT) *vT { return v })
	func() {
		defer func() {
			if msg, _ := recover().(string); strings.Contains(msg, "outer scope") {
				t.Fatalf("the owner refused its own key: %s", msg)
			}
		}()
		_, _ = root.Resolve[*vT]()
	}()
}

// A wrapper's route ends at a scope above the wrapper's whose own route to
// the wrapped owner is recorded, and every scope below that is marked.
func TestWrapRouteEndsAtARecordedScope(t *testing.T) {
	root := di.New()
	root.Wire[wStore](newWPG)
	mid := root.Child("mid")
	_ = mid.Child("y").Get[wStore]() // records mid's route to root
	x := mid.Child("x")
	w := x.Child("w")
	w.Wrap[wStore](newWTracing)
	if got := w.Get[wStore]().Kind(); got != "tracing(pg)" {
		t.Fatalf("got %s", got)
	}
	for _, sc := range []*di.Scope{x, mid} {
		sc.Wire[wStore](newWPG)
		rejected(t, "already resolved it from an outer scope", func() { _, _ = sc.Resolve[wStore]() })
	}
}

// A wrapper in a middle scope's child is bound to the root's registration and
// passes over the middle scope's own registration of the key, which leaves
// that scope free to override its own. v0.17.2 marked every scope on the
// wrapper's route and refused it; e2d70af passes over a scope that registers
// the key. (review 7, 4)
func TestAMiddleScopePassedOverByAWrapperCanOverrideItsOwn(t *testing.T) {
	root := di.New()
	root.Wire[wStore](newWPG)
	mid := root.Child("mid")
	w := mid.Child("w")
	w.Wrap[wStore](newWTracing) // bound to the root's registration
	mid.Wire[wStore](newWPG)
	_, _ = mid.Resolve[*wCache]() // commits mid's batch without resolving the key
	if got := w.Child("c").Get[wStore]().Kind(); got != "tracing(pg)" {
		t.Fatalf("the wrapper served %s", got)
	}
	mid.Wire[wStore](func() wStore { return &wCaching{} }).Override()
	if _, err := mid.Resolve[wStore](); err != nil {
		t.Fatalf("mid could not override its own registration: %v", err)
	}
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

// A child's wrapper guards the parent registration it composes over only
// while the child is alive: a stopped scope never serves the key again, so
// the parent may replace the registration. (review 6, 2)
func TestWrapInAStoppedChildNoLongerPinsTheParent(t *testing.T) {
	root := di.New()
	root.Wire[wStore](newWPG)
	child := root.Child("request")
	child.Wrap[wStore](newWTracing)
	if err := child.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	root.Wire[wStore](func() wStore { return &wPG{} }).Override()
	if got := root.Get[wStore]().Kind(); got != "pg" {
		t.Fatalf("got %s", got)
	}
}

// The guard belongs to each wrapper, not to the registration: one child
// stopping must not release a registration a live sibling still wraps.
// (review 6, 2)
func TestWrapInALiveSiblingStillPinsTheParent(t *testing.T) {
	root := di.New()
	root.Wire[wStore](newWPG)
	a := root.Child("a")
	a.Wrap[wStore](newWTracing)
	b := root.Child("b")
	b.Wrap[wStore](newWTracing)
	if err := b.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	root.Wire[wStore](func() wStore { return &wPG{} }).Override()
	rejected(t, "it is wrapped at", func() { _, _ = root.Resolve[wStore]() })
}

// Wrap and Stop on one scope at once: whichever goes first, the stopped
// scope's wrapper must not go on guarding the parent. Run with -race.
// (review 6, 2)
func TestWrapRacingStopLeavesNoMark(t *testing.T) {
	for i := range 200 {
		root := di.New()
		root.Wire[wStore](newWPG)
		child := root.Child("child")
		var wg sync.WaitGroup
		wg.Go(func() { child.Wrap[wStore](newWTracing) })
		wg.Go(func() { _ = child.Stop(t.Context()) })
		wg.Wait()
		root.Wire[wStore](func() wStore { return &wPG{} }).Override()
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("iteration %d: %v", i, r)
				}
			}()
			if _, err := root.Resolve[wStore](); err != nil {
				t.Fatalf("iteration %d: %v", i, err)
			}
		}()
	}
}

// An Override that replaces a wrapper in its own scope replaces the chain, so
// the replaced wrapper will never serve and stops guarding what it wrapped.
// (review 6, 2)
func TestOverriddenWrapperNoLongerPinsTheParent(t *testing.T) {
	root := di.New()
	root.Wire[wStore](newWPG)
	child := root.Child("child")
	child.Wrap[wStore](newWTracing)
	child.Wire[wStore](func() wStore { return &wPG{} }).Override()
	if got := child.Get[wStore]().Kind(); got != "pg" {
		t.Fatalf("child got %s", got)
	}
	root.Wire[wStore](func() wStore { return &wPG{} }).Override()
	if got := root.Get[wStore]().Kind(); got != "pg" {
		t.Fatalf("root got %s", got)
	}
}

// A chain an Override replaces lets go of what it wraps only as far as
// nothing live still composes over it. A grandchild wraps the first of two
// wrappers in its parent, and the parent overrides the chain: the first
// wrapper still serves the grandchild, so the root's registration stays
// guarded until the grandchild stops. (review 6, 2)
func TestOverriddenChainStillPinnedByADescendant(t *testing.T) {
	root := di.New()
	root.Wire[wStore](newWPG)
	mid := root.Child("mid")
	mid.Wrap[wStore](newWTracing)
	leaf := mid.Child("leaf")
	leaf.Wrap[wStore](newWTracing)
	mid.Wrap[wStore](newWTracing)
	mid.Wire[wStore](func() wStore { return &wPG{} }).Override()
	if got := mid.Get[wStore]().Kind(); got != "pg" {
		t.Fatalf("mid got %s", got)
	}

	root.Wire[wStore](func() wStore { return &wPG{} }).Override()
	rejected(t, "it is wrapped at", func() { _, _ = root.Resolve[wStore]() })

	if err := leaf.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := root.Get[wStore]().Kind(); got != "pg" {
		t.Fatalf("root got %s once the last wrapper over the chain stopped", got)
	}
}
