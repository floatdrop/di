// Package di is a dependency-injection container for Go 1.27+ built on
// generic methods.
//
// Services are registered on a [Scope] and resolved from it by type:
//
//	app := di.New()
//	app.Value(Config{DSN: "postgres://localhost/app"})
//	app.Provide(func(s *di.Scope) *DB { return s.Must(sql.Open("postgres", s.Get[Config]().DSN)) }).
//		OnStop(func(ctx context.Context, db *DB) error { return db.Close() })
//	app.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} })
//
//	repo, err := app.Resolve[*Repo]()
//
// Keys are Go types, so there is no naming scheme and no collisions between
// packages. Constructors return T rather than (T, error): inside a
// constructor, [Scope.Get] and [Scope.Must] abort on failure and the error
// surfaces from the enclosing [Scope.Resolve], [Scope.Start] or [Scope.Run]
// with the dependency path and the registration site.
//
// # Lifetimes
//
// A binding is a singleton by default, cached in the scope that registered
// it. [Binding.Scoped] makes it one instance per resolving scope, built
// there so it can see that scope's values, which is how request-scoped
// services are declared once in the root. [Binding.Transient] builds an
// untracked new instance on every resolution. [Scope.Add] and [Scope.All] handle groups,
// [Scope.Bind] aliases an interface to an implementation, and
// [Scope.Maybe] resolves optional dependencies.
//
// # Scopes
//
// [Scope.Child] creates a scope that resolves through its parent, reuses
// the parent's singletons and owns the lifecycle of what it builds. The
// last registration of a key wins, which is the test seam: wire the
// production graph into a fresh scope, then re-register what you want faked
// before anything is resolved ([Test] does the bookkeeping). For HTTP,
// [Scope.Middleware] gives each request a child scope holding the
// *http.Request, reachable through [FromContext].
//
// # Lifecycle
//
// [Binding.OnStart], [Binding.OnDrain] and [Binding.OnStop] are typed hooks.
// [Scope.Start] builds [Binding.Eager] bindings and runs start hooks in build
// order, rolling back on failure; services built later start as part of being
// built. [Scope.Stop] first drains, which lets work already in flight finish
// while the scope still resolves, then stops child scopes, then services in
// reverse build order, and afterwards the scope refuses to resolve anything.
// [Binding.Run] runs a long-lived function that is cancelled on stop, and
// [Binding.Health] feeds [Scope.HealthCheck]. [Scope.Run] ties it together
// for a main function: start, wait for a signal or [Scope.Shutdown], stop
// with a deadline. [Scope.Observe] reports every step for logging and
// metrics.
//
// # Concurrency
//
// A [Scope] is safe to use from many goroutines, including from goroutines a
// constructor starts for itself: the resolution path is immutable, so
// branches that run in parallel share nothing. A constructor may also keep
// the Scope it was handed and resolve through it later, once its own service
// is built; the finished part of that path is no longer a dependency, so such
// a resolution is not a cycle. A service is built once however many
// resolutions race for it, and a resolution of a running scope returns only a
// service whose start step has finished. A cycle is reported as [ErrCycle]
// even when the two halves are being built concurrently.
//
// Three re-entrancy limits apply. A goroutine started by a constructor must
// use [Scope.Resolve] rather than [Scope.Get], because Get reports failure by
// panicking and that panic cannot unwind to the enclosing call from another
// goroutine. An [Binding.OnStart] hook must not resolve a service that
// depends on the one being started: the hook already holds the value, and
// waiting for itself cannot make progress. And no hook may call [Scope.Stop]
// on its own scope or an ancestor, because Stop waits for the very step the
// hook is running; call [Scope.Shutdown], which never blocks.
package di

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---- keys ------------------------------------------------------------------

// Key identifies a service: its Go type plus an optional name. Keys compare by
// reflect.Type identity, so same-named types in different packages never
// collide (unlike fmt.Sprintf("%T")-derived names).
type Key[T any] struct{ name string }

func Named[T any](name string) Key[T] { return Key[T]{name: name} }

type key struct {
	t    reflect.Type
	name string
}

func (k key) String() string {
	if k.name == "" {
		return typeName(k.t)
	}
	return typeName(k.t) + "#" + k.name
}

func typeName(t reflect.Type) string {
	if t.Kind() == reflect.Pointer {
		return "*" + typeName(t.Elem())
	}
	if t.PkgPath() != "" {
		return t.PkgPath() + "." + t.Name()
	}
	return t.String()
}

// ---- errors ----------------------------------------------------------------

var (
	ErrNotProvided = errors.New("not provided")
	ErrCycle       = errors.New("dependency cycle")
	ErrUnhealthy   = errors.New("unhealthy")
	ErrStopped     = errors.New("scope stopped")
)

// ---- observability ---------------------------------------------------------

// EventKind classifies an Event.
type EventKind string

const (
	EventBuild    EventKind = "build"    // a constructor ran
	EventStart    EventKind = "start"    // an OnStart hook ran
	EventDrain    EventKind = "drain"    // an OnDrain hook ran
	EventStop     EventKind = "stop"     // a Run hook was cancelled and/or an OnStop hook ran
	EventHealth   EventKind = "health"   // a Health hook ran
	EventShutdown EventKind = "shutdown" // Shutdown was called
)

// Event describes one lifecycle step. Observers receive it after the step
// completes, with its duration and error if any.
type Event struct {
	Kind     EventKind
	Service  string // the service, e.g. "*github.com/acme/app.DB" or "...DB#replica"; empty for shutdown
	Scope    string // name of the scope that owns the instance
	Site     string // file:line of the registration; empty for shutdown
	Duration time.Duration
	Err      error
}

// SlogObserver returns an observer that logs every event to l: failures at
// Error level, everything else at Debug.
func SlogObserver(l *slog.Logger) func(Event) {
	return func(ev Event) {
		attrs := []any{"kind", string(ev.Kind), "scope", ev.Scope}
		if ev.Service != "" {
			attrs = append(attrs, "service", ev.Service)
		}
		if ev.Duration > 0 {
			attrs = append(attrs, "duration", ev.Duration)
		}
		if ev.Err != nil {
			l.Error("di: "+string(ev.Kind)+" failed", append(attrs, "err", ev.Err)...)
			return
		}
		l.Debug("di: "+string(ev.Kind), attrs...)
	}
}

type abort struct{ err error }

// ---- state -----------------------------------------------------------------

type binding struct {
	key       key
	site      string
	group     bool
	transient bool
	scoped    bool
	eager     bool
	isValue   bool // registered with Value: lifetimes do not apply
	alias     *key // Bind: serve this key from another binding, lifetime included
	build     func(*Scope) any
	onStart   func(context.Context, any) error
	onDrain   func(context.Context, any) error
	onStop    func(context.Context, any) error
	run       func(context.Context, any) error
	health    func(context.Context, any) error

	// used is set once this binding has served a value. From then on the
	// registration cannot be overridden, since that would leave two live
	// instances of one service. A failed resolution built nothing and leaves
	// the key re-registerable; that is how a key whose constructor failed is
	// recovered.
	used atomic.Bool

	single *instance // the singleton; scoped bindings keep one instance per state
}

// validate rejects lifetime and hook combinations that cannot be honoured.
// It runs at freeze, so the order the builder methods were called in does
// not matter.
func (b *binding) validate() {
	bad := func(what, why string) {
		panic(fmt.Sprintf("di: %s (provided at %s): %s %s", b.key, b.site, what, why))
	}
	hooks := b.onStart != nil || b.onDrain != nil || b.onStop != nil || b.run != nil || b.health != nil
	switch {
	case b.transient && b.scoped:
		bad("Transient and Scoped", "are mutually exclusive")
	case b.transient && hooks:
		bad("lifecycle hooks", "do not apply to a Transient binding: its instances are not tracked")
	case b.eager && lifetimeName(b) != "":
		// Rejected even if a later registration overrides it. Whether an
		// override inherits eagerness is decided in deriveEager.
		bad("Eager", "does not apply to a "+lifetimeName(b)+" binding: it is not built once")
	case b.alias != nil && (b.transient || b.scoped || hooks):
		// Eager is allowed: it means the key exists by the time Start
		// returns, and an alias honours that by building its target.
		bad("lifetimes and lifecycle hooks", "belong on the target binding, not on a Bind alias")
	case b.isValue && lifetimeName(b) != "":
		bad(lifetimeName(b), "is meaningless for a Value binding: the instance already exists")
	}
}

// lifetimeName names a binding's per-scope lifetime, or "" for a singleton.
func lifetimeName(b *binding) string {
	switch {
	case b.scoped:
		return "Scoped"
	case b.transient:
		return "Transient"
	}
	return ""
}

// phase is an instance's position in the build/start/stop sequence. It is
// read and written only under the owning state's mutex, so deciding who
// starts or stops an instance never spans two critical sections.
type phase int8

const (
	phaseNew      phase = iota // no value yet
	phaseBuilding              // a resolution has claimed the build step
	phaseBuilt                 // constructor ran; the start step has not
	phaseStarting              // a goroutine has claimed the start step
	phaseStarted               // the start step succeeded
	phaseFailed                // the build or the start step failed
	phaseStopped               // the stop step ran, or was skipped for good
)

// drainPhase tracks OnDrain the way phase tracks the build and start steps.
// A drain in progress has to be waited for, so a waiter needs to tell it from
// one that has finished; a bare "decided" flag let a concurrent Stop skip the
// hook and run OnStop while the hook still held the value.
type drainPhase int8

