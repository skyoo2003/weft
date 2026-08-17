// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Index.Nearest is the one name milestone 3b adds to the public surface, and
// the golden API file records that as a deliberate edit.
//
// What it promises is narrow on purpose: the DocIDs worth scoring exactly, at
// least k of them when the index holds that many vectors, and no score. The
// metric stays in scorer/vector — D-008 — so the engine knows which documents
// are geometrically plausible and the scorer knows how close each one is. The
// tests below are that sentence, clause by clause.

// commitGenerations commits one generation per entry in sizes, so a test can
// build the multi-segment shapes Nearest and Merge have to answer across.
func commitGenerations(t *testing.T, dir string, sizes []int, dim, groups int) *Index {
	t.Helper()
	total := 0
	for _, n := range sizes {
		total += n
	}
	vs := clusteredCorpus(total, dim, groups)
	ix := New()
	next := 0
	for g, n := range sizes {
		for range n {
			if _, err := ix.Add(Document{
				Key:    fmt.Sprintf("doc-%06d", next),
				Text:   fmt.Sprintf("cluster term%d shared", next%7),
				Vector: vs[next],
			}); err != nil {
				t.Fatalf("Add %d: %v", next, err)
			}
			next++
		}
		if err := ix.Commit(dir); err != nil {
			t.Fatalf("Commit generation %d: %v", g+1, err)
		}
	}
	return ix
}

// TestNearestOffersAtLeastK is the whole reason Nearest takes a k.
//
// nprobe is a constant and it is not exposed, so a caller has no way to widen
// the search itself. The contract that replaces the knob is this one: ask for k
// and get at least k, whatever the partition's shape — Nearest walks further
// down its own ranking of lists until it has them. Without it a query for ten
// documents could come back with three because three lists happened to be
// nearly empty, and the caller would have no way to tell that from a corpus
// holding three vectors.
func TestNearestOffersAtLeastK(t *testing.T) {
	const dim = 8
	ix, _ := commitVectors(t, ivfMinDocs, dim, 12)
	defer ix.Close() //nolint:errcheck // teardown

	q := make([]float32, dim)
	q[0] = 1
	for _, k := range []int{1, 10, 100, 1000, ivfMinDocs, ivfMinDocs * 2} {
		got := ix.Nearest(q, k)
		if want := min(k, ix.Len()); len(got) < want {
			t.Errorf("Nearest(k=%d) offered %d candidates, want at least %d", k, len(got), want)
		}
		if len(got) > ix.Len() {
			t.Errorf("Nearest(k=%d) offered %d candidates from a %d-document index", k, len(got), ix.Len())
		}
	}
}

// TestNearestNarrowsTheScan is the milestone's outcome sentence at the level a
// test can see it: a vector query must stop touching the whole corpus.
//
// The size of the reduction is measured against the real corpus by
// `weft-eval recall`; what this pins is that there is one at all, which is the
// difference between an index that has a partition and one that writes a
// section nothing reads.
//
// The corpus is large because nprobe is a constant. nlist grows as √n, so a
// fixed 64 lists is most of a small segment and a fraction of a large one — that
// is the whole reason the constant is a constant, and it means a small corpus
// cannot show narrowing even when the partition is working perfectly. At 65,536
// documents nlist is 256, so the probe is a quarter of the lists and there is
// something to measure.
func TestNearestNarrowsTheScan(t *testing.T) {
	if testing.Short() {
		t.Skip("commits a 65,536-document corpus")
	}
	const dim = 8
	// ivfMinDocs is 4·nprobe² = 16,384, so this is the 65,536 the comment above
	// computes nlist = 256 from. It was ivfMinDocs*16, which is 262,144 and four
	// times the corpus the surrounding prose, the skip message and narrow_test.go
	// all describe.
	ix, _ := commitVectors(t, ivfMinDocs*4, dim, 40)
	defer ix.Close() //nolint:errcheck // teardown

	q := make([]float32, dim)
	q[0] = 1
	got := len(ix.Nearest(q, 10))
	if got >= ix.Len()/2 {
		t.Errorf("a query for 10 documents offered %d candidates out of %d; the partition is not narrowing anything",
			got, ix.Len())
	}
	t.Logf("%d candidates out of %d documents (%.1f%%)", got, ix.Len(), 100*float64(got)/float64(ix.Len()))
}

