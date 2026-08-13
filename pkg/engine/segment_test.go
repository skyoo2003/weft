package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Round trip: Commit then Open must reproduce the index exactly.
// ---------------------------------------------------------------------------

func addAll(t *testing.T, ix *Index, docs []Document) {
	t.Helper()
	for _, d := range docs {
		if _, err := ix.Add(d); err != nil {
			t.Fatalf("Add(%q): %v", d.Key, err)
		}
	}
}

// assertIndexEqual compares every piece of state a segment carries. Field by
// field rather than reflect.DeepEqual, because time.Time's internal
// representation legitimately differs across a disk round trip (zone dropped,
// monotonic reading stripped) while still naming the same instant.
func assertIndexEqual(t *testing.T, want, got *Index) {
	t.Helper()
	if len(got.docs) != len(want.docs) {
		t.Fatalf("restored %d documents, want %d", len(got.docs), len(want.docs))
	}
	for i := range want.docs {
		w, g := want.docs[i], got.docs[i]
		if g.Key != w.Key || g.Text != w.Text {
			t.Errorf("doc %d: got %q/%q, want %q/%q", i, g.Key, g.Text, w.Key, w.Text)
		}
		if !slices.Equal(g.Vector, w.Vector) {
			t.Errorf("doc %d vector: got %v, want %v", i, g.Vector, w.Vector)
		}
		if !slices.Equal(g.Links, w.Links) {
			t.Errorf("doc %d links: got %v, want %v", i, g.Links, w.Links)
		}
		if !g.Time.Equal(w.Time) {
			t.Errorf("doc %d time: got %v, want %v", i, g.Time, w.Time)
		}
		if g.Time.IsZero() != w.Time.IsZero() {
			// recency reads IsZero as "no opinion", so zeroness must survive
			// the trip as a meaning, not merely as an instant.
			t.Errorf("doc %d: IsZero got %v, want %v", i, g.Time.IsZero(), w.Time.IsZero())
		}
	}
	if !maps.Equal(got.byKey, want.byKey) {
		t.Errorf("byKey: got %v, want %v", got.byKey, want.byKey)
	}
	if !maps.EqualFunc(got.postings, want.postings, slices.Equal) {
		t.Errorf("postings differ: got %d terms, want %d", len(got.postings), len(want.postings))
	}
	if !slices.Equal(got.docLen, want.docLen) {
		t.Errorf("docLen: got %v, want %v", got.docLen, want.docLen)
	}
	if got.totalLen != want.totalLen || got.vecDim != want.vecDim {
		t.Errorf("totalLen/vecDim: got %d/%d, want %d/%d",
			got.totalLen, got.vecDim, want.totalLen, want.vecDim)
	}
}

func TestSegmentRoundTrip(t *testing.T) {
	refNow := time.Date(2026, 8, 13, 10, 30, 0, 123456789, time.UTC)

	multiBlock := make([]Document, 0, 3*blockSize)
	for i := range 3 * blockSize {
		// Every document shares "common" with a varying count, so one term
		// spans several blocks with varying maxTF and minDocLen per block.
		text := "common"
		for range i % 7 {
			text += " common"
		}
		for range i % 5 {
			text += fmt.Sprintf(" filler%d", i%13)
		}
		multiBlock = append(multiBlock, Document{Key: fmt.Sprintf("doc-%04d", i), Text: text})
	}

	tests := []struct {
		name string
		docs []Document
	}{
		{"empty index", nil},
		{"single document", []Document{{Key: "only", Text: "just one"}}},
		{"typical corpus", []Document{
			{Key: "a", Text: "scorer fusion architecture", Vector: []float32{1, 0, 0}, Links: []string{"b"}, Time: refNow.Add(-time.Hour)},
			{Key: "b", Text: "fusion operator ranking", Vector: []float32{0.9, 0.1, 0}, Links: []string{"c", "missing"}, Time: refNow.Add(-100 * 24 * time.Hour)},
			{Key: "c", Text: "graph proximity scorer", Vector: []float32{0, 1, 0}, Time: refNow.Add(-2 * time.Hour)},
			{Key: "lonely", Text: "zzz"}, // zero time, no vector, no links
		}},
		{"unicode", []Document{
			{Key: "한글-키", Text: "검색 엔진 융합 検索"},
			{Key: "emoji🧵", Text: "weft weaves 씨실"},
		}},
		{"awkward times", []Document{
			{Key: "zero"},
			{Key: "epoch", Time: time.Unix(0, 0)},
			{Key: "pre-epoch", Time: time.Date(1832, 6, 1, 0, 0, 0, 42, time.UTC)},
			{Key: "nanos", Time: time.Unix(1, 999999999)},
			{Key: "far-future", Time: time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)},
			{Key: "zoned", Time: time.Date(2026, 8, 13, 9, 0, 0, 0, time.FixedZone("KST", 9*3600))},
		}},
		{"empty text next to full text", []Document{
			{Key: "mute", Vector: []float32{0.5, 0.5}},
			{Key: "loud", Text: "some actual words here", Vector: []float32{1, 0}},
		}},
		{"multi-block postings", multiBlock},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ix := New()
			addAll(t, ix, tt.docs)
			dir := t.TempDir()
			if err := ix.Commit(dir); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			got, err := Open(dir)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			assertIndexEqual(t, ix, got)
		})
	}
}

