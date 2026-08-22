// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unsafe"
)

// postingSize is what one decoded Posting costs, taken from the type rather than
// written down: a hand-written 16 is wrong the moment the struct gains a field,
// and the budget below is a multiple of it.
const postingSize = uint64(unsafe.Sizeof(Posting{}))

// assertReadAPIsAgree compares two indexes through the read methods a scorer
// uses, rather than field by field.
//
// assertIndexEqual reads internal state, which was right while an opened index
// was a rebuilt copy of the committed one. It stops being right here: a lazily
// opened index holds mapped segments and empty in-memory maps, so field
// equality would fail on two indexes that answer every question identically.
// What a caller can observe is the contract. The representation is not.
func assertReadAPIsAgree(t *testing.T, want, got *Index) {
	t.Helper()
	if got.Len() != want.Len() {
		t.Fatalf("Len = %d, want %d", got.Len(), want.Len())
	}
	wn, wavg := want.Stats()
	gn, gavg := got.Stats()
	if gn != wn || gavg != wavg {
		t.Errorf("Stats = %d/%v, want %d/%v", gn, gavg, wn, wavg)
	}
	if got.AvgDocLen() != want.AvgDocLen() {
		t.Errorf("AvgDocLen = %v, want %v", got.AvgDocLen(), want.AvgDocLen())
	}

	for i := range want.Len() {
		id := DocID(i)
		w, wok := want.Doc(id)
		g, gok := got.Doc(id)
		if wok != gok {
			t.Fatalf("Doc(%d) present = %v, want %v", id, gok, wok)
		}
		if g.Key != w.Key || g.Text != w.Text {
			t.Errorf("Doc(%d) = %q/%q, want %q/%q", id, g.Key, g.Text, w.Key, w.Text)
		}
		if !slices.Equal(g.Vector, w.Vector) {
			t.Errorf("Doc(%d).Vector = %v, want %v", id, g.Vector, w.Vector)
		}
		if !slices.Equal(g.Links, w.Links) {
			t.Errorf("Doc(%d).Links = %v, want %v", id, g.Links, w.Links)
		}
		if !g.Time.Equal(w.Time) || g.Time.IsZero() != w.Time.IsZero() {
			// recency reads IsZero as "no opinion", so zeroness has to survive
			// as a meaning and not merely as an instant.
			t.Errorf("Doc(%d).Time = %v, want %v", id, g.Time, w.Time)
		}
		if got.DocLen(id) != want.DocLen(id) {
			t.Errorf("DocLen(%d) = %d, want %d", id, got.DocLen(id), want.DocLen(id))
		}
		// Vector is a second decoder over the same record, skipping the fields it
		// does not return. Two decoders for one format is how a format drifts, so
		// the guard is that it agrees with Doc — on both sides of a commit, and on
		// documents carrying no vector, where both have to answer false.
		wv, wvok := want.Vector(id)
		gv, gvok := got.Vector(id)
		if wvok != (len(w.Vector) > 0) || gvok != (len(g.Vector) > 0) {
			t.Errorf("Vector(%d) present = %v/%v, want %v/%v (Doc's vector lengths %d/%d)",
				id, wvok, gvok, len(w.Vector) > 0, len(g.Vector) > 0, len(w.Vector), len(g.Vector))
		}
		if !slices.Equal(gv, wv) || !slices.Equal(gv, g.Vector) {
			t.Errorf("Vector(%d) = %v, want %v and Doc's %v", id, gv, wv, g.Vector)
		}
		gid, ok := got.Resolve(w.Key)
		if !ok || gid != id {
			t.Errorf("Resolve(%q) = %d, %v; want %d, true", w.Key, gid, ok, id)
		}
	}

	// Postings are compared over the terms the corpus contains, derived from the
	// documents rather than read out of want.postings.
	//
	// want.postings is the *pending* map, and every caller here commits before
	// comparing — Commit's adopt clears it. Ranging over it therefore compared
	// nothing and passed, which is the one failure mode an assertion helper must
	// not have. A mapped segment does not hand out its vocabulary, but the
	// documents are readable through the API and Tokenize is what built the
	// postings in the first place.
	terms := map[string]struct{}{}
	tokens := 0
	for i := range want.Len() {
		d, ok := want.Doc(DocID(i))
		if !ok {
			continue
		}
		tokens += want.DocLen(DocID(i))
		for _, tok := range Tokenize(d.Text) {
			terms[tok] = struct{}{}
		}
	}
	for term := range want.postings {
		terms[term] = struct{}{}
	}
	// A corpus of textless documents has no terms and that is a real case; a
	// corpus with tokens in it and no terms to compare means this derivation
	// stopped working.
	if tokens > 0 && len(terms) == 0 {
		t.Fatalf("%d tokens indexed and no terms to compare postings over", tokens)
	}
	for term := range terms {
		if !slices.Equal(got.Lookup(term), want.Lookup(term)) {
			t.Errorf("Lookup(%q) = %v, want %v", term, got.Lookup(term), want.Lookup(term))
		}
	}

	// And the answers for things that are not there.
	if _, ok := got.Doc(DocID(want.Len())); ok {
		t.Errorf("Doc(%d) answered for a document past the corpus", want.Len())
	}
	if n := got.DocLen(DocID(want.Len())); n != 0 {
		t.Errorf("DocLen past the corpus = %d, want 0", n)
	}
	if _, ok := got.Resolve("zzz-absent"); ok {
		t.Error("Resolve answered for a key that was never added")
	}
	if pl := got.Lookup("zzzabsent"); len(pl) != 0 {
		t.Errorf("Lookup of an absent term returned %d postings", len(pl))
	}
}

// These are the milestone 3 assertions about *when* a segment is verified.
//
// Milestone 2 verified everything on every Open and said so: "Decoder
// verification is O(index) per Open. Free today — Open is eager and already
// reads every byte — but milestone 3's lazy loader cannot verify what it does
// not load." That is this task. The checks do not go away; they split.
//
//	Open   frame header, manifest, meta — bounded, and read in full anyway
//	touch  the unit being read, against its own checksum
//	Scrub  everything, on request
//
// Leaving the frame checksum in Open would keep the O(index) read that lazy
// loading exists to remove, which is why its absence is asserted rather than
// merely permitted.

