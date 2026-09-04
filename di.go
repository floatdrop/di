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
// Two re-entrancy limits apply. A goroutine started by a constructor must use
// [Scope.Resolve] rather than [Scope.Get], because Get reports failure by
// panicking and that panic cannot unwind to the enclosing call from another
// goroutine. An [Binding.OnStart] hook must not resolve a service that
// depends on the one being started: the hook already holds the value, and
// waiting for itself cannot make progress.
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

	// used is set once this binding has successfully served a value, after
	// which the registration can no longer be overridden: doing so would
	// leave two live instances of one service. A resolution that failed
	// built nothing, so it leaves the key re-registerable, which is the
	// only way to recover a key whose constructor failed.
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
		// Contradictory on its own terms, so rejected even if a later
		// registration overrides it. Whether an override can inherit
		// eagerness is a separate question, settled in deriveEager.
		bad("Eager", "does not apply to a "+lifetimeName(b)+" binding: it is not built once")
	case b.alias != nil && (b.transient || b.scoped || hooks):
		// Eager is allowed: it says the key exists by the time Start
		// returns, which an alias honours by building its target.
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
// read and written only under the owning state's mutex, so the decision of
// who starts or stops an instance is never split across two critical
// sections.
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

// drainPhase tracks OnDrain the way phase tracks the build and start steps,
// and for the same reason: recording only that a drain was *decided* let a
// concurrent Stop see the flag, skip the hook and run OnStop while the hook
// was still using the value. A drain that is under way has to be waited for,
// which needs a state a waiter can distinguish from a finished one.
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
	// settled reports that the build step finished, so value and err are
	// final. It replaces a sync.Once, which could only tell a second
	// resolution to carry on, never to wait for the start step as well.
	settled bool       // guarded by the owning state's mutex
	dr      drainPhase // guarded by the owning state's mutex

	// Each step another goroutine can be responsible for finishing has a
	// channel closed when it is finished, so a waiter blocks on that one step
	// rather than on every phase change in the scope. All three are read under
	// the owning state's mutex, in the same critical section as the phase that
	// says which one to wait for, and closed by the goroutine that owns the
	// step. Closing under the mutex too is what makes a lost wakeup
	// impossible: a waiter that has the channel either sees it closed or is
	// released by the close, whichever order the two happen in.
	//
	// settledCh exists from the moment the instance does, because a waiter can
	// arrive before anything has claimed the build. The other two are made by
	// the transition that starts the step they report, since until then there
	// is no step to wait for. A Transient instance has none of them: nothing
	// is ever tracked, so nothing can wait.
	settledCh  chan struct{} // closed by settle: value and err are final
	startingCh chan struct{} // closed when the start step is no longer in flight
	drainedCh  chan struct{} // closed when OnDrain has finished

	// builder is the resolution running the build step, guarded by the
	// container graph's mutex.
	// It is the edge that makes a cycle between concurrent builds visible.
	builder *resolver

	stopWanted bool            // Stop arrived mid-start; the claimer tears it down
	stopCtx    context.Context // the context of the Stop that queued the handoff

	// Run hook bookkeeping, set by start and consumed by stop.
	cancel  context.CancelFunc
	runDone chan struct{}
	runErr  error
}

// newInstance makes a tracked instance: one whose build and start steps other
// resolutions can wait for. A Transient instance is deliberately not made here
// -- it is never cached, so no second resolution can ever find it to wait on,
// and giving it the channels would allocate one per resolution for nobody.
func newInstance(b *binding) *instance {
	return &instance{b: b, settledCh: make(chan struct{})}
}

