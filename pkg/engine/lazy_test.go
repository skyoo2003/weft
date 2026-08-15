// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

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

// TestOpenStillRefusesDamageInsideAUnit guards the boundary against moving too
// far. Skipping the frame checksum decides *where* verification happens; it is
// not permission to load a corrupt record.
func TestOpenStillRefusesDamageInsideAUnit(t *testing.T) {
	dir, segDir, _ := commitSeeded(t)
	path := filepath.Join(segDir, docsFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A byte of document text, which no semantic rule re-derives — a unit
	// checksum is the only thing that can see it.
	b[segHeaderLen+6] ^= 0xff
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open with a damaged document record: got %v, want ErrCorrupt", err)
	}
}
