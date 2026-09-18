package di

// Rendering the graph. Nothing here takes part in resolution or teardown: it
// reads the edges resolve.go records while constructors run, under the
// instance's owning mutex and never two of those at once, and the dependency
// lists Wire declares, which never change after registration.

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Explain renders what T resolves to and what it was built from: the
// dependency tree, each node with its lifetime, scope, lifecycle state and
// registration site, followed by what needed it.
//
// A built service has a recorded tree. One that has not been built is
// reported as such; if it was registered with Wire, its declared dependencies
// are drawn under it with dashed edges, each continuing as a recorded tree
// where built and a declared one where not, and "declared by" lists the
// unbuilt services that declare it. A closure that has not run ends its
// branch, and so does a key nothing provides. A key served by a group is
// explained member by member, and a dependency reached twice is expanded once
// and named on later visits.
//
// A registration that came too late for somebody lists what it missed: a key
// a constructor was told nothing provides, or a group member registered after
// something read the group. Both answers were about the scope chain as it
// stood, so neither is rejected, and the values built on them do not have it.
//
// Explain builds nothing. It commits pending registrations as a resolution
// from this scope would, so a configuration this scope would reject is
// reported here by the same panic.
func (s *Scope) Explain[T any]() string {
	k := s.key(reflect.TypeFor[T]())
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

// found is a binding and the scope that registered it.
type found struct {
	b     *binding
	owner *state
}

// groupMembers lists the group registered for k across the scope chain, in
// the order All resolves them.
func (s *Scope) groupMembers(k key) []found {
	var out []found
	for st := s.st; st != nil; st = st.parent {
		st.freeze()
		for _, b := range st.reg.Load().groups[k] {
			out = append(out, found{b: b, owner: st})
		}
	}
	return out
}

// explainOne renders one binding's tree, and the instances that needed it.
func (s *Scope) explainOne(sb *strings.Builder, b *binding, owner *state, seen map[*instance]bool) {
	holder := owner
	if b.scoped {
		holder = s.st
	}
	holder.mu.Lock()
	in := holder.instanceAt(b)
	holder.mu.Unlock()
	// A Scoped binding this scope has never resolved has no instance; a
	// singleton always has one, built or not.
	phase, deps, fresh := "not built", []dep(nil), true
	if in != nil {
		phase, deps, fresh = dep{in: in, holder: holder}.inspect()
	}
	sb.WriteString(describe(b, holder, phase) + "\n")

	var by []dep
	if fresh {
		s.declaredInto(sb, b, holder, "", seen, map[*binding]bool{b: true})
	} else {
		seen[in] = true
		explainInto(sb, deps, "", seen)
		by = dependentsOf(s.st.root(), in)
	}
	if len(by) > 0 {
		sb.WriteString("needed by: " + strings.Join(namesOf(by), ", ") + "\n")
	}
	if declared := s.declaredBy(b, by); len(declared) > 0 {
		sb.WriteString("declared by: " + strings.Join(declared, ", ") + "\n")
	}
	if missed := missersOf(s.st.root(), found{b, owner}); len(missed) > 0 {
		sb.WriteString("missed by: " + strings.Join(namesOf(missed), ", ") + "\n")
	}
}

// namesOf renders instances as "key in scope", the way a dependency list
// names them.
func namesOf(deps []dep) []string {
	names := make([]string, len(deps))
	for i, d := range deps {
		names[i] = d.in.b.key.String() + " in " + d.holder.name
	}
	return names
}

// declaredInto draws the dependencies b declares under an unbuilt node, with
// dashed edges, looking each up from holder as the build would. A built one
// continues as its recorded tree; an unbuilt one as its own declaration, or
// ends the branch if it is a closure. drawn keeps a declared binding from
// being expanded twice, which a cycle needs.
func (s *Scope) declaredInto(sb *strings.Builder, b *binding, holder *state, prefix string, seen map[*instance]bool, drawn map[*binding]bool) {
	edges := declared(b, holder)
	for i, e := range edges {
		branch, pad := "├╌╌ ", "│   "
		if i == len(edges)-1 {
			branch, pad = "└╌╌ ", "    "
		}
		sb.WriteString(prefix + branch)
		target, owner := e.b, e.owner
		if target == nil {
			if e.optional {
				sb.WriteString(e.k.String() + ": not provided, optional\n")
				continue
			}
			sb.WriteString(e.k.String() + ": not provided\n")
			continue
		}
		th := owner
		if target.scoped {
			th = holder
		}
		th.mu.Lock()
		in := th.instanceAt(target)
		th.mu.Unlock()
		if in != nil {
			if seen[in] {
				sb.WriteString(target.key.String() + ": see above\n")
				continue
			}
			if phase, next, fresh := (dep{in: in, holder: th}).inspect(); !fresh {
				seen[in] = true
				sb.WriteString(describe(target, th, phase) + "\n")
				explainInto(sb, next, prefix+pad, seen)
				continue
			}
		}
		if drawn[target] {
			sb.WriteString(target.key.String() + ": see above\n")
			continue
		}
		drawn[target] = true
		sb.WriteString(describe(target, th, "not built") + "\n")
		s.declaredInto(sb, target, th, prefix+pad, seen, drawn)
	}
}

// declaredBy lists the Wire bindings, in any scope of the container, that
// declare b's key and would resolve it to b from their own scope, leaving out
// the instances already named as needing it. It reads only committed
// registrations and commits nothing: a root Explain must not be the call that
// rejects a descendant's pending batch.
func (s *Scope) declaredBy(b *binding, except []dep) []string {
	var out []string
	for _, st := range walkScopes(s.st.root()) {
		for _, d := range st.live() {
			if d == b || slices.ContainsFunc(except, func(e dep) bool { return e.in.b == d }) {
				continue
			}
			if d.inner != b && !declares(d, b, st) {
				continue
			}
			out = append(out, d.key.String()+" in "+st.name)
		}
	}
	return out
}

// declares reports whether d's parameters would resolve to b from st. A key
// and a group of one type are different bindings, so the kind of the parameter
// decides which of the two a declaration reaches: a plain one what the index
// serves, an AllOf one every member of the group.
func declares(d, b *binding, st *state) bool {
	return slices.ContainsFunc(d.wants, func(w want) bool {
		switch {
		case w.k != b.key:
			return false
		case w.kind == wantGroup:
			return b.group && inGroupFrom(st, b)
		}
		return !b.group && peek(st, b.key) == b
	})
}

// inGroupFrom reports whether b is in the group for its key as a build in st
// would read it, among the registrations already committed.
func inGroupFrom(st *state, b *binding) bool {
	for ; st != nil; st = st.parent {
		if slices.Contains(st.reg.Load().groups[b.key], b) {
			return true
		}
	}
	return false
}

// peek is lookup without the freeze: the binding k resolves to from st among
// the registrations already committed.
func peek(st *state, k key) *binding {
	for ; st != nil; st = st.parent {
		if b, ok := st.reg.Load().index[k]; ok {
			return b
		}
	}
	return nil
}

// explainInto writes one level of the tree and recurses.
func explainInto(sb *strings.Builder, deps []dep, prefix string, seen map[*instance]bool) {
	for i, d := range deps {
		branch, pad := "├── ", "│   "
		if i == len(deps)-1 {
			branch, pad = "└── ", "    "
		}
		sb.WriteString(prefix + branch)
		if seen[d.in] {
			// The same instance by another route: named without its subtree,
			// which says it is one value rather than two of a type.
			sb.WriteString(d.in.b.key.String() + ": see above\n")
			continue
		}
		seen[d.in] = true
		phase, next, _ := d.inspect()
		sb.WriteString(describe(d.in.b, d.holder, phase) + "\n")
		explainInto(sb, next, prefix+pad, seen)
	}
}

// Graph renders everything built in this scope and its descendants as
// Graphviz DOT: one box per instance, one cluster per scope that holds any,
// and an arrow from each instance to what its constructor resolved.
//
// It changes nothing, not even the pending registrations, so it is safe to
// call from a handler or a hook. Nodes are numbered in creation and build
// order, so the same run renders the same document. A stopped scope no longer
// holds its instances and contributes nothing. Use Explain for one service in
// full.
func (s *Scope) Graph() string {
	scopes := walkScopes(s.st)
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
			phase, _, _ := d.inspect()
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
		fmt.Fprintf(&sb, "    label=%s;\n", dotLabel(scopePath(st, s.st)))
		for _, n := range byScope[i] {
			fmt.Fprintf(&sb, "    n%d [label=%s];\n", n.id,
				dotLabel(n.d.in.b.key.String(), lifetime(n.d.in.b)+", "+n.phase))
		}
		sb.WriteString("  }\n")
	}
	// Edges last and outside every cluster: one declared inside a cluster is
	// drawn wrong when it crosses the boundary.
	for _, n := range all {
		_, deps, _ := n.d.inspect()
		for _, d := range deps {
			if to, ok := ids[d.in]; ok {
				fmt.Fprintf(&sb, "  n%d -> n%d;\n", n.id, to)
			}
			// An edge into a stopped scope, or one above the scope Graph was
			// called on, is dropped rather than given a node.
		}
	}
	sb.WriteString("}\n")
	return sb.String()
}

