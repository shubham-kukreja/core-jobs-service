package catalog

import (
	"sort"
)

// graph is the series dependency graph: series id → ids it depends on.
// The catalog holds ~hundreds of series, so it is loaded whole and walked in
// memory; that keeps cycle detection and ordering pure and testable.
type graph struct {
	up   map[int64][]int64 // series → inputs
	down map[int64][]int64 // input → dependents
}

type edge struct{ series, dependsOn int64 }

func newGraph(edges []edge) *graph {
	g := &graph{up: map[int64][]int64{}, down: map[int64][]int64{}}
	for _, e := range edges {
		g.up[e.series] = append(g.up[e.series], e.dependsOn)
		g.down[e.dependsOn] = append(g.down[e.dependsOn], e.series)
	}
	for _, m := range []map[int64][]int64{g.up, g.down} {
		for k := range m {
			sort.Slice(m[k], func(i, j int) bool { return m[k][i] < m[k][j] })
		}
	}
	return g
}

// wouldCycle reports whether making series depend on inputs creates a cycle,
// i.e. whether series is reachable upstream from any input (or is an input).
func (g *graph) wouldCycle(series int64, inputs []int64) bool {
	seen := map[int64]bool{}
	stack := append([]int64(nil), inputs...)
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == series {
			return true
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		// series' own current edges are never expanded (reaching series
		// returns above), so edges being replaced cannot cause false positives.
		stack = append(stack, g.up[n]...)
	}
	return false
}

// downstream returns every series that transitively depends on any of ids
// (excluding ids themselves unless they depend on each other), in
// topological order: each series appears after all of its inputs that are
// also in the result.
func (g *graph) downstream(ids []int64) []int64 {
	set := map[int64]bool{}
	stack := append([]int64(nil), ids...)
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, d := range g.down[n] {
			if !set[d] {
				set[d] = true
				stack = append(stack, d)
			}
		}
	}
	return g.topo(set)
}

// topo orders the nodes in set so inputs precede dependents (Kahn's
// algorithm, ties broken by id for determinism). The graph is acyclic by
// construction (cycles are rejected on save); any nodes left in a cycle are
// appended in id order so callers still see every node.
func (g *graph) topo(set map[int64]bool) []int64 {
	indeg := map[int64]int{}
	for n := range set {
		indeg[n] = 0
	}
	for n := range set {
		for _, in := range g.up[n] {
			if set[in] {
				indeg[n]++
			}
		}
	}
	var ready []int64
	for n, d := range indeg {
		if d == 0 {
			ready = append(ready, n)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })

	out := make([]int64, 0, len(set))
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		out = append(out, n)
		var next []int64
		for _, d := range g.down[n] {
			if !set[d] {
				continue
			}
			indeg[d]--
			if indeg[d] == 0 {
				next = append(next, d)
			}
		}
		ready = append(ready, next...)
		sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })
	}
	if len(out) < len(set) {
		var rest []int64
		for n, d := range indeg {
			if d > 0 {
				rest = append(rest, n)
			}
		}
		sort.Slice(rest, func(i, j int) bool { return rest[i] < rest[j] })
		out = append(out, rest...)
	}
	return out
}

// walkedEdge is an edge found by walk, with its distance from the start.
type walkedEdge struct {
	edge
	depth int
}

// walk returns the edges reachable from start in direction dir, up to
// maxDepth hops (maxDepth <= 0 means unlimited), breadth first.
func (g *graph) walk(start int64, dir DepDirection, maxDepth int) []walkedEdge {
	adj := g.up
	if dir == DepsDown {
		adj = g.down
	}
	type item struct {
		id    int64
		depth int
	}
	seen := map[int64]bool{start: true}
	queue := []item{{start, 0}}
	var out []walkedEdge
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if maxDepth > 0 && it.depth >= maxDepth {
			continue
		}
		for _, n := range adj[it.id] {
			e := edge{series: it.id, dependsOn: n}
			if dir == DepsDown {
				e = edge{series: n, dependsOn: it.id}
			}
			out = append(out, walkedEdge{edge: e, depth: it.depth + 1})
			if !seen[n] {
				seen[n] = true
				queue = append(queue, item{n, it.depth + 1})
			}
		}
	}
	return out
}