func TestReopenedIndexAcceptsAdds(t *testing.T) {
	ix := New()
	addAll(t, ix, []Document{{Key: "a", Text: "first words"}})
	dir := t.TempDir()
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := got.Add(Document{Key: "b", Text: "second words"}); err != nil {
		t.Fatalf("Add after Open: %v", err)
	}
	if _, err := got.Add(Document{Key: "a"}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("duplicate key after Open: got %v, want ErrDuplicateKey", err)
	}
}

// ---------------------------------------------------------------------------
// Lying files: checksum-valid sections whose contents violate the format's
// semantics. This is the D-001 rot check and the buggy-future-writer scenario
// — the CRC is intact, so only the decoder's own verification stands between
// a lie and a loaded index.
// ---------------------------------------------------------------------------

// commitTiny commits the corpus {a: "x x", b: "x"}: one term, two postings,
// small enough that every handcrafted variant below can state its bytes.
// For reference, the honest payloads are
//
//	meta:     2 3 0                    (docCount totalLen vecDim)
//	docs:     2 | "a" "x x" 2 0 0 t | "b" "x" 1 0 0 t
//	postings: 1 | 1  2 1 2 1  0 2 1 1
//	          (terms) (blocks) (cnt maxDoc maxTF minDL) (docID freq delta freq)
//	terms:    1 | "x" 7
//
// — the postings entry starts at payload byte 1, and terms records absolute
// file offsets, so 1 + the 6-byte header = 7.
func commitTiny(t *testing.T) (dir, segDir string) {
	t.Helper()
	ix := New()
	addAll(t, ix, []Document{{Key: "a", Text: "x x"}, {Key: "b", Text: "x"}})
	dir = t.TempDir()
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return dir, filepath.Join(dir, segDirName(1))
}

// makeSegDir creates a segment directory under dir and returns a root on it,
// which is what the writer takes: every path weft writes is resolved inside a
// root so that a symlink cannot redirect it out of the index directory.
func makeSegDir(t *testing.T, dir, name string) *os.Root {
	t.Helper()
	if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

// rewriteSection replaces one section file with handcrafted content, framed
// and checksummed exactly as the real writer frames it.
func rewriteSection(t *testing.T, path string, kind byte, payload func(w *segWriter)) {
	t.Helper()
	// newSegWriter creates exclusively — it refuses a path something already
	// stands at, which is how a planted symlink gets refused. The real writer
	// only ever writes into a directory it just made, so replacing a committed
	// file is a thing only this helper does, and it clears the way itself.
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("clearing %s: %v", path, err)
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", filepath.Dir(path), err)
	}
	defer root.Close()
	w, err := newSegWriter(root, filepath.Base(path), kind)
	if err != nil {
		t.Fatalf("newSegWriter(%s): %v", path, err)
	}
	payload(w)
	if err := w.close(); err != nil {
		t.Fatalf("close(%s): %v", path, err)
	}
}

// uvs writes a run of uvarints — most of the lying payloads below are nothing
// else.
func uvs(w *segWriter, vs ...uint64) {
	for _, v := range vs {
		w.uvarint(v)
	}
}