// damageFrameChecksum flips the four checksum bytes that close a section file,
// leaving every byte of content — and every per-unit checksum inside it —
// intact.
//
// This is the one kind of damage that isolates the question. Anything inside
// the payload is still caught by a unit checksum on first touch, so only the
// frame trailer can tell whether Open computed the whole-file checksum or not.
func damageFrameChecksum(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(b) - crc32.Size; i < len(b); i++ {
		b[i] ^= 0xff
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestOpenSkipsTheFrameChecksum pins the boundary having moved.
//
// The frame checksum covers every byte of a section, so computing it means
// reading every byte — the exact cost this milestone removes. Open must not.
func TestOpenSkipsTheFrameChecksum(t *testing.T) {
	dir, segDir, _ := commitSeeded(t)
	damageFrameChecksum(t, filepath.Join(segDir, docsFile))

	ix, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v — noticing a damaged frame checksum means Open read the whole file", err)
	}
	// And the index is still the one that was committed: the contents were
	// never touched, so nothing about the corpus may have changed.
	if got := ix.Len(); got != 4 {
		t.Fatalf("Open returned %d documents, want 4", got)
	}
}

// TestScrubCatchesWhatOpenNoLongerDoes is the other half, and the reason the
// first is not simply a hole.
//
// Milestone 2 got whole-file verification for free because Open was eager.
// Milestone 3 has to buy it explicitly, and Scrub is the price: the same
// checks, on request, rather than on every open.
func TestScrubCatchesWhatOpenNoLongerDoes(t *testing.T) {
	dir, segDir, _ := commitSeeded(t)
	if err := Scrub(dir); err != nil {
		t.Fatalf("Scrub of an undamaged index: %v", err)
	}

	damageFrameChecksum(t, filepath.Join(segDir, docsFile))
	if err := Scrub(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Scrub after damaging a frame checksum: got %v, want ErrCorrupt", err)
	}
}

// ---------------------------------------------------------------------------
// The router: Open stops decoding, the six read APIs keep their signatures.
// ---------------------------------------------------------------------------

// bulkCorpus builds a byte-heavy, vocabulary-light corpus. Same shape as the
// writer's allocation test and for the same reason: what is being measured is
// whether cost tracks the segment's *bytes*, and a large vocabulary would mix
// a second cost into the same number.
func bulkCorpus(t *testing.T, docs int) *Index {
	t.Helper()
	ix := New()
	for i := range docs {
		var sb strings.Builder
		for j := range 400 {
			fmt.Fprintf(&sb, "w%dq%d ", (i+j)%400, i%3)
		}
		if _, err := ix.Add(Document{Key: fmt.Sprintf("doc-%05d", i), Text: sb.String()}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	return ix
}

func segmentBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	for _, s := range segSections {
		fi, err := os.Stat(filepath.Join(dir, segDirName(1), s.name))
		if err != nil {
			t.Fatal(err)
		}
		total += fi.Size()
	}
	return total
}

// TestOpenDoesNotDecodeTheCorpus is the milestone's outcome sentence turned
// into a number: opening an index must not cost the size of the index.
//
// Until now Open rebuilt the whole thing — every Document, every posting list,
// the key map — so its cost was the corpus and the format's seek tables were
// written for a reader that did not exist. This is that reader.
func TestOpenDoesNotDecodeTheCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an 8 MB corpus")
	}
	ix := bulkCorpus(t, 2000)
	dir := t.TempDir()
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	onDisk := segmentBytes(t, dir)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	runtime.ReadMemStats(&after)
	defer got.Close() //nolint:errcheck // teardown
	allocated := after.TotalAlloc - before.TotalAlloc

	// A quarter of the segment. Open still reads what is bounded by the
	// vocabulary rather than the corpus — the terms index — so this is not
	// zero and is not meant to be.
	if limit := onDisk / 4; int64(allocated) > limit {
		t.Fatalf("Open allocated %d bytes for a %d-byte segment, over the %d-byte limit — it is still decoding the corpus",
			allocated, onDisk, limit)
	}
	t.Logf("segment %d bytes on disk, Open allocated %d", onDisk, allocated)
}

// TestLazyAndEagerAgreeOnEveryReadAPI is the correctness half. Reading through
// a mapping has to be indistinguishable from reading the index that was
// committed — every document, every length, every posting list, every key —
// or the memory saved above was bought with wrong answers.
//
// restore_test.go already asserts that the two rank identically. This asserts
// the layer underneath, so a disagreement reports which method disagreed rather
// than that some ranking moved.
//
// The eager side is a second index that is never committed, and that is the
// point of the test rather than a detail of it: Commit adopts the segment it
// wrote, so the index that did the committing answers through a mapping
// afterwards. Comparing it against a reopen would compare two lazy readers and
// pass without touching the question in the name.
func TestLazyAndEagerAgreeOnEveryReadAPI(t *testing.T) {
	docs := []Document{
		{Key: "delta", Text: "fusion ranking", Vector: []float32{1, 0}, Links: []string{"alpha"}},
		{Key: "alpha", Text: "fusion scorer architecture", Vector: []float32{0, 1}},
		{Key: "charlie", Text: "scorer", Links: []string{"nowhere"}},
		{Key: "bravo", Text: "ranking ranking ranking"},
	}
	want := New()
	addAll(t, want, docs)

	writer := New()
	addAll(t, writer, docs)
	dir := t.TempDir()
	if err := writer.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown

	assertReadAPIsAgree(t, want, got)
}

// TestCloseReleasesTheSegments pins what happens after the mapping is gone.
//
// Reading through an unmapped region is a segmentation fault, not a Go panic,
// and index.go promises this package does not panic. Close therefore drops the
// segments under the write lock before unmapping, so a read that races or
// arrives late gets the same answer it would for an id that was never
// assigned. Double Close is a no-op for the same reason.
func TestCloseReleasesTheSegments(t *testing.T) {
	ix := New()
	addAll(t, ix, []Document{{Key: "a", Text: "x x"}, {Key: "b", Text: "x"}})
	dir := t.TempDir()
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, ok := got.Doc(0); !ok {
		t.Fatal("document 0 missing before Close")
	}
	if err := got.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := got.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, ok := got.Doc(0); ok {
		t.Error("Doc answered after Close")
	}
	if n := got.Len(); n != 0 {
		t.Errorf("Len after Close = %d, want 0", n)
	}
	if pl := got.Lookup("x"); len(pl) != 0 {
		t.Errorf("Lookup after Close returned %d postings, want 0", len(pl))
	}
	if _, ok := got.Resolve("a"); ok {
		t.Error("Resolve answered after Close")
	}
}

