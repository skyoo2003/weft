// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

// TestWorkingSetCountsDistinctPages pins the one piece of arithmetic in the
// recall report that is neither a sum nor an average.
//
// It matters because the number it produces is published: FINDINGS milestone 3b
// reports 210.1 MiB of pages against 124.1 MiB of records, and the 1.7× between
// them is the finding. That ratio is page-granularity rounding minus the pages
// two candidates happen to share, so a counter that double-counted a shared page
// or dropped a spanned one would move a published multiplier rather than fail
// visibly.
//
// The layouts below are the cases that arithmetic has: a record inside one page,
// a record across a boundary, a record across several, two records sharing the
// page between them, and a gap. Offsets ascend, which is what Index.Nearest
// promises its ids do and what lets the count be a running one.
func TestWorkingSetCountsDistinctPages(t *testing.T) {
	const p = recallPageSize
	tests := []struct {
		name      string
		off, size []int64
		ids       []int
		wantBytes int
		wantPages int
	}{
		{
			name: "two records inside one page",
			off:  []int64{0, 100}, size: []int64{100, 100},
			ids: []int{0, 1}, wantBytes: 200, wantPages: 1,
		},
		{
			name: "one record across a boundary",
			off:  []int64{p - 100}, size: []int64{200},
			ids: []int{0}, wantBytes: 200, wantPages: 2,
		},
		{
			name: "one record across three pages",
			off:  []int64{0}, size: []int64{2*p + 1},
			ids: []int{0}, wantBytes: 2*p + 1, wantPages: 3,
		},
		{
			name: "consecutive records share the page between them",
			off:  []int64{0, p + 200}, size: []int64{p + 100, 100},
			ids: []int{0, 1}, wantBytes: p + 200, wantPages: 2,
		},
		{
			name: "a gap between records is not counted",
			off:  []int64{0, 10 * p}, size: []int64{100, 100},
			ids: []int{0, 1}, wantBytes: 200, wantPages: 2,
		},
		// The two cases that pin the −1 in the last-page formula. Every layout above
		// happens to end mid-page, so all of them pass with `(off+size)/p` too — and
		// that formula charges an extra page for every record whose end lands exactly
		// on a boundary, which inflates the published pages/records multiplier rather
		// than failing anywhere.
		{
			name: "a record filling exactly one page is one page",
			off:  []int64{0}, size: []int64{p},
			ids: []int{0}, wantBytes: p, wantPages: 1,
		},
		{
			name: "a record ending on a boundary mid-file is one page",
			off:  []int64{p}, size: []int64{p},
			ids: []int{0}, wantBytes: p, wantPages: 1,
		},
		{
			name: "an id past the corpus is skipped rather than counted",
			off:  []int64{0}, size: []int64{100},
			ids: []int{0, 7, -1}, wantBytes: 100, wantPages: 1,
		},
		{
			name: "no candidates reach nothing",
			off:  []int64{0}, size: []int64{100},
			ids: nil, wantBytes: 0, wantPages: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ids := make([]engine.DocID, len(tc.ids))
			for i, id := range tc.ids {
				ids[i] = engine.DocID(id) //nolint:gosec // a synthetic id, not a width off disk
			}
			gotBytes, gotPages := workingSet(extents{off: tc.off, size: tc.size}, ids)
			if gotBytes != tc.wantBytes {
				t.Errorf("bytes = %d, want %d", gotBytes, tc.wantBytes)
			}
			if gotPages != tc.wantPages {
				t.Errorf("pages = %d, want %d", gotPages, tc.wantPages)
			}
		})
	}
}

