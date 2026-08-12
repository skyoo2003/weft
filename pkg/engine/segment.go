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
	"slices"
	"time"
)

// This file is the on-disk segment format — the milestone 2 asset. Code can be
// rewritten; a format on disk demands migration. docs/FORMAT.md is the
// readable spec, this file is what the bytes obey, and the two disagreeing is
// a bug in the document.
//
// Every file a Commit writes wears the same frame:
//
//	magic "weft" | format version (uvarint) | kind (1 byte) | payload | crc32 Castagnoli (4B LE)
//
// The CRC covers everything before it, header included. The kind byte names
// which section this file is, so a docs file sitting at meta's path is
// refused instead of misread — a checksum can only tell bytes from noise, not
// one healthy file from another. Framing is the one thing that must stay
// stable across versions: a future reader can only reject what it cannot
// parse if the version is always in the same place.
const (
	formatVersion = 1

	// segHeaderLen is the frame header size while the version encodes in one
	// byte, which it does until version 128. The terms index records absolute
	// file offsets, so this constant is part of the format, not a convenience.
	segHeaderLen = 4 + 1 + 1 // magic + version + kind

	kindMeta     byte = 1
	kindDocs     byte = 2
	kindPostings byte = 3
	kindTerms    byte = 4
	kindManifest byte = 5

	// blockSize is how many postings share one block and one
	// (maxDocID, maxTF, minDocLen) header on disk. The metadata is written now
	// and read by no query until milestone 5 — D-001's decision, taken because
	// retrofitting it is a format migration while writing it costs three
	// varints per block.
	//
	// ponytail: 128 is the block-max WAND literature's convention, adopted
	// unmeasured. Revisit when milestone 5 starts actually skipping blocks.
	blockSize = 128
)

var (
	segMagic = []byte("weft")

	// Castagnoli rather than IEEE: better error detection and hardware support
	// on current platforms, at identical cost from the stdlib.
	segCRC = crc32.MakeTable(crc32.Castagnoli)
)

// maxInt is the largest int on this platform. Counts and lengths from disk
// are ranged in uint64 before conversion — the same 32-bit-target discipline
// as the bounds on Index.Doc.
const maxInt = int(^uint(0) >> 1)

// ---------------------------------------------------------------------------
// writing
// ---------------------------------------------------------------------------

// segWriter accumulates one framed section file and writes it out, fsynced,
// on close. Buffering the whole section in memory is fine by construction —
// the section describes an index that is itself entirely in memory — and it
// is what makes the running offset exact for the terms index.
type segWriter struct {
	f   *os.File
	buf []byte
}

// newSegWriter creates name under root, exclusively. Two guards, because the
// index directory belongs to the caller and weft's paths inside it are
// predictable:
//
// The root confines the write to the directory tree it was opened on, so a
// symlink standing where a segment directory belongs cannot redirect it. This
// is the OS's check, not one of ours that a rename could race.
//
// O_EXCL refuses a path something already stands at, symlink included, which
// is what stops the manifest's temp file from being written through a link
// planted at its name — the difference between "can write in the index
// directory" and "can overwrite any file this process can reach". A caller
// clears its own debris first; anything still in the way is somebody else's.
func newSegWriter(root *os.Root, name string, kind byte) (*segWriter, error) {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	w := &segWriter{f: f}
	w.buf = append(w.buf, segMagic...)
	w.buf = binary.AppendUvarint(w.buf, formatVersion)
	w.buf = append(w.buf, kind)
	return w, nil
}

func (w *segWriter) write(b []byte)   { w.buf = append(w.buf, b...) }
func (w *segWriter) uvarint(v uint64) { w.buf = binary.AppendUvarint(w.buf, v) }
func (w *segWriter) varint(v int64)   { w.buf = binary.AppendVarint(w.buf, v) }
func (w *segWriter) str(s string)     { w.uvarint(uint64(len(s))); w.buf = append(w.buf, s...) }