// openHeap is the Go heap an opened index retains, with the corpus that built
// it dropped first.
func openHeap(t *testing.T, docs int) (heap uint64, onDisk int64) {
	t.Helper()
	dir := t.TempDir()
	func() {
		ix := bulkCorpus(t, docs)
		defer ix.Close() //nolint:errcheck // teardown
		if err := ix.Commit(dir); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}()
	onDisk = segmentBytes(t, dir)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	heap = after.HeapAlloc - before.HeapAlloc
	// Read after the measurement, so the compiler cannot decide the index was
	// dead before the second reading and collect what is being measured.
	if got.Len() != docs {
		t.Fatalf("opened %d documents, want %d", got.Len(), docs)
	}
	got.Close() //nolint:errcheck // teardown
	return heap, onDisk
}

// TestHeapDoesNotScaleWithTheCorpus is the milestone's memory pass line.
//
// It measures scaling rather than a size, which is what makes it cheap enough
// to run: eight times the corpus must not be eight times the heap. An index
// that decoded on Open would track the corpus exactly.
//
// What it does not measure is stated here rather than left for a reader to
// assume. Go's heap is not the machine's memory. Mapped pages live in the page
// cache, so this number going flat means the corpus left the Go heap — not that
// it stopped needing to be resident. On the milestone 4 corpus roughly 69% of
// the docs file is vectors and scorer/vector scans all of them per query, so
// that scan's working set is unchanged by everything in this milestone. Only an
// approximate vector index removes it, and this is the assertion that would
// otherwise read as if it had.
func TestHeapDoesNotScaleWithTheCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two corpora, the larger 8 MB")
	}
	requireLazyMapping(t)
	small, smallDisk := openHeap(t, 250)
	large, largeDisk := openHeap(t, 2000)
	t.Logf("250 docs: %d bytes on disk, %d heap; 2000 docs: %d on disk, %d heap",
		smallDisk, small, largeDisk, large)

	if largeDisk < smallDisk*7 {
		t.Fatalf("the corpora are not 8x apart on disk: %d and %d", smallDisk, largeDisk)
	}
	if large > small*2 {
		t.Errorf("8x the corpus retained %d bytes of heap against %d — Open is still holding the corpus", large, small)
	}
}

// ---------------------------------------------------------------------------
// Incremental commit: D-003's repayment.
// ---------------------------------------------------------------------------

// TestCommitIsByteDeterministic is the merge assertion's other half.
//
// A commit reads its term set out of a map too, so the same corpus committed
// twice has the same chance of producing two different files — and the same
// consequence, since posting order decides rankings. Milestone 4 section 4.2 is
// what it costs to find that out downstream.
func TestCommitIsByteDeterministic(t *testing.T) {
	build := func(t *testing.T) map[string]uint32 {
		t.Helper()
		dir := t.TempDir()
		ix := bulkCorpus(t, 200)
		defer ix.Close() //nolint:errcheck // teardown
		if err := ix.Commit(dir); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		return dirBytes(t, filepath.Join(dir, segDirName(1)))
	}
	if first, second := build(t), build(t); !maps.Equal(first, second) {
		t.Errorf("two commits of the same corpus produced different bytes:\n first %v\nsecond %v", first, second)
	}
}

