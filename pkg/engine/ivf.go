// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"cmp"
	"math"
	"slices"
)

// This file is the milestone 3b structure: an IVF-flat partition of a segment's
// vectors, built in memory and written to the `ivf` section by segment.go.
//
// What it computes and what it deliberately does not. The partition answers
// "which documents are worth scoring exactly", by inner product against a small
// set of centroids. It computes no similarity a caller ever sees — cosine stays
// in scorer/vector, and D-008 records why: engine knows the geometry, the scorer
// knows the metric. Everything in this file returns DocIDs.
//
// Why it lives in engine at all. The partition is a section of the segment
// format, written by the writer and read by the reader, and milestone 1's test
// is whether a scorer needs a private store — not whether engine may hold a
// structure the format defines. docs/FINDINGS.md carries the tension in full.
//
// The arithmetic this is bought with, at the milestone 4 scale of N = 148,232
// vectors of width 768:
//
//	build   N·nlist·d assignment + S·nlist·d·5 training ≈ 7.4e10 MAC, one or two minutes
//	query   nlist·d + nprobe·(N/nlist)·d ≈ 2.7e6 MAC, against 1.14e8 for a full scan
//
// So a query's arithmetic falls by ~42x and its record decodes by ~48x, and a
// commit or a merge grows by a constant of a minute or two. Below ivfMinDocs
// that trade does not close and no partition is built.
//
// Two disciplines the rest of the file obeys without saying so again:
//
// No RNG. Not a fixed seed — none at all. Training samples on a fixed stride and
// seeds its centroids from that sample on a fixed stride, so "the same corpus
// gives the same bytes" is a property of the algorithm rather than of a seed
// somebody has to remember not to change. The same rule as the ban on map
// iteration in the writer, and for the same reason: a segment's bytes are
// asserted equal across two builds.
//
// No context. Training is the longest thing a Commit does and cancellation
// cannot reach it, because Commit takes no context and the golden API file
// admits exactly one new name this milestone. Adding one here would be a public
// API change the plan does not buy; the cost is recorded in docs/FINDINGS.md
// rather than paid quietly.
const (
	// ivfMinDocs is the segment size below which no partition is built.
	//
	// Building one costs a training pass over a sample plus an assignment pass
	// over every vector, each vector against nlist centroids — for a segment of
	// n documents that is about n·√n·d multiply-accumulates, against n·d for the
	// scan it replaces. At 4,096 the build is worth roughly 64 full scans, so it
	// takes 64 queries against that segment to break even. Segments smaller than
	// this are the pending ones and the tail of an incremental ingest, which are
	// exactly the ones a query may touch once and never again.
	//
	// ponytail: 4,096 is the break-even arithmetic above and nothing else — no
	// measurement of how many queries a small segment actually sees. Revisit if
	// a workload turns up that queries small segments hard; the observable is
	// candidate count per query per segment, which `weft-eval recall` prints.
	ivfMinDocs = 4096

	// ivfMaxList caps nlist. The assignment pass is linear in nlist, so an
	// unclamped √n would make a billion-document segment cost a thousand times
	// per vector what a million-document one does. FAISS's own convention.
	ivfMaxList = 1024

	// ivfNProbe is how many lists a query scans before it starts widening.
	//
	// It is the only screw on recall, and it is a constant rather than an option
	// on purpose: exposing it would put the recall/latency trade in every
	// deployment's hands, which is the position D-005 warns about. Index.Nearest
	// raises it on its own when a query would otherwise come back with fewer
	// than k candidates, so "at least k" is a contract rather than a tuning
	// exercise.
	ivfNProbe = 8

	// ivfSample is how many vectors training looks at. Lloyd's cost is
	// S·nlist·d per iteration, so this is what keeps training from scaling with
	// the corpus — and it bounds the one large allocation a build makes, at
	// S·d·4 bytes rather than n·d·4.
	ivfSample = 20000

	// ivfLloyd is how many refinement passes the centroids get. Spherical
	// k-means on a sample converges fast and the structure is an approximation
	// either way; five is the literature's usual floor and the build cost is
	// linear in it.
	//
	// ponytail: five, unmeasured. The observable that would move it is recall at
	// fixed nprobe — `weft-eval recall` — not the training objective.
	ivfLloyd = 5
)

// ivfBuild is a partition before it reaches disk: the centroids, and the
// segment-local DocIDs assigned to each.
//
// nlist == 0 means no partition, which is a real and expected state — a segment
// below ivfMinDocs, or one holding no vectors. The section is still written, so
// the section list does not vary with the corpus; see segSections.
type ivfBuild struct {
	nlist int
	dim   int

	// centroids is nlist × dim, flat and row-major, every row L2-normalized.
	// Flat rather than [][]float32 because it is written as one run of float32
	// and read back as one, and because the search below walks it row by row.
	centroids []float32

	// lists[j] holds the segment-local ids assigned to centroid j, ascending.
	// Ascending comes free — the assignment pass walks ids in order — and the
	// delta encoding on disk requires it.
	//
	// An empty list is kept. A list's number is its centroid's index, so
	// dropping one would leave every list after it naming the wrong centroid.
	lists [][]DocID
}

