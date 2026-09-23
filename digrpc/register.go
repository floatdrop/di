package digrpc

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"golang.yandex/di"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Register serves the service desc describes with an H resolved from each
// call's scope, so the implementation is a service like any other: declared
// Scoped when it needs the call, and once for the application when it does
// not. The desc is the generated one, and H must implement its interface:
//
//	digrpc.Register[*Users](srv, &pb.Users_ServiceDesc)
//
// H may also be the service interface itself, resolved from whatever serves
// it. H is resolved after the server's interceptors have run, so what they
// attach to the context is there for its constructors. A constructor that
// returns a status error fails the call with that status; any other failure,
// or a nil H, is codes.Internal, with a failed build reported to the scope's
// observers. Interceptors see a nil Server in the call's info, since the
// implementation does not exist yet when they run. Register panics when H
// does not implement the service.
func Register[H any](srv grpc.ServiceRegistrar, desc *grpc.ServiceDesc) {
	t := reflect.TypeFor[H]()
	iface := reflect.TypeOf(desc.HandlerType).Elem()
	if !t.Implements(iface) {
		panic(fmt.Sprintf("digrpc: %s does not implement %s", t, iface))
	}
	wrapped := *desc
	wrapped.Methods = make([]grpc.MethodDesc, len(desc.Methods))
	for i, m := range desc.Methods {
		wrapped.Methods[i] = grpc.MethodDesc{MethodName: m.MethodName, Handler: unary[H](m.Handler)}
	}
	wrapped.Streams = make([]grpc.StreamDesc, len(desc.Streams))
	for i, s := range desc.Streams {
		s.Handler = streaming[H](s.Handler)
		wrapped.Streams[i] = s
	}
	srv.RegisterService(&wrapped, placeholder[H]())
}

// placeholder is a value of H for RegisterService's implements check, or nil
// for an interface H, which RegisterService accepts. It is never called:
// every handler ignores the value it is given.
func placeholder[H any]() any {
	if t := reflect.TypeFor[H](); t.Kind() == reflect.Pointer {
		return reflect.New(t.Elem()).Interface()
	}
	var zero H
	return zero
}

// unary lets the generated handler decode the request and build the call's
// info, then runs the server's interceptor chain with a handler that resolves
// H and calls the generated handler again with it: a decoder that does
// nothing and an interceptor that hands it the request already decoded make
// that second call the typed method call. The generated handler always calls
// the interceptor it is given, so the value it is first given as the server
// is never used.
func unary[H any](orig grpc.MethodHandler) grpc.MethodHandler {
	return func(_ any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		return orig(nil, ctx, dec, func(ctx context.Context, req any, info *grpc.UnaryServerInfo, _ grpc.UnaryHandler) (any, error) {
			handler := func(ctx context.Context, req any) (any, error) {
				impl, err := resolve[H](ctx)
				if err != nil {
					return nil, err
				}
				return orig(impl, ctx, func(any) error { return nil },
					func(ctx context.Context, _ any, _ *grpc.UnaryServerInfo, call grpc.UnaryHandler) (any, error) {
						return call(ctx, req)
					})
			}
			if interceptor == nil {
				return handler(ctx, req)
			}
			return interceptor(ctx, req, info, handler)
		})
	}
}

// streaming resolves H once the stream interceptors have run and hands it
// to the generated handler as the implementation.
func streaming[H any](orig grpc.StreamHandler) grpc.StreamHandler {
	return func(_ any, ss grpc.ServerStream) error {
		impl, err := resolve[H](ss.Context())
		if err != nil {
			return err
		}
		return orig(impl, ss)
	}
}

// resolve is H from the call's scope, as a status error when it fails.
func resolve[H any](ctx context.Context) (H, error) {
	var zero H
	s, ok := di.FromContext(ctx)
	if !ok {
		return zero, status.Error(codes.Internal, "digrpc: no call scope on the context; is the Interceptor on this server?")
	}
	impl, err := s.Resolve[H]()
	if err == nil {
		if any(impl) == nil {
			return zero, status.Error(codes.Internal, "digrpc: the service resolved to nil")
		}
		return impl, nil
	}
	// A constructor's own status, not the build error wrapping it, is what
	// the client should see.
	if se, ok := errors.AsType[interface {
		error
		GRPCStatus() *status.Status
	}](err); ok {
		return zero, se.GRPCStatus().Err()
	}
	return zero, status.Error(codes.Internal, "digrpc: the service could not be built")
}
