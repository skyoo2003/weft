// SPDX-License-Identifier: Apache-2.0

package fusion

import (
	"math"
	"slices"
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
// weight that breaks the precondition. NaN and +Inf are the dangerous pair, and they
// fail the same way from opposite ends: every score the stream touches becomes NaN or
// becomes +Inf, all of them compare equal, and TopK ties them on DocID — so the fused
// order silently becomes corpus insertion order.
func TestFuseWeightedRejectsOutOfContractWeights(t *testing.T) {
	streams := weightStreams()
	off := docs(FuseWeighted(1, 0)(streams, 6))

	for _, w := range []float64{math.NaN(), -1, math.Inf(-1), math.Inf(1)} {
		if got := docs(FuseWeighted(1, w)(streams, 6)); !equal(got, off...) {
			t.Errorf("weight %v = %v, want %v (same as weight 0)", w, got, off)
		}
	}
}

// TestFuseWeightedInfiniteWeightIsNotJustAnExtremeWeight is why +Inf is folded to 0
// rather than honoured as "trust this stream absolutely".
//
// This file previously asserted the opposite — that +Inf merely silenced the other
// stream — and it passed, because weightStreams happens to rank 4, 5, 6 in DocID
// order. A stream whose ranking had collapsed onto the tiebreak was indistinguishable
// from one that kept it. Here the stream ranks descending, so the two disagree: an
// honoured +Inf would return [6 5 4] only by luck and in fact returns them in DocID
// order [4 5 6], because Inf/(60+1) and Inf/(60+3) are the same float. The stream's
// own ranking is not scaled up, it is erased.
func TestFuseWeightedInfiniteWeightIsNotJustAnExtremeWeight(t *testing.T) {
	streams := [][]engine.Candidate{
		{{Doc: 1, Score: 9}},
		{{Doc: 6, Score: 9}, {Doc: 5, Score: 8}, {Doc: 4, Score: 7}},
	}
	if got := docs(FuseWeighted(1, math.Inf(1))(streams, 4)); !equal(got, 1) {
		t.Errorf("weight +Inf = %v, want [1]: the stream carries no usable order at "+
			"that weight, so it is switched off rather than ranked by DocID", got)
	}

	// The claim above about what an honoured +Inf would do, checked rather than
	// asserted — the fold is only the right call if the alternative really is
	// insertion order.
	var collapsed []engine.DocID
	for _, c := range engine.TopK([]engine.Candidate{
		{Doc: 6, Score: math.Inf(1)}, {Doc: 5, Score: math.Inf(1)}, {Doc: 4, Score: math.Inf(1)},
	}, 3) {
		collapsed = append(collapsed, c.Doc)
	}
	if !equal(collapsed, 4, 5, 6) {
		t.Errorf("tied +Inf scores rank %v, want DocID order [4 5 6]", collapsed)
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

// TestFuseWeightedLargeWeightsDoNotOverflow is the regression for weights that are
// each individually in contract and collectively are not.
//
// Every term is sw/(RRFk+rank), so a weight near math.MaxFloat64 makes one term
// about MaxFloat64/61 — finite, with room to spare. Summing them is what runs out:
// around 62 streams the total passes MaxFloat64 and becomes +Inf, every document the
// streams agree on lands there together, and TopK settles them on DocID. Nothing in
// the call is out of contract — finite non-negative weights, any number of streams —
// so no guard inside fuse can catch it; scaleDown removes the headroom problem
// before the accumulation starts.
//
// The fixture puts the winner at the higher DocID on purpose. With doc 1 ranked
// first the collapsed order and the correct order are the same list, and the test
// would pass on a ranking that had been destroyed.
func TestFuseWeightedLargeWeightsDoNotOverflow(t *testing.T) {
	const n = 100
	streams := make([][]engine.Candidate, n)
	weights := make([]float64, n)
	for i := range streams {
		streams[i] = []engine.Candidate{{Doc: 2, Score: 9}, {Doc: 1, Score: 8}}
		weights[i] = math.MaxFloat64
	}
	got := FuseWeighted(weights...)(streams, 2)
	if !equal(docs(got), 2, 1) {
		t.Fatalf("FuseWeighted(MaxFloat64 x %d) = %v, want [2 1]: doc 2 is ranked first "+
			"in every stream", n, docs(got))
	}
	for _, c := range got {
		if math.IsInf(c.Score, 0) {
			t.Fatalf("doc %d scored %v; the fused score overflowed", c.Doc, c.Score)
		}
	}
}

// TestFuseWeightedIsScaleInvariant pins the property scaleDown relies on: only the
// ratios between weights decide the order, so dividing them all by the largest is a
// no-op for ranking. If this fails, scaleDown is not a safe transformation and the
// overflow has to be handled some other way.
func TestFuseWeightedIsScaleInvariant(t *testing.T) {
	streams := weightStreams()
	small := docs(FuseWeighted(1, 0.5)(streams, 6))
	for _, scale := range []float64{2, 10, 1e6, 1e300} {
		got := docs(FuseWeighted(scale, scale/2)(streams, 6))
		if !equal(got, small...) {
			t.Errorf("FuseWeighted(%v, %v) = %v, want %v (same ratio)", scale, scale/2, got, small)
		}
	}
}

// TestFuseWeightedLeavesSmallWeightsAlone is the compatibility guard on scaleDown.
// Every weight set this repository actually uses has a maximum of 1 —
// FuseWeighted(1, 1, w) in the milestone 4 sweep, FuseWeighted(1, 1, 0.1, 1) in the
// example — so scaling must not touch them, or every published number moves for a
// reason unrelated to the measurement.
func TestFuseWeightedLeavesSmallWeightsAlone(t *testing.T) {
	for _, w := range [][]float64{{1, 1}, {1, 0.5}, {1, 1, 0.1}, {0.5, 0.25}} {
		before := slices.Clone(w)
		scaleDown(w)
		if !slices.Equal(w, before) {
			t.Errorf("scaleDown(%v) = %v, want it untouched", before, w)
		}
	}
}

// TestFuseWeightedScalingPreservesImplicitWeights is the regression for the one place
// scaleDown and the short-slice default met and disagreed.
//
// A stream with no weight of its own counts 1.0, and scaleDown used to divide only the
// weights it was handed. FuseWeighted(2) over two streams therefore became [1] plus an
// untouched implicit 1.0 — the 2:1 the caller asked for, silently served as 1:1, which
// is Fuse. The failure is invisible from the outside: no error, no NaN, just equal
// votes from the one function whose purpose is not to give them.
//
// Both directions are checked. It has to equal the same ratio written out in full, and
// it must not equal the unweighted fusion; either assertion alone passes on a fixture
// where the two happen to agree.
func TestFuseWeightedScalingPreservesImplicitWeights(t *testing.T) {
	streams := weightStreams()

	equalVotes := docs(Fuse(streams, 4))
	for _, scale := range []float64{2, 10, 1e6, 1e300} {
		want := docs(FuseWeighted(scale, scale/2)(streams, 4))
		got := docs(FuseWeighted(scale)(streams, 4))
		if !equal(got, want...) {
			t.Errorf("FuseWeighted(%v) = %v, want %v: the second stream's implicit 1.0 "+
				"is half of %v and has to be scaled with it", scale, got, want, scale)
		}
		if equal(got, equalVotes...) {
			t.Errorf("FuseWeighted(%v) = %v, the same as unweighted Fuse: the ratio was "+
				"scaled away", scale, got)
		}
	}
}

// TestFuseWeightedUnderflowingWeightSilencesAStream closes the last route to the
// collapse the NaN and +Inf guards exist to stop.
//
// math.SmallestNonzeroFloat64 is positive, finite and in contract, so it passes every
// guard on the weight — and then sw*w underflows to 0 at every rank, `+=` creates the
// map entry regardless, and the whole stream arrives at score 0. Those documents tie,
// TopK settles them on DocID, and a stream the caller chose to trust only a little
// contributes corpus insertion order instead of its ranking. Weight 0 is the honest
// answer: a vote that rounds to nothing did not earn a place.
func TestFuseWeightedUnderflowingWeightSilencesAStream(t *testing.T) {
	streams := weightStreams()
	off := docs(FuseWeighted(1, 0)(streams, 6))

	// The largest reciprocal is 1/(RRFk+1), so a weight that underflows at rank 1
	// underflows at every rank; both of these do.
	for _, w := range []float64{math.SmallestNonzeroFloat64, 10 * math.SmallestNonzeroFloat64} {
		fused := FuseWeighted(1, w)(streams, 6)
		for _, c := range fused {
			if c.Score == 0 {
				t.Errorf("weight %v: doc %d is in the result at score 0", w, c.Doc)
			}
		}
		if got := docs(fused); !equal(got, off...) {
			t.Errorf("weight %v = %v, want %v (same as weight 0)", w, got, off)
		}
	}

	// And the other side of the boundary, three orders of magnitude up: 1e-320/61 is
	// still a representable subnormal, so the stream keeps a real vote and ranks below
	// the other one rather than being switched off. The guard is on the product being
	// zero, not on the weight looking small.
	if got := docs(FuseWeighted(1, 1e-320)(streams, 6)); !equal(got, 1, 2, 3, 4, 5, 6) {
		t.Errorf("weight 1e-320 = %v, want [1 2 3 4 5 6]: nothing underflows there", got)
	}
}

// TestFuseWeightedSurplusWeightsDoNotScaleTheActiveOnes is the regression for a weight
// documented as ignored that decided the whole result.
//
// scaleDown used to run once in FuseWeighted, over every weight it was handed, because
// that is the only place the slice was known — the stream count is not. So a weight past
// the last stream still set the maximum every active weight was divided by. At an
// ordinary ratio that is invisible, since scaling all the weights scales every score and
// TopK sorts the same list; past 1e308 it is not, because the division underflows and the
// stream is switched off by the rule that exists for weights the caller actually zeroed.
//
// Both halves are asserted. The surplus weight must not change the result, and the result
// it must not change has to be non-empty — otherwise the test passes on two empty
// rankings, which is the bug.
func TestFuseWeightedSurplusWeightsDoNotScaleTheActiveOnes(t *testing.T) {
	one := [][]engine.Candidate{weightStreams()[0]}

	want := docs(FuseWeighted(1e-320)(one, 3))
	if !equal(want, 1, 2, 3) {
		t.Fatalf("FuseWeighted(1e-320) over one stream = %v, want [1 2 3]", want)
	}
	// Every surplus weight is out of reach of the single stream, so each of these is
	// the fusion above. math.MaxFloat64 is the one that used to underflow it away;
	// the others are ordinary and are here because "ignored" has to mean ignored at
	// any magnitude, including the out-of-contract values fuse folds to 0.
	for _, surplus := range []float64{math.MaxFloat64, 2, 1, 0, -1, math.NaN(), math.Inf(1)} {
		if got := docs(FuseWeighted(1e-320, surplus)(one, 3)); !equal(got, want...) {
			t.Errorf("FuseWeighted(1e-320, %v) over one stream = %v, want %v: the second "+
				"weight has no stream and must not scale the first", surplus, got, want)
		}
	}
}

// TestFuseWeightedIsReusableAcrossStreamCounts pins what moving the scaling into the
// returned closure has to not break.
//
// scaleDown divides in place, so the closure now clones before it scales. Without that
// the first call would leave the scaled weights behind and the second would scale them
// again — a Fuser that quietly returns a different ranking the more often it is used,
// which is the failure mode hardest to find from a search result.
func TestFuseWeightedIsReusableAcrossStreamCounts(t *testing.T) {
	streams := weightStreams()
	fuser := FuseWeighted(1e6, 1e3)

	first := docs(fuser(streams, 6))
	for i := range 3 {
		if got := docs(fuser(streams, 6)); !equal(got, first...) {
			t.Fatalf("call %d = %v, want %v: the Fuser rescaled its own weights", i+2, got, first)
		}
	}
	// And a call with fewer streams in between, which is what now decides how much of
	// the slice takes part in the scaling.
	fuser([][]engine.Candidate{streams[0]}, 3)
	if got := docs(fuser(streams, 6)); !equal(got, first...) {
		t.Errorf("after a one-stream call = %v, want %v", got, first)
	}
}