// docRecord writes one docs-file record with no vector, no links, and the
// epoch timestamp.
func docRecord(w *segWriter, key, text string, docLen uint64) {
	w.str(key)
	w.str(text)
	w.uvarint(docLen)
	w.uvarint(0) // vector width
	w.uvarint(0) // link count
	w.varint(0)  // seconds
	w.uvarint(0) // nanoseconds
}

func TestLyingFilesAreRefused(t *testing.T) {
	nan := math.Float32bits(float32(math.NaN()))

	tests := []struct {
		name string
		file string
		kind byte
		body func(w *segWriter)
	}{
		{"maxTF lies", postingsFile, kindPostings, func(w *segWriter) {
			uvs(w, 1, 1, 2, 1, 3 /* contents say 2 */, 1, 0, 2, 1, 1)
		}},
		{"minDocLen lies", postingsFile, kindPostings, func(w *segWriter) {
			uvs(w, 1, 1, 2, 1, 2, 2 /* contents say 1 */, 0, 2, 1, 1)
		}},
		{"maxDocID lies", postingsFile, kindPostings, func(w *segWriter) {
			// One posting for doc 0, recorded maxDocID 1.
			uvs(w, 1, 1, 1, 1, 2, 2, 0, 2)
		}},
		{"postings repeat a docID", postingsFile, kindPostings, func(w *segWriter) {
			uvs(w, 1, 1, 2, 0, 2, 1, 0, 2, 0 /* delta 0 */, 1)
		}},
		{"posting names a document past the corpus", postingsFile, kindPostings, func(w *segWriter) {
			uvs(w, 1, 1, 1, 5, 2, 1, 5 /* corpus holds 2 */, 2)
		}},
		{"zero frequency", postingsFile, kindPostings, func(w *segWriter) {
			uvs(w, 1, 1, 1, 0, 0, 2, 0, 0)
		}},
		{"frequency exceeds the document's tokens", postingsFile, kindPostings, func(w *segWriter) {
			uvs(w, 1, 1, 2, 1, 9, 1, 0, 9 /* doc 0 holds 2 tokens */, 1, 1)
		}},
		// prev is 1 and the delta is MaxUint64, so the sum wraps to 0 — a
		// document that exists, with a frequency it can hold, in a block whose
		// recorded metadata matches. Every check but the overflow guard passes,
		// and the postings come out [1, 0], descending.
		{"docID delta overflows into the corpus", postingsFile, kindPostings, func(w *segWriter) {
			uvs(w, 1, 1, 2, 0, 2, 1, 1, 1, math.MaxUint64, 2)
		}},
		// Doc 0 holds two tokens and is given one. Legal per posting — the only
		// per-posting rule is freq <= docLen — and impossible in aggregate,
		// because every token Add saw became an increment somewhere.
		{"frequencies do not add up to the document's length", postingsFile, kindPostings, func(w *segWriter) {
			uvs(w, 1, 1, 2, 1, 1, 1, 0, 1, 1, 1)
		}},
		{"non-final block not full", postingsFile, kindPostings, func(w *segWriter) {
			uvs(w, 1, 2, 1 /* first of two blocks holds 1 */)
		}},
		{"term with no blocks", postingsFile, kindPostings, func(w *segWriter) {
			uvs(w, 1, 0)
		}},
		{"terms index offset lies", termsFile, kindTerms, func(w *segWriter) {
			w.uvarint(1)
			w.str("x")
			w.uvarint(99) // the entry sits at 7
		}},
		{"empty term", termsFile, kindTerms, func(w *segWriter) {
			w.uvarint(1)
			w.str("")
			w.uvarint(7)
		}},
		{"terms index count disagrees", termsFile, kindTerms, func(w *segWriter) {
			w.uvarint(2)
			w.str("x")
			w.uvarint(7)
			w.str("y")
			w.uvarint(9)
		}},
		{"meta overstates the doc count", metaFile, kindMeta, func(w *segWriter) {
			uvs(w, 3 /* docs file holds 2 */, 3, 0)
		}},
		{"meta overstates the total length", metaFile, kindMeta, func(w *segWriter) {
			uvs(w, 2, 9 /* documents sum to 3 */, 0)
		}},
		{"meta claims vectors nobody has", metaFile, kindMeta, func(w *segWriter) {
			uvs(w, 2, 3, 1)
		}},
		{"duplicate key", docsFile, kindDocs, func(w *segWriter) {
			w.uvarint(2)
			docRecord(w, "a", "x x", 2)
			docRecord(w, "a", "x", 1)
		}},
		{"empty key", docsFile, kindDocs, func(w *segWriter) {
			w.uvarint(2)
			docRecord(w, "", "x x", 2)
			docRecord(w, "b", "x", 1)
		}},
		{"a billion nanoseconds", docsFile, kindDocs, func(w *segWriter) {
			w.uvarint(2)
			w.str("a")
			w.str("x x")
			uvs(w, 2, 0, 0)
			w.varint(0)
			w.uvarint(1_000_000_000)
			docRecord(w, "b", "x", 1)
		}},
		{"NaN vector component", docsFile, kindDocs, func(w *segWriter) {
			w.uvarint(2)
			w.str("a")
			w.str("x x")
			w.uvarint(2)
			w.uvarint(1) // 1-wide vector holding NaN
			w.write(binary.LittleEndian.AppendUint32(nil, nan))
			w.uvarint(0)
			w.varint(0)
			w.uvarint(0)
			docRecord(w, "b", "x", 1)
		}},
		{"vector widths disagree", docsFile, kindDocs, func(w *segWriter) {
			w.uvarint(2)
			w.str("a")
			w.str("x x")
			w.uvarint(2)
			w.uvarint(1) // 1-wide
			w.write(binary.LittleEndian.AppendUint32(nil, math.Float32bits(1)))
			w.uvarint(0)
			w.varint(0)
			w.uvarint(0)
			w.str("b")
			w.str("x")
			w.uvarint(1)
			w.uvarint(2) // 2-wide in the same corpus
			w.write(binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(nil, math.Float32bits(1)), math.Float32bits(1)))
			w.uvarint(0)
			w.varint(0)
			w.uvarint(0)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, segDir := commitTiny(t)
			rewriteSection(t, filepath.Join(segDir, tt.file), tt.kind, tt.body)
			if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Open: got %v, want ErrCorrupt", err)
			}
		})
	}
}

