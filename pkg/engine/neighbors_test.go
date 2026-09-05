// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"slices"
	"testing"
)

// graphCorpus is one index whose documents carry links *and* vectors, committed
// and reopened.
//
// Both fields together is the point. Neighbors reaches the link list by stepping
// over the vector without reading it, so a corpus of textless, vectorless
// documents would exercise the arithmetic with the interesting term set to zero
// — and a wrong byte count would land the reader inside the links and still
// pass.
func graphCorpus(t *testing.T, docs []Document) *Index {
	t.Helper()
	dir := t.TempDir()
	pending := New()
	for _, d := range docs {
		if _, err := pending.Add(d); err != nil {
			t.Fatalf("Add(%q): %v", d.Key, err)
		}
	}
	// Read before the commit, so the answer a mapped segment gives is compared
	// against the answer the pending path gave for the same corpus. The two read
	// paths are different code and the contract is one.
	before := neighborsOf(pending)

	if err := pending.Commit(context.Background(), dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	t.Cleanup(func() { _ = pending.Close() })

	opened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })

	if after := neighborsOf(opened); !neighborEqual(before, after) {
		t.Fatalf("Neighbors pending = %v, reopened = %v", before, after)
	}
	return opened
}

func neighborsOf(ix *Index) map[DocID][]DocID {
	out := make(map[DocID][]DocID, ix.Len())
	for i := range ix.Len() {
		if nb, ok := ix.Neighbors(DocID(i)); ok {
			out[DocID(i)] = nb
		}
	}
	return out
}

func neighborEqual(a, b map[DocID][]DocID) bool {
	if len(a) != len(b) {
		return false
	}
	for id, av := range a {
		if !slices.Equal(av, b[id]) {
			return false
		}
	}
	return true
}

func eightWide(n int) []float32 {
	v := make([]float32, 8)
	for i := range v {
		v[i] = float32(n*8 + i)
	}
	return v
}

// TestNeighborsStepsOverAVectorToReachTheLinks is the check the whole skip rests
// on. A record is key, text, length, vector, links, time — in that order — so a
// reader that miscounts the vector's bytes reads link keys out of the middle of
// the float array and either fails the record's checksum or, worse, decodes
// something plausible.
func TestNeighborsStepsOverAVectorToReachTheLinks(t *testing.T) {
	docs := []Document{
		{Key: "a", Text: "alpha", Vector: eightWide(0), Links: []string{"b", "c"}},
		{Key: "b", Text: "beta", Vector: eightWide(1), Links: []string{"c"}},
		{Key: "c", Text: "gamma", Vector: eightWide(2)},
	}
	ix := graphCorpus(t, docs)

	for id, want := range map[DocID][]DocID{0: {1, 2}, 1: {2}, 2: nil} {
		got, ok := ix.Neighbors(id)
		if !ok {
			t.Fatalf("Neighbors(%d) absent", id)
		}
		if !slices.Equal(got, want) {
			t.Errorf("Neighbors(%d) = %v, want %v", id, got, want)
		}
	}

	// And the vector is still intact on the path that does read it. A skip that
	// advanced by the wrong amount would corrupt one of these two readers, and
	// only comparing both says which.
	for i, d := range docs {
		got, ok := ix.Vector(DocID(i))
		if !ok || !slices.Equal(got, d.Vector) {
			t.Errorf("Vector(%d) = %v, %v; want %v, true", i, got, ok, d.Vector)
		}
	}
}

// TestNeighborsDropsWhatLeadsNowhere pins the two ways an edge stops being an
// edge. Neither is an error: Document.Links may name a document that has not
// been added yet, and a document may be deleted after something linked to it.
func TestNeighborsDropsWhatLeadsNowhere(t *testing.T) {
	ix := graphCorpus(t, []Document{
		{Key: "a", Text: "alpha", Vector: eightWide(0), Links: []string{"b", "never-added", "c"}},
		{Key: "b", Text: "beta", Vector: eightWide(1)},
		{Key: "c", Text: "gamma", Vector: eightWide(2)},
	})

	if got, _ := ix.Neighbors(0); !slices.Equal(got, []DocID{1, 2}) {
		t.Fatalf("Neighbors(0) = %v, want [1 2] — a dangling key is skipped", got)
	}
	if !ix.Delete("b") {
		t.Fatal("Delete(b) = false")
	}
	if got, _ := ix.Neighbors(0); !slices.Equal(got, []DocID{2}) {
		t.Errorf("Neighbors(0) = %v after deleting b, want [2]", got)
	}
	if _, ok := ix.Neighbors(1); ok {
		t.Error("Neighbors(1) is present after b was deleted; a tombstone reads as an unassigned id")
	}
}

// TestNeighborsTellsALeafFromAMissingDocument is the one distinction the bool
// carries, and a traversal needs it: expanding a leaf is finished, expanding a
// document that is not there is a different fact about the graph.
func TestNeighborsTellsALeafFromAMissingDocument(t *testing.T) {
	ix := graphCorpus(t, []Document{{Key: "only", Text: "alone", Vector: eightWide(0)}})

	nb, ok := ix.Neighbors(0)
	if !ok || len(nb) != 0 {
		t.Errorf("Neighbors(0) = %v, %v; want empty, true for a leaf", nb, ok)
	}
	if _, ok := ix.Neighbors(99); ok {
		t.Error("Neighbors(99) is present; an id that was never assigned must read as absent")
	}
}

// TestNeighborsKeepsARepeatedLink holds the method to what it documents. Links
// is the caller's list and this does not editorialise; a traversal that would
// double-count keeps its own visited set, which is where the judgement belongs.
func TestNeighborsKeepsARepeatedLink(t *testing.T) {
	ix := graphCorpus(t, []Document{
		{Key: "a", Text: "alpha", Vector: eightWide(0), Links: []string{"b", "b", "a"}},
		{Key: "b", Text: "beta", Vector: eightWide(1)},
	})
	if got, _ := ix.Neighbors(0); !slices.Equal(got, []DocID{1, 1, 0}) {
		t.Errorf("Neighbors(0) = %v, want [1 1 0] — repeats and self-links are kept", got)
	}
}

// TestNeighborsResultIsTheCallersToKeep guards the one contract difference from
// Doc and Lookup, which hand back index state. A traversal sorts and filters
// what it gets; writing through into a pending document's Links would rewrite
// the corpus.
func TestNeighborsResultIsTheCallersToKeep(t *testing.T) {
	ix := New()
	for _, d := range []Document{
		{Key: "a", Links: []string{"b", "c"}},
		{Key: "b"},
		{Key: "c"},
	} {
		if _, err := ix.Add(d); err != nil {
			t.Fatalf("Add(%q): %v", d.Key, err)
		}
	}
	first, _ := ix.Neighbors(0)
	first[0] = 99
	second, _ := ix.Neighbors(0)
	if !slices.Equal(second, []DocID{1, 2}) {
		t.Errorf("Neighbors(0) = %v after the first result was written to, want [1 2]", second)
	}
}
