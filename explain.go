package di

// Rendering the recorded graph. Nothing here participates in resolution or
// teardown: it reads the edges di.go records while constructors run, under
// the same mutex that guards every other field of an instance, and never
// holds two of those at once. It is also the only part of the package that
// builds strings for a person rather than for an error.

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Explain renders what T resolves to and what it was built from: the
// dependency tree, each node with its lifetime, its scope, the state of its
// lifecycle and where it was registered, followed by what needed it.
//
// Only what has been built has a tree, because a constructor's dependencies
// are recorded as it resolves them. A service that has not been built yet is
// reported as such, with its registration, and so is one that is not provided
// at all. Explain resolves nothing and builds nothing; it commits pending
// registrations the way a resolution from this scope would, so a
// configuration this scope would reject is reported here by the same panic.
//
// A key served by a group is explained member by member. A dependency reached
// twice, as in a diamond, is expanded once and named on later visits, so the
// tree stays finite and the repeat is visibly the same instance.
func (s *Scope) Explain[T any]() string {
	k := key{t: reflect.TypeFor[T]()}
	b, owner := s.lookup(k)
	members := s.groupMembers(k)
	if b == nil && len(members) == 0 {
		return fmt.Sprintf("%s: not provided\n", k)
	}

	var sb strings.Builder
	seen := map[*instance]bool{}
	if b != nil {
		s.explainOne(&sb, b, owner, seen)
	}
	for _, m := range members {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		s.explainOne(&sb, m.b, m.owner, seen)
	}
	return sb.String()
}

// found is a binding and the scope that registered it, which is what group
// lookup has to carry and single-key lookup returns as a pair.
type found struct {
	b     *binding
	owner *state
}

// groupMembers lists the group registered for k across the scope chain, in
// the order All resolves them.
func (s *Scope) groupMembers(k key) []found {
	var out []found
	for st := s.state; st != nil; st = st.parent {
		st.freeze()
		st.mu.Lock()
		bs := slices.Clone(st.groups[k])
		st.mu.Unlock()
		for _, b := range bs {
			out = append(out, found{b: b, owner: st})
		}
	}
	return out
}

// explainOne renders one binding's tree, and the instances that needed it.
func (s *Scope) explainOne(sb *strings.Builder, b *binding, owner *state, seen map[*instance]bool) {
	holder := owner
	if b.scoped {
		holder = s.state
	}
	holder.mu.Lock()
	in := holder.instanceAt(b)
	holder.mu.Unlock()
	if in == nil {
		// A Scoped binding this scope has never resolved: the registration
		// is known, the instance does not exist.
		sb.WriteString(describe(b, holder, "not built") + "\n")
		return
	}

	root := dep{in: in, holder: holder}
	phase, deps := root.inspect()
	sb.WriteString(describe(b, holder, phase) + "\n")
	seen[in] = true
	explainInto(sb, deps, "", seen)

	if by := dependentsOf(s.root(), in); len(by) > 0 {
		names := make([]string, len(by))
		for i, d := range by {
			names[i] = d.in.b.key.String() + " in " + d.holder.name
		}
		sb.WriteString("needed by: " + strings.Join(names, ", ") + "\n")
	}
}

// explainInto writes one level of the tree and recurses, drawing the spine
// with the usual box characters.
func explainInto(sb *strings.Builder, deps []dep, prefix string, seen map[*instance]bool) {
	for i, d := range deps {
		branch, pad := "├── ", "│   "
		if i == len(deps)-1 {
			branch, pad = "└── ", "    "
		}
		sb.WriteString(prefix + branch)
		if seen[d.in] {
			// The same instance by another route. Naming it without its
			// subtree keeps a diamond from being drawn twice, and says that
			// it is one value rather than two of a type.
			sb.WriteString(d.in.b.key.String() + ": see above\n")
			continue
		}
		seen[d.in] = true
		phase, next := d.inspect()
		sb.WriteString(describe(d.in.b, d.holder, phase) + "\n")
		explainInto(sb, next, prefix+pad, seen)
	}
}

