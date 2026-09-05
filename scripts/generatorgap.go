//go:build ignore

// Command generatorgap reports what only the hand-written tests reach.
//
// The suite has two halves. The generators -- the property test, the
// sequential machine, the concurrent driver and the fuzzers -- explore
// sequences nobody wrote down, and are the only part that can find a defect
// no one thought of. The hand-written tests pin what a review already found.
// A line the second half reaches and the first half does not is a line whose
// only protection is that someone once thought of it, and every defect the
// three September 2026 reviews found lived on such a line.
//
// So the gap is worth measuring, and worth failing on when it widens: this
// exits non-zero if the generators' statement coverage falls below -floor.
//
// Usage, from the repository root:
//
//	go test -run 'TestMachine|TestConcurrent|TestProperty|FuzzMachine' -coverprofile=gen.cov .
//	go test -coverprofile=all.cov .
//	go run scripts/generatorgap.go -floor 80 gen.cov all.cov
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
)

// block is one coverage block, identified the way the profile identifies it:
// by both ends, columns included. Keying it on line numbers alone merges the
// blocks that share a line -- di.go has eighteen such lines, where a hook is
// registered and its body declared in the same expression -- and then a
// generator that reached only the registration makes the body look covered
// too, which is exactly the gap this command exists to find.
type block struct {
	file                         string
	start, startCol, end, endCol int
}

func main() {
	floor := flag.Float64("floor", 0, "fail if the generators cover less than this percentage of statements")
	top := flag.Int("top", 12, "how many functions to list")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: generatorgap [-floor pct] [-top n] gen.cov all.cov")
		os.Exit(2)
	}
	gen, _, genPct := parse(flag.Arg(0))
	all, stmts, _ := parse(flag.Arg(1))

	only := map[string]int{}   // function -> statements only the hand-written tests reach
	nobody := map[string]int{} // function -> statements nothing reaches
	fns := newFuncIndex()
	for b, n := range all {
		switch {
		case n > 0 && gen[b] == 0:
			only[fns.at(b)] += stmts[b]
		case n == 0:
			nobody[fns.at(b)] += stmts[b]
		}
	}
	fmt.Printf("generators cover %.1f%% of statements\n\n", genPct)
	report("reached only by hand-written tests", only, *top)
	report("reached by nothing", nobody, *top)

	if *floor > 0 && genPct < *floor {
		fmt.Fprintf(os.Stderr, "\ngenerator coverage %.1f%% is below the floor of %.1f%%\n", genPct, *floor)
		fmt.Fprintln(os.Stderr, "either the generators lost a shape they used to build, or new code arrived that only a hand-written test reaches")
		os.Exit(1)
	}
}

func report(title string, byFunc map[string]int, top int) {
	type row struct {
		fn string
		n  int
	}
	rows := make([]row, 0, len(byFunc))
	total := 0
	for fn, n := range byFunc {
		rows = append(rows, row{fn, n})
		total += n
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].fn < rows[j].fn
	})
	fmt.Printf("%s: %d statements\n", title, total)
	for i, r := range rows {
		if i == top {
			fmt.Printf("  ... and %d more\n", len(rows)-top)
			break
		}
		fmt.Printf("  %4d  %s\n", r.n, r.fn)
	}
	fmt.Println()
}

// parse reads a coverage profile, returning the count per block and the
// percentage of statements covered.
func parse(path string) (counts, stmts map[block]int, pct float64) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer f.Close()
	counts, stmts = map[block]int{}, map[block]int{}
	var covered, total int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") || line == "" {
			continue
		}
		// name.go:line.col,line.col numberOfStatements count
		colon := strings.LastIndex(line, ":")
		fields := strings.Fields(line[colon+1:])
		if len(fields) != 3 {
			continue
		}
		from, to, ok := strings.Cut(fields[0], ",")
		if !ok {
			continue
		}
		fromLine, fromCol, _ := strings.Cut(from, ".")
		toLine, toCol, _ := strings.Cut(to, ".")
		b := block{
			file:     line[:colon],
			start:    atoi(fromLine),
			startCol: atoi(fromCol),
			end:      atoi(toLine),
			endCol:   atoi(toCol),
		}
		n, count := atoi(fields[1]), atoi(fields[2])
		if _, seen := stmts[b]; !seen {
			stmts[b] = n
			total += n
		}
		was := counts[b]
		counts[b] += count
		if was == 0 && counts[b] > 0 {
			covered += n
		}
	}
	if total == 0 {
		return counts, stmts, 0
	}
	return counts, stmts, 100 * float64(covered) / float64(total)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// funcIndex maps a line of di.go to the function containing it, so the gap
// reads as a list of functions rather than of line numbers.
type funcIndex struct {
	starts []struct {
		line int
		name string
	}
}

func (f funcIndex) at(b block) string {
	name := "?"
	for _, s := range f.starts {
		if s.line <= b.start {
			name = s.name
		}
	}
	return name
}

func newFuncIndex() funcIndex {
	var idx funcIndex
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "di.go", nil, 0)
	if err != nil {
		return idx
	}
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := fn.Name.Name
		if fn.Recv != nil {
			name = recvName(fn.Recv) + "." + name
		}
		idx.starts = append(idx.starts, struct {
			line int
			name string
		}{fset.Position(fn.Pos()).Line, name})
	}
	sort.Slice(idx.starts, func(i, j int) bool { return idx.starts[i].line < idx.starts[j].line })
	return idx
}

func recvName(fl *ast.FieldList) string {
	var buf strings.Builder
	ast.Inspect(fl.List[0].Type, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			buf.WriteString(id.Name)
			return false
		}
		return true
	})
	return buf.String()
}
