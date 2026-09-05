package di_test

// Regressions in registration and lookup: aliases, groups, shadowing, the
// eager set, and the rejections freeze is responsible for.
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
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/dihttp"
)

// Bind must serve the target's own instance, keeping the target's lifetime.
func TestRegressionBindKeepsTargetLifetime(t *testing.T) {
	s := di.New()
	var builds atomic.Int32
	s.Provide(func(*di.Scope) *Repo { builds.Add(1); return &Repo{} }).Transient()
	s.Bind[Reader, *Repo]()
	a, b := s.Get[Reader](), s.Get[Reader]()
	if a == b || builds.Load() != 2 {
		t.Fatalf("alias must not cache a transient target: builds=%d", builds.Load())
	}

	// A Scoped target stays per resolving scope through the alias.
	s2 := di.New()
	s2.Provide(func(sc *di.Scope) *Repo { return &Repo{} }).Scoped()
	s2.Bind[Reader, *Repo]()
	c1, c2 := s2.Child("one"), s2.Child("two")
	if c1.Get[Reader]() == c2.Get[Reader]() {
		t.Fatal("alias to a Scoped target must not collapse to one instance")
	}
	if c1.Get[Reader]() != any(c1.Get[*Repo]()) {
		t.Fatal("alias and target must share one instance within a scope")
	}
}

// Eager on a group member builds it at Start.
func TestRegressionEagerGroupMember(t *testing.T) {
	var builds, starts atomic.Int32
	s := di.New()
	s.Provide(func(*di.Scope) Handler { builds.Add(1); return Handler{} }).Group().Eager().
		OnStart(func(context.Context, Handler) error { starts.Add(1); return nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if builds.Load() != 1 || starts.Load() != 1 {
		t.Fatalf("builds=%d starts=%d, want 1 and 1", builds.Load(), starts.Load())
	}
	if got := s.All[Handler](); len(got) != 1 {
		t.Fatalf("All returned %d members", len(got))
	}
	if builds.Load() != 1 {
		t.Fatal("All rebuilt the eager member")
	}
}

// Combinations that cannot be honoured are rejected, in either call order.
func TestRegressionInvalidCombinations(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		wire func(*di.Scope)
	}{
		{"transient with hooks", "do not apply to a Transient binding", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *rA { return &rA{} }).Transient().OnStart(func(context.Context, *rA) error { return nil })
		}},
		{"hooks then transient", "do not apply to a Transient binding", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *rA { return &rA{} }).OnStop(func(context.Context, *rA) error { return nil }).Transient()
		}},
		{"eager transient", "does not apply to a Transient binding", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *rA { return &rA{} }).Transient().Eager()
		}},
		{"eager scoped", "does not apply to a Scoped binding", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *rA { return &rA{} }).Scoped().Eager()
		}},
		{"transient and scoped", "mutually exclusive", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *rA { return &rA{} }).Scoped().Transient()
		}},
		{"hooks on an alias", "belong on the target binding", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *Repo { return &Repo{} })
			s.Bind[Reader, *Repo]().OnStop(func(context.Context, Reader) error { return nil })
		}},
		{"group on an alias", "does not apply to a Bind alias", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *Repo { return &Repo{} })
			s.Bind[Reader, *Repo]().Group()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := di.New()
			tc.wire(s)
			mustPanic(t, tc.want, func() { _ = s.Start(context.Background()) })
		})
	}
}

// Eager services build in registration order, not map order.
func TestRegressionEagerOrderIsDeterministic(t *testing.T) {
	for range 50 {
		var log []string
		s := di.New()
		s.Provide(func(*di.Scope) *rA { log = append(log, "A"); return &rA{} }).Eager()
		s.Provide(func(*di.Scope) *rB { log = append(log, "B"); return &rB{} }).Eager()
		s.Provide(func(*di.Scope) *rC { log = append(log, "C"); return &rC{} }).Eager()
		s.Provide(func(*di.Scope) *rD { log = append(log, "D"); return &rD{} }).Eager()
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(log, ""); got != "ABCD" {
			t.Fatalf("build order %q, want ABCD", got)
		}
	}
}

