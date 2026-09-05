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
// segment's decoded dictionary with the pending map, and PostingCount adds a
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

// TestPostingCountAgreesWithLookup is the whole contract, and it is equality
// rather than a bound: IDF is computed from this number, so a count that is off
// by the width of a block drifts every score in the corpus by an amount that
// depends on where the block boundaries fell.
func TestPostingCountAgreesWithLookup(t *testing.T) {
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
		if got, want := ix.PostingCount(term), len(ix.Lookup(term)); got != want {
			t.Errorf("PostingCount(%q) = %d, Lookup returns %d postings", term, got, want)
		}
	}
	if got := ix.PostingCount("neverindexed"); got != 0 {
		t.Errorf("PostingCount of an absent term = %d, want 0", got)
	}
	// The pending half has to be counted, or a term added since the last commit
	// reads as absent to whoever is sizing for it.
	if got := ix.PostingCount("pendingonly"); got < 1 {
		t.Errorf("PostingCount(pendingonly) = %d, want at least 1", got)
	}
}

// TestPostingCountSubtractsTombstones. A deleted document keeps its posting
// where it was and Lookup filters at read time, so a count that did not would
// disagree with the list it is supposed to describe — and IDF would be computed
// against documents no query can reach.
func TestPostingCountSubtractsTombstones(t *testing.T) {
	ix := vocabCorpus(t, []Document{
		{Key: "a", Text: "shared"},
		{Key: "b", Text: "shared"},
	}, nil)

	if got := ix.PostingCount("shared"); got != 2 {
		t.Fatalf("PostingCount = %d before any delete, want 2", got)
	}
	if !ix.Delete("a") {
		t.Fatal("Delete(a) = false")
	}
	if got := len(ix.Lookup("shared")); got != 1 {
		t.Fatalf("Lookup returned %d postings after a delete, want 1", got)
	}
	if got := ix.PostingCount("shared"); got != 1 {
		t.Errorf("PostingCount = %d after a delete, want 1 — it counts what is live", got)
	}
}
