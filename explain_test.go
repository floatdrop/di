package di_test

// Tests for the recorded dependency graph and the two renderings of it.
//
// Explain output is compared whole rather than by substring, because the
// shape of the tree is most of what it is for. compact drops the two things
// that cannot be written down in a test: the registration site, which is an
// absolute path, and the package qualifier, which is the same on every line.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floatdrop/di"
)

type (
	xA     struct{}
	xB     struct{}
	xC     struct{}
	xD     struct{}
	xCfg   struct{ dsn string }
	xPlate struct{ n int }
)

var explainSite = regexp.MustCompile(` \(provided at [^)]*\)`)

func compact(s string) string {
	s = explainSite.ReplaceAllString(s, "")
	return strings.ReplaceAll(s, "github.com/floatdrop/di_test.", "")
}

func wantExplain(t *testing.T, got, want string) {
	t.Helper()
	if compact(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", compact(got), want)
	}
}

// A constructor's dependencies are recorded as it resolves them, so the tree
// is the shape of the graph that was actually built.
func TestExplainTree(t *testing.T) {
	s := di.New()
	s.Value(xCfg{dsn: "pg://x"})
	s.Provide(func(sc *di.Scope) *xD { _ = sc.Get[xCfg](); return &xD{} })
	s.Provide(func(sc *di.Scope) *xC { _ = sc.Get[*xD](); return &xC{} })
	s.Provide(func(sc *di.Scope) *xB { _ = sc.Get[*xC](); return &xB{} })
	s.Get[*xB]()

	wantExplain(t, s.Explain[*xB](), `*xB: singleton in root, built
└── *xC: singleton in root, built
    └── *xD: singleton in root, built
        └── xCfg: value in root, built
`)
}

// A dependency reached twice is expanded once. Without that a diamond is
// drawn twice, and a deep one exponentially.
func TestExplainDiamondIsExpandedOnce(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *xD { return &xD{} })
	s.Provide(func(sc *di.Scope) *xC { _ = sc.Get[*xD](); return &xC{} })
	s.Provide(func(sc *di.Scope) *xB { _ = sc.Get[*xD](); return &xB{} })
	s.Provide(func(sc *di.Scope) *xA { _ = sc.Get[*xB](); _ = sc.Get[*xC](); return &xA{} })
	s.Get[*xA]()

	wantExplain(t, s.Explain[*xA](), `*xA: singleton in root, built
├── *xB: singleton in root, built
│   └── *xD: singleton in root, built
└── *xC: singleton in root, built
    └── *xD: see above
`)
}

// The instances that needed a service are as much of an answer as the ones it
// needed, and they are the half a build order cannot give.
func TestExplainNamesWhatNeededIt(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *xD { return &xD{} })
	s.Provide(func(sc *di.Scope) *xC { _ = sc.Get[*xD](); return &xC{} })
	s.Provide(func(sc *di.Scope) *xB { _ = sc.Get[*xD](); return &xB{} })
	s.Get[*xB]()
	s.Get[*xC]()

	wantExplain(t, s.Explain[*xD](), `*xD: singleton in root, built
needed by: *xB in root, *xC in root
`)
}

func TestExplainNotProvided(t *testing.T) {
	wantExplain(t, di.New().Explain[*xA](), "*xA: not provided\n")
}

// Explain resolves nothing: a service that has not been built is reported
// with its registration and left alone.
func TestExplainDoesNotBuild(t *testing.T) {
	var builds atomic.Int32
	s := di.New()
	s.Provide(func(*di.Scope) *xA { builds.Add(1); return &xA{} })

	wantExplain(t, s.Explain[*xA](), "*xA: singleton in root, not built\n")
	if builds.Load() != 0 {
		t.Fatalf("Explain ran the constructor %d times", builds.Load())
	}
}

// Eagerness is registration, not lifecycle, and it is half the answer to why
// a service exists at all.
func TestExplainReportsEagerAndStarted(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *xA { return &xA{} }).Eager().
		OnStart(func(context.Context, *xA) error { return nil })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantExplain(t, s.Explain[*xA](), "*xA: singleton in root, eager, started\n")
}

// A Scoped instance is named by the scope holding it, and what it resolved
// from an outer scope by that scope.
func TestExplainScopedNamesEachHolder(t *testing.T) {
	root := di.New()
	root.Provide(func(*di.Scope) *xD { return &xD{} })
	root.Provide(func(sc *di.Scope) *xA { _ = sc.Get[*xD](); return &xA{} }).Scoped()

	req := root.Child("request")
	req.Get[*xA]()

	wantExplain(t, req.Explain[*xA](), `*xA: scoped in request, built
└── *xD: singleton in root, built
`)
	// The same binding from a scope that never resolved it has no instance.
	other := root.Child("other")
	wantExplain(t, other.Explain[*xA](), "*xA: scoped in other, not built\n")
}

