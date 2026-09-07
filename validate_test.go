package di_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/floatdrop/di"
)

// Fixtures for Validate: an application graph with a request-scoped service
// declared in the root and resolved through a child that provides the request.

type valCfg struct{}
type valDB struct{ cfg valCfg }
type valRepo struct{ db *valDB }
type valReq struct{}
type valUser struct{ req *valReq }
type valHandler struct {
	repo *valRepo
	user *valUser
}
type valMailer struct{ user *valUser }
type valA struct{ b *valB }
type valB struct{ a *valA }

func newValDB(cfg valCfg) *valDB                       { return &valDB{cfg} }
func newValRepo(db *valDB) *valRepo                    { return &valRepo{db} }
func newValUser(r *valReq) *valUser                    { return &valUser{r} }
func newValHandler(r *valRepo, u *valUser) *valHandler { return &valHandler{r, u} }
func newValMailer(u *valUser) *valMailer               { return &valMailer{u} }
func newValA(b *valB) *valA                            { return &valA{b} }
func newValB(a *valA) *valB                            { return &valB{a} }

func appGraph(s *di.Scope) {
	s.Value(valCfg{})
	s.Wire[*valDB](newValDB)
	s.Wire[*valRepo](newValRepo)
	s.Wire[*valUser](newValUser).Scoped()
	s.Wire[*valHandler](newValHandler).Scoped()
}

