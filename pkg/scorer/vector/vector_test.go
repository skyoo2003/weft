package vector

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

const eps = 1e-9

func index(t *testing.T, vecs ...[]float32) *engine.Index {
	t.Helper()
	ix := engine.New()
	for i, v := range vecs {
		if _, err := ix.Add(engine.Document{Key: string(rune('a' + i)), Vector: v}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	return ix
}

func candidates(t *testing.T, ix *engine.Index, q []float32, k int) []engine.Candidate {
	t.Helper()
	cands, err := New(ix).Candidates(t.Context(), engine.Query{Vector: q}, k)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	return cands
}

func TestCosineExactValues(t *testing.T) {
	ix := index(t,
		[]float32{1, 0},  // identical to the query
		[]float32{0, 1},  // orthogonal
		[]float32{-1, 0}, // opposite
		[]float32{5, 0},  // same direction, 5x magnitude
	)
	cands := candidates(t, ix, []float32{1, 0}, 10)
	got := make(map[engine.DocID]float64, len(cands))
	for _, c := range cands {
		got[c.Doc] = c.Score
	}

	for _, tc := range []struct {
		doc  engine.DocID
		want float64
		why  string
	}{
		{0, 1, "identical vectors"},
		{1, 0, "orthogonal vectors"},
		{2, -1, "opposite vectors"},
		{3, 1, "cosine ignores magnitude"},
	} {
		if math.Abs(got[tc.doc]-tc.want) > eps {
			t.Errorf("doc %d scored %v, want %v (%s)", tc.doc, got[tc.doc], tc.want, tc.why)
		}
	}
	if cands[len(cands)-1].Doc != 2 {
		t.Errorf("the opposite vector is not ranked last: %+v", cands)
	}
}

func TestDimensionMismatchIsAnError(t *testing.T) {
	// Mixed embedding widths mean mixed models. Ranking the subset that happens
	// to line up would hide a caller bug.
	ix := index(t, []float32{1, 0, 0})
	_, err := New(ix).Candidates(t.Context(), engine.Query{Vector: []float32{1, 0}}, 10)
	if !errors.Is(err, ErrDimMismatch) {
		t.Fatalf("err = %v, want ErrDimMismatch", err)
	}
}

func TestZeroVectorsProduceNoNaN(t *testing.T) {
	ix := index(t, []float32{0, 0}, []float32{1, 1})
	for _, c := range candidates(t, ix, []float32{1, 1}, 10) {
		if math.IsNaN(c.Score) || math.IsInf(c.Score, 0) {
			t.Fatalf("doc %d scored %v — a zero norm was divided by", c.Doc, c.Score)
		}
		if c.Doc == 0 {
			t.Fatal("zero-norm document was scored instead of skipped")
		}
	}
	// A zero query vector has no direction, so the scorer has no opinion.
	if got := candidates(t, ix, []float32{0, 0}, 10); len(got) != 0 {
		t.Fatalf("zero query vector returned %+v, want none", got)
	}
}

func TestDocumentsWithoutVectorsAreSkipped(t *testing.T) {
	ix := engine.New()
	if _, err := ix.Add(engine.Document{Key: "textonly", Text: "no vector here"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := ix.Add(engine.Document{Key: "vec", Vector: []float32{1, 0}}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	cands := candidates(t, ix, []float32{1, 0}, 10)
	if len(cands) != 1 || cands[0].Doc != 1 {
		t.Fatalf("Candidates = %+v, want only doc 1", cands)
	}
}

func TestNoQueryVectorMeansNoOpinion(t *testing.T) {
	// A text-only query is not this scorer's business: no results, no error, so
	// Search can run every scorer on every query.
	ix := index(t, []float32{1, 0})
	got, err := New(ix).Candidates(t.Context(), engine.Query{Text: "go"}, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Candidates = %+v, want none", got)
	}
}

func TestRespectsK(t *testing.T) {
	ix := index(t, []float32{1, 0}, []float32{0.9, 0.1}, []float32{0.5, 0.5})
	if got := candidates(t, ix, []float32{1, 0}, 2); len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}
	for _, k := range []int{0, -1} {
		if got := candidates(t, ix, []float32{1, 0}, k); len(got) != 0 {
			t.Fatalf("k=%d returned %+v, want none", k, got)
		}
	}
}

func TestEmptyIndex(t *testing.T) {
	if got := candidates(t, engine.New(), []float32{1, 0}, 10); len(got) != 0 {
		t.Fatalf("Candidates = %+v, want none", got)
	}
}

func TestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ix := index(t, []float32{1, 0})
	if _, err := New(ix).Candidates(ctx, engine.Query{Vector: []float32{1, 0}}, 10); err == nil {
		t.Fatal("Candidates on a cancelled context returned no error")
	}
}

func TestNonFiniteQueryVectorIsAnError(t *testing.T) {
	// engine.Add rejects non-finite document vectors, so the query is the only
	// way one can reach scoring. Returning an empty result would hide a caller
	// bug; returning NaN scores would let TopK order them arbitrarily.
	ix := index(t, []float32{1, 0})
	for _, tc := range []struct {
		name string
		q    []float32
	}{
		{"NaN", []float32{float32(math.NaN()), 0}},
		{"positive infinity", []float32{float32(math.Inf(1)), 0}},
		{"negative infinity", []float32{0, float32(math.Inf(-1))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(ix).Candidates(t.Context(), engine.Query{Vector: tc.q}, 10)
			if !errors.Is(err, engine.ErrNonFiniteVector) {
				t.Fatalf("err = %v, want engine.ErrNonFiniteVector", err)
			}
		})
	}
}

// wide is a query vector long enough to cross the 1024-component poll interval
// more than once. All ones, so its norm is finite and non-zero.
func wide(n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = 1
	}
	return v
}

func TestCancellationIsObservedOnAnEmptyCorpus(t *testing.T) {
	// An empty corpus never enters the document loop, so the per-document check
	// never runs even once: the old code scanned the whole caller-supplied query
	// vector to compute its norm and then returned success on an
	// already-cancelled context. This pins the observable contract only — the
	// poll in dot and the check before TopK each catch it on their own, so
	// either alone keeps this passing. The two tests below pin them separately.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := New(engine.New()).Candidates(ctx, engine.Query{Vector: wide(3000)}, 10)
	if err == nil {
		t.Fatal("Candidates on an empty corpus returned no error after cancellation")
	}
}

// cancelAfter reports itself cancelled only once Err has been called n times,
// which is what makes "cancellation is observed at all" distinguishable from
// "cancellation is observed in time". Duplicated from the text and graph
// scorers rather than shared: one test helper is not worth a package every
// scorer would then have to import.
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

func TestCancellationIsObservedInsideTheVectorScan(t *testing.T) {
	// One document, both vectors 3000 wide. The free calls go to the three polls
	// of the query norm and the single per-document check, so cancellation lands
	// on the first poll of the document norm — inside a scan, which is where the
	// old code had no check between one document and the next.
	//
	// The old code called Err exactly once here, at the loop head, and then
	// returned a successful result: with a single document that check is the
	// only one there is.
	ix := index(t, wide(3000))
	ctx := &cancelAfter{Context: t.Context(), n: 4}
	got, err := New(ix).Candidates(ctx, engine.Query{Vector: wide(3000)}, 10)
	if err == nil {
		t.Fatalf("Candidates returned %+v and no error after mid-scan cancellation", got)
	}
}

func TestCancellationArrivingAfterTheLastDocumentIsObserved(t *testing.T) {
	// Vectors narrow enough to poll once each, so the call count is countable:
	// one for the query norm, one for the per-document check, one each for the
	// document norm and the dot product. Cancellation therefore lands on the
	// fifth call, the check guarding TopK — the window between the last document
	// and the sort, which no earlier check covers.
	ix := index(t, []float32{1, 0})
	ctx := &cancelAfter{Context: t.Context(), n: 4}
	got, err := New(ix).Candidates(ctx, engine.Query{Vector: []float32{1, 0}}, 10)
	if err == nil {
		t.Fatalf("Candidates returned %+v and no error after cancellation before the sort", got)
	}
}

var _ engine.Scorer = (*Scorer)(nil)
