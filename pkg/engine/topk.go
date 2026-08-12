package engine

import (
	"cmp"
	"sort"
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
	sort.Slice(cands, func(i, j int) bool {
		if c := cmp.Compare(cands[i].Score, cands[j].Score); c != 0 {
			return c > 0
		}
		return cands[i].Doc < cands[j].Doc
	})
	if len(cands) > k {
		cands = cands[:k]
	}
	return cands
}
