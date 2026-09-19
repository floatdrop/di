package digrpc_test

import (
	"context"
	"testing"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/digrpc"
	"google.golang.org/grpc"
)

// perCall is built once per call scope; register counts its builds and stops.
type perCall struct{ method string }

type counts struct{ built, stopped int }

func register(t *testing.T) (*di.Scope, *counts) {
	t.Helper()
	app := di.Test(t)
	n := &counts{}
	app.Wire[*perCall](func(c *digrpc.Call) *perCall { n.built++; return &perCall{method: c.Method} }).Scoped().
		OnStop(func(context.Context, *perCall) error { n.stopped++; return nil })
	if err := app.Validate(di.Provided[*digrpc.Call]()).Err(); err != nil {
		t.Fatal(err)
	}
	return app, n
}

func TestUnaryOpensAScopePerCall(t *testing.T) {
	app, n := register(t)
	ic := digrpc.New(app)
	info := &grpc.UnaryServerInfo{FullMethod: "/pkg.Svc/Method"}
	handler := func(ctx context.Context, req any) (any, error) {
		s, ok := di.FromContext(ctx)
		if !ok {
			t.Fatal("no scope on the handler's context")
		}
		if got := s.Get[*digrpc.Call]().Context; got != ctx {
			t.Error("Call.Context is not the handler's context")
		}
		if got := s.Get[*perCall]().method; got != info.FullMethod {
			t.Errorf("method %q, want %q", got, info.FullMethod)
		}
		return req, nil
	}
	for i := range 2 {
		res, err := ic.Unary(t.Context(), i, info, handler)
		if err != nil || res != i {
			t.Fatalf("call %d: %v, %v", i, res, err)
		}
	}
	if *n != (counts{built: 2, stopped: 2}) {
		t.Errorf("two calls built and stopped %+v, want 2 and 2", *n)
	}
}

// fakeStream is a ServerStream with a context and nothing else.
type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f fakeStream) Context() context.Context { return f.ctx }

func TestStreamOpensAScopePerCall(t *testing.T) {
	app, n := register(t)
	ic := digrpc.New(app)
	info := &grpc.StreamServerInfo{FullMethod: "/pkg.Svc/Stream"}
	var outer context.Context
	handler := func(_ any, ss grpc.ServerStream) error {
		s, ok := di.FromContext(ss.Context())
		if !ok {
			t.Fatal("no scope on the stream's context")
		}
		if s.Get[*digrpc.Call]().Context != ss.Context() {
			t.Error("Call.Context is not the stream's context")
		}
		if got := s.Get[*perCall]().method; got != info.FullMethod {
			t.Errorf("method %q, want %q", got, info.FullMethod)
		}
		if _, ok := di.FromContext(outer); ok {
			t.Error("the scope leaked into the incoming context")
		}
		return nil
	}
	outer = t.Context()
	if err := ic.Stream(nil, fakeStream{ctx: outer}, info, handler); err != nil {
		t.Fatal(err)
	}
	if *n != (counts{built: 1, stopped: 1}) {
		t.Errorf("one call built and stopped %+v, want 1 and 1", *n)
	}
}