// ivfNList is how many lists a segment of count documents gets: √count, clamped.
//
// The square root is the standard choice, and the reason is that it balances the
// two halves of a query — scanning nlist centroids against scanning nprobe lists
// of count/nlist members each. Computed in integers rather than through
// math.Ceil, because the perfect squares are exactly the sizes a test pins and a
// float sqrt that lands a half-ulp low turns 4096 into 65 lists.
func ivfNList(count int) int {
	if count <= 1 {
		return 1
	}
	// The product is compared in uint64: on a 32-bit build n·n for a corpus of
	// maxInt documents passes what an int holds, and a wrapped product would
	// stop the loop early and hand back a root that is not one.
	n := uint64(math.Sqrt(float64(count)))
	for n*n < uint64(count) {
		n++
	}
	for n > 1 && (n-1)*(n-1) >= uint64(count) {
		n--
	}
	return min(int(n), ivfMaxList)
}

// buildIVF partitions count documents' vectors, read through vecAt.
//
// vecAt returns the segment-local document i's vector, or a slice of any other
// width — nil included — for a document this partition should not hold. It is
// called twice over the corpus: once on a stride for training, once in full for
// assignment. Both callers pass a random-access accessor, so nothing here needs
// the corpus in memory; what it does hold is the training sample, which is
// bounded by ivfSample·dim rather than by the corpus.
func buildIVF(count, dim int, vecAt func(i int) []float32) ivfBuild {
	if count < ivfMinDocs || dim <= 0 {
		return ivfBuild{}
	}
	sample := ivfTrainingSample(count, dim, vecAt)
	if len(sample) == 0 {
		// Documents but no usable vectors. Same answer as a segment below the
		// floor: the reader falls back to every id it holds.
		return ivfBuild{}
	}
	// Never more lists than training vectors. A corpus where most documents
	// carry no vector would otherwise ask for more centroids than there is
	// anything to seed them from, and k-means with k > n has no fixed point.
	nlist := min(ivfNList(count), len(sample)/dim)
	cent := ivfSeedCentroids(sample, nlist, dim)
	ivfRefine(cent, sample, nlist, dim)
	return ivfBuild{
		nlist:     nlist,
		dim:       dim,
		centroids: cent,
		lists:     ivfAssign(count, dim, vecAt, cent, nlist),
	}
}

// ivfTrainingSample takes up to ivfSample vectors on a fixed stride, normalized,
// flat and row-major.
//
// A stride rather than a random draw, which is the determinism rule in its most
// load-bearing place: the sample decides the centroids, the centroids decide the
// bytes, and the bytes are asserted equal across two builds. A stride also picks
// evenly across the corpus, which for a corpus ingested in some order — by
// source, by date — is what a uniform random draw would be bought for.
//
// Its weakness is stated rather than hidden: a corpus whose ordering is periodic
// with the stride is sampled from one phase of that period. Ingest order would
// have to be adversarial for that to bite, and the alternative costs the
// determinism this milestone's fourth assertion is.
func ivfTrainingSample(count, dim int, vecAt func(i int) []float32) []float32 {
	stride := max(1, count/ivfSample)
	out := make([]float32, 0, min(count/stride+1, ivfSample)*dim)
	buf := make([]float32, dim)
	for i := 0; i < count && len(out) < ivfSample*dim; i += stride {
		if !ivfNormalize(buf, vecAt(i)) {
			continue
		}
		out = append(out, buf...)
	}
	return out
}

// ivfSeedCentroids picks nlist starting centroids from the sample, on a stride.
//
// The sample is already spread across the corpus and already normalized, so a
// stride over it is a spread over the corpus — and it is the same no-RNG
// discipline as the sampling above. k-means++ would seed better and would need a
// generator; five Lloyd passes are what buys the difference back.
func ivfSeedCentroids(sample []float32, nlist, dim int) []float32 {
	ns := len(sample) / dim
	stride := ns / nlist // at least 1: buildIVF clamped nlist to ns
	cent := make([]float32, nlist*dim)
	for j := range nlist {
		copy(cent[j*dim:(j+1)*dim], sample[j*stride*dim:])
	}
	return cent
}

// ivfRefine runs Lloyd's algorithm on the sample, in place.
//
// Spherical, not plain: the vectors are normalized, the assignment step
// maximizes an inner product, and each new centroid is the normalized mean of
// its members. That is not a detail — ranking is by cosine, and partitioning by
// raw L2 would gather the documents with the largest norms into one list
// regardless of direction, losing exactly the neighbours a cosine query wants.
// Normalizing makes the two metrics agree.
//
// A centroid with no members keeps its previous position. Reseeding it — from
// the largest cluster, say — is the usual repair and it needs a choice this file
// has no deterministic way to make cheaply; an empty list simply never comes
// back as a candidate, which costs nothing but the centroid scan it sits in.
func ivfRefine(cent, sample []float32, nlist, dim int) {
	ns := len(sample) / dim
	// float64 accumulators, for the reason scorer/vector's dot gives: a centroid
	// is the mean of thousands of float32 components and repeated float32
	// addition loses the tail of it.
	sums := make([]float64, nlist*dim)
	counts := make([]int, nlist)
	for range ivfLloyd {
		clear(sums)
		clear(counts)
		for i := range ns {
			v := sample[i*dim : (i+1)*dim]
			j := ivfNearestCentroid(cent, nlist, dim, v)
			counts[j]++
			s := sums[j*dim : (j+1)*dim]
			for d, c := range v {
				s[d] += float64(c)
			}
		}
		for j := range nlist {
			if counts[j] == 0 {
				continue
			}
			ivfNormalizeSum(cent[j*dim:(j+1)*dim], sums[j*dim:(j+1)*dim])
		}
	}
}

