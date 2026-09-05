// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"slices"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

func adjacency(t *testing.T, ix *engine.Index) *Adjacency {
	t.Helper()
	a, err := NewAdjacency(context.Background(), ix)
	if err != nil {
		t.Fatalf("NewAdjacency: %v", err)
	}
	return a
}

// TestAdjacencyIsItsOwnTranspose is the invariant the reverse direction exists
// to provide, and it is checked by construction rather than by example: every
// edge the forward structure holds has to appear once in the reverse structure,
// and the reverse structure must hold nothing else.
//
// The failure this catches is the counting sort's, and it is silent. A cursor
// off by one writes a real source id into the wrong row — In(v) then answers
// with documents that do not link to v, which is not an error anywhere, it is a
// walk exploring the wrong neighbourhood.
func TestAdjacencyIsItsOwnTranspose(t *testing.T) {
	ix := index(t,
		node{"a", []string{"b", "c"}},
		node{"b", []string{"c"}},
		node{"c", []string{"a"}},
		node{"d", nil},
		node{"e", []string{"a", "a"}},
	)
	a := adjacency(t, ix)

	forward := map[[2]engine.DocID]int{}
	for u := range a.Len() {
		for _, v := range a.Out(engine.DocID(u)) {
			forward[[2]engine.DocID{engine.DocID(u), v}]++
		}
	}
	reverse := map[[2]engine.DocID]int{}
	for v := range a.Len() {
		for _, u := range a.In(engine.DocID(v)) {
			reverse[[2]engine.DocID{u, engine.DocID(v)}]++
		}
	}
	if len(forward) != len(reverse) {
		t.Fatalf("forward holds %d distinct edges, reverse %d", len(forward), len(reverse))
	}
	for e, n := range forward {
		if reverse[e] != n {
			t.Errorf("edge %d->%d appears %d times forward and %d times reverse", e[0], e[1], n, reverse[e])
		}
	}

	// Rows come out ascending by source, which is what makes a walk reproducible
	// across builds — see buildReverse.
	for v := range a.Len() {
		row := a.In(engine.DocID(v))
		if !slices.IsSorted(row) {
			t.Errorf("In(%d) = %v is not ascending", v, row)
		}
	}
}

// TestAdjacencyAgreesWithTheIndex holds the snapshot to its source. The whole
// value of resolving edges once is that the answer is the one the index would
// have given per hop; if it is not, every traversal in this package is walking a
// graph the corpus does not have.
func TestAdjacencyAgreesWithTheIndex(t *testing.T) {
	ix := index(t,
		node{"a", []string{"b", "nowhere", "c"}},
		node{"b", []string{"a", "a"}},
		node{"c", nil},
	)
	a := adjacency(t, ix)

	total := 0
	for u := range ix.Len() {
		want, ok := ix.Neighbors(engine.DocID(u))
		if !ok {
			t.Fatalf("Neighbors(%d) absent", u)
		}
		got := a.Out(engine.DocID(u))
		if len(got) != len(want) {
			t.Fatalf("Out(%d) = %v, want %v", u, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("Out(%d)[%d] = %d, want %d", u, i, got[i], want[i])
			}
		}
		total += len(want)
	}
	if a.Edges() != total {
		t.Errorf("Edges = %d, want %d resolved links", a.Edges(), total)
	}
	if a.Len() != ix.Len() {
		t.Errorf("Len = %d, want the index's %d", a.Len(), ix.Len())
	}
}

