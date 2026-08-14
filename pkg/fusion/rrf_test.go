// SPDX-License-Identifier: Apache-2.0

package fusion

import (
	"math"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

func docs(cands []engine.Candidate) []engine.DocID {
	ids := make([]engine.DocID, len(cands))
	for i, c := range cands {
		ids[i] = c.Doc
	}
	return ids
}

func equal(got []engine.DocID, want ...engine.DocID) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// weightStreams is the fixture the FuseWeighted tests share: two streams that
// disagree completely, so which one is trusted decides the whole ranking.
func weightStreams() [][]engine.Candidate {
	return [][]engine.Candidate{
		{{Doc: 1, Score: 9}, {Doc: 2, Score: 8}, {Doc: 3, Score: 7}},
		{{Doc: 4, Score: 9}, {Doc: 5, Score: 8}, {Doc: 6, Score: 7}},
	}
}

// TestFuseWeightedWithoutWeightsIsFuse is the compatibility guarantee the refactor
// rests on. Fuse now delegates to the same accumulation loop, and if the two ever
// diverge, every ranking pinned elsewhere — milestone 1's assertions, milestone 2's
// restore equivalence — silently starts measuring something else.
//
// Bit equality, not approximate: multiplying by 1.0 is exact in IEEE-754, so there
// is no tolerance to justify.
func TestFuseWeightedWithoutWeightsIsFuse(t *testing.T) {
	streams := [][]engine.Candidate{
		{{Doc: 1, Score: 9}, {Doc: 3, Score: 8}, {Doc: 7, Score: 7}, {Doc: 2, Score: 6}},
		{{Doc: 3, Score: 0.9}, {Doc: 9, Score: 0.8}, {Doc: 1, Score: 0.7}},
		{{Doc: 7, Score: 5}, {Doc: 4, Score: 4}},
	}

	tests := []struct {
		name  string
		fused []engine.Candidate
	}{
		{"no weights at all", FuseWeighted()(streams, 6)},
		{"explicit ones", FuseWeighted(1, 1, 1)(streams, 6)},
		{"short slice, rest default to one", FuseWeighted(1)(streams, 6)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := Fuse(streams, 6)
			if len(tc.fused) != len(want) {
				t.Fatalf("got %d candidates, Fuse gave %d", len(tc.fused), len(want))
			}
			for i := range want {
				if tc.fused[i] != want[i] {
					t.Errorf("rank %d: got %+v, Fuse gave %+v", i+1, tc.fused[i], want[i])
				}
			}
		})
	}
}

// TestFuseWeightedDiscountsAStream is the behaviour milestone 4 bought this for: a
// stream that is mostly noise can be given a smaller vote instead of an equal one.
func TestFuseWeightedDiscountsAStream(t *testing.T) {
	streams := weightStreams()

	// Equal votes: rank 1 of each stream ties, and TopK breaks on DocID.
	if got := docs(Fuse(streams, 2)); !equal(got, 1, 4) {
		t.Fatalf("unweighted = %v, want [1 4] (tied at rank 1, DocID decides)", got)
	}

	// Second stream discounted: the first sweeps the top before the second's best
	// appears.
	if got := docs(FuseWeighted(1, 0.1)(streams, 4)); !equal(got, 1, 2, 3, 4) {
		t.Errorf("weighted 1:0.1 = %v, want [1 2 3 4]", got)
	}

	// And symmetrically, so the effect is the weight and not the stream order.
	if got := docs(FuseWeighted(0.1, 1)(streams, 4)); !equal(got, 4, 5, 6, 1) {
		t.Errorf("weighted 0.1:1 = %v, want [4 5 6 1]", got)
	}
}

// TestFuseWeightedZeroSilencesAStream: weight 0 must remove a stream's influence
// entirely rather than leave it breaking ties. "Turn this signal off" is the first
// thing a caller will try, and a residue would be invisible.
func TestFuseWeightedZeroSilencesAStream(t *testing.T) {
	streams := weightStreams()
	fused := FuseWeighted(1, 0)(streams, 6)

	for _, c := range fused {
		if c.Doc >= 4 {
			t.Errorf("doc %d came from the silenced stream at score %v", c.Doc, c.Score)
		}
	}
	// The surviving stream must rank exactly as it would alone.
	if got := docs(fused); !equal(got, 1, 2, 3) {
		t.Errorf("got %v, want [1 2 3]", got)
	}
}