const (
	drainNone drainPhase = iota // OnDrain has not been considered
	draining                    // a Stop is running OnDrain now
	drained                     // OnDrain ran, or was skipped for good
)

// instance is one built value of a binding, owned by the state that stops it.
type instance struct {
	b     *binding
	ph    phase // guarded by the owning state's mutex
	value any
	err   error // guarded by the owning state's mutex
	// settled is set when the build step has finished and value and err are
	// final.
	settled bool       // guarded by the owning state's mutex
	dr      drainPhase // guarded by the owning state's mutex

	// Each step another goroutine may have to wait for has a channel that is
	// closed when the step is done, so a waiter blocks on that step alone.
	// The first goroutine that has to wait makes the channel (waitOn); the
	// owner of the step closes it if it exists (wake). Both happen under the
	// owning state's mutex, in the same critical section as the phase change,
	// so either order is safe: a waiter that arrives first is released by the
	// close, and an owner that finishes first leaves nil behind, in which case
	// the phase already says the step is done and the waiter never blocks.
	//
	// Nil is the normal state. An uncontended build, an unraced start step
	// and an undisputed drain allocate nothing, and a Transient instance,
	// which is never cached, gets no channel at all. Never block on one of
	// these fields directly, since a receive from a nil channel blocks for
	// ever; go through waitOn.
	settledCh  chan struct{} // closed by settle: value and err are final
	startingCh chan struct{} // closed when the start step is no longer in flight
	drainedCh  chan struct{} // closed when OnDrain has finished

	// builder is the resolution running the build step, guarded by the
	// container graph's mutex. It is the edge that makes a cycle between
	// concurrent builds visible.
	builder *resolver

	// Run hook bookkeeping, guarded by the phase machine rather than a mutex.
	// cancel and runDone are written by start, on the goroutine that owns the
	// start step, and read by stop, which stopIfNeeded reaches only after the
	// start step has left phaseStarting -- in startClaimed's deferred block,
	// under the owning state's mutex, after start returned. That lock handoff
	// is the happens-before. runErr is written by the Run goroutine before it
	// closes runDone and read only after a receive from runDone.
	cancel  context.CancelFunc
	runDone chan struct{}
	runErr  error
}

// hookKey marks a context as belonging to a lifecycle hook.
type hookKey struct{}

// inHook tags the context a hook is called with, so a Stop made with that
// context can name the misuse instead of waiting for a step the caller is
// itself responsible for finishing. A hook that passes some other context
// keeps the plain bounded wait; this catches the case worth catching, which
// is the hook passing on the context it was handed.
func inHook(ctx context.Context, st *state) context.Context {
	return context.WithValue(ctx, hookKey{}, st)
}

// hookOwner returns the scope whose hook ctx belongs to, or nil.
func hookOwner(ctx context.Context) *state {
	st, _ := ctx.Value(hookKey{}).(*state)
	return st
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

// wake releases whoever is waiting for a step. A nil channel means nobody had
// to wait; see the channel fields on instance.
func wake(ch chan struct{}) {
	if ch != nil {
		close(ch)
	}
}

// waitOn returns the channel to block on for an outstanding step, making it
// on first use. Must be called with the owning state's mutex held, in the same
// critical section that read the phase, so the owner cannot close the channel
// in between.
func waitOn(ch *chan struct{}) chan struct{} {
	if *ch == nil {
		*ch = make(chan struct{})
	}
	return *ch
}

// graph is one container's wait-for graph, with two kinds of edge: an
// instance points at the resolution building it (instance.builder), and a
// blocked resolution points at the instance it waits for (blockedFor). Its
// mutex is the innermost lock: a state's mutex may be held while taking it,
// never the reverse, so the graph can be read across scopes without ordering
// state mutexes against each other.
//
// New makes one graph per container and every scope under that root shares
// it. That is as far as a cycle can reach: a resolution follows the parent
// chain, so a wait can cross scopes, but nothing joins two containers.
type graph struct {
	mu         sync.Mutex
	blockedFor map[*resolver]*instance
}

// descends reports whether n is anc or was created below it. A branch blocks
// at a leaf of its path, several nodes below the one that claimed the build it
// is holding up, so both directions of the graph are matched against whole
// paths rather than single nodes.
//
// The walk stops at a node whose resolution has returned, for the same reason
// onPath does: nothing above a finished node is waiting for what is opened
// below it later.
func descends(n, anc *resolver) bool {
	for ; n != nil; n = n.parent {
		if n == anc {
			return true
		}
		if n.done.Load() {
			return false
		}
	}
	return false
}

// wait records that r is about to wait for in, unless that would close a
// wait-for cycle by reaching, through builds that are themselves blocked, a
// build this branch is responsible for finishing. Called with the holder's
// mutex held; the check and the new edge are one critical section, so two
// branches closing a cycle at once cannot both decide to wait.
func (r *resolver) wait(g *graph, in *instance) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	seen := map[*instance]bool{in: true}
	for stack := []*instance{in}; len(stack) > 0; {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		builder := cur.builder
		if builder == nil {
			continue // nobody is building it: whoever holds it will settle it
		}
		if descends(r, builder) {
			return false // waiting on our own branch's work
		}
		for n, j := range g.blockedFor {
			if descends(n, builder) && !seen[j] {
				seen[j] = true
				stack = append(stack, j)
			}
		}
	}
	g.blockedFor[r] = in
	return true
}

func (r *resolver) unwait(g *graph) {
	g.mu.Lock()
	delete(g.blockedFor, r)
	g.mu.Unlock()
}

// claimBuild takes the build step for this resolution. Called with the
// holder's mutex held.
func (in *instance) claimBuild(holder *state, r *resolver) {
	in.ph = phaseBuilding
	g := holder.graph
	g.mu.Lock()
	in.builder = r
	g.mu.Unlock()
}

// settle publishes the outcome of the build step and wakes every resolution
// waiting for this instance.
func (in *instance) settle(holder *state) {
	holder.mu.Lock()
	in.settled = true
	g := holder.graph
	g.mu.Lock()
	in.builder = nil
	g.mu.Unlock()
	wake(in.settledCh)
	holder.mu.Unlock()
}

// fail records a build failure, which is terminal for the instance.
func (in *instance) fail(holder *state, err error) {
	holder.mu.Lock()
	in.ph, in.err = phaseFailed, err
	holder.mu.Unlock()
}

// start runs OnStart and launches the Run hook. The Run context is detached
// from ctx so the worker is cancelled by Stop, in dependency order, rather
// than the moment the application context is cancelled.
func (in *instance) start(ctx context.Context, owner *state) error {
	b := in.b
	if b.onStart != nil {
		t0 := time.Now()
		err := b.onStart(inHook(ctx, owner), in.value)
		owner.emit(Event{Kind: EventStart, Service: b.key.String(), Scope: owner.name, Site: b.site, Duration: time.Since(t0), Err: err})
		if err != nil {
			return err
		}
	}
	if b.run != nil {
		rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		in.cancel, in.runDone = cancel, make(chan struct{})
		hctx := inHook(rctx, owner)
		go func() {
			defer close(in.runDone)
			err := b.run(hctx, in.value)
			if err == nil {
				return
			}
			if rctx.Err() != nil && errors.Is(err, context.Canceled) {
				return // we cancelled it and it reported just that
			}
			// Any other error is the worker's own failure and goes to
			// Shutdown, whether or not the scope had begun stopping. Only
			// the returned error says so: rctx.Err() says whether we
			// cancelled, not why the worker failed, and a worker that fails,
			// flushes until told to stop and then reports returns after the
			// cancellation without owing anything to it.
			//
			// Wrap once and keep that value. Stop reports it and Run
			// receives it, so the two recognise one failure instead of
			// listing it twice. Stop alone is not enough: the owning scope
			// may be a child that detaches before an ancestor's Stop reaches
			// it, and whoever called that Stop may discard its result.
			in.runErr = fmt.Errorf("di: %s: %w", b.key, err)
			(&Scope{state: owner}).Shutdown(in.runErr)
		}()
	}
	return nil
}

// claim takes the start step for this goroutine, returning false if another
// one already has it or the instance is past starting.
func (in *instance) claim(owner *state) bool {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if in.ph != phaseBuilt {
		return false
	}
	in.ph = phaseStarting
	return true
}

// startClaimed runs the start step of an instance already in phaseStarting.
// The phase is settled even if the hook panics, which is what releases a Stop
// or a resolution waiting for the step. The failure is recorded on the
// instance as well as returned, so a resolution that waited reports the same
// error.
//
// A panicking hook is a failed start, converted to an error like a panicking
// constructor: only a hook that returned has started its service. Otherwise a
// caller that recovered the panic would be served a half-initialised service,
// and Stop would pair an OnStop with an OnStart that never finished.
func (in *instance) startClaimed(ctx context.Context, owner *state) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			if a, ok := rec.(abort); ok {
				err = a.err // a nested resolution failed; report that cause
			} else {
				err = fmt.Errorf("panic: %v", rec)
			}
		}
		owner.mu.Lock()
		if err == nil {
			in.ph = phaseStarted
		} else {
			in.ph = phaseFailed
			if in.err == nil {
				in.err = fmt.Errorf("di: starting %s (provided at %s): %w", in.b.key, in.b.site, err)
			}
		}
		wake(in.startingCh) // the start step is no longer in flight
		owner.mu.Unlock()
	}()
	return in.start(ctx, owner)
}

// everStarted reports whether Start was called on this scope or an ancestor.
// It is runContext's walk with the answer discarded; the two questions are
// the same search.
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

