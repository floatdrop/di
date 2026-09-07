package di_test

import (
	"strings"
	"testing"

	"github.com/floatdrop/di"
)

// A small application in four modules, registered in order, plus one
// registration made outside any module.

type (
	mCfg     struct{}
	mDB      struct{}
	mStore   interface{ Kind() string }
	mPGStore struct{}
	mCache   struct{}
	mReq     struct{}
	mUser    struct{}
	mMailer  struct{}
	mServer  struct{}
)

func (*mPGStore) Kind() string { return "pg" }

func mConfig(s *di.Scope) { s.Value(mCfg{}) }
func mStorage(s *di.Scope) {
	s.Wire[*mDB](func(mCfg) *mDB { return &mDB{} })
	s.Wire[mStore](func(*mDB) *mPGStore { return &mPGStore{} })
}
func mCaching(s *di.Scope) {
	s.Wire[*mCache](func() *mCache { return &mCache{} })
	s.Wrap[mStore](func(next mStore, _ *mCache) mStore { return next })
}
func mAPI(s *di.Scope) {
	s.Wire[*mUser](func(*mReq) *mUser { return &mUser{} }).Scoped()
	s.Provide(func(s *di.Scope) *mServer { _ = s.Get[mStore](); return &mServer{} })
	s.Wire[*mMailer](func(*mUser, *mCfg) *mMailer { return &mMailer{} }) // *mCfg is not provided; mCfg is
}

func TestModulesReport(t *testing.T) {
	app := di.New()
	app.Use(mConfig, mStorage, mCaching, mAPI)
	app.Value(&mReq{}) // outside any module

	want := `di_test.mConfig
  provides   di_test.mCfg
di_test.mStorage
  provides   *di_test.mDB, di_test.mStore
  needs      di_test.mCfg ← di_test.mConfig
di_test.mCaching
  provides   *di_test.mCache
  wraps      di_test.mStore ← di_test.mStorage
di_test.mAPI
  provides   *di_test.mUser, *di_test.mServer, *di_test.mMailer
  needs      *di_test.mReq ← registered directly
             *di_test.mCfg ← not provided
  unchecked  *di_test.mServer (closures: needs known when they run)
registered directly
  provides   *di_test.mReq
`
	if got := app.Modules(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A Scoped binding's dependency that only a resolving scope provides is
// owed, not missing, and a module's dependency on itself is not listed.
func TestModulesReportOwedAndInternal(t *testing.T) {
	app := di.New()
	app.Use(mAPI)
	out := app.Modules()
	if !strings.Contains(out, "*di_test.mReq ← owed to a resolving scope") {
		t.Fatalf("the request should be owed:\n%s", out)
	}
	if strings.Contains(out, "*di_test.mUser ←") {
		t.Fatalf("a module's own service is not a module dependency:\n%s", out)
	}
	// From a child that provides the request, nothing is owed.
	req := app.Child("request")
	req.Value(&mReq{})
	if out := req.Modules(); strings.Contains(out, "owed") {
		t.Fatalf("the child provides the request:\n%s", out)
	}
}

func TestModulesReportBuildsNothing(t *testing.T) {
	built := false
	app := di.New()
	app.Use(func(s *di.Scope) { s.Wire[*mDB](func() *mDB { built = true; return &mDB{} }) })
	_ = app.Modules()
	if built {
		t.Fatal("Modules ran a constructor")
	}
}
