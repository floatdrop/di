// Package mail sends messages from a background worker.
package mail

import (
	"context"
	"log"

	"github.com/floatdrop/di"
)

type Mailer struct{ queue chan string }

func New() *Mailer { return &Mailer{queue: make(chan string, 64)} }

// Send queues a message; the worker delivers it.
func (m *Mailer) Send(msg string) {
	select {
	case m.queue <- msg:
	default:
		log.Println("mail: queue full, dropped", msg)
	}
}

// Run delivers until ctx is cancelled, which Stop does before the services
// the mailer depends on are stopped.
func (m *Mailer) Run(ctx context.Context) error {
	for {
		select {
		case msg := <-m.queue:
			log.Println("mail: sent", msg)
		case <-ctx.Done():
			return nil
		}
	}
}

// Module registers the mailer as an eager service with a worker: it exists
// once Start returns, its loop runs in its own goroutine, and Stop cancels
// the loop and waits for it.
func Module(s *di.Scope) {
	s.Wire[*Mailer](New).
		Eager().
		Go(func(ctx context.Context, m *Mailer) error { return m.Run(ctx) })
}
