package di

// Checking the declared graph. Only a constructor registered with Wire
// declares its dependencies; a Provide closure reveals them as it runs, and is
// reported here as unchecked. Nothing in this file resolves or builds: it
// reads bindings and their declared dependency lists after committing pending
// registrations, exactly as a lookup would.

import (
	"errors"
	"fmt"
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
	// these are left to that scope: call Validate there, or dihttp.Validate
	// for request scopes, to have them checked as errors.
	Owed []string
	// Unchecked lists the Provide constructors in the chain, whose
	// dependencies are known only once they run.
	Unchecked []string
}

// Err joins Errors, or is nil when the declared graph proves no failure.
func (v Validation) Err() error { return errors.Join(v.Errors...) }

// Validate checks the wiring visible from this scope without building
// anything. A singleton is checked against the scope that registered it,
// since that is where it is built; a Scoped binding is checked as if resolved
// from this scope, and what this scope does not provide for it is reported as
// Owed rather than as an error, because a descendant may. Like Explain, it
// commits pending registrations the way a resolution would, so a
// configuration this scope would reject is reported by the same panic.
func (s *Scope) Validate() Validation {
	var chain []*state
	for st := s.state; st != nil; st = st.parent {
		st.freeze()
		chain = append(chain, st)
	}
	v := &validator{seen: map[string]bool{}, done: map[visit]bool{}}
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
	for _, b := range st.index {
		serving[b] = true
	}
	for _, bs := range st.groups {
		for _, b := range bs {
			serving[b] = true
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
	out  Validation
	seen map[string]bool // lines already reported, and cycles by their members
	done map[visit]bool  // nodes fully explored, so a diamond is walked once
}

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

// walk follows b's declared dependencies from holder, the scope b would be
// built in. A Scoped dependency is built in the same holder and walked in the
// same mode. A singleton dependency is built in its own scope, and its
// missing dependencies are that scope's report on its own turn, so it is
// walked only for cycles. Cycles are reported once, by their members.
func (v *validator) walk(b *binding, holder *state, md mode, path []*binding) {
	node := visit{b, holder, md}
	if v.done[node] {
		return
	}
	path = append(path, b)
	for _, k := range b.wants {
		dep, owner := (&Scope{state: holder}).lookup(k)
		switch {
		case dep == nil:
			v.missing(k, b, holder, md, path)
		case slices.Contains(path, dep):
			v.cycle(path[slices.Index(path, dep):], k)
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

func (v *validator) missing(k key, b *binding, holder *state, md mode, path []*binding) {
	switch {
	case md == cyclesOnly:
	case md == lenient:
		v.owed(fmt.Sprintf("%s: needed by %s (scoped, provided at %s)", k, b.key, b.site))
	case len(path) > 1:
		v.err(fmt.Errorf("di: %s: %w in scope %s (needed by %s; %s is Scoped, so the singleton %s would build it there)",
			k, ErrNotProvided, holder.name, keysOf(path), b.key, path[0].key))
	default:
		v.err(fmt.Errorf("di: %s: %w (needed by %s, provided at %s)", k, ErrNotProvided, keysOf(path), b.where()))
	}
}

// cycle reports the members once however many turns reach them, so a cycle
// of two is one line rather than one per participant.
func (v *validator) cycle(members []*binding, closing key) {
	names := make([]string, len(members))
	for i, b := range members {
		names[i] = b.key.String()
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

// keysOf renders a path the way a resolution error does.
func keysOf(path []*binding) string {
	keys := make([]string, len(path))
	for i, b := range path {
		keys[i] = b.key.String()
	}
	return fmt.Sprint(keys)
}