// off is the absolute file offset of the next byte written, header included.
func (w *segWriter) off() int { return len(w.buf) }

// close seals the frame with its checksum and flushes to disk. The Sync is
// not optional: the manifest rename publishes this file, and a rename can
// survive a crash that unsynced contents did not — a manifest pointing at
// hollow files is exactly the mixed state Commit promises cannot exist.
func (w *segWriter) close() error {
	w.buf = binary.LittleEndian.AppendUint32(w.buf, crc32.Checksum(w.buf, segCRC))
	if _, err := w.f.Write(w.buf); err != nil {
		w.f.Close()
		return err
	}
	if err := w.f.Sync(); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// writeSegment lays the index down as one segment: meta, docs, postings and
// the terms index, each its own framed file in segDir.
//
// Everything is encoded under a single read lock, so the statistics in meta
// and the documents they describe cannot come from different moments — the
// atomicity FINDINGS section 4.4 asked for, statistics snapshot included.
// Only the in-memory encoding happens under the lock; the disk writes in
// close run after it is released.
func (ix *Index) writeSegment(segRoot *os.Root) error {
	sections := []struct {
		name string
		kind byte
	}{
		{metaFile, kindMeta},
		{docsFile, kindDocs},
		{postingsFile, kindPostings},
		{termsFile, kindTerms},
	}
	ws := make([]*segWriter, len(sections))
	for i, s := range sections {
		w, err := newSegWriter(segRoot, s.name, s.kind)
		if err != nil {
			for _, open := range ws[:i] {
				open.f.Close()
			}
			return err
		}
		ws[i] = w
	}
	meta, docs, post, terms := ws[0], ws[1], ws[2], ws[3]

	// Deferred inside a closure, not unlocked inline: a panic anywhere in the
	// encoders would otherwise leave the index read-locked forever and deadlock
	// every later Add.
	func() {
		ix.mu.RLock()
		defer ix.mu.RUnlock()
		meta.uvarint(uint64(len(ix.docs)))
		meta.uvarint(uint64(ix.totalLen))
		meta.uvarint(uint64(ix.vecDim))
		encodeDocs(docs, ix)
		encodePostings(post, terms, ix)
	}()

	for i, w := range ws {
		if err := w.close(); err != nil {
			for _, rest := range ws[i+1:] {
				rest.f.Close()
			}
			return fmt.Errorf("%s: %w", sections[i].name, err)
		}
	}
	return nil
}

// encodeDocs lays documents out in DocID order — the position in the file is
// the DocID, so the id is never written and cannot disagree with itself.
// Requires ix.mu to be held.
//
// Links are stored as the caller's Keys, not DocIDs (FINDINGS section 4.2):
// lazy resolution is what keeps forward references and dangling edges free,
// and milestone 4's evaluation joins an external citation graph by Key.
//
// docLen is stored rather than recomputed from Text on load, so a future
// change of tokenizer cannot make restored postings and restored lengths
// disagree about what was indexed.
//
// Time is Unix seconds plus nanoseconds. The zone is deliberately dropped — a
// restored Time is the same instant in UTC, and instants are all any scorer
// reads. The zero Time needs no presence flag: its own Unix seconds decode
// back to a Time that IsZero again, so "no timestamp", which recency treats
// as "no opinion", survives the trip by arithmetic rather than by convention.
func encodeDocs(w *segWriter, ix *Index) {
	w.uvarint(uint64(len(ix.docs)))
	for i, d := range ix.docs {
		w.str(d.Key)
		w.str(d.Text)
		w.uvarint(uint64(ix.docLen[i]))
		w.uvarint(uint64(len(d.Vector)))
		for _, c := range d.Vector {
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], math.Float32bits(c))
			w.write(b[:])
		}
		w.uvarint(uint64(len(d.Links)))
		for _, l := range d.Links {
			w.str(l)
		}
		w.varint(d.Time.Unix())
		w.uvarint(uint64(d.Time.Nanosecond()))
	}
}