// TestDegreeCountsBothDirections pins the arithmetic PPR's work bound rests on.
// The push loop divides a node's residual by Degree and then writes to every id
// in Out and In; a degree that disagreed with that edge list would spread more
// or less than the mass it took, and the termination argument would be about a
// different loop than the one running.
func TestDegreeCountsBothDirections(t *testing.T) {
	ix := index(t,
		node{"hub", []string{"a", "b", "c"}},
		node{"a", []string{"hub"}},
		node{"b", []string{"hub"}},
		node{"c", nil},
	)
	a := adjacency(t, ix)

	for u := range a.Len() {
		id := engine.DocID(u)
		want := len(a.Out(id)) + len(a.In(id))
		if got := a.Degree(id); got != want {
			t.Errorf("Degree(%d) = %d, want %d", u, got, want)
		}
	}
	// hub links to three and is linked from two.
	if got := a.Degree(0); got != 5 {
		t.Errorf("Degree(hub) = %d, want 5", got)
	}
	// c is linked from hub and links to nothing, so it is reachable only in
	// reverse — the direction Document.Links does not store and the BFS scorer
	// cannot travel.
	if got := a.In(3); len(got) != 1 || got[0] != 0 {
		t.Errorf("In(c) = %v, want [0]", got)
	}
}

// TestAdjacencyIsASnapshot is the contract, tested rather than merely written
// down. A caller who ingests and forgets to rebuild gets a stale graph, and a
// stale graph answers plausibly — which is why the type documents it at the top
// rather than in a footnote.
func TestAdjacencyIsASnapshot(t *testing.T) {
	ix := index(t, node{"a", []string{"b"}}, node{"b", nil})
	a := adjacency(t, ix)

	if _, err := ix.Add(engine.Document{Key: "c", Links: []string{"a"}}); err != nil {
		t.Fatalf("Add(c): %v", err)
	}
	if a.Len() != 2 {
		t.Errorf("Len = %d after a third document was added, want the 2 it was built with", a.Len())
	}
	if got := a.Out(2); got != nil {
		t.Errorf("Out(2) = %v, want nil for an id past the snapshot", got)
	}
	if got := a.Degree(2); got != 0 {
		t.Errorf("Degree(2) = %d, want 0 for an id past the snapshot", got)
	}
	if got := a.In(0); len(got) != 0 {
		t.Errorf("In(a) = %v, want empty — c's edge was added after the build", got)
	}

	if rebuilt := adjacency(t, ix); rebuilt.Len() != 3 || len(rebuilt.In(0)) != 1 {
		t.Errorf("rebuilt Len = %d, In(a) = %v; want 3 and one edge", rebuilt.Len(), rebuilt.In(0))
	}
}

// TestAdjacencyDropsDeletedDocuments checks that a tombstone leaves a hole
// rather than shifting the id space. Renumbering here would make every DocID a
// scorer hands to fusion name a different document.
func TestAdjacencyDropsDeletedDocuments(t *testing.T) {
	ix := index(t,
		node{"a", []string{"b", "c"}},
		node{"b", []string{"c"}},
		node{"c", nil},
	)
	if !ix.Delete("b") {
		t.Fatal("Delete(b) = false")
	}
	a := adjacency(t, ix)

	if a.Len() != 3 {
		t.Errorf("Len = %d, want 3 — a tombstone keeps its slot", a.Len())
	}
	if got := a.Out(0); len(got) != 1 || got[0] != 2 {
		t.Errorf("Out(a) = %v, want [2] — the edge into b leads nowhere now", got)
	}
	if got := a.Degree(1); got != 0 {
		t.Errorf("Degree(b) = %d, want 0 for a deleted document", got)
	}
	if got := a.In(2); len(got) != 1 || got[0] != 0 {
		t.Errorf("In(c) = %v, want [0] — b's edge went with b", got)
	}
}

// TestAdjacencyBuildIsCancellable guards the one long operation this type has.
// It walks the whole corpus, so a caller who gave up has to be able to stop it.
func TestAdjacencyBuildIsCancellable(t *testing.T) {
	nodes := make([]node, 4096)
	for i := range nodes {
		nodes[i] = node{key: string(rune('a'+i%26)) + string(rune('a'+i/26))}
	}
	ix := index(t, nodes...)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewAdjacency(ctx, ix); err == nil {
		t.Error("NewAdjacency on a cancelled context returned no error")
	}
}