// TestRecordExtentsRefusesAnIndexThatHasBeenDeletedFrom is the Len-is-not-a-count
// case, reached through the tool that publishes a figure from it.
//
// The working set under every recall figure is derived by walking ids 0..Len-1
// and sizing each document, and since deletion exists Len is one past the highest
// id rather than a population. A tombstoned id reads back as absent while its
// bytes are still in the docs section, so the walk cannot size it and the witness
// that every byte is accounted for cannot balance — and the recall figure would be
// wrong anyway, because the exact scan it grades Nearest against scores ids
// Nearest has already dropped. Refused, the way a second segment is.
func TestRecordExtentsRefusesAnIndexThatHasBeenDeletedFrom(t *testing.T) {
	build := func(t *testing.T, deleted string) (*engine.Index, string) {
		t.Helper()
		dir := t.TempDir()
		ix := engine.New()
		for _, k := range []string{"a", "b", "c"} {
			if _, err := ix.Add(engine.Document{Key: k, Text: "cat " + k, Vector: []float32{1, 0}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := ix.Commit(t.Context(), dir); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if deleted != "" {
			if !ix.Delete(deleted) {
				t.Fatalf("Delete(%q): false", deleted)
			}
			// A delete-only commit publishes a generation and no segment, so the
			// one-segment precondition still holds and this reaches the new refusal
			// rather than that one.
			if err := ix.Commit(t.Context(), dir); err != nil {
				t.Fatalf("Commit after Delete: %v", err)
			}
		}
		t.Cleanup(func() { ix.Close() }) //nolint:errcheck // teardown
		return ix, dir
	}

	// The control, so the refusal below is refusing something rather than
	// everything: an index nobody deleted from derives its extents.
	if _, err := recordExtents(build(t, "")); err != nil {
		t.Fatalf("recordExtents on an index with nothing deleted: %v", err)
	}

	_, err := recordExtents(build(t, "b"))
	if err == nil {
		t.Fatal("recordExtents on an index with a tombstone succeeded; the working set it derived is missing a record")
	}
	if !strings.Contains(err.Error(), "deleted from") {
		t.Errorf("recordExtents: %v\nwant a message saying the index has been deleted from", err)
	}
}

// TestADegenerateQueryIsNotAveragedIn is about what the report divides by.
//
// Two queries reach measureRecall that the exact scan cannot grade: one whose
// vector is a width the corpus does not carry, and one whose vector is all
// zeros — the case scorer/vector abstains on before it ever calls Nearest.
// Both produce an empty exact answer, so neither adds to want, and the guard
// that catches a wholly ungradeable run does not fire while one good query
// remains. What they do add is a candidate list, a latency and a working set,
// to three figures printed per query. The wrong-width one is the expensive
// shape: Nearest answers a width it cannot partition with the whole segment,
// so one such query pulls a corpus-sized candidate count into an average that
// FINDINGS publishes as a percentage of the corpus.
func TestADegenerateQueryIsNotAveragedIn(t *testing.T) {
	ix := engine.New()
	for _, v := range [][]float32{{1, 0}, {0, 1}, {1, 1}} {
		if _, err := ix.Add(engine.Document{Key: fmt.Sprint(v), Text: "a", Vector: v}); err != nil {
			t.Fatal(err)
		}
	}
	// Three records of 100 bytes. The values do not matter to what is asserted
	// below, only that every id has an extent to be charged for.
	ext := extents{off: []int64{0, 100, 200}, size: []int64{100, 100, 100}, total: 300}

	st, err := measureRecall(t.Context(), ix, ext, []queryVec{
		{"gradeable", []float32{1, 0}},
		{"all-zero", []float32{0, 0}},
		{"wrong width", []float32{1, 0, 0}},
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if st.n != 1 || st.skipped != 2 {
		t.Errorf("graded %d queries and skipped %d, want 1 and 2", st.n, st.skipped)
	}
	// The one gradeable query's candidates and nothing else. This index holds no
	// partition, so Nearest offers every id it has and each skipped query would
	// have added another corpus to the sum.
	if want := ix.Len(); st.cands != want {
		t.Errorf("candidates = %d, want %d — a skipped query was averaged in", st.cands, want)
	}
	if st.bytesTouched != 300 {
		t.Errorf("bytes touched = %d, want 300 — a skipped query reached records of its own", st.bytesTouched)
	}
}
