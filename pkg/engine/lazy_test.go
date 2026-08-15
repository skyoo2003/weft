// SPDX-License-Identifier: Apache-2.0

package engine

import (
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
)

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
		gid, ok := got.Resolve(w.Key)
		if !ok || gid != id {
			t.Errorf("Resolve(%q) = %d, %v; want %d, true", w.Key, gid, ok, id)
		}
	}

	// Postings are compared over the terms the committed index holds, which is
	// the only side that can enumerate them — a mapped segment answers point
	// queries and does not hand out its vocabulary.
	for term := range want.postings {
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
func TestLazyAndEagerAgreeOnEveryReadAPI(t *testing.T) {
	want := New()
	addAll(t, want, []Document{
		{Key: "delta", Text: "fusion ranking", Vector: []float32{1, 0}, Links: []string{"alpha"}},
		{Key: "alpha", Text: "fusion scorer architecture", Vector: []float32{0, 1}},
		{Key: "charlie", Text: "scorer", Links: []string{"nowhere"}},
		{Key: "bravo", Text: "ranking ranking ranking"},
	})
	dir := t.TempDir()
	if err := want.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
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

// ---------------------------------------------------------------------------
// Incremental commit: D-003's repayment.
// ---------------------------------------------------------------------------

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
