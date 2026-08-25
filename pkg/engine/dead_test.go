// SPDX-License-Identifier: Apache-2.0

// This file is the milestone 11 pass/fail line, and it is package engine_test
// for the same reason architecture_test.go is: it imports the four scorers, and
// those import engine.
//
// The assertion is one sentence — a deleted document does not reach a scorer —
// and what makes it the milestone's experiment rather than a feature test is
// where it is *not* enforced. No scorer in this repository knows the word
// tombstone. If one has to, engine's read path failed to hide the deletion and
// the falsification condition in the PRD has fired.
package engine_test

import (
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/scorer/graph"
	"github.com/skyoo2003/weft/pkg/scorer/recency"
	"github.com/skyoo2003/weft/pkg/scorer/text"
	"github.com/skyoo2003/weft/pkg/scorer/vector"
)

// deletionCorpusSize is small enough that every assertion below can afford to
// ask for the whole corpus back, which is what makes "no dead document appears"
// a statement about the candidate set rather than about the top of it. A scorer
// asked for k=10 hides a leak at rank 11.
const deletionCorpusSize = 48

// deletionDoc is the i'th document of the corpus every test in this file uses.
//
// Every field feeds a different scorer and every document is visible to all
// four: shared text so BM25 ranks it, a vector so cosine does, a link from its
// predecessor so the traversal reaches it, and a timestamp so recency has an
// opinion. A document invisible to one scorer would let that scorer pass this
// file by having no candidates at all.
func deletionDoc(i int) engine.Document {
	return engine.Document{
		Key: deletionKey(i),
		// Three tokens, always, whichever document this is. The average length
		// is then exactly 3 for any subset, which is what lets
		// TestStatsExcludeDeletedDocumentsAndLenDoesNot assert a number rather
		// than a direction.
		Text: "alpha beta " + deletionTerm(i),
		// Four dimensions and a coarse spread: the point is that every document
		// has a direction, not that the neighbourhoods are interesting.
		Vector: []float32{float32(i%7) + 1, float32(i%5) + 1, float32(i%3) + 1, 1},
		Links:  []string{deletionKey((i + 1) % deletionCorpusSize)},
		Time:   refNow.Add(-time.Duration(i) * time.Hour),
	}
}

func deletionKey(i int) string  { return "doc-" + strconv.Itoa(i) }
func deletionTerm(i int) string { return "term" + strconv.Itoa(i) }

// deletionCorpus fills ix with deletionCorpusSize documents, ids 0..n-1.
func deletionCorpus(t *testing.T, ix *engine.Index) {
	t.Helper()
	for i := range deletionCorpusSize {
		if _, err := ix.Add(deletionDoc(i)); err != nil {
			t.Fatalf("Add(%q): %v", deletionKey(i), err)
		}
	}
}

// deadPick chooses which documents to delete, deterministically from seed.
//
// Seeded rather than fixed, and reported on failure, because the interesting
// failures are positional: a tombstone that is the first posting of a block, the
// last id of a segment, or every member of one inverted list behaves differently
// from one in the middle, and a hand-picked subset tests whichever of those the
// author happened to think of.
func deadPick(seed uint64) map[string]bool {
	r := rand.New(rand.NewPCG(seed, 0x9e3779b9))
	dead := make(map[string]bool)
	for i := range deletionCorpusSize {
		if r.IntN(3) == 0 {
			dead[deletionKey(i)] = true
		}
	}
	return dead
}

// deleteAll removes every key in dead and returns their DocIDs, resolved before
// the deletion because afterwards Resolve is documented not to answer.
func deleteAll(t *testing.T, ix *engine.Index, dead map[string]bool) map[engine.DocID]string {
	t.Helper()
	ids := make(map[engine.DocID]string, len(dead))
	for key := range dead {
		id, ok := ix.Resolve(key)
		if !ok {
			t.Fatalf("Resolve(%q) before deleting it: not found", key)
		}
		ids[id] = key
		if !ix.Delete(key) {
			t.Errorf("Delete(%q) = false, want true — the document was there", key)
		}
	}
	return ids
}

