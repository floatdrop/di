package di_test

// Regressions in registration and lookup: groups, shadowing, the eager set,
// and the rejections freeze is responsible for.
// Tags are explained in fixtures_test.go.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.yandex/di"
	"golang.yandex/di/dihttp"
)

// Eager on a group member builds it at Start.
func TestRegressionEagerGroupMember(t *testing.T) {
	var builds, starts atomic.Int32
	s := di.New()
	s.Provide(func(*di.Scope) Handler { builds.Add(1); return Handler{} }).Group().Eager().
		OnStart(func(context.Context, Handler) error { starts.Add(1); return nil })
	if err := s.Start(t.Context()); err != nil {
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
		{"eager scoped", "does not apply to a Scoped binding", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *rA { return &rA{} }).Scoped().Eager()
		}},
		{"scoped then eager", "does not apply to a Scoped binding", func(s *di.Scope) {
			s.Provide(func(*di.Scope) *rA { return &rA{} }).Eager().Scoped()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := di.New()
			tc.wire(s)
			mustPanic(t, tc.want, func() { _ = s.Start(t.Context()) })
		})
	}
}

// A binding carries one hook of each kind, and a second registration is
// rejected: it used to assign over the first, so a release the caller asked
// for was dropped without a word. (fx review)
func TestSecondHookIsRejected(t *testing.T) {
	noop := func(context.Context, *DB) error { return nil }
	for _, tc := range []struct {
		name string
		want string
		set  func(di.Binding[*DB])
	}{
		{"OnStart", "a second OnStart hook", func(b di.Binding[*DB]) { b.OnStart(noop) }},
		{"OnDrain", "a second OnDrain hook", func(b di.Binding[*DB]) { b.OnDrain(noop) }},
		{"OnStop", "a second OnStop hook", func(b di.Binding[*DB]) { b.OnStop(noop) }},
		{"Go", "a second Go worker", func(b di.Binding[*DB]) { b.Go(noop) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := di.New().Value(&DB{})
			tc.set(b)
			mustPanic(t, tc.want, func() { tc.set(b) })
		})
	}
}

// The rejection names the second call, which is one more frame count to get
// right; the closure and the at call are one line, so the hook's site is
// that line. (fx review)
func TestSecondHookNamesTheSecondCall(t *testing.T) {
	at := func(fn func()) (int, func()) { _, _, line, _ := runtime.Caller(1); return line, fn }
	noop := func(context.Context, *DB) error { return nil }
	b := di.New().Value(&DB{}).OnStop(noop)

	line, second := at(func() { b.OnStop(noop) })
	mustPanic(t, fmt.Sprintf("wiring_test.go:%d;", line), second)
}

// Wiring late is not rejected: whether a key is provided is a question about
// the chain as it stands, and a scope below may answer it differently. What
// the services built before it missed is reported by Explain. (fx review)
func TestOptionalWiredLateIsAccepted(t *testing.T) {
	s := di.New()
	s.Provide(func(sc *di.Scope) *rA {
		_, _ = sc.Maybe[*rC]()
		return &rA{}
	})
	s.Get[*rA]()

	s.Value(&rC{}) // nobody was promised there would never be one
	if _, ok := s.Maybe[*rC](); !ok {
		t.Fatal("the late registration did not take")
	}
	if got := s.Explain[*rC](); !strings.Contains(compact(got), "missed by: *rA in root") {
		t.Fatalf("Explain does not report what missed it:\n%s", compact(got))
	}
}

// A miss outside a constructor records nothing: no value was built on the
// answer, so there is nobody to report. (fx review)
func TestTopLevelMaybeMissRecordsNothing(t *testing.T) {
	s := di.New()
	if _, ok := s.Maybe[*rC](); ok {
		t.Fatal("*rC is not provided")
	}
	s.Value(&rC{}) // the register-a-default-if-absent pattern
	if _, ok := s.Maybe[*rC](); !ok {
		t.Fatal("the default was not registered")
	}
	if got := s.Explain[*rC](); strings.Contains(got, "missed by") {
		t.Fatalf("a top-level miss was reported:\n%s", got)
	}
}