// encodePostings writes the postings file and the terms index side by side.
// Requires ix.mu to be held.
//
// The term strings live only in the terms index; the postings file is bare
// entries, found by the absolute offsets the index records. That makes the
// terms index load-bearing from day one — it is milestone 3's seek structure,
// but it never gets the chance to rot unread, because reading a segment
// without it is impossible.
//
// Entries are in sorted term order, so identical index states produce
// identical bytes. Postings within a term are already ascending by DocID —
// Add appends monotonically — which is what makes each block's last posting
// its maxDocID and every delta after a block's first strictly positive.
//
// Delta chains stop at the block boundary: every block's first posting is an
// absolute DocID. That is what makes a block decodable on its own, which is
// the whole point of the D-001 metadata — a skipper that had to decode every
// preceding block to learn where this one starts would be skipping nothing.
// Costing at most three extra bytes per block, and a format migration to
// retrofit.
//
// Each block carries maxDocID, maxTF and minDocLen (D-001): segment-local,
// immutable, and together the block's true BM25 score ceiling under whatever
// the collection statistics are at query time.
func encodePostings(post, terms *segWriter, ix *Index) {
	keys := slices.Sorted(maps.Keys(ix.postings))
	post.uvarint(uint64(len(keys)))
	terms.uvarint(uint64(len(keys)))

	for _, t := range keys {
		terms.str(t)
		terms.uvarint(uint64(post.off()))

		pl := ix.postings[t]
		post.uvarint(uint64((len(pl) + blockSize - 1) / blockSize))

		for start := 0; start < len(pl); start += blockSize {
			blk := pl[start:min(start+blockSize, len(pl))]
			maxTF, minDL := 0, maxInt
			for _, p := range blk {
				maxTF = max(maxTF, p.Freq)
				minDL = min(minDL, ix.docLen[p.Doc])
			}
			post.uvarint(uint64(len(blk)))
			post.uvarint(uint64(blk[len(blk)-1].Doc)) // maxDocID
			post.uvarint(uint64(maxTF))
			post.uvarint(uint64(minDL))
			prev := uint64(0)
			for i, p := range blk {
				id := uint64(p.Doc)
				if i == 0 {
					post.uvarint(id)
				} else {
					post.uvarint(id - prev)
				}
				post.uvarint(uint64(p.Freq))
				prev = id
			}
		}
	}
}

// ---------------------------------------------------------------------------
// reading
// ---------------------------------------------------------------------------

// segReader walks one section's payload with every read bounds-checked. Bytes
// from disk are a trust boundary: truncated, bit-flipped past what the CRC
// can notice, or written by something else entirely. No read here may index
// past the buffer or allocate more than the buffer could back, and every
// failure is ErrCorrupt — never a panic; index.go promises library code does
// not panic, and a hostile file does not get an exemption.
type segReader struct {
	name string
	b    []byte
	off  int
}

func (r *segReader) uvarint(what string) (uint64, error) {
	v, n := binary.Uvarint(r.b[r.off:])
	if n <= 0 {
		return 0, fmt.Errorf("%s: %s at byte %d is truncated or oversized: %w", r.name, what, r.off, ErrCorrupt)
	}
	r.off += n
	return v, nil
}

func (r *segReader) varint(what string) (int64, error) {
	v, n := binary.Varint(r.b[r.off:])
	if n <= 0 {
		return 0, fmt.Errorf("%s: %s at byte %d is truncated or oversized: %w", r.name, what, r.off, ErrCorrupt)
	}
	r.off += n
	return v, nil
}

// intn reads a uvarint that must fit in [0, limit]. The comparison happens in
// uint64 so a value past this platform's int cannot wrap on conversion — the
// mirror of the uint64 bounds on Index.Doc.
func (r *segReader) intn(what string, limit int) (int, error) {
	v, err := r.uvarint(what)
	if err != nil {
		return 0, err
	}
	if v > uint64(limit) {
		return 0, fmt.Errorf("%s: %s is %d, limit %d: %w", r.name, what, v, limit, ErrCorrupt)
	}
	return int(v), nil
}

