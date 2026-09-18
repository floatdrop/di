// Package mail sends messages from a background worker. It exports its
// contract, Mailer, and its Module; the constructor and the loop are private.
package mail

import (
	"context"
	"log/slog"

	"github.com/floatdrop/di"
)

type Mailer struct {
	log   *slog.Logger
	queue chan string
}

// newMailer takes the logger as a parameter, like any other dependency.
func newMailer(log *slog.Logger) *Mailer {
	return &Mailer{log: log, queue: make(chan string, 64)}
}

// Send queues a message; the worker delivers it.
func (m *Mailer) Send(msg string) {
	select {
	case m.queue <- msg:
	default:
		m.log.Warn("mail: queue full, dropped", "msg", msg)
	}
}

// run delivers until ctx is cancelled, which Stop does before the services
// the mailer depends on are stopped. Cancellation means stop accepting, not
// stop finishing: what is already queued is delivered before returning, so a
// request that queued a message just before shutdown is not silently dropped.
//
// The tail is bounded by what is queued at that moment, not by the queue
// running dry. Draining until empty would let a Send racing the shutdown keep
// the worker past Stop's deadline, and a worker that does not return is
// reported as a teardown failure.
func (m *Mailer) run(ctx context.Context) error {
	for {
		select {
		case msg := <-m.queue:
			m.log.Info("mail: sent", "msg", msg)
		case <-ctx.Done():
			for range len(m.queue) {
				m.log.Info("mail: sent", "msg", <-m.queue)
			}
			return nil
		}
	}
}

// Module registers the mailer as an eager service with a worker: it exists
// once Start returns, its loop runs in its own goroutine, and Stop cancels
// the loop and waits for it.
func Module(s *di.Scope) {
	s.Wire[*Mailer](newMailer).
		Eager().
		Go(func(ctx context.Context, m *Mailer) error { return m.run(ctx) })
}
