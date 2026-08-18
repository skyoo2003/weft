// SPDX-License-Identifier: Apache-2.0

// This file is milestone 6's first question, asked in code rather than in prose:
// can a signal that weft has never heard of be added from outside the module?
//
// The milestone 1 assertions next door measured a fourth scorer written by
// someone holding the whole tree. Every one of those four reads a field
// engine.Document already has — Text, Vector, Links, Time — so none of them ever
// had to answer where an outsider's own data lives. Document is closed and Query
// is closed, and doc.go tells a fifth scorer to "add a field here", which is a
// maintainer's instruction and not available to anyone else.
//
// So the path an outsider actually has is a side store of their own, joined to
// the corpus through the two exported halves of the identity map, Resolve and
// Doc. Whether that is sufficient is what these tests decide. They are plain
// Test functions rather than Examples on purpose: godoc renders Examples, and
// the adoption trial in docs/ADOPTION.md is blind by construction — it must not
// be able to read the answer out of the documentation it is handed.
package engine_test

import (
	"context"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/scorer/graph"
	"github.com/skyoo2003/weft/pkg/scorer/recency"
	"github.com/skyoo2003/weft/pkg/scorer/text"
	"github.com/skyoo2003/weft/pkg/scorer/vector"
)

// popularity is the fifth signal, and it is written here rather than under
// pkg/scorer to keep it in an outsider's position: engine_test is a separate
// package, so it can reach nothing this repository does not export.
//
// Its input is a view count per Key that no engine.Document field can hold. The
// join is Resolve, the exported half of the identity map that turns a caller's
// own identifier into the DocID fusion compares.
type popularity struct {
	ix    *engine.Index
	views map[string]int
}

func newPopularity(ix *engine.Index, views map[string]int) engine.Scorer {
	return &popularity{ix: ix, views: views}
}

func (p *popularity) Name() string { return "popularity" }

// Candidates walks the side store, not the corpus.
//
// scorer/recency — the 99-line proof that a fourth signal is cheap, and the only
// implementation an outsider has to copy — sweeps every DocID and calls Doc on
// each, which decodes a whole record per document. Milestone 5 section 3.2 found
// that same decode to be where throughput collapses under concurrency. A side
// store does not have to inherit that: its keys are its own, so the loop is over
// what the caller knows rather than over the corpus, and Doc is never called.
func (p *popularity) Candidates(ctx context.Context, q engine.Query, k int) ([]engine.Candidate, error) {
	if k <= 0 {
		return nil, nil
	}
	cands := make([]engine.Candidate, 0, len(p.views))
	for key, n := range p.views {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id, ok := p.ix.Resolve(key)
		if !ok {
			// A key this index has never seen. No opinion, the same as a scorer
			// handed a query with no vector — not an error.
			continue
		}
		cands = append(cands, engine.Candidate{Doc: id, Score: float64(n)})
	}
	return engine.TopK(cands, k), nil
}

// TestASignalCanCarryDataDocumentDoesNotHold is the milestone 6 claim reduced to
// one assertion: a scorer whose entire input is a map the engine has never seen
// reaches the ranking, through the same Search call every other scorer uses.
//
// The document it surfaces is "d", which the corpus wrote to be uninteresting —
// unrelated prose, an orthogonal vector, no links, the oldest timestamp. Text
// alone cannot see it. If a side store is a real extension path, a view count
// has to be able to put it there, and if it is not, no view count will.
//
// It is asked of two streams rather than five on purpose. What five would also
// measure is how loud one equal RRF vote is against four others, which milestone
// 4 already answered and which is a property of the fuser rather than of the
// extension path. The five-scorer call is still made below, for the milestone 1
// question of whether the call shape survives a scorer weft does not ship.
func TestASignalCanCarryDataDocumentDoesNotHold(t *testing.T) {
	ix := corpus(t)
	txt := text.New(ix)
	q := engine.Query{Text: "fusion scorer", Vector: []float32{1, 0, 0}}

	// The data engine.Document has nowhere to put. Keyed by Key, because that is
	// the caller's own identifier and the only one that means anything before an
	// Add has happened or after a restart.
	views := map[string]int{"a": 3, "b": 2, "c": 1, "d": 1000, "lonely": 0}
	pop := newPopularity(ix, views)

	d, ok := ix.Resolve("d")
	if !ok {
		t.Fatal("corpus is missing document d")
	}

	before, err := engine.Search(t.Context(), q, 3, fusion.Fuse, txt)
	if err != nil {
		t.Fatalf("Search with text alone: %v", err)
	}
	after, err := engine.Search(t.Context(), q, 3, fusion.Fuse, txt, pop)
	if err != nil {
		t.Fatalf("Search with text and popularity: %v", err)
	}

	if contains(before, d) {
		t.Fatalf("text alone already ranked d — the corpus no longer isolates the new signal: %+v", before)
	}
	if !contains(after, d) {
		t.Fatalf("d is still absent once popularity is added: a signal carrying data Document does not hold is not reaching fusion: %+v", after)
	}

	// Five scorers, four of them weft's own, and this is the four-scorer call
	// with one more element in the slice. Nothing about invoking Search or Fuse
	// changed for a scorer that lives outside every package weft ships — that it
	// compiles and returns is the assertion.
	five := []engine.Scorer{txt, vector.New(ix), graph.New(ix, txt), recency.NewAt(ix, refNow), pop}
	if _, err := engine.Search(t.Context(), q, 5, fusion.Fuse, five...); err != nil {
		t.Fatalf("Search with 5 scorers, the fifth from outside the module: %v", err)
	}
}

// TestASideStoreSurvivesCommitAndOpen is the cost half of the same question.
//
// A side store is keyed by Key and scored by DocID, so it is only usable across
// a restart if that mapping is stable — and nothing in weft carries the store
// itself, since Commit writes documents and knows of no other data. An outsider
// therefore rebuilds it on every Open, and this test pins the one thing that
// makes the rebuild possible. If DocIDs were reassigned by Open, an external
// signal would keep working and silently score the wrong documents, which is the
// failure worth having a test for.
func TestASideStoreSurvivesCommitAndOpen(t *testing.T) {
	dir := t.TempDir()
	ix := corpus(t)
	views := map[string]int{"a": 3, "b": 2, "c": 1, "d": 1000, "lonely": 0}
	q := engine.Query{Text: "fusion scorer", Vector: []float32{1, 0, 0}}

	want, err := engine.Search(t.Context(), q, 5, fusion.Fuse, newPopularity(ix, views))
	if err != nil {
		t.Fatalf("Search before commit: %v", err)
	}
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	restored, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer restored.Close()

	// The rebuild an outsider has to perform: every key in the side store must
	// still name the document it named before.
	for key := range views {
		id, ok := restored.Resolve(key)
		if !ok {
			t.Fatalf("Resolve(%q) after Open: not found — a side store cannot be rebuilt", key)
		}
		doc, ok := restored.Doc(id)
		if !ok {
			t.Fatalf("Doc(%v) after Open: not found, though Resolve(%q) returned it", id, key)
		}
		if doc.Key != key {
			t.Fatalf("Resolve(%q) after Open gave the document keyed %q: an external signal would score the wrong document", key, doc.Key)
		}
	}

	got, err := engine.Search(t.Context(), q, 5, fusion.Fuse, newPopularity(restored, views))
	if err != nil {
		t.Fatalf("Search after Open: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ranking after Open has %d results, before had %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Doc != want[i].Doc {
			t.Fatalf("rank %d after Open is doc %v, was %v: the side store no longer points where it did", i+1, got[i].Doc, want[i].Doc)
		}
	}
}
