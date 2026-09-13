package di

// The lifecycle: an instance's phase machine, the hooks that move it through
// its start, drain and stop steps, and Start and Stop, which drive that
// machine for a scope tree. Every phase is read and written under the owning
// state's mutex, and every user hook is called through callHook.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"
)

// phase is an instance's position in the build/start/stop sequence, read and
// written only under the owning state's mutex.
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

// drainPhase tracks OnDrain as phase tracks the other steps: a drain in
// progress is waited for, so it must be told from one that has finished.
type drainPhase int8

const (
	drainNone drainPhase = iota // OnDrain has not been considered
	draining                    // a Stop is running OnDrain now
	drained                     // OnDrain ran, or was skipped for good
)

// dep is one recorded dependency edge: an instance a constructor resolved,
// and the scope holding it. The holder is carried because an instance does
// not know its scope, and the scope it was resolved from may be gone when the
// edge is read.
type dep struct {
	in     *instance
	holder *state
}

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

	// ready summarises ph, err and settled for the warm path, which reads it
	// without the mutex: set, the value is final and may be returned without
	// waiting. Written only by refresh.
	ready atomic.Bool

	// deps are the services this instance's constructor resolved, in order,
	// each once. Guarded by the owning state's mutex, since a constructor may
	// resolve from several goroutines. Only Explain and Graph read them.
	deps []dep

	// Each step another goroutine may have to wait for has a channel closed
	// when the step is done. The first goroutine that has to wait makes it
	// (waitOn); the owner of the step closes it if it exists (wake). Both
	// happen under the owning state's mutex, in the critical section that
	// changes the phase, so a waiter never picks up the channel of a later
	// step and an owner that finishes first leaves nil behind. Nil is the
	// normal state: nothing is allocated unless someone waits. Never receive
	// from one of these fields directly; go through waitOn.
	settledCh  chan struct{} // closed by settle: value and err are final
	startingCh chan struct{} // closed when the start step is no longer in flight
	drainedCh  chan struct{} // closed when OnDrain has finished

	// builder is the resolution running the build step, guarded by the
	// container graph's mutex. It is the edge that makes a cycle between
	// concurrent builds visible.
	builder *resolver

	// Worker bookkeeping, ordered by the phase machine rather than a mutex:
	// cancel and runDone are written by start and read by stop, which runs
	// only after startClaimed has moved the phase past phaseStarting under
	// the owning mutex. runErr is written before runDone is closed and read
	// only after a receive from it.
	cancel  context.CancelFunc
	runDone chan struct{}
	runErr  error
}

// refresh recomputes ready: settled without an error, and built with no start
// step in flight, or started. That is the state in which await's locked loop
// returns the value at once. Called under the owning state's mutex after
// every change to ph, err or settled; a site that forgets it leaves ready
// stale, and only a stale false is harmless.
func (in *instance) refresh() {
	in.ready.Store(in.settled && in.err == nil && (in.ph == phaseBuilt || in.ph == phaseStarted))
}

// wake closes a step's channel if a waiter made one.
func wake(ch chan struct{}) {
	if ch != nil {
		close(ch)
	}
}

// waitOn returns a step's channel, making it on first use. Called under the
// owning state's mutex, in the critical section that read the phase.
func waitOn(ch *chan struct{}) chan struct{} {
	if *ch == nil {
		*ch = make(chan struct{})
	}
	return *ch
}

// once is a teardown phase that runs at most once per scope: the first caller
// runs it, and every later or concurrent caller waits for that run, bounded by
// its own context. Its fields are guarded by the state's mutex, so claiming
// the phase and recording what the claim decided are one critical section.
type once struct {
	done chan struct{} // made by the claimer, closed once its run has finished
	err  error         // that run's result
}

// claim reports whether this caller owns the run. The owner must call settle
// exactly once; everyone else calls wait. claimed, if non-nil, runs in the
// critical section that picks the winner.
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
// reports false if ctx expires first. Only a caller whose claim returned
// false may wait: an unclaimed phase has no channel.
func (o *once) wait(st *state, ctx context.Context) (finished bool, err error) {
	st.mu.Lock()
	done := o.done
	st.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return false, nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return true, o.err
}

