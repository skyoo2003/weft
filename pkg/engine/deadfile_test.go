// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tombstone file is a trust boundary like every other section: its bytes come
// off disk, and what they decide is which documents a query is allowed to see.
// The failure this file is about is therefore not a crash — it is a plausible
// wrong answer. A count that disagrees with the manifest, a repeated id, or one
// past the corpus each produce an index that opens cleanly and then hides, or
// fails to hide, the wrong documents.

// deadPayload frames a handcrafted tombstone payload as a section file and
// returns a reader over it, the way openSection would.
func deadPayload(t *testing.T, dir string, payload func(w *segWriter)) *segReader {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	name := deadFileName(1)
	w, err := newSegWriter(root, name, kindDead)
	if err != nil {
		t.Fatal(err)
	}
	payload(w)
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	r, err := parseSection(name, b, kindDead)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTombstoneListRefusesWhatNoWriterProduces(t *testing.T) {
	tests := []struct {
		name    string
		want    int // what the manifest claims
		total   int // documents in the corpus
		payload func(w *segWriter)
		msg     string
	}{
		{
			// The manifest's count and the file's are written by one commit and
			// read by different paths, so a disagreement means one of them is not
			// describing this index — and the manifest's is the one that decided
			// this file had to be read at all.
			name: "count disagrees with the manifest", want: 3, total: 10,
			payload: func(w *segWriter) { uvs(w, 2, 1, 1) },
			msg:     "holds 2 tombstones",
		},
		{
			// A repeat would be counted twice in deadSet.n, so the live document
			// count would be one too low for the life of the index.
			name: "a repeated id", want: 3, total: 10,
			payload: func(w *segWriter) { uvs(w, 3, 1, 2, 0) },
			msg:     "repeats document",
		},
		{
			// An id past the corpus is damage, or a tombstone file copied in from
			// a larger index — where it hides whichever documents land on those
			// ids here.
			name: "an id past the corpus", want: 2, total: 5,
			payload: func(w *segWriter) { uvs(w, 2, 1, 9) },
			msg:     "of a 5-document index",
		},
		{
			// More tombstones than there are documents cannot describe any index,
			// and it is worth its own check because it bounds the allocation
			// under it.
			name: "more tombstones than documents", want: 6, total: 5,
			payload: func(w *segWriter) { uvs(w, 6, 0, 1, 1, 1, 1, 1) },
			msg:     "in a 5-document index",
		},
		{
			// Truncated mid-value: the count promises entries the payload does
			// not carry.
			name: "fewer entries than the count", want: 3, total: 10,
			payload: func(w *segWriter) { uvs(w, 3, 1, 2) },
			msg:     "truncated",
		},
		{
			// Trailing bytes mean these were not written by this encoder, and
			// accepting them would let two files mean one tombstone set.
			name: "trailing bytes", want: 2, total: 10,
			payload: func(w *segWriter) { uvs(w, 2, 1, 2, 7) },
			msg:     "trailing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := deadPayload(t, t.TempDir(), tt.payload)
			_, err := parseDead(r, tt.want, tt.total)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("parseDead: got %v, want ErrCorrupt", err)
			}
			if !strings.Contains(err.Error(), tt.msg) {
				t.Errorf("parseDead: %v\nwant a message naming %q", err, tt.msg)
			}
		})
	}
}

// TestTombstoneListRoundTrips is the honest payload, so the refusals above are
// refusing something rather than everything.
func TestTombstoneListRoundTrips(t *testing.T) {
	// Crossing the bitmap's word boundary three times: 63/64/65 is where an
	// off-by-one in the shift would show, and 100000 is far enough out to need
	// the slice to have grown.
	want := []DocID{0, 1, 63, 64, 65, 4095, 100000}
	r := deadPayload(t, t.TempDir(), func(w *segWriter) { encodeDead(w, want) })
	got, err := parseDead(r, len(want), 100001)
	if err != nil {
		t.Fatalf("parseDead: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("parseDead returned %d ids, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("id %d: got %d, want %d", i, got[i], want[i])
		}
	}
	var d deadSet
	for _, id := range got {
		d.mark(id, 1)
	}
	for _, id := range want {
		if !d.has(id) {
			t.Errorf("deadSet lost document %d", id)
		}
	}
	for _, id := range []DocID{2, 62, 66, 4094, 99999} {
		if d.has(id) {
			t.Errorf("deadSet claims document %d, which was never marked", id)
		}
	}
	// And all() is the inverse of the walk that built it, which is what Commit
	// republishes the set through.
	if back := d.all(); len(back) != len(want) {
		t.Fatalf("all() returned %d ids, want %d", len(back), len(want))
	}
}

