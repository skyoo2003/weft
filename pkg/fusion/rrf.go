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
	// Every stream is at most k long, so this is an upper bound rather than a
	// guess, and it is small.
	fused := make(map[engine.DocID]float64, max(k, 0)*len(streams))
	for _, stream := range streams {
		for i, c := range stream {
			// Note what is absent: c.Score is never read. Position is
			// everything.
			rank := float64(i + 1)
			fused[c.Doc] += 1 / (RRFk + rank)
		}
	}

	cands := make([]engine.Candidate, 0, len(fused))
	for doc, score := range fused {
		cands = append(cands, engine.Candidate{Doc: doc, Score: score})
	}
	return engine.TopK(cands, k)
}
