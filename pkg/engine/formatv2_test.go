// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// These are the milestone 3 format assertions. Version 1 laid documents out as
// a bare run of variable-length records and rebuilt byKey by reading all of
// them, so neither a DocID nor a Key could reach its document without decoding
// every document before it. That is not a slow lazy loader, it is no lazy
// loader at all — the reason this milestone bumps the format rather than
// extending it.
//
// Both new sections are the same shape: a fixed-width table of absolute file
// offsets, then the variable-length entries those offsets point at. Fixed
// width is the whole point. A uvarint table would have to be walked from the
// front to reach entry i, which is the cost being removed.

// commitSeeded commits a corpus whose keys are deliberately not in insertion
// order, so a keys section that merely echoed DocID order would pass nothing
// here. "delta" sorts last and is added first.
func commitSeeded(t *testing.T) (dir, segDir string, ix *Index) {
	t.Helper()
	ix = New()
	addAll(t, ix, []Document{
		{Key: "delta", Text: "fusion ranking"},
		{Key: "alpha", Text: "fusion scorer architecture"},
		{Key: "charlie", Text: "scorer"},
		{Key: "bravo", Text: "ranking ranking ranking"},
	})
	dir = t.TempDir()
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return dir, filepath.Join(dir, segDirName(1)), ix
}

// section reads one committed section file and returns a reader over its
// payload, the same way Open does.
func section(t *testing.T, segDir, name string, kind byte) *segReader {
	t.Helper()
	root, err := os.OpenRoot(segDir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", segDir, err)
	}
	defer root.Close()
	r, b, err := openSection(root, name, kind, false)
	if err != nil {
		t.Fatalf("openSection(%s): %v", name, err)
	}
	// The mapping outlives the test that reads through it. Unmapping at the end
	// of the subtest would be tidier and would also turn any slice still
	// pointing into the region into a segmentation fault rather than a test
	// failure, which is a bad trade in a test.
	t.Cleanup(func() { unmapFile(b) }) //nolint:errcheck // test teardown
	return r
}