// graph is one container's wait-for graph. It has two kinds of edge: an
// instance points at the resolution building it (instance.builder), and a
// blocked resolution points at the instance it is waiting for (blockedFor).
// Its mutex is the innermost lock. A state's mutex may be held while taking
// it, never the other way round, so the graph can be read consistently across
// scopes without ever ordering two state mutexes against each other.
//
// One graph is made by [New] and shared by every scope under that root, which
// is exactly the reach a cycle can have: a wait crosses scopes, because a
// resolution follows the parent chain, but nothing joins two containers, so
// there is no edge between them to look for. A package-level graph found the
// same cycles, but every blocked resolution scanned every other container's
// edges under one process-wide lock to do it.
type graph struct {
	mu         sync.Mutex
	blockedFor map[*resolver]*instance
}

// descends reports whether n is anc or was created below it. A branch that
// blocks does so at a leaf of the path, several nodes below the one that
// claimed the build it is holding up, so both directions of the graph have
// to be read against whole paths rather than single nodes.
func descends(n, anc *resolver) bool {
	for ; n != nil; n = n.parent {
		if n == anc {
			return true
		}
	}
	return false
}

// wait records that r is about to wait for in, unless that would close a
// wait-for cycle: reaching, through builds that are themselves blocked, a
// build this branch is responsible for finishing. Reporting a cycle beats
// deadlocking on two constructors that need each other. Called with the
// holder's mutex held; the check and the edge it adds are one critical
// section, so two branches closing a cycle at the same time cannot both
// decide to wait.
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
	close(in.settledCh)
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
		err := b.onStart(ctx, in.value)
		owner.emit(Event{Kind: EventStart, Service: b.key.String(), Scope: owner.name, Site: b.site, Duration: time.Since(t0), Err: err})
		if err != nil {
			return err
		}
	}
	if b.run != nil {
		rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		in.cancel, in.runDone = cancel, make(chan struct{})
		go func() {
			defer close(in.runDone)
			err := b.run(rctx, in.value)
			if err == nil {
				return
			}
			if rctx.Err() != nil && errors.Is(err, context.Canceled) {
				return // we cancelled it and it reported just that
			}
			// Wrap once and keep that one value: Stop reports it, and a
			// worker that died on its own also hands it to Run, which
			// recognises the two as the same failure rather than listing
			// it twice. Stop cannot be relied on alone, because the scope
			// that owns the worker may be a child that detaches first.
			in.runErr = fmt.Errorf("di: %s: %w", b.key, err)
			if rctx.Err() == nil {
				(&Scope{state: owner}).Shutdown(in.runErr)
			}
		}()
	}
	return nil
}

// claimStart moves a built instance into phaseStarting and makes the channel
// that reports when that step is over, in one critical section, so a waiter
// that reads the phase always finds the channel that goes with it. Called with
// the owner's mutex held, and paired with the close in startClaimed.
func (in *instance) claimStart() {
	in.ph = phaseStarting
	in.startingCh = make(chan struct{})
}

// claim takes the start step for this goroutine, returning false if another
// one already has it or the instance is past starting.
func (in *instance) claim(owner *state) bool {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if in.ph != phaseBuilt {
		return false
	}
	in.claimStart()
	return true
}

// startClaimed runs the start step of an instance already in phaseStarting.
// The phase is settled even if the hook panics, and a Stop that arrived
// while the step was in flight is honoured here, on this goroutine. The
// failure is recorded on the instance as well as returned, so a resolution
// that waited for the step reports the same error as the one that ran it.
//
// A panicking start hook is a failed start, turned into an error the way a
// panicking constructor is: only a hook that returned normally has started
// its service. Recording success on the way past a panic left the instance
// looking started, so a caller that recovered the panic was served an
// initialisation that never finished, and Stop paired an OnStop with an
// OnStart that never completed.
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
		wanted, stopCtx := in.stopWanted, in.stopCtx
		close(in.startingCh) // the start step is no longer in flight
		owner.mu.Unlock()
		if wanted {
			if stopCtx == nil {
				stopCtx = owner.stopContext()
			}
			_ = in.stopIfNeeded(stopCtx, owner)
		}
	}()
	return in.start(ctx, owner)
}