// stopIfNeeded runs the stop step once, when it is owed: when the start step
// succeeded, or when the instance was built and its OnStop has no OnStart to
// pair with, either because the binding declares none or because the scope
// was never started, so OnStop is a plain destructor. An instance whose
// OnStart failed or was skipped by a rollback is not stopped.
//
// It first waits out whichever step another goroutine is still running for
// this instance, so the release never runs against a value one of them holds:
// a start step in flight, and then a drain hook, since a drain waits for the
// start step too and the two cannot overlap. Waiting for the start step is
// what makes Stop synchronous. It is safe because a hook may not call Stop on
// its own scope or an ancestor -- that wait would be on itself -- and Stop
// rejects the case it can see rather than hanging.
//
// If ctx expires first the release is still owed, and this is the only caller
// that can make it happen: Stop took the instance off its scope's list before
// the walk began. So the deadline ends the caller's wait, not the teardown,
// which finishes on a goroutine of its own once the step returns.
// WithoutCancel keeps the context's values and drops the spent deadline, so
// that goroutine waits properly and then releases.
func (in *instance) stopIfNeeded(ctx context.Context, owner *state) error {
	// everStarted walks the parent chain, so it is answered before the
	// owner's mutex is taken rather than under it.
	paired := in.b.onStart != nil && owner.everStarted()
	for {
		owner.mu.Lock()
		step, what := in.outstanding()
		if step == nil {
			owed := in.ph == phaseStarted || (in.ph == phaseBuilt && !paired)
			in.ph = phaseStopped
			owner.mu.Unlock()
			if !owed {
				return nil
			}
			return in.stop(ctx, owner)
		}
		owner.mu.Unlock()
		select {
		case <-step:
			// One step done; the next may be outstanding now.
		case <-ctx.Done():
			go func() { _ = in.stopIfNeeded(context.WithoutCancel(ctx), owner) }()
			return fmt.Errorf("di: stopping %s: %s did not return: %w", in.b.key, what, ctx.Err())
		}
	}
}

// outstanding names the step another goroutine is running for this instance,
// with the channel that goroutine will close, or nil if the instance is
// nobody else's business. Called with the owning state's mutex held, so the
// phase and the channel are read in one critical section.
func (in *instance) outstanding() (chan struct{}, string) {
	switch {
	case in.ph == phaseStarting:
		return waitOn(&in.startingCh), "OnStart"
	case in.dr == draining:
		return waitOn(&in.drainedCh), "OnDrain"
	}
	return nil, ""
}

// drainIfNeeded runs OnDrain once, under the same predicate as stopIfNeeded:
// a service that will not be stopped has nothing to wind down. It reports
// whether this call ran or waited for the hook, so a drain pass can tell
// that it did work.
//
// A drain another Stop has begun is waited for, not skipped, or this Stop
// would go on to run OnStop while that hook still holds the value. A start
// step in flight is waited for as well, for the same reason stopIfNeeded
// waits for it.
func (in *instance) drainIfNeeded(ctx context.Context, owner *state) (bool, error) {
	b := in.b
	if b.onDrain == nil {
		return false, nil
	}
	paired := b.onStart != nil && owner.everStarted()
	for {
		owner.mu.Lock()
		if in.dr == drained {
			owner.mu.Unlock()
			return false, nil
		}
		if in.dr == draining {
			done := waitOn(&in.drainedCh)
			owner.mu.Unlock()
			select {
			case <-done:
				return true, nil
			case <-ctx.Done():
				return true, fmt.Errorf("di: draining %s: another Stop did not finish OnDrain: %w", b.key, ctx.Err())
			}
		}
		if in.ph == phaseStarting {
			// The start step is in flight on another goroutine. Wait for it:
			// a service that is starting owes a drain as soon as it has
			// started, and leaving it undecided for a later pass meant a
			// step that outlasted the phase was never drained at all.
			starting := waitOn(&in.startingCh)
			owner.mu.Unlock()
			select {
			case <-starting:
				continue
			case <-ctx.Done():
				return false, fmt.Errorf("di: draining %s: OnStart did not return: %w", b.key, ctx.Err())
			}
		}
		if owner.isStopped() {
			// The scope's own Stop has moved past draining. A sweep still
			// running in an ancestor can reach an instance built into such a
			// scope; draining it would run the hook against a scope that no
			// longer resolves, for work it can no longer take on.
			in.dr = drained
			owner.mu.Unlock()
			return false, nil
		}
		if owed := in.ph == phaseStarted || (in.ph == phaseBuilt && !paired); !owed {
			// Straight to drained, never draining, so no waiter can arrive
			// and no channel is needed.
			in.dr = drained
			owner.mu.Unlock()
			return false, nil
		}
		in.dr = draining
		owner.mu.Unlock()
		break
	}

	t0 := time.Now()
	err := b.onDrain(inHook(ctx, owner), in.value)
	owner.emit(Event{Kind: EventDrain, Service: b.key.String(), Scope: owner.name, Site: b.site, Duration: time.Since(t0), Err: err})

	owner.mu.Lock()
	in.dr = drained
	wake(in.drainedCh) // release a concurrent Stop waiting for this hook
	owner.mu.Unlock()

	if err != nil {
		return true, fmt.Errorf("di: draining %s: %w", b.key, err)
	}
	return true, nil
}

// stop cancels the Run hook, waits for it within ctx, then runs OnStop.
//
// A Run hook that outlasts ctx still holds the value, so OnStop cannot run yet
// without racing the worker. The missed deadline is reported to the caller and
// the release is finished when the worker returns, as Stop does for a start
// step in flight.
func (in *instance) stop(ctx context.Context, owner *state) error {
	b := in.b
	if in.cancel == nil && b.onStop == nil {
		return nil
	}
	t0 := time.Now()
	var errs []error
	if in.cancel != nil {
		in.cancel()
		select {
		case <-in.runDone:
			if in.runErr != nil {
				errs = append(errs, in.runErr)
			}
		case <-ctx.Done():
			err := fmt.Errorf("di: stopping %s: Run hook did not return: %w", b.key, ctx.Err())
			if b.onStop == nil {
				owner.emit(Event{Kind: EventStop, Service: b.key.String(), Scope: owner.name, Site: b.site, Duration: time.Since(t0), Err: err})
				return err
			}
			// WithoutCancel keeps the values and drops the spent deadline; the
			// caller has stopped waiting, so what is left is best-effort.
			go in.releaseAfterRun(context.WithoutCancel(ctx), owner, err)
			return err
		}
	}
	if b.onStop != nil {
		if err := b.onStop(inHook(ctx, owner), in.value); err != nil {
			errs = append(errs, fmt.Errorf("di: stopping %s: %w", b.key, err))
		}
	}
	err := errors.Join(errs...)
	owner.emit(Event{Kind: EventStop, Service: b.key.String(), Scope: owner.name, Site: b.site, Duration: time.Since(t0), Err: err})
	return err
}

// releaseAfterRun finishes a stop step whose Run hook outlasted Stop's
// context, once the hook returns. missed is what Stop returned to its caller;
// the instance's single EventStop is emitted here and carries it along with
// the release's own result, so no observer sees a service stopped twice.
func (in *instance) releaseAfterRun(ctx context.Context, owner *state, missed error) {
	<-in.runDone
	b := in.b
	t0 := time.Now()
	errs := []error{missed}
	if in.runErr != nil {
		errs = append(errs, in.runErr)
	}
	if err := b.onStop(inHook(ctx, owner), in.value); err != nil {
		errs = append(errs, fmt.Errorf("di: stopping %s: %w", b.key, err))
	}
	owner.emit(Event{Kind: EventStop, Service: b.key.String(), Scope: owner.name, Site: b.site, Duration: time.Since(t0), Err: errors.Join(errs...)})
}

// once is a teardown phase that runs at most once per scope: the first caller
// runs it, and every later or concurrent caller waits for that run, bounded by
// its own context. Stop and the scope-wide drain are both this shape.
//
// Its fields are guarded by the state's mutex, so claiming the phase and
// recording what the claim decided are one critical section, and no third
// lock joins the ordering rules.
type once struct {
	done chan struct{} // made by the claimer, closed once its run has finished
	err  error         // that run's result
}

// claim reports whether this caller owns the run. The owner must call settle
// exactly once; everyone else calls wait. claimed, if non-nil, runs under the
// mutex in the same critical section that picks the winner, for state a waiter
// must see as soon as it sees the phase claimed.
func (o *once) claim(st *state, claimed func()) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if o.done != nil {
		return false
	}
	o.done = make(chan struct{})
	if claimed != nil {
		claimed()
	}
	return true
}

// settle publishes the run's result and releases the waiters.
func (o *once) settle(st *state, err error) {
	st.mu.Lock()
	o.err = err
	st.mu.Unlock()
	close(o.done)
}

// wait blocks until the owning run has finished and reports its error, or
// reports false if the caller's context expires first. Only a caller whose
// claim returned false may wait: an unclaimed phase has no channel and would
// block until ctx expires.
func (o *once) wait(st *state, ctx context.Context) (error, bool) {
	st.mu.Lock()
	done := o.done
	st.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return nil, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return o.err, true
}

