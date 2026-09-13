package di

// A scope's state: its registry, the freeze that commits registrations into
// it, and the readers that walk the parent chain. No two state mutexes are
// ever ordered against each other; a walk takes and releases each in turn.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
)

// state is a scope's registry and lifecycle bookkeeping. A Scope is a handle
// over it.
type state struct {
	name   string
	parent *state
	graph  *graph // the container's wait-for graph, shared with every other scope under the root

	// reg is the committed registry. freeze builds a new one and stores it,
	// and nothing writes to a registry after that, so a lookup reads it
	// without the mutex. hasPending says whether freeze has a batch to
	// commit: register sets it and freeze clears it, both under mu, so a
	// lookup that finds it clear skips the lock as well.
	reg        atomic.Pointer[registry]
	hasPending atomic.Bool
	frozen     bool // guarded by mu

	mu       sync.Mutex
	pending  []*binding             // registrations not yet indexed
	started  []*instance            // build order; stopped in reverse
	scoped   map[*binding]*instance // per-scope instances of Scoped bindings
	served   map[key]bool           // keys this scope resolved from an outer scope; lazily made
	children []*state

	// observers is replaced whole by Observe, under mu, and read without it
	// by emit, which runs for every step in every scope below this one.
	observers atomic.Pointer[[]func(Event)]

	// startCtx is set once, by Start, and running once Start reaches its
	// hook phase, which is when a service built later starts itself. Both
	// are atomic because every build reads them up the whole scope chain.
	// What makes a late build start exactly once is their order against
	// publish, not a mutex: see startIfRunning.
	startCtx atomic.Pointer[context.Context]
	running  atomic.Bool

	stopped  atomic.Bool     // set by the seal that ends Stop's drain, a failed Start's included; resolution then fails with ErrStopped
	stopCtx  context.Context // the context Stop was called with
	stopOnce once            // this scope's teardown; later Stop calls wait for it

	// drainOnce is the scope-wide drain phase, once-with-wait like stopOnce:
	// a second Stop reaching this scope waits for the first drain instead of
	// running its own or skipping past it.
	drainOnce once

	// drainGen counts what could create drain work in this scope's subtree:
	// a build published into it, a start step claimed in it. sealed and
	// sealCh are how a teardown ends the drain phase against those without a
	// lock the two sides share; see seal and announce.
	drainGen atomic.Uint64
	sealCh   chan struct{} // guarded by mu; made by an announcer that must wait, closed when the seal is decided
	sealed   atomic.Bool

	// The fields are ordered so the 4-byte atomics and the bool pack
	// together: a state is allocated per request scope, and scattered they
	// padded it into the next size class.
	shutdownOnce sync.Once
	shutdownCh   chan struct{}
	shutdownErr  error
}

// registry is a scope's committed registrations. It is immutable once
// stored: freeze builds the next one from copies and swaps it in whole.
type registry struct {
	index  map[key]*binding
	groups map[key][]*binding
	all    []*binding // every binding, in registration order
	eager  []*binding // derived by deriveEager: what Start builds
}

// emptyRegistry is what a scope starts with. Sharing it is safe because no
// registry is ever written to.
var emptyRegistry = &registry{index: map[key]*binding{}, groups: map[key][]*binding{}}

