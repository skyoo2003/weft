// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"cmp"
	"slices"
)

// TopK orders cands best-first and truncates to k, sorting in place and
// returning a prefix of the input.
//
// Ties break on DocID. That matters more than it looks: fusion consumes ranks,
// so if two equal-scoring documents could come back in either order, the final
// ranking would be nondeterministic and no ranking test could be written.
//
// Scores go through cmp.Compare rather than >, which is what makes that promise
// hold for a NaN. Both `NaN > x` and `x > NaN` are false, so comparing by hand
// reports a NaN as neither better nor worse than anything and skips the DocID
// tiebreak with it: the outcome depended on the order candidates arrived in, and
// a NaN-scored document could hold rank 1 above one scoring 0.9, which fusion
// would pass on as a plausible result. cmp.Compare defines NaN as less than
// every other value and equal to itself, so it sorts to the bottom and still
// ties on DocID.
//
// This is not the guard ErrNonFiniteVector provides. That one keeps NaN out of
// the built-in vector scorer; this is the exported selection path every scorer
// shares, including ones this repo will never see.
//
// ponytail: full sort, O(n log n). A bounded container/heap is O(n log k) and
// worth it once a scorer produces candidate sets far larger than k — none does
// in milestone 1, and one shared selection path beats four hand-rolled ones.
// Do not upgrade before the cursor interface is settled (docs/FINDINGS.md
// section 3.1): early termination maintains a threshold rather than a k-sized
// heap, so building the heap first means writing selection twice.
func TopK(cands []Candidate, k int) []Candidate {
	if k <= 0 || len(cands) == 0 {
		return nil
	}
	// slices.SortFunc rather than sort.Slice: same pdqsort and the same
	// cmp.Compare semantics, but monomorphized instead of swapping through
	// reflect. This is the one selection path every scorer and the fuser share,
	// and text.go emits one candidate per matching document, so the candidate
	// set is corpus-sized for a common term rather than k-sized.
	slices.SortFunc(cands, func(a, b Candidate) int {
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
			return c
		}
		return cmp.Compare(a.Doc, b.Doc)
	})
	// Capped at k, not just sliced to it. A bare cands[:k] keeps the spare
	// capacity, so a caller appending to the result writes over element k of the
	// array it just handed in.
	return cands[:min(len(cands), k):min(len(cands), k)]
}
