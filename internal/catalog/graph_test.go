package catalog

import (
	"reflect"
	"testing"
)

// 1 → 2 → 4, 1 → 3 → 4, 4 → 5 (arrows point from input to dependent).
func diamond() *graph {
	return newGraph([]edge{
		{series: 2, dependsOn: 1}, {series: 3, dependsOn: 1},
		{series: 4, dependsOn: 2}, {series: 4, dependsOn: 3},
		{series: 5, dependsOn: 4},
	})
}

func TestWouldCycle(t *testing.T) {
	g := diamond()
	cases := []struct {
		series int64
		inputs []int64
		want   bool
	}{
		{1, []int64{5}, true},     // 1 ← 5 closes 1→…→5
		{1, []int64{1}, true},     // self
		{2, []int64{3}, false},    // sibling
		{4, []int64{1, 2}, false}, // redundant edge is fine
		{3, []int64{4}, true},     // 3 → 4 already
		{6, []int64{5}, false},    // new series
	}
	for _, c := range cases {
		if got := g.wouldCycle(c.series, c.inputs); got != c.want {
			t.Errorf("wouldCycle(%d, %v) = %v, want %v", c.series, c.inputs, got, c.want)
		}
	}
}

func TestDownstreamTopological(t *testing.T) {
	g := diamond()
	if got, want := g.downstream([]int64{1}), []int64{2, 3, 4, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("downstream(1) = %v, want %v", got, want)
	}
	if got, want := g.downstream([]int64{3}), []int64{4, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("downstream(3) = %v, want %v", got, want)
	}
	if got := g.downstream([]int64{5}); len(got) != 0 {
		t.Fatalf("downstream(5) = %v, want none", got)
	}
	// Several changed inputs share dependents: each appears once, in order.
	if got, want := g.downstream([]int64{2, 3}), []int64{4, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("downstream(2,3) = %v, want %v", got, want)
	}
}

func TestTopoKeepsCycleMembers(t *testing.T) {
	g := newGraph([]edge{{series: 1, dependsOn: 2}, {series: 2, dependsOn: 1}, {series: 3, dependsOn: 1}})
	got := g.topo(map[int64]bool{1: true, 2: true, 3: true})
	if len(got) != 3 {
		t.Fatalf("topo dropped nodes: %v", got)
	}
}

func TestWalk(t *testing.T) {
	g := diamond()
	up := g.walk(5, DepsUp, 0)
	if len(up) != 5 { // 5→4, 4→2, 4→3, 2→1, 3→1
		t.Fatalf("walk up = %v", up)
	}
	limited := g.walk(5, DepsUp, 1)
	if len(limited) != 1 || limited[0].dependsOn != 4 || limited[0].depth != 1 {
		t.Fatalf("walk up depth 1 = %v", limited)
	}
	down := g.walk(1, DepsDown, 2)
	for _, e := range down {
		if e.depth > 2 {
			t.Fatalf("depth exceeded: %v", e)
		}
	}
	if len(down) != 4 { // 2←1, 3←1, 4←2, 4←3
		t.Fatalf("walk down = %v", down)
	}
}
