// SPDX-License-Identifier: Apache-2.0

package text

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

// index builds an index from texts in order, so DocIDs match argument order.
func index(t *testing.T, texts ...string) *engine.Index {
	t.Helper()
	ix := engine.New()
	for i, txt := range texts {
		if _, err := ix.Add(engine.Document{Key: string(rune('a' + i)), Text: txt}); err != nil {
			t.Fatalf("Add(%q): %v", txt, err)
		}
	}
	return ix
}

func score(t *testing.T, ix *engine.Index, query string, doc engine.DocID) float64 {
	t.Helper()
	cands, err := New(ix).Candidates(t.Context(), engine.Query{Text: query}, 100)
	if err != nil {
		t.Fatalf("Candidates(%q): %v", query, err)
	}
	for _, c := range cands {
		if c.Doc == doc {
			return c.Score
		}
	}
	t.Fatalf("Candidates(%q) did not return doc %d (got %+v)", query, doc, cands)
	return 0
}

// The four boundary cases the plan requires, plus the empty-index case.
func TestCandidatesBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		texts []string
		query string
	}{
		{"absent term, n(q)=0", []string{"go rust"}, "python"},
		{"empty query", []string{"go rust"}, ""},
		{"query of separators only", []string{"go rust"}, "!!! ???"},
		{"empty index", nil, "go"},
		{"corpus of empty documents, avgdl=0", []string{"", "!!!"}, "go"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := New(index(t, tc.texts...)).Candidates(t.Context(), engine.Query{Text: tc.query}, 10)
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("Candidates = %+v, want none", got)
			}
		})
	}
}

func TestSingleDocumentCorpusScoresPositive(t *testing.T) {
	// N = 1 and n(q) = 1, the case where the classic IDF form yields
	// ln(0.5/1.5) < 0 and the document would score negative.
	ix := index(t, "go")
	if got := score(t, ix, "go", 0); got <= 0 {
		t.Fatalf("score = %v, want > 0 — IDF went non-positive on a single-doc corpus", got)
	}
}

func TestTermInEveryDocumentStillScoresPositive(t *testing.T) {
	// n(q) == N is where the classic IDF form is most negative.
	ix := index(t, "go", "go", "go")
	cands, err := New(ix).Candidates(t.Context(), engine.Query{Text: "go"}, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cands) != 3 {
		t.Fatalf("got %d candidates, want 3", len(cands))
	}
	for _, c := range cands {
		if c.Score <= 0 {
			t.Fatalf("doc %d scored %v, want > 0", c.Doc, c.Score)
		}
	}
}

func TestLengthNormalizationFavoursShorterDocuments(t *testing.T) {
	// Same term, same frequency, different lengths. If the normalization sign
	// were flipped, the long document would win.
	ix := index(t, "go", "go filler filler filler filler filler")
	short, long := score(t, ix, "go", 0), score(t, ix, "go", 1)
	if short <= long {
		t.Fatalf("short doc scored %v, long doc %v — length normalization is inverted or absent", short, long)
	}
}

func TestTermFrequencySaturates(t *testing.T) {
	// Equal lengths isolate frequency from normalization: doc 1 has the term
	// twice, so it must score higher, but by less than 2x — that sub-linearity
	// is the whole point of the K1 saturation term.
	ix := index(t, "go pad pad pad", "go go pad pad")
	once, twice := score(t, ix, "go", 0), score(t, ix, "go", 1)
	if twice <= once {
		t.Fatalf("freq 2 scored %v, freq 1 scored %v — frequency is not counted", twice, once)
	}
	if twice >= 2*once {
		t.Fatalf("freq 2 scored %v, freq 1 scored %v — frequency is not saturating", twice, once)
	}
}

func TestMultiTermQuerySumsAcrossTerms(t *testing.T) {
	// A document matching both query terms must outrank one matching only one.
	ix := index(t, "go fusion", "go unrelated")
	both, one := score(t, ix, "go fusion", 0), score(t, ix, "go fusion", 1)
	if both <= one {
		t.Fatalf("two-term match scored %v, one-term match %v", both, one)
	}
}

func TestCandidatesRespectsK(t *testing.T) {
	ix := index(t, "go", "go", "go", "go")
	cands, err := New(ix).Candidates(t.Context(), engine.Query{Text: "go"}, 2)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want 2", len(cands))
	}
	if cands[0].Score < cands[1].Score {
		t.Fatal("candidates are not best-first")
	}

	for _, k := range []int{0, -1} {
		got, err := New(ix).Candidates(t.Context(), engine.Query{Text: "go"}, k)
		if err != nil {
			t.Fatalf("Candidates(k=%d): %v", k, err)
		}
		if len(got) != 0 {
			t.Fatalf("Candidates(k=%d) = %+v, want none", k, got)
		}
	}
}

func TestCandidatesHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := New(index(t, "go")).Candidates(ctx, engine.Query{Text: "go"}, 10); err == nil {
		t.Fatal("Candidates on a cancelled context returned no error")
	}
}

// cancelAfter reports itself cancelled only once Err has been called n times.
// A plain cancelled context is caught by the per-term check before the posting
// loop starts, so it cannot distinguish "cancellation is observed" from
// "cancellation is observed in time".
type cancelAfter struct {
	context.Context
	n int
}

func (c *cancelAfter) Err() error {
	if c.n > 0 {
		c.n--
		return nil
	}
	return context.Canceled
}

func TestCancellationIsObservedInsideThePostingScan(t *testing.T) {
	// One term, one posting list longer than the poll interval. The three free
	// calls are the pre-tokenize check, the per-term check and the i == 0 poll,
	// so the cancellation lands on the i == 1024 poll — inside the scan, which is
	// the only place the old code had no check at all. The count must include the
	// pre-tokenize check: while it did not, deleting the entire posting poll left
	// this test green, because the cancellation fell through to the guard before
	// TopK instead.
	ix := engine.New()
	for i := range 3000 {
		if _, err := ix.Add(engine.Document{Key: fmt.Sprintf("d%d", i), Text: "go"}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	ctx := &cancelAfter{Context: t.Context(), n: 3}
	got, err := New(ix).Candidates(ctx, engine.Query{Text: "go"}, 10)
	if err == nil {
		t.Fatalf("Candidates returned %d results and no error after mid-scan cancellation", len(got))
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Candidates error = %v, want context.Canceled", err)
	}
}

func TestScorerIgnoresNonTextQueryFields(t *testing.T) {
	// A vector-only query is not this scorer's business: it must return nothing
	// rather than erroring, so Search can run every scorer on every query.
	got, err := New(index(t, "go")).Candidates(t.Context(), engine.Query{Vector: []float32{1, 0}}, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Candidates = %+v, want none", got)
	}
}

func TestConcurrentAddNeverProducesNonPositiveScores(t *testing.T) {
	// Stats and Lookup take the read lock separately, so postings can be newer
	// than the document count. Unclamped that makes N - n(q) negative and the
	// IDF negative, inverting the ranking. Run this under -race.
	ix := engine.New()
	for i := range 20 {
		if _, err := ix.Add(engine.Document{Key: fmt.Sprintf("seed%d", i), Text: "go"}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	// The writer is bounded and the reader stops with it, so total work stays
	// linear. An unbounded writer would make every query scan a longer posting
	// list than the last.
	const writes = 300
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := range writes {
			// Errors are irrelevant here; the writer only has to keep the
			// corpus growing while queries run.
			_, _ = ix.Add(engine.Document{Key: fmt.Sprintf("more%d", i), Text: "go"})
		}
	}()

	s := New(ix)
	queries := 0
	for {
		select {
		case <-done:
			wg.Wait()
			if queries == 0 {
				t.Fatal("the writer finished before any query ran")
			}
			return
		default:
		}
		cands, err := s.Candidates(t.Context(), engine.Query{Text: "go"}, 10)
		if err != nil {
			t.Fatalf("Candidates: %v", err)
		}
		for _, c := range cands {
			if c.Score <= 0 {
				t.Fatalf("doc %d scored %v during a concurrent Add — IDF went non-positive", c.Doc, c.Score)
			}
		}
		queries++
	}
}

func TestCancellationIsObservedBeforeTokenizing(t *testing.T) {
	// A query of pure punctuation tokenizes to no terms, so the per-term loop
	// never runs and the old code returned a successful empty result whatever the
	// context said. Not a review finding — the same shape as the seed and vector
	// scans, found by looking for the rest of them.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := New(engine.New()).Candidates(ctx, engine.Query{Text: "!!! ---"}, 10)
	if err == nil {
		t.Fatalf("Candidates returned %+v and no error for an untokenizable query after cancellation", got)
	}
}

func TestCancellationArrivingAfterTheLastPostingIsObserved(t *testing.T) {
	// One term over a one-document corpus, so the calls are countable: the
	// pre-tokenize check, the per-term check and the i == 0 poll. Cancellation
	// lands on the fourth, the check guarding TopK — the window between the last
	// posting and the sort, which no earlier check covers. The count must include
	// the pre-tokenize check, or the cancellation lands one check early and
	// deleting the pre-TopK guard leaves this green.
	ix := engine.New()
	if _, err := ix.Add(engine.Document{Key: "a", Text: "go"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	ctx := &cancelAfter{Context: t.Context(), n: 3}
	got, err := New(ix).Candidates(ctx, engine.Query{Text: "go"}, 10)
	if err == nil {
		t.Fatalf("Candidates returned %+v and no error after cancellation before the sort", got)
	}
}

// Compile-time proof that this satisfies the interface fusion never sees.
var _ engine.Scorer = (*Scorer)(nil)

// TestSearchIsSafeForConcurrentUse is the assumption internal/loadgen.Drive makes
// and nothing else in this tree had ever exercised.
//
// Every caller before milestone 5 ran queries one at a time: `run`, `sweep`,
// `weights` and `recall` all replay sequentially. `weft-eval bench` is the first to
// call engine.Search from 4×GOMAXPROCS goroutines against one Index and one scorer,
// and its whole latency distribution is meaningless if that races. The driver's own
// tests deliberately run without an index, so `make test -race` would not have
// caught it: this is where the -race gate reaches the read path.
//
// Fails only under -race, which `make all` runs.
func TestSearchIsSafeForConcurrentUse(t *testing.T) {
	ix := index(t, "the quick brown fox", "a lazy brown dog", "quick quick fox fox")
	s := New(ix)
	fuse := func(streams [][]engine.Candidate, k int) []engine.Candidate {
		if len(streams) == 0 {
			return nil
		}
		return engine.TopK(streams[0], k)
	}

	const goroutines, each = 8, 50
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Different queries per goroutine, so the postings lists being walked
			// overlap rather than being identical work the scheduler could serialise.
			q := []string{"quick", "brown fox", "lazy dog", "fox"}[g%4]
			for range each {
				if _, err := engine.Search(context.Background(), engine.Query{Text: q}, 3, fuse, s); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Search: %v", err)
	}
}

// A property, not a number: **a query's allocation must scale with the documents it
// matches, not with the size of the corpus it does not.** Two corpora that answer the
// same query with the same eight documents must cost about the same to score, whether
// the corpus around them holds sixty-four documents or sixteen thousand.
//
// The accumulator is hinted at the corpus size, so the hint is paid in full by a query
// matching eight documents: about twenty-eight bytes of map per document nothing looks
// at. On the evaluation corpus that is 4.5 MiB a query at 171,332 documents, and with
// forty requests in flight it is the order of the mark
// [FINDINGS](../../../docs/FINDINGS.md) milestone 8 §7 could not attribute.
//
// Written as a property because the fixes differ in mechanism — no hint at all, a hint
// from the posting list in hand, or a hint from the union of them — and all three satisfy
// it. The budget is deliberately loose: this is not a benchmark, it is a guard against
// allocation proportional to something the query never reads.

// allocBytesForTextCandidates commits a synthetic corpus in which only the first eight
// documents carry the query term, reopens it so the mapped decode path is the one under
// test, and reports bytes allocated by one scored query.
func allocBytesForTextCandidates(t *testing.T, docs int) uint64 {
	t.Helper()
	const matches = 8
	ix := engine.New()
	for i := range docs {
		// One unique token per document, so every posting list but the query term's
		// holds exactly one entry and nothing else in the index scales with docs.
		txt := fmt.Sprintf("filler%d", i)
		if i < matches {
			txt = "aardvark " + txt
		}
		if _, err := ix.Add(engine.Document{Key: fmt.Sprintf("d%d", i), Text: txt}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	dir := t.TempDir()
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	open, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer open.Close() //nolint:errcheck // nothing left to do about it in a test

	s := New(open)
	q := engine.Query{Text: "aardvark"}
	// One scan first: whatever the mapping and the scorer set up once is not what this
	// measures.
	if _, err := s.Candidates(t.Context(), q, matches); err != nil {
		t.Fatalf("Candidates warm-up: %v", err)
	}

	// TotalAlloc rather than a heap reading: it is cumulative and monotonic, so a
	// collection landing inside the window cannot move it.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := s.Candidates(t.Context(), q, matches); err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestScoringDoesNotAllocateForDocumentsTheQueryNeverMatches(t *testing.T) {
	const fewDocs, manyDocs = 64, 16384
	small := allocBytesForTextCandidates(t, fewDocs)
	large := allocBytesForTextCandidates(t, manyDocs)

	// A map entry costs about twenty-eight bytes of buckets at this key and value width,
	// which is what a corpus-sized hint charges per document whether the query touches it
	// or not. A tenth of the difference is the budget: enough slack for the map's own
	// bookkeeping over eight entries, far less than one bucket per document.
	const perDoc = 28
	if budget := uint64(manyDocs-fewDocs) * perDoc / 10; large > small+budget {
		t.Errorf("scoring allocated %d bytes over %d documents against %d over %d, "+
			"for the same eight matches: the accumulator is sized by the corpus rather "+
			"than by what the query found (budget was small+%d)",
			large, manyDocs, small, fewDocs, budget)
	}
}
