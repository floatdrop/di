// Wire: plain constructors whose parameters are their dependencies, and a
// graph that is checked before anything is built.
package main

import (
	"fmt"
	"net/http"

	"github.com/floatdrop/di"
)

type Config struct{ DSN string }
type DB struct{ dsn string }
type Repo struct{ db *DB }
type User struct{ name string }
type Handler struct {
	repo *Repo
	user *User
}
type Mailer struct{ user *User }

// The constructors know nothing about di.
func NewDB(cfg Config) *DB                       { return &DB{dsn: cfg.DSN} }
func NewRepo(db *DB) *Repo                       { return &Repo{db: db} }
func NewUser(r *http.Request) *User              { return &User{name: r.Header.Get("X-User")} }
func NewHandler(repo *Repo, user *User) *Handler { return &Handler{repo: repo, user: user} }
func NewMailer(user *User) *Mailer               { return &Mailer{user: user} }

func main() {
	app := di.New()
	app.Value(Config{DSN: "postgres://localhost/app"})
	app.Wire[*DB](NewDB)
	app.Wire[*Repo](NewRepo)
	app.Wire[*User](NewUser).Scoped() // one per request scope, where the *http.Request is
	app.Wire[*Handler](NewHandler).Scoped()

	// Nothing has been built, but the constructors declared their edges, so
	// Explain draws them, dashed, down to what only a request scope provides.
	fmt.Print(app.Explain[*Handler]())
	fmt.Println()

	// Validate walks the same edges. From the application scope, *User needs
	// an *http.Request that only a request scope provides: owed, not wrong.
	v := app.Validate()
	fmt.Println("errors:", v.Err())
	fmt.Println("owed:  ", v.Owed)

	// Told what a request scope holds, the check is the one that scope
	// would make, and nothing is owed.
	fmt.Println("request scopes:", app.Validate(di.Provided[*http.Request]()).Err())

	// A singleton depending on a request-scoped service would be built in
	// app, where there is no request. A closure would fail on first use;
	// the declared graph fails here.
	app.Wire[*Mailer](NewMailer)
	fmt.Println(app.Validate().Err())
}
