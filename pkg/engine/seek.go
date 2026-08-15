// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"encoding/binary"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// This file is the half of format v2 that version 1 could not express: two
// sections that let a DocID or a Key reach its document without reading the
// documents in front of it.
//
// Both are the same shape — a fixed-width table of absolute file offsets, then
// the variable-length entries those offsets point at. Everything else in this
// format is uvarint-packed and these two deliberately are not: a uvarint table
// has to be walked from the front to reach entry i, which is precisely the cost
// being removed.
//
// Offsets are absolute into the file, header included — the same convention the
// terms index already uses for postings. One convention, two places.

// docoffWidth is the byte width of one docoff entry: an absolute file offset
// and the document's token count, eight bytes each.
//
// Eight rather than four for the offset. The milestone 4 corpus already writes
// a 656 MB docs file, so a uint32 table sits one order of magnitude away from a
// corpus weft is meant to hold — and raising the ceiling later is a format
// migration, the one cost D-007 says this format does not get to pay twice.
//
// The token count rides along rather than living in the record, and that is not
// tidiness either. BM25 asks for a document's length once per posting, and a
// length reachable only by decoding the record would make every posting cost a
// key, a text and a vector. Two words per document is 2.7 MB against a 626 MiB
// docs file, and it turns DocLen into arithmetic.
//
// The record still carries its own length as well. The two agreeing is checked
// where every other derived value is checked — a copy nobody compares is the
// rot D-001 is about.
//
// The keys table uses this width too, and needs only the offset half: its upper
// eight bytes are written zero. That is eight bytes a document of padding, and
// it stays because the width is on disk — narrowing the keys table to eight is a
// format change, so it belongs to whatever bumps the version next rather than to
// a reader that would then disagree with every v2 index already written.
const docoffWidth = 16

// docoffLenAt is the offset of the token count within a docoff entry.
const docoffLenAt = 8

// ---------------------------------------------------------------------------
// writing
// ---------------------------------------------------------------------------

// encodeDocOffsets writes the docoff section: one (offset, token count) pair
// per DocID, in DocID order, so entry i sits at a computable position.
func encodeDocOffsets(w *segWriter, offs, docLen []int) {
	w.uvarint(uint64(len(offs)))
	for i, off := range offs {
		// scratch, not a local, for the reason segWriter.scratch exists: a local
		// handed to write escapes, once per document.
		binary.LittleEndian.PutUint64(w.scratch[:docoffLenAt], uint64(off))
		binary.LittleEndian.PutUint64(w.scratch[docoffLenAt:docoffWidth], uint64(docLen[i]))
		w.write(w.scratch[:docoffWidth])
	}
}

// encodeKeys writes the keys section: keys in sorted order with an offset table
// in front, so Resolve is a binary search rather than a map rebuilt by reading
// every document.
//
// The offsets are computed rather than patched back in. Every entry's encoded
// length is known from the key and the id alone, and the table's own size is
// known from the count, so the section is written front to back with nothing
// seeking backwards. That is not tidiness: milestone 3's merge cannot buffer
// O(corpus) in order to patch, so a writer that needed to would have to be
// rewritten one task later.
func encodeKeys(w *segWriter, docKeys []string) {
	// Sorted by key, carrying the DocID each key belongs to. The keys arrive in
	// DocID order from encodeDocs, which has already decoded every document —
	// asking the index for them again would decode the corpus twice.
	order := make([]int, len(docKeys))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return strings.Compare(docKeys[a], docKeys[b]) })

	w.uvarint(uint64(len(order)))
	off := w.off() + len(order)*docoffWidth
	for _, id := range order {
		// Same width as a docoff entry, and only the first eight bytes carry
		// anything: the rest is the token count docoff needs beside an offset
		// and this table does not. See the note on docoffWidth.
		clear(w.scratch[:docoffWidth])
		binary.LittleEndian.PutUint64(w.scratch[:docoffLenAt], uint64(off))
		w.write(w.scratch[:docoffWidth])
		off += keyEntryLen(docKeys[id], DocID(id))
	}
	for _, id := range order {
		w.str(docKeys[id])
		w.uvarint(uint64(id))
	}
}

