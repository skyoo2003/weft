// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"maps"
	"math"
	"os"
	"path/filepath"
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

// ---------------------------------------------------------------------------
// The `ivf` section, and the reader that understands two format versions.
// ---------------------------------------------------------------------------
//
// Format v3 is v2 plus one section. FORMAT.md section 7.6 asked a version 3 for
// either a converter or a reader that understands both, and this milestone owes
// the second: the fallback path a v2 segment needs — "no partition, every id is
// a candidate" — is the same path a pending segment and a small segment already
// need, so reading v2 costs almost nothing on top of code that has to exist.
//
// The risk that buys is a quiet divergence: two readers, two answers. What pins
// it is that the v2 path is not a performance path but an exactness path — it
// returns every id, so scoring over it is by definition the brute-force answer,
// and the v3 path is measured against it here rather than trusted.

// commitVectors commits n documents carrying dim-wide clustered vectors and
// returns the index and the directory it was committed to.
func commitVectors(t *testing.T, n, dim, groups int) (ix *Index, dir string) {
	t.Helper()
	vs := clusteredCorpus(n, dim, groups)
	ix = New()
	for i, v := range vs {
		if _, err := ix.Add(Document{
			Key:    fmt.Sprintf("doc-%06d", i),
			Text:   fmt.Sprintf("cluster term%d shared", i%7),
			Vector: v,
		}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	dir = t.TempDir()
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return ix, dir
}

// TestIVFSectionRoundTrips is the format half of Task 1's determinism claim:
// what the reader hands back is what the builder produced, centroid bits
// included. A float32 written and read back through a different path is where a
// format quietly loses a mantissa bit, and a centroid one bit off is a different
// partition on the next merge.
func TestIVFSectionRoundTrips(t *testing.T) {
	const dim = 8
	ix, _ := commitVectors(t, ivfMinDocs, dim, 12)
	defer ix.Close() //nolint:errcheck // teardown

	if len(ix.segs) != 1 {
		t.Fatalf("one commit left %d segments", len(ix.segs))
	}
	got := ix.segs[0].ivf
	if got.nlist == 0 {
		t.Fatalf("a %d-document segment carries no partition", ivfMinDocs)
	}

	// Rebuilt from the documents the index hands back, so the comparison is
	// against the corpus rather than against a copy of the writer's output.
	want := buildIVF(ix.Len(), dim, func(i int) []float32 {
		d, ok := ix.Doc(DocID(i))
		if !ok {
			t.Fatalf("Doc(%d) is missing", i)
		}
		return d.Vector
	})
	if got.nlist != want.nlist || got.dim != want.dim {
		t.Fatalf("section holds %d lists of width %d, the build made %d of %d",
			got.nlist, got.dim, want.nlist, want.dim)
	}
	for i := range want.centroids {
		if math.Float32bits(got.centroids[i]) != math.Float32bits(want.centroids[i]) {
			t.Fatalf("centroid component %d read back as %v, written as %v",
				i, got.centroids[i], want.centroids[i])
		}
	}
	for j := range want.nlist {
		l, ok := got.list(j)
		if !ok {
			t.Fatalf("list %d does not decode", j)
		}
		if !slices.Equal(l, want.lists[j]) {
			t.Fatalf("list %d read back with %d members, written with %d", j, len(l), len(want.lists[j]))
		}
	}
}

// TestAnUnpartitionedSegmentStillWritesTheSection is why the section list does
// not vary with the corpus.
//
// Every entry in segSections is written by every commit, so refuseForeignEntries
// and FORMAT.md's rejection table never have to say "this file may or may not be
// here". A segment too small to partition writes nlist = 0 and the reader treats
// it the way it treats a pending segment: every id is a candidate.
//
// Both causes of nlist = 0 are asserted, because they answer differently and the
// difference is easy to lose. Too small to partition still offers everything —
// nothing to narrow with, but the documents can score. Holding no vectors offers
// nothing: scorer/vector skips those documents before it compares widths, so a
// candidate list of them is a decode of the whole segment that cannot change an
// answer.
func TestAnUnpartitionedSegmentStillWritesTheSection(t *testing.T) {
	dir, segDir, ix := commitSeeded(t)
	defer ix.Close() //nolint:errcheck // teardown

	fi, err := os.Stat(filepath.Join(segDir, ivfFile))
	if err != nil {
		t.Fatalf("a four-document commit wrote no %s section: %v", ivfFile, err)
	}
	// A constant, not the corpus: the payload is two uvarints inside a frame.
	if fi.Size() > 64 {
		t.Errorf("the empty partition is %d bytes, which is not a constant", fi.Size())
	}

	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown
	if n := got.segs[0].ivf.nlist; n != 0 {
		t.Fatalf("nlist = %d for a four-document segment, want 0", n)
	}
	// Seeded with text and no vectors, so this is the vectorless half.
	if cands := got.segs[0].nearest([]float32{1, 0}, 10); len(cands) != 0 {
		t.Errorf("a segment holding no vectors offered %d candidates; every one is a record decoded to be skipped", len(cands))
	}

	// And the too-small half, which is the same nlist and the opposite answer.
	small, _ := commitVectors(t, 64, 2, 4)
	defer small.Close() //nolint:errcheck // teardown
	if n := small.segs[0].ivf.nlist; n != 0 {
		t.Fatalf("nlist = %d for a 64-document segment, want 0", n)
	}
	if cands := small.segs[0].nearest([]float32{1, 0}, 10); len(cands) != small.Len() {
		t.Errorf("a segment too small to partition offered %d of %d documents as candidates", len(cands), small.Len())
	}
}

// downgradeToV2 turns a freshly committed generation into the bytes milestone 3a
// wrote.
//
// v3 is v2 plus the ivf section and nothing else, so removing that file and
// stamping every remaining frame with version 2 produces exactly a v2 segment —
// which is what makes this a test of the reader rather than of a fixture
// somebody would have to keep in step with the format.
func downgradeToV2(t *testing.T, dir string) {
	t.Helper()
	segs, err := filepath.Glob(filepath.Join(dir, segPrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) == 0 {
		t.Fatalf("no segment directories under %s", dir)
	}
	for _, seg := range segs {
		if err := os.Remove(filepath.Join(seg, ivfFile)); err != nil {
			t.Fatal(err)
		}
		for _, s := range segSections {
			p := filepath.Join(seg, s.name)
			if _, err := os.Stat(p); err == nil {
				patchVersion(t, p, 2)
			}
		}
	}
	patchVersion(t, filepath.Join(dir, manifestName), 2)
}

// TestAV2SegmentOpensAndAnswersExactly is the dual reader, and the reason the
// milestone did not need a converter.
//
// Two claims, and they are different claims. The read APIs must agree, which is
// what says the two section lists describe one corpus. And the v2 path must
// offer every id as a candidate, which is what makes it the exact path rather
// than a degraded one — the v3 partition is then measured against it in the test
// below rather than trusted.
func TestAV2SegmentOpensAndAnswersExactly(t *testing.T) {
	const dim = 8
	v3, dir := commitVectors(t, ivfMinDocs, dim, 12)
	defer v3.Close() //nolint:errcheck // teardown

	v2dir := t.TempDir()
	copyIndexDir(t, dir, v2dir)
	downgradeToV2(t, v2dir)

	v2, err := Open(v2dir)
	if err != nil {
		t.Fatalf("Open a version 2 generation: %v — the v3 reader must read both", err)
	}
	defer v2.Close() //nolint:errcheck // teardown

	assertReadAPIsAgree(t, v3, v2)

	if got := v2.segs[0].ivf.nlist; got != 0 {
		t.Errorf("a v2 segment reports %d lists; it carries no partition at all", got)
	}
	q := make([]float32, dim)
	q[0] = 1
	if cands := v2.segs[0].nearest(q, 10); len(cands) != v2.Len() {
		t.Errorf("the v2 path offered %d of %d documents; it is the exact path and must offer all of them",
			len(cands), v2.Len())
	}
}

// TestTheTwoReadersRankTheSame is the divergence risk turned into an assertion.
//
// The v2 path scores every document and the v3 path scores a partition of them,
// so this compares an exact ranking against an approximate one. It is stable
// because the corpus is clustered and the partition's recall on it is 1.0 —
// which is the same property TestIVFRecallOnClusteredCorpus measures directly.
// A failure here on this corpus is a reader bug; the real corpus's recall is
// `weft-eval recall`'s question and not this one.
func TestTheTwoReadersRankTheSame(t *testing.T) {
	const (
		dim = 16
		k   = 10
	)
	v3, dir := commitVectors(t, ivfMinDocs*2, dim, 40)
	defer v3.Close() //nolint:errcheck // teardown

	v2dir := t.TempDir()
	copyIndexDir(t, dir, v2dir)
	downgradeToV2(t, v2dir)
	v2, err := Open(v2dir)
	if err != nil {
		t.Fatalf("Open a version 2 generation: %v", err)
	}
	defer v2.Close() //nolint:errcheck // teardown

	vs := make([][]float32, v3.Len())
	for i := range vs {
		d, _ := v3.Doc(DocID(i))
		vs[i] = d.Vector
	}

	g := splitmix(4242)
	for range 20 {
		q := slices.Clone(vs[int(g.next()%uint64(len(vs)))])
		for j := range q {
			q[j] += g.unit() * 0.05
		}
		exact := topByCosine(vs, q, v2.segs[0].nearest(q, k), k)
		approx := topByCosine(vs, q, v3.segs[0].nearest(q, k), k)
		if !slices.Equal(exact, approx) {
			t.Errorf("v2 ranked %v, v3 ranked %v", exact, approx)
		}
	}
}

// copyIndexDir copies a whole index directory, segment directories included, so
// a test can damage or downgrade one version of it while keeping the original to
// compare against. copyTree beside it copies one flat segment directory; this
// one walks, and dst is expected to exist already.
func copyIndexDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			return os.Mkdir(filepath.Join(dst, rel), 0o700)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestASegmentMixingFormatVersionsIsRefused is the check the dual reader made
// necessary.
//
// While one version was accepted, "every frame says 2" was enforced by the
// version check itself. With two accepted, a segment whose meta says 3 and whose
// docs says 2 passes every frame check individually and describes no segment any
// writer produced — and the section list is chosen from one of those numbers, so
// the reader would be applying v3's list to v2's bytes.
func TestASegmentMixingFormatVersionsIsRefused(t *testing.T) {
	for _, name := range []string{docsFile, postingsFile, termsFile, docoffFile, keysFile} {
		t.Run(name, func(t *testing.T) {
			dir, segDir, ix := commitSeeded(t)
			if err := ix.Close(); err != nil {
				t.Fatal(err)
			}
			patchVersion(t, filepath.Join(segDir, name), 2)
			if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("%s at version 2 beside a version 3 meta: got %v, want ErrCorrupt", name, err)
			}
		})
	}
}

// TestAV3SegmentMissingItsIVFSectionIsRefused is the other half of "the version
// decides the section list". A v3 segment owes seven files; six is damage, not
// an older segment.
func TestAV3SegmentMissingItsIVFSectionIsRefused(t *testing.T) {
	dir, segDir, ix := commitSeeded(t)
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(segDir, ivfFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a version 3 segment with no %s section: got %v, want ErrCorrupt", ivfFile, err)
	}
}

// TestADamagedIVFListFallsBackToTheWholeSegment is D-006 applied to a structure
// whose damage is not visible as damage.
//
// A list holds no copy of its own number, so a reader following a damaged offset
// decodes a healthy list under the wrong centroid — and the result is not an
// error, it is a plausible wrong candidate set, which is indistinguishable from
// ordinary recall loss. The seeded checksum is what makes it visible, and the
// answer once it is visible is the one D-006 gives everywhere else: behave as
// though the structure were not there. Slower, not wrong.
func TestADamagedIVFListFallsBackToTheWholeSegment(t *testing.T) {
	const dim = 8
	ix, dir := commitVectors(t, ivfMinDocs, dim, 12)
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, segDirName(1), ivfFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The last byte before the frame checksum is inside the final list's own
	// checksum, so this damages a list rather than the header or a table.
	b[len(b)-crc32.Size-1] ^= 0xff
	binary.LittleEndian.PutUint32(b[len(b)-crc32.Size:], crc32.Checksum(b[:len(b)-crc32.Size], segCRC))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open with a damaged ivf list: %v — damage in this section must cost speed, not availability", err)
	}
	defer got.Close() //nolint:errcheck // teardown

	q := make([]float32, dim)
	q[0] = 1
	// The damaged list is one of nlist, and which lists a query probes depends
	// on the query — so the assertion is over enough queries to reach it, and
	// what it demands is that reaching it never returns fewer than the whole
	// segment and never panics.
	g := splitmix(5)
	fellBack := false
	for range 200 {
		for j := range q {
			q[j] = g.unit()
		}
		n := len(got.segs[0].nearest(q, 10))
		if n == got.Len() {
			fellBack = true
		}
		if n == 0 {
			t.Fatalf("a damaged list produced an empty candidate set; absence is not a slower answer")
		}
	}
	if !fellBack {
		t.Error("no query reached the damaged list; the fallback was never exercised")
	}
	// And Scrub is what names it, which is the other half of D-006.
	if err := Scrub(dir); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Scrub on a damaged ivf list: got %v, want ErrCorrupt", err)
	}
}

// TestIVFBytesAreIdenticalAcrossTwoCommits is assertion (4) of the milestone's
// pass line, at the level it is finally stated: the same corpus committed twice
// produces the same partition byte for byte.
func TestIVFBytesAreIdenticalAcrossTwoCommits(t *testing.T) {
	build := func() map[string]uint32 {
		ix, dir := commitVectors(t, ivfMinDocs, 8, 12)
		defer ix.Close() //nolint:errcheck // teardown
		return dirBytes(t, filepath.Join(dir, segDirName(1)))
	}
	first, second := build(), build()
	if first[ivfFile] != second[ivfFile] {
		t.Errorf("two commits of one corpus wrote different %s sections: %08x and %08x",
			ivfFile, first[ivfFile], second[ivfFile])
	}
	if !maps.Equal(first, second) {
		t.Errorf("two commits of one corpus produced different bytes:\n first %v\nsecond %v", first, second)
	}
}
