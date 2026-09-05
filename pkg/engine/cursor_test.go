// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

// drain walks a cursor to the end and returns everything it yielded.
func drain(t *testing.T, c *BlockCursor) []Posting {
	t.Helper()
	var out []Posting
	buf := make([]Posting, blockSize)
	for blk := c.Next(buf); blk != nil; blk = c.Next(buf) {
		if len(blk) > blockSize {
			t.Fatalf("a block yielded %d postings, above the block size %d", len(blk), blockSize)
		}
		out = append(out, blk...)
	}
	if err := c.Err(); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	return out
}

// cursorCorpus commits enough documents to span several blocks and leaves some
// pending on top, so one walk crosses a mapped segment and the in-memory one.
func cursorCorpus(t *testing.T, committed, pending int) *Index {
	t.Helper()
	dir := t.TempDir()
	ix := New()
	add := func(from, n int) {
		for i := from; i < from+n; i++ {
			// "all" is in every document, "third" in one in three, "solo" in one.
			text := "all"
			if i%3 == 0 {
				text += " third"
			}
			if i == 7 {
				text += " solo"
			}
			if _, err := ix.Add(Document{Key: fmt.Sprintf("d%05d", i), Text: text}); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
	}
	add(0, committed)
	if err := ix.Commit(context.Background(), dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	t.Cleanup(func() { _ = ix.Close() })

	opened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	ix = opened
	add(committed, pending)
	return opened
}

// TestACursorAgreesWithLookup is the check that licenses the cursor at all.
//
// It is a **second walker of the postings format**, and the format document says
// what that costs: two decoders for one format is how a format drifts. The
// answer here is that they are not two decoders — both go through decodeBlock —
// but they are two walkers, and the walking is where a block boundary, a segment
// boundary, a delta chain restart and a tombstone filter can each be got wrong
// in a way no checksum sees. Lookup is the reference.
func TestACursorAgreesWithLookup(t *testing.T) {
	// Several blocks in the committed half, and a pending tail that is not a
	// whole block, so the last piece of the walk is short on both sides.
	ix := cursorCorpus(t, 5*blockSize+13, 40)

	for _, term := range ix.Terms("", 100) {
		t.Run(term, func(t *testing.T) {
			want := ix.Lookup(term)
			got := drain(t, ix.BlockCursor(term))
			if !slices.Equal(got, want) {
				t.Errorf("cursor yielded %d postings, Lookup %d; first difference at %d",
					len(got), len(want), firstDiff(got, want))
			}
		})
	}
	if got := drain(t, ix.BlockCursor("neverindexed")); got != nil {
		t.Errorf("a cursor over an absent term yielded %v", got)
	}
}

func firstDiff(a, b []Posting) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// TestACursorDoesNotYieldDeletedDocuments is the bug this nearly shipped with.
//
// A tombstoned document keeps its posting, its length and its record — only the
// index knows it is gone — so a cursor that skipped the filter would put deleted
// documents in front of every scorer built on it and nothing downstream could
// tell. Every other read path takes tombstones off inside the read that decoded
// them, and this one has to as well.
func TestACursorDoesNotYieldDeletedDocuments(t *testing.T) {
	ix := cursorCorpus(t, 3*blockSize, 10)
	for i := range 3*blockSize + 10 {
		if i%2 == 0 {
			ix.Delete(fmt.Sprintf("d%05d", i))
		}
	}
	for _, term := range []string{"all", "third"} {
		want, got := ix.Lookup(term), drain(t, ix.BlockCursor(term))
		if !slices.Equal(got, want) {
			t.Errorf("term %q: cursor yielded %d postings, Lookup %d", term, len(got), len(want))
		}
		for _, p := range got {
			if _, live := ix.Doc(p.Doc); !live {
				t.Errorf("term %q: cursor yielded deleted document %d", term, p.Doc)
			}
		}
	}
}

// TestACursorSurvivesAWholeBlockOfTombstones is the case the filter makes
// possible and the loop has to answer. A block every posting of which is deleted
// decodes to nothing, and nothing is what a caller reads as "the term is
// finished" — so the walk has to take the next block rather than return it.
func TestACursorSurvivesAWholeBlockOfTombstones(t *testing.T) {
	// Delete exactly the first block's worth, so the term continues past a hole
	// the width of one block.
	ix := cursorCorpus(t, 3*blockSize, 0)
	for i := range blockSize {
		ix.Delete(fmt.Sprintf("d%05d", i))
	}
	want, got := ix.Lookup("all"), drain(t, ix.BlockCursor("all"))
	if len(want) != 2*blockSize {
		t.Fatalf("Lookup returned %d postings, want %d — the fixture is wrong", len(want), 2*blockSize)
	}
	if !slices.Equal(got, want) {
		t.Errorf("cursor yielded %d postings across a fully deleted block, want %d", len(got), len(want))
	}
}

// TestACursorGrowsAShortBuffer. The buffer is a performance contract and not a
// correctness one — Next says so — so a caller passing a small one gets the
// right answer slowly rather than a truncated block.
func TestACursorGrowsAShortBuffer(t *testing.T) {
	ix := cursorCorpus(t, 2*blockSize, 0)
	want := ix.Lookup("all")

	var got []Posting
	buf := make([]Posting, 1)
	c := ix.BlockCursor("all")
	for blk := c.Next(buf); blk != nil; blk = c.Next(buf) {
		got = append(got, blk...)
	}
	if err := c.Err(); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("with a one-posting buffer the cursor yielded %d postings, want %d", len(got), len(want))
	}
}

// TestACursorReportsASegmentThatWentAway pins the difference between "the term
// ended" and "the term could not be read", which Next's nil cannot carry.
//
// Close is the reachable way to take a segment out from under a live cursor; a
// merge is the one that happens in production and it removes the segment the
// same way. Either is a short posting list if it goes unreported, and a short
// posting list is not a smaller answer.
func TestACursorReportsASegmentThatWentAway(t *testing.T) {
	ix := cursorCorpus(t, 3*blockSize, 0)
	c := ix.BlockCursor("all")
	buf := make([]Posting, blockSize)
	if blk := c.Next(buf); len(blk) == 0 {
		t.Fatal("the first block yielded nothing")
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if blk := c.Next(buf); blk != nil {
		t.Errorf("the cursor yielded %d postings after the segments went away", len(blk))
	}
	if c.Err() == nil {
		t.Error("the cursor stopped without saying why")
	}
}
