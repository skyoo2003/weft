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

// ivfListLen is the encoded size of one list, checksum included. It has to agree
// with what encodeIVF writes below it; the decoder's offset check is what proves
// the two still do.
func ivfListLen(l []DocID) int {
	n := crc32.Size
	prev := uint64(0)
	for i, id := range l {
		d := uint64(id)
		if i > 0 {
			d -= prev
		}
		n += uvarintLen(d)
		prev = uint64(id)
	}
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
		prev := uint64(0)
		for i, id := range l {
			if i == 0 {
				w.uvarint(uint64(id))
			} else {
				w.uvarint(uint64(id) - prev)
			}
			prev = uint64(id)
		}
		w.endUnit()
	}
}

// ivfSection is the parsed ivf section: everything bounded by nlist decoded up
// front, and the lists themselves left on disk.
//
// The centroids are the one thing decoded rather than left as bytes, and the
// reason is that a query scans all of them: reading them out of the mapping
// would mean a Float32frombits per component per query, where decoding once at
// Open costs nlist × dim × 4 bytes of heap. That is 1.13 MB for the milestone 4
// corpus and it is bounded by ivfMaxList × dim rather than by the corpus, so the
// "heap does not scale with the corpus" line still holds — it scales with its
// square root, and then stops.
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
	// The partition indexes this segment's vectors, so its width is the width
	// meta claims. Disagreement means the two were written by different commits.
	if dim != vecDim {
		return ivfSection{}, fmt.Errorf("%s: centroids are %d wide, %s says the corpus is %d: %w",
			r.name, dim, metaFile, vecDim, ErrCorrupt)
	}
	// In uint64: on a 32-bit build nlist·dim·4 for a wide embedding passes what
	// an int holds, and a wrapped product would slice a region shorter than the
	// centroids it is about to be read as.
	need := uint64(nlist) * uint64(dim) * 4
	if need > uint64(len(r.b)-r.off) {
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
	out := make([]DocID, 0, x.counts[j])
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