// A decline is about the key a lookup answers, and a group member answers
// none: All reads the group, so a member is not what the asker missed.
// (fx review)
func TestDeclinedKeyAndAGroupMemberAreDifferentQuestions(t *testing.T) {
	s := di.New()
	s.Provide(func(sc *di.Scope) *rA {
		_, _ = sc.Maybe[Handler]()
		return &rA{}
	})
	s.Get[*rA]()

	s.Provide(func(*di.Scope) Handler { return Handler{} }).Group()
	if got := s.All[Handler](); len(got) != 1 {
		t.Fatalf("All returned %d members", len(got))
	}
	if got := s.Explain[Handler](); strings.Contains(got, "missed by") {
		t.Fatalf("a group member was reported as a missed optional:\n%s", got)
	}
}

// A constructor may ask from goroutines of its own, so the record is written
// under the holder's mutex. Meaningful under -race. (fx review)
func TestDeclineFromAConstructorsGoroutines(t *testing.T) {
	s := di.New()
	s.Provide(func(sc *di.Scope) *rA {
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() { _, _ = sc.Maybe[*rC]() })
		}
		wg.Wait()
		return &rA{}
	})
	s.Get[*rA]()

	s.Value(&rC{})
	if got := s.Explain[*rC](); !strings.Contains(compact(got), "missed by: *rA in root") {
		t.Fatalf("Explain does not report what missed it:\n%s", compact(got))
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
		if err := s.Start(t.Context()); err != nil {
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
	s.Value(&DB{dsn: "b"}).Override()
	mustPanic(t, "cannot be overridden", func() { s.Get[*DB]() })

	// Overriding before the key is resolved stays legal: the test seam.
	ok := di.New()
	ok.Value(&DB{dsn: "a"})
	ok.Value(&DB{dsn: "b"}).Override()
	if got := ok.Get[*DB]().dsn; got != "b" {
		t.Fatalf("override before resolution must win, got %q", got)
	}
}

// An eager binding that a later registration overrode must not be built.
// (pass 2)
func TestRegressionShadowedEagerNotBuilt(t *testing.T) {
	var log []string
	s := di.New()
	s.Provide(func(*di.Scope) *DB { log = append(log, "real"); return &DB{dsn: "real"} }).Eager().
		OnStart(func(context.Context, *DB) error { log = append(log, "startReal"); return nil })
	s.Provide(func(*di.Scope) *DB { log = append(log, "fake"); return &DB{dsn: "fake"} }).Eager().Override().
		OnStart(func(context.Context, *DB) error { log = append(log, "startFake"); return nil })
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); got != "fake,startFake" {
		t.Fatalf("got %q, want only the winning registration built", got)
	}
	if got := s.Get[*DB]().dsn; got != "fake" {
		t.Fatalf("Get returned %q", got)
	}
}

// Overriding an Eager binding keeps the key eager: the replacement is built
// at Start.
// (pass 3)
func TestRegressionOverrideKeepsKeyEager(t *testing.T) {
	var log []string
	s := di.New()
	s.Provide(func(*di.Scope) *DB { log = append(log, "real"); return &DB{dsn: "real"} }).Eager().
		OnStart(func(context.Context, *DB) error { log = append(log, "startReal"); return nil })
	s.Value(&DB{dsn: "fake"}).Override().
		OnStart(func(context.Context, *DB) error { log = append(log, "startFake"); return nil })
	if err := s.Start(t.Context()); err != nil {
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			built := false
			s := di.New()
			s.Provide(func(*di.Scope) *DB { return &DB{dsn: "real"} }).Eager()
			tc.apply(s.Provide(func(*di.Scope) *DB { built = true; return &DB{dsn: "fake"} }).Override())
			mustPanic(t, "eagerness cannot transfer", func() { _ = s.Start(t.Context()) })
			if built {
				t.Fatalf("a %s binding was built at Start", tc.name)
			}
		})
	}
}

