// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"hash/crc32"
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
// The arithmetic this is bought with, at the milestone 4 scale of C = 171,332
// documents of which V = 148,232 carry a vector of width d = 768, so nlist is
// ⌈√C⌉ = 414 and nprobe is 64:
//
//	build   C·nlist·d assignment + S·nlist·d·5 training ≈ 8.6e10 MAC, about a minute
//	query   nlist·d + nprobe·(V/nlist)·d ≈ 1.8e7 MAC, against 1.14e8 for a full scan
//
// The assignment pass is charged per document and not per vector, because it
// walks the corpus to find out which documents have one; nlist comes from the
// document count for the same reason.
//
// So a query's arithmetic falls by ~6x and its record decodes by about the same,
// and a commit or a merge grows by a constant minute. Both figures are measured
// rather than predicted — `weft-eval recall` puts the decodes at 5.6x, and
// docs/FINDINGS.md prints the rest. The trade closes only once nlist is
// comfortably past nprobe, which is what ivfMinDocs is derived from; below it no
// partition is built.
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
	// Derived from ivfNProbe rather than chosen, because the two are one decision
	// and were briefly two. A query scans nlist centroids and then nprobe lists of
	// count/nlist members each, so with nlist = √count it narrows nothing at all
	// until √count exceeds nprobe. 4,096 was this floor while nprobe was 8; when
	// nprobe was measured up to 64 it became exactly the break-even point, and the
	// milestone's own observable said so — at 4,096 a query takes 100% of the
	// segment as candidates, and the commit pays a training pass and an assignment
	// pass for a partition that excluded nothing.
	//
	// Twice break-even, so nlist ≥ 2·nprobe and a query touches at most half the
	// segment. Candidates per query as a share of the segment, measured on
	// clustered synthetic corpora at d = 8:
	//
	//	count    4,096   8,192  16,384  32,768  65,536
	//	nlist       64      91     128     182     256
	//	share     100%     76%     48%     41%     27%
	//
	// The build is worth a few hundred full scans of arithmetic and a query saves
	// 1 − nprobe/√count of one, so a segment at this floor repays its partition
	// over something like a thousand queries and a larger one sooner. Below it are
	// the pending segment and the tail of an incremental ingest — the segments a
	// query may touch once and never again.
	//
	// ponytail: arithmetic and a candidate-share measurement, not a workload. What
	// nobody has counted is how many queries a small segment really takes; the
	// observable that would move this is the share above, which `weft-eval recall`
	// prints per query.
	ivfMinDocs = 4 * ivfNProbe * ivfNProbe

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
	//
	// 64 is measured, not chosen. The milestone plan fixed the quality bar before
	// anything was built — `text+vector` within 0.005 of milestone 4's 0.6233 —
	// and 8, the value the plan proposed, missed it by 0.0230. The plan's own
	// registered response was to raise this constant and re-measure, which
	// produced the curve docs/EVAL.md prints:
	//
	//	nprobe    8      16      32      64     128     256
	//	nDCG   0.6003  0.6095  0.6174  0.6211  0.6205  0.6233
	//
	// 64 is the smallest measured value that clears the bar. Two things in that
	// curve are worth more than the number. It is not monotone — 128 scores below
	// 64 — because adding candidates reshuffles ties as well as adding neighbours,
	// which is a reminder that recall and nDCG are different quantities. And 64
	// of nlist=414 is 15% of the lists, where the IVF literature expects one to
	// ten: this corpus does not cluster tightly, and docs/FINDINGS.md records that
	// as a finding about the data rather than a tuning result.
	//
	// A constant rather than a fraction of nlist, and that is the load-bearing
	// part. nlist grows as √n, so a constant probes a shrinking share of the
	// corpus as it grows — 15% at 171k documents. A fraction would scan a fixed
	// share of the corpus at every size, which is a full scan with a discount
	// rather than an index.
	//
	// The shrinking stops where ivfMaxList does, and that bound is twelve lines up
	// rather than somewhere else. Past 2^20 documents nlist is pinned at 1,024, so
	// the share floors at 64/1024 = 6.25% and stays there: at ten million documents
	// a query still decodes 625,000 records, not the 200,000 an uncapped √n would
	// give. Raising the cap is the answer if a corpus ever gets there, and what it
	// buys back is measured against a linear assignment pass — see ivfMaxList.
	//
	// The cost of the constant is at the other end: a segment with fewer than 64
	// lists is scanned nearly whole, and the answer there is simply exact.
	ivfNProbe = 64

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

