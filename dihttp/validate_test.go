package dihttp_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/floatdrop/di"
	"github.com/floatdrop/di/dihttp"
)

type Session struct{}
type Greeter struct{ who *Caller }
type Auditor struct{ s *Session }

func newCaller(r *http.Request) *Caller { return &Caller{Name: r.Header.Get("X-User")} }
func newGreeter(c *Caller) *Greeter     { return &Greeter{c} }
func newAuditor(s *Session) *Auditor    { return &Auditor{s} }

func TestValidateAcceptsAGraphTheRequestScopeSatisfies(t *testing.T) {
	app := di.New()
	app.Wire[*Caller](newCaller).Scoped()
	app.Wire[*Greeter](newGreeter).Scoped()
	if v := app.Validate(); len(v.Owed) != 1 || v.Err() != nil {
		t.Fatalf("from the application scope the request is owed, nothing more: %v / %v", v.Owed, v.Err())
	}
	if err := dihttp.Validate(app); err != nil {
		t.Fatalf("a request scope provides *http.Request, yet %v", err)
	}
}

func TestValidateReportsWhatNoRequestScopeProvides(t *testing.T) {
	app := di.New()
	app.Wire[*Caller](newCaller).Scoped()
	app.Wire[*Auditor](newAuditor).Scoped() // *Session comes from nowhere
	err := dihttp.Validate(app)
	if !errors.Is(err, di.ErrNotProvided) || !strings.Contains(err.Error(), "Session") || !strings.Contains(err.Error(), "request scope") {
		t.Fatalf("want *Session reported as not provided in a request scope, got %v", err)
	}
}

func TestValidateBuildsNothing(t *testing.T) {
	app := di.New()
	built := false
	app.Wire[*Caller](func(r *http.Request) *Caller { built = true; return newCaller(r) }).Scoped()
	if err := dihttp.Validate(app); err != nil || built {
		t.Fatalf("err=%v built=%v", err, built)
	}
}
