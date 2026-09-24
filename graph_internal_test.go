package di

import (
	"reflect"
	"testing"
)

// A wait is indexed under every node of the blocked resolution's path up to
// and including the first finished one, because descends matches a node before
// it asks whether that node has finished. So a finished frame that is itself a
// builder still connects two waits: n is blocked beneath the finished frame d
// on what r is building, and r now waits on what d was building, which closes
// a cycle. An index that stopped one node short would let r wait for ever, and
// no test through the exported API reaches that shape.
func TestWaitIndexIncludesTheFinishedNode(t *testing.T) {
	g := &graph{under: map[*resolver]map[*waitEdge]struct{}{}}
	b := &binding{}
	d := (&resolver{}).child(b, nil)
	d.done.Store(true)
	n := d.child(b, nil)
	r := (&resolver{}).child(b, nil)

	byR := &instance{b: b, builder: r}
	byD := &instance{b: b, builder: d}
	edge := n.wait(g, byR)
	if edge == nil {
		t.Fatal("n waiting on r's build is not a cycle on its own")
	}
	if e := r.wait(g, byD); e != nil {
		t.Fatal("r waiting on d's build closes a cycle through n and was allowed")
	}
	g.unwait(edge)
	if len(g.under) != 0 {
		t.Fatalf("%d nodes still indexed after the only wait ended", len(g.under))
	}
}

// Each wait carries the nodes it was indexed under, so two waits by one
// resolution, indexed along different lengths of its path because a frame
// finished in between, are each removed exactly.
func TestWaitEdgesAreRemovedPerWait(t *testing.T) {
	g := &graph{under: map[*resolver]map[*waitEdge]struct{}{}}
	b := &binding{}
	mid := (&resolver{}).child(b, nil).child(b, nil)
	r := mid.child(b, nil)

	long := r.wait(g, &instance{b: b})
	mid.done.Store(true)
	short := r.wait(g, &instance{b: b})
	if len(long.path) <= len(short.path) {
		t.Fatalf("paths of %d and %d nodes; the second should be shorter", len(long.path), len(short.path))
	}
	g.unwait(short)
	g.unwait(long)
	if len(g.under) != 0 {
		t.Fatalf("%d nodes still indexed after both waits ended", len(g.under))
	}
}

// A warm top-level resolution makes no path node, and the one root every
// top-level resolution shares is neither indexed nor ever finished: indexed,
// it would collect every blocked top-level resolution in the container, and
// finished, no view over it would count as in flight again.
func TestTopLevelResolutionSharesAnUnindexedRoot(t *testing.T) {
	s := New()
	s.Provide(func(*Scope) *graph { return &graph{} })
	child := s.Child("child")
	_ = child.Get[*graph]()
	if n := testing.AllocsPerRun(100, func() { _ = s.Get[*graph](); _ = child.Get[*graph]() }); n != 0 && !raceEnabled {
		t.Errorf("a warm Get allocates %v times", n)
	}

	g := &graph{under: map[*resolver]map[*waitEdge]struct{}{}}
	b := &binding{}
	e := topLevel.child(b, nil).wait(g, &instance{b: b})
	if _, ok := g.under[topLevel]; ok || len(e.path) != 1 {
		t.Errorf("the shared root is indexed: path of %d nodes", len(e.path))
	}
	g.unwait(e)
	if topLevel.done.Load() {
		t.Error("the shared root was marked finished")
	}
}

// A claim that ends at a scope with a recorded route looks k up above it
// after reading that record, so a registration committed since the caller's
// own lookup is the owner it returns. (review 7, 3)
func TestClaimLooksUpAfterTheRecordItStopsAt(t *testing.T) {
	k := key{t: reflect.TypeFor[*graph]()}
	root := New()
	root.Provide(func(*Scope) *graph { return &graph{} })
	mid := root.Child("mid")
	leaf := mid.Child("leaf")
	lc := leaf.Child("lc")
	if _, owner := lc.st.lookup(k); owner != root.st {
		t.Fatal("the setup expects the root to own the key at first")
	}
	mid.Provide(func(*Scope) *graph { return &graph{} })
	_ = leaf.Get[*graph]() // commits mid's registration; leaf records mid
	if _, owner := lc.st.claim(k); owner != mid.st {
		t.Fatalf("claim returned %s's registration, want mid's", owner.name)
	}
}

// get trusts a lookup only when it found the owner of its scope's recorded
// route: a lookup made before a nearer owner was recorded is stale.
// (review 7, 3)
func TestClaimedDistrustsALookupStaleAgainstTheRecord(t *testing.T) {
	k := key{t: reflect.TypeFor[*graph]()}
	root := New()
	root.Provide(func(*Scope) *graph { return &graph{} })
	mid := root.Child("mid")
	leaf := mid.Child("leaf")
	_, stale := leaf.st.lookup(k)
	mid.Provide(func(*Scope) *graph { return &graph{} })
	_ = leaf.Child("sibling").Get[*graph]() // records mid on leaf
	if leaf.st.claimed(k, stale) {
		t.Fatal("a lookup that found the root before mid registered was trusted")
	}
	if !leaf.st.claimed(k, mid.st) {
		t.Fatal("the recorded owner was not trusted")
	}
}

// A wrapper's route that meets a scope with a recorded route stops there and
// records the owner on the scope above the wrapper. (review 7, 3)
func TestMarkBoundRecordsAtAScopeWithARecordedRoute(t *testing.T) {
	k := key{t: reflect.TypeFor[*graph]()}
	root := New()
	root.Provide(func(*Scope) *graph { return &graph{} })
	mid := root.Child("mid")
	_ = mid.Child("y").Get[*graph]() // records root on mid
	x := mid.Child("x")
	w := x.Child("w")
	w.Wrap[*graph](func(g *graph) *graph { return g })
	_ = w.Get[*graph]()
	if got := x.st.served[k]; got != root.st {
		t.Fatalf("x records %v, want the root", got)
	}
}