// ivfNList is how many lists a segment of count documents gets: ⌈√count⌉,
// clamped to at least one and at most ivfMaxList.
//
// The square root is the standard choice, and the reason is that it balances the
// two halves of a query — scanning nlist centroids against scanning nprobe lists
// of count/nlist members each.
//
// A float root rather than an integer one, and the perfect squares are why that
// is exact rather than merely short. IEEE-754 requires sqrt to be correctly
// rounded and an int is exact in a float64 below 2^53, so √(k²) is k and the
// ceiling leaves it there — 4096 gives 64, not 65. Above 2^20 the clamp answers
// before rounding could matter, and maxDocCount stops the corpus twelve orders
// short of where a float64 stops being exact.
func ivfNList(count int) int {
	return min(max(int(math.Ceil(math.Sqrt(float64(count)))), 1), ivfMaxList)
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
	// Ceiling division, and that is the whole of "evenly". A truncating stride
	// walks off the end of the sample budget before it walks off the end of the
	// corpus: at count = 2·ivfSample − 1 it is 1, the loop stops at ivfSample
	// vectors, and the back half of the corpus never trains a centroid at all.
	// Rounding up trades a slightly smaller sample for one that spans everything.
	// Written as 1 + (count-1)/ivfSample rather than (count+ivfSample-1)/ivfSample
	// because count is bounded by maxInt and the second form can wrap on a 32-bit
	// build.
	stride := 1 + (count-1)/ivfSample
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
// has no deterministic way to make cheaply. What it costs instead is the centroid
// scan it sits in, and nothing more, because segment.nearest counts only lists
// with members against nprobe: an empty centroid parked at a real document's
// direction ranks high for a query near that document, and charging it a probe
// would quietly shrink the only screw on recall.
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
//
// arm64 compiles this to one FMADDD where amd64 emits a multiply and an add, and
// here that is free rather than a determinism hazard: two float32 values widened
// to float64 have a product of at most 48 significand bits, which float64 holds
// exactly, so there is no intermediate rounding for the fusion to skip. Every
// architecture gets the same sum. ivfNormalizeSum below squares float64s and does
// not have that luxury.
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
func ivfNormalize(dst, v []float32) bool {
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
//
// The square goes through an explicit conversion, which is the one thing that
// stops a compiler fusing it into the addition. Unlike ivfDot's, this product is
// float64 × float64 and does not fit a float64 exactly, so a fused multiply-add
// rounds once where a separate multiply and add round twice — and arm64 emits
// FMADDD here while amd64 emits MULSD and ADDSD. The norm then differs by an ULP
// between architectures, and this is the only float64 multiply on the path from a
// corpus to the bytes on disk, which is the property FINDINGS publishes a sha256
// for. One conversion is cheaper than an asterisk on that claim.
func ivfNormalizeSum(dst []float32, sum []float64) {
	var sq float64
	for _, c := range sum {
		sq += float64(c * c)
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
//
// The shape is a precondition rather than a check, because there is nowhere for
// a bad one to come from. parseIVF allocates exactly nlist × dim centroids and
// refuses a section whose header does not agree with meta; segment.nearest is
// the only caller and it has already returned for a segment with no vectors, no
// partition, or a query of another width. A guard here would be four conditions
// none of which can be false, on the per-query path.
func ivfOrder(cent []float32, nlist, dim int, v []float32) []int {
	dots := make([]float64, nlist)
	out := make([]int, nlist)
	for j := range nlist {
		dots[j], out[j] = ivfDot(cent[j*dim:(j+1)*dim], v), j
	}
	slices.SortFunc(out, func(a, b int) int {
		if c := cmp.Compare(dots[b], dots[a]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	return out
}

// ---------------------------------------------------------------------------
// the ivf section on disk
// ---------------------------------------------------------------------------
//
//	nlist       uvarint          0 means no partition; the reader answers with every id
//	dim         uvarint          centroid width; must equal meta's vector width
//	centroids   nlist × dim × float32 LE, normalized
//	counts      nlist × uvarint  members per list
//	offsets     nlist × uint64 LE, absolute file offsets of the lists
//	lists       per list: ascending segment-local DocIDs, delta uvarint,
//	                      then a CRC-32C seeded with the list's own number
//
// The offset table is fixed width for the reason docoff's is: a query probes
// nprobe lists out of up to a thousand, and reaching list j through a uvarint
// table would mean decoding the j−1 lists in front of it — which is the corpus,
// and the cost this section exists to remove. At eight bytes an entry it is at
// most eight kilobytes, computed forward the way encodeKeys computes its own.
//
// The per-list checksum is seeded with the list number for the reason a document
// record's is seeded with its DocID: a list carries no copy of which centroid it
// belongs to, so a reader that followed a damaged offset would decode a perfectly
// healthy list under the wrong centroid. That is not an error, it is a plausible
// wrong candidate set — and a wrong candidate set is indistinguishable from
// ordinary recall loss, which is exactly the failure this structure must not be
// allowed to hide.

// ivfDeltas hands each id in l to f as the number the encoding writes for it: the
// id itself for the first, the gap from its predecessor for the rest.
//
// One walker rather than one per caller, because ivfListLen has to agree with
// encodeIVF byte for byte — the offset table is computed from the first and filled
// by the second — and the drift if they stop agreeing is the invisible kind. Every
// list would fail its own seeded checksum, the reader would fall back to the whole
// segment, and the answer would stay correct at exactly the cost this section
// exists to remove. Only Scrub would say so.
func ivfDeltas(l []DocID, f func(delta uint64)) {
	prev := uint64(0)
	for i, id := range l {
		d := uint64(id)
		if i > 0 {
			d -= prev
		}
		f(d)
		prev = uint64(id)
	}
}

// ivfListLen is the encoded size of one list, checksum included. It has to agree
// with what encodeIVF writes below it; the decoder's offset check is what proves
// the two still do.
func ivfListLen(l []DocID) int {
	n := crc32.Size
	ivfDeltas(l, func(d uint64) { n += uvarintLen(d) })
	return n
}

// encodeIVF writes the partition. An empty build writes the two-uvarint header
// and stops, which is what keeps the section list the same for every segment —
// see segSections.
func encodeIVF(w *segWriter, b ivfBuild) {
	w.uvarint(uint64(b.nlist))
	w.uvarint(uint64(b.dim))
	if b.nlist == 0 {
		return
	}
	for _, c := range b.centroids {
		// scratch, not a local, for the reason the varint encoders give: a local
		// escapes, and this runs nlist × dim times.
		binary.LittleEndian.PutUint32(w.scratch[:4], math.Float32bits(c))
		w.write(w.scratch[:4])
	}
	for _, l := range b.lists {
		w.uvarint(uint64(len(l)))
	}
	off := w.off() + b.nlist*8
	for _, l := range b.lists {
		binary.LittleEndian.PutUint64(w.scratch[:8], uint64(off))
		w.write(w.scratch[:8])
		off += ivfListLen(l)
	}
	for j, l := range b.lists {
		w.beginUnit(uint64(j))
		ivfDeltas(l, w.uvarint)
		w.endUnit()
	}
}

// ivfSection is the parsed ivf section: everything bounded by nlist decoded up
// front, and the lists themselves left on disk.
//
// The centroids are the one thing decoded rather than left as bytes, and the
// reason is that a query scans all of them: reading them out of the mapping
// would mean a Float32frombits per component per query, where decoding once at
// Open costs nlist × dim × 4 bytes of heap. That is 1.21 MiB for the milestone 4
// corpus — 414 × 768 × 4, nlist from the document count as ivfNList takes it —
// and it is bounded by ivfMaxList × dim rather than by the corpus, so the "heap
// does not scale with the corpus" line still holds: it scales with its square
// root, and then stops.
type ivfSection struct {
	name  string
	nlist int
	dim   int

	// docs is the segment's document count, which is what a list's ids are
	// ranged against. A list is the one structure here whose contents name
	// documents, and an id past the segment reaches into a neighbour's.
	docs int

	centroids []float32
	counts    []int
	offs      []byte // nlist × 8, LE, absolute file offsets
	b         []byte // the whole payload, for decoding a list on request
}

// parseIVF reads the header and the two bounded tables. count and vecDim come
// from meta, which is decoded first.
//
// Everything it checks is answerable from bounded data, which is what makes it
// affordable at Open. What it does not check is any list's contents: that is the
// size of the corpus, it belongs to Scrub, and a list that fails at the point of
// use is answered the way D-006 answers damage elsewhere.
func parseIVF(r *segReader, count, vecDim int) (ivfSection, error) {
	x := ivfSection{name: r.name, docs: count}
	nlist, err := r.intn("ivf list count", ivfMaxList)
	if err != nil {
		return ivfSection{}, err
	}
	dim, err := r.intn("ivf centroid width", maxInt)
	if err != nil {
		return ivfSection{}, err
	}
	if nlist == 0 {
		// No partition. dim is written zero beside it, and a non-zero width with
		// no lists to give it to describes nothing the writer produces.
		if dim != 0 {
			return ivfSection{}, fmt.Errorf("%s: no lists but a centroid width of %d: %w", r.name, dim, ErrCorrupt)
		}
		return x, r.done()
	}
	// Lists but no width. buildIVF returns no partition at all for dim <= 0, so
	// this describes nothing the writer produces — and it is not caught by the
	// comparison below, because a segment holding no vectors claims width zero in
	// meta too and the two agree. Every centroid in such a section would tie at
	// an inner product of zero, which makes the probe set arbitrary rather than
	// ranked.
	if dim == 0 {
		return ivfSection{}, fmt.Errorf("%s: %d lists whose centroids have no width: %w", r.name, nlist, ErrCorrupt)
	}
	// The partition indexes this segment's vectors, so its width is the width
	// meta claims. Disagreement means the two were written by different commits.
	if dim != vecDim {
		return ivfSection{}, fmt.Errorf("%s: centroids are %d wide, %s says the corpus is %d: %w",
			r.name, dim, metaFile, vecDim, ErrCorrupt)
	}
	// Division rather than multiplication, and that is the whole of the guard.
	// dim comes off meta ranged only against maxInt, so nlist·dim·4 overflows
	// uint64 for a wide enough claim — at nlist 1024 and dim 2^52 it wraps to
	// exactly zero — and the wrapped product passes this check. What follows is
	// make([]float32, nlist*dim) on a product that overflowed an int too, which
	// panics in a package whose first line promises it never does. A quotient
	// cannot wrap, and nlist is at least one here.
	if uint64(dim) > uint64(len(r.b)-r.off)/4/uint64(nlist) {
		return ivfSection{}, fmt.Errorf("%s: %d centroids of width %d do not fit in %d remaining bytes: %w",
			r.name, nlist, dim, len(r.b)-r.off, ErrCorrupt)
	}
	x.nlist, x.dim = nlist, dim
	x.centroids = make([]float32, nlist*dim)
	for i := range x.centroids {
		bits, err := r.u32("centroid component")
		if err != nil {
			return ivfSection{}, err
		}
		c := math.Float32frombits(bits)
		if f := float64(c); math.IsNaN(f) || math.IsInf(f, 0) {
			// A NaN centroid poisons every comparison it takes part in, and
			// ivfOrder would sort it last rather than refuse it — so the query
			// would silently never probe that list.
			return ivfSection{}, fmt.Errorf("%s: centroid component %d is %v: %w", r.name, i, c, ErrCorrupt)
		}
		x.centroids[i] = c
	}

	x.counts = make([]int, nlist)
	total := 0
	for j := range nlist {
		// A list cannot hold more documents than the segment has, and the lists
		// together cannot either: they partition the vectors, one document to at
		// most one list. Checked against what is left rather than against count,
		// so the running total cannot itself overflow.
		n, err := r.intn(fmt.Sprintf("ivf list %d length", j), count-total)
		if err != nil {
			return ivfSection{}, err
		}
		x.counts[j] = n
		total += n
	}

	if nlist > (len(r.b)-r.off)/8 {
		return ivfSection{}, fmt.Errorf("%s: %d list offsets do not fit in %d remaining bytes: %w",
			r.name, nlist, len(r.b)-r.off, ErrCorrupt)
	}
	x.offs = r.b[r.off : r.off+nlist*8]
	r.off += nlist * 8
	x.b = r.b
	// No r.done(): the lists are the rest of the payload and walking them is the
	// read this section is lazy to avoid. Scrub is what proves they fill it.
	return x, nil
}

// list decodes one inverted list. The bool is false for a list that does not
// decode, which the caller answers by behaving as though there were no partition
// at all — see segment.nearest and D-006.
func (x ivfSection) list(j int) ([]DocID, bool) {
	if j < 0 || j >= x.nlist {
		return nil, false
	}
	off := binary.LittleEndian.Uint64(x.offs[j*8:])
	if off < segHeaderLen || off-segHeaderLen > uint64(len(x.b)) {
		return nil, false
	}
	// The next list's offset is where this one stops. Bounding the reader by it
	// is what keeps a damaged length from reading into the list beside it — the
	// same arithmetic docoff does for a document record, and free for the same
	// reason: the table is fixed width.
	end := uint64(len(x.b))
	if j+1 < x.nlist {
		next := binary.LittleEndian.Uint64(x.offs[(j+1)*8:])
		if next < off || next-segHeaderLen > uint64(len(x.b)) {
			return nil, false
		}
		end = next - segHeaderLen
	}
	r := &segReader{name: x.name, b: x.b[:end], off: int(off - segHeaderLen)}
	ids, err := x.decodeList(r, j)
	if err != nil {
		return nil, false
	}
	return ids, true
}

// decodeList reads list j from wherever r is positioned, verifying its seeded
// checksum. The count comes from the table parsed at Open, so a list's length is
// not a number taken from beside the list itself.
func (x ivfSection) decodeList(r *segReader, j int) ([]DocID, error) {
	start := r.off
	// Same capacity discipline as decodeTermIndex and the document decoders: the
	// count came off disk and is bounded only by the segment's document count, so
	// on its own it lets a damaged counts table ask for four bytes per document of
	// a corpus this list plainly does not hold. A delta is at least one byte, so
	// what is left of the payload is the honest ceiling — and nearest decodes up
	// to nprobe lists per query.
	out := make([]DocID, 0, min(x.counts[j], len(r.b)-r.off))
	prev := uint64(0)
	for i := range x.counts[j] {
		d, err := r.uvarint("ivf posting delta")
		if err != nil {
			return nil, err
		}
		var id uint64
		if i == 0 {
			id = d
		} else {
			// Strictly ascending, and checked before the addition: uint64 wraps
			// silently, and a wrapped id lands back inside the segment and passes
			// the range check below.
			if d == 0 {
				return nil, fmt.Errorf("%s: list %d repeats a document; lists are strictly ascending: %w", x.name, j, ErrCorrupt)
			}
			if d > math.MaxUint64-prev {
				return nil, fmt.Errorf("%s: list %d delta %d overflows past %d: %w", x.name, j, d, prev, ErrCorrupt)
			}
			id = prev + d
		}
		if id >= uint64(x.docs) {
			return nil, fmt.Errorf("%s: list %d names document %d of a %d-document segment: %w", x.name, j, id, x.docs, ErrCorrupt)
		}
		prev = id
		out = append(out, DocID(id))
	}
	if err := r.unit(fmt.Sprintf("ivf list %d", j), start, uint64(j)); err != nil {
		return nil, err
	}
	return out, nil
}

// scrubIVF walks every list and checks everything parseIVF is too lazy to.
//
// Three things, and each is damage no other check here can see. Every list
// verifies against its own seeded checksum, so a healthy list served under
// another centroid's number is caught. No document appears in two lists, because
// one that did would be scored twice and reach the caller as a duplicate result.
// And the lists sit exactly where the offset table says and fill the payload
// between them, which is the witness the counts themselves do not carry — a
// count that names fewer members than were written leaves the members it does
// name intact and verifying.
//
// What it deliberately does not check is that every vector-bearing document
// appears in some list. A document missing from the partition is invisible to a
// vector query, and the cost of that is recall — the same currency IVF spends by
// design, and not separable from it by any rule available here. Buying the check
// would mean carrying one bit per document out of the docs walk to compare
// against, and the answer it would give is "this index recalls slightly less
// than it should", which is what `weft-eval recall` measures directly.
func scrubIVF(r *segReader, count, vecDim int) error {
	x, err := parseIVF(r, count, vecDim)
	if err != nil {
		return err
	}
	if x.nlist == 0 {
		return nil // parseIVF already required the payload to end there
	}
	seen := make([]bool, count)
	for j := range x.nlist {
		at := uint64(segHeaderLen + r.off)
		if off := binary.LittleEndian.Uint64(x.offs[j*8:]); off != at {
			return fmt.Errorf("%s: list %d is recorded at offset %d, it begins at %d: %w", x.name, j, off, at, ErrCorrupt)
		}
		ids, err := x.decodeList(r, j)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if seen[id] {
				return fmt.Errorf("%s: document %d is in list %d and in an earlier one: %w", x.name, id, j, ErrCorrupt)
			}
			seen[id] = true
		}
	}
	return r.done()
}
