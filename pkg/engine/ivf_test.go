// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"math"
	"slices"
	"testing"
)

// These are the milestone 3b assertions about the partition itself, with no
// disk under them. Task 2 puts the same structure on disk; this file is what
// says the structure is worth writing down.
//
// Three properties, in the order they matter:
//
//	determinism  the same corpus trained twice gives bit-identical centroids
//	coverage     every vector lands in exactly one list, and no list vanishes
//	recall       nprobe lists out of nlist still find what a full scan finds
//
// Determinism comes first because it is the one the format depends on: a
// segment's bytes are asserted identical across two builds, and a k-means that
// consulted a random number generator — or a map — would make that assertion
// unwritable. The design answer is not a fixed seed but no seed at all; there
// is no RNG in ivf.go to fix.

// splitmix is the test's own source of arbitrary-but-fixed numbers.
//
// It lives here rather than in math/rand because the production code has no
// RNG and this file is what proves it: a generator visible only to the test
// cannot be reached from ivf.go by accident. Splitmix64, whose constants are
// published, so a reader can check that the corpus below is what it says.
type splitmix uint64

func (s *splitmix) next() uint64 {
	*s += 0x9E3779B97F4A7C15
	z := uint64(*s)
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// unit returns a float in [-1, 1).
func (s *splitmix) unit() float32 {
	return float32(int64(s.next()>>11))/float32(int64(1)<<52) - 1
}

// clusteredCorpus builds n vectors of width dim drawn from groups well
// separated centres, with membership taken from the generator rather than from
// the index.
//
// Membership by i%groups would be periodic, and the training sample is taken on
// a fixed stride — the two periods would alias and the initial centroids would
// cover a fraction of the clusters. That would be a property of this corpus and
// not of the algorithm, which is the wrong thing for a recall test to measure.
func clusteredCorpus(n, dim, groups int) [][]float32 {
	g := splitmix(1)
	centres := make([][]float32, groups)
	for c := range centres {
		v := make([]float32, dim)
		for j := range v {
			v[j] = g.unit()
		}
		centres[c] = v
	}
	out := make([][]float32, n)
	for i := range out {
		c := centres[int(g.next()%uint64(groups))]
		v := make([]float32, dim)
		for j := range v {
			// Noise well below the spread between centres, so a nearest
			// neighbour by cosine is nearly always a cluster sibling and a
			// partition that finds the cluster finds the neighbours.
			v[j] = c[j] + g.unit()*0.12
		}
		out[i] = v
	}
	return out
}

// vecAt is the accessor buildIVF reads a corpus through: nil for a document
// that carries no vector.
func vecAt(vs [][]float32) func(int) []float32 {
	return func(i int) []float32 { return vs[i] }
}

func cosine(a, b []float32) float64 {
	var ab, aa, bb float64
	for i := range a {
		ab += float64(a[i]) * float64(b[i])
		aa += float64(a[i]) * float64(a[i])
		bb += float64(b[i]) * float64(b[i])
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return ab / math.Sqrt(aa*bb)
}

// TestIVFTrainingIsDeterministic is assertion (4) of the milestone's pass line,
// one layer below where it is finally checked.
//
// The segment format asserts that two builds of one corpus produce identical
// bytes. Everything else in the writer already satisfies that by construction —
// sorted term order, positional ids — and k-means is the first structure that
// could break it. Bit equality rather than approximate equality, because the
// bytes on disk are bits: two centroids that differ in the last mantissa place
// are two different segments.
func TestIVFTrainingIsDeterministic(t *testing.T) {
	vs := clusteredCorpus(ivfMinDocs*2, 16, 24)
	first := buildIVF(len(vs), 16, vecAt(vs))
	second := buildIVF(len(vs), 16, vecAt(vs))

	if first.nlist != second.nlist || first.dim != second.dim {
		t.Fatalf("two builds disagree on shape: %d/%d and %d/%d",
			first.nlist, first.dim, second.nlist, second.dim)
	}
	if len(first.centroids) != len(second.centroids) {
		t.Fatalf("two builds produced %d and %d centroid components",
			len(first.centroids), len(second.centroids))
	}
	for i := range first.centroids {
		if math.Float32bits(first.centroids[i]) != math.Float32bits(second.centroids[i]) {
			t.Fatalf("centroid component %d differs between builds: %v and %v — training is not deterministic",
				i, first.centroids[i], second.centroids[i])
		}
	}
	for j := range first.lists {
		if !slices.Equal(first.lists[j], second.lists[j]) {
			t.Fatalf("list %d differs between builds", j)
		}
	}
}

// TestIVFAssignsEveryVectorToExactlyOneList is what makes a candidate set a
// partition rather than a sample.
//
// Twice over. A document in two lists is scored twice and reaches the caller as
// a duplicate result; a document in none is invisible to every vector query,
// which reads as a corpus that does not hold it. Lists with no members are kept
// rather than dropped, because a list's number is its centroid's index — drop
// one and every list after it names the wrong centroid.
func TestIVFAssignsEveryVectorToExactlyOneList(t *testing.T) {
	dim := 12
	vs := clusteredCorpus(ivfMinDocs+500, dim, 17)
	// Two documents the partition has to leave out, for reasons the vector
	// scorer already applies: no vector at all, and a vector with no direction.
	vs[7] = nil
	vs[11] = make([]float32, dim)

	b := buildIVF(len(vs), dim, vecAt(vs))
	if b.nlist == 0 {
		t.Fatalf("a %d-document corpus was not partitioned", len(vs))
	}
	if len(b.lists) != b.nlist {
		t.Fatalf("nlist is %d and %d lists were built — an empty list was dropped, so list numbers no longer name centroids",
			b.nlist, len(b.lists))
	}

	seen := make(map[DocID]int, len(vs))
	for j, l := range b.lists {
		if !slices.IsSorted(l) {
			t.Errorf("list %d is not ascending; the delta encoding on disk requires it", j)
		}
		for _, id := range l {
			seen[id]++
		}
	}
	for i, v := range vs {
		want := 1
		if v == nil || cosine(v, v) == 0 {
			want = 0
		}
		if got := seen[DocID(i)]; got != want {
			t.Errorf("document %d appears in %d lists, want %d", i, got, want)
		}
	}
}

// TestIVFRecallOnClusteredCorpus is the property the whole milestone rests on:
// scanning nprobe lists out of nlist finds what scanning everything finds.
//
// Synthetic, and deliberately so. Whether the real citation corpus clusters this
// well is a question about the data rather than about the code, and the plan
// keeps the two apart: this pins the algorithm, and `weft-eval recall` measures
// the corpus. A synthetic pass with a real-corpus failure is a finding about the
// data; a synthetic failure is a bug here.
func TestIVFRecallOnClusteredCorpus(t *testing.T) {
	const (
		dim    = 16
		groups = 48
		k      = 10
	)
	vs := clusteredCorpus(ivfMinDocs*2, dim, groups)
	b := buildIVF(len(vs), dim, vecAt(vs))
	if b.nlist == 0 {
		t.Fatalf("a %d-document corpus was not partitioned", len(vs))
	}

	g := splitmix(99)
	hits, want := 0, 0
	const queries = 40
	for range queries {
		// A query near a document rather than a centroid: this is what a caller
		// does, and it is the case where the query sits between two lists.
		q := slices.Clone(vs[int(g.next()%uint64(len(vs)))])
		for j := range q {
			q[j] += g.unit() * 0.05
		}

		exact := topByCosine(vs, q, allIDsOf(vs), k)
		order := ivfOrder(b.centroids, b.nlist, b.dim, q)
		var cands []DocID
		for _, j := range order[:min(ivfNProbe, len(order))] {
			cands = append(cands, b.lists[j]...)
		}
		got := topByCosine(vs, q, cands, k)

		for _, id := range exact {
			if slices.Contains(got, id) {
				hits++
			}
			want++
		}
	}
	recall := float64(hits) / float64(want)
	t.Logf("recall@%d over %d queries with nprobe=%d of nlist=%d: %.3f", k, queries, ivfNProbe, b.nlist, recall)
	if recall < 0.9 {
		t.Errorf("recall@%d is %.3f, want at least 0.900", k, recall)
	}
}

func allIDsOf(vs [][]float32) []DocID {
	out := make([]DocID, len(vs))
	for i := range out {
		out[i] = DocID(i)
	}
	return out
}

// topByCosine is the exact scorer the partition is judged against — the same
// metric scorer/vector computes, kept in the test so this file never depends on
// the scorer it exists to make cheaper.
func topByCosine(vs [][]float32, q []float32, ids []DocID, k int) []DocID {
	type scored struct {
		id DocID
		s  float64
	}
	all := make([]scored, 0, len(ids))
	for _, id := range ids {
		v := vs[id]
		if len(v) == 0 {
			continue
		}
		all = append(all, scored{id, cosine(q, v)})
	}
	slices.SortFunc(all, func(a, b scored) int {
		switch {
		case a.s > b.s:
			return -1
		case a.s < b.s:
			return 1
		}
		return int(a.id) - int(b.id)
	})
	out := make([]DocID, 0, k)
	for _, s := range all[:min(k, len(all))] {
		out = append(out, s.id)
	}
	return out
}

// TestIVFIsNotBuiltWhereItCannotPay is the ivfMinDocs arithmetic, asserted
// rather than left in a comment.
//
// Building a partition costs a scan of the corpus several times over. Below the
// threshold the query it saves is smaller than the build that made it, so the
// answer is the same one an empty segment gives: no partition, and the reader
// falls back to every id it holds.
func TestIVFIsNotBuiltWhereItCannotPay(t *testing.T) {
	dim := 8
	tests := []struct {
		name  string
		count int
		dim   int
		vs    [][]float32
	}{
		{"one under the floor", ivfMinDocs - 1, dim, clusteredCorpus(ivfMinDocs-1, dim, 5)},
		{"no vectors at all", ivfMinDocs * 2, 0, make([][]float32, ivfMinDocs*2)},
		{"empty corpus", 0, dim, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := buildIVF(tc.count, tc.dim, vecAt(tc.vs))
			if b.nlist != 0 {
				t.Errorf("nlist = %d, want 0", b.nlist)
			}
			if len(b.lists) != 0 {
				t.Errorf("%d lists were built for an unpartitioned segment", len(b.lists))
			}
		})
	}
}

// TestIVFListCountFollowsSqrt pins the one number that decides both the build
// cost and the query cost. The upper clamp is not decoration: the build's
// centroid assignment is linear in nlist, so an unclamped sqrt would make a
// billion-document segment cost a thousand times what a million-document one
// does per vector.
func TestIVFListCountFollowsSqrt(t *testing.T) {
	tests := []struct {
		count int
		want  int
	}{
		{0, 1},
		{1, 1},
		{4096, 64},
		{4097, 65},
		{10000, 100},
		{148232, 386},
		{1 << 30, ivfMaxList},
	}
	for _, tc := range tests {
		if got := ivfNList(tc.count); got != tc.want {
			t.Errorf("ivfNList(%d) = %d, want %d", tc.count, got, tc.want)
		}
	}
}

// TestIVFOrderIsBestFirstAndTotal is the contract Nearest's nprobe escalation
// rests on: the ranking covers every list, so widening the probe is taking a
// longer prefix rather than searching again.
func TestIVFOrderIsBestFirstAndTotal(t *testing.T) {
	dim := 4
	nlist := 5
	cent := []float32{
		1, 0, 0, 0,
		0, 1, 0, 0,
		0, 0, 1, 0,
		0, 0, 0, 1,
		-1, 0, 0, 0,
	}
	order := ivfOrder(cent, nlist, dim, []float32{0.9, 0.1, 0, 0})
	if len(order) != nlist {
		t.Fatalf("order covers %d of %d lists", len(order), nlist)
	}
	if order[0] != 0 || order[1] != 1 {
		t.Errorf("order = %v, want list 0 then list 1 in front", order)
	}
	if order[len(order)-1] != 4 {
		t.Errorf("order = %v, want the opposing centroid last", order)
	}
	seen := map[int]bool{}
	for _, j := range order {
		if seen[j] {
			t.Fatalf("order = %v names a list twice", order)
		}
		seen[j] = true
	}
}
