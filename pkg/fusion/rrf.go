// SPDX-License-Identifier: Apache-2.0

// Package fusion combines any number of candidate streams into one ranking.
//
// This package imports engine for the Candidate type and nothing else. It has
// no idea what a text, vector, graph or recency scorer is, and
//
//	go list -deps ./pkg/fusion
//
// naming no scorer/* package is the mechanical proof of that — the milestone 1
// hypothesis stands or falls on this command's output, not on code review.
package fusion

import (
	"math"
	"slices"

	"github.com/skyoo2003/weft/pkg/engine"
)

// RRFk is the rank constant from Cormack, Clarke & Buettcher (2009). It damps
// the contribution of top ranks so one confident stream cannot dominate the
// fused order on its own.
const RRFk = 60.0

// Fuse merges streams by Reciprocal Rank Fusion: each document scores
// Σ 1/(RRFk + rank) over every stream it appears in, ranks being 1-based.
//
// Reciprocal *rank* is the reason this works for arbitrary scorers. Candidate
// scores arrive on incompatible scales — BM25 is unbounded, cosine is [-1,1],
// graph proximity is (0,1] — so any weighted sum of *scores* would first need
// per-scorer normalization, and knowing how to normalize means knowing which
// scorer you hold. Rank is scale-free, so this function never reads
// Candidate.Score at all.
//
// There is deliberately no scorer count, no scorer identity, and no branch on
// either. A switch on scorer name appearing in this file is the hypothesis
// failing.
// Empty streams, no streams and a non-positive k all fall through to TopK,
// which already returns nil for each — guarding them here would be three copies
// of an invariant this package does not own.
//
// Every stream gets one equal vote. That is a ranking decision rather than a
// neutral default, and milestone 4 measured what it costs when one stream is much
// weaker than the others: 0.1202 nDCG@10, two orders of magnitude more than the
// signal under evaluation was worth (docs/FINDINGS.md milestone 4 section 7).
// FuseWeighted is the escape hatch.
func Fuse(streams [][]engine.Candidate, k int) []engine.Candidate {
	return fuse(streams, k, nil, 1)
}

// FuseWeighted is Fuse with a per-stream multiplier: Σ wᵢ/(RRFk + rank).
//
// Weights are indexed by **position in streams**, and a stream with no
// corresponding weight gets 1.0 — so FuseWeighted() with no arguments is exactly
// Fuse. Weight 0 switches a stream off completely: its documents do not appear at
// all, rather than appearing last at score 0.
//
// Precondition: a weight is finite and non-negative. Anything else — a negative
// weight, or a NaN or +Inf from an unchecked division — is treated as 0, which is
// the one defined behaviour that cannot corrupt a ranking silently. It is not
// rejected, because a Fuser has no error to return. A negative weight would
// otherwise push a document below documents that no stream ranked at all, which is
// exactly the unearned position the weight-0 rule exists to prevent. A NaN weight
// would make every score it touches NaN and a +Inf weight would make every one of
// them +Inf; either way those documents compare equal, TopK ties them on DocID, and
// the fused order quietly becomes corpus insertion order. A weight small enough that
// its vote underflows to zero joins them, for the same reason and at the point of use
// rather than here — see fuse.
//
// Only the ratios between weights matter. Weights larger than 1 are scaled down so
// the largest is 1, which leaves the fused order unchanged and keeps a large weight
// over many streams from overflowing the accumulated score to +Inf — see scaleDown.
// FuseWeighted(2, 1) and FuseWeighted(1, 0.5) are therefore the same Fuser, and a
// caller reading Candidate.Score off the result is reading a number whose scale it
// did not choose. Score has never been comparable across fusions anyway; the ranking
// is the output.
//
// The implicit 1.0 is scaled with them, and has to be. It is a weight like any other,
// so dividing only the explicit ones changes the ratio between a stream that was given
// a weight and a stream that was not: FuseWeighted(2) over two streams means 2:1, and
// scaling the slice alone turns it into 1:1 — which is Fuse, the one fusion a caller
// reaching for this function is asking not to get. It is stored as 1:0.5 instead.
//
// A weight with no corresponding stream is ignored. Positional coupling cuts both
// ways: a caller that drops or reorders a scorer without editing its weights gets a
// different ranking and no complaint, so the two lists belong next to each other at
// the call site.
//
// Position, not scorer identity, is what makes this legal here. The caller already
// fixed the order when it passed its scorers to engine.Search, so expressing
// "trust the third stream less" needs no knowledge of what the third scorer is.
// This package still never learns that, and `go list -deps ./pkg/fusion` still
// names no scorer. Weighting *scores* would have required normalization and
// therefore identity; weighting *votes* does not.
//
// Milestone 4's evidence for adding it: at weight 1.0 a near-noise stream cost
// 0.1202 nDCG@10, and halving that one weight erased all but 0.0019 of it. What
// that milestone did not answer is where weights should come from. Hand-tuning
// per corpus reintroduces the per-deployment tuning burden a scorer-agnostic design
// exists to avoid, and learning them from relevance judgments is a different
// project. Until one of those is settled, a caller with no measurement of its own
// should use Fuse.
func FuseWeighted(weights ...float64) engine.Fuser {
	// Cloned because the caller is free to reuse or mutate the slice it passed,
	// and a Fuser outlives the call that built it.
	w := slices.Clone(weights)
	dflt := scaleDown(w)
	return func(streams [][]engine.Candidate, k int) []engine.Candidate {
		return fuse(streams, k, w, dflt)
	}
}

