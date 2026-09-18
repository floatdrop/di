package di

import (
	"reflect"
	"strings"
	"testing"
)

type wiCfg struct{}
type wiDB struct{}
type wiRepo struct{}

func newWiRepo(wiCfg, *wiDB) *wiRepo { return &wiRepo{} }

// Wire records its parameter types and, calling register directly, leaves the
// registration site pointing at the caller, which is read a fixed number of
// frames up the stack.
func TestWireRecordsDependencies(t *testing.T) {
	s := New()
	wired := s.Wire[*wiRepo](newWiRepo).b
	none := s.Wire[*wiDB](func() *wiDB { return &wiDB{} }).b
	closure := s.Provide(func(*Scope) *wiCfg { return &wiCfg{} }).b

	cfg, db := reflect.TypeFor[wiCfg](), reflect.TypeFor[*wiDB]()
	want := []want{
		{k: key{t: cfg}, param: cfg, kind: wantValue},
		{k: key{t: db}, param: db, kind: wantValue},
	}
	if !reflect.DeepEqual(wired.wants, want) {
		t.Errorf("wants %v, want %v", wired.wants, want)
	}
	if !strings.Contains(wired.site, "wire_internal_test.go") {
		t.Errorf("site %q should be this file", wired.site)
	}
	if none.wants == nil || len(none.wants) != 0 {
		t.Errorf("a zero-arity constructor should declare an empty list, got %v", none.wants)
	}
	if closure.wants != nil {
		t.Errorf("Provide should declare nothing, got %v", closure.wants)
	}
}
