package di

// Checking the declared graph. Only a Wire constructor declares its
// dependencies; a Provide closure reveals them as it runs and is reported as
// unchecked. Nothing here resolves or builds: it reads bindings and their
// declared lists after committing pending registrations, as a lookup would.

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
// The stubs say what such a descendant will hold, so the check is made as
// that descendant would make it, and a dependency neither this scope nor the
// stubs provide is an error:
//
//	err := app.Validate(di.Provided[*http.Request]()).Err()
//
// Like Explain, Validate commits pending registrations as a resolution would,
// so a configuration this scope would reject is reported by the same panic.
func (s *Scope) Validate(stubs ...Stub) Validation {
	var chain []*state
	for st := s.st; st != nil; st = st.parent {
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
				v.walk(b, s.st, lenient, nil)
			default:
				v.walk(b, st, strict, nil)
			}
		}
	}
	return v.out
}

// live returns the bindings that can serve a key from this scope, in
// registration order: what index and groups hold, without the registrations
// an Override replaced. A wrapper's whole chain is live, since what it wraps
// is built underneath it.
func (st *state) live() []*binding {
	reg := st.reg.Load()
	serving := map[*binding]bool{}
	chain := func(b *binding) {
		for ; b != nil && !serving[b]; b = b.inner {
			serving[b] = true
		}
	}
	for _, b := range reg.index {
		chain(b)
	}
	for _, bs := range reg.groups {
		for _, b := range bs {
			chain(b)
		}
	}
	var out []*binding
	for _, b := range reg.all {
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

// visit is a node of the declared graph: a binding in the scope it would be
// built in. The same binding under another holder is another node, since a
// Scoped binding looks its dependencies up from where it is built.
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

// step is one node on a walk's path. The holder is part of the identity, as
// on a resolution path at run time: a Scoped binding reached again under
// another holder is another instance, not a cycle.
type step struct {
	b      *binding
	holder *state
}

// walk follows b's declared dependencies from holder, the scope b would be
// built in. A Scoped dependency is built in the same holder and walked in the
// same mode. A singleton dependency is built in its own scope and reports its
// own misses on its own turn, so it is walked only for cycles. Cycles are
// reported once, by their members.
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
		case dep == nil && e.optional:
			// Nothing provides it and the constructor said it can do without.
		case dep == nil && md == lenient && v.stubs[k]:
			// A stub is honoured on the Scoped path only: a singleton builds
			// in its own scope, where the resolving scope's values are not.
		case dep == nil:
			v.missing(k, b, holder, md, path)
		case slices.Contains(path, next):
			v.cycle(path[slices.Index(path, next):], k)
		case dep.wants == nil:
			// A Provide closure or a Value declares nothing to follow.
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

// cycle reports the members once however many turns reach them.
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
// from the holder with the scope that registered it, or nil. optional marks a
// parameter Needs made optional, whose nil binding is not a failure.
type edge struct {
	k        key
	b        *binding
	owner    *state
	optional bool
}

// declared lists what b declares, in build order: the registration a wrapper
// composes over, bound rather than looked up, then its parameters, each looked
// up from holder as the build would. A group parameter is one edge per member,
// since that is what the build resolves; an empty group declares nothing, as
// an empty group is no failure.
func declared(b *binding, holder *state) []edge {
	out := make([]edge, 0, len(b.wants)+1)
	if b.inner != nil {
		out = append(out, edge{k: b.key, b: b.inner, owner: b.innerAt})
	}
	for _, w := range b.wants {
		if w.kind == wantGroup {
			for _, m := range (&Scope{st: holder}).groupMembers(w.k) {
				out = append(out, edge{k: w.k, b: m.b, owner: m.owner})
			}
			continue
		}
		dep, owner := (&Scope{st: holder}).lookup(w.k)
		out = append(out, edge{k: w.k, b: dep, owner: owner, optional: w.kind == wantOptional})
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