// scaleDown divides the weights by the largest of them, in place, when that is
// greater than 1.
//
// Only the ratios between weights affect the fused order — every score is a sum of
// wᵢ/(RRFk + rank) terms, so scaling all the weights scales every score by the same
// factor and TopK sorts the same list. What the scaling buys is a bound. Each term
// is at most maxW/(RRFk+1), so len(streams) of them sum to at most
// len(streams)·maxW/61; with maxW near math.MaxFloat64 that overflows to +Inf at
// around 62 streams, and every document the streams agree on lands there together,
// compares equal, and is settled by TopK on DocID. That is the same collapse to
// insertion order the NaN and +Inf weight guards prevent, arriving from weights
// each of which is individually finite, non-negative and entirely in contract.
// After scaling maxW is exactly 1, no term exceeds 1/61, and reaching +Inf would
// take more than 1e310 streams.
//
// Two things it deliberately does not do. It does not scale *up*: weights at or
// below 1 already cannot overflow, and leaving them untouched keeps every existing
// call bit-for-bit what it was — FuseWeighted(1, 1, 0.1) is the shape callers
// actually write, and no published measurement moves. And it does not renormalize
// out-of-contract values: NaN and ±Inf are excluded from the maximum but left in
// the slice, because dividing them changes nothing about what they are and fuse
// still has to fold them to 0.
//
// A weight can underflow to 0 here, and that is honest rather than lossy: it takes
// a ratio past 1e308 to the largest weight, at which point the stream's every
// contribution was already further below the leader than float64 can represent.
//
// The return value is what a stream past the end of w now weighs. Streams without a
// weight are documented to count 1.0, and 1.0 is a weight in the same ratio set as
// the rest — scaling every explicit weight and leaving that one alone would silently
// re-rank the very streams the caller distinguished. Returning it rather than
// appending to w keeps this working for a stream count nobody knows yet: FuseWeighted
// builds a Fuser once and the caller decides how many streams to hand it.
func scaleDown(w []float64) float64 {
	maxW := 0.0
	for _, v := range w {
		// NaN fails every comparison and is skipped for free; +Inf would win and has
		// to be named. Both are folded to 0 by fuse regardless.
		if v > maxW && !math.IsInf(v, 1) {
			maxW = v
		}
	}
	if maxW <= 1 {
		return 1
	}
	for i := range w {
		w[i] /= maxW
	}
	return 1 / maxW
}

