package di

// The lifecycle: an instance's phase machine, the hooks that move it through
// its start, drain and stop steps, and Start and Stop, which drive that
// machine for a whole scope tree. Every phase is read and written under the
// owning state's mutex, and every user hook is called through callHook.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

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

// drainPhase tracks OnDrain the way phase tracks the build and start steps:
// a drain in progress is waited for, so a waiter has to tell it from one that
// has finished.
type drainPhase int8

const (
	drainNone drainPhase = iota // OnDrain has not been considered
	draining                    // a Stop is running OnDrain now
	drained                     // OnDrain ran, or was skipped for good
)

// dep is one recorded dependency edge: an instance a constructor resolved,
// and the scope holding it, which is what names it in a rendering. The
// holder is carried rather than looked up because an instance does not know
// its own scope, and the scope it was resolved from may be gone by the time
// anything reads the edge.
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

	// deps are the services this instance's constructor resolved, in the
	// order it asked for them, each recorded once however many times it
	// asked. Guarded by the owning state's mutex, because a constructor may
	// resolve from several goroutines at once and they share the Scope it
	// was handed. Only Explain and Graph read them; nothing in the
	// build/start/stop machine does.
	deps []dep

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
	// and an undisputed drain allocate nothing. Never block on one of these
	// fields directly, since a receive from a nil channel blocks for ever;
	// go through waitOn.
	settledCh  chan struct{} // closed by settle: value and err are final
	startingCh chan struct{} // closed when the start step is no longer in flight
	drainedCh  chan struct{} // closed when OnDrain has finished

	// builder is the resolution running the build step, guarded by the
	// container graph's mutex. It is the edge that makes a cycle between
	// concurrent builds visible.
	builder *resolver

	// Worker hook bookkeeping, guarded by the phase machine rather than a mutex.
	// cancel and runDone are written by start, on the goroutine that owns the
	// start step, and read by stop, which stopIfNeeded reaches only after
	// startClaimed has moved the phase past phaseStarting under the owning
	// state's mutex; that lock handoff is the happens-before. runErr is written
	// by the worker goroutine before it closes runDone and read only after a
	// receive from runDone.
	cancel  context.CancelFunc
	runDone chan struct{}
	runErr  error
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
// context can name the misuse instead of waiting for a step the caller is
// itself running. A hook that passes a context of its own is not seen.
func inHook(ctx context.Context, st *state) context.Context {
	return context.WithValue(ctx, hookKey{}, st)
}

// hookOwner returns the scope whose hook ctx belongs to, or nil.
func hookOwner(ctx context.Context) *state {
	st, _ := ctx.Value(hookKey{}).(*state)
	return st
}

// callHook runs a lifecycle hook and reports what it did as an error, a panic
// included. A hook that panics -- or resolves something whose registration is
// rejected, which reaches it as a panic -- must not take the teardown down
// with it: stopOnce would be claimed and never settled, every later Stop
// would wait for it, and every instance behind it would never be released.
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

// start runs OnStart and launches the Worker hook. The worker's context is
// detached from ctx so the worker is cancelled by Stop, in dependency order,
// rather than the moment the application context is cancelled.
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
			err := b.worker(hctx, in.value)
			if err == nil {
				return
			}
			if rctx.Err() != nil && onlyCancellation(err) {
				return // we cancelled it and it reported just that
			}
			// Any other error is the worker's own failure and goes to
			// Shutdown, whether or not the scope had begun stopping:
			// rctx.Err() says whether we cancelled, not why the worker
			// failed. It is wrapped once and kept, so that Stop, which
			// reports it, and Run, which receives it, recognise one failure
			// rather than listing it twice.
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

// startClaimed runs the start step of an instance already in phaseStarting
// and settles the phase, which releases a Stop or a resolution waiting for
// the step. Only a hook that returned has started its service: a panic is a
// failed start, as it is for a constructor, or a caller that recovered it
// would be served a half-initialised service and Stop would pair an OnStop
// with an OnStart that never finished. The failure is recorded on the
// instance as well as returned, so a resolution that waited reports it too.
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
// started, or it was built and has no start step to pair with, so OnStop is
// a plain destructor. An instance whose OnStart failed or was skipped by a
// rollback owes nothing. Called with the owning state's mutex held.
func (in *instance) owes(paired bool) bool {
	return in.ph == phaseStarted || (in.ph == phaseBuilt && !paired)
}