func (r *segReader) str(what string) (string, error) {
	n, err := r.uvarint(what + " length")
	if err != nil {
		return "", err
	}
	if n > uint64(len(r.b)-r.off) {
		return "", fmt.Errorf("%s: %s of %d bytes overruns the buffer: %w", r.name, what, n, ErrCorrupt)
	}
	s := string(r.b[r.off : r.off+int(n)])
	r.off += int(n)
	return s, nil
}

func (r *segReader) u32(what string) (uint32, error) {
	if len(r.b)-r.off < 4 {
		return 0, fmt.Errorf("%s: %s overruns the buffer at byte %d: %w", r.name, what, r.off, ErrCorrupt)
	}
	v := binary.LittleEndian.Uint32(r.b[r.off:])
	r.off += 4
	return v, nil
}

// done rejects trailing bytes. A payload that decodes cleanly but has
// leftover content was not written by this encoder, and accepting it would
// let two different files mean the same index.
func (r *segReader) done() error {
	if r.off != len(r.b) {
		return fmt.Errorf("%s: %d trailing bytes: %w", r.name, len(r.b)-r.off, ErrCorrupt)
	}
	return nil
}

// openSection reads one framed file and hands back a reader over its payload.
//
// A section the manifest names but that is not on disk is damage, not an
// absent index: it reports ErrCorrupt rather than fs.ErrNotExist, so a caller
// whose "nothing committed here yet" branch starts a fresh index cannot be
// handed a half-deleted segment and quietly overwrite it.
func openSection(root *os.Root, name string, kind byte) (*segReader, error) {
	b, err := root.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: the manifest names this segment but the file is missing: %w", name, ErrCorrupt)
	} else if err != nil {
		return nil, err
	}
	return parseSection(name, b, kind)
}

// parseSection verifies a file's frame. The order of checks is deliberate:
// magic first, so a file that was never weft's reads as corrupt rather than
// as a version problem; CRC second, so a flipped bit in the version byte
// cannot masquerade as ErrBadVersion; version third, once the bytes are known
// intact; kind last, catching intact files standing in the wrong place.
func parseSection(name string, b []byte, kind byte) (*segReader, error) {
	if len(b) < segHeaderLen+crc32.Size {
		return nil, fmt.Errorf("%s: %d bytes is shorter than the file frame: %w", name, len(b), ErrCorrupt)
	}
	if !bytes.Equal(b[:len(segMagic)], segMagic) {
		return nil, fmt.Errorf("%s: bad magic: %w", name, ErrCorrupt)
	}
	body := b[:len(b)-crc32.Size]
	if crc32.Checksum(body, segCRC) != binary.LittleEndian.Uint32(b[len(b)-crc32.Size:]) {
		return nil, fmt.Errorf("%s: checksum mismatch: %w", name, ErrCorrupt)
	}
	v, n := binary.Uvarint(body[len(segMagic):])
	if n <= 0 || len(segMagic)+n+1 > len(body) {
		return nil, fmt.Errorf("%s: unreadable version: %w", name, ErrCorrupt)
	}
	if v != formatVersion {
		return nil, fmt.Errorf("%s: format version %d, this build reads %d: %w", name, v, formatVersion, ErrBadVersion)
	}
	// Version 1 is one byte, and only one byte. binary.Uvarint accepts overlong
	// encodings — 0x81 0x00 also decodes to 1 — which would make the header
	// seven bytes while segHeaderLen, and every absolute offset the terms index
	// records against it, still says six. Two byte strings meaning the same
	// index is one too many for a format whose writer is expected to be
	// deterministic.
	if n != segHeaderLen-len(segMagic)-1 {
		return nil, fmt.Errorf("%s: format version 1 written in %d bytes, not 1: %w", name, n, ErrCorrupt)
	}
	if got := body[len(segMagic)+n]; got != kind {
		return nil, fmt.Errorf("%s: section kind %d where %d belongs: %w", name, got, kind, ErrCorrupt)
	}
	return &segReader{name: name, b: body[len(segMagic)+n+1:]}, nil
}

