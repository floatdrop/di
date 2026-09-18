package mail

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// Cancelling the worker must deliver what is already queued: a request that
// queued a message just before shutdown is not dropped.
func TestRunDeliversTheQueueOnCancel(t *testing.T) {
	var sent strings.Builder
	m := newMailer(slog.New(slog.NewTextHandler(&sent, nil)))
	m.Send("first")
	m.Send("second")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.run(ctx); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"first", "second"} {
		if !strings.Contains(sent.String(), want) {
			t.Fatalf("%q was queued and never sent:\n%s", want, sent.String())
		}
	}
}
