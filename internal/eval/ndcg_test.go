// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// Both implementations are float64 doing the same divisions in the same order,
// so agreement should be near-exact; the tolerance is for the last bits of
// log2, which Go and C compute with different libm code.
const refTolerance = 1e-12

type ndcgReference struct {
	Measure     string `json:"measure"`
	K           int    `json:"k"`
	GeneratedBy string `json:"generated_by"`
	Cases       []struct {
		Name  string         `json:"name"`
		Why   string         `json:"why"`
		Qrels map[string]int `json:"qrels"`
		Run   []string       `json:"run"`
		// Pointer, not float64: the generator writes null for a query
		// trec_eval declined to evaluate, and that must fail loudly rather
		// than read as 0.0 and agree with us by accident.
		NDCG *float64 `json:"ndcg"`
	} `json:"cases"`
}

func loadReference(t *testing.T) ndcgReference {
	t.Helper()
	path := filepath.Join("testdata", "ndcg_reference.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var ref ndcgReference
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(ref.Cases) == 0 {
		t.Fatalf("%s has no cases", path)
	}
	return ref
}

// TestNDCGMatchesPytrecEval is the gate the milestone 4 plan puts ahead of every
// number: if our nDCG disagrees with the implementation BEIR reports, no arm
// comparison means anything, because the graph delta would be measured against
// a metric of our own invention.
func TestNDCGMatchesPytrecEval(t *testing.T) {
	ref := loadReference(t)
	if ref.Measure != "ndcg_cut_10" || ref.K != 10 {
		t.Fatalf("reference is %s at k=%d, want ndcg_cut_10 at k=10", ref.Measure, ref.K)
	}

	for _, c := range ref.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.NDCG == nil {
				t.Fatalf("trec_eval returned no value for this case (%s). "+
					"Decide what the harness should do and encode it here "+
					"rather than letting the case pass silently", c.Why)
			}
			got := NDCG(c.Run, c.Qrels, ref.K)
			if math.Abs(got-*c.NDCG) > refTolerance {
				t.Errorf("NDCG = %.17g, %s = %.17g (diff %.3g)\nwhy this case exists: %s",
					got, ref.GeneratedBy, *c.NDCG, got-*c.NDCG, c.Why)
			}
		})
	}
}

// TestNDCGArithmeticIsIndependentOfTheGoldenFile guards the failure mode the
// golden file cannot: someone edits the fixtures, regenerates the reference,
// and both sides move together while the metric is wrong. These three
// expectations were derived by hand and the arithmetic is written out, so they
// break if the meaning of the metric changes even when the golden agrees.
func TestNDCGArithmeticIsIndependentOfTheGoldenFile(t *testing.T) {
	const (
		log2of3  = 1.5849625007211562
		log2of4  = 2.0
		log2of11 = 3.4594316186372973
	)

	tests := []struct {
		name   string
		ranked []string
		qrels  map[string]int
		want   float64
		arith  string
	}{
		{
			name:   "linear gain, not exponential",
			ranked: []string{"b", "a"},
			qrels:  map[string]int{"a": 2, "b": 1},
			// DCG is 1/log2(2) plus 2/log2(3); IDCG is 2/log2(2) plus 1/log2(3).
			want: (1 + 2/log2of3) / (2 + 1/log2of3),
			arith: "if this fails at 0.7967 the gain was changed to 2^rel-1, " +
				"which disagrees with trec_eval and therefore with BEIR",
		},
		{
			name:   "IDCG counts relevant documents never retrieved",
			ranked: []string{"a"},
			qrels:  map[string]int{"a": 1, "b": 1, "c": 1},
			// DCG is 1; IDCG is 1 plus 1/log2(3) plus 1/log2(4).
			want:  1 / (1 + 1/log2of3 + 1/log2of4),
			arith: "a short run is penalised for the pool it did not reach",
		},
		{
			name:   "rank 10 is inside the cut",
			ranked: []string{"x1", "x2", "x3", "x4", "x5", "x6", "x7", "x8", "x9", "a"},
			qrels:  map[string]int{"a": 1},
			want:   1 / log2of11,
			arith:  "the discount at rank 10 is log2(11), not log2(10)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NDCG(tc.ranked, tc.qrels, 10)
			if math.Abs(got-tc.want) > refTolerance {
				t.Errorf("NDCG = %.17g, want %.17g\n%s", got, tc.want, tc.arith)
			}
		})
	}
}

func TestNDCGEdgeCases(t *testing.T) {
	tests := []struct {
		name   string
		ranked []string
		qrels  map[string]int
		k      int
		want   float64
	}{
		{
			name:   "k of zero measures nothing",
			ranked: []string{"a"},
			qrels:  map[string]int{"a": 2},
			k:      0,
			want:   0,
		},
		{
			name:   "negative k is the same as zero, not a panic",
			ranked: []string{"a"},
			qrels:  map[string]int{"a": 2},
			k:      -3,
			want:   0,
		},
		{
			name:   "a scorer with no opinion returns 0, not NaN",
			ranked: nil,
			qrels:  map[string]int{"a": 2},
			k:      10,
			want:   0,
		},
		{
			name:   "no qrels at all is 0, not a division by zero",
			ranked: []string{"a", "b"},
			qrels:  nil,
			k:      10,
			want:   0,
		},
		{
			name:   "grades at or below zero never enter IDCG",
			ranked: []string{"a"},
			qrels:  map[string]int{"a": 1, "z": 0, "n": -1},
			k:      10,
			want:   1, // IDCG is a alone, so retrieving it first is ideal.
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NDCG(tc.ranked, tc.qrels, tc.k)
			if math.IsNaN(got) {
				t.Fatalf("NDCG = NaN, want %v", tc.want)
			}
			if math.Abs(got-tc.want) > refTolerance {
				t.Errorf("NDCG = %v, want %v", got, tc.want)
			}
		})
	}
}
