package graph

import (
	"fmt"
	"strings"
	"testing"
)

func TestAnAcyclicGraphPasses(t *testing.T) {
	g := New()
	g.Edge("a", "b")
	g.Edge("b", "c")
	g.Edge("a", "c") // a diamond is not a cycle
	if err := g.Check(); err != nil {
		t.Errorf("Check: %v", err)
	}
}

// The message names every node in the cycle, because the edit that fixes it
// could be at any of them — the node that happened to close the loop is not
// more responsible than the others.
func TestACycleNamesEveryNodeInIt(t *testing.T) {
	g := New()
	g.Edge("a", "b")
	g.Edge("b", "c")
	g.Edge("c", "a")
	err := g.Check()
	if err == nil {
		t.Fatal("Check accepted a cycle")
	}
	for _, want := range []string{"a", "b", "c"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestANodeReferringToItselfIsACycle(t *testing.T) {
	g := New()
	g.Edge("a", "a")
	if err := g.Check(); err == nil {
		t.Fatal("Check accepted a self-edge")
	}
}

// A graph whose branches converge must not be walked once per path through it.
// The path set that DETECTS a cycle does not BOUND the walk: without a second
// set this is 2^40 visits, which is a hang at load with no error to show for
// it. The test would not finish if that regressed.
func TestASharedSubtreeIsWalkedOnce(t *testing.T) {
	g := New()
	for i := 0; i < 40; i++ {
		g.Edge(fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1))
		g.Edge(fmt.Sprintf("n%d", i), fmt.Sprintf("mid%d", i))
		g.Edge(fmt.Sprintf("mid%d", i), fmt.Sprintf("n%d", i+1))
	}
	if err := g.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

// A node's description is what the error calls it, so a caller says "route pi"
// rather than leaking the key it used to keep routes and tasks apart.
func TestDescriptionsAppearInTheError(t *testing.T) {
	g := New()
	g.Describe("r:pi", "route pi")
	g.Describe("t:unlock", "task unlock")
	g.Edge("r:pi", "t:unlock")
	g.Edge("t:unlock", "r:pi")
	err := g.Check()
	if err == nil {
		t.Fatal("Check accepted a cycle")
	}
	if !strings.Contains(err.Error(), "route pi") || !strings.Contains(err.Error(), "task unlock") {
		t.Errorf("error %q does not use the descriptions", err)
	}
}

// Which cycle is reported must not depend on map iteration order.
func TestTheSameGraphReportsTheSameCycle(t *testing.T) {
	build := func() *Graph {
		g := New()
		g.Edge("a", "b")
		g.Edge("b", "a")
		g.Edge("x", "y")
		g.Edge("y", "x")
		return g
	}
	first := build().Check()
	for i := 0; i < 20; i++ {
		if got := build().Check(); got.Error() != first.Error() {
			t.Fatalf("Check reported %q then %q", first, got)
		}
	}
}