// everStarted reports whether Start was called on this scope or an ancestor.
func (st *state) everStarted() bool {
	for ; st != nil; st = st.parent {
		st.mu.Lock()
		started := st.startCtx != nil
		st.mu.Unlock()
		if started {
			return true
		}
	}
	return false
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

// stopIfNeeded runs the stop step once, and only when it is owed. It is owed
// when the start step succeeded, and when the instance was merely built but
// its OnStop is not paired with anything: either the binding declares no
// OnStart, or the scope was never started at all, in which case OnStop is a
// plain destructor for what the constructor acquired. An instance whose
// OnStart was skipped by a rollback, or failed, is not stopped.
//
// A drain hook still running for this instance is waited for. The scope-wide
// drain phase is not enough on its own: it ends per scope, and it has to, or
// an outer scope draining an HTTP server would deadlock against a handler
// stopping its own request scope. So an instance built after its scope's
// phase ended can be drained by a sweep still running above it while its own
// Stop arrives, and only a per-instance wait keeps a release off a value its
// drain hook is still holding.
//
// A start step in flight is handed to the goroutine running it rather than
// waited for. That goroutine may be this one -- a start hook may call Stop --
// and Go offers no way to tell that case from another goroutine's start step,
// so waiting here would deadlock exactly the callers the handoff exists for.
// Stop's documentation carries the consequence: such a teardown can finish
// just after Stop returns, and its error reaches observers, not the caller.
func (in *instance) stopIfNeeded(ctx context.Context, owner *state) error {
	paired := in.b.onStart != nil && owner.everStarted()
	owner.mu.Lock()
	if in.dr == draining {
		done := in.drainedCh
		owner.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("di: stopping %s: OnDrain did not return: %w", in.b.key, ctx.Err())
		}
		owner.mu.Lock()
	}
	if in.ph == phaseStarting {
		in.stopWanted = true
		in.stopCtx = ctx // this Stop's context, not whichever is current later
		owner.mu.Unlock()
		return nil
	}
	owed := in.ph == phaseStarted || (in.ph == phaseBuilt && !paired)
	in.ph = phaseStopped
	owner.mu.Unlock()
	if !owed {
		return nil
	}
	return in.stop(ctx, owner)
}

// drainIfNeeded runs OnDrain once, when the instance is live enough to owe a
// teardown at all. It is the same predicate as stopIfNeeded's, because a
// service that will not be stopped has nothing to wind down either. It
// reports whether this call was responsible for the hook, which is how drain
// notices that a pass over the scope did some work.
//
// A drain another Stop has already begun is waited for rather than skipped:
// the flag alone would let this Stop go on to run OnStop while that hook is
// still using the value.
func (in *instance) drainIfNeeded(ctx context.Context, owner *state) (bool, error) {
	b := in.b
	if b.onDrain == nil {
		return false, nil
	}
	paired := b.onStart != nil && owner.everStarted()
	owner.mu.Lock()
	if in.dr != drainNone {
		if in.dr == drained {
			owner.mu.Unlock()
			return false, nil
		}
		done := in.drainedCh
		owner.mu.Unlock()
		select {
		case <-done:
			return true, nil
		case <-ctx.Done():
			return true, fmt.Errorf("di: draining %s: another Stop did not finish OnDrain: %w", b.key, ctx.Err())
		}
	}
	if in.ph == phaseStarting {
		// Its start step is in flight and, as in stopIfNeeded, may be on this
		// goroutine. Leave the instance undecided rather than waiting or
		// writing it off: a later sweep of this phase drains it if the step
		// settles in time.
		owner.mu.Unlock()
		return false, nil
	}
	if owner.isStopped() {
		// The scope is already being torn down, by a Stop of its own that has
		// moved past draining. Winding an instance down for work it can no
		// longer take on is the opposite of what the hook is for: it would
		// run against a scope that resolves nothing. A sweep still running in
		// an ancestor can reach an instance built into a scope in this state,
		// which is the one way the phase and the teardown meet out of order.
		in.dr = drained
		owner.mu.Unlock()
		return false, nil
	}
	owed := in.ph == phaseStarted || (in.ph == phaseBuilt && !paired)
	if !owed {
		// Straight to drained without ever being draining, so no channel is
		// needed: a waiter only ever arrives at one it saw draining.
		in.dr = drained
		owner.mu.Unlock()
		return false, nil
	}
	in.dr, in.drainedCh = draining, make(chan struct{})
	owner.mu.Unlock()

	t0 := time.Now()
	err := b.onDrain(ctx, in.value)
	owner.emit(Event{Kind: EventDrain, Service: b.key.String(), Scope: owner.name, Site: b.site, Duration: time.Since(t0), Err: err})

	owner.mu.Lock()
	in.dr = drained
	close(in.drainedCh) // release a concurrent Stop waiting for this hook
	owner.mu.Unlock()

	if err != nil {
		return true, fmt.Errorf("di: draining %s: %w", b.key, err)
	}
	return true, nil
}

