// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
)

// errSegmentGone is a manifest naming a segment that is not there — the
// directory itself, or a section file inside one that a prune is partway
// through removing.
//
// It is ErrCorrupt to every caller that only asks what kind of failure this is.
// Open and Scrub tell it apart because it is the one failure that can mean
// nothing is wrong: Merge publishes its replacement and then prunes what it
// replaced, so a reader that read the manifest before the flip and reaches the
// files after the prune is holding a list that was true when it read it.
// Mappings already taken survive the unlink; only this window does not.
var errSegmentGone = fmt.Errorf("the manifest names this segment but it is no longer there: %w", ErrCorrupt)

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

	// ivf is the approximate vector index, or the zero value for a segment that
	// carries no partition — one written before ivfMinDocs was reached, one
	// holding no vectors, and one written by format version 2, which had no such
	// section at all. All three are the same case to every reader: nearest
	// answers with every id this segment holds.
	ivf ivfSection

	// terms maps a term to the extent of its postings entry, both offsets
	// absolute into the postings file.
	//
	// This one structure is built eagerly, and what allows that is its size
	// being the vocabulary and not the corpus: the milestone 4 index carries
	// 626 MiB of documents behind a 2.7 MB terms index. Making it a binary
	// search over a third fixed-width table would remove that too, and would be
	// a format section bought before anything measured a need for it.
	terms map[string]termSpan
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
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errSegmentGone
		}
		if serr == nil && !fi.IsDir() {
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

	// meta first, alone, because its frame is what says which sections this
	// segment has. Two format versions are accepted and they differ by exactly
	// one file, so "open the list and see what is there" would read a v3 segment
	// missing its partition as a v2 segment — damage reported as an older
	// vintage. The version decides the list; the list decides what must exist.
	metaR, metaB, err := openSection(segRoot, metaFile, kindMeta, true)
	if err != nil {
		return nil, err
	}
	s.maps = append(s.maps, metaB)

	secs := segSectionsFor(metaR.version)
	rs := make([]*segReader, len(secs))
	rs[0] = metaR
	for i, sec := range secs[1:] {
		r, b, err := openSection(segRoot, sec.name, sec.kind, sec.eager)
		if err != nil {
			return nil, err
		}
		s.maps = append(s.maps, b)
		// Every section of one segment was written by one commit, so they all
		// carry one version. Without this a segment whose meta says 3 and whose
		// docs says 2 passes every frame check on its own while describing
		// nothing a writer produced — and the section list above was chosen from
		// one of those two numbers.
		if r.version != metaR.version {
			return nil, fmt.Errorf("%s: format version %d beside %s at version %d: %w",
				sec.name, r.version, metaFile, metaR.version, ErrCorrupt)
		}
		rs[i+1] = r
	}
	docsR, postR, termsR, docoffR, keysR := rs[1], rs[2], rs[3], rs[4], rs[5]

	if s.count, s.totalLen, s.vecDim, err = decodeMeta(metaR); err != nil {
		return nil, err
	}
	// The partition is read after meta because it is checked against it: the
	// centroids are as wide as the corpus's vectors, and a list's ids are ranged
	// against the segment's document count. A v2 segment has no such section and
	// leaves the zero value, which every reader treats as no partition.
	if len(rs) > 6 {
		if s.ivf, err = parseIVF(rs[6], s.count, s.vecDim); err != nil {
			return nil, err
		}
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
	// And the same cross-check for the other seek table. Keys are unique and
	// every document has one, so the table indexes exactly as many keys as meta
	// counts documents — FORMAT.md section 5 refuses either seek table
	// disagreeing with meta, and this is the keys half of that rule.
	if s.keys.n != s.count {
		return nil, fmt.Errorf("%s: meta says %d documents, %s indexes %d keys: %w",
			metaR.name, s.count, keysFile, s.keys.n, ErrCorrupt)
	}
	// The postings payload's end is where the last term's entry stops, which is
	// what gives every entry an extent rather than only a start.
	if s.terms, err = decodeTermIndex(termsR, segHeaderLen+len(s.postings)); err != nil {
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
	// The centroids are the one thing here on the Go heap rather than in a
	// mapping, and the offset table and payload point into one that is about to
	// go — reading through an unmapped region is a segmentation fault, not a
	// panic this package could promise its way out of.
	s.ivf = ivfSection{}
	return first
}

// nearest is the segment's half of Index.Nearest: the segment-wide DocIDs worth
// scoring exactly for v, ascending.
//
// It computes no similarity. Which documents are geometrically plausible is what
// a partition knows; how close each one actually is belongs to the caller, and
// D-008 records why the line is drawn there rather than one function later.
//
// Three cases answer with every id this segment holds, and it matters that they
// are one branch rather than three. A segment with no partition — too small,
// vectorless, or version 2 — has nothing to narrow with. A query whose width is
// not this segment's cannot be compared to its centroids at all, and filtering
// it out silently here would turn "you mixed embedding models" into an empty
// result, which is the disguise scorer/vector's ErrDimMismatch exists to
// prevent: handing the ids over lets the scorer decode a document and say so.
// And a list that fails its own checksum is answered the way D-006 answers
// damage everywhere else — as though the structure were not there. Slower, and
// not wrong.
func (s *segment) nearest(v []float32, k int) []DocID {
	if s.ivf.nlist == 0 || len(v) != s.vecDim {
		return s.allIDs()
	}
	order := ivfOrder(s.ivf.centroids, s.ivf.nlist, s.ivf.dim, v)
	if order == nil {
		return s.allIDs()
	}
	// nprobe lists, then as many more as it takes to have k candidates. The
	// counts were read at Open, so widening is arithmetic on a table rather than
	// decoding lists to find out how short the answer would have been. This is
	// what lets "at least k" be a contract the caller never has to know nprobe
	// to rely on.
	probe := min(ivfNProbe, len(order))
	have := 0
	for _, j := range order[:probe] {
		have += s.ivf.counts[j]
	}
	for have < k && probe < len(order) {
		have += s.ivf.counts[order[probe]]
		probe++
	}

	out := make([]DocID, 0, have)
	for _, j := range order[:probe] {
		ids, ok := s.ivf.list(j)
		if !ok {
			return s.allIDs()
		}
		for _, id := range ids {
			out = append(out, s.base+id)
		}
	}
	// Ascending, which is not cosmetic. The docs file is laid out in DocID order,
	// so a caller decoding these records in order walks the mapping forwards
	// instead of jumping between pages it has already left. Compact is the guard
	// the sort makes free: intact lists partition the segment and cannot repeat
	// an id, and a repeat that got past the checksums would reach the caller as a
	// duplicate result rather than as an error.
	slices.Sort(out)
	return slices.Compact(out)
}

// allIDs is every DocID this segment holds — the answer when there is nothing to
// narrow with.
//
// ponytail: it materializes the segment's whole id space, four bytes a document,
// on a path that used to be a bare loop counter in the scorer. That is the price
// of the scorer having one loop instead of two, and it is paid only where no
// partition applies. If a profile ever shows it, the upgrade is an iterator
// rather than a slice — and that is a change to Nearest's signature, which is in
// the golden API file for a reason.
func (s *segment) allIDs() []DocID {
	out := make([]DocID, s.count)
	for i := range out {
		out[i] = s.base + DocID(i)
	}
	return out
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
	r, ok := s.recordAt(local)
	if !ok {
		return Document{}, false
	}
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
	// The id comes off disk with nothing but the section's own extent behind it:
	// a keys entry carries no seeded checksum, so unlike a document record it
	// cannot prove which document it names. Ranging it against this segment's
	// own count is the guard, at the point of use — the same placement and the
	// same reason as lookup's guard on a term offset. Without it a damaged entry
	// resolves a key past this segment's ids and into a neighbour's, and Doc
	// answers with a real document that is not the one asked for.
	if err != nil || !ok || uint64(id) >= uint64(s.count) {
		return 0, false
	}
	// The range guard stops a damaged id leaving this segment. It does not stop
	// one landing on the wrong document inside it, which is the same plausible
	// wrong answer one segment further in. So the entry has to agree with the
	// record it names — the record is the witness the entry itself does not
	// carry. Its key is its first field, so asking costs one offset lookup and
	// one string decode, against the log2(n) of exactly that which the binary
	// search above has already paid.
	if got, ok := s.keyAt(id); !ok || got != key {
		return 0, false
	}
	return s.base + id, true
}

// keyAt decodes the key of the record for a segment-local id, and nothing else:
// the key is the record's first field, so the text, the vector and the links
// stay on disk.
//
// It does not verify the record's seeded checksum, which would mean reading the
// whole record — the cost doc pays and this deliberately does not. Two
// independently written structures agreeing on the key is the question being
// asked, and the first field answers it.
func (s *segment) keyAt(local DocID) (string, bool) {
	r, ok := s.recordAt(local)
	if !ok {
		return "", false
	}
	got, err := r.str("document key")
	return got, err == nil
}

// recordAt positions a reader on a segment-local id's record and bounds it by
// that record, which is the part that is not bookkeeping.
//
// A record's seeded checksum is what says its fields are real, and it cannot be
// consulted first: it stands at the end of the record, and where the record ends
// is what decoding it establishes. So every length inside is acted on before
// anything has vouched for it, and the only question is what bounds it. Handed
// the whole section, a damaged vector width or text length is bounded by the
// corpus — one flipped byte asks for an allocation the size of the index and
// gets the process killed instead of Doc returning false.
//
// docoff answers it for nothing: the next document's offset is where this
// record stops. The table is fixed-width, so the second read is arithmetic, and
// its frame checksum is one of the two Open verifies.
func (s *segment) recordAt(local DocID) (*segReader, bool) {
	off, ok := s.offs.at(local)
	if !ok || off < segHeaderLen || off-segHeaderLen > len(s.docs) {
		return nil, false
	}
	// The last record runs to the end of the section; every other one stops
	// where the next begins. An entry that would end this record before it
	// starts, or past the section, describes no record the writer produced.
	end := len(s.docs)
	if next, ok := s.offs.at(local + 1); ok {
		if next < off || next-segHeaderLen > len(s.docs) {
			return nil, false
		}
		end = next - segHeaderLen
	}
	return &segReader{name: docsFile, b: s.docs[:end], off: off - segHeaderLen}, true
}

// lookup decodes the postings for term, ascending by index-wide DocID, or nil
// if this segment does not hold it.
//
// The offset is bounds-checked here rather than where the terms index was
// decoded, and that placement is forced. decodePostings can check it at decode
// time because it walks the postings file in step with the terms file and knows
// where each entry belongs; decodeTermIndex performs no such walk, since not
// walking is what makes Open lazy. So the check moves to the point of use, the
// same place and the same shape as doc's check on the offset it takes from
// docoff — and nil is the same answer doc's false is, for the reason D-006
// gives.
func (s *segment) lookup(term string) []Posting {
	var pl []Posting
	if s.scanPostings(term, func(p Posting) { pl = append(pl, p) }) == 0 {
		// Zero is both "this segment does not hold the term" and "its postings
		// do not decode", and nil is the answer to either: a partial list
		// yielded before a decoder gave up is not a shorter posting list, it is
		// a wrong one.
		return nil
	}
	return pl
}

// scanPostings hands term's postings to yield, ascending by index-wide DocID,
// and reports how many. Zero means this segment does not hold the term or
// cannot decode it, and yield may already have been called when that is known.
//
// Streaming, because Merge writes a term's postings without ever holding them:
// see segSource. Lookup is the caller that does want the slice, and builds it.
func (s *segment) scanPostings(term string, yield func(Posting)) int {
	sp, ok := s.terms[term]
	if !ok || sp.off < segHeaderLen || sp.off-segHeaderLen > len(s.postings) {
		return 0
	}
	// The end is bounded here for the same reason the start is: both come off
	// disk, and an entry whose extent runs past the payload is one the decoder
	// can never satisfy.
	if sp.end < sp.off || sp.end-segHeaderLen > len(s.postings) {
		return 0
	}
	r := &segReader{name: postingsFile, b: s.postings, off: sp.off - segHeaderLen}
	n, err := decodeTermPostings(r, term, s.offs, sp.end-segHeaderLen, func(p Posting) {
		p.Doc += s.base
		yield(p)
	})
	if err != nil {
		return 0
	}
	return n
}