// A failed build is exactly when the question gets asked, so the instance is
// reported with the error it cached rather than as absent.
func TestExplainReportsAFailedBuild(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *xA { panic("boom") })
	if _, err := s.Resolve[*xA](); err == nil {
		t.Fatal("expected the constructor panic to surface")
	}
	got := compact(s.Explain[*xA]())
	if !strings.HasPrefix(got, "*xA: singleton in root, failed: ") || !strings.Contains(got, "boom") {
		t.Fatalf("got %q", got)
	}
	// It never became an instance the scope holds, so the graph has no node.
	if strings.Contains(s.Graph(), "xA") {
		t.Fatalf("a failed build appears in the graph:\n%s", s.Graph())
	}
}

// A key served only by a group still resolves, through All, so reporting it
// as not provided would be a lie. Each member is explained in turn.
func TestExplainGroupMembers(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *xD { return &xD{} })
	s.Provide(func(sc *di.Scope) xPlate { _ = sc.Get[*xD](); return xPlate{1} }).Group()
	s.Provide(func(*di.Scope) xPlate { return xPlate{2} }).Group()
	_ = s.All[xPlate]()

	wantExplain(t, s.Explain[xPlate](), `xPlate: singleton group member in root, built
└── *xD: singleton in root, built

xPlate: singleton group member in root, built
`)
}

// A plain registration and a group of the same type are different bindings.
// Explain shows both, because Get and All each resolve one of them.
func TestExplainPlainBindingAndGroupTogether(t *testing.T) {
	s := di.New()
	s.Value(xPlate{9})
	s.Provide(func(*di.Scope) xPlate { return xPlate{1} }).Group()
	s.Get[xPlate]()
	_ = s.All[xPlate]()

	wantExplain(t, s.Explain[xPlate](), `xPlate: value in root, built

xPlate: singleton group member in root, built
`)
}

// One edge per distinct dependency, however many times it was asked for.
func TestEdgesAreRecordedOnce(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *xD { return &xD{} })
	s.Provide(func(sc *di.Scope) *xA {
		for range 5 {
			_ = sc.Get[*xD]()
		}
		return &xA{}
	})
	s.Get[*xA]()

	wantExplain(t, s.Explain[*xA](), `*xA: singleton in root, built
└── *xD: singleton in root, built
`)
}

// A Get made outside a constructor has nobody to tell, which is what keeps
// the recording off the warm path.
func TestNoEdgeForATopLevelGet(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *xD { return &xD{} })
	s.Provide(func(*di.Scope) *xA { return &xA{} })
	s.Get[*xD]()
	s.Get[*xA]()
	s.Get[*xD]()

	if strings.Contains(s.Graph(), "->") {
		t.Fatalf("a top-level Get recorded an edge:\n%s", s.Graph())
	}
}

// A constructor may keep the Scope it was handed and resolve through it once
// its own service is built. That resolution is a new path, so it is not
// recorded as a dependency -- the same rule that stops it being called a
// cycle.
func TestNoEdgeThroughAScopeKeptPastItsConstructor(t *testing.T) {
	s := di.New()
	var kept *di.Scope
	s.Provide(func(sc *di.Scope) *xA { kept = sc; return &xA{} })
	s.Provide(func(*di.Scope) *xD { return &xD{} })
	s.Get[*xA]()
	_ = kept.Get[*xD]()

	wantExplain(t, s.Explain[*xA](), "*xA: singleton in root, built\n")
}

// A constructor may resolve from several goroutines at once, and they share
// the Scope it was handed, so the edges land on one instance concurrently.
// Run with -race; the order they arrive in is not fixed, so only the set is
// checked.
func TestEdgesFromGoroutinesInsideAConstructor(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *xB { return &xB{} })
	s.Provide(func(*di.Scope) *xC { return &xC{} })
	s.Provide(func(*di.Scope) *xD { return &xD{} })
	s.Provide(func(sc *di.Scope) *xA {
		var wg sync.WaitGroup
		for range 3 {
			wg.Add(3)
			go func() { defer wg.Done(); _, _ = sc.Resolve[*xB]() }()
			go func() { defer wg.Done(); _, _ = sc.Resolve[*xC]() }()
			go func() { defer wg.Done(); _, _ = sc.Resolve[*xD]() }()
		}
		wg.Wait()
		return &xA{}
	})
	s.Get[*xA]()

	lines := strings.Split(strings.TrimSuffix(compact(s.Explain[*xA]()), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want the root and three dependencies, got:\n%s", strings.Join(lines, "\n"))
	}
	for _, want := range []string{"*xB", "*xC", "*xD"} {
		if !strings.Contains(compact(s.Explain[*xA]()), want+": singleton in root, built") {
			t.Fatalf("%s missing:\n%s", want, compact(s.Explain[*xA]()))
		}
	}
}