func has(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

func TestValidateCleanGraphOwesTheRequest(t *testing.T) {
	app := di.New()
	appGraph(app)
	v := app.Validate()
	if err := v.Err(); err != nil {
		t.Fatalf("clean graph reported %v", err)
	}
	// *valUser is Scoped and needs *valReq, which only a request scope provides:
	// owed, not an error, from the root.
	if len(v.Owed) != 1 || !has(v.Owed, "valReq") || !has(v.Owed, "valUser") {
		t.Fatalf("want *valReq owed by *valUser, got %v", v.Owed)
	}
	if len(v.Unchecked) != 0 {
		t.Fatalf("nothing is a closure here, got %v", v.Unchecked)
	}
}

func TestValidateFromTheRequestScopeChecksWhatItProvides(t *testing.T) {
	app := di.New()
	appGraph(app)
	req := app.Child("request")
	req.Value(&valReq{})
	v := req.Validate()
	if err := v.Err(); err != nil {
		t.Fatal(err)
	}
	if len(v.Owed) != 0 {
		t.Fatalf("the request scope provides *valReq, yet %v", v.Owed)
	}

	bare := app.Child("request")
	if v := bare.Validate(); len(v.Owed) != 1 || v.Err() != nil {
		t.Fatalf("a child without the request still owes it: %v / %v", v.Owed, v.Err())
	}
}

func TestValidateMissingSingletonDependency(t *testing.T) {
	s := di.New()
	s.Wire[*valRepo](newValRepo) // *valDB is not provided
	v := s.Validate()
	if len(v.Errors) != 1 || !errors.Is(v.Err(), di.ErrNotProvided) {
		t.Fatalf("want one ErrNotProvided, got %v", v.Errors)
	}
	msg := v.Err().Error()
	if !strings.Contains(msg, "valDB") || !strings.Contains(msg, "needed by [*github.com/floatdrop/di_test.valRepo]") || !strings.Contains(msg, "validate_test.go") {
		t.Fatalf("message should name the key, the path and the site: %s", msg)
	}
}

func TestValidateSingletonCapturingAScopedIsAnErrorOnlyWhenItWouldFail(t *testing.T) {
	// *valMailer is a singleton in the root that depends on *valUser, which is
	// Scoped and needs the request. The root cannot provide it, so building
	// *valMailer would fail there: that is proved, and an error.
	app := di.New()
	appGraph(app)
	app.Wire[*valMailer](newValMailer)
	v := app.Validate()
	if len(v.Errors) != 1 || !errors.Is(v.Err(), di.ErrNotProvided) {
		t.Fatalf("want the capture reported once as ErrNotProvided, got %v", v.Errors)
	}
	if msg := v.Err().Error(); !strings.Contains(msg, "valMailer") || !strings.Contains(msg, "valUser") || !strings.Contains(msg, "Scoped") {
		t.Fatalf("message should explain the capture: %s", msg)
	}
	// *valReq is still owed for the Scoped resolution path, independently.
	if !has(v.Owed, "valReq") {
		t.Fatalf("owed list lost the request: %v", v.Owed)
	}

	// The same shape where the Scoped service is satisfiable from the root
	// is not an error: the singleton captures the root's own instance, which
	// is legitimate and what the runtime does.
	s := di.New()
	s.Value(valCfg{})
	s.Wire[*valDB](newValDB).Scoped()
	s.Wire[*valRepo](newValRepo)
	if v := s.Validate(); v.Err() != nil || len(v.Owed) != 0 {
		t.Fatalf("a satisfiable Scoped dependency is not a failure: %v / %v", v.Err(), v.Owed)
	}
}

func TestValidateFindsACycleAmongWireConstructors(t *testing.T) {
	s := di.New()
	s.Wire[*valA](newValA)
	s.Wire[*valB](newValB)
	v := s.Validate()
	if !errors.Is(v.Err(), di.ErrCycle) {
		t.Fatalf("want ErrCycle, got %v", v.Err())
	}
	// One cycle, one line, however many of its members take a turn.
	if len(v.Errors) != 1 {
		t.Fatalf("want the cycle reported once, got %v", v.Errors)
	}
	if msg := v.Errors[0].Error(); !strings.Contains(msg, "-> ") {
		t.Fatalf("the message should draw the path: %s", msg)
	}
}

func TestValidateCannotSeeThroughAClosure(t *testing.T) {
	s := di.New()
	s.Wire[*valA](newValA)
	s.Provide(func(s *di.Scope) *valB { return &valB{s.Get[*valA]()} }) // the cycle's other half is opaque
	v := s.Validate()
	if v.Err() != nil {
		t.Fatalf("a closure hides its dependencies, yet %v", v.Err())
	}
	if len(v.Unchecked) != 1 || !has(v.Unchecked, "valB") {
		t.Fatalf("the closure should be listed as unchecked, got %v", v.Unchecked)
	}
	// The runtime still finds the cycle, which is why Unchecked exists.
	if _, err := s.Resolve[*valA](); !errors.Is(err, di.ErrCycle) {
		t.Fatalf("runtime should report the cycle, got %v", err)
	}
}

func TestValidateChecksAncestorsFromAChild(t *testing.T) {
	app := di.New()
	app.Wire[*valRepo](newValRepo) // missing *valDB in the root
	req := app.Child("request")
	req.Value(&valDB{}) // provided in the child: does not help a root singleton
	v := req.Validate()
	if !errors.Is(v.Err(), di.ErrNotProvided) {
		t.Fatalf("a root singleton is built in the root, so the child's *valDB cannot satisfy it: %v", v.Err())
	}
}

func TestValidateBuildsNothingAndIsRepeatable(t *testing.T) {
	s := di.New()
	built := false
	s.Wire[*valDB](func(valCfg) *valDB { built = true; return &valDB{} })
	s.Value(valCfg{})
	s.Wire[*valRepo](newValRepo)
	first := s.Validate()
	second := s.Validate()
	if built {
		t.Fatal("Validate ran a constructor")
	}
	if first.Err() != nil || len(first.Owed) != len(second.Owed) || len(first.Unchecked) != len(second.Unchecked) {
		t.Fatalf("results differ between calls: %+v / %+v", first, second)
	}
}

func TestValidateReportsADiamondOnce(t *testing.T) {
	s := di.New()
	s.Wire[*valRepo](newValRepo)
	s.Wire[*valUser](func(*valRepo) *valUser { return nil }).Scoped()
	s.Wire[*valHandler](func(*valRepo, *valUser) *valHandler { return nil }).Scoped()
	v := s.Validate()
	// *valDB is missing for the singleton *valRepo: one error. The two Scoped
	// bindings reach *valRepo, a singleton checked on its own turn, and add
	// nothing.
	if len(v.Errors) != 1 || len(v.Owed) != 0 {
		t.Fatalf("want one error and no owed lines, got %v / %v", v.Errors, v.Owed)
	}
}

func TestValidateGroupMembersAreChecked(t *testing.T) {
	s := di.New()
	s.Wire[*valRepo](newValRepo).Group()
	s.Wire[*valRepo](func() *valRepo { return nil }).Group()
	v := s.Validate()
	if len(v.Errors) != 1 || !errors.Is(v.Err(), di.ErrNotProvided) {
		t.Fatalf("the member that needs *valDB should be reported once, got %v", v.Errors)
	}
}

func TestValidateIgnoresAReplacedRegistration(t *testing.T) {
	s := di.New()
	s.Wire[*valRepo](newValRepo)                                       // needs *valDB, which is missing
	s.Wire[*valRepo](func() *valRepo { return &valRepo{} }).Override() // replaces it
	if v := s.Validate(); v.Err() != nil {
		t.Fatalf("the replaced registration no longer serves anything: %v", v.Err())
	}
}

func TestValidateCommitsPendingLikeAResolution(t *testing.T) {
	s := di.New()
	s.Wire[*valRepo](newValRepo)
	s.Wire[*valRepo](func() *valRepo { return nil }) // a collision without Override
	defer func() {
		msg, ok := recover().(string)
		if !ok || !strings.HasPrefix(msg, "di: ") {
			t.Fatalf("want the configuration rejection, got %v", msg)
		}
	}()
	s.Validate()
	t.Fatal("Validate accepted a colliding registration")
}

// The stubs describe the scope that will resolve a Scoped binding, so the
// check a request scope would make is made from the application scope: what
// the stubs cover is satisfied, and what remains unmet is an error rather
// than owed. Without stubs nothing changes.
func TestValidateWithStubsIsALeaf(t *testing.T) {
	app := di.New()
	appGraph(app)
	v := app.Validate(di.Provided[*valReq]())
	if err := v.Err(); err != nil || len(v.Owed) != 0 {
		t.Fatalf("the stub covers the request, yet err=%v owed=%v", err, v.Owed)
	}

	app.Wire[*valMailer](func(*valUser, *valCfg) *valMailer { return nil }).Scoped() // *valCfg is registered as a value, but a *valCfg is not
	v = app.Validate(di.Provided[*valReq]())
	if !errors.Is(v.Err(), di.ErrNotProvided) || !strings.Contains(v.Err().Error(), "valCfg") || !strings.Contains(v.Err().Error(), "stubs") {
		t.Fatalf("what neither the scope nor the stubs provide is an error at a leaf, got %v", v.Err())
	}
	if len(v.Owed) != 0 {
		t.Fatalf("a leaf owes nothing, got %v", v.Owed)
	}
	if v := app.Validate(); v.Err() != nil || len(v.Owed) != 2 {
		t.Fatalf("without stubs both are owed and neither is an error: %v / %v", v.Err(), v.Owed)
	}
}

// A stub is a key, so an interface a request scope provides is stubbed by
// its interface type, and a stub satisfies only the Scoped path: a singleton
// that would build a Scoped service in its own scope still fails there.
func TestValidateStubsAreKeysAndDoNotReachSingletons(t *testing.T) {
	type principal interface{ Name() string }
	s := di.New()
	s.Wire[*valUser](func(principal) *valUser { return nil }).Scoped()
	if v := s.Validate(di.Provided[principal]()); v.Err() != nil {
		t.Fatalf("an interface stub should satisfy the interface key: %v", v.Err())
	}

	app := di.New()
	appGraph(app)
	app.Wire[*valMailer](newValMailer) // a singleton capturing the request-scoped *valUser
	v := app.Validate(di.Provided[*valReq]())
	if len(v.Errors) != 1 || !strings.Contains(v.Err().Error(), "valMailer") {
		t.Fatalf("the capture is a failure in root whatever a request scope holds, got %v", v.Errors)
	}
}

func TestValidateWithStubsBuildsNothing(t *testing.T) {
	built := false
	app := di.New()
	app.Wire[*valUser](func(*valReq) *valUser { built = true; return nil }).Scoped()
	if err := app.Validate(di.Provided[*valReq]()).Err(); err != nil || built {
		t.Fatalf("err=%v built=%v", err, built)
	}
}

type valSrc struct{}
type valScopedSvc struct{ src *valSrc }
type valSingleton struct{ svc *valScopedSvc }

// A valid graph can visit one Scoped binding in two scopes: from a child,
// the scoped service needs a source the child provides, whose constructor
// needs a root singleton, which needs the same scoped service built in the
// root, which needs the root's source. Run time resolves it, since a path
// node is a binding in a holder; Validate compared bindings alone and called
// it a cycle. (issue 35)
func TestValidateTellsScopedInstancesApartByHolder(t *testing.T) {
	app := di.New()
	app.Value(&valSrc{})
	app.Wire[*valScopedSvc](func(s *valSrc) *valScopedSvc { return &valScopedSvc{s} }).Scoped()
	app.Wire[*valSingleton](func(s *valScopedSvc) *valSingleton { return &valSingleton{s} })
	child := app.Child("child")
	child.Wire[*valSrc](func(s *valSingleton) *valSrc { return s.svc.src })

	if err := child.Validate().Err(); err != nil {
		t.Fatalf("a graph the runtime resolves must validate: %v", err)
	}
	if _, err := child.Resolve[*valScopedSvc](); err != nil {
		t.Fatalf("runtime: %v", err)
	}

	// The same binding twice under one holder is still a cycle.
	cyc := di.New()
	cyc.Wire[*valScopedSvc](func(*valSingleton) *valScopedSvc { return nil }).Scoped()
	cyc.Wire[*valSingleton](func(*valScopedSvc) *valSingleton { return nil }).Scoped()
	if !errors.Is(cyc.Validate().Err(), di.ErrCycle) {
		t.Fatal("a real cycle among scoped bindings must still be reported")
	}
}