// decodeMeta returns the collection statistics a segment claims. The claims
// are cross-checked against the docs file by Open — meta is the snapshot BM25
// trusts, so it does not get to disagree with the documents it describes.
func decodeMeta(r *segReader) (docCount, totalLen, vecDim int, err error) {
	// DocIDs are dense from 0, so the doc count shares DocID's uint32 ceiling —
	// the same limit Add enforces on the write side.
	if docCount, err = r.intn("doc count", min(maxInt, math.MaxUint32)); err != nil {
		return 0, 0, 0, err
	}
	if totalLen, err = r.intn("total length", maxInt); err != nil {
		return 0, 0, 0, err
	}
	if vecDim, err = r.intn("vector dim", maxInt); err != nil {
		return 0, 0, 0, err
	}
	return docCount, totalLen, vecDim, r.done()
}

// decodeDocs rebuilds the document store: docs, byKey, docLen, and the
// derived totalLen and vecDim. Postings arrive separately via decodePostings.
//
// The invariants re-checked here are the ones the write path enforces — keys
// unique and non-empty, vector components finite and uniformly wide — because
// scorer/vector builds on ErrNonFiniteVector's promise by not re-checking
// documents, and the decoder re-validating is what keeps that promise true
// for restored corpora.
func decodeDocs(r *segReader) (*Index, error) {
	n, err := r.intn("document count", min(maxInt, math.MaxUint32))
	if err != nil {
		return nil, err
	}

	// Capacity hints are capped twice: by what the payload could physically
	// hold — a document entry is at least 7 bytes — and by a flat ceiling,
	// because a Document is ~100 bytes of header for those 7 bytes on disk and
	// the ratio, not the count, is what a hostile file would buy. Past the
	// ceiling append grows the slice, which is what it is for.
	hint := min(n, len(r.b)/7, 1<<16)
	ix := &Index{
		docs:     make([]Document, 0, hint),
		byKey:    make(map[string]DocID, hint),
		postings: make(map[string][]Posting),
		docLen:   make([]int, 0, hint),
	}

	for i := 0; i < n; i++ {
		var d Document
		if d.Key, err = r.str("document key"); err != nil {
			return nil, err
		}
		if d.Key == "" {
			return nil, fmt.Errorf("%s: document %d has an empty key, which Add refuses: %w", r.name, i, ErrCorrupt)
		}
		if _, dup := ix.byKey[d.Key]; dup {
			return nil, fmt.Errorf("%s: duplicate key %q: %w", r.name, d.Key, ErrCorrupt)
		}
		if d.Text, err = r.str("document text"); err != nil {
			return nil, err
		}
		dl, err := r.intn("document length", maxInt)
		if err != nil {
			return nil, err
		}
		if dl > maxInt-ix.totalLen {
			return nil, fmt.Errorf("%s: document lengths overflow their sum: %w", r.name, ErrCorrupt)
		}

		vn, err := r.intn("vector width", (len(r.b)-r.off)/4)
		if err != nil {
			return nil, err
		}
		if vn > 0 {
			if ix.vecDim == 0 {
				ix.vecDim = vn
			} else if vn != ix.vecDim {
				return nil, fmt.Errorf("%s: document %d vector is %d wide, corpus is %d: %w", r.name, i, vn, ix.vecDim, ErrCorrupt)
			}
			d.Vector = make([]float32, vn)
			for j := range d.Vector {
				bits, err := r.u32("vector component")
				if err != nil {
					return nil, err
				}
				c := math.Float32frombits(bits)
				if f := float64(c); math.IsNaN(f) || math.IsInf(f, 0) {
					return nil, fmt.Errorf("%s: document %d vector component %d is %v, which Add refuses: %w", r.name, i, j, c, ErrCorrupt)
				}
				d.Vector[j] = c
			}
		}

		ln, err := r.intn("link count", len(r.b)-r.off)
		if err != nil {
			return nil, err
		}
		if ln > 0 {
			// Same ceiling as the document hint, for the same reason: a link is
			// one byte on disk and sixteen in a slice header.
			d.Links = make([]string, 0, min(ln, 1<<12))
			for range ln {
				l, err := r.str("link key")
				if err != nil {
					return nil, err
				}
				d.Links = append(d.Links, l)
			}
		}

		sec, err := r.varint("time seconds")
		if err != nil {
			return nil, err
		}
		nsec, err := r.intn("time nanoseconds", int(time.Second/time.Nanosecond)-1)
		if err != nil {
			return nil, err
		}
		d.Time = time.Unix(sec, int64(nsec)).UTC()

		ix.byKey[d.Key] = DocID(len(ix.docs))
		ix.docs = append(ix.docs, d)
		ix.docLen = append(ix.docLen, dl)
		ix.totalLen += dl
	}
	return ix, r.done()
}

