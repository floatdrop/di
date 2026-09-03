//go:build unix

package di_test

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/floatdrop/di"
)

func TestRunStopsOnSIGTERM(t *testing.T) {
	started := make(chan struct{})
	s := di.New()
	s.Value(&DB{}).Eager().OnStart(func(context.Context, *DB) error { close(started); return nil })
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	<-started // the handler is registered before Start, so the signal is safe to send now
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on SIGTERM")
	}
}