// A rejected registration is rejected every time: a retried Start must not
// succeed with the invalid configuration dropped.
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
			_ = s.Start(t.Context())
		}()
	}
}

// A rejected batch leaves the scope as it was, so every later operation is
// rejected identically.
// (pass 6)
func TestRegressionRejectedBatchIsNotHalfApplied(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *DB { return &DB{} })
	s.Provide(func(*di.Scope) *Repo { return &Repo{} }).Scoped().Eager() // invalid

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
		if !strings.Contains(m, "does not apply to a Scoped binding") {
			t.Fatalf("attempt %d: %s", i, m)
		}
	}
	if msgs[0] != msgs[1] || msgs[1] != msgs[2] {
		t.Fatalf("rejection was not identical across attempts: %v", msgs)
	}
}

// A key whose constructor failed built nothing, so it can be overridden.
// Because a failed instance caches its error for good, this is the only way
// to recover such a key.
// (pass 7)
func TestRegressionFailedResolveLeavesKeyReRegisterable(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *DB { panic("boom") })
	if _, err := s.Resolve[*DB](); err == nil {
		t.Fatal("expected the constructor panic to surface")
	}
	s.Provide(func(*di.Scope) *DB { return &DB{dsn: "recovered"} }).Override()
	got, err := s.Resolve[*DB]()
	if err != nil {
		t.Fatalf("re-registration should recover the key: %v", err)
	}
	if got.dsn != "recovered" {
		t.Fatalf("got %q", got.dsn)
	}
}

// A scope that has served a key from an outer scope must not then shadow it:
// the key would have two live values there. Shadowing before resolving stays
// legal. Found by the model-based test.
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