// hookKey marks a context as belonging to a lifecycle hook.
type hookKey struct{}

// inHook tags the context a hook is called with, so a Stop made with that
// context can report the misuse instead of waiting for the step the caller is
// itself running. A hook that passes a context of its own is not seen.
func inHook(ctx context.Context, st *state) context.Context {
	return context.WithValue(ctx, hookKey{}, st)
}

// hookOwner returns the scope whose hook ctx belongs to, or nil.
func hookOwner(ctx context.Context) *state {
	st, _ := ctx.Value(hookKey{}).(*state)
	return st
}

// callHook runs a lifecycle hook and reports a panic as its error. A panic
// that escaped a hook would leave stopOnce claimed and never settled: every
// later Stop would wait for ever, and nothing behind it would be released.
func callHook(hook func(context.Context, any) error, ctx context.Context, v any) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			if a, ok := rec.(abort); ok {
				err = a.err // a nested resolution failed; report that cause
			} else {
				err = fmt.Errorf("panic: %v", rec)
			}
		}
	}()
	return hook(ctx, v)
}

// start runs OnStart and launches the worker. The worker's context is
// detached from ctx so that Stop cancels it, in dependency order, rather than
// the application context.
func (in *instance) start(ctx context.Context, owner *state) error {
	b := in.b
	if b.onStart != nil {
		t0 := time.Now()
		err := callHook(b.onStart, inHook(ctx, owner), in.value)
		owner.report(EventStart, b, t0, err)
		if err != nil {
			return err
		}
	}
	if b.worker != nil {
		rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		in.cancel, in.runDone = cancel, make(chan struct{})
		hctx := inHook(rctx, owner)
		go func() {
			defer close(in.runDone)
			// Through callHook, so a panicking worker is a failed worker
			// rather than a crash with no OnStop and no event.
			err := callHook(b.worker, hctx, in.value)
			if err == nil {
				return
			}
			if rctx.Err() != nil && onlyCancellation(err) {
				return // we cancelled it and it reported just that
			}
			// The worker's own failure goes to Shutdown even if the scope
			// was already stopping. It is wrapped once and kept, so Stop and
			// Run report one failure rather than two.
			in.runErr = fmt.Errorf("di: %s: %w", b.key, err)
			(&Scope{st: owner}).Shutdown(in.runErr)
		}()
	}
	return nil
}

// claim takes the start step for this goroutine, or reports that another one
// has it, the instance is past starting, or the scope has stopped.
func (in *instance) claim(owner *state) bool {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if in.ph != phaseBuilt || owner.isStopped() {
		return false
	}
	in.ph = phaseStarting
	in.refresh()
	return true
}

// startClaimed runs the start step of an instance in phaseStarting and settles
// the phase, releasing whoever waits for it. Only a hook that returned has
// started its service: a panic is a failed start, as for a constructor. The
// failure is recorded on the instance as well as returned, so a resolution
// that waited reports it too.
func (in *instance) startClaimed(ctx context.Context, owner *state) error {
	err := in.start(ctx, owner)
	owner.mu.Lock()
	if err == nil {
		in.ph = phaseStarted
	} else {
		in.ph = phaseFailed
		if in.err == nil {
			in.err = fmt.Errorf("di: starting %s (provided at %s): %w", in.b.key, in.b.where(), err)
		}
	}
	in.refresh()
	wake(in.startingCh)
	owner.mu.Unlock()
	return err
}

// paired reports whether the instance's OnStop has an OnStart to pair with:
// the binding declares one and the scope has been started. It walks the
// parent chain, so it is answered before the owning state's mutex is taken.
func (in *instance) paired(owner *state) bool {
	return in.b.onStart != nil && owner.everStarted()
}

// owes reports whether the instance owes its drain and stop steps: it
// started, or it was built with no start step to pair with, so OnStop is a
// plain destructor. Called with the owning state's mutex held.
func (in *instance) owes(paired bool) bool {
	return in.ph == phaseStarted || (in.ph == phaseBuilt && !paired)
}

