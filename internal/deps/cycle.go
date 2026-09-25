package deps

import (
	"fmt"
	"sort"
)

// Graph is a package-level dependency graph: package -> packages it locks
// against. Version edges all point at already-registered packages, so the
// only way a cycle can appear is for the new registration to close one.
type Graph map[string][]string

// AddCycle checks whether adding an edge from pkg to each of deps would
// close a package cycle, returning the first cycle found (starting at
// pkg). Existing edges in g are assumed acyclic.
func (g Graph) AddCycle(pkg string, deps []Lock) []string {
	targets := make(map[string]bool, len(deps))
	for _, d := range deps {
		if d.Package == pkg {
			return []string{pkg, pkg}
		}
		targets[d.Package] = true
	}
	for target := range targets {
		// The edge pkg -> target closes a cycle exactly when target can
		// already reach pkg through existing edges. The returned path is
		// target -> ... -> pkg; prepend pkg so the rendered cycle reads
		// pkg -> target -> ... -> pkg.
		if path := g.path(target, pkg, map[string]bool{}); path != nil {
			return append([]string{pkg}, path...)
		}
	}
	return nil
}

// path returns a path from from to to (both included) using existing
// graph edges, or nil. Edges are visited in sorted order for stable
// messages.
func (g Graph) path(from, to string, visiting map[string]bool) []string {
	if from == to {
		return []string{to}
	}
	if visiting[from] {
		return nil
	}
	visiting[from] = true
	defer delete(visiting, from)
	for _, next := range g.sortedEdges(from) {
		if p := g.path(next, to, visiting); p != nil {
			return append([]string{from}, p...)
		}
	}
	return nil
}

func (g Graph) sortedEdges(pkg string) []string {
	edges := append([]string(nil), g[pkg]...)
	sort.Strings(edges)
	return edges
}

// FormatCycle renders a cycle like "acme.a -> acme.b -> acme.a".
func FormatCycle(cycle []string) string {
	out := ""
	for i, p := range cycle {
		if i > 0 {
			out += " -> "
		}
		out += p
	}
	if out == "" {
		return "(empty cycle)"
	}
	return out
}

// CycleError describes a rejected registration that would close a cycle.
type CycleError struct {
	Cycle []string
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("dependency cycle rejected: %s", FormatCycle(e.Cycle))
}