// ---- rendering helpers -----------------------------------------------------

// inspect reads one instance's phase and edges together, the only critical
// section a rendering takes, and reports whether the instance is unbuilt, in
// which case what the binding declares stands in for the edges. Nothing is
// held across the recursion, so two scopes' mutexes are never held at once.
func (d dep) inspect() (phase string, deps []dep, fresh bool) {
	d.holder.mu.Lock()
	defer d.holder.mu.Unlock()
	return phaseWord(d.in), slices.Clone(d.in.deps), d.in.ph == phaseNew
}

// phaseWord names where an instance is in its lifecycle. Called with the
// owning state's mutex held.
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
	if b.inner != nil {
		life += " wrapper"
	}
	return life
}

// describe is one line of a tree: the service, where it lives, its phase and
// its registration site.
func describe(b *binding, holder *state, phase string) string {
	attrs := []string{lifetime(b) + " in " + holder.name}
	if b.eager {
		attrs = append(attrs, "eager")
	}
	attrs = append(attrs, phase)
	return fmt.Sprintf("%s: %s (provided at %s)", b.key, strings.Join(attrs, ", "), b.site)
}

// dependentsOf finds the built instances whose constructors resolved target.
// It searches from the container root, since a dependent lives in the scope
// that holds it or below, never above what it depends on.
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

