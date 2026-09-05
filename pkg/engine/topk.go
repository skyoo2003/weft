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
// # Selecting k out of n, rather than sorting n
//
// This used to be one full sort, under a ponytail note saying a bounded heap was
// worth buying once a scorer produced candidate sets far larger than k. That
// condition has fired: pkg/scorer/text emits one candidate per matching
// document, so a query for a term held by every document in a 50,000-document
// corpus arrives here with 50,000 candidates and asks for ten. A profile of that
// query put the sort and its comparator at about 13% of the whole query — the
// largest single share of work that was not the posting walk itself.
//
// So k < n takes a k-sized heap, ordered so its root is the *worst* of the best
// k found so far. Every remaining candidate is compared against that root and
// discarded in one comparison unless it beats it. That is n comparisons plus a
// sift per survivor, against n·log₂n for the sort — on the query above, roughly
// 50 thousand against 780 thousand.
//
// The heap is `cands[:k]` itself, so this still sorts in place, still returns a
// prefix of the input, and allocates nothing.
//
// k >= n keeps the full sort, because there is nothing to select: every
// candidate is in the answer and all that is left is to order them. That is the
// path pkg/query's scorers take, which return every match rather than
// truncating.
//
// **The answer is identical either way**, which is what makes this a
// substitution rather than a change: the comparator is a total order — ties
// break on DocID and no two candidates name one document — so "the best k" is a
// unique set and sorting it gives a unique sequence.
//
// ponytail: a hand-rolled sift rather than container/heap, for the reason
// slices.SortFunc was chosen over sort.Slice below — container/heap dispatches
// every comparison through an interface, and comparisons are the thing being
// removed. Thirty lines against reintroducing the cost.
func TopK(cands []Candidate, k int) []Candidate {
	if k <= 0 || len(cands) == 0 {
		return nil
	}
	if k >= len(cands) {
		// slices.SortFunc rather than sort.Slice: same pdqsort and the same
		// cmp.Compare semantics, but monomorphized instead of swapping through
		// reflect.
		slices.SortFunc(cands, rank)
		return cands[:len(cands):len(cands)]
	}

	// The first k are the heap; heapify makes its root the worst of them.
	heap := cands[:k]
	for i := k/2 - 1; i >= 0; i-- {
		siftDown(heap, i)
	}
	for _, c := range cands[k:] {
		// One comparison rejects a candidate that cannot be in the answer, which
		// on a corpus-sized stream is nearly all of them.
		if rank(c, heap[0]) < 0 {
			heap[0] = c
			siftDown(heap, 0)
		}
	}
	slices.SortFunc(heap, rank)
	// Capped at k, not just sliced to it. A bare cands[:k] keeps the spare
	// capacity, so a caller appending to the result writes over element k of the
	// array it just handed in.
	return cands[:k:k]
}

// Collector keeps the best k candidates out of a stream, in memory bounded by k
// rather than by how many candidates it is shown.
//
// TopK is the same selection with the stream already in a slice. This is for the
// scorer that never builds one — and the difference is not a convenience, it is
// the whole of what a document-at-a-time scorer is for. docs/FINDINGS.md
// milestone 22 measured weft against bleve and found weft faster on every query
// shape and allocating **156× more** on the widest: 2.82 MB against 18 KB, of
// which a corpus-sized candidate slice was 800 KB. A slice that is never built
// cannot be sized to the corpus.
//
//	c := engine.NewCollector(k)
//	for … { c.Offer(doc, score) }
//	return c.Take(), nil
//
// **Offer is one comparison in the common case.** Once k candidates have been
// seen, anything that cannot beat the worst of them is rejected against a single
// value; only a survivor pays a sift. So a stream of a hundred thousand
// candidates costs a hundred thousand comparisons and a handful of log₂k sifts.
//
// The zero value is not usable; NewCollector is. A Collector is not safe for
// concurrent use and is not meant to outlive the query that built it.
type Collector struct {
	k    int
	best []Candidate
}

