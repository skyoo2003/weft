// SPDX-License-Identifier: Apache-2.0

package engine

import "fmt"

// BlockCursor walks one term's postings a block at a time, ascending by DocID.
//
// # What it is for
//
// Lookup and LookupInto materialise a term's whole list. For a scorer that walks
// one term to exhaustion before looking up the next, that is right and
// LookupInto's buffer reuse makes it cheap. For a scorer that walks *several*
// terms in step — document-at-a-time, which is what lets a query hold a k-sized
// result and nothing else — it is not: every list has to be live at once, so the
// cost becomes the sum of the lists rather than the longest of them.
//
// docs/FINDINGS.md milestone 22 is the measurement that made this necessary.
// Against bleve, weft was faster on every query shape and allocated **156×
// more** on the widest — 2.82 MB against 18 KB — and every byte of the
// difference was a structure sized by the corpus rather than by the answer. A
// cursor is what makes the largest of them, the posting list itself, sized by a
// block instead.
//
//	c := ix.BlockCursor(term)
//	var buf [128]Posting
//	for blk := c.Next(buf[:]); blk != nil; blk = c.Next(buf[:]) { … }
//	if err := c.Err(); err != nil { … }
//
// # What it costs, and the two things it is not
//
// One read lock per block rather than one per term. A term held by fifty
// thousand documents is about four hundred blocks, so that is four hundred
// acquisitions against one — against the corpus walk it replaces, this is not
// the term that matters.
//
// **It is not a snapshot of the postings.** The extents are read once, at
// construction, and the bytes are read per block under a fresh lock. So a merge
// that lands mid-walk can take the segment a cursor was part-way through, and
// the cursor stops and says so through Err rather than returning a short list
// that looks complete. A short posting list is not a smaller answer, it is a
// wrong one — the same rule D-006 gives corruption on this path.
//
// **It is not safe for concurrent use**, and it is not meant to outlive the
// query that built it.
type BlockCursor struct {
	ix   *Index
	term string

	// segs is where this term's postings live, taken once. It holds numbers and
	// not pointers, which is the whole reason a cursor can exist at all: a
	// *segment holds a memory mapping that Close unmaps, and a cursor keeping one
	// across a lock release would be a segmentation fault rather than an error.
	// A base is a stable name for a segment that Next re-resolves each time.
	segs []cursorSpan
	si   int // which of them
	bi   int // which block within it
	off  int // payload-relative byte offset of that block
	prev uint64

	// pend is the pending segment's postings for this term, aliased the way
	// Lookup aliases them. setPosting and dropPosting both replace the slice
	// rather than editing it, so this stays valid for as long as it is held.
	pend []Posting

	// r is reused across blocks rather than built per block. A segReader carries
	// an eight-byte scratch array for the checksum seed, so it escapes to the
	// heap, and a term of four hundred blocks was four hundred of them — about a
	// third of what a query allocated once the corpus-sized structures were gone.
	// It holds a []byte into a mapping, so it is re-pointed under the lock on
	// every block and never read outside one.
	r segReader

	err error
}

// cursorSpan is one segment's contribution to a term, named by numbers only.
type cursorSpan struct {
	base    DocID
	off     int // payload-relative offset of the first block
	end     int // payload-relative end of the term's entry
	nblocks int
}

// BlockCursor returns a cursor over term's postings.
//
// A term no segment claims and nothing pending yields a cursor whose first Next
// returns nil, which is the answer Lookup's nil already is and needs no branch
// of its own.
func (ix *Index) BlockCursor(term string) *BlockCursor {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	c := &BlockCursor{ix: ix, term: term, pend: ix.postings[term]}
	for _, s := range ix.segs {
		sp, ok := s.terms[term]
		if !ok {
			continue
		}
		// The same two bounds scanPostings checks before it decodes anything,
		// made here so a damaged extent is refused once rather than per block.
		if sp.off < segHeaderLen || sp.off-segHeaderLen > len(s.postings) ||
			sp.end < sp.off || sp.end-segHeaderLen > len(s.postings) {
			continue
		}
		r := &segReader{name: postingsFile, b: s.postings, off: sp.off - segHeaderLen}
		nblocks, err := r.intn("block count", len(r.b))
		if err != nil || nblocks == 0 {
			continue
		}
		c.segs = append(c.segs, cursorSpan{
			base: s.base, off: r.off, end: sp.end - segHeaderLen, nblocks: nblocks,
		})
	}
	return c
}

