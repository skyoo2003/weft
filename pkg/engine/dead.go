// SPDX-License-Identifier: Apache-2.0

package engine

import "math/bits"

// This file is deletion's whole in-memory state: which documents Delete has
// removed, and the two running totals the collection statistics are computed
// from.
//
// A set of DocIDs rather than of Keys, and that is the decision the rest of the
// milestone rests on. Doc has to answer for an id without decoding the record
// that carries its key, so a key-shaped tombstone would make every point read
// pay for the very decode the lazy reader exists to avoid. Ids are also stable
// in a way keys are not asked to be: Merge concatenates adjacent segments and
// renumbers nothing, so an id marked today still names the same document after
// every merge that will ever run over it.
//
// What is deliberately absent is any way to un-mark. The set only grows, which
// is what makes "a re-added key gets a fresh id" the only spelling of an update
// rather than one of two.

// deadSet is the tombstone set: a bitmap over DocID, plus what the statistics
// need so that neither is a walk.
//
// The zero value is an empty set and every method works on it, which is what
// keeps a zero-value Index usable the way Add already promises.
type deadSet struct {
	// bits is indexed by id/64. It is grown to reach the highest id ever marked
	// and never to the size of the corpus, so an index with one tombstone holds
	// one word rather than a bit per document.
	bits []uint64

	// n and tokens are what Stats and AvgDocLen subtract. Maintained here rather
	// than derived on demand because BM25 asks for both once per query and the
	// derivation is a walk of the set with a docoff lookup per member.
	n      int
	tokens int
}

// empty reports whether anything has been deleted. Callers that would otherwise
// filter a whole posting list check this first: an index that has never had a
// document removed must pay a branch per lookup rather than a branch per
// posting, which is what keeps the milestone 8 and 9 figures the property of the
// code that earned them.
func (d *deadSet) empty() bool { return d.n == 0 }

// has reports whether id has been deleted.
//
// The n == 0 test is first and is not redundant with the bounds check below: it
// is the same fast path empty() offers, taken by the point reads that cannot
// hoist it out of a loop. Bounds are compared in uint64 for the reason
// Index.Doc gives — DocID is uint32, and on a 32-bit build int(id) wraps
// negative at 1<<31.
func (d *deadSet) has(id DocID) bool {
	if d.n == 0 {
		return false
	}
	w := uint64(id) / 64
	if w >= uint64(len(d.bits)) {
		return false
	}
	return d.bits[w]&(1<<(uint64(id)%64)) != 0
}

// mark records id as deleted and reports whether this call was the one that did
// it. docLen is the document's token count, taken by the caller *before* the
// mark lands, because every accessor that could supply it afterwards answers for
// a deleted document the way it answers for an id that was never assigned.
//
// A second mark of the same id changes nothing and returns false, so a caller
// deleting a key twice cannot make the statistics drift.
func (d *deadSet) mark(id DocID, docLen int) bool {
	w := uint64(id) / 64
	if w >= uint64(len(d.bits)) {
		// Grown to reach this id and no further. append rather than make+copy
		// because the growth is amortised and the common shape is a set that
		// gains a word every sixty-four deletions.
		d.bits = append(d.bits, make([]uint64, w+1-uint64(len(d.bits)))...)
	}
	bit := uint64(1) << (uint64(id) % 64)
	if d.bits[w]&bit != 0 {
		return false
	}
	d.bits[w] |= bit
	d.n++
	d.tokens += docLen
	return true
}

// all lists the deleted ids in ascending order.
//
// Ascending because that is the order the on-disk section records them in, and
// because a caller checking the set against a segment's id range walks the
// segments in the same direction.
func (d *deadSet) all() []DocID {
	if d.n == 0 {
		return nil
	}
	out := make([]DocID, 0, d.n)
	for w, word := range d.bits {
		for word != 0 {
			// The lowest set bit, cleared as it is taken: a word holding one
			// tombstone costs one iteration rather than sixty-four.
			b := word & -word
			out = append(out, DocID(uint64(w)*64+uint64(bits.TrailingZeros64(b))))
			word ^= b
		}
	}
	return out
}