// keyEntryLen is the encoded size of one keys entry. It has to agree with what
// encodeKeys writes below it; the decoder's offset check is what proves the two
// still do.
func keyEntryLen(key string, id DocID) int {
	return uvarintLen(uint64(len(key))) + len(key) + uvarintLen(uint64(id))
}

// uvarintLen is the byte count binary.AppendUvarint produces for v.
func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// ---------------------------------------------------------------------------
// reading
// ---------------------------------------------------------------------------

// docOffsets is the parsed docoff section. It holds the table bytes and never
// decodes them up front: reading entry i is arithmetic, which is the point.
type docOffsets struct {
	name string
	tab  []byte // exactly n*docoffWidth bytes
	n    int
}

// parseDocOffsets reads the count and takes the table as a slice. It does not
// look at a single offset: an offset is only verified by decoding the document
// it points at, which is the size of the corpus and therefore Scrub's. Each one
// is bounds-checked where it is used instead.
//
// The section's own frame checksum is a different cost, and Open does pay that
// one — sixteen bytes a document, scanned once, against a docs file three orders
// larger. It is what stands between a flipped token count and every BM25 score
// computed from it, because the record's own copy of that count is reachable
// only through the decode this table exists to avoid. See segSection.eager.
func parseDocOffsets(r *segReader) (docOffsets, error) {
	n, err := r.intn("document count", maxDocCount)
	if err != nil {
		return docOffsets{}, err
	}
	if n > (len(r.b)-r.off)/docoffWidth {
		return docOffsets{}, fmt.Errorf("%s: %d offsets do not fit in %d remaining bytes: %w",
			r.name, n, len(r.b)-r.off, ErrCorrupt)
	}
	d := docOffsets{name: r.name, tab: r.b[r.off : r.off+n*docoffWidth], n: n}
	r.off += n * docoffWidth
	return d, r.done()
}

// at returns the absolute file offset of the record for id.
//
// The bool is false for an id the table does not cover, and for an offset too
// large to be an int on this platform — the same uint64 comparison discipline
// Index.Doc uses, for the same reason. It is not a bounds check against the
// docs file: this section does not know how long that file is, so the caller
// checks the offset against the payload it is about to read.
func (d docOffsets) at(id DocID) (int, bool) {
	if uint64(id) >= uint64(d.n) {
		return 0, false
	}
	off := binary.LittleEndian.Uint64(d.tab[int(id)*docoffWidth:])
	if off > uint64(maxInt) {
		return 0, false
	}
	return int(off), true
}

// docLen returns the token count recorded for id, and 0 for an id the table
// does not cover — the same answer Index.DocLen gives for an unknown id, which
// BM25 is documented to read as "no normalization".
func (d docOffsets) docLen(id DocID) int {
	if uint64(id) >= uint64(d.n) {
		return 0
	}
	n := binary.LittleEndian.Uint64(d.tab[int(id)*docoffWidth+docoffLenAt:])
	if n > uint64(maxInt) {
		return 0
	}
	return int(n)
}

// keyTable is the parsed keys section: a sorted key list reachable by binary
// search.
type keyTable struct {
	name string
	b    []byte // the whole section payload
	tab  []byte // exactly n*docoffWidth bytes
	n    int
}

// parseKeyTable reads the count and takes the offset table, leaving the entries
// themselves untouched. Same reasoning as parseDocOffsets: entries are read one
// at a time, by whoever asks for one.
func parseKeyTable(r *segReader) (keyTable, error) {
	n, err := r.intn("key count", maxDocCount)
	if err != nil {
		return keyTable{}, err
	}
	if n > (len(r.b)-r.off)/docoffWidth {
		return keyTable{}, fmt.Errorf("%s: %d offsets do not fit in %d remaining bytes: %w",
			r.name, n, len(r.b)-r.off, ErrCorrupt)
	}
	return keyTable{name: r.name, b: r.b, tab: r.b[r.off : r.off+n*docoffWidth], n: n}, nil
}