// One box per instance, one cluster per scope that holds any, and an arrow
// for every recorded edge.
func TestGraphClustersScopesAndDrawsEdges(t *testing.T) {
	root := di.New()
	root.Provide(func(*di.Scope) *xD { return &xD{} })
	root.Provide(func(sc *di.Scope) *xA { _ = sc.Get[*xD](); return &xA{} }).Scoped()
	req := root.Child("request")
	req.Get[*xA]()

	want := `digraph di {
  rankdir=LR;
  node [shape=box, fontname="monospace"];
  subgraph cluster0 {
    label="root";
    n0 [label="*xD\nsingleton, built"];
  }
  subgraph cluster1 {
    label="root/request";
    n1 [label="*xA\nscoped, built"];
  }
  n1 -> n0;
}
`
	if got := compact(root.Graph()); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Nodes are numbered by scope creation order and then build order, so the
// same run of the same program renders the same document.
func TestGraphIsDeterministic(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *xD { return &xD{} })
	s.Provide(func(sc *di.Scope) *xC { _ = sc.Get[*xD](); return &xC{} })
	s.Provide(func(sc *di.Scope) *xB { _ = sc.Get[*xC](); return &xB{} })
	s.Get[*xB]()
	first := s.Graph()
	for range 20 {
		if got := s.Graph(); got != first {
			t.Fatalf("Graph differs between calls:\n%s\n%s", first, got)
		}
	}
}

func TestGraphOfAnEmptyScope(t *testing.T) {
	want := "digraph di {\n  rankdir=LR;\n  node [shape=box, fontname=\"monospace\"];\n}\n"
	if got := di.New().Graph(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Graph reads and changes nothing, so an unresolvable configuration is not
// its business. Explain has to look a key up, and a lookup commits the
// pending registrations, so it reports the rejection the same way a
// resolution would.
func TestGraphDoesNotCommitRegistrationsButExplainDoes(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *xA { return &xA{} }).Scoped().Eager() // cannot be honoured

	if got := s.Graph(); !strings.HasPrefix(got, "digraph di {") {
		t.Fatalf("Graph did not render a pending rejection scope: %q", got)
	}
	mustPanic(t, "does not apply to a Scoped binding", func() { _ = s.Explain[*xA]() })
}

// A stopped scope holds no instances, so it contributes none.
func TestGraphAfterStop(t *testing.T) {
	s := di.New()
	s.Provide(func(*di.Scope) *xD { return &xD{} })
	s.Get[*xD]()
	if !strings.Contains(s.Graph(), "xD") {
		t.Fatal("the built instance is missing before Stop")
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.Graph(), "xD") {
		t.Fatalf("a stopped scope still reports instances:\n%s", s.Graph())
	}
}

// Scope names come from the caller, so a quote in one must not end the DOT
// label it is written into.
func TestGraphEscapesScopeNames(t *testing.T) {
	root := di.New()
	root.Provide(func(*di.Scope) *xD { return &xD{} }).Scoped()
	kid := root.Child(`we"ird\one`)
	kid.Get[*xD]()
	if !strings.Contains(root.Graph(), `label="root/we\"ird\\one"`) {
		t.Fatalf("scope name not escaped:\n%s", root.Graph())
	}
}

// Explain and Graph are read-only, so a hook may call them while the scope is
// winding down without waiting for anything.
func TestExplainAndGraphFromInsideAHook(t *testing.T) {
	s := di.New()
	var seen string
	s.Provide(func(*di.Scope) *xD { return &xD{} })
	s.Provide(func(sc *di.Scope) *xA { _ = sc.Get[*xD](); return &xA{} }).
		OnStop(func(context.Context, *xA) error {
			seen = compact(s.Explain[*xA]()) + s.Graph()
			return nil
		})
	s.Get[*xA]()
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "*xA: singleton in root, stopped") {
		t.Fatalf("a hook did not see its own instance stopping:\n%s", seen)
	}
	if !strings.Contains(seen, "*xD") {
		t.Fatalf("a hook did not see the graph:\n%s", seen)
	}
}

// Explain from inside the constructor of the very thing being explained must
// not wait for the build it is part of.
func TestExplainFromInsideAConstructor(t *testing.T) {
	s := di.New()
	var seen string
	s.Provide(func(*di.Scope) *xD { return &xD{} })
	s.Provide(func(sc *di.Scope) *xA {
		_ = sc.Get[*xD]()
		seen = compact(sc.Explain[*xA]())
		return &xA{}
	})
	done := make(chan struct{})
	go func() { defer close(done); s.Get[*xA]() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Explain from inside a constructor did not return")
	}
	if !strings.HasPrefix(seen, "*xA: singleton in root, building") {
		t.Fatalf("got %q", seen)
	}
}

// An error a hook returns is not a wiring failure, so the shapes above must
// not have changed what Resolve reports.
func TestExplainLeavesResolutionAlone(t *testing.T) {
	s := di.New()
	s.Provide(func(sc *di.Scope) *xA { _ = sc.Get[*xD](); return &xA{} })
	_, err := s.Resolve[*xA]()
	if !errors.Is(err, di.ErrNotProvided) {
		t.Fatalf("got %v", err)
	}
}