// stopIfNeeded runs the stop step once, if it is owed, after waiting out any
// step another goroutine is still running for the instance, so the release
// never runs against a value a hook holds. That wait is what makes Stop
// synchronous, and it is safe because a hook may not Stop its own scope.
//
// If ctx expires first, the release is still owed and nothing else will reach
// the instance, since Stop took it off the scope's list. So the deadline ends
// the caller's wait, not the teardown, which finishes on its own goroutine
// with the spent deadline dropped.
func (in *instance) stopIfNeeded(ctx context.Context, owner *state) error {
	paired := in.paired(owner)
	for {
		owner.mu.Lock()
		step, what := in.outstanding()
		if step == nil {
			owed := in.owes(paired)
			in.ph = phaseStopped
			in.refresh()
			owner.mu.Unlock()
			if !owed {
				return nil
			}
			return in.stop(ctx, owner)
		}
		owner.mu.Unlock()
		select {
		case <-step:
		case <-ctx.Done():
			go func() { _ = in.stopIfNeeded(context.WithoutCancel(ctx), owner) }()
			return fmt.Errorf("di: stopping %s: %s did not return: %w", in.b.key, what, ctx.Err())
		}
	}
}

// outstanding names the step another goroutine is running for this instance,
// with the channel it will close, or nil. Called with the owning state's
// mutex held, so the phase and the channel are read together.
func (in *instance) outstanding() (chan struct{}, string) {
	switch {
	case in.ph == phaseStarting:
		return waitOn(&in.startingCh), "OnStart"
	case in.dr == draining:
		return waitOn(&in.drainedCh), "OnDrain"
	}
	return nil, ""
}

// drainIfNeeded runs OnDrain once, if it is owed, and reports whether this
// call ran or waited for the hook, so a drain pass can tell that it did work.
// A drain another Stop has begun is waited for, or this Stop would go on to
// run OnStop under it. A start step in flight is waited for as well, since a
// service that is starting owes a drain as soon as it has started.
func (in *instance) drainIfNeeded(ctx context.Context, owner *state) (bool, error) {
	b := in.b
	if b.onDrain == nil {
		return false, nil
	}
	paired := in.paired(owner)
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
			starting := waitOn(&in.startingCh)
			owner.mu.Unlock()
			select {
			case <-starting:
				continue
			case <-ctx.Done():
				return false, fmt.Errorf("di: draining %s: OnStart did not return: %w", b.key, ctx.Err())
			}
		}
		if !owner.isStopped() && in.ph == phaseBuilt && paired {
			// Built and waiting for its start step, so whether it will owe a
			// drain is not decided yet: left alone rather than marked drained.
			// A claim announces itself and sends the sweep round again; a seal
			// nothing announced to means it never starts and owes nothing.
			owner.mu.Unlock()
			return false, nil
		}
		if owner.isStopped() || !in.owes(paired) {
			// Not owed, or built into a scope whose own Stop is past draining
			// while an ancestor's sweep is still running: winding down for
			// work it can no longer take is not what the hook is for.
			in.dr = drained
			owner.mu.Unlock()
			return false, nil
		}
		in.dr = draining
		owner.mu.Unlock()
		break
	}

	t0 := time.Now()
	err := callHook(b.onDrain, inHook(ctx, owner), in.value)
	owner.report(EventDrain, b, t0, err)

	owner.mu.Lock()
	in.dr = drained
	wake(in.drainedCh)
	owner.mu.Unlock()

	if err != nil {
		return true, fmt.Errorf("di: draining %s: %w", b.key, err)
	}
	return true, nil
}

// stop cancels the worker, waits for it within ctx, then runs OnStop. A
// worker that outlasts ctx still holds the value, so the missed deadline is
// reported and the release finishes when the worker returns.
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
			err := fmt.Errorf("di: stopping %s: worker did not return: %w", b.key, ctx.Err())
			if b.onStop == nil {
				owner.report(EventStop, b, t0, err)
				return err
			}
			go in.releaseAfterWorker(context.WithoutCancel(ctx), owner, err)
			return err
		}
	}
	if b.onStop != nil {
		if err := callHook(b.onStop, inHook(ctx, owner), in.value); err != nil {
			errs = append(errs, fmt.Errorf("di: stopping %s: %w", b.key, err))
		}
	}
	err := errors.Join(errs...)
	owner.report(EventStop, b, t0, err)
	return err
}