type state struct {
	name   string
	parent *state
	graph  *graph // the container's wait-for graph, shared with every other scope under the root

	mu        sync.Mutex
	pending   []*binding // registrations not yet indexed
	index     map[key]*binding
	groups    map[reflect.Type][]*binding
	frozen    bool
	all       []*binding             // every binding, in registration order
	eager     []*binding             // derived by deriveEager: what Start builds
	started   []*instance            // build order; stopped in reverse
	scoped    map[*binding]*instance // per-scope instances of Scoped bindings
	served    map[key]bool           // keys this scope resolved from an outer scope; lazily made
	children  []*state
	observers []func(Event)

	stopped  atomic.Bool     // set by Stop or a failed Start; resolution then fails with ErrStopped
	stopCtx  context.Context // the context Stop was called with
	stopOnce once            // this scope's teardown; later Stop calls wait for it
	startCtx context.Context // set by Start; read by Context()
	running  bool            // set once Start reaches the hook phase; enables late OnStart

	// drainOnce is the scope-wide drain phase, once-with-wait like stopOnce:
	// a second Stop reaching this scope waits for the first drain instead of
	// running its own or skipping past it.
	drainOnce once

	shutdownOnce sync.Once
	shutdownCh   chan struct{}
	shutdownErr  error
}

// freeze commits the pending registrations. The batch is validated against a
// copy of the registry and committed only if it passes, so a rejected
// registration leaves the scope as it was and is rejected identically on
// every later attempt.
func (st *state) freeze() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.pending) == 0 {
		return
	}

	index := maps.Clone(st.index)
	groups := maps.Clone(st.groups)
	all := slices.Clone(st.all)
	for _, b := range st.pending {
		b.validate()
		if b.group {
			groups[b.key.t] = append(slices.Clone(groups[b.key.t]), b)
		} else {
			// The later registration wins (that is how overrides work), but
			// only until the key has been resolved; replacing it afterwards
			// would leave two live instances.
			if prev, ok := index[b.key]; ok && prev.used.Load() {
				panic(fmt.Sprintf("di: %s (provided at %s) cannot be overridden at %s: it has already been resolved",
					b.key, prev.site, b.site))
			}
			if st.served[b.key] {
				// This scope already served the key from an outer scope;
				// shadowing it now would give one key two live values here.
				panic(fmt.Sprintf("di: %s cannot be registered at %s: this scope has already resolved it from an outer scope",
					b.key, b.site))
			}
			index[b.key] = b
		}
		all = append(all, b)
	}
	eager := deriveEager(all, index)

	st.index, st.groups, st.all, st.eager = index, groups, all, eager
	st.pending, st.frozen = nil, true
}

// deriveEager returns the ordered set of bindings Start builds, and is the
// one place that decides what Eager means. For every key with an Eager
// registration, the set holds the binding that serves that key, once, at the
// position of the first such registration. A group member is its own entry. A
// binding with a per-scope lifetime cannot honour eagerness and is rejected
// here, whether declared so directly or arriving through an override. An
// alias may serve an eager key; Start then builds its target.
func deriveEager(all []*binding, index map[key]*binding) []*binding {
	var eager []*binding
	seen := make(map[*binding]bool, len(all))
	for _, b := range all {
		if !b.eager {
			continue
		}
		w := b
		if !b.group {
			if w = index[b.key]; w == nil {
				continue
			}
		}
		if seen[w] {
			continue
		}
		if kind := lifetimeName(w); kind != "" {
			// b itself is caught by validate, so w is an override here.
			panic(fmt.Sprintf("di: %s is Eager (provided at %s), but the %s registration at %s owns the key: eagerness cannot transfer to a per-scope lifetime",
				b.key, b.site, kind, w.site))
		}
		seen[w] = true
		eager = append(eager, w)
	}
	return eager
}

// resolver is one node of a resolution path: what is being resolved and the
// node that needed it. The path is a linked list, not a slice, because a
// constructor may resolve from several goroutines at once; a node is never
// mutated after it is made (except done), so branches share nothing and each
// carries the whole path for cycle detection and error messages.
//
// A node is identified by binding and holder, not by key: a group member and
// a plain registration of the same type are different bindings, and one
// Scoped binding is a different node in each scope that holds an instance of
// it.
type resolver struct {
	parent *resolver
	key    key
	b      *binding // nil on the root node, which resolves nothing itself
	holder *state

	// done marks a node whose resolution has returned. The path stays whole
	// for error messages, but a finished node is no longer a dependency: a
	// constructor may keep the Scope it was handed and resolve through it
	// later, and that resolution must not meet its own finished frame and be
	// called a cycle. Written once by the resolution that owns the node, read
	// from any branch.
	done atomic.Bool
}

func (r *resolver) child(k key, b *binding, holder *state) *resolver {
	return &resolver{parent: r, key: k, b: b, holder: holder}
}

// onPath reports whether this exact binding is still being resolved further
// up the path, which is a dependency cycle within one branch.
//
// The walk stops at the first finished node rather than skipping it. Only a
// constructor that kept its Scope can put a finished node on a live path, and
// a resolution made through that Scope afterwards is a new branch: the
// ancestors above the finished node may still be building, but not for it,
// so it has only to wait for them. Counting them reported a false cycle and
// cached it on whatever was being built.
//
// The case this gives up cannot be told apart without goroutine-local state,
// as with Stop's mid-start handoff: a constructor that blocks, itself or
// through a goroutine it waits for, on a resolution made through a finished
// descendant's Scope that leads back to it. That used to be reported as a
// cycle and now deadlocks. It requires a service to reach back into its own
// unfinished construction through a Scope that escaped a nested constructor.
func (r *resolver) onPath(b *binding, holder *state) bool {
	for n := r; n != nil; n = n.parent {
		if n.done.Load() {
			return false
		}
		if n.b == b && n.holder == holder {
			return true
		}
	}
	return false
}

func (r *resolver) path() string {
	if r == nil || r.b == nil {
		return ""
	}
	return " (needed by " + r.pathStr() + ")"
}

func (r *resolver) pathStr() string {
	var parts []string
	for n := r; n != nil; n = n.parent {
		if n.b != nil {
			parts = append(parts, n.key.String())
		}
	}
	slices.Reverse(parts)
	return fmt.Sprint(parts)
}

// Scope is a container. A Scope value handed to a constructor is a view over
// the same state that carries the current resolution path.
type Scope struct {
	*state
	r *resolver
}

func New() *Scope { return &Scope{state: newState("root", nil)} }

func newState(name string, parent *state) *state {
	st := &state{
		name:       name,
		parent:     parent,
		index:      map[key]*binding{},
		groups:     map[reflect.Type][]*binding{},
		scoped:     map[*binding]*instance{},
		shutdownCh: make(chan struct{}),
	}
	// One graph per container, shared by every scope under the root.
	if parent != nil {
		st.graph = parent.graph
	} else {
		st.graph = &graph{blockedFor: map[*resolver]*instance{}}
	}
	return st
}

// Child creates a scope that resolves through s. Stopping s stops its
// children first.
//
// A child made inside a constructor carries that constructor's resolution
// path, so a cycle through it is reported rather than deadlocking. The path
// goes inert when the constructor returns (see resolver.done), so a child kept
// for later, such as a request scope, resolves as an independent branch.
func (s *Scope) Child(name string) *Scope {
	st := newState(name, s.state)
	s.mu.Lock()
	s.children = append(s.children, st)
	s.mu.Unlock()
	return &Scope{state: st, r: s.r}
}

func (s *Scope) view(r *resolver) *Scope { return &Scope{state: s.state, r: r} }

// Observe registers fn to receive lifecycle events from this scope and every
// scope under it. Use it for logging and metrics; see SlogObserver.
func (s *Scope) Observe(fn func(Event)) {
	s.mu.Lock()
	s.observers = append(s.observers, fn)
	s.mu.Unlock()
}

// emit delivers ev to the observers of st and its ancestors.
func (st *state) emit(ev Event) {
	for ; st != nil; st = st.parent {
		st.mu.Lock()
		obs := slices.Clone(st.observers)
		st.mu.Unlock()
		for _, fn := range obs {
			fn(ev)
		}
	}
}

func callsite() string {
	_, file, line, _ := runtime.Caller(3)
	return fmt.Sprintf("%s:%d", file, line)
}

// ---- registration (generic methods) ---------------------------------------

// Binding is the typed handle returned by Provide/Value/Bind/Add. Its
// methods refine the registration; they must be called before the first
// resolution from this scope.
type Binding[T any] struct {
	s *Scope
	b *binding
}

func (s *Scope) register(k key, build func(*Scope) any) *binding {
	b := &binding{key: k, site: callsite(), build: build}
	b.single = &instance{b: b}
	s.mu.Lock()
	s.pending = append(s.pending, b)
	s.mu.Unlock()
	return b
}

// Provide registers a lazily built singleton. T is inferred from the
// constructor's return type; dependencies are pulled with s.Get[...]().
func (s *Scope) Provide[T any](ctor func(*Scope) T) Binding[T] {
	return Binding[T]{s, s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any { return ctor(s) })}
}

// Value registers an already-built instance.
func (s *Scope) Value[T any](v T) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(*Scope) any { return v })
	b.isValue = true
	return Binding[T]{s, b}
}

// Bind registers an alias: requests for I are served by T's binding, with
// T's lifetime, instance and hooks. Both parameters are explicit:
// s.Bind[Reader, *Repo](). Lifetimes and lifecycle hooks belong on the
// target binding, not on the alias.
func (s *Scope) Bind[I, T any]() Binding[I] {
	it, tt := reflect.TypeFor[I](), reflect.TypeFor[T]()
	if it.Kind() != reflect.Interface {
		panic(fmt.Sprintf("di: Bind's first type parameter must be an interface, got %s", typeName(it)))
	}
	if !tt.Implements(it) {
		panic(fmt.Sprintf("di: %s does not implement %s", typeName(tt), typeName(it)))
	}
	b := s.register(key{t: it}, nil)
	target := key{t: tt}
	b.alias = &target
	return Binding[I]{s, b}
}