// TestBlocksStartAtAbsoluteDocIDs pins block independence, which is the whole
// point of the D-001 metadata: a skipper that had to decode every preceding
// block to learn where this one starts would be skipping nothing. Both
// directions are pinned — the bytes the writer emits, and the decoder refusing
// a block that continues the previous block's delta chain.
func TestBlocksStartAtAbsoluteDocIDs(t *testing.T) {
	ix := New()
	docs := make([]Document, blockSize+1)
	for i := range docs {
		docs[i] = Document{Key: fmt.Sprintf("d%04d", i), Text: "x"}
	}
	addAll(t, ix, docs)
	dir := t.TempDir()
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	path := filepath.Join(dir, segDirName(1), postingsFile)

	// Term "x" in every document, one token each: block 0 holds documents
	// 0..127, block 1 holds document 128. lastField is what block 1's single
	// posting records — the absolute DocID, or the delta a chained writer
	// would have emitted.
	payload := func(lastField uint64) func(*segWriter) {
		return func(w *segWriter) {
			uvs(w, 1, 2)                         // one term, two blocks
			uvs(w, blockSize, blockSize-1, 1, 1) // cnt, maxDocID, maxTF, minDocLen
			uvs(w, 0, 1)                         // document 0, frequency 1
			for range blockSize - 1 {
				uvs(w, 1, 1) // delta 1, frequency 1
			}
			uvs(w, 1, blockSize, 1, 1)
			uvs(w, lastField, 1)
		}
	}

	want := filepath.Join(t.TempDir(), "want")
	rewriteSection(t, want, kindPostings, payload(blockSize))
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, wantBytes) {
		t.Fatalf("postings file:\n got %x\nwant %x", got, wantBytes)
	}

	rewriteSection(t, path, kindPostings, payload(1)) // the chained encoding
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("second block continuing the delta chain: got %v, want ErrCorrupt", err)
	}
}