// TestFuseWeightedRejectsOutOfContractWeights pins the one defined behaviour for a
// weight that breaks the precondition. A NaN weight is the dangerous one: it would
// otherwise make every score it touches NaN, and TopK sorts those last and ties them
// on DocID, so the fused order silently becomes corpus insertion order.
func TestFuseWeightedRejectsOutOfContractWeights(t *testing.T) {
	streams := weightStreams()
	off := docs(FuseWeighted(1, 0)(streams, 6))

	for _, w := range []float64{math.NaN(), -1, math.Inf(-1)} {
		if got := docs(FuseWeighted(1, w)(streams, 6)); !equal(got, off...) {
			t.Errorf("weight %v = %v, want %v (same as weight 0)", w, got, off)
		}
	}
	// +Inf is finite-contract-breaking too, but it is not folded to 0: it is a
	// weight, just an absurd one, and it silences the other stream instead.
	if got := docs(FuseWeighted(1, math.Inf(1))(streams, 3)); !equal(got, 4, 5, 6) {
		t.Errorf("weight +Inf = %v, want [4 5 6]", got)
	}
}

// TestFuseWeightedDoesNotAliasCallerSlice: a Fuser outlives the call that built it,
// and callers reuse scratch slices.
func TestFuseWeightedDoesNotAliasCallerSlice(t *testing.T) {
	w := []float64{1, 0.1}
	fuser := FuseWeighted(w...)
	before := docs(fuser(weightStreams(), 4))

	w[1] = 1 // Would re-enable the discounted stream if the slice were shared.

	if after := docs(fuser(weightStreams(), 4)); !equal(after, before...) {
		t.Errorf("after mutating the caller's slice: %v, before: %v", after, before)
	}
}

func TestSingleStreamPreservesOrder(t *testing.T) {
	stream := []engine.Candidate{{Doc: 5, Score: 9}, {Doc: 3, Score: 4}, {Doc: 8, Score: 1}}
	got := docs(Fuse([][]engine.Candidate{stream}, 10))
	if !equal(got, 5, 3, 8) {
		t.Fatalf("Fuse = %v, want [5 3 8]", got)
	}
}

func TestScoresAreNeverRead(t *testing.T) {
	// Doc 1 is ranked first with a negligible score, doc 2 second with an
	// enormous one. If Fuse looked at Score at all, doc 2 would win.
	stream := []engine.Candidate{{Doc: 1, Score: 0.0001}, {Doc: 2, Score: 999999}}
	got := docs(Fuse([][]engine.Candidate{stream}, 10))
	if !equal(got, 1, 2) {
		t.Fatalf("Fuse = %v, want [1 2] — Score leaked into the ranking", got)
	}
}

func TestAgreementBeatsSingleStreamConfidence(t *testing.T) {
	// Doc 2 is second in both streams; doc 1 is first in one and absent from the
	// other. Two mid-rank appearances must outweigh one top-rank appearance.
	a := []engine.Candidate{{Doc: 1, Score: 100}, {Doc: 2, Score: 1}}
	b := []engine.Candidate{{Doc: 3, Score: 100}, {Doc: 2, Score: 1}}
	got := docs(Fuse([][]engine.Candidate{a, b}, 10))
	if len(got) != 3 || got[0] != 2 {
		t.Fatalf("Fuse = %v, want doc 2 first", got)
	}
}

func TestIncompatibleScoreScalesDoNotDistortRanking(t *testing.T) {
	// The real reason RRF was chosen: unbounded BM25-like scores in one stream,
	// [-1,1] cosine-like scores in another. A weighted sum without normalization
	// would let the first stream dictate the whole ranking.
	bm25 := []engine.Candidate{{Doc: 1, Score: 42.7}, {Doc: 2, Score: 31.9}}
	cosine := []engine.Candidate{{Doc: 2, Score: 0.81}, {Doc: 3, Score: -0.4}}
	got := docs(Fuse([][]engine.Candidate{bm25, cosine}, 10))
	// Doc 2 appears in both (ranks 2 and 1), so it must lead despite its scores
	// being tiny next to doc 1's.
	if len(got) != 3 || got[0] != 2 {
		t.Fatalf("Fuse = %v, want doc 2 first", got)
	}
}