// A nil interface is a service like any other, on every hand-back path.
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
	if err := s.Stop(t.Context()); err != nil {
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
	srv := httptest.NewServer(dihttp.NewMiddleware(app)(mux))
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

// A pre-built member joins a group like a constructed one, and the plain
// registration of the same type is neither shadowed by the members nor
// counted among them.
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

// A key cannot be overridden while a resolution of it is in flight: a
// constructor that registers over its own key and resolves the replacement
// would be served the new value in the nested call and return the old one.
// (review 4, 3)
func TestReview4CannotOverrideAKeyBeingResolved(t *testing.T) {
	s := di.New()
	var nested *DB
	s.Provide(func(sc *di.Scope) *DB {
		sc.Provide(func(*di.Scope) *DB { return &DB{dsn: "new"} }).Override()
		nested, _ = sc.Resolve[*DB]()
		return &DB{dsn: "old"}
	})
	outer, err := s.Resolve[*DB]()
	if err == nil {
		t.Fatalf("the re-registration was accepted: nested=%v outer=%q", nested, outer.dsn)
	}
	if !strings.Contains(err.Error(), "it is being resolved") {
		t.Fatalf("want the rejection to say the key is being resolved, got %v", err)
	}

	// The key is free again once that resolution has failed.
	s.Provide(func(*di.Scope) *DB { return &DB{dsn: "recovered"} }).Override()
	got, err := s.Resolve[*DB]()
	if err != nil {
		t.Fatalf("re-registration after the failure should recover the key: %v", err)
	}
	if got.dsn != "recovered" {
		t.Fatalf("got %q", got.dsn)
	}
}

// callsite finds a registration's site by counting frames, so each
// registration method is called directly here and its site compared with the
// exact line: a count one frame short or long fails.
func TestRegistrationSiteNamesTheCaller(t *testing.T) {
	// at returns the line it is called from, so at(s.Provide(...)) is the
	// line of that Provide call.
	at := func(any) int { _, _, line, _ := runtime.Caller(1); return line }
	sites := map[string]int{}
	want := func(name string, s *di.Scope, line int) {
		t.Helper()
		sites[name] = line
		site := fmt.Sprintf("wiring_test.go:%d)", line)
		if out := s.Explain[*DB](); !strings.Contains(out, site) {
			t.Errorf("%s: want the site %s, got:\n%s", name, site, out)
		}
	}

	s := di.New()
	want("Provide", s, at(s.Provide(func(*di.Scope) *DB { return &DB{} })))
	s = di.New()
	want("Value", s, at(s.Value(&DB{})))
	s = di.New()
	want("Wire", s, at(s.Wire[*DB](func() *DB { return &DB{} })))
	s = di.New()
	s.Value(&DB{})
	want("Wrap", s, at(s.Wrap[*DB](func(db *DB) *DB { return db })))
	if len(sites) != 4 {
		t.Fatalf("checked %d registration methods, want 4", len(sites))
	}
}

// An Override racing the first resolution of what it replaces commits only if
// that resolution serves nothing from the replaced registration. The window is
// a few instructions wide, so this repeats it for a second. (review 7, 2)
func TestReviewOverrideRacingFirstResolution(t *testing.T) {
	for i, start := 0, time.Now(); time.Since(start) < time.Second; i++ {
		s := di.New()
		s.Provide(func(*di.Scope) *vT { return &vT{n: 1} })
		var got *vT
		var ready atomic.Int32
		gate := func() {
			ready.Add(1)
			for ready.Load() < 2 {
			}
		}
		var wg sync.WaitGroup
		wg.Go(func() {
			defer func() { _ = recover() }()
			gate()
			got = s.Get[*vT]()
		})
		wg.Go(func() {
			defer func() { _ = recover() }()
			gate()
			s.Provide(func(*di.Scope) *vT { return &vT{n: 2} }).Override()
			_, _ = s.Resolve[*vT]()
		})
		wg.Wait()
		committed := func() (ok bool) {
			defer func() { _ = recover() }() // a rejected Override panics again here
			now, err := s.Resolve[*vT]()
			return err == nil && now.n == 2
		}()
		if got != nil && got.n == 1 && committed {
			t.Fatalf("iteration %d: the old registration served a value and the Override still committed", i)
		}
	}
}

// A scope a resolution is passing through, between the resolving scope and
// the owner it looked up, that commits its own registration of the key before
// the route is marked gives the resolving scope nothing: the resolution looks
// again, and its scope serves one value for the key. (review 7, 3)
func TestReviewRegistrationOnARouteBeingResolved(t *testing.T) {
	servedAny := false
	for i := range 1000 {
		root := di.New()
		root.Provide(func(*di.Scope) *vT { return &vT{n: 1} })
		mid := root.Child("mid")
		leaf := mid.Child("leaf")
		got := raceGetAgainst(func() *vT { return leaf.Get[*vT]() }, func() {
			mid.Provide(func(*di.Scope) *vT { return &vT{n: 2} })
			_, _ = mid.Resolve[*vT]()
		})
		if later := served(func() *vT { return leaf.Get[*vT]() }); got != nil && later != nil && later != got {
			t.Fatalf("iteration %d: leaf was served n=%d, then n=%d", i, got.n, later.n)
		}
		servedAny = servedAny || got != nil
	}
	if !servedAny {
		t.Fatal("no iteration served leaf a value, so none was checked")
	}
}

// served returns what get serves, or the zero value if it panics, as it does
// through a scope whose rejected registration is still pending.
func served[T any](get func() T) (v T) {
	defer func() { _ = recover() }()
	return get()
}

// raceGetAgainst runs get and other together, released at once, and returns
// what get served, or nil if it panicked.
func raceGetAgainst[T any](get func() T, other func()) (got T) {
	var ready atomic.Int32
	gate := func() {
		ready.Add(1)
		for ready.Load() < 2 {
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		defer func() { _ = recover() }()
		gate()
		got = get()
	})
	wg.Go(func() {
		defer func() { _ = recover() }()
		gate()
		other()
	})
	wg.Wait()
	return got
}

// A route is claimed before its value is built, so a scope on it cannot
// register the key while a resolution through it is still building, and no
// value is built from an owner the resolution then abandons. (review 7, 3)
func TestReviewRouteIsClaimedBeforeTheBuild(t *testing.T) {
	for _, shape := range []string{"plain", "beside a wrapper"} {
		t.Run(shape, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var park atomic.Bool
			root := di.New()
			root.Provide(func(*di.Scope) *vT {
				if park.CompareAndSwap(true, false) {
					close(entered)
					<-release
				}
				return &vT{n: 1}
			}).Scoped()
			mid := root.Child("mid")
			if shape == "beside a wrapper" {
				// Built first, so its route's marks are there for leaf's claim.
				// This shape passes on e7b5447 too: it guards how the two kinds
				// of mark meet, not a defect.
				wrapping := mid.Child("wrapping")
				wrapping.Wrap[*vT](func(v *vT) *vT { return &vT{n: 10 + v.n} })
				if got := wrapping.Child("r").Get[*vT]().n; got != 11 {
					t.Fatalf("the wrapper served n=%d", got)
				}
			}
			leaf := mid.Child("leaf")
			park.Store(true)
			var got *vT
			var wg sync.WaitGroup
			wg.Go(func() { got = leaf.Get[*vT]() })
			<-entered
			mid.Provide(func(*di.Scope) *vT { return &vT{n: 2} })
			rejected(t, "already resolved it from an outer scope", func() { _, _ = mid.Resolve[*vT]() })
			close(release)
			wg.Wait()
			if got.n != 1 {
				t.Fatalf("leaf was served n=%d", got.n)
			}
		})
	}
}

