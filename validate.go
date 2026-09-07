package di

// Checking the declared graph. Only a constructor registered with Wire
// declares its dependencies; a Provide closure reveals them as it runs, and is
// reported here as unchecked. Nothing in this file resolves or builds: it
// reads bindings and their declared dependency lists after committing pending
// registrations, exactly as a lookup would.

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
)

// Validation is what Validate found.
type Validation struct {
	// Errors are the failures the declared graph proves: a dependency nothing
	// provides, a cycle among Wire constructors, or a singleton that would
	// build a Scoped service in its own scope, where that service's
	// dependencies are not provided. Each wraps ErrNotProvided or ErrCycle.
	Errors []error
	// Owed lists the dependencies of Scoped bindings that this scope does not
	// provide. A Scoped service is built in the scope that resolves it, so
	// these are left to that scope: call Validate from there, or say what it
	// will hold with Provided stubs, and they are checked as errors instead.
	Owed []string
	// Unchecked lists the Provide constructors in the chain, whose
	// dependencies are known only once they run.
	Unchecked []string
}

// Err joins Errors, or is nil when the declared graph proves no failure.
func (v Validation) Err() error { return errors.Join(v.Errors...) }

// Validate checks the wiring visible from this scope without building
// anything. A singleton is checked against the scope that registered it,
// since that is where it is built. A Scoped binding is checked as if resolved
// from this scope, and what this scope does not provide for it is reported as
// Owed rather than as an error, because a descendant may:
//
//	v := app.Validate() // *http.Request is owed to a request scope
//
// The stubs say what such a descendant will hold, so that the check can be
// made from the application scope as that descendant would make it. With
// stubs the caller has described the resolving scope, and a dependency neither
// this scope nor the stubs provide is an error:
//
//	err := app.Validate(di.Provided[*http.Request]()).Err()
//
// Like Explain, Validate commits pending registrations the way a resolution
// would, so a configuration this scope would reject is reported by the same
// panic.
func (s *Scope) Validate(stubs ...Stub) Validation {
	var chain []*state
	for st := s.state; st != nil; st = st.parent {
		st.freeze()
		chain = append(chain, st)
	}
	v := &validator{seen: map[string]bool{}, done: map[visit]bool{}, stubs: map[key]bool{}, leaf: len(stubs) > 0}
	for _, st := range stubs {
		v.stubs[st.k] = true
	}
	// Ancestors first, so a report reads top-down like the scope tree.
	for _, st := range slices.Backward(chain) {
		for _, b := range st.live() {
			switch {
			case b.isValue:
			case b.wants == nil:
				v.out.Unchecked = append(v.out.Unchecked, fmt.Sprintf("%s (provided at %s)", b.key, b.where()))
			case b.scoped:
				v.walk(b, s.state, lenient, nil)
			default:
				v.walk(b, st, strict, nil)
			}
		}
	}
	return v.out
}

// live returns the bindings that can serve a key from this scope, in
// registration order: what index and groups hold, without the registrations
// an Override replaced.
func (st *state) live() []*binding {
	st.mu.Lock()
	defer st.mu.Unlock()
	serving := map[*binding]bool{}
	// A wrapper serves the key and what it wraps is built underneath it,
	// so the whole chain is live; a chain an Override replaced is not.
	chain := func(b *binding) {
		for ; b != nil && !serving[b]; b = b.inner {
			serving[b] = true
		}
	}
	for _, b := range st.index {
		chain(b)
	}
	for _, bs := range st.groups {
		for _, b := range bs {
			chain(b)
		}
	}
	var out []*binding
	for _, b := range st.all {
		if serving[b] {
			out = append(out, b)
		}
	}
	return out
}

type validator struct {
	out   Validation
	seen  map[string]bool // lines already reported, and cycles by their members
	done  map[visit]bool  // nodes fully explored, so a diamond is walked once
	stubs map[key]bool    // what the resolving scope will hold, by the caller's word
	leaf  bool            // stubs were given: the resolving scope is described, so nothing is owed
}

// Stub names a key the scope resolving a Scoped binding will provide, for
// Validate to take as given. Make one with Provided.
type Stub struct{ k key }

// Provided is a Stub for T: the resolving scope will hold a T, as a request
// scope holds an *http.Request.
func Provided[T any]() Stub { return Stub{k: key{t: reflect.TypeFor[T]()}} }

// A node of the declared graph is a binding in the scope it would be built
// in. The same binding is a different node under a different holder, since a
// Scoped binding built in one scope looks its dependencies up from there.
type visit struct {
	b      *binding
	holder *state
	mode   mode
}

// mode says what a missing dependency means on the current walk.
type mode uint8