// TestVersionOneIsRefused pins the migration exemption D-007 records. weft has
// no users and no version 1 index exists outside a directory that can be
// rebuilt, so v2 refuses v1 outright rather than carrying a reader for it.
// This is the last format change that gets to make that argument.
func TestVersionOneIsRefused(t *testing.T) {
	dir, _ := commitTiny(t)
	for _, path := range segmentFiles(t, dir) {
		orig, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		patchVersion(t, path, 1)
		if _, err := Open(dir); !errors.Is(err, ErrBadVersion) {
			t.Fatalf("%s at version 1: got %v, want ErrBadVersion", filepath.Base(path), err)
		}
		if err := os.WriteFile(path, orig, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDocOffsetsLandOnRecordStarts is the assertion that makes Doc(id) a seek.
// Every entry must be the absolute file offset where that DocID's record
// begins, and decoding from there — with nothing before it read — must yield
// that document.
func TestDocOffsetsLandOnRecordStarts(t *testing.T) {
	_, segDir, ix := commitSeeded(t)

	offR := section(t, segDir, docoffFile, kindDocoff)
	offs, err := parseDocOffsets(offR)
	if err != nil {
		t.Fatalf("parseDocOffsets: %v", err)
	}
	if offs.n != ix.Len() {
		t.Fatalf("docoff holds %d entries, corpus has %d documents", offs.n, ix.Len())
	}

	docsR := section(t, segDir, docsFile, kindDocs)
	for id := range ix.Len() {
		want, _ := ix.Doc(DocID(id))
		off, ok := offs.at(DocID(id))
		if !ok {
			t.Fatalf("docoff has no entry for document %d", id)
		}
		// The offset is absolute into the file; the reader's payload starts
		// segHeaderLen bytes in. Landing anywhere but a record start makes the
		// decode below fail or return the wrong document, which is the point.
		r := &segReader{name: docsFile, b: docsR.b, off: off - segHeaderLen}
		d, _, err := decodeDocRecord(r, id)
		if err != nil {
			t.Fatalf("document %d at offset %d: %v", id, off, err)
		}
		if d.Key != want.Key {
			t.Errorf("offset %d for document %d decoded key %q, want %q", off, id, d.Key, want.Key)
		}
	}

	// Out-of-range ids report false rather than indexing past the table.
	if _, ok := offs.at(DocID(ix.Len())); ok {
		t.Errorf("docoff answered for document %d, which does not exist", ix.Len())
	}
}

// TestKeysSectionIsSortedAndAgreesWithDocs is the assertion that makes
// Resolve(key) a binary search. Sorted order is not decoration: it is what the
// search rests on, so a writer that emitted insertion order would be caught
// here rather than by a wrong lookup much later.
func TestKeysSectionIsSortedAndAgreesWithDocs(t *testing.T) {
	_, segDir, ix := commitSeeded(t)

	keysR := section(t, segDir, keysFile, kindKeys)
	kt, err := parseKeyTable(keysR)
	if err != nil {
		t.Fatalf("parseKeyTable: %v", err)
	}
	if kt.n != ix.Len() {
		t.Fatalf("keys holds %d entries, corpus has %d keys", kt.n, ix.Len())
	}

	prev := ""
	for i := range kt.n {
		key, id, err := kt.at(i)
		if err != nil {
			t.Fatalf("keys entry %d: %v", i, err)
		}
		if i > 0 && key <= prev {
			t.Fatalf("keys entry %d is %q, which does not sort after %q", i, key, prev)
		}
		prev = key
		if want, ok := ix.Resolve(key); !ok || id != want {
			t.Errorf("keys maps %q to %d; the corpus says %d (present: %v)", key, id, want, ok)
		}
	}

	// The lookup itself, including a key that is not there.
	for id := range ix.Len() {
		d, _ := ix.Doc(DocID(id))
		key, want := d.Key, DocID(id)
		got, ok, err := kt.lookup(key)
		if err != nil {
			t.Fatalf("lookup(%q): %v", key, err)
		}
		if !ok || got != want {
			t.Errorf("lookup(%q) = %d, %v; want %d, true", key, got, ok, want)
		}
	}
	if _, ok, err := kt.lookup("zzz-absent"); err != nil || ok {
		t.Errorf("lookup of an absent key returned %v, %v; want false, nil", ok, err)
	}
}

// ---------------------------------------------------------------------------
// Per-unit integrity: what is left once the frame checksum stops being read.
// ---------------------------------------------------------------------------

// flipAndRepairFrame flips one payload byte and rewrites the frame checksum, so
// the file is damaged in a way only a per-unit check can notice.
//
// This is not a contrived attack, it is what every lazy read looks like. The
// frame CRC covers the whole file, so verifying it means reading every byte —
// the cost this milestone exists to remove. A reader that maps a segment and
// touches one record has not checked the frame and never will, so a segment's
// integrity has to be checkable one unit at a time or it is not checkable at
// all.
func flipAndRepairFrame(t *testing.T, path string, i int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[i] ^= 0xff
	binary.LittleEndian.PutUint32(b[len(b)-crc32.Size:], crc32.Checksum(b[:len(b)-crc32.Size], segCRC))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestEveryByteFlipIsCaughtWithoutTheFrameCRC is TestEveryByteFlipIsCaught with
// its cheapest defence taken away.
//
// That test passes today because the frame checksum catches everything, and it
// will keep passing after the reader stops computing it — which makes it the
// wrong instrument for milestone 3. This one repairs the frame first, so the
// only thing left standing is whatever the section itself can prove about its
// own contents.
//
// Three sections, and the reason each is here is the same: nothing else
// re-derives their contents. decodePostings says outright that it never
// re-tokenizes a document to check its postings, so a flipped byte of document
// text is invisible; a term string is whatever the file says it is. keys and
// docoff are left out because they are re-derived from docs — by the scrub's own
// walk now rather than by every Open, which is where that check went.
func TestEveryByteFlipIsCaughtWithoutTheFrameCRC(t *testing.T) {
	for _, name := range []string{docsFile, postingsFile, termsFile} {
		t.Run(name, func(t *testing.T) {
			dir, segDir := commitTiny(t)
			path := filepath.Join(segDir, name)
			orig, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Payload only. The header and the trailing checksum are the frame,
			// and the frame is what this test is doing without.
			for i := segHeaderLen; i < len(orig)-crc32.Size; i++ {
				flipAndRepairFrame(t, path, i)
				// Scrub, because Open no longer decodes a payload. The frame
				// checksum is repaired, so Scrub's own whole-file check passes
				// and what is left to catch the flip is the per-unit checksum
				// — which is still exactly what this test measures.
				//
				// Errorf, not Fatalf: how many positions slip through is the
				// measurement, and stopping at the first one hides it.
				if err := Scrub(dir); !errors.Is(err, ErrCorrupt) {
					t.Errorf("%s payload byte %d flipped, frame checksum repaired: got %v, want ErrCorrupt", name, i, err)
				}
				if err := os.WriteFile(path, orig, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// TestARecordDecodedUnderTheWrongIDIsRefused is the other half, and the half
// that has nothing to do with damaged bytes.
//
// A document record does not say which document it is — its position in the
// file did, and docoff is what turns a DocID into that position. So the moment
// the offset table is trusted rather than re-derived, a single wrong offset
// yields a perfectly healthy record decoded under someone else's id: not a
// crash, not an error, a plausible wrong answer of exactly the kind
// engine.Search's precondition already warns about.
//
// Position cannot be the only thing that names a record. The id has to be
// bound into whatever the record carries to prove itself.
func TestARecordDecodedUnderTheWrongIDIsRefused(t *testing.T) {
	_, segDir, ix := commitSeeded(t)

	offs, err := parseDocOffsets(section(t, segDir, docoffFile, kindDocoff))
	if err != nil {
		t.Fatalf("parseDocOffsets: %v", err)
	}
	docsR := section(t, segDir, docsFile, kindDocs)

	const right, wrong = 2, 1
	off, ok := offs.at(right)
	if !ok {
		t.Fatalf("docoff has no entry for document %d", right)
	}
	r := &segReader{name: docsFile, b: docsR.b, off: off - segHeaderLen}
	d, _, err := decodeDocRecord(r, wrong)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("document %d's record (%q) decoded as document %d returned %q, %v; want ErrCorrupt",
			right, ix.docs[right].Key, wrong, d.Key, err)
	}
}

// TestDocOffsetTableIsFixedWidth pins the property the whole design rests on:
// entry i is at a computable position, so reaching it costs no decoding of the
// entries before it. A uvarint table would still pass every assertion above
// while making Doc(id) O(id).
func TestDocOffsetTableIsFixedWidth(t *testing.T) {
	_, segDir, ix := commitSeeded(t)
	offR := section(t, segDir, docoffFile, kindDocoff)
	offs, err := parseDocOffsets(offR)
	if err != nil {
		t.Fatalf("parseDocOffsets: %v", err)
	}
	if got, want := len(offs.tab), offs.n*docoffWidth; got != want {
		t.Fatalf("offset table is %d bytes for %d entries; fixed width says %d", got, offs.n, want)
	}
	// And the file is no larger than the table plus its own frame and count.
	b, err := os.ReadFile(filepath.Join(segDir, docoffFile))
	if err != nil {
		t.Fatal(err)
	}
	if limit := segHeaderLen + crc32.Size + 4 + ix.Len()*docoffWidth; len(b) > limit {
		t.Fatalf("docoff is %d bytes, which is more than a %d-entry fixed-width table needs (%d)", len(b), ix.Len(), limit)
	}
}

// keyIDEntry is one keys-section entry: a key and the DocID it claims.
type keyIDEntry struct {
	key string
	id  uint64
}

// keysPayload writes a keys section — count, fixed-width offset table, then the
// entries — with the ids as parameters, because the case below hands one an id
// its segment does not own and that disagreement is the thing being tested.
func keysPayload(w *segWriter, entries ...keyIDEntry) {
	w.uvarint(uint64(len(entries)))
	off := w.off() + len(entries)*docoffWidth
	for _, e := range entries {
		var b [docoffWidth]byte
		binary.LittleEndian.PutUint64(b[:], uint64(off))
		w.write(b[:])
		off += uvarintLen(uint64(len(e.key))) + len(e.key) + uvarintLen(e.id)
	}
	for _, e := range entries {
		w.str(e.key)
		w.uvarint(e.id)
	}
}

// TestAKeysEntryCannotPointOutsideItsSegment is the sibling of
// TestALyingTermOffsetIsNeverFollowed, on the other value the lazy path takes
// on trust.
//
// A keys entry carries no seeded checksum, so unlike a document record it
// cannot prove which document it names — and the id it hands back is added to
// the segment's base before anyone sees it. Unranged, a damaged entry resolves a
// key straight into a neighbouring segment's ids, and Doc then answers with a
// real document that is not the one asked for: a plausible wrong answer, which
// is the failure every seed in this format exists to rule out.
func TestAKeysEntryCannotPointOutsideItsSegment(t *testing.T) {
	dir, segDir, _ := commitSeeded(t)
	// Still four entries, still ascending, still framed and checksummed exactly
	// as the writer frames them — so the count agrees with meta and the file is
	// reachable through Open rather than refused by it. Only "alpha"'s id lies.
	rewriteSection(t, filepath.Join(segDir, keysFile), kindKeys, func(w *segWriter) {
		keysPayload(w,
			keyIDEntry{"alpha", 9}, // a four-document segment has no document 9
			keyIDEntry{"bravo", 3},
			keyIDEntry{"charlie", 2},
			keyIDEntry{"delta", 0},
		)
	})

	ix, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ix.Close() //nolint:errcheck // teardown

	if id, ok := ix.Resolve("alpha"); ok {
		t.Errorf("Resolve(alpha) = %d from an entry naming document 9 of a 4-document segment", id)
	}
	// One entry refused, not the table: the guard is on the id, at the point it
	// is used, and its neighbours are untouched.
	if id, ok := ix.Resolve("bravo"); !ok || id != 3 {
		t.Errorf("Resolve(bravo) = %d, %v; want 3, true", id, ok)
	}

	if err := Scrub(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Scrub with a keys entry naming a document outside the segment: got %v, want ErrCorrupt", err)
	}
}

// TestAKeysEntryCannotNameTheWrongDocumentInside is what ranging the id against
// the segment cannot reach.
//
// An id that stays inside the segment passes every bound there is, and Resolve
// hands back a document that exists, decodes and checksums clean — it is simply
// not the one the key belongs to. That is the plausible wrong answer of the
// sibling test above, moved one segment closer in, and the range check is blind
// to it. The record is the witness a keys entry does not carry, so resolve asks
// it: the record's own first field is its key.
func TestAKeysEntryCannotNameTheWrongDocumentInside(t *testing.T) {
	dir, segDir, _ := commitSeeded(t)
	// commitSeeded lays down delta=0, alpha=1, charlie=2, bravo=3. Only "alpha"
	// lies, and unlike the test above it lies about a document this segment
	// really holds — so nothing but the record can contradict it.
	rewriteSection(t, filepath.Join(segDir, keysFile), kindKeys, func(w *segWriter) {
		keysPayload(w,
			keyIDEntry{"alpha", 2}, // charlie's id: in range, in this segment
			keyIDEntry{"bravo", 3},
			keyIDEntry{"charlie", 2},
			keyIDEntry{"delta", 0},
		)
	})

	ix, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ix.Close() //nolint:errcheck // teardown

	if id, ok := ix.Resolve("alpha"); ok {
		d, _ := ix.Doc(id)
		t.Errorf("Resolve(alpha) = %d, which is document %q", id, d.Key)
	}
	// One entry refused, not the table, and not the document the liar named:
	// charlie's own entry is truthful and still answers.
	if id, ok := ix.Resolve("charlie"); !ok || id != 2 {
		t.Errorf("Resolve(charlie) = %d, %v; want 2, true", id, ok)
	}

	if err := Scrub(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Scrub with a keys entry naming the wrong document: got %v, want ErrCorrupt", err)
	}
}
