package eval

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/scorer/graph"
	"github.com/skyoo2003/weft/pkg/scorer/recency"
	"github.com/skyoo2003/weft/pkg/scorer/text"
	"github.com/skyoo2003/weft/pkg/scorer/vector"
)

// testIndex builds a corpus small enough to reason about by hand but wide enough
// that every scorer has an opinion: text terms overlap, vectors are axis-aligned
// so cosine order is obvious, links form a chain so hop distance is readable,
// and times are strictly ordered.
func testIndex(t *testing.T) *engine.Index {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	docs := []engine.Document{
		{Key: "d0", Text: "alpha beta", Vector: []float32{1, 0}, Links: []string{"d1"}, Time: base},
		{Key: "d1", Text: "alpha gamma", Vector: []float32{0, 1}, Links: []string{"d2"}, Time: base.Add(time.Hour)},
		{Key: "d2", Text: "beta gamma delta", Vector: []float32{1, 1}, Links: []string{"d3"}, Time: base.Add(2 * time.Hour)},
		{Key: "d3", Text: "delta epsilon", Vector: []float32{0, 1}, Links: nil, Time: base.Add(3 * time.Hour)},
		{Key: "d4", Text: "alpha delta", Vector: []float32{1, 0}, Links: []string{"d0"}, Time: base.Add(4 * time.Hour)},
	}
	ix := engine.New()
	for _, d := range docs {
		if _, err := ix.Add(d); err != nil {
			t.Fatalf("add %q: %v", d.Key, err)
		}
	}
	return ix
}

// TestEvaluateIsBlindToScorerCountAndKind is the milestone 1 claim restated for
// the harness. One call shape runs one, two, three and four scorers, in
// different orders, with no arm-specific code anywhere.
func TestEvaluateIsBlindToScorerCountAndKind(t *testing.T) {
	ix := testIndex(t)
	ts := text.New(ix)
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	arms := []Arm{
		{Name: "text", Scorers: []engine.Scorer{ts}},
		{Name: "text+vector", Scorers: []engine.Scorer{ts, vector.New(ix)}},
		{Name: "text+vector+graph", Scorers: []engine.Scorer{ts, vector.New(ix), graph.New(ix, ts)}},
		{Name: "text+vector+graph+recency", Scorers: []engine.Scorer{
			ts, vector.New(ix), graph.New(ix, ts), recency.NewAt(ix, at),
		}},
		// Reordered: fusion is documented as order-independent, so the harness
		// must not acquire an order dependency of its own.
		{Name: "reordered", Scorers: []engine.Scorer{
			recency.NewAt(ix, at), graph.New(ix, ts), vector.New(ix), ts,
		}},
		// The graph variant that keeps seeds. The plan's third arm exists to
		// quantify double counting; here it only has to run the same code.
		{Name: "including-seeds", Scorers: []engine.Scorer{ts, graph.NewIncludingSeeds(ix, ts)}},
	}

	qs := []Query{
		{ID: "q1", Query: engine.Query{Text: "alpha delta", Vector: []float32{1, 0}}, Qrels: map[string]int{"d4": 2, "d0": 1}},
		{ID: "q2", Query: engine.Query{Text: "gamma", Vector: []float32{0, 1}}, Qrels: map[string]int{"d1": 1, "d2": 2}},
	}

	for _, a := range arms {
		t.Run(a.Name, func(t *testing.T) {
			a.Fuse = fusion.Fuse
			run, err := Evaluate(context.Background(), ix, qs, a, 3)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if run.Arm != a.Name || run.K != 3 {
				t.Errorf("Run{Arm:%q, K:%d}, want {%q, 3}", run.Arm, run.K, a.Name)
			}
			if len(run.PerQuery) != len(qs) {
				t.Errorf("PerQuery has %d entries, want %d", len(run.PerQuery), len(qs))
			}
			if run.NDCG < 0 || run.NDCG > 1 || math.IsNaN(run.NDCG) {
				t.Errorf("NDCG = %v, want a number in [0,1]", run.NDCG)
			}
		})
	}
}

