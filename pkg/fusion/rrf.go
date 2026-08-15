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

import "github.com/skyoo2003/weft/pkg/engine"

// RRFk is the rank constant from Cormack, Clarke & Buettcher (2009). It damps
// the contribution of top ranks so one confident stream cannot dominate the
// fused order on its own.
const RRFk = 60.0

// Fuse merges streams by Reciprocal Rank Fusion: each document scores
// Σ 1/(RRFk + rank) over every stream it appears in, ranks being 1-based.
//
// Reciprocal *rank* is the reason this works for arbitrary scorers. Candidate
// scores arrive on incompatible scales — BM25 is unbounded, cosine is [-1,1],
// graph proximity is (0,1] — so any weighted sum would first need per-scorer
// normalization, and knowing how to normalize means knowing which scorer you
// hold. Rank is scale-free, so this function never reads Candidate.Score at all.
//
// There is deliberately no scorer count, no scorer identity, and no branch on
// either. A switch on scorer name appearing in this file is the hypothesis
// failing.
// Empty streams, no streams and a non-positive k all fall through to TopK,
// which already returns nil for each — guarding them here would be three copies
// of an invariant this package does not own.
func Fuse(streams [][]engine.Candidate, k int) []engine.Candidate {
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
		for _, stream := range streams {
			if i < len(stream) {
				fused[stream[i].Doc] += w
			}
		}
	}

	cands := make([]engine.Candidate, 0, len(fused))
	for doc, score := range fused {
		cands = append(cands, engine.Candidate{Doc: doc, Score: score})
	}
	return engine.TopK(cands, k)
}
