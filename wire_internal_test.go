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

// Both forms record the same dependency list, and neither disturbs the
// registration site, which is read a fixed number of frames up the stack.
func TestWireRecordsDependencies(t *testing.T) {
	s := New()
	typed := s.Wire2(newWiRepo).b
	reflective := s.Wire[*wiRepo](newWiRepo).b
	none := s.Wire0(func() *wiDB { return &wiDB{} }).b
	closure := s.Provide(func(*Scope) *wiCfg { return &wiCfg{} }).b

	want := []key{{t: reflect.TypeFor[wiCfg]()}, {t: reflect.TypeFor[*wiDB]()}}
	for name, b := range map[string]*binding{"typed": typed, "reflective": reflective} {
		if !reflect.DeepEqual(b.wants, want) {
			t.Errorf("%s: wants %v, want %v", name, b.wants, want)
		}
		if !strings.Contains(b.site, "wire_internal_test.go") {
			t.Errorf("%s: site %q should be this file", name, b.site)
		}
	}
	if none.wants == nil || len(none.wants) != 0 {
		t.Errorf("Wire0 should declare an empty list, got %v", none.wants)
	}
	if closure.wants != nil {
		t.Errorf("Provide should declare nothing, got %v", closure.wants)
	}
}