// TestALostTombstoneFileIsCorruptionRatherThanAnEmptySet is the resurrection
// case stated as a test.
//
// Nothing names this file, so its absence has to be caught by the count the
// manifest carries — otherwise a bad prune or a partial copy of an index
// directory is indistinguishable from an index nobody deleted anything from, and
// every document it named comes back.
func TestALostTombstoneFileIsCorruptionRatherThanAnEmptySet(t *testing.T) {
	dir := t.TempDir()
	ix := New()
	addAll(t, ix, []Document{{Key: "a", Text: "x"}, {Key: "b", Text: "y"}})
	if !ix.Delete("a") {
		t.Fatal("Delete: false")
	}
	if err := ix.Commit(t.Context(), dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deads, err := filepath.Glob(filepath.Join(dir, deadPrefix+"*"))
	if err != nil || len(deads) != 1 {
		t.Fatalf("Glob(%s*) = %v, %v; want exactly one tombstone file", deadPrefix, deads, err)
	}
	if err := os.Remove(deads[0]); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Open with the tombstone file removed: got %v, want ErrCorrupt — the deleted document is back", err)
	}
	if err := Scrub(dir); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Scrub with the tombstone file removed: got %v, want ErrCorrupt", err)
	}
}

// TestAnEmptyTombstoneFileIsStillRequired is the same resurrection argument at
// the one size that reads as innocent.
//
// A commit with nothing deleted still writes the file and still counts zero, and
// a reader that took the zero as permission not to look would put "nothing was
// deleted" and "the file is gone" back into one state — the ambiguity the file
// was written to remove. Every version 4 generation has one, so the manifest's
// version is what decides whether to read it and never the count.
func TestAnEmptyTombstoneFileIsStillRequired(t *testing.T) {
	build := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		ix := New()
		addAll(t, ix, []Document{{Key: "a", Text: "x"}, {Key: "b", Text: "y"}})
		if err := ix.Commit(t.Context(), dir); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := ix.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		deads, err := filepath.Glob(filepath.Join(dir, deadPrefix+"*"))
		if err != nil || len(deads) != 1 {
			t.Fatalf("Glob(%s*) = %v, %v; want one tombstone file even with nothing deleted", deadPrefix, deads, err)
		}
		return deads[0]
	}
	check := func(t *testing.T, dir, how string) {
		t.Helper()
		if ix, err := Open(dir); !errors.Is(err, ErrCorrupt) {
			if err == nil {
				ix.Close() //nolint:errcheck // cleaning up after a failure
			}
			t.Errorf("Open with the empty tombstone file %s: got %v, want ErrCorrupt", how, err)
		}
		if err := Scrub(dir); !errors.Is(err, ErrCorrupt) {
			t.Errorf("Scrub with the empty tombstone file %s: got %v, want ErrCorrupt", how, err)
		}
	}

	t.Run("removed", func(t *testing.T) {
		name := build(t)
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
		check(t, filepath.Dir(name), "removed")
	})

	t.Run("damaged", func(t *testing.T) {
		name := build(t)
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		// The last byte is inside the frame checksum — the part of an empty file
		// there is nothing else to catch, and precisely what a reader that skipped
		// the file on a zero count would never look at.
		b[len(b)-1] ^= 0x01
		if err := os.WriteFile(name, b, 0o644); err != nil {
			t.Fatal(err)
		}
		check(t, filepath.Dir(name), "damaged")
	})
}

// TestAFlippedTombstoneByteIsCaught. The frame checksum is verified eagerly for
// this section, unlike the ones inside a segment, so damage anywhere in it is
// refused rather than surfacing as a wrong candidate set.
func TestAFlippedTombstoneByteIsCaught(t *testing.T) {
	dir := t.TempDir()
	ix := New()
	for _, k := range []string{"a", "b", "c", "d"} {
		if _, err := ix.Add(Document{Key: k, Text: "x " + k}); err != nil {
			t.Fatal(err)
		}
	}
	if !ix.Delete("b") || !ix.Delete("d") {
		t.Fatal("Delete: false")
	}
	if err := ix.Commit(t.Context(), dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deads, err := filepath.Glob(filepath.Join(dir, deadPrefix+"*"))
	if err != nil || len(deads) != 1 {
		t.Fatalf("Glob(%s*) = %v, %v", deadPrefix, deads, err)
	}
	orig, err := os.ReadFile(deads[0])
	if err != nil {
		t.Fatal(err)
	}
	for i := range orig {
		b := append([]byte(nil), orig...)
		b[i] ^= 0x01
		if err := os.WriteFile(deads[0], b, 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := Open(dir)
		if err == nil {
			got.Close() //nolint:errcheck // cleaning up after a failure
			t.Errorf("byte %d flipped and Open succeeded", i)
			continue
		}
		if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrBadVersion) {
			t.Errorf("byte %d flipped: got %v, want ErrCorrupt or ErrBadVersion", i, err)
		}
	}
}
