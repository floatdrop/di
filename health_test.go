package di_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/floatdrop/di"
)

func TestHealthCheck(t *testing.T) {
	s := di.New()
	sick := errors.New("connection refused")
	s.Provide(func(*di.Scope) *DB { return &DB{} }).
		Health(func(context.Context, *DB) error { return sick })
	s.Provide(func(s *di.Scope) *Repo { return &Repo{db: s.Get[*DB]()} }).
		Health(func(context.Context, *Repo) error { return nil })

	if err := s.HealthCheck(context.Background()); err != nil {
		t.Fatalf("nothing built yet, got %v", err)
	}
	s.Get[*Repo]()
	err := s.HealthCheck(context.Background())
	if !errors.Is(err, di.ErrUnhealthy) || !errors.Is(err, sick) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "di_test.DB unhealthy") || strings.Contains(err.Error(), "Repo") {
		t.Fatalf("should name only the failing service: %v", err)
	}
}

func TestHealthCheckIncludesChildrenAndRunsConcurrently(t *testing.T) {
	root := di.New()
	root.Value(&DB{}).Health(func(ctx context.Context, _ *DB) error {
		select {
		case <-time.After(100 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	root.Get[*DB]()
	child := root.Child("child")
	child.Value(&Repo{}).Health(func(ctx context.Context, _ *Repo) error {
		select {
		case <-time.After(100 * time.Millisecond):
			return errors.New("child sick")
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	child.Get[*Repo]()

	start := time.Now()
	err := root.HealthCheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), "child sick") {
		t.Fatalf("child service not checked: %v", err)
	}
	if time.Since(start) > 180*time.Millisecond {
		t.Fatal("checks did not run concurrently")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := root.HealthCheck(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout not propagated: %v", err)
	}
}