// at decodes entry i. An out-of-range offset is ErrCorrupt rather than a panic:
// these bytes come from disk, and index.go's promise that library code never
// panics does not exempt a hostile file.
func (k keyTable) at(i int) (string, DocID, error) {
	if i < 0 || i >= k.n {
		return "", 0, fmt.Errorf("%s: key %d of %d: %w", k.name, i, k.n, ErrCorrupt)
	}
	off := binary.LittleEndian.Uint64(k.tab[i*docoffWidth:])
	// Offsets are absolute into the file; the payload starts segHeaderLen in.
	if off < segHeaderLen || off-segHeaderLen > uint64(len(k.b)) {
		return "", 0, fmt.Errorf("%s: key %d sits at offset %d, outside the section: %w", k.name, i, off, ErrCorrupt)
	}
	r := &segReader{name: k.name, b: k.b, off: int(off - segHeaderLen)}
	key, err := r.str("key")
	if err != nil {
		return "", 0, err
	}
	if key == "" {
		return "", 0, fmt.Errorf("%s: key %d is empty, which Add refuses: %w", k.name, i, ErrCorrupt)
	}
	id, err := r.intn("key document id", maxDocCount-1)
	if err != nil {
		return "", 0, err
	}
	return key, DocID(id), nil
}

// verifyKeyTable re-derives the keys section from the documents it indexes and
// refuses a disagreement.
//
// This is D-001's rule applied to a structure milestone 3 writes and nothing on
// the verification path otherwise reads: a section written now and trusted later
// rots silently, and a keys section that disagreed with docs would not fail
// loudly — it would resolve a Key to the wrong document and rank a
// plausible-looking wrong answer.
//
// found is what the docs walk collected, one entry per key. It is read here
// rather than rebuilt, because that map is the size of the document count and a
// second one alongside it would double the only thing a scrub keeps. The docoff
// half of this check lives in the walk itself for the same reason: comparing an
// entry against where the record actually starts asks the same question as
// following the entry and decoding that record a second time, at none of the
// cost.
func verifyKeyTable(keysR *segReader, seg string, n int, found map[string]scrubbedKey) error {
	kt, err := parseKeyTable(keysR)
	if err != nil {
		return err
	}
	if kt.n != n {
		return fmt.Errorf("%s indexes %d keys, the segment holds %d documents: %w", kt.name, kt.n, n, ErrCorrupt)
	}
	prev := ""
	for i := range kt.n {
		key, id, err := kt.at(i)
		if err != nil {
			return err
		}
		// Sorted order is not cosmetic: lookup is a binary search, so an
		// unsorted table does not fail, it answers wrongly.
		if i > 0 && key <= prev {
			return fmt.Errorf("%s: key %q out of order after %q: %w", kt.name, key, prev, ErrCorrupt)
		}
		prev = key
		// The entry has to name the document whose own record carries this key,
		// in this segment. A key held by another segment's documents is as wrong
		// here as one held by none: kt.n entries, sorted and therefore distinct,
		// each matching a distinct record, is what makes this table the bijection
		// Resolve treats it as.
		want, ok := found[key]
		if !ok || want.seg != seg || want.id != id {
			return fmt.Errorf("%s: key %q maps to document %d, which is not the document whose record carries it: %w",
				kt.name, key, id, ErrCorrupt)
		}
	}
	return nil
}

// lookup binary-searches the table. The error is separate from the bool: a key
// that is not in the corpus is an ordinary answer — it is how a dangling Link
// is detected — while an entry that will not decode is damage.
func (k keyTable) lookup(key string) (DocID, bool, error) {
	var err error
	i := sort.Search(k.n, func(i int) bool {
		if err != nil {
			return false
		}
		var got string
		got, _, err = k.at(i)
		return got >= key
	})
	if err != nil {
		return 0, false, err
	}
	if i >= k.n {
		return 0, false, nil
	}
	got, id, err := k.at(i)
	if err != nil {
		return 0, false, err
	}
	if got != key {
		return 0, false, nil
	}
	return id, true, nil
}
