// SPDX-License-Identifier: Apache-2.0

package engine

import (
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
	r, err := openSection(root, name, kind)
	if err != nil {
		t.Fatalf("openSection(%s): %v", name, err)
	}
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
	if offs.n != len(ix.docs) {
		t.Fatalf("docoff holds %d entries, corpus has %d documents", offs.n, len(ix.docs))
	}

	docsR := section(t, segDir, docsFile, kindDocs)
	for id := range ix.docs {
		off, ok := offs.at(DocID(id))
		if !ok {
			t.Fatalf("docoff has no entry for document %d", id)
		}
		// The offset is absolute into the file; the reader's payload starts
		// segHeaderLen bytes in. Landing anywhere but a record start makes the
		// decode below fail or return the wrong document, which is the point.
		r := &segReader{name: docsFile, b: docsR.b, off: off - segHeaderLen}
		d, _, err := decodeDocRecord(r, 0)
		if err != nil {
			t.Fatalf("document %d at offset %d: %v", id, off, err)
		}
		if d.Key != ix.docs[id].Key {
			t.Errorf("offset %d for document %d decoded key %q, want %q", off, id, d.Key, ix.docs[id].Key)
		}
	}

	// Out-of-range ids report false rather than indexing past the table.
	if _, ok := offs.at(DocID(len(ix.docs))); ok {
		t.Errorf("docoff answered for document %d, which does not exist", len(ix.docs))
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
	if kt.n != len(ix.byKey) {
		t.Fatalf("keys holds %d entries, corpus has %d keys", kt.n, len(ix.byKey))
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
		if want, ok := ix.byKey[key]; !ok || id != want {
			t.Errorf("keys maps %q to %d; the corpus says %d (present: %v)", key, id, want, ok)
		}
	}

	// The lookup itself, including a key that is not there.
	for key, want := range ix.byKey {
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
	if limit := segHeaderLen + crc32.Size + 4 + len(ix.docs)*docoffWidth; len(b) > limit {
		t.Fatalf("docoff is %d bytes, which is more than a %d-entry fixed-width table needs (%d)", len(b), len(ix.docs), limit)
	}
}