// Graph renders everything built in this scope and its descendants as
// Graphviz DOT: one box per instance, one cluster per scope that holds any,
// and an arrow from each instance to what its constructor resolved.
//
// It reads the graph and changes nothing, not even the pending registrations,
// so it is safe to call from a handler or a hook. Nodes are numbered in the
// order the scopes were created and the instances were built, so the same run
// of the same program renders the same document. A scope that has been
// stopped no longer holds its instances and contributes nothing.
//
// The detail is deliberately thin -- a registration site would not fit in a
// box. Use Explain for one service in full.
func (s *Scope) Graph() string {
	scopes := walkScopes(s.state)
	type node struct {
		d     dep
		id    int
		phase string
	}
	ids := map[*instance]int{}
	byScope := make([][]node, len(scopes))
	var all []node
	for i, st := range scopes {
		st.mu.Lock()
		built := slices.Clone(st.started)
		st.mu.Unlock()
		for _, in := range built {
			d := dep{in: in, holder: st}
			phase, _ := d.inspect()
			n := node{d: d, id: len(all), phase: phase}
			ids[in] = n.id
			all = append(all, n)
			byScope[i] = append(byScope[i], n)
		}
	}

	var sb strings.Builder
	sb.WriteString("digraph di {\n")
	sb.WriteString("  rankdir=LR;\n")
	sb.WriteString("  node [shape=box, fontname=\"monospace\"];\n")
	for i, st := range scopes {
		if len(byScope[i]) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "  subgraph cluster%d {\n", i)
		fmt.Fprintf(&sb, "    label=%s;\n", dotLabel(scopePath(st, s.state)))
		for _, n := range byScope[i] {
			fmt.Fprintf(&sb, "    n%d [label=%s];\n", n.id,
				dotLabel(n.d.in.b.key.String(), lifetime(n.d.in.b)+", "+n.phase))
		}
		sb.WriteString("  }\n")
	}
	// Edges last and outside every cluster: one that crosses a cluster
	// boundary is drawn wrong if it is declared inside one.
	for _, n := range all {
		_, deps := n.d.inspect()
		for _, d := range deps {
			if to, ok := ids[d.in]; ok {
				fmt.Fprintf(&sb, "  n%d -> n%d;\n", n.id, to)
			}
			// An edge to something outside this walk is dropped rather than
			// given a node of its own: it points into a scope that has been
			// stopped, or one above the scope Graph was called on.
		}
	}
	sb.WriteString("}\n")
	return sb.String()
}

// ---- rendering helpers -----------------------------------------------------

// inspect reads the one instance's phase and edges together, which is the
// only critical section a rendering takes. Nothing is held across the
// recursion, so two scopes' mutexes are never held at once.
func (d dep) inspect() (string, []dep) {
	d.holder.mu.Lock()
	defer d.holder.mu.Unlock()
	return phaseWord(d.in), slices.Clone(d.in.deps)
}

// phaseWord names where an instance is in its lifecycle. Called with the
// owning state's mutex held, like every other read of ph and err.
func phaseWord(in *instance) string {
	switch in.ph {
	case phaseNew:
		return "not built"
	case phaseBuilding:
		return "building"
	case phaseBuilt:
		return "built"
	case phaseStarting:
		return "starting"
	case phaseStarted:
		return "started"
	case phaseStopped:
		return "stopped"
	case phaseFailed:
		if in.err != nil {
			return "failed: " + in.err.Error()
		}
		return "failed"
	}
	return "unknown"
}

// lifetime names how a binding is kept, in the words the API uses.
func lifetime(b *binding) string {
	life := "singleton"
	switch {
	case b.isValue:
		life = "value"
	case b.scoped:
		life = "scoped"
	}
	if b.group {
		life += " group member"
	}
	return life
}

// describe is one line of a tree: what the service is, where it lives, how
// far through its lifecycle it is, and where it was registered.
func describe(b *binding, holder *state, phase string) string {
	attrs := []string{lifetime(b) + " in " + holder.name}
	if b.eager {
		attrs = append(attrs, "eager")
	}
	attrs = append(attrs, phase)
	return fmt.Sprintf("%s: %s (provided at %s)", b.key, strings.Join(attrs, ", "), b.site)
}

// dependentsOf finds the built instances whose constructors resolved target.
// It searches from the container root, because a dependent lives in the
// scope that holds it or below, never above what it depends on.
func dependentsOf(from *state, target *instance) []dep {
	var out []dep
	for _, st := range walkScopes(from) {
		st.mu.Lock()
		for _, in := range st.started {
			if slices.ContainsFunc(in.deps, func(d dep) bool { return d.in == target }) {
				out = append(out, dep{in: in, holder: st})
			}
		}
		st.mu.Unlock()
	}
	return out
}

// walkScopes lists st and every scope under it, parents before children and
// in creation order, so a rendering is stable across runs.
func walkScopes(st *state) []*state {
	st.mu.Lock()
	children := slices.Clone(st.children)
	st.mu.Unlock()
	out := []*state{st}
	for _, c := range children {
		out = append(out, walkScopes(c)...)
	}
	return out
}

// root returns the topmost scope of this container.
func (st *state) root() *state {
	for st.parent != nil {
		st = st.parent
	}
	return st
}

// scopePath names st relative to from, so two scopes with the same name are
// told apart by where they hang.
func scopePath(st, from *state) string {
	var parts []string
	for ; st != nil; st = st.parent {
		parts = append(parts, st.name)
		if st == from {
			break
		}
	}
	slices.Reverse(parts)
	return strings.Join(parts, "/")
}

// dotEscape is what a DOT quoted string needs escaped inside it.
var dotEscape = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// dotLabel quotes the parts as one DOT label, one per line. Scope names come
// from the caller, so they are escaped rather than trusted.
func dotLabel(parts ...string) string {
	esc := make([]string, len(parts))
	for i, p := range parts {
		esc[i] = dotEscape.Replace(p)
	}
	return `"` + strings.Join(esc, `\n`) + `"`
}