// TestOverlongVersionEncodingIsRefused pins the frame's canonical form.
// binary.Uvarint decodes 0x81 0x00 to 1 as readily as 0x01, which would put a
// seven-byte header under a file while segHeaderLen — and every absolute offset
// the terms index records against it — still says six.
func TestOverlongVersionEncodingIsRefused(t *testing.T) {
	dir, segDir := commitTiny(t)
	path := filepath.Join(segDir, metaFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := b[:len(b)-crc32.Size]

	// Same magic, same kind, same payload; version 1 spelled in two bytes.
	overlong := append([]byte(nil), segMagic...)
	overlong = append(overlong, 0x81, 0x00)
	overlong = append(overlong, body[len(segMagic)+1:]...)
	overlong = binary.LittleEndian.AppendUint32(overlong, crc32.Checksum(overlong, segCRC))
	if err := os.WriteFile(path, overlong, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open: got %v, want ErrCorrupt", err)
	}
}

// TestFrequencySumCannotWrap pins the check on each document's remaining token
// budget. Bounding a posting by docLen alone and summing afterwards leaves the
// sum itself free to overflow: a document claiming MaxInt tokens with four
// frequencies of MaxInt, MaxInt, MaxInt and 2 wraps back onto exactly MaxInt,
// so every total agrees and a posting set Add could never produce would load as
// healthy. The identity holds at any int width, so this is not a 64-bit test.
func TestFrequencySumCannotWrap(t *testing.T) {
	dir, segDir := commitTiny(t)
	mi := uint64(maxInt)

	// One document, claiming every token this platform can count.
	rewriteSection(t, filepath.Join(segDir, metaFile), kindMeta, func(w *segWriter) {
		uvs(w, 1, mi, 0)
	})
	rewriteSection(t, filepath.Join(segDir, docsFile), kindDocs, func(w *segWriter) {
		w.uvarint(1)
		docRecord(w, "a", "x", mi)
	})

	// Four terms, each with one posting in document 0. No frequency exceeds
	// docLen on its own, so only the running budget catches them.
	freqs := []uint64{mi, mi, mi, 2}
	var offs []uint64
	rewriteSection(t, filepath.Join(segDir, postingsFile), kindPostings, func(w *segWriter) {
		w.uvarint(uint64(len(freqs)))
		for _, f := range freqs {
			// off() is the absolute file offset the terms index must record.
			offs = append(offs, uint64(w.off()))
			// blocks, count, maxDocID, maxTF, minDocLen, then delta and freq.
			uvs(w, 1, 1, 0, f, mi, 0, f)
		}
	})
	rewriteSection(t, filepath.Join(segDir, termsFile), kindTerms, func(w *segWriter) {
		w.uvarint(uint64(len(freqs)))
		for i, term := range []string{"a", "b", "c", "d"} {
			w.str(term)
			w.uvarint(offs[i])
		}
	})

	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open: got %v, want ErrCorrupt", err)
	}
}

