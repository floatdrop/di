// Graceful shutdown of a gRPC server, with a service built per call.
//
// The digrpc interceptor opens a scope for every call and registers the
// *digrpc.Call in it, and digrpc.Register serves the health service with an
// implementation resolved from that scope, so a Scoped Health is built per
// call with a Caller read from the call's metadata. Run starts the scope, waits for SIGINT/SIGTERM or a
// Shutdown call, then stops everything in reverse order with a bounded
// context. The server's OnDrain calls GracefulStop, which stops accepting
// calls and waits for in-flight ones. Draining runs before anything is torn
// down, so those calls still have their scopes.
package main

import (
	"cmp"
	"context"
	"errors"
	"log"
	"net"
	"strings"
	"time"

	"golang.yandex/di"
	"golang.yandex/di/digrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
)

type DB struct{ dsn string }

// Caller is who is making the call, read from its metadata. It depends on
// *digrpc.Call, which only a call scope provides, so it is Scoped.
type Caller struct{ Name string }

func NewCaller(c *digrpc.Call) *Caller {
	md, _ := metadata.FromIncomingContext(c.Context)
	return &Caller{Name: cmp.Or(strings.Join(md.Get("x-caller"), ","), "anonymous")}
}

// Health is the service implementation, built per call because it takes the
// Caller. Methods it does not define fall through to the embedded type.
type Health struct {
	grpc_health_v1.UnimplementedHealthServer
	db     *DB
	caller *Caller
}

func NewHealth(db *DB, c *Caller) *Health { return &Health{db: db, caller: c} }

func (h *Health) Check(context.Context, *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	time.Sleep(2 * time.Second) // simulate slow work that must not be cut short
	log.Println("checked by", h.caller.Name, "against", h.db.dsn)
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

// NewServer is a plain constructor: the interceptor arrives as a dependency,
// and Register resolves *Health from each call's scope.
func NewServer(ic digrpc.Interceptor) *grpc.Server {
	srv := grpc.NewServer(ic.Options()...)
	digrpc.Register[*Health](srv, &grpc_health_v1.Health_ServiceDesc)
	return srv
}

func main() {
	app := di.New()
	app.Use(digrpc.Module)

	app.Wire[*DB](func() *DB { return &DB{dsn: "postgres://localhost/app"} }).
		OnStop(func(ctx context.Context, db *DB) error { log.Println("db closed"); return nil })
	app.Wire[*Caller](NewCaller).Scoped()
	app.Wire[*Health](NewHealth).Scoped()

	app.Wire[*grpc.Server](NewServer).
		Eager().
		OnStart(func(ctx context.Context, srv *grpc.Server) error {
			// Bind synchronously so a busy port fails Start; serve in the background.
			ln, err := net.Listen("tcp", ":50051")
			if err != nil {
				return err
			}
			log.Println("listening on", ln.Addr())
			go func() {
				// Serve returns nil after GracefulStop or Stop, and ErrServerStopped
				// when a rollback stopped the server before it got here.
				if err := srv.Serve(ln); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
					app.Shutdown(err) // the listener died: stop the whole application
				}
			}()
			return nil
		}).
		// OnDrain runs before anything is stopped, so calls that are still
		// running keep their scopes and dependencies.
		OnDrain(func(ctx context.Context, srv *grpc.Server) error {
			log.Println("draining")
			// GracefulStop takes no context: if the stop context expires first,
			// Stop cuts the remaining calls short.
			stop := context.AfterFunc(ctx, srv.Stop)
			srv.GracefulStop()
			if !stop() {
				return ctx.Err()
			}
			return nil
		}).
		OnStop(func(ctx context.Context, srv *grpc.Server) error { srv.Stop(); return nil })

	// The graph is checked as a call scope would resolve it: the interceptor
	// is provided by the module, the call by each call.
	if err := app.Validate(di.Provided[*digrpc.Call]()).Err(); err != nil {
		log.Fatal(err)
	}

	// Blocks until Ctrl-C, SIGTERM, or app.Shutdown. A second signal cancels
	// the stop context so a hung hook cannot keep the process alive.
	if err := app.Run(context.Background(), di.StopTimeout(10*time.Second)); err != nil {
		log.Fatal(err)
	}
}