func TestEmptyInputs(t *testing.T) {
	tests := []struct {
		name    string
		streams [][]engine.Candidate
		k       int
	}{
		{"nil streams", nil, 10},
		{"zero streams", [][]engine.Candidate{}, 10},
		{"one empty stream", [][]engine.Candidate{{}}, 10},
		{"all streams empty", [][]engine.Candidate{{}, {}, {}}, 10},
		{"k zero", [][]engine.Candidate{{{Doc: 1}}}, 0},
		{"k negative", [][]engine.Candidate{{{Doc: 1}}}, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The point of the table is that none of these panic.
			if got := Fuse(tc.streams, tc.k); len(got) != 0 {
				t.Fatalf("Fuse = %+v, want empty", got)
			}
		})
	}
}

func TestRespectsK(t *testing.T) {
	stream := []engine.Candidate{{Doc: 1}, {Doc: 2}, {Doc: 3}, {Doc: 4}}
	got := Fuse([][]engine.Candidate{stream}, 2)
	if len(got) != 2 {
		t.Fatalf("Fuse returned %d candidates, want 2", len(got))
	}
}

// TestScorerCountIsIrrelevant is the architecture assertion at the fusion layer:
// the same call shape works for any number of streams, so adding a scorer never
// changes this function's signature or body.
func TestScorerCountIsIrrelevant(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 7, 20} {
		streams := make([][]engine.Candidate, n)
		for i := range streams {
			streams[i] = []engine.Candidate{
				{Doc: engine.DocID(i), Score: float64(i)},
				{Doc: 999, Score: 0},
			}
		}
		got := Fuse(streams, 5)
		if len(got) == 0 {
			t.Fatalf("Fuse with %d streams returned nothing", n)
		}
		// Doc 999 sits at rank 2 in every stream; with two or more streams its
		// accumulated score must put it first.
		if n >= 2 && got[0].Doc != 999 {
			t.Fatalf("with %d streams, Fuse = %v, want doc 999 first", n, docs(got))
		}
	}
}

func TestEqualRankMultisetsTieRegardlessOfStreamOrder(t *testing.T) {
	// Doc 1 holds ranks 1, 2, 7 and doc 2 holds 7, 1, 2 — the same multiset, so
	// RRF owes them the same total and TopK owes the tie to the lower DocID.
	// Ranks 1, 2 and 7 are the smallest triple whose reciprocals sum to two
	// different float64 values depending on the order they are added in, so
	// accumulating stream by stream let a permutation of the scorer slice pick
	// the winner. Fillers are distinct per stream so they cannot reach the top.
	f := func(n engine.DocID) engine.Candidate { return engine.Candidate{Doc: n} }
	streams := [][]engine.Candidate{
		{f(1), f(10), f(11), f(12), f(13), f(14), f(2)},
		{f(2), f(1), f(20), f(21), f(22), f(23), f(24)},
		{f(30), f(2), f(31), f(32), f(33), f(34), f(1)},
	}
	perms := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, p := range perms {
		got := Fuse([][]engine.Candidate{streams[p[0]], streams[p[1]], streams[p[2]]}, 2)
		if !equal(docs(got), 1, 2) {
			t.Fatalf("stream order %v: Fuse = %v, want [1 2]", p, docs(got))
		}
		if got[0].Score != got[1].Score {
			t.Fatalf("stream order %v: scores %.20g and %.20g differ; equal rank "+
				"multisets must fuse to identical bits", p, got[0].Score, got[1].Score)
		}
	}
}

func TestLargeKDoesNotReserveMemoryForAbsentCandidates(t *testing.T) {
	// k is caller-supplied — cmd/weft takes it straight from a flag — so sizing
	// the accumulator from k rather than from the streams lets one large -k
	// exhaust memory against an eight-document corpus.
	got := Fuse([][]engine.Candidate{{{Doc: 1}, {Doc: 2}}}, 1<<40)
	if !equal(docs(got), 1, 2) {
		t.Fatalf("Fuse = %v, want [1 2]", docs(got))
	}
}

func TestDuplicateDocWithinOneStreamAccumulates(t *testing.T) {
	// Not a case any scorer produces today, but Fuse must not corrupt or panic
	// if a stream ever repeats a document.
	stream := []engine.Candidate{{Doc: 1}, {Doc: 1}, {Doc: 2}}
	got := Fuse([][]engine.Candidate{stream}, 10)
	if len(got) != 2 {
		t.Fatalf("Fuse = %+v, want 2 distinct docs", got)
	}
	if got[0].Doc != 1 {
		t.Fatalf("Fuse = %v, want doc 1 first", docs(got))
	}
}