// Next decodes the next block into buf and returns the postings, or nil when
// there are none left or the walk stopped early.
//
// The result is buf resliced, so it is the caller's and is overwritten by the
// next call. buf should hold blockSize postings; a shorter one is grown by
// append, which defeats the point and is why the doc comment above sizes it.
//
// DocIDs are index-wide, so a caller never sees a segment-local one — the same
// contract Lookup has.
func (c *BlockCursor) Next(buf []Posting) []Posting {
	if c.err != nil {
		return nil
	}
	for c.si < len(c.segs) {
		sp := c.segs[c.si]
		if c.bi >= sp.nblocks {
			c.si, c.bi = c.si+1, 0
			continue
		}
		if c.bi == 0 {
			c.off, c.prev = sp.off, 0
		}
		out := c.block(sp, buf)
		if c.err != nil {
			return nil
		}
		// A block every posting of which was a tombstone decodes to nothing, and
		// that is not the end of the term — it is a block with no live documents
		// in it. Returning it would end the caller's walk on a list that
		// continues, which is a short posting list wearing the face of a complete
		// one. So the loop takes the next block instead.
		if len(out) > 0 {
			return out
		}
	}
	// The pending segment last, which keeps the whole walk ascending for free:
	// segments are ordered by base and pending starts past all of them. It has no
	// blocks, so it is handed out in block-sized pieces to keep one shape.
	if len(c.pend) == 0 {
		return nil
	}
	// Under the lock, because the tombstone set is index state and reading it is
	// what every other read path does inside one.
	c.ix.mu.RLock()
	defer c.ix.mu.RUnlock()
	filter := !c.ix.dead.empty()
	for len(c.pend) > 0 {
		n := min(len(c.pend), blockSize)
		out := buf[:0]
		for _, p := range c.pend[:n] {
			if filter && c.ix.dead.has(p.Doc) {
				continue
			}
			out = append(out, p)
		}
		c.pend = c.pend[n:]
		// A piece that was entirely tombstones is not the end of the term, so the
		// loop takes the next one rather than reporting exhaustion.
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// block decodes one block of one segment, under its own read lock.
func (c *BlockCursor) block(sp cursorSpan, buf []Posting) []Posting {
	c.ix.mu.RLock()
	defer c.ix.mu.RUnlock()

	s := c.segFor(sp.base)
	if s == nil {
		// A merge replaced the run this segment was in, or the index was closed.
		// Either way the rest of this term is unreachable, and a walk that
		// stopped here would hand back a prefix that looks like the whole list.
		c.err = fmt.Errorf("term %q: the segment at base %d went away mid-walk: %w", c.term, sp.base, ErrCorrupt)
		return nil
	}
	c.r.name, c.r.b, c.r.off = postingsFile, s.postings, c.off
	out := buf[:0]
	// Tombstones come off here, inside the read that decoded them, which is where
	// every other read path in this package takes them off — see lookupAt. A
	// cursor that handed them back would put deleted documents in front of every
	// scorer built on it, and nothing downstream could tell: a tombstoned
	// document has a posting, a length and a record, and only the index knows it
	// is gone.
	//
	// The check is hoisted for an index that has never deleted anything, which is
	// both the common case and what every published figure was measured on.
	filter := !c.ix.dead.empty()
	if _, err := decodeBlock(&c.r, c.term, c.bi, sp.nblocks, s.count, s.offs, &c.prev,
		func(p Posting) {
			if filter && c.ix.dead.has(p.Doc+s.base) {
				return
			}
			out = append(out, p)
		}); err != nil {
		c.err = err
		return nil
	}
	c.off = c.r.off
	c.bi++
	// The last block has to land exactly on the entry's end. A block count naming
	// fewer blocks than were written leaves every block it does name intact and
	// verifying, so this is the only place the omission is visible — the same
	// check decodeTermPostings makes after its loop.
	if c.bi == sp.nblocks && c.off != sp.end {
		c.err = fmt.Errorf("term %q holds %d blocks ending at %d, its entry runs to %d: %w",
			c.term, sp.nblocks, segHeaderLen+c.off, segHeaderLen+sp.end, ErrCorrupt)
		return nil
	}
	// Index-wide on the way out, which is where scanPostings does it too.
	for i := range out {
		out[i].Doc += s.base
	}
	return out
}

// segFor finds the segment a cursor remembered by base. Requires ix.mu.
//
// By base and not by position: the slice is rebuilt by every commit and every
// merge, so an index into it is a name that stops meaning what it meant. A base
// is what a segment *is* — the first DocID it owns — and two segments cannot
// share one, because the manifest's bases tile the id space.
func (c *BlockCursor) segFor(base DocID) *segment {
	for _, s := range c.ix.segs {
		if s.base == base {
			return s
		}
	}
	return nil
}

// Err reports why the walk stopped, or nil if it ran out of postings.
//
// It has to be asked. Next returning nil means "no more", and the difference
// between "the term ended" and "the term could not be read" is not visible in
// that value — which is the reason this method exists rather than the walk
// simply ending.
func (c *BlockCursor) Err() error { return c.err }