// decodePostings walks the terms index and the postings file in lockstep,
// filling host.postings. The terms index leads: it holds the term strings and
// the absolute offset each entry must sit at, so a wrong offset is caught
// today rather than discovered by milestone 3's first seek.
//
// The D-001 block metadata is re-derived from each block's contents and
// compared against what the block records. Unread fields rot silently — D-001
// requires tests to catch that, and the decoder checking on every Open means
// the rot cannot even wait for a test run.
func decodePostings(post, terms *segReader, host *Index) error {
	pn, err := post.intn("term count", len(post.b))
	if err != nil {
		return err
	}
	tn, err := terms.intn("term count", len(terms.b))
	if err != nil {
		return err
	}
	if pn != tn {
		return fmt.Errorf("%s lists %d terms, %s lists %d: %w", post.name, pn, terms.name, tn, ErrCorrupt)
	}

	// Every token Add saw became exactly one posting increment, so a document's
	// frequencies summed across all terms equal the docLen stored beside it.
	// Per-posting the check is only freq <= docLen, which lets a doctored file
	// give a one-token document a frequency of 1 under two different terms —
	// each legal alone, together describing a document that cannot exist. BM25
	// would then divide real frequencies by a length that never held them and
	// return scores that look reasonable and are wrong.
	sumFreq := make([]int, len(host.docs))

	prevTerm := ""
	for i := 0; i < pn; i++ {
		term, err := terms.str("term")
		if err != nil {
			return err
		}
		if term == "" {
			return fmt.Errorf("%s: term %d is empty, which the tokenizer never emits: %w", terms.name, i, ErrCorrupt)
		}
		// prevTerm starts empty and terms are non-empty, so the first term is
		// never out of order and needs no special case.
		if term <= prevTerm {
			return fmt.Errorf("%s: term %q out of order after %q: %w", terms.name, term, prevTerm, ErrCorrupt)
		}
		prevTerm = term

		off, err := terms.uvarint("term offset")
		if err != nil {
			return err
		}
		if off != uint64(segHeaderLen+post.off) {
			return fmt.Errorf("%s: term %q recorded at offset %d, entry sits at %d: %w",
				terms.name, term, off, segHeaderLen+post.off, ErrCorrupt)
		}

		nblocks, err := post.intn("block count", len(post.b))
		if err != nil {
			return err
		}
		if nblocks == 0 {
			return fmt.Errorf("%s: term %q has no blocks; a term with no postings is never written: %w", post.name, term, ErrCorrupt)
		}

		var pl []Posting
		prev := uint64(0)
		for b := 0; b < nblocks; b++ {
			cnt, err := post.intn("block posting count", blockSize)
			if err != nil {
				return err
			}
			// Every block is full except the last — that is what keeps the
			// block count and the posting count consistent with each other.
			if cnt == 0 || (b < nblocks-1 && cnt != blockSize) {
				return fmt.Errorf("%s: term %q block %d holds %d postings: %w", post.name, term, b, cnt, ErrCorrupt)
			}
			maxDoc, err := post.uvarint("block maxDocID")
			if err != nil {
				return err
			}
			maxTF, err := post.intn("block maxTF", maxInt)
			if err != nil {
				return err
			}
			minDL, err := post.intn("block minDocLen", maxInt)
			if err != nil {
				return err
			}

			gotMaxTF, gotMinDL := 0, maxInt
			for j := range cnt {
				delta, err := post.uvarint("posting docID delta")
				if err != nil {
					return err
				}
				var id uint64
				switch {
				case j > 0:
					if delta == 0 {
						return fmt.Errorf("%s: term %q repeats a docID; postings are strictly ascending: %w", post.name, term, ErrCorrupt)
					}
					// Checked before the addition, not after: uint64 wraps
					// silently, and a wrapped id lands back inside the corpus
					// and passes every check below while breaking the ascending
					// order block skipping and the TopK tiebreak both assume.
					if delta > math.MaxUint64-prev {
						return fmt.Errorf("%s: term %q docID delta %d overflows past %d: %w", post.name, term, delta, prev, ErrCorrupt)
					}
					id = prev + delta
				case b == 0:
					id = delta // the term's first posting: nothing precedes it
				default:
					// A block's first posting is absolute, so the check that it
					// still ascends past the previous block is explicit.
					id = delta
					if id <= prev {
						return fmt.Errorf("%s: term %q block %d starts at document %d, block %d ended at %d; postings are strictly ascending: %w",
							post.name, term, b, id, b-1, prev, ErrCorrupt)
					}
				}
				if id >= uint64(len(host.docs)) {
					return fmt.Errorf("%s: term %q names document %d of a %d-document corpus: %w", post.name, term, id, len(host.docs), ErrCorrupt)
				}
				prev = id
				freq, err := post.intn("posting frequency", maxInt)
				if err != nil {
					return err
				}
				if freq == 0 {
					return fmt.Errorf("%s: term %q in document %d has frequency 0, which Add never writes: %w", post.name, term, id, ErrCorrupt)
				}
				// A term cannot occur more often than its document has tokens.
				if freq > host.docLen[id] {
					return fmt.Errorf("%s: term %q occurs %d times in document %d of %d tokens: %w",
						post.name, term, freq, id, host.docLen[id], ErrCorrupt)
				}
				pl = append(pl, Posting{Doc: DocID(id), Freq: freq})
				sumFreq[id] += freq
				gotMaxTF = max(gotMaxTF, freq)
				gotMinDL = min(gotMinDL, host.docLen[id])
			}

			if last := uint64(pl[len(pl)-1].Doc); last != maxDoc {
				return fmt.Errorf("%s: term %q block %d records maxDocID %d, contents end at %d: %w", post.name, term, b, maxDoc, last, ErrCorrupt)
			}
			if gotMaxTF != maxTF {
				return fmt.Errorf("%s: term %q block %d records maxTF %d, contents say %d: %w", post.name, term, b, maxTF, gotMaxTF, ErrCorrupt)
			}
			if gotMinDL != minDL {
				return fmt.Errorf("%s: term %q block %d records minDocLen %d, contents say %d: %w", post.name, term, b, minDL, gotMinDL, ErrCorrupt)
			}
		}
		host.postings[term] = pl
	}

	for id, sum := range sumFreq {
		if sum != host.docLen[id] {
			return fmt.Errorf("%s: document %d holds %d indexed tokens, its stored length is %d: %w",
				post.name, id, sum, host.docLen[id], ErrCorrupt)
		}
	}

	if err := post.done(); err != nil {
		return err
	}
	return terms.done()
}