// A stopped scope refuses before it claims anything, so a resolution it
// refuses leaves no mark on the live scopes above it. It passes on e7b5447
// too, which marked only after a build: it guards the claim's order.
// (review 7, 3)
func TestReviewStoppedScopeClaimsNothing(t *testing.T) {
	root := di.New()
	root.Provide(func(*di.Scope) *vT { return &vT{n: 1} })
	mid := root.Child("mid")
	leaf := mid.Child("leaf")
	if err := leaf.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Resolve[*vT](); !errors.Is(err, di.ErrStopped) {
		t.Fatalf("got %v, want ErrStopped", err)
	}
	mid.Provide(func(*di.Scope) *vT { return &vT{n: 2} })
	if got, err := mid.Resolve[*vT](); err != nil || got.n != 2 {
		t.Fatalf("mid's own registration: %v, %v", got, err)
	}
}

// A claim commits what the resolution would look through and nothing above
// it, so a rejected batch in a scope above the owner, or between a wrapper
// and what it wraps, is not this resolution's to report. It passes on e7b5447
// too: it guards how far the claim reaches.
func TestClaimLeavesUnrelatedBatchesAlone(t *testing.T) {
	root := di.New()
	mid := root.Child("mid")
	mid.Provide(func(*di.Scope) *vT { return &vT{n: 1} })
	root.Provide(func(*di.Scope) *DB { return &DB{} })
	root.Provide(func(*di.Scope) *DB { return &DB{} }) // a collision, pending
	if got, err := mid.Child("leaf").Resolve[*vT](); err != nil || got.n != 1 {
		t.Fatalf("above the owner: %v, %v", got, err)
	}

	r := di.New()
	r.Provide(func(*di.Scope) *vT { return &vT{n: 1} })
	m := r.Child("m")
	w := m.Child("w")
	w.Wrap[*vT](func(v *vT) *vT { return &vT{n: 10 + v.n} })
	m.Provide(func(*di.Scope) *DB { return &DB{} })
	m.Provide(func(*di.Scope) *DB { return &DB{} }) // a collision, pending
	if got, err := w.Resolve[*vT](); err != nil || got.n != 11 {
		t.Fatalf("between a wrapper and what it wraps: %v, %v", got, err)
	}
}