// stop cancels the Run hook, waits for it within ctx, then runs OnStop.
//
// A Run hook that outlasts ctx still holds the value, so OnStop cannot run
// yet: releasing what a live worker is still reading would race that worker
// inside code that only followed the lifecycle API. The missed deadline is
// reported to the caller and the release is finished off the worker's own
// return, which is the same trade Stop makes for a start step in flight.
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
			// context.WithoutCancel keeps the values and drops the spent
			// deadline: the caller has stopped waiting, so what is left is
			// best-effort release rather than something it can still bound.
			go in.releaseAfterRun(context.WithoutCancel(ctx), owner, err)
			return err
		}
	}
	if b.onStop != nil {
		if err := b.onStop(ctx, in.value); err != nil {
			errs = append(errs, fmt.Errorf("di: stopping %s: %w", b.key, err))
		}
	}
	err := errors.Join(errs...)
	owner.emit(Event{Kind: EventStop, Service: b.key.String(), Scope: owner.name, Site: b.site, Duration: time.Since(t0), Err: err})
	return err
}

// releaseAfterRun finishes a stop step whose Run hook outlasted Stop's
// context, once that hook returns. missed is what Stop returned to its
// caller; the single EventStop for this instance is emitted here and carries
// it alongside whatever the release itself reports, so no observer sees one
// service stopped twice.
func (in *instance) releaseAfterRun(ctx context.Context, owner *state, missed error) {
	<-in.runDone
	b := in.b
	t0 := time.Now()
	errs := []error{missed}
	if in.runErr != nil {
		errs = append(errs, in.runErr)
	}
	if err := b.onStop(ctx, in.value); err != nil {
		errs = append(errs, fmt.Errorf("di: stopping %s: %w", b.key, err))
	}
	owner.emit(Event{Kind: EventStop, Service: b.key.String(), Scope: owner.name, Site: b.site, Duration: time.Since(t0), Err: errors.Join(errs...)})
}

// once is a teardown phase that runs at most once per scope: the first caller
// to reach it runs it, and every later or concurrent caller waits for that run
// instead of starting a second. Stop and the scope-wide drain are both this
// shape, and both need the wait bounded by the waiting caller's own context
// rather than the owner's.
//
// Its fields are guarded by the state's mutex, the one the scope already has,
// so claiming the phase and recording what that claim decided stay a single
// critical section. A mutex of its own would buy nothing here and would add a
// third lock to order against the other two.
type once struct {
	done chan struct{} // made by the claimer, closed once its run has finished
	err  error         // that run's result
}

// claim reports whether this caller owns the run; the owner must go on to call
// settle exactly once, and everyone else to call wait. claimed, when non-nil,
// runs with the mutex still held, in the same critical section that picks the
// winner: it is for state a waiter has to see set as soon as it can see the
// phase claimed at all.
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