// missersOf finds the built instances that asked about target's key and were
// answered before it was registered, so their values do not have it: a
// constructor told nothing provides the key, or one that read the group
// without this member. Both answers are about the chain as it stood, which is
// why they are reported and not rejected.
//
// It searches from the container root, as dependentsOf does, and counts only
// an asker whose chain reached the scope target was registered in: one that
// asked from a sibling branch was never going to see it.
func missersOf(from *state, target found) []dep {
	missed := func(in *instance) bool {
		if target.b.group {
			// Reported only when no read that reached the scope had the member:
			// a constructor may read a group twice, and the later read decides.
			reached, had := false, false
			for _, r := range in.reads {
				if r.k == target.b.key && r.from.descendsFrom(target.owner) {
					reached = true
					had = had || slices.Contains(r.seen, target.b)
				}
			}
			return reached && !had
		}
		// A miss means nothing on the chain served the key when it was asked,
		// so the registration came later. Unless the constructor went on to
		// resolve the key anyway — registering the default itself, say — in
		// which case its value has one and it missed nothing.
		return slices.ContainsFunc(in.declines, func(d decline) bool {
			return d.k == target.b.key && d.from.descendsFrom(target.owner)
		}) && !slices.ContainsFunc(in.deps, func(d dep) bool {
			return d.in.b.key == target.b.key
		})
	}
	var out []dep
	for _, st := range walkScopes(from) {
		st.mu.Lock()
		for _, in := range st.started {
			if missed(in) {
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
// from the caller, so they are escaped.
func dotLabel(parts ...string) string {
	esc := make([]string, len(parts))
	for i, p := range parts {
		esc[i] = dotEscape.Replace(p)
	}
	return `"` + strings.Join(esc, `\n`) + `"`
}

// Modules renders the modules registered into this scope and its ancestors:
// what each provides, what it needs and which module serves it, what it
// wraps, and which of its constructors are closures whose needs are unknown
// until they run. A need only a resolving scope can provide is reported as
// owed, as Validate reports it. A dependency a module serves for itself is
// left out. Registrations made outside any module are grouped as "registered
// directly".
//
// Like Explain, it builds nothing and commits pending registrations as a
// resolution would, so a configuration this scope would reject is reported
// by the same panic.
func (s *Scope) Modules() string {
	var chain []*state
	for st := s.st; st != nil; st = st.parent {
		st.freeze()
		chain = append(chain, st)
	}
	type module struct {
		name                              string
		provides, needs, wraps, unchecked []string
		seen                              map[string]bool
	}
	var order []*module
	byName := map[string]*module{}
	get := func(name string) *module {
		if m := byName[name]; m != nil {
			return m
		}
		m := &module{name: name, seen: map[string]bool{}}
		byName[name] = m
		order = append(order, m)
		return m
	}
	// Deduped per section as well as per line: a key is listed under
	// provides and again under unchecked.
	add := func(m *module, list *[]string, line string) {
		id := fmt.Sprintf("%p:%s", list, line)
		if !m.seen[id] {
			m.seen[id] = true
			*list = append(*list, line)
		}
	}
	// Ancestors first, so the report reads top-down like the scope tree.
	for _, st := range slices.Backward(chain) {
		for _, b := range st.live() {
			m := get(moduleLabel(b))
			if b.inner != nil {
				add(m, &m.wraps, b.key.name(shortName)+" ← "+moduleLabel(b.inner))
			} else {
				add(m, &m.provides, b.key.name(shortName))
			}
			switch {
			case b.isValue:
				continue
			case b.wants == nil:
				add(m, &m.unchecked, b.key.name(shortName))
				continue
			}
			holder := st
			if b.scoped {
				holder = s.st
			}
			for _, w := range b.wants {
				if w.kind == wantGroup {
					// A group is a set, not one registration, and every member
					// names its own module under "provides".
					add(m, &m.needs, "all of "+w.k.name(shortName))
					continue
				}
				dep, _ := (&Scope{st: holder}).lookup(w.k)
				var from string
				switch {
				case dep == nil && w.kind == wantOptional:
					from = "not provided, optional"
				case dep == nil && b.scoped:
					from = "owed to a resolving scope"
				case dep == nil:
					from = "not provided"
				case moduleLabel(dep) == m.name:
					continue // the module's own business
				default:
					from = moduleLabel(dep)
				}
				add(m, &m.needs, w.k.name(shortName)+" ← "+from)
			}
		}
	}

	var sb strings.Builder
	for _, m := range order {
		sb.WriteString(m.name + "\n")
		section := func(label string, lines []string) {
			for i, l := range lines {
				if i == 0 {
					fmt.Fprintf(&sb, "  %-10s %s\n", label, l)
				} else {
					fmt.Fprintf(&sb, "  %-10s %s\n", "", l)
				}
			}
		}
		if len(m.provides) > 0 {
			section("provides", []string{strings.Join(m.provides, ", ")})
		}
		section("wraps", m.wraps)
		section("needs", m.needs)
		if len(m.unchecked) > 0 {
			section("unchecked", []string{strings.Join(m.unchecked, ", ") + " (closures: needs known when they run)"})
		}
	}
	return sb.String()
}

// moduleLabel names the module a binding was registered from, or says that
// there was none.
func moduleLabel(b *binding) string {
	if b.module == "" {
		return "registered directly"
	}
	return b.module
}

// shortName is a key with its package named as code names it, storage.Store
// rather than the import path, to match the module labels beside it. Explain
// keeps the full path, since an error must not confuse two packages of one
// name.
func shortName(t reflect.Type) string {
	if t.Kind() == reflect.Pointer {
		return "*" + shortName(t.Elem())
	}
	if t.PkgPath() != "" {
		return t.PkgPath()[strings.LastIndex(t.PkgPath(), "/")+1:] + "." + t.Name()
	}
	return t.String()
}
