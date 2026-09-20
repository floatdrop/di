// Package digrpc connects a di.Scope to google.golang.org/grpc.
//
// An [Interceptor] gives every call its own child scope holding a [*Call],
// so services that depend on the call are declared once in the application
// scope as Scoped and built per call. [Register] serves a generated service
// with an implementation resolved from that scope, so the implementation is
// such a service too; one registered the plain way reaches the scope through
// [di.FromContext] on the context it is given. [Module] registers the
// interceptor as a service; [New] makes one directly.
//
// It is a separate module, so the library itself does not depend on grpc:
//
//	go get github.com/floatdrop/di/digrpc
package digrpc

import (
	"context"

	"github.com/floatdrop/di"
	"google.golang.org/grpc"
)

// Call is what a call's scope holds: the method being served and the
// context the call arrived with, which carries its metadata, peer and
// deadline. It is registered as *Call, so Validate takes
// di.Provided[*Call]().
type Call struct {
	// Method is the full method name, "/package.Service/Method".
	Method string
	// Context is the handler's context, with the call's scope attached.
	Context context.Context
}

// Interceptor gives every call its own child scope of the application scope:
// a *Call is registered in it, the scope is attached to the handler's
// context, and it is stopped and detached when the handler returns. Stop
// failures reach the application scope's observers as EventStop with Err
// set. Unary and Stream are the two grpc interceptor shapes; Options wraps
// both as server options. Make one with New.
type Interceptor struct{ scope *di.Scope }

// New makes an Interceptor whose call scopes are children of s.
func New(s *di.Scope) Interceptor { return Interceptor{scope: s} }

// Module registers an Interceptor over the scope it is applied to, so that a
// constructor wired into that scope can take one as a parameter:
//
//	app.Use(digrpc.Module, api.Module)
//
//	func NewServer(ic digrpc.Interceptor) *grpc.Server {
//		return grpc.NewServer(ic.Options()...)
//	}
//
// This is a Provide closure rather than a wired constructor because the
// interceptor needs the scope itself, to open a child per call.
func Module(s *di.Scope) {
	s.Provide(func(s *di.Scope) Interceptor { return New(s) })
}

// Unary is a grpc.UnaryServerInterceptor.
func (i Interceptor) Unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	ctx, stop := i.open(ctx, info.FullMethod)
	defer stop()
	return handler(ctx, req)
}

// Stream is a grpc.StreamServerInterceptor. The handler's stream returns the
// call's context from Context.
func (i Interceptor) Stream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx, stop := i.open(ss.Context(), info.FullMethod)
	defer stop()
	return handler(srv, &stream{ServerStream: ss, ctx: ctx})
}

// Options returns Unary and Stream as server options. They chain, so other
// interceptors can be given to the same server before or after them.
func (i Interceptor) Options() []grpc.ServerOption {
	return []grpc.ServerOption{grpc.ChainUnaryInterceptor(i.Unary), grpc.ChainStreamInterceptor(i.Stream)}
}

// open makes the call's scope and returns the context to hand the handler
// and the function that stops the scope once it returns.
func (i Interceptor) open(ctx context.Context, method string) (context.Context, func()) {
	call := i.scope.Child("call")
	ctx = di.WithScope(ctx, call)
	call.Value(&Call{Method: method, Context: ctx})
	return ctx, func() { _ = call.Stop(context.WithoutCancel(ctx)) }
}

// stream is ss with the call's context.
type stream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *stream) Context() context.Context { return s.ctx }
