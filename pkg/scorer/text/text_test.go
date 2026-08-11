package text

import (
	"context"
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

// Compile-time proof that this satisfies the interface fusion never sees.
var _ engine.Scorer = (*Scorer)(nil)
