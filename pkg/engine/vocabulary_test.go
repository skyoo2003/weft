// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

// vocabCorpus is one index with documents on both sides of a commit: a
// committed, reopened half and a pending half added on top of it.
//
// Both halves at once is the point for everything in this file. Terms merges a
// segment's decoded dictionary with the pending map, and PostingBound adds a
// varint read per segment to a map lookup — so a corpus living entirely on one
// side exercises one of the two paths and reports success.
func vocabCorpus(t *testing.T, committed, pending []Document) *Index {
	t.Helper()
	dir := t.TempDir()
	ix := New()
	for _, d := range committed {
		if _, err := ix.Add(d); err != nil {
			t.Fatalf("Add(%q): %v", d.Key, err)
		}
	}
	if err := ix.Commit(context.Background(), dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	t.Cleanup(func() { _ = ix.Close() })

	opened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })

	for _, d := range pending {
		if _, err := opened.Add(d); err != nil {
			t.Fatalf("Add(%q): %v", d.Key, err)
		}
	}
	return opened
}

func TestTermsMergesBothHalvesOfTheIndex(t *testing.T) {
	ix := vocabCorpus(t,
		[]Document{{Key: "c1", Text: "alpha beta shared"}},
		[]Document{{Key: "p1", Text: "alphabet gamma shared"}},
	)

	for _, tc := range []struct {
		prefix string
		want   []string
	}{
		{"", []string{"alpha", "alphabet", "beta", "gamma", "shared"}},
		{"alpha", []string{"alpha", "alphabet"}},
		{"beta", []string{"beta"}},   // committed only
		{"gamma", []string{"gamma"}}, // pending only
		{"shared", []string{"shared"}},
		{"zzz", nil},
	} {
		t.Run("prefix="+tc.prefix, func(t *testing.T) {
			if got := ix.Terms(tc.prefix, 100); !slices.Equal(got, tc.want) {
				t.Errorf("Terms(%q) = %v, want %v", tc.prefix, got, tc.want)
			}
		})
	}
}

// TestTermsSortsBeforeItTruncates is the property that keeps a limited result
// reproducible. Truncating as the scan goes would make which terms survive
// depend on map iteration order, so the same call against an unchanged index
// would answer differently on consecutive runs — a wrong answer wearing the face
// of a flaky test.
func TestTermsSortsBeforeItTruncates(t *testing.T) {
	docs := make([]Document, 64)
	for i := range docs {
		docs[i] = Document{Key: fmt.Sprintf("d%02d", i), Text: fmt.Sprintf("t%02d", i)}
	}
	ix := vocabCorpus(t, docs[:32], docs[32:])

	first := ix.Terms("t", 5)
	if !slices.Equal(first, []string{"t00", "t01", "t02", "t03", "t04"}) {
		t.Fatalf("Terms(t, 5) = %v, want the five smallest", first)
	}
	for range 20 {
		if got := ix.Terms("t", 5); !slices.Equal(got, first) {
			t.Fatalf("Terms(t, 5) = %v on a later call, want %v every time", got, first)
		}
	}
	if got := ix.Terms("t", 0); got != nil {
		t.Errorf("Terms(t, 0) = %v, want nothing", got)
	}
	if got := ix.Terms("t", -1); got != nil {
		t.Errorf("Terms(t, -1) = %v, want nothing", got)
	}
}

// TestPostingBoundBoundsTheRealList is the whole contract: a hint that is never
// under. A caller sizes an accumulator with it, so a bound below the truth is a
// map that grows anyway — harmless — while one the truth exceeds is the case
// this must never produce.
func TestPostingBoundBoundsTheRealList(t *testing.T) {
	committed := make([]Document, 300)
	for i := range committed {
		text := "everywhere"
		if i%50 == 0 {
			text += " occasional"
		}
		if i == 7 {
			text += " singular"
		}
		committed[i] = Document{Key: fmt.Sprintf("c%03d", i), Text: text}
	}
	ix := vocabCorpus(t, committed, []Document{
		{Key: "p0", Text: "everywhere pendingonly"},
		{Key: "p1", Text: "everywhere"},
	})

	for _, term := range ix.Terms("", 100) {
		bound, actual := ix.PostingBound(term), len(ix.Lookup(term))
		if bound < actual {
			t.Errorf("PostingBound(%q) = %d, below the %d postings Lookup returns", term, bound, actual)
		}
		if actual > 0 && bound == 0 {
			t.Errorf("PostingBound(%q) = 0 for a term held by %d documents", term, actual)
		}
	}
	if got := ix.PostingBound("neverindexed"); got != 0 {
		t.Errorf("PostingBound of an absent term = %d, want 0", got)
	}
	// The pending half has to be counted, or a term added since the last commit
	// reads as absent to whoever is sizing for it.
	if got := ix.PostingBound("pendingonly"); got < 1 {
		t.Errorf("PostingBound(pendingonly) = %d, want at least 1", got)
	}
}

// TestPostingBoundIsTightEnoughToBeWorthReading. A bound is free to be the
// corpus and correct; it would also be the thing it was written to replace. One
// block of slack per segment is what the block count buys, and a term held by
// every document must not round up to several times the corpus.
func TestPostingBoundIsTightEnoughToBeWorthReading(t *testing.T) {
	docs := make([]Document, 1000)
	for i := range docs {
		docs[i] = Document{Key: fmt.Sprintf("d%04d", i), Text: "everywhere"}
	}
	ix := vocabCorpus(t, docs, nil)

	bound, actual := ix.PostingBound("everywhere"), len(ix.Lookup("everywhere"))
	if actual != 1000 {
		t.Fatalf("Lookup returned %d postings, want 1000", actual)
	}
	if bound < actual || bound > actual+blockSize {
		t.Errorf("PostingBound = %d for %d postings; want it within one block (%d) above",
			bound, actual, blockSize)
	}
}

// TestPostingBoundDoesNotSubtractTombstones states the one thing about the
// number that is easy to assume and wrong. A deleted document keeps its posting
// where it was and Lookup filters at read time, so the bound still counts it —
// which is what a bound is for, and is why nothing may treat it as a count.
func TestPostingBoundDoesNotSubtractTombstones(t *testing.T) {
	ix := vocabCorpus(t, []Document{
		{Key: "a", Text: "shared"},
		{Key: "b", Text: "shared"},
	}, nil)

	before := ix.PostingBound("shared")
	if !ix.Delete("a") {
		t.Fatal("Delete(a) = false")
	}
	if got := len(ix.Lookup("shared")); got != 1 {
		t.Fatalf("Lookup returned %d postings after a delete, want 1", got)
	}
	if got := ix.PostingBound("shared"); got != before {
		t.Errorf("PostingBound = %d after a delete, want the unchanged %d — it bounds "+
			"what is written, not what is live", got, before)
	}
}