// A key cannot be re-registered once it has been resolved.
func TestRegressionOverrideAfterResolveRejected(t *testing.T) {
	s := di.New()
	s.Value(&DB{dsn: "a"})
	s.Get[*DB]()
	s.Value(&DB{dsn: "b"})
	mustPanic(t, "cannot be overridden", func() { s.Get[*DB]() })

	// Overriding before the key is resolved stays legal: that is how tests
	// and child scopes substitute dependencies.
	ok := di.New()
	ok.Value(&DB{dsn: "a"})
	ok.Value(&DB{dsn: "b"})
	if got := ok.Get[*DB]().dsn; got != "b" {
		t.Fatalf("override before resolution must win, got %q", got)
	}
}

// A transient is built in the scope that resolves it, like Scoped.
func TestRegressionTransientBuildsInResolvingScope(t *testing.T) {
	app := di.New()
	app.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} }).Transient()
	req := app.Child("request")
	req.Value(&DB{dsn: "request-scoped"})
	got, err := req.Resolve[*Repo]()
	if err != nil {
		t.Fatal(err)
	}
	if got.db.dsn != "request-scoped" {
		t.Fatalf("transient saw %q", got.db.dsn)
	}
}

// An eager binding that a later registration overrode must not be built.
// (pass 2)
func TestRegressionShadowedEagerNotBuilt(t *testing.T) {
	var log []string
	s := di.New()
	s.Provide(func(*di.Scope) *DB { log = append(log, "real"); return &DB{dsn: "real"} }).Eager().
		OnStart(func(context.Context, *DB) error { log = append(log, "startReal"); return nil })
	s.Provide(func(*di.Scope) *DB { log = append(log, "fake"); return &DB{dsn: "fake"} }).Eager().
		OnStart(func(context.Context, *DB) error { log = append(log, "startFake"); return nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); got != "fake,startFake" {
		t.Fatalf("got %q, want only the winning registration built", got)
	}
	if got := s.Get[*DB]().dsn; got != "fake" {
		t.Fatalf("Get returned %q", got)
	}
}

// Overriding an Eager binding keeps the key eager: the replacement is
// built at Start, which is what the test seam relies on.
// (pass 3)
func TestRegressionOverrideKeepsKeyEager(t *testing.T) {
	var log []string
	s := di.New()
	s.Provide(func(*di.Scope) *DB { log = append(log, "real"); return &DB{dsn: "real"} }).Eager().
		OnStart(func(context.Context, *DB) error { log = append(log, "startReal"); return nil })
	s.Value(&DB{dsn: "fake"}).
		OnStart(func(context.Context, *DB) error { log = append(log, "startFake"); return nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); got != "startFake" {
		t.Fatalf("got %q, want the replacement started and the original never built", got)
	}
	if got := s.Get[*DB]().dsn; got != "fake" {
		t.Fatalf("Get returned %q", got)
	}
}

// Eagerness transfers to whichever binding owns the key, so a per-scope
// winner must be rejected rather than built once in the declaring scope.
// (pass 4)
func TestRegressionEagerCannotTransferToPerScopeLifetime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(di.Binding[*DB]) di.Binding[*DB]
	}{
		{"Scoped", func(b di.Binding[*DB]) di.Binding[*DB] { return b.Scoped() }},
		{"Transient", func(b di.Binding[*DB]) di.Binding[*DB] { return b.Transient() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			built := false
			s := di.New()
			s.Provide(func(*di.Scope) *DB { return &DB{dsn: "real"} }).Eager()
			tc.apply(s.Provide(func(*di.Scope) *DB { built = true; return &DB{dsn: "fake"} }))
			mustPanic(t, "eagerness cannot transfer", func() { _ = s.Start(context.Background()) })
			if built {
				t.Fatalf("a %s binding was built at Start", tc.name)
			}
		})
	}
}