// fuse is the shared implementation. dflt is what a stream past the end of weights
// weighs; a nil weights slice with dflt 1.0 means every stream weighs 1.0, and that
// path is bit-identical to weighting explicitly: IEEE-754 multiplication by 1.0 is
// exact, so no ranking changes and no test pinning an exact order needs to know which
// entry point produced it.
func fuse(streams [][]engine.Candidate, k int, weights []float64, dflt float64) []engine.Candidate {
	// Sized from what the streams actually hold, not from k. A stream is at most
	// k long, but k is caller-supplied and unbounded — the demo takes it from a
	// flag — so hinting k*len(streams) asks the runtime to reserve space for a
	// number of candidates that may not exist. The summed lengths cannot
	// overflow: those elements are already in memory.
	total, depth := 0, 0
	for _, s := range streams {
		total += len(s)
		depth = max(depth, len(s))
	}
	fused := make(map[engine.DocID]float64, total)

	// Rank-major, not stream-major. Float addition is not associative, so the
	// order a document's contributions arrive in decides its last bit — and
	// sweeping streams first makes that order the caller's scorer order. Two
	// documents holding the same multiset of ranks in different streams, 1,2,7
	// against 7,1,2, then get mathematically equal totals that differ as
	// float64, and merely permuting the scorer slice flips which one wins
	// instead of letting TopK's DocID tiebreak settle it.
	//
	// Sweeping rank by rank, every document accumulates in ascending rank order
	// whatever stream supplied each hit, so equal multisets sum to identical
	// bits. Repeats of one rank across streams add the same value and commute.
	// Same total work, and the reciprocal is computed once per rank rather than
	// once per candidate.
	for i := 0; i < depth; i++ {
		// Note what is absent: Candidate.Score is never read. Position is
		// everything.
		w := 1 / (RRFk + float64(i+1))
		for si, stream := range streams {
			if i >= len(stream) {
				continue
			}
			// Unweighted streams take the multiply too. Skipping it behind a branch
			// would save nothing measurable and would create two accumulation paths
			// whose last bits could drift apart, which is exactly what the
			// rank-major ordering above exists to prevent.
			sw := dflt
			if si < len(weights) {
				sw = weights[si]
			}
			// Weight 0 must not create the map entry. Accumulating 0 would leave the
			// document in the result at score 0 — last, but present, holding a rank
			// it did not earn from a stream the caller switched off. A document that
			// also appears in a stream with weight is unaffected: it gets its entry
			// from there.
			//
			// Written as !(sw > 0) rather than sw <= 0 on purpose: it is the same test
			// for 0 and for negatives, and it also catches NaN, which every ordered
			// comparison reports as false. +Inf passes that test and so is named
			// separately: it would send every document in the stream to +Inf, where they
			// compare equal and TopK settles them on DocID — the same silent collapse to
			// insertion order the NaN case produces, reached from the opposite end. That
			// folds all four out-of-contract weights onto the one behaviour documented on
			// FuseWeighted instead of letting either value into the score map.
			if !(sw > 0) || math.IsInf(sw, 1) {
				continue
			}
			// The same rule again, on the product rather than the weight, because
			// that is where the last way to reach it lives. A weight below about
			// 1e-322 is positive, finite and entirely in contract, and still
			// multiplies to nothing at every rank: the vote underflows, `+=` creates
			// the map entry anyway, and the stream's documents land in the result at
			// score 0 — tied, settled by TopK on DocID, holding the insertion-order
			// position that weight 0 is excluded to deny them. Testing the product is
			// also the only test that stays correct if RRFk or the depth changes,
			// since both move where the underflow starts.
			vote := sw * w
			if vote == 0 {
				continue
			}
			fused[stream[i].Doc] += vote
		}
	}

	cands := make([]engine.Candidate, 0, len(fused))
	for doc, score := range fused {
		cands = append(cands, engine.Candidate{Doc: doc, Score: score})
	}
	return engine.TopK(cands, k)
}
