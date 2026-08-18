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
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/scorer/graph"
	"github.com/skyoo2003/weft/pkg/scorer/recency"
	"github.com/skyoo2003/weft/pkg/scorer/text"
	"github.com/skyoo2003/weft/pkg/scorer/vector"
)

// rankOf reports where doc landed, 1-based, or 0 when it is absent.
func rankOf(cands []engine.Candidate, want engine.DocID) int {
	for i, c := range cands {
		if c.Doc == want {
			return i + 1
		}
	}
	return 0
}

// TestASignalCanCarryDataDocumentDoesNotHold is the milestone 6 claim reduced to
// one assertion: a fifth scorer whose entire input is a map the engine has never
// seen changes the ranking, and does so through the same Search call the other
// four use.
//
// The document it promotes is "d", which the corpus wrote to be uninteresting to
// every existing scorer — unrelated prose, an orthogonal vector, no links, and
// the oldest timestamp. If a side store is a real extension path, popularity
// alone has to be able to move it; if it is not, no amount of view count will.
func TestASignalCanCarryDataDocumentDoesNotHold(t *testing.T) {
	ix := corpus(t)
	txt := text.New(ix)
	q := engine.Query{Text: "fusion scorer", Vector: []float32{1, 0, 0}}

	four := []engine.Scorer{txt, vector.New(ix), graph.New(ix, txt), recency.NewAt(ix, refNow)}

	// The data engine.Document has nowhere to put. Keyed by Key, because that is
	// the caller's own identifier and the only one that means anything before an
	// Add has happened or after a restart.
	views := map[string]int{"a": 3, "b": 2, "c": 1, "d": 1000, "lonely": 0}

	// Five scorers, and note the call below is the four-scorer call with one more
	// element. That is the milestone 1 shape holding for a scorer that lives
	// outside every package weft ships.
	five := append(append([]engine.Scorer{}, four...), newPopularity(ix, views))

	before, err := engine.Search(t.Context(), q, 5, fusion.Fuse, four...)
	if err != nil {
		t.Fatalf("Search with 4 scorers: %v", err)
	}
	after, err := engine.Search(t.Context(), q, 5, fusion.Fuse, five...)
	if err != nil {
		t.Fatalf("Search with 5 scorers: %v", err)
	}

	d, ok := ix.Resolve("d")
	if !ok {
		t.Fatal("corpus is missing document d")
	}
	was, now := rankOf(before, d), rankOf(after, d)
	if was == 0 {
		t.Fatalf("d was absent from the 4-scorer ranking, so this test cannot tell a promotion from an appearance: %+v", before)
	}
	if now >= was {
		t.Fatalf("d ranked %d without popularity and %d with it: a signal carrying data Document does not hold is not reaching fusion", was, now)
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
