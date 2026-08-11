package engine

import "sort"

// TopK orders cands best-first and truncates to k, sorting in place and
// returning a prefix of the input.
//
// Ties break on DocID. That matters more than it looks: fusion consumes ranks,
// so if two equal-scoring documents could come back in either order, the final
// ranking would be nondeterministic and no ranking test could be written.
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
		if cands[i].Score != cands[j].Score {
			return cands[i].Score > cands[j].Score
		}
		return cands[i].Doc < cands[j].Doc
	})
	if len(cands) > k {
		cands = cands[:k]
	}
	return cands
}