// TestNearestOffersOnlyIDsThatResolve is the guard against the failure a
// candidate list makes possible in the first place: an id nobody can look up.
// The scorer takes what Nearest hands it straight to Doc, so a stale or
// out-of-range id would read as a document that is not there — silently
// dropping a result rather than reporting anything.
func TestNearestOffersOnlyIDsThatResolve(t *testing.T) {
	const dim = 8
	dir := t.TempDir()
	// Three committed generations and a pending one, so the walk crosses every
	// kind of segment boundary Nearest has to.
	ix := commitGenerations(t, dir, []int{ivfMinDocs, 40, 7}, dim, 12)
	defer ix.Close() //nolint:errcheck // teardown
	addAll(t, ix, []Document{
		{Key: "pending-a", Text: "still in memory", Vector: make([]float32, dim)},
		{Key: "pending-b", Text: "still in memory too"},
	})

	q := make([]float32, dim)
	q[0] = 1
	got := ix.Nearest(q, 10)
	if len(got) == 0 {
		t.Fatal("Nearest offered nothing")
	}
	if !slices.IsSorted(got) {
		t.Error("candidates are not ascending; the docs file is laid out in DocID order and a caller decoding them pays for the jumps")
	}
	seen := map[DocID]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("candidate %d offered twice; the caller would score it twice and report it twice", id)
		}
		seen[id] = true
		if _, ok := ix.Doc(id); !ok {
			t.Fatalf("candidate %d is not a document this index holds", id)
		}
	}
}

// TestNearestIncludesPendingDocuments is the case a partition cannot cover.
//
// Documents added since the last commit are in no segment and therefore in no
// inverted list, and a query that skipped them would make a vector search
// silently stale by exactly one commit's worth of ingest. The pending segment is
// small by construction — it is what a commit flushes — so offering all of it is
// the same fallback the other three unpartitioned cases take.
func TestNearestIncludesPendingDocuments(t *testing.T) {
	const dim = 8
	dir := t.TempDir()
	ix := commitGenerations(t, dir, []int{ivfMinDocs}, dim, 12)
	defer ix.Close() //nolint:errcheck // teardown

	before := ix.Len()
	addAll(t, ix, []Document{{Key: "arrived-late", Text: "not committed yet", Vector: make([]float32, dim)}})

	q := make([]float32, dim)
	q[0] = 1
	if got := ix.Nearest(q, 10); !slices.Contains(got, DocID(before)) {
		t.Errorf("the document added since the last commit is not a candidate; a vector query would be one commit stale")
	}
}

// TestNearestOnAWrongWidthOffersEverything is the disguise this design refuses
// to wear.
//
// A query whose width is not the corpus's cannot be compared to any centroid, so
// the tempting answer is to skip those segments. That turns "you mixed embedding
// models" into an empty result — which is the exact failure scorer/vector's
// ErrDimMismatch exists to make impossible, and its comment says so. Handing the
// ids over means the scorer decodes a document, sees the widths differ, and
// reports it to the caller who can fix it.
func TestNearestOnAWrongWidthOffersEverything(t *testing.T) {
	const dim = 8
	ix, _ := commitVectors(t, ivfMinDocs, dim, 12)
	defer ix.Close() //nolint:errcheck // teardown

	if got := ix.Nearest(make([]float32, dim+1), 10); len(got) != ix.Len() {
		t.Errorf("a query of the wrong width offered %d of %d documents; a narrowed answer would disguise a model mismatch as a thin result",
			len(got), ix.Len())
	}
}

// TestNearestOnAnEmptyIndexIsEmpty pins the boundary a scorer relies on for its
// early return: no documents, no candidates, no panic.
func TestNearestOnAnEmptyIndexIsEmpty(t *testing.T) {
	ix := New()
	if got := ix.Nearest([]float32{1, 0}, 10); len(got) != 0 {
		t.Errorf("an empty index offered %d candidates", len(got))
	}
	var zero Index
	if got := zero.Nearest([]float32{1, 0}, 10); len(got) != 0 {
		t.Errorf("the zero-value index offered %d candidates", len(got))
	}
}

// ---------------------------------------------------------------------------
// Merge is the converter.
// ---------------------------------------------------------------------------

