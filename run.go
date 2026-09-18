package di

// Run and Shutdown: the main-function loop over Start and Stop, and the
// signal handling around it.

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Shutdown asks a running Run to stop and records the cause it should return.
// It never blocks, may be called from any goroutine, and the first call wins.
// It propagates to ancestor scopes, so a service in a child scope can stop the
// application.
func (s *Scope) Shutdown(cause error) {
	first := false
	for st := s.st; st != nil; st = st.parent {
		st.shutdownOnce.Do(func() {
			st.shutdownErr = cause
			close(st.shutdownCh)
			first = first || st == s.st
		})
	}
	if first {
		s.st.emit(Event{Kind: EventShutdown, Scope: s.st.name, Err: cause})
	}
}

// RunOption configures Run.
type RunOption func(*runConfig)

type runConfig struct{ startTimeout, stopTimeout time.Duration }

// defaultTimeout bounds both of Run's phases until an option says otherwise.
const defaultTimeout = 15 * time.Second

// exitSignals are what make Run exit: an interrupt or a termination request.
var exitSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// StartTimeout bounds how long Run's start may take: the context the OnStart
// hooks Start runs receive expires after d, and once it has, the start ends at
// the next step and rolls back what it had started. The default is 15 seconds;
// a d of zero or less takes the bound off, leaving the start bounded only by
// the context Run was called with.
//
// It bounds that phase and no more. A constructor reads Scope.Context, which
// stays the context Run was called with, and a worker runs for as long as its
// service; a service resolved from inside a start hook starts on the scope's
// context too, so a hook that waits on one of those is not bounded either.
// Nothing here cuts short a constructor or hook that ignores its context: the
// phase ends between steps.
func StartTimeout(d time.Duration) RunOption { return func(c *runConfig) { c.startTimeout = d } }

// StopTimeout bounds how long Stop may take once Run decides to exit.
// The default is 15 seconds.
func StopTimeout(d time.Duration) RunOption { return func(c *runConfig) { c.stopTimeout = d } }

// startContext bounds the start phase. It is not the context the scope keeps:
// that one outlives the phase, and start is given both.
func (c runConfig) startContext(ctx context.Context) (context.Context, func()) {
	if c.startTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.startTimeout)
}

// stopContext builds the context Run stops with: detached from the caller's,
// bounded by StopTimeout, and cancelled by a second signal. A rollback from a
// failed Start gets the same context.
func (c runConfig) stopContext(ctx context.Context) (context.Context, func()) {
	stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), c.stopTimeout)
	forceCtx, cancelForce := signal.NotifyContext(stopCtx, exitSignals...)
	return forceCtx, func() { cancelForce(); cancelStop() }
}

// Run starts the scope within StartTimeout and blocks until ctx is cancelled,
// a termination signal arrives, or Shutdown is called. It then stops the scope
// within StopTimeout; a second signal during the stop cancels that context so
// a hung hook cannot keep the process alive. Run returns the Start error, the
// error passed to Shutdown, and any Stop errors, joined; a worker that died on
// its own is reported once.
func (s *Scope) Run(ctx context.Context, opts ...RunOption) error {
	cfg := runConfig{startTimeout: defaultTimeout, stopTimeout: defaultTimeout}
	for _, o := range opts {
		o(&cfg)
	}

	// Register before Start so a signal during a slow start is not lost.
	sigCtx, cancelSig := signal.NotifyContext(ctx, exitSignals...)
	defer cancelSig()

	startCtx, cancelStart := cfg.startContext(ctx)
	defer cancelStart() // a configuration rejection unwinds past the call below
	err := s.start(ctx, startCtx, func() (context.Context, func()) { return cfg.stopContext(ctx) })
	cancelStart() // the phase is over; the scope keeps ctx, not startCtx
	if err != nil {
		// The rollback runs the hooks, so a worker can die and publish its
		// failure here as it can during an ordinary shutdown.
		return joinCause(err, s.publishedCause())
	}

	var cause error
	select {
	case <-sigCtx.Done():
	case <-s.st.shutdownCh:
		cause = s.st.shutdownErr
	}

	stopCtx, cancel := cfg.stopContext(ctx)
	defer cancel()

	stopErr := s.Stop(stopCtx)
	if cause == nil {
		// A worker that died during the stop published its failure after the
		// select above had woken for a signal.
		cause = s.publishedCause()
	}
	return joinCause(stopErr, cause)
}

// publishedCause reports the failure Shutdown recorded, without waiting for
// one.
func (s *Scope) publishedCause() error {
	select {
	case <-s.st.shutdownCh:
		return s.st.shutdownErr
	default:
		return nil
	}
}

// joinCause adds a published cause to what Run is already returning, unless
// it is in there already: a worker's error reaches Run both as the cause and
// through the Stop that cancelled it.
func joinCause(err, cause error) error {
	if cause == nil {
		return err
	}
	if errors.Is(err, cause) {
		return err // one failure, reached by both routes
	}
	return errors.Join(err, cause)
}