// A Bind alias may end up owning an eager key. Start must build the
// target it redirects to, not the alias's own absent constructor.
// (pass 5)
func TestRegressionEagerAliasBuildsTarget(t *testing.T) {
	var builds int
	s := di.New()
	s.Provide(func(*di.Scope) Reader { return &Repo{db: &DB{dsn: "direct"}} }).Eager()
	s.Provide(func(*di.Scope) *Repo { builds++; return &Repo{db: &DB{dsn: "target"}} })
	s.Bind[Reader, *Repo]() // now owns the Reader key, and inherits its eagerness

	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if builds != 1 {
		t.Fatalf("target built %d times, want 1", builds)
	}
	if got := s.Get[Reader]().Read(); got != "target" {
		t.Fatalf("Reader resolved to %q", got)
	}
	if any(s.Get[Reader]()) != any(s.Get[*Repo]()) {
		t.Fatal("alias and target must share one instance")
	}
}

// Maybe must report absent for an alias whose target is missing, rather
// than reporting present and then failing inside Get.
// (alias refactor)
func TestRegressionMaybeFollowsAliases(t *testing.T) {
	s := di.New()
	s.Bind[Reader, *Repo]() // target never registered
	if _, ok := s.Maybe[Reader](); ok {
		t.Fatal("Maybe reported an alias to a missing target as present")
	}
	s.Provide(func(*di.Scope) *Repo { return &Repo{db: &DB{dsn: "x"}} })
	if v, ok := s.Maybe[Reader](); !ok || v.Read() != "x" {
		t.Fatalf("Maybe = %v, %v once the target exists", v, ok)
	}
}

// Bind's first type parameter must be an interface, and saying so
// beats surfacing a raw reflect panic.
// (alias refactor)
func TestRegressionBindRejectsNonInterface(t *testing.T) {
	mustPanic(t, "must be an interface", func() { di.New().Bind[*Repo, *Repo]() })
}

// An eager key served through an alias to a per-scope target cannot
// honour eagerness, exactly as a direct per-scope winner cannot.
// (alias refactor)
func TestRegressionEagerAliasToPerScopeTargetRejected(t *testing.T) {
	built := false
	s := di.New()
	s.Provide(func(*di.Scope) Reader { return &Repo{db: &DB{}} }).Eager()
	s.Provide(func(*di.Scope) *Repo { built = true; return &Repo{db: &DB{}} }).Scoped()
	s.Bind[Reader, *Repo]()
	mustPanic(t, "eagerness cannot transfer", func() { _ = s.Start(context.Background()) })
	if built {
		t.Fatal("the scoped target was built at Start")
	}
}

// Eagerness is a property of the key, so an alias may declare it: the
// target is built at Start.
// (alias refactor)
func TestRegressionEagerDeclaredOnAlias(t *testing.T) {
	var builds int
	s := di.New()
	s.Provide(func(*di.Scope) *Repo { builds++; return &Repo{db: &DB{dsn: "t"}} })
	s.Bind[Reader, *Repo]().Eager()
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if builds != 1 {
		t.Fatalf("target built %d times, want 1", builds)
	}
	if got := s.Get[Reader]().Read(); got != "t" {
		t.Fatalf("got %q", got)
	}
}

// A rejected registration must be rejected every time. freeze used to
// clear pending before deriving the eager set, so a panic there left the
// batch consumed and a retried Start silently succeeded with the invalid
// configuration dropped.
// (pass 6)
func TestRegressionRejectionIsRepeatable(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} }).Eager()
	s.Provide(func(*di.Scope) *Repo { return &Repo{} }).Eager()
	s.Provide(func(*di.Scope) *Repo { return &Repo{} }).Scoped() // eagerness cannot transfer

	for attempt := range 3 {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("attempt %d: Start was accepted, the invalid config was dropped", attempt)
				}
			}()
			_ = s.Start(context.Background())
		}()
	}
}

// A rejected batch must leave the scope as it was, so the rejection is
// the same on every subsequent operation rather than a half-applied registry.
// (pass 6)
func TestRegressionRejectedBatchIsNotHalfApplied(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} })
	s.Provide(func(*di.Scope) *Repo { return &Repo{} }).Scoped().Transient() // invalid

	var msgs []string
	for range 3 {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					msgs = append(msgs, "accepted")
					return
				}
				msgs = append(msgs, r.(string))
			}()
			_, _ = s.Resolve[*DB]()
		}()
	}
	for i, m := range msgs {
		if !strings.Contains(m, "mutually exclusive") {
			t.Fatalf("attempt %d: %s", i, m)
		}
	}
	if msgs[0] != msgs[1] || msgs[1] != msgs[2] {
		t.Fatalf("rejection was not identical across attempts: %v", msgs)
	}
}