// TestMergeUpgradesAV2SegmentToV3 is what lets the two-version reader be the
// whole migration story.
//
// FORMAT.md section 7.6 asks a version 3 for a converter or a reader that
// understands both. The reader is the answer, and this is why that answer is not
// "v2 segments stay v2 forever": every merge rewrites the run it collapses with
// the current writer, so a v2 generation is upgraded by the ordinary maintenance
// an index already performs. No migration command, and no directory that has to
// be rebuilt.
func TestMergeUpgradesAV2SegmentToV3(t *testing.T) {
	const dim = 8
	dir := t.TempDir()
	sizes := make([]int, maxSegments+1)
	// The two oldest are what a merge collapses, and they have to be big enough
	// between them for the result to carry a partition — otherwise this would
	// assert the file exists and prove nothing about its contents.
	sizes[0], sizes[1] = ivfMinDocs/2, ivfMinDocs/2
	for i := 2; i < len(sizes); i++ {
		sizes[i] = 2
	}
	ix := commitGenerations(t, dir, sizes, dim, 12)
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeToV2(t, dir)

	v2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open a version 2 directory: %v", err)
	}
	defer v2.Close() //nolint:errcheck // teardown
	for i, s := range v2.segs {
		if s.ivf.nlist != 0 {
			t.Fatalf("segment %d claims a partition before the merge", i)
		}
	}
	if err := v2.Merge(); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// The replacement is version 3 and carries a partition; the segments it did
	// not touch are still version 2 and still readable, which is the point of
	// merging the oldest run rather than rewriting the directory.
	if got := v2.segs[0].ivf.nlist; got == 0 {
		t.Errorf("the merged segment carries no partition, so the merge did not write it as version 3")
	}
	merged := filepath.Join(dir, segDirName(uint64(len(sizes)+1)))
	if _, err := os.Stat(filepath.Join(merged, ivfFile)); err != nil {
		t.Errorf("the merged segment has no %s section: %v", ivfFile, err)
	}
	if err := Scrub(dir); err != nil {
		t.Errorf("Scrub after a merge that upgraded a version 2 segment: %v", err)
	}

	// And the corpus is the corpus. A merge that moved a ranking would be worse
	// than one that failed.
	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after Merge: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown
	assertReadAPIsAgree(t, v2, got)
}

// TestMergeIsByteDeterministicWithVectors extends the existing merge determinism
// assertion to the one structure that could have broken it.
//
// The rest of the writer is deterministic by construction — sorted term order,
// positional ids. k-means is the first thing in this format whose output depends
// on how it was computed rather than only on what it was computed from, which is
// why the algorithm has no RNG in it at all.
func TestMergeIsByteDeterministicWithVectors(t *testing.T) {
	const dim = 8
	build := func() map[string]uint32 {
		dir := t.TempDir()
		sizes := make([]int, maxSegments+1)
		sizes[0], sizes[1] = ivfMinDocs/2, ivfMinDocs/2
		for i := 2; i < len(sizes); i++ {
			sizes[i] = 2
		}
		ix := commitGenerations(t, dir, sizes, dim, 12)
		defer ix.Close() //nolint:errcheck // teardown
		if err := ix.Merge(); err != nil {
			t.Fatalf("Merge: %v", err)
		}
		return dirBytes(t, filepath.Join(dir, segDirName(uint64(len(sizes)+1))))
	}
	first, second := build(), build()
	if first[ivfFile] != second[ivfFile] {
		t.Errorf("two merges of one corpus wrote different %s sections: %08x and %08x",
			ivfFile, first[ivfFile], second[ivfFile])
	}
}

// TestMergeRefusesAPartitionItCouldNotRead keeps the partition inside the rule
// every other section already obeys: a merge publishes nothing built out of
// bytes that would not read.
//
// The build reads each document to reach its vector, and a mapped segment
// answers a damaged record with absence — which here would mean a document
// quietly left out of every list of the replacement, with the original pruned.
// mergedSource collects that failure and Merge checks it before naming anything.
func TestMergeRefusesAPartitionItCouldNotRead(t *testing.T) {
	dir := t.TempDir()
	ix := commitEach(t, dir, maxSegments+1)
	defer ix.Close() //nolint:errcheck // teardown

	// Damage a record in the oldest segment, which is inside the run the merge
	// will collapse, leaving the frame checksum repaired so only the record's own
	// seeded checksum stands.
	path := filepath.Join(dir, segDirName(1), docsFile)
	flipAndRepairFrame(t, path, segHeaderLen+3)

	if err := ix.Merge(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Merge over a damaged record: got %v, want ErrCorrupt", err)
	}
}