// maxPrealloc caps what one Collector reserves up front: 4096 candidates, 64 KiB.
//
// The ceiling exists because k is the *caller's* number, and since milestone 24 a
// caller can be an HTTP handler relaying a request body — `{"size": 2000000000}`
// asked this constructor for roughly 32 GB before a document was scored. Every
// caller before that chose its own k and could not be a threat to itself, which
// is why the allocation stood unguarded for twenty-three milestones and why the
// server, not the engine, is where the hazard arrived.
//
// It is not a limit on k. A Collector still keeps the best k of any size; past
// this it grows by append, bounded by the documents that actually arrive rather
// than by the number the caller named. 4096 is above every k this project
// measures — the published benchmarks use 10 — so the reservation still happens
// exactly as it did wherever it was ever worth having: docs/FINDINGS.md
// milestone 24.
const maxPrealloc = 4096

// NewCollector returns a Collector keeping the best k. A k of zero or less
// keeps nothing, which is the convention every Candidates already uses.
func NewCollector(k int) *Collector {
	if k <= 0 {
		return &Collector{}
	}
	if k > maxPrealloc {
		return &Collector{k: k}
	}
	return &Collector{k: k, best: make([]Candidate, 0, k)}
}

// Offer shows the collector one candidate. It keeps what it needs and drops the
// rest.
//
// Duplicate DocIDs are not detected: a caller offering one document twice gets
// it twice in the result. A document-at-a-time scorer sums a document's
// contributions before offering it once, which is the shape this is for.
func (c *Collector) Offer(doc DocID, score float64) {
	if c.k == 0 {
		return
	}
	cand := Candidate{Doc: doc, Score: score}
	// Filling: append and, on the last one, make a heap of what is there.
	if len(c.best) < c.k {
		c.best = append(c.best, cand)
		if len(c.best) == c.k {
			for i := c.k/2 - 1; i >= 0; i-- {
				siftDown(c.best, i)
			}
		}
		return
	}
	// Full: one comparison against the worst of the best, which is the root.
	if rank(cand, c.best[0]) < 0 {
		c.best[0] = cand
		siftDown(c.best, 0)
	}
}

// Take returns what was kept, best first, and empties the collector.
//
// The result is the collector's array, so a caller keeping it must not reuse the
// collector — which Take makes impossible rather than merely discouraged by
// dropping the reference.
func (c *Collector) Take() []Candidate {
	out := c.best
	c.best = nil
	if len(out) == 0 {
		// nil rather than an empty slice, which is what every scorer returns for
		// "no opinion" and what a `== nil` caller reads.
		return nil
	}
	// Fewer than k were offered, so nothing was ever heapified — and more than
	// that, a heap is not sorted. Both cases end in one sort of at most k.
	slices.SortFunc(out, rank)
	return out[:len(out):len(out)]
}

// rank orders two candidates best-first: higher score, then lower DocID.
//
// Scores go through cmp.Compare rather than >, which is what makes TopK's
// determinism promise hold for a NaN — see the doc comment above.
func rank(a, b Candidate) int {
	if c := cmp.Compare(b.Score, a.Score); c != 0 {
		return c
	}
	return cmp.Compare(a.Doc, b.Doc)
}

// siftDown restores the heap property at i: every node ranks no better than its
// children, so the root is the worst element and is what a new candidate has to
// beat.
//
// Worst-at-the-root is the inversion that makes this work. A heap ordered the
// obvious way — best at the root — would answer "what is the best so far", which
// nothing needs; what a bounded selection needs is "what is the first thing to
// throw away".
func siftDown(h []Candidate, i int) {
	for {
		worst := i
		if l := 2*i + 1; l < len(h) && rank(h[l], h[worst]) > 0 {
			worst = l
		}
		if r := 2*i + 2; r < len(h) && rank(h[r], h[worst]) > 0 {
			worst = r
		}
		if worst == i {
			return
		}
		h[i], h[worst] = h[worst], h[i]
		i = worst
	}
}
