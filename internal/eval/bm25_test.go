package eval

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/scorer/text"
)

// This test lives here rather than in pkg/scorer/text because the reference
// apparatus does — the golden file, the generator that produced it and the JSON
// loading are all milestone 4's instrument. Putting it in pkg would give the
// library's own tests a fixture dependency on the evaluation harness, for a check
// that is not about the library's contract but about whether its arithmetic
// agrees with the outside world.

// bm25Tolerance: both sides are float64 accumulating the same query terms in the
// same order, so the only expected divergence is libm's last bits. A real
// disagreement is orders of magnitude larger than this.
const bm25Tolerance = 1e-12

type bm25Reference struct {
	GeneratedBy string  `json:"generated_by"`
	K1          float64 `json:"k1"`
	B           float64 `json:"b"`
	IDFForm     string  `json:"idf_form"`
	AvgDL       float64 `json:"avgdl"`
	Corpus      []struct {
		Key  string `json:"key"`
		Text string `json:"text"`
	} `json:"corpus"`
	Queries []struct {
		ID     string             `json:"id"`
		Text   string             `json:"text"`
		Tokens []string           `json:"tokens"`
		Scores map[string]float64 `json:"scores"`
	} `json:"queries"`
}

func loadBM25Reference(t *testing.T) bm25Reference {
	t.Helper()
	path := filepath.Join("testdata", "bm25_reference.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var ref bm25Reference
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(ref.Corpus) == 0 || len(ref.Queries) == 0 {
		t.Fatalf("%s has no corpus or no queries", path)
	}
	return ref
}

func referenceIndex(t *testing.T, ref bm25Reference) *engine.Index {
	t.Helper()
	ix := engine.New()
	for _, d := range ref.Corpus {
		if _, err := ix.Add(engine.Document{Key: d.Key, Text: d.Text}); err != nil {
			t.Fatalf("add %q: %v", d.Key, err)
		}
	}
	return ix
}

// TestBM25MatchesReferenceImplementation is the PRD's "correctness floor" row.
// The graph delta this milestone publishes is measured on top of the text
// baseline, so a wrong baseline makes the delta meaningless in a way no amount of
// statistical care would catch.
func TestBM25MatchesReferenceImplementation(t *testing.T) {
	ref := loadBM25Reference(t)

	// The golden was generated at specific parameters. If the constants move —
	// and the plan's phase 2 sweep intends to make them fields — this must fail
	// rather than compare against a stale reference.
	if ref.K1 != text.K1 || ref.B != text.B {
		t.Fatalf("reference was generated at K1=%v B=%v, package has K1=%v B=%v: regenerate bm25_reference.json",
			ref.K1, ref.B, text.K1, text.B)
	}

	ix := referenceIndex(t, ref)

	// avgdl is the input most likely to be subtly wrong — it is the only
	// corpus-wide quantity in the formula — and a mismatch here explains any
	// score mismatch below.
	if got := ix.AvgDocLen(); math.Abs(got-ref.AvgDL) > bm25Tolerance {
		t.Fatalf("AvgDocLen = %v, reference avgdl = %v", got, ref.AvgDL)
	}

	scorer := text.New(ix)
	var maxErr float64
	maxErrWhere := "none"

	for _, q := range ref.Queries {
		t.Run(q.ID, func(t *testing.T) {
			// Tokenization first. If the two sides split text differently they are
			// not scoring the same query, and every score below would differ for a
			// reason that has nothing to do with BM25.
			if got := engine.Tokenize(q.Text); !slices.Equal(got, q.Tokens) {
				t.Fatalf("Tokenize(%q) = %v, reference tokenized to %v", q.Text, got, q.Tokens)
			}

			cands, err := scorer.Candidates(context.Background(), engine.Query{Text: q.Text}, len(ref.Corpus))
			if err != nil {
				t.Fatalf("Candidates: %v", err)
			}

			got := make(map[string]float64, len(cands))
			for _, c := range cands {
				doc, ok := ix.Doc(c.Doc)
				if !ok {
					t.Fatalf("candidate doc %d is not in the index", c.Doc)
				}
				got[doc.Key] = c.Score
			}

			for key, want := range q.Scores {
				have, present := got[key]
				if want == 0 {
					// A document sharing no term with the query must be absent, not
					// present at zero: fusion consumes ranks, so a zero-scored
					// document occupying a rank is a vote it did not earn.
					if present {
						t.Errorf("%s scored 0 in the reference but weft returned it at %v", key, have)
					}
					continue
				}
				if !present {
					t.Errorf("%s scored %v in the reference but weft did not return it", key, want)
					continue
				}
				if d := math.Abs(have - want); d > bm25Tolerance {
					t.Errorf("%s = %.17g, reference = %.17g (diff %.3g)", key, have, want, d)
				} else if d > maxErr {
					maxErr, maxErrWhere = d, q.ID+"/"+key
				}
			}
		})
	}

	// Recorded, not just asserted: the PRD row asks for the error, and "within
	// tolerance" is a claim a reader should be able to see a number for.
	t.Logf("max absolute score error vs %s: %.3g (at %s), tolerance %.3g",
		ref.GeneratedBy, maxErr, maxErrWhere, bm25Tolerance)
}

// TestBM25UsesTheNonNegativeIDFForm checks the one thing the reference cannot:
// gen_bm25_reference.py substitutes weft's IDF into rank_bm25 rather than
// comparing the two forms, so the substitution would hide a swap between them.
//
// The IDF is recovered from a single-term score by dividing out the saturation
// term, which is fully determined by the fixture, and compared against both
// candidate formulas. On this corpus "beta" appears in 5 of 8 documents, which is
// exactly where the classic form goes negative — the case text.go says the
// Lucene form was chosen to rule out.
func TestBM25UsesTheNonNegativeIDFForm(t *testing.T) {
	ref := loadBM25Reference(t)
	ix := referenceIndex(t, ref)

	const term = "beta"
	n := float64(len(ix.Lookup(term)))
	total, avgdl := ix.Stats()
	N := float64(total)
	if n <= N/2 {
		t.Fatalf("%q is in %v of %v documents; the fixture must keep it above half "+
			"or this test no longer distinguishes the two IDF forms", term, n, N)
	}

	lucene := math.Log(1 + (N-n+0.5)/(n+0.5))
	classic := math.Log((N - n + 0.5) / (n + 0.5))
	if classic >= 0 {
		t.Fatalf("classic IDF is %v, want negative — the fixture no longer exercises the difference", classic)
	}

	// d4 is "beta delta": one occurrence of the term, length 2.
	id, ok := ix.Resolve("d4")
	if !ok {
		t.Fatal("d4 is missing from the fixture")
	}
	f, docLen := 1.0, float64(ix.DocLen(id))
	norm := 1 - text.B + text.B*docLen/avgdl
	saturation := f * (text.K1 + 1) / (f + text.K1*norm)

	cands, err := text.New(ix).Candidates(context.Background(), engine.Query{Text: term}, len(ref.Corpus))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	var score float64
	for _, c := range cands {
		if c.Doc == id {
			score = c.Score
		}
	}
	if score == 0 {
		t.Fatalf("d4 has no score for %q", term)
	}

	gotIDF := score / saturation
	if math.Abs(gotIDF-lucene) > bm25Tolerance {
		t.Errorf("recovered IDF = %.17g, Lucene form = %.17g, classic form = %.17g",
			gotIDF, lucene, classic)
	}
	if gotIDF <= 0 {
		t.Errorf("recovered IDF = %v; a common term must not subtract from a score", gotIDF)
	}
}