// stopIfNeeded runs the stop step once, if it is owed. It first waits out
// whichever step another goroutine is still running for this instance -- a
// start step, then a drain hook -- so the release never runs against a value
// one of them holds. That wait is what makes Stop synchronous, and it is safe
// because a hook may not call Stop on its own scope or an ancestor.
//
// If ctx expires first the release is still owed, and this is the only caller
// that can make it happen: Stop took the instance off its scope's list before
// the walk began. So the deadline ends the caller's wait, not the teardown,
// which finishes on a goroutine of its own once the step returns, with the
// spent deadline dropped so that it waits properly.
func (in *instance) stopIfNeeded(ctx context.Context, owner *state) error {
	paired := in.paired(owner)
	for {
		owner.mu.Lock()
		step, what := in.outstanding()
		if step == nil {
			owed := in.owes(paired)
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

// drainIfNeeded runs OnDrain once, if it is owed: a service that will not be
// stopped has nothing to wind down. It reports whether this call ran or waited
// for the hook, so a drain pass can tell that it did work.
//
// A drain another Stop has begun is waited for, not skipped, or this Stop
// would go on to run OnStop while that hook still holds the value. A start
// step in flight is waited for as well: a service that is starting owes a
// drain as soon as it has started.
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
		if owner.isStopped() || !in.owes(paired) {
			// Not owed, or the scope's own Stop has moved past draining and a
			// sweep still running in an ancestor reached an instance built
			// into it: winding it down for work it can no longer take on is
			// the opposite of what the hook is for. Straight to drained, so
			// no waiter can arrive and no channel is needed.
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

// stop cancels the Worker hook, waits for it within ctx, then runs OnStop.
//
// A Worker hook that outlasts ctx still holds the value, so OnStop cannot run yet
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
			err := fmt.Errorf("di: stopping %s: Worker hook did not return: %w", b.key, ctx.Err())
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

// releaseAfterWorker finishes a stop step whose Worker hook outlasted Stop's
// context, once the hook returns. missed is what Stop returned to its caller;
// the instance's single EventStop is emitted here and carries it along with
// the release's own result, so no observer sees a service stopped twice.
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

// onlyCancellation reports whether err says nothing beyond context.Canceled:
// the cancellation itself, or wrappings of it. A worker that returns
// errors.Join(ctx.Err(), failure) after being cancelled is reporting the
// failure, and errors.Is would have called the whole thing a cancellation.
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
			s.enter().resolve(b, s.state)
			continue
		}
		// By key, so whichever registration owns the key is what gets built;
		// deriveEager has already checked that it can honour eagerness.
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

// Context returns the context passed to Start (or Run) on this scope or the
// nearest started ancestor, so constructors can dial with a deadline. Before
// Start it returns context.Background().
func (s *Scope) Context() context.Context {
	if ctx, _ := s.runContext(); ctx != nil {
		return ctx
	}
	return context.Background()
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
// hook another Stop began, a Worker hook being cancelled -- so when it returns,
// the teardown has happened and its failures are in the error. A teardown
// outlives the call only when ctx expires first: the missed deadline is
// reported here, and the release is finished once the outstanding step
// returns, on a goroutine of its own, reaching observers rather than this
// caller.
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
	if h := hookOwner(ctx); h != nil && h.descendsFrom(s.state) {
		return fmt.Errorf("di: a lifecycle hook of scope %s called Stop on scope %s, which it is inside: call Shutdown instead", h.name, s.name)
	}
	if !s.stopOnce.claim(s.state, func() {
		if s.stopCtx == nil {
			s.stopCtx = ctx // the first Stop owns it; a later call must not clobber it
		}
	}) {
		finished, err := s.stopOnce.wait(s.state, ctx)
		if !finished {
			return fmt.Errorf("di: waiting for scope %s to stop: %w", s.name, ctx.Err())
		}
		return err
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
		// The owner settles the phase with what this scope's own hooks
		// reported, and a Stop reports that whether it ran the hooks or
		// waited for someone else to.
		finished, err := s.drainOnce.wait(s.state, ctx)
		if !finished {
			return fmt.Errorf("di: waiting for scope %s to drain: %w", s.name, ctx.Err())
		}
		return err
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
// It returns the errors that belong to *this* scope's Stop. A descendant's are
// settled into that descendant's phase instead, so its own Stop reports them,
// and they reach this caller through teardown, which stops its children and
// joins what their Stop returns; that is what keeps one failure to one place
// in the aggregate. Errors found in a descendant after its phase has ended
// have nowhere to be settled, so those bubble up here.
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
