// SPDX-License-Identifier: Apache-2.0

package main

import (
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