// An alias key counts as resolved once something has been served through
// it, so it cannot be re-registered either.
// (pass 6)
func TestRegressionAliasKeyCannotBeRebound(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *Repo { return &Repo{db: &DB{dsn: "first"}} })
	s.Bind[Reader, *Repo]()
	if got := s.Get[Reader]().Read(); got != "first" {
		t.Fatalf("got %q", got)
	}
	s.Provide(func(*di.Scope) Reader { return &Repo{db: &DB{dsn: "second"}} })
	mustPanic(t, "cannot be overridden", func() { _ = s.Get[Reader]() })
}

// A key whose constructor failed built nothing, so it can be
// re-registered. Because a failed instance caches its error for good, this
// is the only way to recover such a key.
// (pass 7)
func TestRegressionFailedResolveLeavesKeyReRegisterable(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *DB { panic("boom") })
	if _, err := s.Resolve[*DB](); err == nil {
		t.Fatal("expected the constructor panic to surface")
	}
	s.Provide(func(*di.Scope) *DB { return &DB{dsn: "recovered"} })
	got, err := s.Resolve[*DB]()
	if err != nil {
		t.Fatalf("re-registration should recover the key: %v", err)
	}
	if got.dsn != "recovered" {
		t.Fatalf("got %q", got.dsn)
	}
}

// The same for an alias: redirecting the interface to a working
// implementation is the whole point of Bind, so a failed target must not
// foreclose it.
// (pass 7)
func TestRegressionFailedAliasTargetLeavesKeyReAliasable(t *testing.T) {
	s := di.New()
	s.Bind[Reader, *Repo]()
	s.Provide(func(*di.Scope) *Repo { panic("boom") })
	if _, err := s.Resolve[Reader](); err == nil {
		t.Fatal("expected the target's panic to surface")
	}

	// Redirect the interface at a different, untouched implementation.
	s.Bind[Reader, *altReader]()
	s.Provide(func(*di.Scope) *altReader { return &altReader{} })
	got, err := s.Resolve[Reader]()
	if err != nil {
		t.Fatalf("re-aliasing should be allowed: %v", err)
	}
	if got.Read() != "alt" {
		t.Fatalf("got %q", got.Read())
	}
}

// Found by the model-based test: a scope that has already served a key
// from an outer scope must not then shadow it, which would give one key two
// live values within that scope. Shadowing before resolving stays legal,
// since that is how child scopes and tests substitute dependencies.
func TestRegressionCannotShadowAKeyAlreadyServed(t *testing.T) {
	root := di.New()
	root.Provide(func(*di.Scope) *DB { return &DB{dsn: "root"} })

	child := root.Child("child")
	if got := child.Get[*DB]().dsn; got != "root" {
		t.Fatalf("got %q", got)
	}
	mustPanic(t, "already resolved it from an outer scope", func() {
		child.Value(&DB{dsn: "shadow"})
		_ = child.Get[*DB]()
	})

	// The same registration is fine in a scope that has not resolved it.
	fresh := root.Child("fresh")
	fresh.Value(&DB{dsn: "shadow"})
	if got := fresh.Get[*DB]().dsn; got != "shadow" {
		t.Fatalf("pre-resolution shadowing must work, got %q", got)
	}
}

// A Transient constructor goes through the same wrapper as any other: a
// panic becomes an error and a successful build is observed.
// (review 1, 5)
func TestReviewTransientPanicIsAnError(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *vA { panic("boom") }).Transient()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Resolve panicked instead of returning an error: %v", r)
		}
	}()
	if _, err := s.Resolve[*vA](); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v", err)
	}
}

func TestReviewTransientIsObserved(t *testing.T) {
	var builds int
	s := di.New()
	s.Observe(func(ev di.Event) {
		if ev.Kind == di.EventBuild {
			builds++
		}
	})
	s.Provide(func(*di.Scope) *vA { return &vA{} }).Transient()
	s.Get[*vA]()
	s.Get[*vA]()
	if builds != 2 {
		t.Fatalf("EventBuild fired %d times for two transient builds", builds)
	}
}

