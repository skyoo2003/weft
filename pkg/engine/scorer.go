// SPDX-License-Identifier: Apache-2.0

package engine

import "context"

// Scorer is the whole architecture. Text, vector, graph and recency implement
// exactly this and nothing wider, so the fusion operator can consume any number
// of them without knowing what any of them are.
//
// If this interface ever grows a method that only some scorers can answer, the
// milestone 1 hypothesis is in trouble: that is the moment fusion (or search)
// starts needing to know which scorer it holds.
type Scorer interface {
	// Name is for diagnostics and test assertions only. Nothing in the ranking
	// path is allowed to branch on it.
	Name() string

	// Candidates returns at most k documents this scorer considers relevant to
	// q, best first. Returning fewer than k, or none at all, is normal: a
	// scorer that has no opinion (no vector in the query, no reachable
	// neighbours) returns an empty slice and no error.
	Candidates(ctx context.Context, q Query, k int) ([]Candidate, error)
}