// releaseAfterWorker finishes a stop step whose worker outlasted Stop's
// context. missed is what Stop returned; the instance's single EventStop is
// emitted here and carries it with the release's own result.
func (in *instance) releaseAfterWorker(ctx context.Context, owner *state, missed error) {
	<-in.runDone
	b := in.b
	t0 := time.Now()
	errs := []error{missed}
	if in.runErr != nil {
		errs = append(errs, in.runErr)
	}
	if err := callHook(b.onStop, inHook(ctx, owner), in.value); err != nil {
		errs = append(errs, fmt.Errorf("di: stopping %s: %w", b.key, err))
	}
	owner.report(EventStop, b, t0, errors.Join(errs...))
}

// onlyCancellation reports whether err says nothing beyond context.Canceled.
// errors.Is would call errors.Join(ctx.Err(), failure) a cancellation and
// drop the failure with it.
func onlyCancellation(err error) bool {
	if err == context.Canceled {
		return true
	}
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		errs := u.Unwrap()
		if len(errs) == 0 {
			return false
		}
		for _, e := range errs {
			if !onlyCancellation(e) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		inner := u.Unwrap()
		return inner != nil && onlyCancellation(inner)
	}
	return false
}

// Start builds every Eager binding in registration order, then runs the
// start step of everything built so far, in build order. If a constructor or
// a start step fails, the scope is stopped, rolling back the services that
// did start, child scopes included; a service built but never started is not
// stopped, so acquire resources in OnStart when the binding declares one.
//
// After Start returns, a service built later starts as part of being built.
// Start may be called once, and builds only this scope's own Eager bindings.
func (s *Scope) Start(ctx context.Context) error {
	// The rollback detaches the caller's context: an already-cancelled ctx
	// must not skip the teardown.
	return s.start(ctx, func() (context.Context, func()) {
		return context.WithoutCancel(ctx), func() {}
	})
}

