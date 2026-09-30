// Package graph answers one question for the rest of chore: do these references
// contain a cycle, and if so which things are in it.
//
// It exists because the question kept being answered separately, and not always
// well. Variables had a detector fused into their resolver, reporting a cycle
// only once a pass made no progress. Routes had a walk of their own, and a
// project's `deps:` had none at all, so a two-task cycle spawned concurrently
// until something gave out. internal/loader now walks every task's `deps:`,
// routes and route predicates as one graph. A dependency name written as a
// template is rendered at run time, so that one edge cannot be known at load and
// is left out.
//
// Nodes are opaque strings. A caller with more than one kind of node prefixes
// them, so a route and a task of the same name stay distinct, and gives each a
// Describe so the error reads in the caller's own words rather than in its
// keys.
package graph

import (
	"fmt"
	"sort"
	"strings"
)

// Graph is a directed graph of named nodes. The zero value is not usable; call
// New.
type Graph struct {
	edges map[string][]string
	names map[string]string
	// order is first-mention order, so Check reports the same cycle every time.
	// Ranging over the edge map instead would make the reported cycle depend on
	// Go's map iteration order, and an error message that moves between runs is
	// one nobody trusts.
	order []string
}

// New returns an empty graph.
func New() *Graph {
	return &Graph{edges: map[string][]string{}, names: map[string]string{}}
}

// Edge records that from refers to to. Either end is created if unknown.
func (g *Graph) Edge(from, to string) {
	g.touch(from)
	g.touch(to)
	g.edges[from] = append(g.edges[from], to)
}

// Describe sets how a node is named in an error. Without one, the node's own
// key is used.
func (g *Graph) Describe(node, text string) {
	g.touch(node)
	g.names[node] = text
}

func (g *Graph) touch(node string) {
	if _, known := g.edges[node]; !known {
		g.edges[node] = nil
		g.order = append(g.order, node)
	}
}

func (g *Graph) describe(node string) string {
	if text, ok := g.names[node]; ok {
		return text
	}
	return node
}

// Check reports the first cycle it finds, naming every node in it.
//
// Depth-first with two sets rather than one, and the distinction matters.
// onPath is what DETECTS the cycle and is popped on the way back out; done is
// what BOUNDS the walk. With only the first, a graph whose branches converge is
// re-walked once per path through it — forty nodes with two converging edges
// each is 2^40 visits, which is a hang at load time with no error to show for
// it.
func (g *Graph) Check() error {
	done := map[string]bool{}
	onPath := map[string]bool{}
	var path []string

	var visit func(node string) []string
	visit = func(node string) []string {
		if onPath[node] {
			// Trim to where this node first appears: what came before leads to
			// the cycle without being part of it, and naming it would send the
			// reader to a line that is not the problem.
			for i, n := range path {
				if n == node {
					return append(append([]string{}, path[i:]...), node)
				}
			}
			return []string{node, node}
		}
		if done[node] {
			return nil
		}
		onPath[node] = true
		path = append(path, node)
		for _, next := range g.edges[node] {
			if cycle := visit(next); cycle != nil {
				return cycle
			}
		}
		path = path[:len(path)-1]
		delete(onPath, node)
		done[node] = true
		return nil
	}

	for _, node := range g.order {
		if cycle := visit(node); cycle != nil {
			return g.cycleError(cycle)
		}
	}
	return nil
}

// cycleError names every node in the cycle and what each one points at, so the
// message reaches the edit that fixes it rather than only the place the loop
// happened to close.
func (g *Graph) cycleError(cycle []string) error {
	parts := make([]string, 0, len(cycle)-1)
	for i := 0; i+1 < len(cycle); i++ {
		parts = append(parts, fmt.Sprintf("%s references %s", g.describe(cycle[i]), g.describe(cycle[i+1])))
	}
	return fmt.Errorf("cycle: %s", strings.Join(parts, "; "))
}

// Sorted returns a map's keys in order, for a caller building a graph from one:
// the order edges are added in decides which cycle is reported first, and a
// message that moves between runs is one nobody trusts.
func Sorted[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