const (
	strict     mode = iota // a singleton's own graph: missing is an error
	lenient                // a Scoped binding as this scope would resolve it: missing is owed to a descendant
	cyclesOnly             // a singleton reached from elsewhere: its own turn reports what it misses
)

// step is one node on a walk's path: a binding in the scope it would be
// built in. The holder is part of the identity, as it is on a resolution
// path at run time: a Scoped binding reached again under another holder is
// another instance, not a cycle, and a valid graph can visit one twice.
type step struct {
	b      *binding
	holder *state
}

// walk follows b's declared dependencies from holder, the scope b would be
// built in. A Scoped dependency is built in the same holder and walked in the
// same mode. A singleton dependency is built in its own scope, and its
// missing dependencies are that scope's report on its own turn, so it is
// walked only for cycles. Cycles are reported once, by their members.
func (v *validator) walk(b *binding, holder *state, md mode, path []step) {
	node := visit{b, holder, md}
	if v.done[node] {
		return
	}
	path = append(path, step{b, holder})
	for _, e := range declared(b, holder) {
		k, dep, owner := e.k, e.b, e.owner
		next := step{dep, owner}
		if dep != nil && dep.scoped {
			next.holder = holder
		}
		switch {
		case dep == nil && md == lenient && v.stubs[k]:
			// The resolving scope will hold it, the caller says, and a value
			// declares nothing further. Only on the Scoped path: a singleton
			// builds in its own scope, where that scope's values are not.
		case dep == nil:
			v.missing(k, b, holder, md, path)
		case slices.Contains(path, next):
			v.cycle(path[slices.Index(path, next):], k)
		case dep.wants == nil:
			// A Provide closure or a Value: nothing declared to follow.
		case dep.scoped:
			v.walk(dep, holder, md, path)
		default:
			v.walk(dep, owner, cyclesOnly, path)
		}
	}
	v.done[node] = true
}

func (v *validator) missing(k key, b *binding, holder *state, md mode, path []step) {
	switch {
	case md == cyclesOnly:
	case md == lenient && v.leaf:
		v.err(fmt.Errorf("di: %s: %w by this scope or the stubs (needed by %s; scoped, provided at %s)", k, ErrNotProvided, keysOf(path), b.site))
	case md == lenient:
		v.owed(fmt.Sprintf("%s: needed by %s (scoped, provided at %s)", k, b.key, b.site))
	case len(path) > 1:
		v.err(fmt.Errorf("di: %s: %w in scope %s (needed by %s; %s is Scoped, so the singleton %s would build it there)",
			k, ErrNotProvided, holder.name, keysOf(path), b.key, path[0].b.key))
	default:
		v.err(fmt.Errorf("di: %s: %w (needed by %s, provided at %s)", k, ErrNotProvided, keysOf(path), b.where()))
	}
}

// cycle reports the members once however many turns reach them, so a cycle
// of two is one line rather than one per participant.
func (v *validator) cycle(members []step, closing key) {
	names := make([]string, len(members))
	for i, m := range members {
		names[i] = m.b.key.String()
	}
	slices.Sort(names)
	id := "cycle " + fmt.Sprint(names)
	if !v.seen[id] {
		v.seen[id] = true
		v.err(fmt.Errorf("di: %w: %s -> %s", ErrCycle, keysOf(members), closing))
	}
}

func (v *validator) err(e error) {
	if !v.seen[e.Error()] {
		v.seen[e.Error()] = true
		v.out.Errors = append(v.out.Errors, e)
	}
}

func (v *validator) owed(line string) {
	if !v.seen[line] {
		v.seen[line] = true
		v.out.Owed = append(v.out.Owed, line)
	}
}

// edge is one declared dependency: the key, and the binding it resolves to
// from the holder with the scope that registered it, or nil.
type edge struct {
	k     key
	b     *binding
	owner *state
}

// declared lists what b declares, in build order: the registration a wrapper
// composes over, which is bound rather than looked up, and then the
// parameter types, each looked up from holder as the build would.
func declared(b *binding, holder *state) []edge {
	out := make([]edge, 0, len(b.wants)+1)
	if b.inner != nil {
		out = append(out, edge{b.key, b.inner, b.innerAt})
	}
	for _, k := range b.wants {
		dep, owner := (&Scope{state: holder}).lookup(k)
		out = append(out, edge{k, dep, owner})
	}
	return out
}

// keysOf renders a path the way a resolution error does.
func keysOf(path []step) string {
	keys := make([]string, len(path))
	for i, s := range path {
		keys[i] = s.b.key.String()
	}
	return fmt.Sprint(keys)
}