// start is Start with the rollback context supplied by the caller: Start
// detaches the caller's context, Run applies its StopTimeout and signals.
func (s *Scope) start(ctx context.Context, rollbackCtx func() (context.Context, func())) (err error) {
	defer recoverAbort(&err)
	s.st.freeze()
	if !s.st.startCtx.CompareAndSwap(nil, &ctx) {
		return errors.New("di: Start called twice")
	}
	eager := s.st.reg.Load().eager // a registry is never written to; a later freeze stores a new one

	if err := s.buildEager(eager); err != nil {
		return errors.Join(err, s.rollback(rollbackCtx))
	}

	s.st.running.Store(true)

	// Anything built before the flag was set is still waiting here, and
	// starting one service may build more.
	for {
		in, owner := s.st.claimNext()
		if in == nil {
			if s.st.isStopped() {
				return fmt.Errorf("di: Start: %w", ErrStopped)
			}
			return nil
		}
		if !in.gateStart(owner) {
			continue // the scope sealed and stopped first; claimNext now says so
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

// buildEager builds the eager bindings, turning a constructor failure into an
// error rather than letting it unwind past Start's rollback.
func (s *Scope) buildEager(eager []*binding) (err error) {
	defer recoverAbort(&err)
	for _, b := range eager {
		if b.group {
			// A group member is not reachable by key.
			s.enter().resolve(b, s.st)
			continue
		}
		// By key, so whichever registration owns the key is what gets built.
		s.enter().get(b.key)
	}
	return nil
}

// claimNext claims the start step of the next built-but-unstarted instance
// in this scope or a descendant, in build order, or nil for a stopped scope:
// a claim gateStart refused leaves its instance built, and the loop in start
// would otherwise find it again for ever.
func (st *state) claimNext() (*instance, *state) {
	st.mu.Lock()
	if st.isStopped() {
		st.mu.Unlock()
		return nil, nil
	}
	for _, in := range st.started {
		if in.ph == phaseBuilt {
			in.ph = phaseStarting
			in.refresh()
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

// Context returns the context passed to Start (or Run) on this scope or the
// nearest started ancestor, so constructors can dial with a deadline. Before
// Start it returns context.Background().
func (s *Scope) Context() context.Context {
	if ctx, _ := s.st.runContext(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// Stop winds the scope down in three phases. First it drains: OnDrain hooks
// run from the innermost scope outwards, in reverse build order, while every
// scope still resolves, so work in flight can finish; a service or child scope
// that phase brings into being is drained too. Then the scope is marked
// stopped and its child scopes are stopped. Then OnStop hooks run in reverse
// build order. A service is stopped only if it started, or if it declares no
// OnStart, in which case OnStop is a plain destructor. Every failure is
// reported.
//
// Stop is synchronous: it waits out a start step, a drain hook or a worker
// another goroutine is still running, so when it returns the teardown has
// happened. Only an expired ctx cuts that short; the missed deadline is
// reported and the release finishes on a goroutine of its own, reaching
// observers. Afterwards the scope and its descendants refuse to resolve with
// ErrStopped, and a stopped child scope is detached from its parent.
//
// Stop is idempotent and concurrent calls are safe: the first tears the scope
// down and the others wait for it and report its result, bounded by their own
// context. Because Stop waits, a hook must not call Stop on its own scope or
// an ancestor; a hook that passes on its own context gets an error saying so.
// Call Shutdown, which never blocks.
func (s *Scope) Stop(ctx context.Context) error {
	if h := hookOwner(ctx); h != nil && h.descendsFrom(s.st) {
		return fmt.Errorf("di: a lifecycle hook of scope %s called Stop on scope %s, which it is inside: call Shutdown instead", h.name, s.st.name)
	}
	if !s.st.stopOnce.claim(s.st, func() {
		if s.st.stopCtx == nil {
			s.st.stopCtx = ctx // the first Stop owns it; a later call must not clobber it
		}
	}) {
		finished, err := s.st.stopOnce.wait(s.st, ctx)
		if !finished {
			return fmt.Errorf("di: waiting for scope %s to stop: %w", s.st.name, ctx.Err())
		}
		return err
	}
	err := s.teardown(ctx)
	s.st.stopOnce.settle(s.st, err)
	return err
}

// teardown is the body of the first Stop.
func (s *Scope) teardown(ctx context.Context) error {
	errs := []error{s.drain(ctx)}

	s.st.mu.Lock()
	children := slices.Clone(s.st.children)
	started := s.st.started
	s.st.started = nil // stopped was stored by drain's seal, before this snapshot
	var wrappers []*binding
	for _, b := range s.st.reg.Load().all {
		if b.inner != nil {
			wrappers = append(wrappers, b)
		}
	}
	for _, b := range s.st.pending {
		if b.inner != nil {
			wrappers = append(wrappers, b)
		}
	}
	s.st.mu.Unlock()
	// The scope serves nothing from here on, so its wrappers stop guarding
	// what they wrap. Outside the mutex: the binding may be an ancestor's.
	for _, w := range wrappers {
		w.inner.unwrap(w)
	}

	for _, c := range children {
		errs = append(errs, (&Scope{st: c}).Stop(ctx))
	}
	errs = append(errs, stopAll(ctx, s.st, started))

	if p := s.st.parent; p != nil {
		p.mu.Lock()
		p.children = slices.DeleteFunc(p.children, func(c *state) bool { return c == s.st })
		p.mu.Unlock()
	}
	return errors.Join(errs...)
}

// drain runs the OnDrain hooks of this scope's subtree, innermost first and in
// reverse build order, and then seals the phase, which marks the scope
// stopped. Only the first drain of a scope runs its hooks; a Stop that reaches
// the scope by another route waits for it, or it would start releasing what
// those hooks are still using.
func (s *Scope) drain(ctx context.Context) error {
	g0 := s.st.drainGen.Load()
	var r *drainRun
	newRun := func() *drainRun {
		root := &drainScope{st: s.st, ours: true}
		return &drainRun{root: root, seen: map[*state]*drainScope{s.st: root}}
	}
	claimed := s.st.drainOnce.claim(s.st, nil)
	var err error
	if claimed {
		r = newRun()
		err = r.sweepAll(ctx)
	} else {
		// The owner settles the phase with what this scope's own hooks
		// reported, and every Stop of the scope reports that.
		finished, werr := s.st.drainOnce.wait(s.st, ctx)
		if !finished {
			werr = fmt.Errorf("di: waiting for scope %s to drain: %w", s.st.name, ctx.Err())
		}
		err = werr
	}
	// Whoever ran the sweep, this Stop seals the scope against anything
	// announced since the sweep began, and sweeps again if there was any.
	for !s.st.seal(ctx, g0) {
		g0 = s.st.drainGen.Load()
		if r == nil {
			r = newRun()
		}
		err = errors.Join(err, r.sweepAll(ctx))
	}
	if claimed {
		s.st.drainOnce.settle(s.st, err) // this scope's phase is the last to end
	}
	return err
}

// seal ends the drain phase and marks the scope stopped, unless drain work was
// announced in the subtree since g0; then it reports false and the caller
// sweeps again. An expired ctx seals regardless.
//
// seal and announce are a Dekker pair: seal stores sealed then reads drainGen,
// announce adds to drainGen then reads sealed, so neither can miss the other.
// Either the sweep goes round again and finds the work, or the announcer
// waits for this decision, which is two atomic reads away, never a hook.
func (st *state) seal(ctx context.Context, g0 uint64) bool {
	st.sealed.Store(true)

	ok := st.drainGen.Load() == g0 || ctx.Err() != nil
	if ok {
		st.stopped.Store(true)
	}

	// sealCh exists only if an announcer had to wait. It checks sealed and
	// makes the channel under the mutex, and this clears sealed and wakes it
	// under the same mutex, so the wake cannot fall between the two.
	st.mu.Lock()
	st.sealed.Store(false)
	wake(st.sealCh)
	st.sealCh = nil
	st.mu.Unlock()
	return ok
}

// announce records drain work in st's subtree, an instance published or a
// start step claimed, and reports whether a teardown has sealed an enclosing
// scope and stopped it, in which case the work must be undone rather than
// drained. Called with no state mutex held, after the work is visible to a
// sweep.
func (st *state) announce() (stopped bool) {
	for a := st; a != nil; a = a.parent {
		a.drainGen.Add(1)
	}
	for a := st; a != nil; a = a.parent {
		if !a.sealed.Load() {
			continue
		}
		var ch chan struct{}
		a.mu.Lock()
		if a.sealed.Load() {
			ch = waitOn(&a.sealCh) // still sealed under the mutex: the wake is ahead of us
		}
		a.mu.Unlock()
		if ch != nil {
			<-ch
		}
	}
	return st.isStopped()
}

// gateStart announces a start step just claimed, and undoes the claim if an
// enclosing scope sealed and stopped first: the instance stays built, owes
// nothing, and never starts. A waiter on the start step is released.
func (in *instance) gateStart(owner *state) bool {
	if !owner.announce() {
		return true
	}
	owner.mu.Lock()
	in.ph = phaseBuilt
	in.refresh()
	wake(in.startingCh)
	in.startingCh = nil
	owner.mu.Unlock()
	return false
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

// sweepAll sweeps the subtree until a pass finds no new work: the scope still
// resolves during this phase, so a hook may build a service or open a child
// scope that owes a drain too. ctx bounds the sweep as well as the hooks.
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
// first and in reverse creation order. Every owned scope is swept on every
// pass, because a hook may build into a scope already visited.
//
// It returns the errors that belong to this scope's Stop. A descendant's are
// settled into that descendant's phase, so its own Stop reports them and they
// reach this caller through teardown, which joins what its children's Stop
// returns. Errors found in a descendant after its phase ended have nowhere
// else to go and bubble up here.
//
// A descendant's phase is claimed just before its subtree is swept and ended
// as soon as the sweep finishes, so while a hook runs the only unended phases
// this run holds are the scope being swept and its ancestors, which a hook may
// not Stop anyway. Claiming the whole subtree up front would deadlock a hook
// that stops a scope the walk has claimed but not yet reached. A scope another
// Stop already owns is waited for and then left alone, subtree included.
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
			} else if finished, _ := c.drainOnce.wait(c, ctx); !finished {
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
		ds.st.drainOnce.settle(ds.st, errors.Join(errs...))
		ds.settled = true
		return nil // reported by this scope's own Stop, not by its parent's
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
