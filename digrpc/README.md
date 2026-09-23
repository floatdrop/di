# digrpc

`google.golang.org/grpc` on a [`golang.yandex/di`](https://github.com/yandex/di)
scope. A separate module, so the library itself stays dependency-free.

```sh
go get golang.yandex/di/digrpc
```

`Interceptor` opens a child scope per call holding a `*digrpc.Call` — the full
method name and the context the call arrived with, so its metadata, peer and
deadline are all there. The scope is attached to the handler's context and
stopped when the handler returns. `Module` registers the interceptor, so a
constructor can take one:

```go
func NewServer(ic digrpc.Interceptor) *grpc.Server {
	srv := grpc.NewServer(ic.Options()...)
	digrpc.Register[*Users](srv, &pb.Users_ServiceDesc)
	return srv
}
```

```go
app := di.New()
app.Use(digrpc.Module)
app.Wire[*Users](NewUsers).Scoped() // takes the *digrpc.Call, so built per call
app.Wire[*grpc.Server](NewServer)
```

There are two ways for a service to reach the call's scope. Registered the
generated way, with `pb.RegisterUsersServer`, it is one value for the whole
server and reads the scope off the context it is handed, with
`di.FromContext`. `Register[H]` instead resolves the implementation from each
call's scope, so it is a service like any other: `Scoped()` when it takes the
call, built after the server's interceptors have run, and a constructor
returning a status error fails the call with that status.

A call scope is a leaf as far as the declared graph goes, so validate it as
one:

```go
app.Validate(di.Provided[*digrpc.Call]()).Err()
```

Graceful shutdown is yours to write, and it is one hook — `OnDrain` calling
`srv.GracefulStop()`, which runs before anything is torn down, so the calls
still in flight still have their scopes. `examples/grpc` in the main
repository is that program end to end.

Versioned on its own as `digrpc/vX.Y.Z`, against a released `di`; bumping that
requirement is how it picks up a library change. Its tests serve grpc's own
health service over `bufconn`, so nothing here is generated from protobuf.

```sh
cd digrpc && go test -race ./... && golangci-lint run ./...
```