// assertNoDeadAnywhere is the milestone assertion. Every read path a scorer can
// reach, then the four scorers themselves, then the fused result.
func assertNoDeadAnywhere(t *testing.T, ix *engine.Index, tombs map[engine.DocID]string, seed uint64) {
	t.Helper()

	fail := func(where string, id engine.DocID) {
		t.Errorf("seed %d: %s returned document %d (%q), which was deleted", seed, where, id, tombs[id])
	}

	// The point queries first. A scorer reaching a deleted document through one
	// of these is the leak; the scorer-level assertions below cannot say which
	// of them leaked.
	for id, key := range tombs {
		if _, ok := ix.Doc(id); ok {
			fail("Doc", id)
		}
		if _, ok := ix.Vector(id); ok {
			fail("Vector", id)
		}
		if got, ok := ix.Resolve(key); ok {
			t.Errorf("seed %d: Resolve(%q) = %d, want not found — the document was deleted", seed, key, got)
		}
		if n := ix.DocLen(id); n != 0 {
			t.Errorf("seed %d: DocLen(%d) = %d, want 0 for a deleted document", seed, id, n)
		}
	}

	// The inverted index. Every term the corpus ever held, not just the shared
	// one: a per-document term is the only posting in its list, and a filter
	// that empties a list is a different code path from one that shortens it.
	terms := []string{"alpha", "beta"}
	for i := range deletionCorpusSize {
		terms = append(terms, deletionTerm(i))
	}
	var buf []engine.Posting
	for _, term := range terms {
		for _, p := range ix.Lookup(term) {
			if _, dead := tombs[p.Doc]; dead {
				fail("Lookup("+term+")", p.Doc)
			}
		}
		buf = ix.LookupInto(term, buf)
		for _, p := range buf {
			if _, dead := tombs[p.Doc]; dead {
				fail("LookupInto("+term+")", p.Doc)
			}
		}
	}

	// The vector candidate set. Asked for the whole corpus so the answer is
	// every id the partition would ever offer.
	for _, id := range ix.Nearest(deletionDoc(0).Vector, deletionCorpusSize) {
		if _, dead := tombs[id]; dead {
			fail("Nearest", id)
		}
	}

	// And the four scorers, each asked for more than the corpus holds.
	txt := text.New(ix)
	scorers := []engine.Scorer{
		txt,
		vector.New(ix),
		graph.New(ix, txt),
		recency.NewAt(ix, refNow),
	}
	q := engine.Query{
		Text:   "alpha beta",
		Vector: deletionDoc(0).Vector,
		Seeds:  []string{deletionKey(0)},
	}
	for _, s := range scorers {
		cands, err := s.Candidates(t.Context(), q, deletionCorpusSize*2)
		if err != nil {
			t.Fatalf("seed %d: %s.Candidates: %v", seed, s.Name(), err)
		}
		for _, c := range cands {
			if _, dead := tombs[c.Doc]; dead {
				fail(s.Name()+".Candidates", c.Doc)
			}
		}
	}

	// The fused result last. It cannot leak what the streams did not, so this is
	// a regression net rather than an independent check — and it is the shape a
	// caller actually runs.
	fused, err := engine.Search(t.Context(), q, deletionCorpusSize*2, fusion.Fuse, scorers...)
	if err != nil {
		t.Fatalf("seed %d: Search: %v", seed, err)
	}
	for _, c := range fused {
		if _, dead := tombs[c.Doc]; dead {
			fail("Search", c.Doc)
		}
	}
}

// TestNoScorerEverSeesADeletedDocument is the milestone 11 assertion in its
// cheapest state: everything pending, nothing written.
//
// Several seeds, because which documents are deleted is exactly what a
// positional bug depends on — see deadPick.
func TestNoScorerEverSeesADeletedDocument(t *testing.T) {
	for seed := uint64(1); seed <= 8; seed++ {
		ix := engine.New()
		deletionCorpus(t, ix)
		tombs := deleteAll(t, ix, deadPick(seed))
		assertNoDeadAnywhere(t, ix, tombs, seed)
	}
}