// An alias owned by the root whose target is local to a child still
// serves the child that key: registering it there afterwards would give the
// child two live values.
// (review 1, 6a)
func TestReviewAliasTargetLocalShadow(t *testing.T) {
	root := di.New()
	root.Bind[vI, *vT]()
	child := root.Child("c")
	child.Provide(func(*di.Scope) *vT { return &vT{n: 1} })
	child.Get[vI]()

	defer func() {
		if recover() == nil {
			t.Fatal("shadowing a key the scope already served through an alias was accepted")
		}
	}()
	child.Provide(func(*di.Scope) vI { return &vT{n: 2} })
	child.Get[vI]()
}

// A scope between the resolver and the owner served the key too, so it
// cannot shadow it afterwards either.
// (review 1, 6b)
func TestReviewIntermediateScopeShadow(t *testing.T) {
	root := di.New()
	root.Provide(func(*di.Scope) *vT { return &vT{n: 1} })
	mid := root.Child("mid")
	gc := mid.Child("gc")
	gc.Get[*vT]()

	defer func() {
		if recover() == nil {
			t.Fatal("an intermediate scope shadowed a key its descendant had already been served")
		}
	}()
	mid.Provide(func(*di.Scope) *vT { return &vT{n: 2} })
	gc.Get[*vT]()
}

// A nil interface is a service like any other. Every hand-back path has
// to survive it, not just the one that stores it.
// (review 1, 7)
func TestReviewNilInterfaceValue(t *testing.T) {
	s := di.New()
	s.Value[error](nil)
	s.Provide(func(*di.Scope) error { return nil }).Group()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a nil interface value panicked: %v", r)
		}
	}()
	if v, err := s.Resolve[error](); err != nil || v != nil {
		t.Fatalf("Resolve: v=%v err=%v", v, err)
	}
	if v := s.Get[error](); v != nil {
		t.Fatalf("Get: %v", v)
	}
	if v, ok := s.Maybe[error](); !ok || v != nil {
		t.Fatalf("Maybe: %v %v", v, ok)
	}
	if all := s.All[error](); len(all) != 1 || all[0] != nil {
		t.Fatalf("All: %v", all)
	}
}

func TestReviewNilInterfaceReachesHooks(t *testing.T) {
	ran := false
	s := di.New()
	s.Value[error](nil).OnStop(func(_ context.Context, v error) error {
		ran = v == nil
		return nil
	})
	_ = s.Get[error]()
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("OnStop did not receive the nil value")
	}
}

// The handler and the injected *http.Request must be one request, so a
// scoped constructor sees what the router matched.
// (review 1, 10)
func TestReviewMiddlewareInjectedRequestRouting(t *testing.T) {
	var gotID, gotScope atomic.Value
	gotID.Store("")
	gotScope.Store(false)

	app := di.New()
	app.Provide(func(s *di.Scope) *Handler {
		r := s.Get[*http.Request]()
		gotID.Store(r.PathValue("id"))
		_, ok := di.FromContext(r.Context())
		gotScope.Store(ok)
		return &Handler{name: r.PathValue("id")}
	}).Scoped()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		sc, _ := di.FromContext(r.Context())
		sc.Get[*Handler]()
	})
	srv := httptest.NewServer(dihttp.Middleware(app)(mux))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/users/42")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if id := gotID.Load().(string); id != "42" {
		t.Fatalf("the injected request has PathValue(id)=%q, want 42", id)
	}
	if !gotScope.Load().(bool) {
		t.Fatal("the injected request's context has no scope")
	}
}

// A pre-built member joins a group like a constructed one, which Add could
// not express, and the plain registration of the same type is neither
// shadowed by the members nor counted among them.
func TestGroupAcceptsValues(t *testing.T) {
	s := di.New()
	s.Value(Handler{"users"}).Group()
	s.Provide(func(*di.Scope) Handler { return Handler{"orders"} }).Group()
	s.Value(Handler{"plain"})
	got := s.All[Handler]()
	if len(got) != 2 || got[0].name != "users" || got[1].name != "orders" {
		t.Fatalf("All = %v", got)
	}
	if got := s.Get[Handler]().name; got != "plain" {
		t.Fatalf("Get = %q, want the plain registration", got)
	}
}