// freeze commits the pending registrations. The batch is validated against a
// copy of the registry and committed only if it passes, so a rejected
// registration leaves the scope as it was and is rejected identically on
// every later attempt.
func (st *state) freeze() {
	if !st.hasPending.Load() {
		return // the warm path: nothing queued, so nothing to lock for
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.pending) == 0 {
		return
	}

	cur := st.reg.Load()
	var replaced []*binding // chains an Override replaces, whose marks go on commit
	index := maps.Clone(cur.index)
	groups := maps.Clone(cur.groups)
	all := slices.Clone(cur.all)
	for _, b := range st.pending {
		// A wrapper is built where what it wraps is built, so it takes that
		// lifetime, read here rather than at registration because the
		// wrapped binding's own Scoped() may come later in the batch.
		if b.inner != nil && b.inner.scoped {
			b.scoped = true
		}
		b.validate()
		if b.group {
			groups[b.key] = append(slices.Clone(groups[b.key]), b)
		} else {
			prev, ok := index[b.key]
			act := "overridden"
			if b.inner != nil {
				act = "wrapped"
			}
			switch {
			case ok && !b.override && b.inner == nil:
				// A replacement is a thing a caller declares; a later
				// registration winning silently would let one module reroute
				// another's wiring.
				panic(fmt.Sprintf("di: %s is provided at %s and again at %s: a second registration of a key must be marked Override() to replace the first",
					b.key, prev.where(), b.where()))
			case !ok && b.override:
				// Nearly always a fake for a service that was renamed, which
				// would otherwise be a registration nobody resolves. A child
				// shadows its parent without Override: that is a different
				// registry, not a replacement.
				panic(fmt.Sprintf("di: %s (provided at %s) is marked Override() but nothing in scope %s provides it; a child scope shadows its parent without Override",
					b.key, b.where(), st.name))
			}
			if ok {
				if why := prev.against(b); why != "" {
					panic(fmt.Sprintf("di: %s (provided at %s) cannot be %s at %s: %s",
						b.key, prev.where(), act, b.where(), why))
				}
			}
			if st.served[b.key] {
				// This scope already handed the key down from an outer scope;
				// shadowing it now would give the key two live values here.
				panic(fmt.Sprintf("di: %s cannot be registered at %s: this scope has already resolved it from an outer scope",
					b.key, b.where()))
			}
			if ok && b.inner == nil {
				replaced = append(replaced, prev)
			}
			index[b.key] = b
		}
		all = append(all, b)
	}
	eager := deriveEager(all, index)

	st.reg.Store(&registry{index: index, groups: groups, all: all, eager: eager})
	// The batch stands, so the chains its Overrides replaced will never
	// serve from this scope: none of them has served, or the guard above
	// would have refused. Every link registered here is retired, down to the
	// first that wraps an ancestor's registration, whose own chain is intact.
	// A retired link releases what it wraps only once nothing live wraps it:
	// a descendant's wrapper over a middle link still composes over the rest,
	// and that link lets go when the descendant stops. Not before the commit,
	// since a rejected batch keeps every mark it made.
	for _, prev := range replaced {
		for r := prev; r.inner != nil; r = r.inner {
			r.retire() // only a wrapper's flag is ever read, so a plain registration is not marked
			if r.innerAt != st {
				break
			}
		}
		prev.release()
	}
	st.pending, st.frozen = nil, true
	st.hasPending.Store(false)
}

// deriveEager returns the ordered set of bindings Start builds, and is the
// one place that decides what Eager means. For every key with an Eager
// registration, the set holds the binding that serves that key, once, at the
// position of the first such registration. A group member is its own entry. A
// binding with a per-scope lifetime cannot honour eagerness and is rejected
// here, whether declared so directly or arriving through an override.
func deriveEager(all []*binding, index map[key]*binding) []*binding {
	var eager []*binding
	seen := make(map[*binding]bool, len(all))
	for _, b := range all {
		if !b.eager {
			continue
		}
		w := b
		if !b.group {
			w = index[b.key] // whichever registration owns the key by now
		}
		if seen[w] {
			continue
		}
		if w.scoped {
			// b itself is caught by validate, so w is an override here.
			panic(fmt.Sprintf("di: %s is Eager (provided at %s), but the Scoped registration at %s owns the key: eagerness cannot transfer to a per-scope lifetime",
				b.key, b.where(), w.where()))
		}
		seen[w] = true
		eager = append(eager, w)
	}
	return eager
}

// descendsFrom reports whether st is anc or a scope under it.
func (st *state) descendsFrom(anc *state) bool {
	for ; st != nil; st = st.parent {
		if st == anc {
			return true
		}
	}
	return false
}

// isStopped reports whether this scope or an ancestor has stopped.
func (st *state) isStopped() bool {
	for ; st != nil; st = st.parent {
		if st.stopped.Load() {
			return true
		}
	}
	return false
}

// runContext walks up the scope chain to the nearest state that Start was
// called on. running reports whether that Start has passed its hook phase,
// which is when bindings built later must start themselves; it is never true
// with a nil ctx, since start records the context before setting the flag.
func (st *state) runContext() (ctx context.Context, running bool) {
	for ; st != nil; st = st.parent {
		if p := st.startCtx.Load(); p != nil {
			return *p, st.running.Load()
		}
	}
	return nil, false
}

// everStarted reports whether Start was called on this scope or an ancestor.
func (st *state) everStarted() bool {
	ctx, _ := st.runContext()
	return ctx != nil
}

// stopContext returns the context Stop was called with, or a background one
// if the scope was stopped without recording it.
func (st *state) stopContext() context.Context {
	for ; st != nil; st = st.parent {
		st.mu.Lock()
		ctx := st.stopCtx
		st.mu.Unlock()
		if ctx != nil {
			return ctx
		}
	}
	return context.Background()
}

// instanceFor picks the instance a resolution uses. A singleton has one for
// the whole binding; a Scoped binding has one per scope that holds it.
func (st *state) instanceFor(b *binding) *instance {
	if !b.scoped {
		return b.single
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	in := st.scoped[b]
	if in == nil {
		in = &instance{b: b}
		st.scoped[b] = in
	}
	return in
}

// instanceAt is instanceFor without the making: the instance b already has
// in st, or nil. Called with st's mutex held, by the callers that must not
// bring one into being -- a recorded edge, and the inspection API.
func (st *state) instanceAt(b *binding) *instance {
	if !b.scoped {
		return b.single
	}
	return st.scoped[b]
}