// TestDeletionSurvivesACommit is the same assertion once the documents a
// tombstone names live in a mapped segment rather than in memory.
//
// The reopened index is the load-bearing half. An in-memory tombstone that is
// never written would pass every assertion above and resurrect the whole set on
// the next Open, which is the failure mode a caller discovers in production
// rather than in a test.
func TestDeletionSurvivesACommit(t *testing.T) {
	const seed = 3
	dir := t.TempDir()

	ix := engine.New()
	deletionCorpus(t, ix)
	tombs := deleteAll(t, ix, deadPick(seed))
	if err := ix.Commit(t.Context(), dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	assertNoDeadAnywhere(t, ix, tombs, seed)
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close() //nolint:errcheck // teardown
	assertNoDeadAnywhere(t, reopened, tombs, seed)
}

// TestDeletionAfterACommitSurvivesAReopen deletes documents that were already on
// disk, which is the other order and a different code path: the tombstone names
// an id no pending structure has ever held.
func TestDeletionAfterACommitSurvivesAReopen(t *testing.T) {
	const seed = 5
	dir := t.TempDir()

	ix := engine.New()
	deletionCorpus(t, ix)
	if err := ix.Commit(t.Context(), dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	tombs := deleteAll(t, ix, deadPick(seed))
	assertNoDeadAnywhere(t, ix, tombs, seed)

	// A commit with nothing pending and a tombstone set that grew. Publishing it
	// is the whole point: without it the deletions above are lost on Close.
	if err := ix.Commit(t.Context(), dir); err != nil {
		t.Fatalf("Commit after deleting: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close() //nolint:errcheck // teardown
	assertNoDeadAnywhere(t, reopened, tombs, seed)
}

// TestDeletionSurvivesAMerge is the third state. Merge rewrites the oldest
// segments, and a tombstone whose id it renumbered — or dropped — would
// resurrect a document or kill a live one.
func TestDeletionSurvivesAMerge(t *testing.T) {
	const seed = 7
	dir := t.TempDir()

	// One document per commit, so the segment count passes maxSegments and Merge
	// has something to collapse. deletionCorpusSize commits is far past the
	// ceiling, which is what makes the merge more than one segment's worth of
	// work.
	ix := engine.New()
	for i := range deletionCorpusSize {
		if _, err := ix.Add(deletionDoc(i)); err != nil {
			t.Fatalf("Add(%q): %v", deletionKey(i), err)
		}
		if err := ix.Commit(t.Context(), dir); err != nil {
			t.Fatalf("Commit %d: %v", i, err)
		}
	}
	tombs := deleteAll(t, ix, deadPick(seed))
	if err := ix.Commit(t.Context(), dir); err != nil {
		t.Fatalf("Commit after deleting: %v", err)
	}
	if err := ix.Merge(); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	assertNoDeadAnywhere(t, ix, tombs, seed)
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("Open after merge: %v", err)
	}
	defer reopened.Close() //nolint:errcheck // teardown
	assertNoDeadAnywhere(t, reopened, tombs, seed)

	// And the survivors are all still there. Every assertion above is satisfied
	// by an index that deleted everything, which is why this one is here.
	dead := deadPick(seed)
	for i := range deletionCorpusSize {
		key := deletionKey(i)
		if dead[key] {
			continue
		}
		if _, ok := reopened.Resolve(key); !ok {
			t.Errorf("Resolve(%q) after a merge: not found — a live document was collected", key)
		}
	}
}

// TestStatsExcludeDeletedDocumentsAndLenDoesNot pins the split decided in the
// plan, and it is a split rather than an oversight.
//
// Stats is what BM25 normalizes against, so a tombstone left in the count makes
// every IDF quietly wrong. Len is the id bound rather than a population — the
// recency scorer walks 0..Len() and skips what Doc refuses — so narrowing it
// would hide the tail of the corpus from a scorer that never mentions deletion.
func TestStatsExcludeDeletedDocumentsAndLenDoesNot(t *testing.T) {
	const seed = 2
	ix := engine.New()
	deletionCorpus(t, ix)
	tombs := deleteAll(t, ix, deadPick(seed))

	if got, want := ix.Len(), deletionCorpusSize; got != want {
		t.Errorf("Len() = %d after deleting %d of %d, want %d — Len is the id bound, not the population",
			got, len(tombs), deletionCorpusSize, want)
	}
	docs, avg := ix.Stats()
	if want := deletionCorpusSize - len(tombs); docs != want {
		t.Errorf("Stats() counts %d documents, want %d live", docs, want)
	}
	// Three tokens each, all the same shape, so the live average is exactly 3
	// whichever documents were deleted. That is what says the token total moved
	// with the count rather than staying behind it.
	if avg != 3 {
		t.Errorf("Stats() average length = %v, want 3 — deleted documents are still in the token total", avg)
	}
}