// TestOverfetchIsTruncationNotADifferentRanking checks the property Arm.Overfetch
// is built on, and the reason engine.Search did not need a new parameter: RRF
// scores a document from its ranks alone, so k reaches only TopK. If that ever
// stops being true, over-fetch silently starts measuring a different fusion
// rather than a deeper one, and every sweep number becomes uninterpretable.
func TestOverfetchIsTruncationNotADifferentRanking(t *testing.T) {
	streams := [][]engine.Candidate{
		{{Doc: 7, Score: 9}, {Doc: 3, Score: 8}, {Doc: 1, Score: 7}, {Doc: 9, Score: 6}, {Doc: 4, Score: 5}},
		{{Doc: 3, Score: 0.9}, {Doc: 9, Score: 0.8}, {Doc: 2, Score: 0.7}, {Doc: 7, Score: 0.6}},
		{{Doc: 1, Score: 0.5}, {Doc: 4, Score: 0.4}},
	}

	for k := 1; k <= 5; k++ {
		shallow := fusion.Fuse(streams, k)
		for _, m := range []int{2, 3, 10} {
			deep := fusion.Fuse(streams, k*m)
			if len(deep) > k {
				deep = deep[:k]
			}
			if len(shallow) != len(deep) {
				t.Fatalf("k=%d m=%d: shallow has %d, truncated deep has %d", k, m, len(shallow), len(deep))
			}
			for i := range shallow {
				if shallow[i] != deep[i] {
					t.Errorf("k=%d m=%d rank %d: shallow %+v, truncated deep %+v",
						k, m, i+1, shallow[i], deep[i])
				}
			}
		}
	}
}

// fixedScorer returns candidates it was handed, ignoring the query. It exists to
// drive the error paths and the exact rankings a real scorer cannot be made to
// produce on demand.
type fixedScorer struct {
	name  string
	cands []engine.Candidate
}

func (s fixedScorer) Name() string { return s.name }

func (s fixedScorer) Candidates(_ context.Context, _ engine.Query, k int) ([]engine.Candidate, error) {
	if k <= 0 || len(s.cands) == 0 {
		return nil, nil
	}
	// Copied before TopK, which sorts in place: the fixture is reused across
	// queries and must not be reordered by the first one.
	return engine.TopK(append([]engine.Candidate(nil), s.cands...), k), nil
}