// wait blocks until the owning run has finished and reports its error. It
// reports false instead if the caller's own context expires first, which is
// the one failure that belongs to the waiter rather than to the owner. Only a
// caller whose claim returned false may wait: an unclaimed phase has no
// channel to wait on and would block until ctx expires.
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

	// The drain phase gets the same treatment as the stop phase, for the
	// same reason: two Stop calls may reach one scope, and the second must
	// wait for the first scope-wide drain instead of running its own or
	// skipping past it into teardown.
	drainOnce once

	shutdownOnce sync.Once
	shutdownCh   chan struct{}
	shutdownErr  error
}

// freeze commits the pending registrations. It is transactional: the batch
// is validated against a prospective copy of the registry and only then
// committed, so a rejected registration leaves the scope exactly as it was.
// A scope that holds an invalid registration therefore keeps rejecting,
// consistently, instead of appearing to succeed on a second attempt with
// the offending entry silently dropped.
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
			// Later registration wins, which is how overrides work, but only
			// until the key has been resolved: replacing it afterwards would
			// leave two live instances of one service.
			if prev, ok := index[b.key]; ok && prev.used.Load() {
				panic(fmt.Sprintf("di: %s (provided at %s) cannot be overridden at %s: it has already been resolved",
					b.key, prev.site, b.site))
			}
			if st.served[b.key] {
				// This scope already handed out a value for the key from an
				// outer scope, so shadowing it now would give one key two
				// live values within this scope.
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

// deriveEager returns the ordered set of bindings Start builds. It is the
// only place that decides what Eager means, so the set and the rules it must
// satisfy cannot drift apart:
//
// For every key, the set holds the binding that serves the key exactly once
// if any registration for that key was marked Eager, at the position of the
// first such registration. A group member is its own entry. A binding that
// serves the key but has a per-scope lifetime cannot honour eagerness and is
// rejected here, whether that combination was declared directly or arrived
// through an override. A Bind alias may serve an eager key: Start then
// builds the target it redirects to.
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

// resolver is one node of a resolution path: what is being resolved, and the
// node that needed it. The path is a linked list rather than a slice because
// a constructor may resolve its dependencies from several goroutines at
// once; nothing here is ever mutated after the node is made, so those
// branches share no state and each still carries the whole path for cycle
// detection and error messages.
//
// A node is identified by binding and holder, not by key: a group member and
// a plain registration of the same type are different bindings and so
// different nodes, and one Scoped binding is a different node in each scope
// that holds an instance of it.
type resolver struct {
	parent *resolver
	key    key
	b      *binding // nil on the root node, which resolves nothing itself
	holder *state

	// done marks a node whose own resolution has returned. The path stays
	// immutable and stays whole, for error messages, but a finished node is
	// no longer a dependency anything is waiting on. A constructor may keep
	// the Scope it was handed -- that is how a goroutine it starts resolves
	// later -- and without this a deferred resolution through that Scope
	// would meet its own finished frame and be reported as a cycle. Written
	// once by the resolution that owns the node, read from any branch.
	done atomic.Bool
}

func (r *resolver) child(k key, b *binding, holder *state) *resolver {
	return &resolver{parent: r, key: k, b: b, holder: holder}
}

// onPath reports whether this exact binding is still being resolved further
// up the path, which is a dependency cycle within one branch.
func (r *resolver) onPath(b *binding, holder *state) bool {
	for n := r; n != nil; n = n.parent {
		if n.b == b && n.holder == holder && !n.done.Load() {
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
	// One graph per container: the root makes it and every scope under it
	// shares that one pointer, so reaching it is a field read rather than a
	// walk to the root.
	if parent != nil {
		st.graph = parent.graph
	} else {
		st.graph = &graph{blockedFor: map[*resolver]*instance{}}
	}
	return st
}

// Child creates a scope that resolves through s. Stopping s stops its
// children first.
func (s *Scope) Child(name string) *Scope {
	st := newState(name, s.state)
	s.mu.Lock()
	s.children = append(s.children, st)
	s.mu.Unlock()
	return &Scope{state: st}
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
	b.single = newInstance(b)
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
// Lifetime and lifecycle hooks belong to a registration, because they are
// typed on that particular value, but eagerness belongs to the key: it means
// the service exists by the time Start returns. So overriding an eager
// binding keeps the key eager and builds the replacement, while a
// replacement with a per-scope lifetime, which cannot be built once at
// Start, is rejected.
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
// worker still reading it. Returning a non-nil error before that calls
// Shutdown with it, stopping the application.
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

// found is what lookup located for a key: the binding that actually serves
// it, the scope that owns that binding, the key it is registered under, and
// the alias keys traversed on the way. An alias binding is never reported,
// so resolve cannot receive one.
type found struct {
	b       *binding
	owner   *state
	at      key   // the key b is registered under, after following aliases
	aliases []hop // alias bindings traversed: their keys are the route
	cycle   bool  // the alias chain looped back on itself
}

// hop is one alias on the route to a binding, with the scope that owns it.
// The owner matters: an alias resolved from an outer scope shadows the same
// way its target would, so it has to be recorded the same way.
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

// enter returns a view carrying a resolver, starting a new resolution when
// called outside a constructor.
func (s *Scope) enter() *Scope {
	if s.r != nil {
		return s
	}
	return s.view(&resolver{})
}

// get resolves k. Outside a constructor the internal abort is converted into
// a panic carrying the plain error; inside one it unwinds to the enclosing
// Resolve/Start call.
func (s *Scope) get(k key) any {
	if s.r == nil {
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

	// Alias keys join the path so a cycle through an alias is detected and
	// the error names the whole route. Skipped entirely when the key is
	// served directly, which is the common case.
	//
	// A hop is identified by the alias binding and the holder the route ends
	// at, not by the scope the alias was registered in: one root alias to a
	// Scoped target is a different edge in each scope that holds an instance
	// of that target, exactly as the target's own node is, and keying the hop
	// on the registering scope collapsed those edges into a false cycle.
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
	// An alias key counts as resolved once a value has actually been served
	// through it. resolve panics on failure, so a failed resolution leaves
	// the alias re-registerable, which is what lets a caller redirect the
	// interface to a working implementation.
	for _, h := range f.aliases {
		h.b.used.Store(true)
		s.markServed(h.owner, h.b.key)
	}
	s.markServed(f.owner, f.at)
	return v
}

// markServed records that k was served to this scope from owner, in every
// scope on the way there. binding.used protects the owner itself, but it
// cannot protect the scopes in between: each of them handed out a value for
// k, so registering k in any of them afterwards would give one key two live
// values within that scope. The route matters as much as the destination,
// which is why every alias hop is marked the same way.
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
		// lookup never reports an alias and groups never are one, so this
		// is a programming error in the container, not in the caller's wiring.
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
	// This node stops being an active dependency the moment the resolution
	// of it returns, however it returns. See resolver.done.
	defer sc.r.done.Store(true)

	if b.transient {
		// Nothing is cached, so it cannot become captive and it is never
		// tracked for teardown. It still goes through construct, so a
		// failing transient constructor is an error like any other and a
		// successful one is reported to observers like any other, and it is
		// made directly rather than by newInstance because nothing can ever
		// find it to wait on.
		in := &instance{b: b}
		if err := sc.construct(in, holder, k); err != nil {
			panic(abort{err})
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
		in = newInstance(b)
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
		// Which step is outstanding decides which channel to block on, and
		// both are read here, under the mutex, in the same critical section as
		// the phase that chose. Whoever owns the step closes it under this
		// mutex too, so it cannot close between the read and the block and
		// leave this branch waiting for a wakeup that has already happened.
		ready := in.settledCh
		if in.settled {
			ready = in.startingCh // settled, so the outstanding step is OnStart
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
		// Something stopped while we were building or waiting. resolve
		// checked on the way in, but that was before the wait: every
		// resolution needs the check, not just the one that ran the build.
		// The check is on the resolving scope, which covers the holder --
		// always that scope or an ancestor of it -- because a scope that has
		// stopped must refuse the request whether or not what it asked for
		// is still alive somewhere above.
		value, err = nil, fmt.Errorf("di: %s: %w", k, ErrStopped)
	}
	holder.mu.Unlock()
	return value, err
}

// materialise brings an instance into being, exactly once. It records any
// failure on the instance rather than unwinding, so every later resolution
// of the same instance reports it identically, and it settles the instance
// on the way out so waiting resolutions are released whatever happened.
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
	if s.r == nil {
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
	if s.r != nil {
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
	// A plain Start has no deadline of its own to give the rollback, so it
	// detaches the caller's context: an already-cancelled ctx must not skip
	// the teardown of what did start.
	return s.start(ctx, func() (context.Context, func()) {
		return context.WithoutCancel(ctx), func() {}
	})
}

// start is Start with the rollback context supplied by the caller, because
// only the caller knows what bounds it: Start detaches, Run applies the same
// StopTimeout and signal handling it would use for an ordinary exit. Both
// rollbacks go through here, so neither can be left behind.
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
		// By key, so a Bind alias that owns the key builds its target
		// rather than its own absent constructor. The target must be able
		// to honour eagerness, exactly as a direct winner must.
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
			in.claimStart()
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
// A service is stopped only if it actually started, or if it declares no
// OnStart, in which case OnStop is a plain destructor. Every failure is
// reported. Two teardowns can outlive the call, both reported to observers
// rather than returned here: a service whose start step was in flight is torn
// down by the goroutine running that step, and a service whose Run hook
// outlasts ctx is released once that hook returns, rather than while it is
// still using the value.
//
// Afterwards the scope and its descendants refuse to resolve anything, with
// ErrStopped, so a closed service can never be handed out, and so can a
// resolution that was already waiting when the scope stopped. Stopping a
// child scope also detaches it from its parent, so per-request scopes are
// released once stopped.
//
// Stop is idempotent, and concurrent calls are safe: only the first tears the
// scope down, and the others wait for it and report its result, bounded by
// their own context. Two Stop calls that meet at one scope, as a child and
// its parent do, wait for each other phase by phase, so neither starts
// releasing what the other's hooks are still using. For that reason a hook
// must not call Stop on its own scope or an ancestor, which would be a wait
// on itself; call Shutdown, which never blocks.
func (s *Scope) Stop(ctx context.Context) error {
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
// marked stopped, innermost first and in reverse build order, which is the
// order Stop itself uses. Nothing here changes an instance's phase: draining
// is a chance to finish, not a teardown.
//
// Only the first drain of a scope runs; a second one, from a Stop that
// arrived at the same scope by another route, waits for it. That wait is what
// keeps the phases apart when a child and its parent are stopped at once:
// without it the second Stop would walk past a drain still in flight and
// start releasing what those hooks are using.
func (s *Scope) drain(ctx context.Context) error {
	if !s.drainOnce.claim(s.state, nil) {
		// Whoever owns this drain reports its failures through the Stop that
		// owns it, and this Stop either is that one or goes on to wait for it,
		// so the owner's error is deliberately dropped rather than joined
		// again here. Only the failure to wait belongs to this call.
		if _, finished := s.drainOnce.wait(s.state, ctx); !finished {
			return fmt.Errorf("di: waiting for scope %s to drain: %w", s.name, ctx.Err())
		}
		return nil
	}
	r := drainRun{
		owned: []drainScope{{st: s.state}},
		seen:  map[*state]bool{s.state: true},
	}
	err := r.sweepAll(ctx)
	s.drainOnce.settle(s.state, err) // this scope's phase is the last to end
	return err
}

// drainRun is the bookkeeping of one drain phase: the scopes it took, in
// discovery order, and where each of them has got to.
type drainRun struct {
	owned []drainScope
	seen  map[*state]bool
}

type drainScope struct {
	st      *state
	settled bool // its phase has ended, which for a descendant is as soon as
	// its own sweep does
}

// sweepAll is the body of the first drain. It sweeps the whole subtree
// repeatedly rather than once, because draining is the one teardown phase
// during which the scope still resolves: a hook finishing in-flight work may
// build a service, or open a child scope, for the first time. Those owe a
// drain too, and it has to happen before anything is marked stopped, or their
// own hooks would run with nothing left to resolve.
//
// Every scope the phase owns is swept on every pass, not just the ones that
// appeared in it. Sweeping each descendant once was enough for a hook that
// builds into its own scope and not for one that builds into a scope already
// visited, which is the same defect one level along. A pass that finds no new
// work ends the phase.
//
// A descendant's phase ends the moment its own sweep does, not when the whole
// run does. Holding it open until the end would be tidier -- an ancestor's
// hook can still build into it -- and it deadlocks the case the phase exists
// for: an HTTP server draining in an outer scope waits for a handler that is
// stopping its own request scope, and that Stop would be waiting for this run
// to finish.
func (r *drainRun) sweepAll(ctx context.Context) error {
	var errs []error
	for {
		progress := false
		// Take, or wait for, the phase of every scope that has appeared under
		// one this drain owns. The loop reads r.owned as it grows, so a scope
		// discovered in this pass is descended into in the same pass.
		for i := 0; i < len(r.owned); i++ {
			st := r.owned[i].st
			st.mu.Lock()
			children := slices.Clone(st.children)
			st.mu.Unlock()
			for _, c := range children {
				if r.seen[c] {
					continue
				}
				r.seen[c] = true
				progress = true
				if c.drainOnce.claim(c, nil) {
					r.owned = append(r.owned, drainScope{st: c})
				} else if _, finished := c.drainOnce.wait(c, ctx); !finished {
					errs = append(errs, fmt.Errorf("di: waiting for scope %s to drain: %w", c.name, ctx.Err()))
				}
			}
		}
		// Innermost first, and index 0 -- this scope -- last, which is the
		// order Stop itself uses.
		for i := len(r.owned) - 1; i >= 0; i-- {
			ds := &r.owned[i]
			ds.st.mu.Lock()
			started := slices.Clone(ds.st.started)
			ds.st.mu.Unlock()
			for _, in := range slices.Backward(started) {
				ran, err := in.drainIfNeeded(ctx, ds.st)
				progress = progress || ran
				errs = append(errs, err)
			}
			if i > 0 && !ds.settled {
				// A descendant's failures are joined into this run's error,
				// so its waiters need only the release.
				ds.st.drainOnce.settle(ds.st, nil)
				ds.settled = true
			}
		}
		// ctx bounds the sweep as well as the hooks, so a hook that keeps
		// building cannot hold the phase open for ever.
		if !progress || ctx.Err() != nil {
			return errors.Join(errs...)
		}
	}
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
		// Attach the scope first, then register that same request and hand
		// it on. The handler and the constructors must see one *http.Request
		// and not two: routers write path values and the matched pattern
		// into the request they are given, so a copy registered here would
		// be missing everything the route matched.
		r = r.WithContext(WithScope(r.Context(), req))
		req.Value(r)
		// Stop failures are reported to observers as EventStop with Err set.
		defer func() { _ = req.Stop(context.WithoutCancel(r.Context())) }()
		next.ServeHTTP(w, r)
	})
}

// Shutdown asks a running Run to stop, recording why: it wakes any waiting
// Run and records the cause it should return. It never blocks, may be called
// from any goroutine, and the first call wins. The request propagates to
// ancestor scopes, so a service in a child scope can stop the application.
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

// stopContext builds the context Run stops with: bounded by StopTimeout and
// detached from the caller's, with a second signal cancelling it so a hung
// hook cannot keep the process alive. A rollback from a failed Start gets the
// same treatment, because a hook that ignores its deadline hangs a failed
// start exactly as readily as a clean shutdown.
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
	if cause != nil && errors.Is(stopErr, cause) {
		return stopErr // the same failure, reached by both routes
	}
	return errors.Join(cause, stopErr)
}
