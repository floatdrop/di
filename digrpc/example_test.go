package digrpc_test

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/digrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// Caller is who is making the call. It depends on the *digrpc.Call, which
// only a call scope provides, so it is Scoped and built per call.
type Caller struct{ Name string }

func NewCaller(c *digrpc.Call) *Caller {
	md, _ := metadata.FromIncomingContext(c.Context)
	return &Caller{Name: strings.Join(md.Get("x-caller"), ",")}
}

// Health is a service implementation registered the plain way: one value
// for the whole server, reaching per-call services through the scope on the
// context. See ExampleRegister for the implementation built per call.
type Health struct {
	grpc_health_v1.UnimplementedHealthServer
}

func (Health) Check(ctx context.Context, _ *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	scope, _ := di.FromContext(ctx)
	fmt.Println("check from", scope.Get[*Caller]().Name)
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

// NewServer is a plain constructor: the interceptor arrives as a dependency.
func NewServer(ic digrpc.Interceptor) *grpc.Server {
	srv := grpc.NewServer(ic.Options()...)
	grpc_health_v1.RegisterHealthServer(srv, Health{})
	return srv
}

func ExampleModule() {
	app := di.New()
	app.Use(digrpc.Module)
	app.Wire[*Caller](NewCaller).Scoped()
	app.Wire[*grpc.Server](NewServer)

	// The graph is checked as a call scope would resolve it: the interceptor
	// is provided by the module, the call by each call.
	fmt.Println(app.Validate(di.Provided[*digrpc.Call]()).Err())

	lis := bufconn.Listen(1 << 20)
	srv := app.Get[*grpc.Server]()
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		panic(err)
	}
	defer func() { _ = conn.Close() }()

	ctx := metadata.AppendToOutgoingContext(context.Background(), "x-caller", "ada")
	res, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	fmt.Println(res.GetStatus(), err)
	// Output:
	// <nil>
	// check from ada
	// SERVING <nil>
}

// Greeter is the service implementation built per call: it takes the Caller,
// so it is Scoped, and Register resolves it from each call's scope.
type Greeter struct {
	grpc_health_v1.UnimplementedHealthServer
	caller *Caller
}

func NewGreeter(c *Caller) *Greeter { return &Greeter{caller: c} }

func (g *Greeter) Check(context.Context, *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	fmt.Println("check from", g.caller.Name)
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

func ExampleRegister() {
	app := di.New()
	app.Use(digrpc.Module)
	app.Wire[*Caller](NewCaller).Scoped()
	app.Wire[*Greeter](NewGreeter).Scoped()
	app.Wire[*grpc.Server](func(ic digrpc.Interceptor) *grpc.Server {
		srv := grpc.NewServer(ic.Options()...)
		digrpc.Register[*Greeter](srv, &grpc_health_v1.Health_ServiceDesc)
		return srv
	})
	fmt.Println(app.Validate(di.Provided[*digrpc.Call]()).Err())

	lis := bufconn.Listen(1 << 20)
	srv := app.Get[*grpc.Server]()
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		panic(err)
	}
	defer func() { _ = conn.Close() }()

	ctx := metadata.AppendToOutgoingContext(context.Background(), "x-caller", "ada")
	res, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	fmt.Println(res.GetStatus(), err)
	// Output:
	// <nil>
	// check from ada
	// SERVING <nil>
}