func TestUnsortedTermsAreRefused(t *testing.T) {
	// Two terms so order can be wrong: corpus {a: "x y"}. Each postings entry
	// is 7 one-byte varints, so "x" sits at absolute offset 7 and "y" at 14.
	ix := New()
	addAll(t, ix, []Document{{Key: "a", Text: "x y"}})
	dir := t.TempDir()
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	rewriteSection(t, filepath.Join(dir, segDirName(1), termsFile), kindTerms, func(w *segWriter) {
		w.uvarint(2)
		w.str("y")
		w.uvarint(7)
		w.str("x")
		w.uvarint(14)
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open: got %v, want ErrCorrupt", err)
	}
}

// ---------------------------------------------------------------------------
// Damage: any byte flip or truncation anywhere must produce ErrCorrupt —
// and never a panic. The checksum makes this exhaustively testable.
// ---------------------------------------------------------------------------

func segmentFiles(t *testing.T, dir string) []string {
	t.Helper()
	files := []string{filepath.Join(dir, manifestName)}
	for _, s := range segSections {
		files = append(files, filepath.Join(dir, segDirName(1), s.name))
	}
	return files
}

func TestEveryByteFlipIsCaught(t *testing.T) {
	dir, _ := commitTiny(t)
	for _, path := range segmentFiles(t, dir) {
		orig, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i := range orig {
			b := slices.Clone(orig)
			b[i] ^= 0xff
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("%s byte %d flipped: got %v, want ErrCorrupt",
					filepath.Base(path), i, err)
			}
		}
		if err := os.WriteFile(path, orig, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEveryTruncationIsCaught(t *testing.T) {
	dir, _ := commitTiny(t)
	for _, path := range segmentFiles(t, dir) {
		orig, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for n := range orig { // every proper prefix, empty file included
			if err := os.WriteFile(path, orig[:n], 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("%s truncated to %d bytes: got %v, want ErrCorrupt",
					filepath.Base(path), n, err)
			}
		}
		if err := os.WriteFile(path, orig, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSwappedSectionFilesAreCaught(t *testing.T) {
	// The frame's kind byte exists for exactly this: an intact file standing
	// at another section's path.
	dir, segDir := commitTiny(t)
	docs, err := os.ReadFile(filepath.Join(segDir, docsFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(segDir, metaFile), docs, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("docs file in meta's place: got %v, want ErrCorrupt", err)
	}
}

// patchVersion rewrites a file's version byte and repairs the checksum, which
// is what a file from a different-format weft looks like: healthy, just not
// this vintage.
func patchVersion(t *testing.T, path string, v byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[len(segMagic)] = v // small versions encode as one varint byte
	binary.LittleEndian.PutUint32(b[len(b)-4:], crc32.Checksum(b[:len(b)-4], segCRC))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOtherVersionsAreRefusedNotMisread(t *testing.T) {
	dir, _ := commitTiny(t)
	for _, path := range segmentFiles(t, dir) {
		orig, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		patchVersion(t, path, 2)
		if _, err := Open(dir); !errors.Is(err, ErrBadVersion) {
			t.Fatalf("%s at version 2: got %v, want ErrBadVersion", filepath.Base(path), err)
		}
		if err := os.WriteFile(path, orig, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------------------
// Fuzz: for arbitrary bytes the decoders return an error or a valid index —
// never a panic, never runaway allocation.
// ---------------------------------------------------------------------------

func fuzzHost() *Index {
	host := New()
	for i := range 3 {
		host.docs = append(host.docs, Document{Key: fmt.Sprintf("k%d", i)})
		host.docLen = append(host.docLen, 4)
		host.totalLen += 4
	}
	return host
}

func FuzzSegmentDecoding(f *testing.F) {
	ix := New()
	for _, d := range []Document{
		{Key: "a", Text: "fusion scorer fusion", Vector: []float32{1, 0}, Links: []string{"b"}, Time: time.Unix(1700000000, 42)},
		{Key: "b", Text: "graph ranking", Vector: []float32{0, 1}},
	} {
		if _, err := ix.Add(d); err != nil {
			f.Fatal(err)
		}
	}
	dir := f.TempDir()
	if err := ix.Commit(dir); err != nil {
		f.Fatal(err)
	}
	payload := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, segDirName(1), name))
		if err != nil {
			f.Fatal(err)
		}
		return b[segHeaderLen : len(b)-crc32.Size]
	}
	f.Add(payload(metaFile), payload(docsFile), payload(postingsFile), payload(termsFile))
	f.Add([]byte{}, []byte{}, []byte{}, []byte{})

	f.Fuzz(func(t *testing.T, meta, docs, postings, terms []byte) {
		// Errors are the expected outcome for almost every input; the
		// assertion is the absence of panics.
		_, _, _, _ = decodeMeta(&segReader{name: "meta", b: meta})
		host := fuzzHost()
		if ix, err := decodeDocs(&segReader{name: "docs", b: docs}); err == nil {
			host = ix // decoded docs make the most interesting postings host
		}
		_ = decodePostings(&segReader{name: "postings", b: postings}, &segReader{name: "terms", b: terms}, host)
	})
}

func FuzzParseSection(f *testing.F) {
	for _, kind := range []byte{kindMeta, kindDocs, kindPostings, kindTerms, kindManifest} {
		root, err := os.OpenRoot(f.TempDir())
		if err != nil {
			f.Fatal(err)
		}
		w, err := newSegWriter(root, "seed", kind)
		if err != nil {
			f.Fatal(err)
		}
		w.uvarint(1)
		if err := w.close(); err != nil {
			f.Fatal(err)
		}
		b, err := os.ReadFile(w.f.Name())
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b, kind)
	}
	f.Fuzz(func(t *testing.T, b []byte, kind byte) {
		_, _ = parseSection("fuzz", b, kind)
	})
}