// ivfAssign walks every document and files it under its nearest centroid.
//
// This is the pass whose cost is count·nlist·dim, and the one that makes a build
// take a minute where the training takes seconds. What it holds is the lists —
// four bytes a document — and not the vectors.
func ivfAssign(count, dim int, vecAt func(i int) []float32, cent []float32, nlist int) [][]DocID {
	lists := make([][]DocID, nlist)
	buf := make([]float32, dim)
	for i := range count {
		if !ivfNormalize(buf, vecAt(i)) {
			continue
		}
		j := ivfNearestCentroid(cent, nlist, dim, buf)
		// Ids ascend, so appending keeps every list sorted for free — which is
		// what the delta encoding on disk needs.
		lists[j] = append(lists[j], DocID(i))
	}
	return lists
}

// ivfNearestCentroid is the argmax over centroids of the inner product with v,
// which for normalized vectors is the argmax of cosine.
//
// Ties keep the lowest list number. Strictly-greater rather than
// greater-or-equal is the whole of that: two centroids equidistant from a vector
// would otherwise file it under whichever the loop reached last, and "whichever
// the loop reached last" is stable only until somebody reorders something.
func ivfNearestCentroid(cent []float32, nlist, dim int, v []float32) int {
	best, bestDot := 0, math.Inf(-1)
	for j := range nlist {
		if d := ivfDot(cent[j*dim:(j+1)*dim], v); d > bestDot {
			best, bestDot = j, d
		}
	}
	return best
}

// ivfDot accumulates in float64 for the reason scorer/vector's dot does: the
// inputs are float32 and the widths are in the hundreds, so repeated float32
// rounding would cost real precision. No context poll — see the file comment.
func ivfDot(a, b []float32) float64 {
	var sum float64
	for i, c := range a {
		sum += float64(c) * float64(b[i])
	}
	return sum
}

// ivfNormalize writes v scaled to unit length into dst and reports whether it
// had a direction to scale.
//
// False for three cases the partition must leave out, and each of them is a
// document the vector scorer already declines to rank: a document with no
// vector, one whose width is not the corpus's, and one whose vector is all
// zeroes. A non-finite component is refused by Add and by the decoder, so it
// cannot reach here — but the norm check catches it anyway, since a NaN
// component makes the norm NaN.
func ivfNormalize(dst []float32, v []float32) bool {
	if len(v) != len(dst) {
		return false
	}
	n := math.Sqrt(ivfDot(v, v))
	if n == 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return false
	}
	for i, c := range v {
		dst[i] = float32(float64(c) / n)
	}
	return true
}

// ivfNormalizeSum writes the unit-length direction of a float64 accumulator into
// a float32 centroid, leaving it untouched if the accumulator has no direction.
func ivfNormalizeSum(dst []float32, sum []float64) {
	var sq float64
	for _, c := range sum {
		sq += c * c
	}
	n := math.Sqrt(sq)
	if n == 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return
	}
	for i, c := range sum {
		dst[i] = float32(c / n)
	}
}

// ivfOrder ranks every list number by its centroid's inner product with v, best
// first.
//
// Every list, not the best nprobe: Index.Nearest widens its probe when a query
// would come back short, and a total order makes that a longer prefix rather
// than a second search. nlist is at most ivfMaxList, so the sort is over a
// thousand elements against the nlist·dim multiply-accumulates that produced
// them.
//
// v is not normalized first, and does not need to be: scaling a query by a
// positive constant scales every inner product equally and cannot reorder them.
//
// Scores go through cmp.Compare rather than >, the same choice TopK makes and
// for the same reason — a NaN sorts to the bottom rather than landing wherever
// the comparison happened to reach it.
func ivfOrder(cent []float32, nlist, dim int, v []float32) []int {
	if nlist <= 0 || dim <= 0 || len(v) != dim || len(cent) < nlist*dim {
		return nil
	}
	type ranked struct {
		list int
		dot  float64
	}
	all := make([]ranked, nlist)
	for j := range nlist {
		all[j] = ranked{j, ivfDot(cent[j*dim:(j+1)*dim], v)}
	}
	slices.SortFunc(all, func(a, b ranked) int {
		if c := cmp.Compare(b.dot, a.dot); c != 0 {
			return c
		}
		return cmp.Compare(a.list, b.list)
	})
	out := make([]int, nlist)
	for i, r := range all {
		out[i] = r.list
	}
	return out
}