// dirBytes hashes every file under dir, so "these bytes did not change" is one
// comparison rather than a walk in the test.
func dirBytes(t *testing.T, dir string) map[string]uint32 {
	t.Helper()
	out := map[string]uint32{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[rel] = crc32.Checksum(b, segCRC)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestCommitAfterOneAddWritesOneDocument is D-003 coming due.
//
// "One segment per commit, the whole corpus rewritten" was the milestone 2
// decision, marked with a ponytail comment naming this milestone as its
// repayment trigger: at in-memory scale O(corpus) per commit is irrelevant, and
// at the scale this milestone is about it means reading and writing the entire
// corpus to record one new document. The 666 MB evaluation index carries
// generation 9 — nine commits, nine full rewrites, roughly 6 GB moved to store
// 666 MB.
//
// Two things are asserted, and the second is the stronger one. The new
// generation must be small, and the previous generation's bytes must be
// untouched — a commit that rewrote the old segment identically would pass a
// size check while doing exactly the work being removed.
func TestCommitAfterOneAddWritesOneDocument(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an 8 MB corpus")
	}
	ix := bulkCorpus(t, 2000)
	dir := t.TempDir()
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	first := dirBytes(t, filepath.Join(dir, segDirName(1)))
	bulk := segmentBytes(t, dir)

	if _, err := ix.Add(Document{Key: "one-more", Text: "a single extra document"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("second Commit: %v", err)
	}

	// The first generation is still there, byte for byte.
	if got := dirBytes(t, filepath.Join(dir, segDirName(1))); !maps.Equal(got, first) {
		t.Errorf("the first generation was rewritten by the second commit:\n got %v\nwant %v", got, first)
	}

	// And the second holds one document's worth of bytes, not the corpus's.
	var second int64
	for _, s := range segSections {
		fi, err := os.Stat(filepath.Join(dir, segDirName(2), s.name))
		if err != nil {
			t.Fatalf("second generation is missing %s: %v", s.name, err)
		}
		second += fi.Size()
	}
	if limit := int64(64 << 10); second > limit {
		t.Errorf("committing one document wrote %d bytes on top of a %d-byte corpus, over the %d-byte limit",
			second, bulk, limit)
	}
	t.Logf("corpus %d bytes, one more document cost %d", bulk, second)

	// The whole corpus is still readable, and the new document with it.
	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown
	if n := got.Len(); n != 2001 {
		t.Fatalf("reopened index holds %d documents, want 2001", n)
	}
	if _, ok := got.Resolve("one-more"); !ok {
		t.Error("the document the second commit was for is not in the reopened index")
	}
	if _, ok := got.Resolve("doc-00000"); !ok {
		t.Error("a document from the first generation is missing after the second commit")
	}
	assertReadAPIsAgree(t, ix, got)
}

// ---------------------------------------------------------------------------
// Merge: bounding the segment count, deterministically.
// ---------------------------------------------------------------------------

// commitEach commits after every document, so the directory ends up with one
// generation per document — the shape merge exists to collapse.
func commitEach(t *testing.T, dir string, n int) *Index {
	t.Helper()
	ix := New()
	for i := range n {
		if _, err := ix.Add(Document{
			Key:   fmt.Sprintf("doc-%03d", i),
			Text:  fmt.Sprintf("shared term%d and more shared", i%3),
			Links: []string{fmt.Sprintf("doc-%03d", (i+1)%n)},
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if err := ix.Commit(dir); err != nil {
			t.Fatalf("Commit %d: %v", i, err)
		}
	}
	return ix
}

// TestMergeBoundsTheSegmentCount is the reason merge exists. Every commit adds
// a segment, and every read consults all of them, so an index committed often
// enough answers a point query by walking a list that grows without limit.
func TestMergeBoundsTheSegmentCount(t *testing.T) {
	dir := t.TempDir()
	ix := commitEach(t, dir, 12)
	defer ix.Close() //nolint:errcheck // teardown

	if got := len(ix.segs); got != 12 {
		t.Fatalf("12 commits left %d segments", got)
	}
	if err := ix.Merge(); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got := len(ix.segs); got > maxSegments {
		t.Errorf("Merge left %d segments, the ceiling is %d", got, maxSegments)
	}

	// The corpus is unchanged, read through the merged index and through a
	// fresh Open of the directory it published.
	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after Merge: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown
	assertReadAPIsAgree(t, ix, got)
	if got.Len() != 12 {
		t.Errorf("reopened index holds %d documents, want 12", got.Len())
	}
}

// TestMergeDoesNotMoveRankings is the correctness line merge has to hold.
//
// A merged segment renumbers nothing — merging adjacent segments is
// concatenation, so a document keeps the id it had — but the postings are
// rewritten and TopK breaks ties on DocID. Milestone 4 measured what that
// tiebreak decides on a real corpus: 241 reported slots sat at a cut score 960
// further candidates were excluded from by DocID alone. A merge that moved ids
// would move rankings, silently.
func TestMergeDoesNotMoveRankings(t *testing.T) {
	dir := t.TempDir()
	ix := commitEach(t, dir, 12)
	defer ix.Close() //nolint:errcheck // teardown

	before := map[string][]Posting{}
	docs := make([]Document, ix.Len())
	for i := range ix.Len() {
		docs[i], _ = ix.Doc(DocID(i))
	}
	for _, term := range []string{"shared", "term0", "term1", "term2", "and", "more"} {
		before[term] = slices.Clone(ix.Lookup(term))
	}
	n, avg := ix.Stats()

	if err := ix.Merge(); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	for i := range docs {
		got, ok := ix.Doc(DocID(i))
		if !ok || got.Key != docs[i].Key {
			t.Errorf("after Merge, document %d is %q; before it was %q", i, got.Key, docs[i].Key)
		}
	}
	for term, want := range before {
		if got := ix.Lookup(term); !slices.Equal(got, want) {
			t.Errorf("after Merge, Lookup(%q) = %v; before it was %v", term, got, want)
		}
	}
	if gn, gavg := ix.Stats(); gn != n || gavg != avg {
		t.Errorf("after Merge, Stats = %d/%v; before it was %d/%v", gn, gavg, n, avg)
	}
}

// TestMergeIsByteDeterministic is milestone 4 section 4.2 arriving upstream.
//
// That milestone published a number, then found the pipeline that produced it
// was nondeterministic: a Go map ranged over during a build chose a different
// winner every run, and 6.6% of the citation graph moved between builds. The
// bootstrap, the pinned seed and the 28-configuration sweep were all downstream
// of it and all blind to it.
//
// Merge is where this milestone could introduce the same thing. It reads term
// sets out of maps and writes them to disk, and what it writes decides posting
// order, which decides rankings. So: same inputs, same bytes.
func TestMergeIsByteDeterministic(t *testing.T) {
	build := func(t *testing.T) map[string]uint32 {
		t.Helper()
		dir := t.TempDir()
		ix := commitEach(t, dir, 12)
		defer ix.Close() //nolint:errcheck // teardown
		if err := ix.Merge(); err != nil {
			t.Fatalf("Merge: %v", err)
		}
		out := map[string]uint32{}
		for name, sum := range dirBytes(t, dir) {
			// The manifest names generations, and the generation number depends
			// on how many commits came first — which is the same in both runs
			// but is not what this test is about. Segment contents are.
			if strings.HasPrefix(filepath.Base(name), segPrefix) || strings.Contains(name, string(filepath.Separator)) {
				out[name] = sum
			}
		}
		return out
	}
	first, second := build(t), build(t)
	if !maps.Equal(first, second) {
		t.Errorf("two merges of the same corpus produced different bytes:\n first %v\nsecond %v", first, second)
	}
}

// TestADamagedRecordIsNeverServed guards the boundary against moving too far,
// and records what it cost to move it at all.
//
// Open no longer reads a document record, so it cannot refuse one. What must
// still hold is that nobody is ever handed a damaged document: the read that
// touches it verifies the record's checksum and reports the id as absent, and
// Scrub is what names the damage.
//
// The cost is in that sentence. Doc's signature is (Document, bool), and
// keeping it — the milestone's central claim — means corruption and "no such
// document" are the same answer at the read API. Widening Doc to return an
// error is the one change that would break every scorer, which is the trade
// this milestone exists to avoid making; docs/FINDINGS.md carries it.
func TestADamagedRecordIsNeverServed(t *testing.T) {
	dir, segDir, want := commitSeeded(t)
	path := filepath.Join(segDir, docsFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A byte of document 0's text, which no semantic rule re-derives — a unit
	// checksum is the only thing that can see it.
	b[segHeaderLen+6] ^= 0xff
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	ix, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ix.Close() //nolint:errcheck // teardown

	// Not the damaged text, not a wrong document, not a panic.
	if d, ok := ix.Doc(0); ok {
		t.Errorf("Doc(0) returned %q from a record that fails its checksum", d.Text)
	}
	// And the documents beside it are untouched: damage is contained to its
	// own unit, which is the whole reason the checksums are per unit.
	for id := 1; id < want.Len(); id++ {
		d, ok := ix.Doc(DocID(id))
		if !ok {
			t.Errorf("Doc(%d) went missing alongside a damaged neighbour", id)
			continue
		}
		if w, _ := want.Doc(DocID(id)); d.Text != w.Text {
			t.Errorf("Doc(%d).Text = %q, want %q", id, d.Text, w.Text)
		}
	}

	if err := Scrub(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Scrub with a damaged document record: got %v, want ErrCorrupt", err)
	}
}

// A term's offset is the one value the lazy path takes on trust.
//
// The eager decoder does check it: decodePostings walks the postings file in
// step with the terms file and refuses a term whose recorded offset is not
// where that walk currently sits. decodeTermIndex performs no walk — not
// walking is what makes Open lazy — so the check has nowhere to live there,
// and it has to happen where the offset is used instead.
//
// A checksum does not substitute for it. CRC32C is an integrity code, not a
// signature: whoever changes the bytes recomputes it, and this package's
// premise is that it parses files it did not write. So the offset arrives
// arbitrary, and an offset below the frame header indexes a slice negatively —
// which is a panic, in a package whose documented promise is that library code
// never panics.
//
// segment.doc guards the offset it takes from docoff. This is the same guard,
// on the sibling that did not have one.
func TestALyingTermOffsetIsNeverFollowed(t *testing.T) {
	tests := []struct {
		name string
		off  uint64
	}{
		{"before the frame header", 0},
		{"past the payload", 1 << 20},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, segDir, _ := commitSeeded(t)
			// Framed and checksummed exactly as the real writer frames it. Only
			// the offset lies, which is what makes this reachable through Open
			// rather than caught by it.
			rewriteSection(t, filepath.Join(segDir, termsFile), kindTerms, func(w *segWriter) {
				termsPayload(w, 1, termEntry{term: "fusion", off: tc.off})
			})

			ix, err := Open(dir)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer ix.Close() //nolint:errcheck // teardown

			// No postings, and no panic. Corruption reports as absence here for
			// the reason D-006 gives: Lookup has no error to return, and giving
			// it one would move every scorer.
			if pl := ix.Lookup("fusion"); len(pl) != 0 {
				t.Errorf("Lookup followed an offset of %d and returned %d postings", tc.off, len(pl))
			}

			// Detectable, and detected — on the path that reads everything.
			if err := Scrub(dir); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Scrub with a lying term offset: got %v, want ErrCorrupt", err)
			}
		})
	}
}

// TestMergeRefusesToPublishWhatItCouldNotRead is the other side of D-006's
// trade, and the one place that answer must not be taken at face value.
//
// A mapped segment reports a damaged record as absent, because Doc's signature
// is (Document, bool). A merge reading through that same API would copy the
// absence into the segment it writes — an empty-key document, or a term with no
// postings — publish it, and prune the originals. Contained damage that Scrub
// can name and a rebuild can repair would become a segment no reader accepts,
// and nothing would have reported a failure. So the merge refuses instead, and
// refuses before anything on disk is named.
func TestMergeRefusesToPublishWhatItCouldNotRead(t *testing.T) {
	dir := t.TempDir()
	ix := commitEach(t, dir, 9) // nine is the fewest that merges anything: k = 9 - maxSegments + 1
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	before, manifest := dirNames(t, dir), manifestBytes(t, dir)

	// A byte inside the first generation's only document record, where nothing
	// but the record's own checksum can see it.
	path := filepath.Join(dir, segDirName(1), docsFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[segHeaderLen+3] ^= 0xff
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown
	if _, ok := got.Doc(0); ok {
		t.Fatal("the damaged record still reads; this test is flipping the wrong byte")
	}

	if err := got.Merge(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Merge over a segment holding a damaged record: got %v, want ErrCorrupt", err)
	}
	// Nothing was swapped in memory and nothing was published on disk, so the
	// damage is still exactly where Scrub can find it and a rebuild can replace
	// it. The half-written segment is left behind unnamed, which is what a failed
	// Commit leaves too: no manifest points at it, and the next write's RemoveAll
	// clears it.
	if n := len(got.segs); n != 9 {
		t.Errorf("Merge left %d segments after refusing, want 9", n)
	}
	for _, name := range before {
		if !slices.Contains(dirNames(t, dir), name) {
			t.Errorf("a refused merge removed %s", name)
		}
	}
	if after := manifestBytes(t, dir); !slices.Equal(after, manifest) {
		t.Error("a refused merge published a new manifest")
	}
	if _, ok := got.Doc(8); !ok {
		t.Error("a document outside the damage went missing")
	}
}

// manifestBytes is the published manifest, verbatim. The commit point of both
// Commit and Merge is the rename that puts these bytes in place, so "nothing was
// published" is exactly "these bytes did not change".
func manifestBytes(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---------------------------------------------------------------------------
// Neither Merge nor Scrub may hold the corpus it walks.
// ---------------------------------------------------------------------------

// ubiquitousCorpus adds n documents that all carry the same vocabulary in full,
// so every term's posting list is as long as the corpus.
//
// The shape is the measurement rather than a convenience. Walking a segment
// costs two things that scale with different quantities — the documents decoded
// one at a time, and the posting lists held whole — and a corpus where every
// term names every document is what makes the second dominate. The terms are
// two characters each, which is the smallest a token can be and still leave 676
// of them: the fewer text bytes a posting costs, the more plainly a number taken
// here is about posting lists.
func ubiquitousCorpus(t *testing.T, ix *Index, from, n int) {
	t.Helper()
	// 200 of the 676 two-character tokens, which is enough vocabulary for the
	// terms index to be a real one and few enough that the postings dominate.
	const terms = 200
	var sb strings.Builder
	for j := range terms {
		fmt.Fprintf(&sb, "%c%c ", 'a'+j/26, 'a'+j%26)
	}
	text := sb.String()
	for i := range n {
		if _, err := ix.Add(Document{Key: fmt.Sprintf("doc-%06d", from+i), Text: text}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
}

// TestMergeDoesNotBufferPostingLists is the merge half of the milestone's memory
// claim, and the half nothing measured.
//
// Open and Commit were both pinned against the corpus. Merge was not, and it is
// the one operation that reads segments already on disk — the corpus that does
// not fit is exactly what it moves. Asking each segment for a term's complete
// posting list and concatenating those into another retained slice makes a term
// present in most documents cost the corpus, twice, on a path whose entire
// purpose is corpora too large for that.
//
// TotalAlloc rather than HeapAlloc, for the reason the writer's test gives: it
// never goes down, so a buffer allocated and then collected still shows up. It
// also bounds what was retained, since nothing can be live that was never
// allocated.
func TestMergeDoesNotBufferPostingLists(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a corpus of 800,000 postings")
	}
	requireLazyMapping(t)
	dir := t.TempDir()
	ix := New()
	defer ix.Close() //nolint:errcheck // teardown

	// One commit holding the whole corpus, then eight holding a document each.
	// The policy collapses len(segs) - maxSegments + 1 = 2 segments and the
	// oldest run is what it takes, so the segment being merged is the one that
	// holds everything. "The first committed segment can contain the entire
	// corpus" is the case this is about, and the one that reusing a buffer
	// across segments would not touch.
	ubiquitousCorpus(t, ix, 0, 4000)
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	onDisk := segmentBytes(t, dir)
	for i := range 8 {
		ubiquitousCorpus(t, ix, 4000+i, 1)
		if err := ix.Commit(dir); err != nil {
			t.Fatalf("Commit %d: %v", i, err)
		}
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := ix.Merge(); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	// Twice the segment, and the slack is named rather than left to be inferred.
	// A merge decodes every document in order to write it again, so one copy of
	// the records is a floor no amount of streaming removes, and on this corpus
	// that floor is most of the segment by itself. What the bound is here to
	// catch is a *multiple* of the corpus — the sixteen bytes a posting costs in
	// memory against the two it costs on disk, doubled again by the
	// concatenation — and that failure is nowhere near this line: holding the
	// lists allocated 108 MB over a 4 MB segment when this was written.
	if limit := onDisk * 2; int64(allocated) > limit {
		t.Fatalf("Merge allocated %d bytes over a %d-byte segment, over the %d-byte limit — it is still holding posting lists rather than streaming them",
			allocated, onDisk, limit)
	}
	t.Logf("segment %d bytes on disk, Merge allocated %d", onDisk, allocated)
}

// TestScrubDoesNotMaterializeTheSegment is the same claim for the other whole-
// corpus walk.
//
// Scrub exists because Open stopped verifying everything, and the index a caller
// runs it on is the one that is mapped rather than loaded — larger than the heap
// is the premise. Decoding that segment into a heap-resident Index to check it,
// which is what it did, means the answer to "is this index intact" can be the
// process dying. Each document, each key and each posting is checkable on its
// own, so each is checked and dropped.
//
// What it keeps is the document count's worth and not the corpus's: one key per
// document, because uniqueness is an index-wide rule Scrub carries across
// segments, and one token count per document, because the postings are checked
// against lengths read before them.
func TestScrubDoesNotMaterializeTheSegment(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a corpus of 800,000 postings")
	}
	requireLazyMapping(t)
	dir := t.TempDir()
	ix := New()
	ubiquitousCorpus(t, ix, 0, 4000)
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	onDisk := segmentBytes(t, dir)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := Scrub(dir); err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	// Same bound and same reason as the merge above: verifying a record means
	// decoding it, so one pass over the records is the floor. A second decode of
	// every one of them, or a posting list held until the segment is finished,
	// is not — and that failure is nowhere near this line either: materializing
	// the segment allocated 60 MB over a 4 MB one when this was written.
	if limit := onDisk * 2; int64(allocated) > limit {
		t.Fatalf("Scrub allocated %d bytes verifying a %d-byte segment, over the %d-byte limit — it is still materializing the segment rather than walking it",
			allocated, onDisk, limit)
	}
	t.Logf("segment %d bytes on disk, Scrub allocated %d", onDisk, allocated)
}

// ---------------------------------------------------------------------------
// The two numbers a term's postings are decoded from that nothing seals.
// ---------------------------------------------------------------------------

// TestATruncatedBlockCountIsRefused covers the one field in a term's postings
// entry that no unit checksum reaches.
//
// Every block seals itself, seeded with its own file offset, so a block that is
// read verifies. The count standing in front of them is not inside any block, so
// a flip that lowers it does not fail a checksum — it makes the decoder stop
// early and hand back the surviving prefix as if it were the whole list. The
// blocks it skipped are never touched, so their checksums protect nothing.
//
// That is a wrong answer rather than a missing one: the term's postings for
// every document past the first block silently disappear from every query, and a
// Merge reading through the same path would publish the truncation and prune the
// segment that still held the rest.
//
// The terms index is what closes it. Entries are written in sorted order with
// each term's postings laid out contiguously, so one term's entry ends exactly
// where the next one begins — and the last ends at the payload's end. A count
// that names too few blocks stops short of that boundary.
func TestATruncatedBlockCountIsRefused(t *testing.T) {
	dir := t.TempDir()
	ix := New()
	// One term, 200 documents, so its postings need two blocks — the fewest that
	// lets a lowered count still decode into a nonempty list.
	for i := range 200 {
		mustAdd(t, ix, Document{Key: fmt.Sprintf("doc-%03d", i), Text: "fusion"})
	}
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The payload is the postings file's term count, then this term's block
	// count, then the blocks. Both counts are one-byte uvarints on this corpus,
	// so the damage is a single byte and every block the count still names
	// verifies under its own checksum.
	path := filepath.Join(dir, segDirName(1), postingsFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if b[segHeaderLen] != 1 || b[segHeaderLen+1] != 2 {
		t.Fatalf("this segment holds %d terms in %d blocks, the case needs 1 and 2", b[segHeaderLen], b[segHeaderLen+1])
	}
	b[segHeaderLen+1] = 1
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown

	// Not 128 postings out of 200. A list that stops early is not a shorter
	// posting list, it is a wrong one, and nil is the answer D-006 gives to
	// damage on this path.
	if pl := got.Lookup("fusion"); len(pl) != 0 {
		t.Errorf("Lookup returned %d postings after the block count was cut from 2 to 1; the corpus holds 200", len(pl))
	}
	if err := Scrub(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Scrub with a truncated block count: got %v, want ErrCorrupt", err)
	}
}

// TestAnImpossibleFrequencyIsRefused is the lazy path's half of a rule the eager
// one already enforces.
//
// A term cannot occur in a document more often than that document has tokens.
// decodePostings knows it because it reads every term and can track what is left
// of each document's budget; decodeTermPostings reads one term and cannot — but
// the per-document bound does not need the other terms, and docoff puts the
// token count one arithmetic step away without decoding the record.
//
// Without it, a frequency the writer could never have produced reaches BM25 and
// comes back as a plausible score. That is the failure D-001 names: a number
// nobody re-derives, believed because it parsed.
func TestAnImpossibleFrequencyIsRefused(t *testing.T) {
	dir, segDir, ix := commitSeeded(t)
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Document 0 is "fusion ranking": two tokens, so "fusion" occurs once. This
	// says three, in a block whose own metadata agrees with its contents and
	// whose checksum is computed rather than corrupted — the lie is the
	// frequency alone, which is what makes it reachable through Open.
	rewriteSection(t, filepath.Join(segDir, postingsFile), kindPostings, func(w *segWriter) {
		// cnt, maxDocID, maxTF, minDocLen, then delta and freq.
		postingsPayload(w, 1, []uint64{1, 0, 3, 2, 0, 3})
	})
	// The entry sits one byte in, past the postings file's own term count.
	rewriteSection(t, filepath.Join(segDir, termsFile), kindTerms, func(w *segWriter) {
		termsPayload(w, 1, termEntry{term: "fusion", off: segHeaderLen + 1})
	})

	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown

	if pl := got.Lookup("fusion"); len(pl) != 0 {
		t.Errorf("Lookup returned %+v: a frequency of 3 in a document of 2 tokens", pl)
	}
	if err := Scrub(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Scrub with an impossible frequency: got %v, want ErrCorrupt", err)
	}
}

// TestADamagedRecordCannotAllocateTheCorpus is the cost of deciding a record is
// damaged.
//
// A record's seeded checksum is the thing that says whether its fields are
// real, and it cannot be read before the fields are: the checksum stands at the
// end of the record, and where the record ends is what decoding it establishes.
// So every length inside is acted on before anything has vouched for it, and a
// length is bounded only by whatever the decoder can see. On the lazy path that
// was the rest of the docs section — the corpus. One damaged vector width, or
// one damaged text length, therefore asks for an allocation the size of the
// index before the checksum gets the chance to refuse it, and the process dies
// instead of Doc returning false.
//
// docoff is what closes it, and it is already read: the next document's offset
// is where this record ends, so the reader is handed the record rather than the
// section, and every length inside is bounded by the record's own size.
func TestADamagedRecordCannotAllocateTheCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a corpus of a few megabytes")
	}
	const docs = 2000
	dir := t.TempDir()
	func() {
		ix := bulkCorpus(t, docs)
		defer ix.Close() //nolint:errcheck // teardown
		if err := ix.Commit(dir); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}()
	segDir := filepath.Join(dir, segDirName(1))

	// Where the writer put the first two records. The crafted record has to
	// start where the table says record 0 starts and end before record 1, which
	// is what makes it reachable through the lazy path at all.
	offs, err := parseDocOffsets(section(t, segDir, docoffFile, kindDocoff))
	if err != nil {
		t.Fatal(err)
	}
	off0, _ := offs.at(0)
	off1, _ := offs.at(1)
	path := filepath.Join(segDir, docsFile)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// A vector of a million components: four megabytes, and legal as far as the
	// old bound could tell, because a few megabytes of docs section still stood
	// behind it. The record is framed and checksummed exactly as the writer
	// frames one — the width is the only lie, which is what keeps this a test of
	// the bound rather than of the checksum.
	const width = 1 << 20
	rewriteSection(t, path, kindDocs, func(w *segWriter) {
		w.uvarint(uint64(docs)) // the same count, so record 0 still starts at off0
		if w.off() != off0 {
			t.Fatalf("record 0 starts at %d, the table says %d", w.off(), off0)
		}
		w.beginUnit(0)
		w.str("doc-00000")
		w.str("")
		w.uvarint(1) // token count
		w.uvarint(width)
		w.endUnit()
		if w.off() > off1 {
			t.Fatalf("the crafted record runs to %d, past record 1 at %d", w.off(), off1)
		}
		// The rest of the section stays: "what is left to read" is the quantity
		// the old bound used, so removing it would remove the case.
		pad := make([]byte, 4096)
		for w.off() < int(fi.Size())-4 {
			w.write(pad[:min(len(pad), int(fi.Size())-4-w.off())])
		}
	})

	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if d, ok := got.Doc(0); ok {
		t.Errorf("Doc(0) returned %q from a record claiming a %d-wide vector", d.Key, width)
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	// Refusing a record must cost the record. Four megabytes is what the width
	// asks for and the whole complaint; 64 KB is well above what decoding one
	// short record needs and well below it.
	if limit := uint64(64 << 10); allocated > limit {
		t.Errorf("refusing one damaged record allocated %d bytes, over the %d-byte limit — the width is still bounded by the section rather than the record",
			allocated, limit)
	}
	t.Logf("refusing the record allocated %d bytes", allocated)
}

// requireLazyMapping skips a memory assertion on the platforms mmap_other.go
// serves.
//
// The fallback reads each section into a retained heap slice, deliberately and
// documented as such: correctness is identical and laziness is absent. Every
// bound below is the difference between mapped bytes and heap bytes, so on those
// platforms these tests do not measure a weaker version of the claim — they
// measure the fallback, and fail for behaving exactly as documented. Skipping is
// the honest answer; asserting the fallback's own shape would be pinning a
// number nobody runs.
func requireLazyMapping(t *testing.T) {
	t.Helper()
	if !mapsLazily {
		t.Skip("this platform reads sections into the heap; see mmap_other.go")
	}
}

// TestADamagedKeyEntryCannotAllocateTheKeysFile is the record bound's sibling on
// the other seek table.
//
// A keys entry carries no checksum at all — the section's frame is the only
// thing behind it, and Open skips that — so a key length is a number acted on
// with nothing whatever vouching for it. Bounded by the rest of the section, one
// flipped byte makes Resolve ask for a string the size of the keys file, and a
// binary search visits log2(n) entries on the way to answering.
//
// The offset table bounds it, exactly as docoff bounds a document record: the
// next entry's offset is where this one stops.
func TestADamagedKeyEntryCannotAllocateTheKeysFile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an 8 MB keys section")
	}
	dir, segDir, ix := commitSeeded(t)
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Four entries, eight bytes apart, each claiming a four-megabyte key that is
	// not there — whichever the binary search visits is the damaged one, so the
	// case does not rest on which. The section is padded past that length so the
	// old bound, which was whatever remained of it, would have accepted the
	// claim.
	const claimed = 1 << 22
	const section = 8 << 20
	const spacing = 8
	rewriteSection(t, filepath.Join(segDir, keysFile), kindKeys, func(w *segWriter) {
		w.uvarint(4)
		first := w.off() + 4*docoffWidth
		for i := range 4 {
			clear(w.scratch[:docoffWidth])
			binary.LittleEndian.PutUint64(w.scratch[:8], uint64(first+i*spacing))
			w.write(w.scratch[:docoffWidth])
		}
		for range 4 {
			at := w.off()
			w.uvarint(claimed)
			for w.off() < at+spacing {
				w.write([]byte{0})
			}
		}
		pad := make([]byte, 4096)
		for w.off() < section {
			w.write(pad[:min(len(pad), section-w.off())])
		}
	})

	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if id, ok := got.Resolve("alpha"); ok {
		t.Errorf("Resolve returned %d from a keys table whose entries decode to nothing", id)
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	if limit := uint64(64 << 10); allocated > limit {
		t.Errorf("refusing a damaged key entry allocated %d bytes, over the %d-byte limit — the length is still bounded by the section rather than the entry",
			allocated, limit)
	}
	t.Logf("refusing the entry allocated %d bytes", allocated)
}

// TestLookupRefusesAPartialAnswer is the merge's rule applied where a query
// reads.
//
// Merge already refuses to publish a term whose postings one contributing
// segment could not decode, because a list missing a segment's worth of
// documents is not a shorter list, it is a wrong one. A query has the same
// problem and had the opposite answer: Lookup concatenated whatever came back,
// so a damaged posting block in one segment silently dropped its documents from
// every ranking while the other segments made the result look whole.
//
// The terms index is what makes the difference visible: it says which segments
// claim the term, so a segment that claims one and cannot decode it is damage
// rather than a segment that simply does not hold it.
func TestLookupRefusesAPartialAnswer(t *testing.T) {
	dir := t.TempDir()
	ix := New()
	// One term in two segments, so a damaged one still leaves a plausible
	// nonempty answer behind.
	mustAdd(t, ix, Document{Key: "one", Text: "fusion"})
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	mustAdd(t, ix, Document{Key: "two", Text: "fusion"})
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A byte inside the second segment's only posting block, where nothing but
	// the block's own checksum can see it.
	path := filepath.Join(dir, segDirName(2), postingsFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-8] ^= 0xff
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown

	if pl := got.Lookup("fusion"); len(pl) != 0 {
		t.Errorf("Lookup returned %+v with one of the two segments holding the term unreadable", pl)
	}
	if err := Scrub(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Scrub with a damaged posting block: got %v, want ErrCorrupt", err)
	}
}

// TestALyingBlockMinimumIsRefused is the third block-max statistic, which the
// lazy decoder read and threw away.
//
// D-001 wrote maxDocID, maxTF and minDocLen into every block before any query
// used them, on the argument that retrofitting them is a format migration while
// writing them costs three varints — and the standing hazard of that argument is
// a field that rots unread. The answer was that every decoder re-derives them
// from the block's own contents. Two of the three were. The third was read to
// advance the reader and discarded, so a block whose minimum disagreed with the
// documents its postings name stayed wrong through every Lookup and every Merge
// that touched it, until somebody ran Scrub.
//
// docoff is what makes re-deriving it as cheap as the other two: the token count
// is arithmetic on a mapped table, and the decoder already reads it for the
// frequency bound beside this one.
func TestALyingBlockMinimumIsRefused(t *testing.T) {
	dir, segDir, ix := commitSeeded(t)
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Document 0 is "fusion ranking", two tokens, so the block holding its only
	// posting has minDocLen 2. This says 1 — the block's checksum is computed
	// over the lie rather than corrupted, so nothing but re-derivation catches
	// it, and maxDocID and maxTF still agree with the contents.
	rewriteSection(t, filepath.Join(segDir, postingsFile), kindPostings, func(w *segWriter) {
		// cnt, maxDocID, maxTF, minDocLen, then delta and freq.
		postingsPayload(w, 1, []uint64{1, 0, 1, 1, 0, 1})
	})
	rewriteSection(t, filepath.Join(segDir, termsFile), kindTerms, func(w *segWriter) {
		termsPayload(w, 1, termEntry{term: "fusion", off: segHeaderLen + 1})
	})

	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown

	if pl := got.Lookup("fusion"); len(pl) != 0 {
		t.Errorf("Lookup returned %+v from a block recording minDocLen 1 over a 2-token document", pl)
	}
	if err := Scrub(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Scrub with a lying block minimum: got %v, want ErrCorrupt", err)
	}
}

// TestLookupDoesNotCopyPostingsItWasAlreadyHanded prices the merge on the shape every
// published figure was measured on.
//
// lookupAt asks each claiming segment for its postings and merges what comes back, and
// the merge is a copy: `out = append(out, pl...)`. With one segment claiming the term and
// nothing pending, that copy has nothing to merge — the caller gets a second
// materialisation of a list it had already been handed, plus whatever append's growth
// left behind.
//
// One segment is not an edge case here. `make eval-data` builds exactly one, so it is the
// shape behind every number in docs/PERF.md, and a common term on that corpus is tens of
// thousands of postings — per term, per query, forty queries in flight.
//
// The floor is the decoded list itself, which the caller has to get. The budget is that
// plus a quarter: enough for the map's own bookkeeping, not enough for a second copy.
func TestLookupDoesNotCopyPostingsItWasAlreadyHanded(t *testing.T) {
	const docs = 20000
	ix := New()
	for i := range docs {
		// Two terms a document: one every document shares, one nothing else has. The
		// shared one is what a query asks for; the unique one keeps the vocabulary
		// honest so the postings block is not the whole segment.
		if _, err := ix.Add(Document{Key: fmt.Sprintf("doc-%05d", i), Text: fmt.Sprintf("common u%d", i)}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	dir := t.TempDir()
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	open, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer open.Close() //nolint:errcheck // nothing left to do about it in a test

	// One lookup first: whatever the mapping and the terms index set up once is not what
	// this measures.
	if got := open.Lookup("common"); len(got) != docs {
		t.Fatalf("Lookup returned %d postings, want %d", len(got), docs)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := open.Lookup("common")
	runtime.ReadMemStats(&after)
	used := after.TotalAlloc - before.TotalAlloc

	floor := uint64(len(got)) * postingSize
	if budget := floor + floor/4; used > budget {
		t.Errorf("Lookup allocated %d bytes for %d postings whose decoded form is %d: "+
			"the list is materialised more than once (budget was %d)",
			used, len(got), floor, budget)
	}
}
