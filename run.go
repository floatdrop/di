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

type runConfig struct{ stopTimeout time.Duration }

// exitSignals are what make Run exit: an interrupt or a termination request.
var exitSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// StopTimeout bounds how long Stop may take once Run decides to exit.
// The default is 15 seconds.
func StopTimeout(d time.Duration) RunOption { return func(c *runConfig) { c.stopTimeout = d } }

// stopContext builds the context Run stops with: detached from the caller's,
// bounded by StopTimeout, and cancelled by a second signal so a hung hook
// cannot keep the process alive. A rollback from a failed Start gets the same
// context.
func (c runConfig) stopContext(ctx context.Context) (context.Context, func()) {
	stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), c.stopTimeout)
	forceCtx, cancelForce := signal.NotifyContext(stopCtx, exitSignals...)
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
	cfg := runConfig{stopTimeout: 15 * time.Second}
	for _, o := range opts {
		o(&cfg)
	}

	// Register before Start so a signal during a slow start is not lost.
	sigCtx, cancelSig := signal.NotifyContext(ctx, exitSignals...)
	defer cancelSig()

	if err := s.start(ctx, func() (context.Context, func()) { return cfg.stopContext(ctx) }); err != nil {
		// A failed Start rolls back through Stop, which runs the drain and
		// stop hooks, so a worker can die and publish its failure here
		// exactly as it can during an ordinary shutdown.
		return joinCause(err, s.publishedCause())
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
		cause = s.publishedCause()
	}
	return joinCause(stopErr, cause)
}

// publishedCause reports the failure Shutdown recorded, without waiting for
// one. Run reads it on both ways out, because a worker dies when it dies:
// during the stop that follows a signal, or during the rollback of a Start
// that never finished.
func (s *Scope) publishedCause() error {
	select {
	case <-s.shutdownCh:
		return s.shutdownErr
	default:
		return nil
	}
}

// joinCause adds a published cause to what Run is already returning, unless
// that failure is in there already: a worker's error reaches Run by two
// routes, as the cause and through the Stop that cancelled it, and it is one
// failure either way.
func joinCause(err, cause error) error {
	if cause == nil {
		return err
	}
	if errors.Is(err, cause) {
		return err // one failure, reached by both routes
	}
	return errors.Join(err, cause)
}
