// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// segment is one immutable on-disk segment, mapped and answering point queries.
//
// It is what an Index reads when the answer is not in the pending in-memory
// segment. Nothing here is decoded up front except the parts whose size is the
// vocabulary rather than the corpus: meta, the two fixed-width tables' headers,
// and the terms index. A document, a posting list and a key lookup are each
// decoded when asked for and not before.
//
// The DocIDs a segment owns are [base, base+count). Ids stay dense and
// index-wide unique, so the milestone 1 precondition on Search — every scorer
// reads one index — is unchanged, and TopK's tiebreak still means what it
// meant. A composite (segment, local id) would have widened DocID, and DocID's
// width is in the golden API file for a reason.
type segment struct {
	base     DocID
	count    int
	totalLen int
	vecDim   int

	maps [][]byte // every mapping, in openSegment order, for close

	docs     []byte // docs payload
	postings []byte // postings payload
	offs     docOffsets
	keys     keyTable

	// terms maps a term to the absolute file offset of its postings entry.
	//
	// This one structure is built eagerly, and what allows that is its size
	// being the vocabulary and not the corpus: the milestone 4 index carries
	// 626 MiB of documents behind a 2.7 MB terms index. Making it a binary
	// search over a third fixed-width table would remove that too, and would be
	// a format section bought before anything measured a need for it.
	terms map[string]int
}

// openSegment maps a segment directory and reads the parts bounded by the
// vocabulary. base is the first DocID this segment owns.
func openSegment(root *os.Root, name string, base DocID) (*segment, error) {
	segRoot, err := root.OpenRoot(name)
	if err != nil {
		// The manifest named it, so absence or a foreign layout is damage
		// rather than an index that was never written — the same judgement
		// openSection makes about a missing section file. A directory that is
		// genuinely there and still will not open is the filesystem refusing
		// us, and reports as itself. Lstat, not Stat: Stat follows the link in
		// question.
		fi, serr := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) || (serr == nil && !fi.IsDir()) {
			return nil, fmt.Errorf("the manifest names this segment but no directory stands there: %w", ErrCorrupt)
		}
		return nil, err
	}
	defer segRoot.Close()

	s := &segment{base: base, maps: make([][]byte, 0, len(segSections))}
	// One deferred cleanup rather than a close call on every error path below:
	// there are six of them and the seventh is the one somebody forgets.
	ok := false
	defer func() {
		if !ok {
			s.close() //nolint:errcheck // already returning an error
		}
	}()

	rs := make([]*segReader, len(segSections))
	for i, sec := range segSections {
		r, b, err := openSection(segRoot, sec.name, sec.kind, false)
		if err != nil {
			return nil, err
		}
		s.maps = append(s.maps, b)
		rs[i] = r
	}
	metaR, docsR, postR, termsR, docoffR, keysR := rs[0], rs[1], rs[2], rs[3], rs[4], rs[5]

	if s.count, s.totalLen, s.vecDim, err = decodeMeta(metaR); err != nil {
		return nil, err
	}
	s.docs, s.postings = docsR.b, postR.b

	if s.offs, err = parseDocOffsets(docoffR); err != nil {
		return nil, err
	}
	// meta is the statistics snapshot BM25 trusts, so it does not get to
	// disagree with the table that says how many documents there are. This is
	// the one document-count cross-check the lazy path can still afford,
	// because both numbers are read anyway.
	if s.offs.n != s.count {
		return nil, fmt.Errorf("%s: meta says %d documents, %s indexes %d: %w",
			metaR.name, s.count, docoffFile, s.offs.n, ErrCorrupt)
	}
	if s.keys, err = parseKeyTable(keysR); err != nil {
		return nil, err
	}
	if s.terms, err = decodeTermIndex(termsR); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

// close releases every mapping. Safe to call twice: the slice is cleared, and
// unmapping a region a second time would be an error at best.
func (s *segment) close() error {
	var first error
	for _, b := range s.maps {
		if err := unmapFile(b); err != nil && first == nil {
			first = err
		}
	}
	s.maps, s.docs, s.postings = nil, nil, nil
	s.offs, s.keys, s.terms = docOffsets{}, keyTable{}, nil
	return first
}

// holds reports whether id belongs to this segment.
func (s *segment) holds(id DocID) bool {
	return uint64(id) >= uint64(s.base) && uint64(id) < uint64(s.base)+uint64(s.count)
}

// doc decodes the document with the given index-wide id.
//
// Every call decodes: a key, a text, a vector and a link list are built fresh
// from the mapping. Index.Doc used to return values aliasing index state at no
// cost, so a scorer walking the whole corpus now pays an allocation per
// document per query. scorer/vector is exactly that scorer, and the milestone 3
// section of docs/FINDINGS.md carries the arithmetic: roughly 69% of the
// milestone 4 docs file is vectors, and a full scan touches every page of them,
// so lazy loading moves that working set from the Go heap into the page cache
// without shrinking it. An approximate vector index is what removes the scan.
// Nothing in this file can.
func (s *segment) doc(id DocID) (Document, bool) {
	local := id - s.base
	off, ok := s.offs.at(local)
	if !ok || off < segHeaderLen || off-segHeaderLen > len(s.docs) {
		return Document{}, false
	}
	r := &segReader{name: docsFile, b: s.docs, off: off - segHeaderLen}
	d, _, err := decodeDocRecord(r, int(local))
	if err != nil {
		// The checksum says this record is not intact, or not the one asked
		// for. Reporting false rather than an error is what keeps Doc's
		// signature — the milestone's whole claim — and the caller sees what it
		// sees for an id that was never assigned. Scrub is what names the
		// damage, and docs/FINDINGS.md records the trade.
		return Document{}, false
	}
	return d, true
}

// docLen is arithmetic on the mapped table, not a decode. BM25 asks once per
// posting, which is why the token count is in the table at all.
func (s *segment) docLen(id DocID) int {
	return s.offs.docLen(id - s.base)
}

// resolve binary-searches this segment's keys. The DocID returned is
// index-wide, so a caller never sees a segment-local one.
func (s *segment) resolve(key string) (DocID, bool) {
	id, ok, err := s.keys.lookup(key)
	if err != nil || !ok {
		return 0, false
	}
	return s.base + id, true
}

// lookup decodes the postings for term, ascending by index-wide DocID, or nil
// if this segment does not hold it.
func (s *segment) lookup(term string) []Posting {
	off, ok := s.terms[term]
	if !ok {
		return nil
	}
	r := &segReader{name: postingsFile, b: s.postings, off: off - segHeaderLen}
	pl, err := decodeTermPostings(r, term, s.count)
	if err != nil {
		return nil
	}
	if s.base != 0 {
		for i := range pl {
			pl[i].Doc += s.base
		}
	}
	return pl
}