// Add appends to the multi-binding group for T; read back with s.All[T]().
func (s *Scope) Add[T any](ctor func(*Scope) T) Binding[T] {
	b := s.register(key{t: reflect.TypeFor[T]()}, func(s *Scope) any { return ctor(s) })
	b.group = true
	return Binding[T]{s, b}
}

func (b Binding[T]) edit(f func(*binding)) Binding[T] {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	if b.s.frozen && !slices.Contains(b.s.pending, b.b) {
		panic(fmt.Sprintf("di: %s (provided at %s) modified after the scope was first resolved", b.b.key, b.b.site))
	}
	f(b.b)
	return b
}

func (b Binding[T]) Named(name string) Binding[T] {
	return b.edit(func(b *binding) { b.key.name = name })
}

// Transient builds a new instance on every resolution, in the scope that
// resolves it. Instances are not tracked, so lifecycle hooks and Eager are
// rejected on a transient binding: it must own its own cleanup.
func (b Binding[T]) Transient() Binding[T] {
	return b.edit(func(b *binding) { b.transient = true })
}

// Scoped makes the binding one-per-scope: each scope that resolves it gets
// its own instance, built in that scope (so it can see that scope's
// values) and stopped with it. Declare request-scoped services once in the
// root and resolve them through the request scope.
func (b Binding[T]) Scoped() Binding[T] {
	return b.edit(func(b *binding) { b.scoped = true })
}

// Eager builds the service during Start rather than on first use.
//
// Eagerness belongs to the key, not the registration: it means the service
// exists by the time Start returns. Overriding an eager binding therefore
// keeps the key eager and builds the replacement; a replacement with a
// per-scope lifetime, which cannot be built once at Start, is rejected.
func (b Binding[T]) Eager() Binding[T] { return b.edit(func(b *binding) { b.eager = true }) }

// Typed lifecycle hooks: no interface sniffing, no reflection.
//
// OnStart runs once the service is built, and only a hook that returns
// normally starts it: one that panics fails the start step, like a panicking
// constructor, and the service is never served.
func (b Binding[T]) OnStart(f func(context.Context, T) error) Binding[T] {
	return b.edit(func(b *binding) { b.onStart = func(ctx context.Context, v any) error { return f(ctx, as[T](v)) } })
}

// OnDrain runs before anything is stopped: Stop drains the whole tree, from
// the innermost scope outwards and in reverse build order, while every scope
// still resolves normally. It is where a service stops accepting new work and
// waits for the work it already has, such as an HTTP server that must finish
// in-flight requests whose handlers still need their request scope. Anything
// those handlers build, including a request scope of their own, is drained
// before the phase ends. Use OnStop for the release that follows.
func (b Binding[T]) OnDrain(f func(context.Context, T) error) Binding[T] {
	return b.edit(func(b *binding) { b.onDrain = func(ctx context.Context, v any) error { return f(ctx, as[T](v)) } })
}
func (b Binding[T]) OnStop(f func(context.Context, T) error) Binding[T] {
	return b.edit(func(b *binding) { b.onStop = func(ctx context.Context, v any) error { return f(ctx, as[T](v)) } })
}

// Run registers a long-running function for T, such as a worker loop. It is
// started in its own goroutine once the service starts and its context is
// cancelled when the service stops; Stop waits for it to return, bounded by
// its own context. A hook that outlasts that deadline is reported by Stop,
// and OnStop then waits for it rather than releasing the value underneath a
// worker still reading it.
//
// Returning a non-nil error calls Shutdown with it, stopping the application,
// even if the scope was already stopping: a worker may fail, flush while the
// scope winds down, and only then report. The exception is context.Canceled
// from a hook that was already cancelled, which is a worker reporting the
// cancellation and nothing else. A hook that wants to stay quiet during
// shutdown should return nil.
func (b Binding[T]) Run(f func(context.Context, T) error) Binding[T] {
	return b.edit(func(b *binding) { b.run = func(ctx context.Context, v any) error { return f(ctx, as[T](v)) } })
}

// Health registers a health check for T, run by HealthCheck once the
// service has been built.
func (b Binding[T]) Health(f func(context.Context, T) error) Binding[T] {
	return b.edit(func(b *binding) { b.health = func(ctx context.Context, v any) error { return f(ctx, as[T](v)) } })
}

// ---- resolution ------------------------------------------------------------

// as unwraps a stored value. A nil interface is a legitimate service, and a
// nil any cannot be asserted back to the interface type it was stored as, so
// it becomes T's zero value rather than a panic. Every hand-back of a stored
// value goes through here.
func as[T any](v any) T {
	if v == nil {
		var zero T
		return zero
	}
	return v.(T)
}

// found is what lookup located for a key: the binding that serves it, the
// scope that owns that binding, the key it is registered under, and the alias
// keys traversed on the way. An alias binding is never reported, so resolve
// cannot receive one.
type found struct {
	b       *binding
	owner   *state
	at      key   // the key b is registered under, after following aliases
	aliases []hop // alias bindings traversed: their keys are the route
	cycle   bool  // the alias chain looped back on itself
}

// hop is one alias on the route to a binding, with the scope that owns it; an
// alias resolved from an outer scope is marked served the same way its target
// is.
type hop struct {
	b     *binding
	owner *state
}

// lookupKey finds the binding registered for k in this scope or an ancestor,
// without following aliases.
func (s *Scope) lookupKey(k key) (*binding, *state) {
	for st := s.state; st != nil; st = st.parent {
		st.freeze()
		st.mu.Lock()
		b, ok := st.index[k]
		st.mu.Unlock()
		if ok {
			return b, st
		}
	}
	return nil, nil
}

// lookup resolves k through any Bind aliases to the binding that serves it.
// A nil b means either nothing is registered for at, or cycle is set.
func (s *Scope) lookup(k key) found {
	f := found{at: k}
	for {
		b, st := s.lookupKey(f.at)
		if b == nil {
			return f
		}
		if b.alias == nil {
			f.b, f.owner = b, st
			return f
		}
		f.aliases = append(f.aliases, hop{b, st})
		next := *b.alias
		if slices.ContainsFunc(f.aliases, func(a hop) bool { return a.b.key == next }) {
			f.cycle, f.at = true, next
			return f
		}
		f.at = next
	}
}

// inFlight reports whether this Scope is a live view of a resolution: it
// carries a path whose last node has not returned. A Scope kept past that
// point, by a constructor or by a Child made in one, is not in flight, and
// calls through it are top-level calls.
func (s *Scope) inFlight() bool { return s.r != nil && !s.r.done.Load() }

// enter returns a view carrying a resolver, starting a new resolution unless
// one is in flight.
func (s *Scope) enter() *Scope {
	if s.inFlight() {
		return s
	}
	return s.view(&resolver{})
}

// get resolves k. Outside a constructor the internal abort is converted into
// a panic carrying the plain error; inside one it unwinds to the enclosing
// Resolve/Start call.
func (s *Scope) get(k key) any {
	if !s.inFlight() {
		defer func() {
			if rec := recover(); rec != nil {
				if a, ok := rec.(abort); ok {
					panic(a.err)
				}
				panic(rec)
			}
		}()
		return s.enter().get(k)
	}
	f := s.lookup(k)

	// Alias keys join the path, so a cycle through an alias is detected and
	// the error names the whole route. A hop is identified by the alias
	// binding and the holder the route ends at, not the scope the alias was
	// registered in: one root alias to a Scoped target is a different edge in
	// each scope that holds an instance of the target, like the target's own
	// node. Keying it on the registering scope collapsed those edges into a
	// false cycle.
	hopHolder := f.owner
	if f.b != nil && (f.b.scoped || f.b.transient) {
		hopHolder = s.state
	}
	sc := s
	for _, h := range f.aliases {
		if sc.r.onPath(h.b, hopHolder) {
			panic(abort{fmt.Errorf("di: %w: %s -> %s", ErrCycle, sc.r.pathStr(), h.b.key)})
		}
		sc = sc.view(sc.r.child(h.b.key, h.b, hopHolder))
		defer sc.r.done.Store(true)
	}

	switch {
	case f.cycle:
		panic(abort{fmt.Errorf("di: %w: %s -> %s", ErrCycle, sc.r.pathStr(), f.at)})
	case f.b == nil:
		panic(abort{fmt.Errorf("di: %s: %w%s", f.at, ErrNotProvided, sc.r.path())})
	}
	v := sc.resolve(f.b, f.owner, f.at)
	// An alias counts as resolved once a value has been served through it.
	// resolve panics on failure, so a failed resolution leaves the alias
	// re-registerable and the interface can be redirected to a working
	// implementation.
	for _, h := range f.aliases {
		h.b.used.Store(true)
		s.markServed(h.owner, h.b.key)
	}
	s.markServed(f.owner, f.at)
	return v
}

// markServed records that k was served to this scope from owner, in every
// scope between the two. binding.used protects the owner; the scopes in
// between each handed out a value for k as well, and registering k in one of
// them afterwards would give the key two live values there. Alias hops are
// marked the same way.
func (s *Scope) markServed(owner *state, k key) {
	for st := s.state; st != nil && st != owner; st = st.parent {
		st.mu.Lock()
		if st.served == nil {
			st.served = make(map[key]bool, 4)
		}
		st.served[k] = true
		st.mu.Unlock()
	}
}