func TestEvaluateRejectsMisconfiguration(t *testing.T) {
	ix := testIndex(t)
	ts := text.New(ix)
	okQueries := []Query{{ID: "q1", Query: engine.Query{Text: "alpha"}, Qrels: map[string]int{"d0": 1}}}

	tests := []struct {
		name string
		arm  Arm
		qs   []Query
		k    int
		want error
	}{
		{
			name: "no scorers is not an arm that found nothing",
			arm:  Arm{Name: "empty", Fuse: fusion.Fuse},
			qs:   okQueries,
			k:    3,
			want: ErrNoScorers,
		},
		{
			name: "no queries",
			arm:  Arm{Name: "a", Scorers: []engine.Scorer{ts}, Fuse: fusion.Fuse},
			qs:   nil,
			k:    3,
			want: ErrNoQueries,
		},
		{
			name: "empty query id would collide in PerQuery",
			arm:  Arm{Name: "a", Scorers: []engine.Scorer{ts}, Fuse: fusion.Fuse},
			qs:   []Query{{ID: "", Query: engine.Query{Text: "alpha"}}},
			k:    3,
			want: ErrQueryID,
		},
		{
			name: "duplicate query id would undercount the mean",
			arm:  Arm{Name: "a", Scorers: []engine.Scorer{ts}, Fuse: fusion.Fuse},
			qs: []Query{
				{ID: "q1", Query: engine.Query{Text: "alpha"}},
				{ID: "q1", Query: engine.Query{Text: "beta"}},
			},
			k:    3,
			want: ErrDuplicateQ,
		},
		{
			name: "negative overfetch",
			arm:  Arm{Name: "a", Scorers: []engine.Scorer{ts}, Fuse: fusion.Fuse, Overfetch: -1},
			qs:   okQueries,
			k:    3,
			want: ErrOverfetchRange,
		},
		{
			name: "overfetch that would overflow int",
			arm:  Arm{Name: "a", Scorers: []engine.Scorer{ts}, Fuse: fusion.Fuse, Overfetch: math.MaxInt},
			qs:   okQueries,
			k:    3,
			want: ErrOverfetchRange,
		},
		{
			name: "a DocID from another index is caught, not scored",
			arm: Arm{Name: "foreign", Fuse: fusion.Fuse, Scorers: []engine.Scorer{
				fixedScorer{name: "foreign", cands: []engine.Candidate{{Doc: 4242, Score: 1}}},
			}},
			qs:   okQueries,
			k:    3,
			want: ErrForeignDocID,
		},
		{
			name: "nil fuser is engine's error, not a second copy of the check",
			arm:  Arm{Name: "a", Scorers: []engine.Scorer{ts}},
			qs:   okQueries,
			k:    3,
			want: engine.ErrNoFuser,
		},
		{
			name: "typed nil scorer is engine's error too",
			arm: Arm{Name: "a", Fuse: fusion.Fuse, Scorers: []engine.Scorer{
				ts, (*vector.Scorer)(nil),
			}},
			qs:   okQueries,
			k:    3,
			want: engine.ErrNilScorer,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Evaluate(context.Background(), ix, tc.qs, tc.arm, tc.k)
			if !errors.Is(err, tc.want) {
				t.Errorf("Evaluate error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEvaluateRejectsNonPositiveK(t *testing.T) {
	ix := testIndex(t)
	arm := Arm{Name: "a", Scorers: []engine.Scorer{text.New(ix)}, Fuse: fusion.Fuse}
	qs := []Query{{ID: "q1", Query: engine.Query{Text: "alpha"}, Qrels: map[string]int{"d0": 1}}}

	for _, k := range []int{0, -1} {
		if _, err := Evaluate(context.Background(), ix, qs, arm, k); err == nil {
			t.Errorf("Evaluate with k=%d returned no error", k)
		}
	}
}

// TestEvaluateMeanIsMacroAverage pins the averaging rule with an arm whose
// ranking is fixed, so both per-query values are hand-computable. A micro
// average weighted by judgment depth would give a different number, and
// TREC-COVID's per-query depth varies enough for the difference to matter.
func TestEvaluateMeanIsMacroAverage(t *testing.T) {
	ix := testIndex(t)
	// One stream, so the fused order is the scorer's order: d0, d1, d2.
	arm := Arm{
		Name: "fixed",
		Fuse: fusion.Fuse,
		Scorers: []engine.Scorer{fixedScorer{name: "fixed", cands: []engine.Candidate{
			{Doc: 0, Score: 3}, {Doc: 1, Score: 2}, {Doc: 2, Score: 1},
		}}},
	}
	qs := []Query{
		// Ideal order for d0=1: nDCG 1.0.
		{ID: "perfect", Qrels: map[string]int{"d0": 1}},
		// d2 lands at rank 3, so DCG = 1/log2(4) = 0.5 against IDCG 1.
		{ID: "third", Qrels: map[string]int{"d2": 1}},
	}

	run, err := Evaluate(context.Background(), ix, qs, arm, 3)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got, want := run.PerQuery["perfect"], 1.0; math.Abs(got-want) > refTolerance {
		t.Errorf("PerQuery[perfect] = %v, want %v", got, want)
	}
	if got, want := run.PerQuery["third"], 0.5; math.Abs(got-want) > refTolerance {
		t.Errorf("PerQuery[third] = %v, want %v", got, want)
	}
	if got, want := run.NDCG, 0.75; math.Abs(got-want) > refTolerance {
		t.Errorf("NDCG = %v, want %v (macro average of 1.0 and 0.5)", got, want)
	}
}

// TestEvaluateHonoursCancellation makes sure a cancelled run fails rather than
// reporting the zeros of an abandoned measurement.
func TestEvaluateHonoursCancellation(t *testing.T) {
	ix := testIndex(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	arm := Arm{Name: "a", Scorers: []engine.Scorer{text.New(ix)}, Fuse: fusion.Fuse}
	qs := []Query{{ID: "q1", Query: engine.Query{Text: "alpha"}, Qrels: map[string]int{"d0": 1}}}

	if _, err := Evaluate(ctx, ix, qs, arm, 3); !errors.Is(err, context.Canceled) {
		t.Errorf("Evaluate error = %v, want context.Canceled", err)
	}
}
