// SPDX-License-Identifier: Apache-2.0

// This file is the milestone 2 pass/fail line, the way architecture_test.go
// is milestone 1's. One assertion, from the plan: an index restored from disk
// is indistinguishable from the index that was committed — same four scorers,
// same fused ranking, same scores, for every query.
//
// It is package engine_test for the same reason architecture_test.go is: it
// imports the scorers, and the scorers import engine.
package engine_test

import (
	"slices"
	"testing"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/scorer/graph"
	"github.com/skyoo2003/weft/pkg/scorer/recency"
	"github.com/skyoo2003/weft/pkg/scorer/text"
	"github.com/skyoo2003/weft/pkg/scorer/vector"
)

// scorers builds the standard four against one index, the same shape the
// architecture test and the demo use. Both sides of the equivalence must call
// this — the committed index and the restored one get identically configured
// scorers, so any difference in results is the disk's doing.
func scorers(ix *engine.Index, now time.Time) []engine.Scorer {
	txt := text.New(ix)
	return []engine.Scorer{txt, vector.New(ix), graph.New(ix, txt), recency.NewAt(ix, now)}
}

func TestRestoredIndexRanksIdentically(t *testing.T) {
	ix := corpus(t) // architecture_test.go's corpus: vectors, links, zero-time lonely doc
	if _, err := ix.Add(engine.Document{
		Key:  "dangler",
		Text: "ranking scorer prose",
		// Links to a document that was never added: the dangling edge has to
		// survive the disk exactly as it is, unresolved.
		Links: []string{"nowhere"},
		Time:  refNow.Add(-24 * time.Hour),
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	dir := t.TempDir()
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	restored, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	queries := []struct {
		name string
		q    engine.Query
		k    int
	}{
		{"text only", engine.Query{Text: "fusion scorer"}, 5},
		{"text and vector", engine.Query{Text: "fusion scorer", Vector: []float32{1, 0, 0}}, 5},
		{"vector only", engine.Query{Vector: []float32{0, 1, 0}}, 5},
		{"caller-named seeds", engine.Query{Text: "ranking", Seeds: []string{"a"}}, 5},
		{"nothing matches", engine.Query{Text: "xylophone quintet"}, 5},
		{"k of one", engine.Query{Text: "fusion scorer", Vector: []float32{1, 0, 0}}, 1},
		{"k past the corpus", engine.Query{Text: "fusion scorer", Vector: []float32{1, 0, 0}}, 100},
	}
	for _, tt := range queries {
		t.Run(tt.name, func(t *testing.T) {
			want, err := engine.Search(t.Context(), tt.q, tt.k, fusion.Fuse, scorers(ix, refNow)...)
			if err != nil {
				t.Fatalf("Search before commit: %v", err)
			}
			got, err := engine.Search(t.Context(), tt.q, tt.k, fusion.Fuse, scorers(restored, refNow)...)
			if err != nil {
				t.Fatalf("Search after restore: %v", err)
			}
			// Candidate is two comparable fields, so slices.Equal demands the
			// same documents, the same order, and bit-identical scores. Scores
			// only match exactly because the restored postings, lengths and
			// statistics are exactly the committed ones — which is the point.
			if !slices.Equal(got, want) {
				t.Errorf("restored ranking differs:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}