// resolve produces b's value for the resolving scope s, honouring the
// binding's lifetime and starting the instance when the scope is running.
func (s *Scope) resolve(b *binding, owner *state, k key) any {
	if b.alias != nil {
		// lookup never reports an alias and a group member never is one;
		// this is a bug in the container, not in the wiring.
		panic("di: internal: alias binding reached resolve")
	}
	if s.isStopped() {
		panic(abort{fmt.Errorf("di: %s: %w%s", k, ErrStopped, s.r.path())})
	}
	// The holder owns the instance's lifecycle: a singleton lives in the
	// scope that registered the binding, a scoped or transient one in the
	// scope that resolves it, so it can see that scope's values.
	holder := owner
	if b.scoped || b.transient {
		holder = s.state
	}
	if s.r.onPath(b, holder) {
		panic(abort{fmt.Errorf("di: %w: %s -> %s", ErrCycle, s.r.pathStr(), k)})
	}
	sc := s.view(s.r.child(k, b, holder))
	// The node stops being a dependency when this resolution returns, however
	// it returns. See resolver.done.
	defer sc.r.done.Store(true)

	if b.transient {
		// Nothing is cached or tracked for teardown. It still goes through
		// construct, so a failing constructor is an error and a successful
		// build is observed like any other.
		in := &instance{b: b}
		if err := sc.construct(in, holder, k); err != nil {
			panic(abort{err})
		}
		if s.isStopped() {
			// The same check await makes after its wait: the scope can stop
			// while the constructor runs, and a stopped scope must not hand
			// out a value it cannot tear down.
			panic(abort{fmt.Errorf("di: %s: %w", k, ErrStopped)})
		}
		b.used.Store(true)
		return in.value
	}

	v, err := sc.await(holder.instanceFor(b), holder, k)
	if err != nil {
		panic(abort{err})
	}
	b.used.Store(true)
	return v
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

// await returns the instance's value: this branch builds it if it gets there
// first, and otherwise waits for whoever did. It waits for the start step as
// well, so a resolution of a running scope never hands out a service whose
// OnStart is still in flight. A wait that would close a cycle between two
// concurrent builds is reported as ErrCycle rather than deadlocking.
func (s *Scope) await(in *instance, holder *state, k key) (any, error) {
	holder.mu.Lock()
	for in.ph == phaseNew || !in.settled || in.ph == phaseStarting {
		if in.ph == phaseNew {
			in.claimBuild(holder, s.r)
			holder.mu.Unlock()
			s.materialise(in, holder, k)
			holder.mu.Lock()
			continue
		}
		// The phase says which step is outstanding and so which channel to
		// block on. Both are read in this critical section, and the owner
		// closes the channel under the same mutex, so it cannot be closed
		// between the choice and the block.
		var ready chan struct{}
		if in.settled {
			ready = waitOn(&in.startingCh) // settled, so OnStart is outstanding
		} else {
			ready = waitOn(&in.settledCh)
		}
		if !s.r.wait(holder.graph, in) {
			holder.mu.Unlock()
			return nil, fmt.Errorf("di: %w: %s -> %s", ErrCycle, s.r.parent.pathStr(), k)
		}
		holder.mu.Unlock()
		<-ready
		s.r.unwait(holder.graph)
		holder.mu.Lock()
	}
	value, err := in.value, in.err
	if err == nil && s.isStopped() {
		// The scope stopped while this branch was building or waiting;
		// resolve's check was before the wait. The check is on the resolving
		// scope, which covers the holder (always that scope or an ancestor):
		// a stopped scope must refuse the request whether or not the value is
		// still alive above it.
		value, err = nil, fmt.Errorf("di: %s: %w", k, ErrStopped)
	}
	holder.mu.Unlock()
	return value, err
}

// materialise builds an instance, once. A failure is recorded on the instance
// rather than unwound, so every later resolution reports it identically, and
// the instance is settled on the way out so waiters are released whatever
// happened.
func (s *Scope) materialise(in *instance, holder *state, k key) {
	defer in.settle(holder)
	if err := s.construct(in, holder, k); err != nil {
		in.fail(holder, err)
		return
	}
	if !in.publish(holder, k) {
		return
	}
	in.startIfRunning(holder, k)
}

// construct runs the constructor, turning a panic or an abort from a nested
// resolution into an error, and reports the attempt to observers either way.
func (s *Scope) construct(in *instance, holder *state, k key) (err error) {
	b := in.b
	t0 := time.Now()
	defer func() {
		if rec := recover(); rec != nil {
			if a, ok := rec.(abort); ok {
				err = fmt.Errorf("di: building %s (provided at %s): %w", k, b.site, a.err)
			} else {
				err = fmt.Errorf("di: building %s (provided at %s): panic: %v", k, b.site, rec)
			}
		}
		holder.emit(Event{Kind: EventBuild, Service: k.String(), Scope: holder.name, Site: b.site, Duration: time.Since(t0), Err: err})
	}()
	in.value = b.build((&Scope{state: holder}).view(s.r))
	return nil
}

// publish adds the instance to its owner's stop list so it will be torn
// down. If Stop ran while the constructor was in flight its snapshot did not
// include this instance, so undo it here and report ErrStopped instead.
func (in *instance) publish(owner *state, k key) bool {
	owner.mu.Lock()
	stopped := owner.isStopped()
	in.ph = phaseBuilt
	if !stopped {
		owner.started = append(owner.started, in)
	}
	owner.mu.Unlock()
	if !stopped {
		return true
	}
	err := errors.Join(fmt.Errorf("di: %s: %w", k, ErrStopped), in.stopIfNeeded(owner.stopContext(), owner))
	owner.mu.Lock()
	in.err = err
	owner.mu.Unlock()
	return false
}

// startIfRunning runs the start step when the scope is already running.
// publish strictly precedes the read below, and Start sets running before it
// drains, so either this starts the instance or Start's drain finds it.
// startClaimed records its own failure on the instance, so a resolution that
// waited for the step reports it too.
func (in *instance) startIfRunning(owner *state, k key) {
	if sctx, running := owner.runContext(); running && in.claim(owner) {
		if sctx == nil {
			sctx = context.Background()
		}
		_ = in.startClaimed(sctx, owner)
	}
	stopped := owner.isStopped()
	owner.mu.Lock()
	if in.err == nil && stopped {
		// Stop ran while we were starting; it waits for the start step, so
		// the instance is torn down. Do not hand it out.
		in.err = fmt.Errorf("di: %s: %w", k, ErrStopped)
	}
	owner.mu.Unlock()
}

func (st *state) isStopped() bool {
	for ; st != nil; st = st.parent {
		if st.stopped.Load() {
			return true
		}
	}
	return false
}

// Get resolves T. Inside a constructor, failure unwinds to the enclosing
// Resolve/Start call and becomes an error; at top level it panics. In a
// goroutine a constructor started, use Resolve instead: that panic has no
// enclosing call to unwind to and would take the process down.
func (s *Scope) Get[T any]() T { return as[T](s.get(key{t: reflect.TypeFor[T]()})) }

// Lookup resolves a named key: s.Lookup(di.Named[*DB]("replica")).
func (s *Scope) Lookup[T any](k Key[T]) T {
	return as[T](s.get(key{t: reflect.TypeFor[T](), name: k.name}))
}

// Maybe resolves T if it is provided anywhere in the scope chain.
func (s *Scope) Maybe[T any]() (T, bool) {
	// lookup follows aliases, so an alias whose target is missing, or whose
	// chain loops, reports absent rather than reporting present and then
	// failing inside Get.
	if f := s.lookup(key{t: reflect.TypeFor[T]()}); f.b == nil {
		var zero T
		return zero, false
	}
	return s.Get[T](), true
}

// All resolves the multi-binding group for T across the scope chain. Members
// are singletons (or Scoped/Transient if so marked) with the same lifecycle
// as any other binding.
func (s *Scope) All[T any]() []T {
	if !s.inFlight() {
		defer func() {
			if rec := recover(); rec != nil {
				if a, ok := rec.(abort); ok {
					panic(a.err)
				}
				panic(rec)
			}
		}()
		return s.enter().All[T]()
	}
	k := key{t: reflect.TypeFor[T]()}
	var out []T
	for st := s.state; st != nil; st = st.parent {
		st.freeze()
		st.mu.Lock()
		bs := slices.Clone(st.groups[k.t])
		st.mu.Unlock()
		for _, b := range bs {
			out = append(out, as[T](s.resolve(b, st, k)))
		}
	}
	return out
}

// Must unwraps a (value, error) pair inside a constructor:
//
//	db := s.Must(sql.Open("postgres", dsn))
//
// A non-nil error aborts the constructor and surfaces from the enclosing
// Resolve, Start or Run. Outside a constructor it panics with the error.
func (s *Scope) Must[T any](v T, err error) T {
	if err == nil {
		return v
	}
	if s.inFlight() {
		panic(abort{err})
	}
	panic(err)
}

// Context returns the context passed to Start (or Run) on this scope or the
// nearest started ancestor, so constructors can dial with a deadline. Before
// Start it returns context.Background().
func (s *Scope) Context() context.Context {
	if ctx, _ := s.runContext(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// runContext walks up the scope chain to the nearest state that Start was
// called on. running reports whether that Start has passed its hook phase,
// which is when bindings built later must start themselves.
func (st *state) runContext() (ctx context.Context, running bool) {
	for ; st != nil; st = st.parent {
		st.mu.Lock()
		ctx, running = st.startCtx, st.running
		st.mu.Unlock()
		if ctx != nil {
			return ctx, running
		}
	}
	return nil, false
}

// Resolve is the error-returning entry point.
func (s *Scope) Resolve[T any]() (v T, err error) {
	defer recoverAbort(&err)
	return as[T](s.enter().get(key{t: reflect.TypeFor[T]()})), nil
}

func recoverAbort(err *error) {
	if rec := recover(); rec != nil {
		if a, ok := rec.(abort); ok {
			*err = a.err
			return
		}
		panic(rec)
	}
}

// ---- lifecycle -------------------------------------------------------------

// Start builds every Eager binding in registration order, then runs the
// start step of everything built so far, in build order. If a constructor
// or a start step fails, the scope is stopped, which rolls back exactly the
// services that did start, child scopes included. A service that was built
// but never started is not stopped, so acquire resources in OnStart rather
// than in the constructor when the binding declares one.
//
// After Start returns, a service built later runs its start step as part of
// being built, so lazily resolved services start too. Start may be called
// once, and builds only this scope's own Eager bindings: a child scope's are
// built by that child's Start.
func (s *Scope) Start(ctx context.Context) error {
	// Start has no deadline of its own for the rollback, so it detaches the
	// caller's context: an already-cancelled ctx must not skip the teardown.
	return s.start(ctx, func() (context.Context, func()) {
		return context.WithoutCancel(ctx), func() {}
	})
}

// start is Start with the rollback context supplied by the caller: Start
// detaches the caller's context, Run applies its StopTimeout and signal
// handling.
func (s *Scope) start(ctx context.Context, rollbackCtx func() (context.Context, func())) (err error) {
	defer recoverAbort(&err)
	s.freeze()
	s.mu.Lock()
	if s.startCtx != nil {
		s.mu.Unlock()
		return errors.New("di: Start called twice")
	}
	s.startCtx = ctx
	eager := slices.Clone(s.eager) // derived at freeze; clone so a later freeze cannot truncate it
	s.mu.Unlock()

	// A failing eager constructor must roll back like a failing hook.
	if err := s.buildEager(eager); err != nil {
		return errors.Join(err, s.rollback(rollbackCtx))
	}

	s.mu.Lock()
	s.running = true
	s.mu.Unlock()

	// Drain: anything built before the flag was set is still waiting here,
	// and starting one service may build more.
	for {
		in, owner := s.claimNext()
		if in == nil {
			if s.isStopped() {
				// A start hook stopped the scope; nothing is running.
				return fmt.Errorf("di: Start: %w", ErrStopped)
			}
			return nil
		}
		if err := in.startClaimed(ctx, owner); err != nil {
			err = fmt.Errorf("di: starting %s: %w", in.b.key, err)
			return errors.Join(err, s.rollback(rollbackCtx))
		}
	}
}

func (s *Scope) rollback(mk func() (context.Context, func())) error {
	ctx, cancel := mk()
	defer cancel()
	return s.Stop(ctx)
}

// buildEager builds the eager bindings, turning a constructor failure into
// an error rather than letting it unwind past Start's rollback.
func (s *Scope) buildEager(eager []*binding) (err error) {
	defer recoverAbort(&err)
	for _, b := range eager {
		if b.group {
			// A group member is not reachable by key: resolve it directly.
			s.enter().resolve(b, s.state, b.key)
			continue
		}
		// By key, so an alias that owns the key builds its target. The target
		// must be able to honour eagerness, as a direct winner must.
		if f := s.lookup(b.key); f.b != nil && lifetimeName(f.b) != "" {
			panic(fmt.Sprintf("di: %s is Eager (provided at %s), but it resolves through an alias to the %s binding at %s: eagerness cannot transfer to a per-scope lifetime",
				b.key, b.site, lifetimeName(f.b), f.b.site))
		}
		s.enter().get(b.key)
	}
	return nil
}

// claimNext claims the start step of the next built-but-unstarted instance
// in this scope or a descendant, in build order.
func (st *state) claimNext() (*instance, *state) {
	st.mu.Lock()
	for _, in := range st.started {
		if in.ph == phaseBuilt {
			in.ph = phaseStarting
			st.mu.Unlock()
			return in, st
		}
	}
	children := slices.Clone(st.children)
	st.mu.Unlock()
	for _, c := range children {
		if in, owner := c.claimNext(); in != nil {
			return in, owner
		}
	}
	return nil, nil
}

// Stop winds the scope down in three phases. First it drains: OnDrain hooks
// run from the innermost scope outwards, in reverse build order, while every
// scope still resolves, so work already in flight can finish and still reach
// its dependencies. A service or child scope that phase brings into being is
// drained too, before anything is marked stopped. Then the scope is marked
// stopped and child scopes are stopped. Then OnStop hooks run in reverse
// build order (dependents first).
//
// A service is stopped only if it started, or if it declares no OnStart, in
// which case OnStop is a plain destructor. Every failure is reported.
//
// Stop is synchronous. It waits out whatever another goroutine is still
// running for a service it is tearing down -- a start step in flight, a drain
// hook another Stop began, a Run hook being cancelled -- so when it returns,
// the teardown has happened and its failures are in the error. A teardown
// outlives the call in one case, when ctx expires first: the missed deadline
// is reported here, and the release is finished once the outstanding step
// returns, on a goroutine of its own, reaching observers rather than this
// caller. A constructor that finishes after the scope has stopped is likewise
// undone by whichever goroutine was resolving it.
//
// Afterwards the scope and its descendants refuse to resolve anything, with
// ErrStopped; that includes a resolution that was already waiting when the
// scope stopped, so a closed service is never handed out. Stopping a child
// scope also detaches it from its parent, so per-request scopes are released
// once stopped.
//
// Stop is idempotent, and concurrent calls are safe: only the first tears the
// scope down, and the others wait for it and report its result, bounded by
// their own context. Two Stop calls that meet at one scope, as a child and
// its parent do, wait for each other phase by phase, so neither starts
// releasing what the other's hooks are still using.
//
// Because Stop waits, a hook must not call Stop on its own scope or an
// ancestor: it would be waiting for the step it is itself running. Stopping a
// sibling, or a scope below the hook's own, is allowed. A hook that passes on
// the context it was given gets an error saying so; one that passes a context
// of its own is not recognised, and waits until that context expires. Call
// Shutdown, which never blocks.
func (s *Scope) Stop(ctx context.Context) error {
	// A hook of this scope, or of one under it, calling Stop here would be
	// waiting for a step it is itself running. Report it: the wait would
	// otherwise last until ctx expired, and with a background context for
	// ever. Only a hook that passed on the context it was given can be seen
	// this way, which is the shape worth catching.
	if h := hookOwner(ctx); h != nil && h.descendsFrom(s.state) {
		return fmt.Errorf("di: a lifecycle hook of scope %s called Stop on scope %s, which it is inside: call Shutdown instead", h.name, s.name)
	}
	if !s.stopOnce.claim(s.state, func() {
		if s.stopCtx == nil {
			s.stopCtx = ctx // the first Stop owns it; a later call must not clobber it
		}
	}) {
		err, finished := s.stopOnce.wait(s.state, ctx)
		if !finished {
			return fmt.Errorf("di: waiting for scope %s to stop: %w", s.name, ctx.Err())
		}
		return err // the owning teardown's result, reported to this call too
	}
	err := s.teardown(ctx)
	s.stopOnce.settle(s.state, err)
	return err
}

// teardown is the body of the first Stop.
func (s *Scope) teardown(ctx context.Context) error {
	errs := []error{s.drain(ctx)}

	s.mu.Lock()
	children := slices.Clone(s.children)
	started := s.started
	s.started = nil
	s.stopped.Store(true)
	s.mu.Unlock()

	for _, c := range children {
		errs = append(errs, (&Scope{state: c}).Stop(ctx))
	}
	errs = append(errs, stopAll(ctx, s.state, started))

	if p := s.parent; p != nil {
		p.mu.Lock()
		p.children = slices.DeleteFunc(p.children, func(c *state) bool { return c == s.state })
		p.mu.Unlock()
	}
	return errors.Join(errs...)
}

// drain runs the OnDrain hooks of this scope's subtree before anything is
// marked stopped, innermost first and in reverse build order, the order Stop
// uses. Nothing here changes an instance's phase.
//
// Only the first drain of a scope runs; a Stop that reaches the scope by
// another route waits for it. Without that wait, when a child and its parent
// are stopped at once, the second Stop would walk past a drain still in
// flight and start releasing what its hooks are using.
func (s *Scope) drain(ctx context.Context) error {
	if !s.drainOnce.claim(s.state, nil) {
		// The owner's error reaches the caller through the Stop that owns
		// the drain, which this Stop either is or waits for, so it is not
		// joined again here. Only a failure to wait belongs to this call.
		if _, finished := s.drainOnce.wait(s.state, ctx); !finished {
			return fmt.Errorf("di: waiting for scope %s to drain: %w", s.name, ctx.Err())
		}
		return nil
	}
	root := &drainScope{st: s.state, ours: true}
	r := drainRun{root: root, seen: map[*state]*drainScope{s.state: root}}
	err := r.sweepAll(ctx)
	s.drainOnce.settle(s.state, err) // this scope's phase is the last to end
	return err
}

// drainRun is the bookkeeping of one drain phase: the scopes it has reached,
// whether it owns each one's phase, and whether that phase has ended.
type drainRun struct {
	root *drainScope
	seen map[*state]*drainScope
}

type drainScope struct {
	st      *state
	ours    bool // this run claimed the phase; otherwise another Stop owns it
	settled bool // its phase has ended; for a descendant, when its own sweep does
}

// sweepAll is the body of the first drain. It sweeps the subtree until a pass
// finds no new work, because the scope still resolves during this phase: a
// hook finishing in-flight work may build a service or open a child scope,
// and those owe a drain too, before anything is marked stopped. ctx bounds
// the sweep as well as the hooks, so a hook that keeps building cannot hold
// the phase open for ever.
func (r *drainRun) sweepAll(ctx context.Context) error {
	var errs []error
	for {
		progress := false
		errs = append(errs, r.visit(ctx, r.root, &progress)...)
		if !progress || ctx.Err() != nil {
			return errors.Join(errs...)
		}
	}
}

// visit sweeps one scope this run owns and everything below it, innermost
// first and in reverse creation order, the order Stop uses. Every owned scope
// is swept on every pass, not only the ones that appeared in it, because a
// hook may build into a scope already visited.
//
// A descendant's phase is claimed just before its subtree is swept and ended
// as soon as that sweep finishes, so while a hook runs the only unended phases
// this run holds are the scope being swept and its ancestors, which a hook may
// not Stop anyway. Claiming the whole subtree up front would deadlock a hook
// that stops a scope the walk has claimed but not yet reached, such as a
// server draining in one child and stopping a request scope in another.
//
// A scope another Stop already owns is waited for and then left alone,
// subtree included; that Stop's run drains it.
func (r *drainRun) visit(ctx context.Context, ds *drainScope, progress *bool) []error {
	var errs []error
	ds.st.mu.Lock()
	children := slices.Clone(ds.st.children)
	ds.st.mu.Unlock()
	for _, c := range slices.Backward(children) {
		cs := r.seen[c]
		if cs == nil {
			*progress = true
			cs = &drainScope{st: c}
			r.seen[c] = cs
			if c.drainOnce.claim(c, nil) {
				cs.ours = true
			} else if _, finished := c.drainOnce.wait(c, ctx); !finished {
				errs = append(errs, fmt.Errorf("di: waiting for scope %s to drain: %w", c.name, ctx.Err()))
			}
		}
		if cs.ours {
			errs = append(errs, r.visit(ctx, cs, progress)...)
		}
	}
	ds.st.mu.Lock()
	started := slices.Clone(ds.st.started)
	ds.st.mu.Unlock()
	for _, in := range slices.Backward(started) {
		ran, err := in.drainIfNeeded(ctx, ds.st)
		*progress = *progress || ran
		errs = append(errs, err)
	}
	if ds != r.root && !ds.settled {
		// A descendant's failures are joined into this run's error, so its
		// waiters need only the release.
		ds.st.drainOnce.settle(ds.st, nil)
		ds.settled = true
	}
	return errs
}

func stopAll(ctx context.Context, owner *state, started []*instance) error {
	var errs []error
	for _, in := range slices.Backward(started) {
		errs = append(errs, in.stopIfNeeded(ctx, owner))
	}
	return errors.Join(errs...)
}

// HealthCheck runs every Health hook of the services built in this scope and
// its descendants, concurrently, and returns the failures joined. Each
// failure wraps ErrUnhealthy and names the service.
func (s *Scope) HealthCheck(ctx context.Context) error {
	type checked struct {
		in    *instance
		owner *state
	}
	var ins []checked
	var collect func(st *state)
	collect = func(st *state) {
		st.mu.Lock()
		for _, in := range st.started {
			if in.b.health != nil {
				ins = append(ins, checked{in, st})
			}
		}
		children := slices.Clone(st.children)
		st.mu.Unlock()
		for _, c := range children {
			collect(c)
		}
	}
	collect(s.state)

	errs := make([]error, len(ins))
	var wg sync.WaitGroup
	for i, c := range ins {
		wg.Go(func() {
			in, b := c.in, c.in.b
			t0 := time.Now()
			err := b.health(ctx, in.value)
			c.owner.emit(Event{Kind: EventHealth, Service: b.key.String(), Scope: c.owner.name, Site: b.site, Duration: time.Since(t0), Err: err})
			if err != nil {
				errs[i] = fmt.Errorf("di: %s %w: %w", b.key, ErrUnhealthy, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// ---- testing ---------------------------------------------------------------

// TB is the subset of testing.TB that Test needs.
type TB interface {
	Helper()
	Cleanup(func())
	Errorf(format string, args ...any)
}

// Test returns a scope for a test: the wire functions register the graph
// under test, and the scope is stopped when the test ends, failing it if
// a stop hook errors. Override what you need faked after wiring and before
// resolving; the last registration wins.
//
//	s := di.Test(t, app.Wire)
//	s.Value(&DB{DSN: "sqlite://memory"})
//	repo := s.Get[*Repo]()
func Test(tb TB, wire ...func(*Scope)) *Scope {
	tb.Helper()
	s := New()
	for _, w := range wire {
		w(s)
	}
	tb.Cleanup(func() {
		if err := s.Stop(context.Background()); err != nil {
			tb.Errorf("di: stopping test scope: %v", err)
		}
	})
	return s
}

// ---- request scopes --------------------------------------------------------

type ctxKey struct{}

// WithScope attaches s to ctx so handlers and their callees can reach it
// with FromContext.
func WithScope(ctx context.Context, s *Scope) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext returns the scope attached with WithScope, if any.
func FromContext(ctx context.Context) (*Scope, bool) {
	s, ok := ctx.Value(ctxKey{}).(*Scope)
	return s, ok
}

// Middleware gives every request its own child scope: the *http.Request is
// registered in it, the scope is attached to the request context, and it is
// stopped (and detached) when the handler returns.
func (s *Scope) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := s.Child("request")
		// Attach the scope, then register that same request and hand it on.
		// The handler and the constructors must see one *http.Request:
		// routers write path values and the matched pattern into the request
		// they are given, so a copy registered here would miss them.
		r = r.WithContext(WithScope(r.Context(), req))
		req.Value(r)
		// Stop failures are reported to observers as EventStop with Err set.
		defer func() { _ = req.Stop(context.WithoutCancel(r.Context())) }()
		next.ServeHTTP(w, r)
	})
}

// Shutdown asks a running Run to stop and records the cause it should return.
// It never blocks, may be called from any goroutine, and the first call wins.
// It propagates to ancestor scopes, so a service in a child scope can stop the
// application.
func (s *Scope) Shutdown(cause error) {
	first := false
	for st := s.state; st != nil; st = st.parent {
		st.shutdownOnce.Do(func() {
			st.shutdownErr = cause
			close(st.shutdownCh)
			first = first || st == s.state
		})
	}
	if first {
		s.emit(Event{Kind: EventShutdown, Scope: s.name, Err: cause})
	}
}

// RunOption configures Run.
type RunOption func(*runConfig)

type runConfig struct {
	stopTimeout time.Duration
	signals     []os.Signal
}

// StopTimeout bounds how long Stop may take once Run decides to exit.
// The default is 15 seconds.
func StopTimeout(d time.Duration) RunOption { return func(c *runConfig) { c.stopTimeout = d } }

// Signals replaces the signals that make Run exit. The default is
// os.Interrupt and SIGTERM.
func Signals(sig ...os.Signal) RunOption { return func(c *runConfig) { c.signals = sig } }

// stopContext builds the context Run stops with: detached from the caller's,
// bounded by StopTimeout, and cancelled by a second signal so a hung hook
// cannot keep the process alive. A rollback from a failed Start gets the same
// context.
func (c runConfig) stopContext(ctx context.Context) (context.Context, func()) {
	stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), c.stopTimeout)
	forceCtx, cancelForce := signal.NotifyContext(stopCtx, c.signals...)
	return forceCtx, func() { cancelForce(); cancelStop() }
}

// Run starts the scope and blocks until ctx is cancelled, a termination
// signal arrives, or Shutdown is called. It then stops the scope with a
// bounded context; a second signal during the stop cancels that context so
// a hung hook cannot keep the process alive. Run returns the Start error, the
// error passed to Shutdown, and any Stop errors, joined. A worker that died
// on its own is reported once, whether it reached Run as the cause or as a
// Stop error.
func (s *Scope) Run(ctx context.Context, opts ...RunOption) error {
	cfg := runConfig{stopTimeout: 15 * time.Second, signals: []os.Signal{os.Interrupt, syscall.SIGTERM}}
	for _, o := range opts {
		o(&cfg)
	}

	// Register before Start so a signal during a slow start is not lost.
	sigCtx, cancelSig := signal.NotifyContext(ctx, cfg.signals...)
	defer cancelSig()

	if err := s.start(ctx, func() (context.Context, func()) { return cfg.stopContext(ctx) }); err != nil {
		return err
	}

	var cause error
	select {
	case <-sigCtx.Done():
	case <-s.shutdownCh:
		cause = s.shutdownErr
	}

	stopCtx, cancel := cfg.stopContext(ctx)
	defer cancel()

	stopErr := s.Stop(stopCtx)
	if cause == nil {
		// A worker that died during the stop published its failure through
		// Shutdown after the select above had woken for a signal or a
		// cancelled ctx. Read it again: the Stop that saw the failure may have
		// been a child's, called from a drain hook that handled the error
		// itself.
		select {
		case <-s.shutdownCh:
			cause = s.shutdownErr
		default:
		}
	}
	if cause != nil && errors.Is(stopErr, cause) {
		return stopErr // the same failure, reached by both routes
	}
	return errors.Join(cause, stopErr)
}
