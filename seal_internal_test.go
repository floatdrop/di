package di

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type sealProbe struct{}

// A start step claimed while a teardown holds the seal waits for the seal's
// decision. Decided stopped, the claim is undone: the service stays built,
// never starts, and Start reports the stopped scope. Decided not stopped, the
// start goes ahead. The window between claimNext and announce runs no user
// code, so neither a generator nor a test through the exported API can put a
// seal inside it; this places the seal first and lets Start walk into it.
func TestSealDecidesAClaimedStart(t *testing.T) {
	for _, stop := range []bool{true, false} {
		t.Run(map[bool]string{true: "stopped", false: "reopened"}[stop], func(t *testing.T) {
			var started atomic.Bool
			s := New()
			s.Provide(func(*Scope) *sealProbe { return &sealProbe{} }).
				OnStart(func(context.Context, *sealProbe) error { started.Store(true); return nil })
			if _, err := s.Resolve[*sealProbe](); err != nil {
				t.Fatal(err)
			}

			st := s.st
			ch := make(chan struct{})
			st.mu.Lock()
			st.sealCh = ch
			st.sealed.Store(true)
			st.mu.Unlock()
			before := st.drainGen.Load()

			done := make(chan error, 1)
			go func() { done <- s.Start(t.Context()) }()
			for deadline := time.Now().Add(5 * time.Second); st.drainGen.Load() == before; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("Start never announced its start claim")
				}
			}
			select {
			case err := <-done:
				t.Fatalf("Start returned while the seal was undecided: %v", err)
			case <-time.After(50 * time.Millisecond):
			}

			// Decide as seal does: stopped or not, then unsealed, then wake.
			if stop {
				st.stopped.Store(true)
			}
			st.mu.Lock()
			st.sealed.Store(false)
			st.sealCh = nil
			st.mu.Unlock()
			close(ch)

			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Start never returned after the seal was decided")
			}
			in := st.reg.Load().index[key{t: reflect.TypeFor[*sealProbe]()}].single
			st.mu.Lock()
			ph := in.ph
			st.mu.Unlock()
			if stop {
				if !errors.Is(err, ErrStopped) || started.Load() || ph != phaseBuilt {
					t.Fatalf("Start: %v, started=%v, phase=%v; want ErrStopped, not started, built", err, started.Load(), ph)
				}
				// Undoing the claim left a built, settled instance, so refresh
				// set ready. What refuses it from here on is the stopped scope:
				// resolve checks before it waits, and await's warm path checks
				// again for a scope that stops mid-resolution.
				if !in.ready.Load() {
					t.Fatal("the undone claim did not leave ready set")
				}
				if _, err := s.Resolve[*sealProbe](); !errors.Is(err, ErrStopped) {
					t.Fatalf("Resolve after the refused start: %v, want ErrStopped", err)
				}
				return
			}
			if err != nil || !started.Load() || ph != phaseStarted {
				t.Fatalf("Start: %v, started=%v, phase=%v; want nil, started, started", err, started.Load(), ph)
			}
			if err := s.Stop(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
