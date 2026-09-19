// Graceful shutdown of a gRPC server, with a service built per call.
//
// The digrpc interceptor opens a scope for every call and registers the
// *digrpc.Call in it, so a Caller declared Scoped is built per call from the
// call's metadata. Run starts the scope, waits for SIGINT/SIGTERM or a
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

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/digrpc"
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

// Health is a service implementation. grpc registers one value for the whole
// server, so it is a singleton that reaches per-call services through the
// scope on its context.
type Health struct {
	grpc_health_v1.UnimplementedHealthServer
	db *DB
}

func NewHealth(db *DB) *Health { return &Health{db: db} }

func (h *Health) Check(ctx context.Context, _ *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	scope, _ := di.FromContext(ctx)
	caller := scope.Get[*Caller]()
	time.Sleep(2 * time.Second) // simulate slow work that must not be cut short
	log.Println("checked by", caller.Name, "against", h.db.dsn)
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

// NewServer is a plain constructor: the interceptor arrives as a dependency.
func NewServer(ic digrpc.Interceptor, h *Health) *grpc.Server {
	srv := grpc.NewServer(ic.Options()...)
	grpc_health_v1.RegisterHealthServer(srv, h)
	return srv
}

func main() {
	app := di.New()
	app.Use(digrpc.Module)

	app.Wire[*DB](func() *DB { return &DB{dsn: "postgres://localhost/app"} }).
		OnStop(func(ctx context.Context, db *DB) error { log.Println("db closed"); return nil })
	app.Wire[*Caller](NewCaller).Scoped()
	app.Wire[*Health](NewHealth)

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
