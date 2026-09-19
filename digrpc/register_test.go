package digrpc_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"golang.yandex/di"
	"golang.yandex/di/digrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type caller struct{ name string }

// newCaller is where a call is validated: its status error is the call's.
func newCaller(c *digrpc.Call) (*caller, error) {
	md, _ := metadata.FromIncomingContext(c.Context)
	name := strings.Join(md.Get("x-caller"), ",")
	switch name {
	case "":
		return nil, status.Error(codes.Unauthenticated, "no x-caller")
	case "broken":
		return nil, errors.New("something the client should not read")
	}
	return &caller{name}, nil
}

// health is the whole service implementation, built per call.
type health struct {
	grpc_health_v1.UnimplementedHealthServer
	caller *caller
	log    *[]string
}

func (h *health) Check(context.Context, *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	*h.log = append(*h.log, "check by "+h.caller.name)
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

func (h *health) Watch(_ *grpc_health_v1.HealthCheckRequest, ss grpc.ServerStreamingServer[grpc_health_v1.HealthCheckResponse]) error {
	*h.log = append(*h.log, "watch by "+h.caller.name)
	return ss.Send(&grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_NOT_SERVING})
}

// serve registers health through Register on a server with the interceptor
// and one unary interceptor of the test's own, and returns a client.
func serve(t *testing.T, log *[]string, opts ...grpc.ServerOption) grpc_health_v1.HealthClient {
	t.Helper()
	app := di.Test(t)
	app.Use(digrpc.Module)
	app.Wire[*caller](newCaller).Scoped()
	app.Wire[*health](func(c *caller) *health {
		*log = append(*log, "build")
		return &health{caller: c, log: log}
	}).Scoped()
	app.Wire[*grpc.Server](func(ic digrpc.Interceptor) *grpc.Server {
		srv := grpc.NewServer(append(ic.Options(), opts...)...)
		digrpc.Register[*health](srv, &grpc_health_v1.Health_ServiceDesc)
		return srv
	})
	if err := app.Validate(di.Provided[*digrpc.Call]()).Err(); err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	srv := app.Get[*grpc.Server]()
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return grpc_health_v1.NewHealthClient(conn)
}

func as(t *testing.T, name string) context.Context {
	t.Helper()
	return metadata.AppendToOutgoingContext(t.Context(), "x-caller", name)
}

func TestRegisterBuildsTheServicePerCall(t *testing.T) {
	var log []string
	client := serve(t, &log)
	res, err := client.Check(as(t, "ada"), &grpc_health_v1.HealthCheckRequest{})
	if err != nil || res.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("unary: %v, %v", res, err)
	}
	w, err := client.Watch(as(t, "bob"), &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if msg, err := w.Recv(); err != nil || msg.GetStatus() != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("stream: %v, %v", msg, err)
	}
	want := []string{"build", "check by ada", "build", "watch by bob"}
	if strings.Join(log, ",") != strings.Join(want, ",") {
		t.Errorf("log %q, want %q", log, want)
	}
}

func TestRegisterResolvesAfterTheInterceptors(t *testing.T) {
	var log []string
	seen := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		log = append(log, "interceptor "+info.FullMethod)
		if info.Server != nil {
			t.Error("the info names a server before the service is built")
		}
		return h(ctx, req)
	}
	client := serve(t, &log, grpc.ChainUnaryInterceptor(seen))
	if _, err := client.Check(as(t, "ada"), &grpc_health_v1.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"interceptor /grpc.health.v1.Health/Check", "build", "check by ada"}
	if strings.Join(log, ",") != strings.Join(want, ",") {
		t.Errorf("log %q, want %q", log, want)
	}
}

func TestRegisterFailsTheCallWithTheConstructorsStatus(t *testing.T) {
	var log []string
	client := serve(t, &log)
	_, err := client.Check(t.Context(), &grpc_health_v1.HealthCheckRequest{})
	if st := status.Convert(err); st.Code() != codes.Unauthenticated || st.Message() != "no x-caller" {
		t.Errorf("got %v, want Unauthenticated with the constructor's message", err)
	}
	_, err = client.Check(as(t, "broken"), &grpc_health_v1.HealthCheckRequest{})
	if st := status.Convert(err); st.Code() != codes.Internal || strings.Contains(st.Message(), "should not read") {
		t.Errorf("got %v, want Internal without the constructor's text", err)
	}
	if len(log) != 0 {
		t.Errorf("a rejected call built the service: %q", log)
	}
}

func TestRegisterLeavesUnimplementedMethodsAlone(t *testing.T) {
	var log []string
	client := serve(t, &log)
	_, err := client.List(as(t, "ada"), &grpc_health_v1.HealthListRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("List: %v, want Unimplemented from the embedded type", err)
	}
}

func TestRegisterWithoutTheInterceptorIsInternal(t *testing.T) {
	srv := grpc.NewServer()
	digrpc.Register[*health](srv, &grpc_health_v1.Health_ServiceDesc)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, err = grpc_health_v1.NewHealthClient(conn).Check(t.Context(), &grpc_health_v1.HealthCheckRequest{})
	if st := status.Convert(err); st.Code() != codes.Internal || !strings.Contains(st.Message(), "Interceptor") {
		t.Errorf("got %v, want Internal naming the Interceptor", err)
	}
}

type notAHealth struct{}

func TestRegisterRejectsATypeThatDoesNotImplement(t *testing.T) {
	defer func() {
		msg, _ := recover().(string)
		if !strings.Contains(msg, "notAHealth does not implement") {
			t.Errorf("panic %q, want one naming the type", msg)
		}
	}()
	digrpc.Register[notAHealth](grpc.NewServer(), &grpc_health_v1.Health_ServiceDesc)
}

func TestRegisterAcceptsTheServiceInterface(t *testing.T) {
	var log []string
	app := di.Test(t)
	app.Use(digrpc.Module)
	app.Wire[*caller](newCaller).Scoped()
	app.Wire[grpc_health_v1.HealthServer](func(c *caller) grpc_health_v1.HealthServer {
		if c.name == "nobody" {
			return nil
		}
		log = append(log, "build")
		return &health{caller: c, log: &log}
	}).Scoped()
	srv := grpc.NewServer(digrpc.New(app).Options()...)
	digrpc.Register[grpc_health_v1.HealthServer](srv, &grpc_health_v1.Health_ServiceDesc)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := grpc_health_v1.NewHealthClient(conn)

	if _, err := client.Check(as(t, "ada"), &grpc_health_v1.HealthCheckRequest{}); err != nil {
		t.Fatal("unary:", err)
	}
	w, err := client.Watch(as(t, "bob"), &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Recv(); err != nil {
		t.Fatal("stream:", err)
	}
	want := []string{"build", "check by ada", "build", "watch by bob"}
	if strings.Join(log, ",") != strings.Join(want, ",") {
		t.Errorf("log %q, want %q", log, want)
	}
	_, err = client.Check(as(t, "nobody"), &grpc_health_v1.HealthCheckRequest{})
	if st := status.Convert(err); st.Code() != codes.Internal || !strings.Contains(st.Message(), "nil") {
		t.Errorf("nil unary: %v, want Internal naming nil", err)
	}
	w, err = client.Watch(as(t, "nobody"), &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Recv(); status.Code(err) != codes.Internal {
		t.Errorf("nil stream: %v, want Internal", err)
	}
}
