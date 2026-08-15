// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"math"
	"os"
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
	// formatVersion 2 is milestone 3's. Version 1 wrote documents as a bare run
	// of variable-length records and rebuilt byKey by reading all of them, so
	// neither a DocID nor a Key could reach its document without decoding every
	// document in front of it — no arrangement of a lazy reader fixes that, only
	// different bytes do. v1 is refused rather than migrated; D-007 records why
	// that argument is available exactly once.
	formatVersion = 2

	// segHeaderLen is the frame header size while the version encodes in one
	// byte, which it does until version 128. The terms index records absolute
	// file offsets, so this constant is part of the format, not a convenience.
	segHeaderLen = 4 + 1 + 1 // magic + version + kind

	kindMeta     byte = 1
	kindDocs     byte = 2
	kindPostings byte = 3
	kindTerms    byte = 4
	kindManifest byte = 5
	kindDocoff   byte = 6
	kindKeys     byte = 7

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

// maxDocCount is the document ceiling both decoders range against. DocIDs are
// dense from 0, so the count shares DocID's uint32 ceiling — the same limit Add
// enforces on the write side.
//
// The min runs in uint64, not int, for the reason Add's own ceiling comment
// gives: an untyped MaxUint32 beside an int operand becomes an int, which does
// not fit on a 32-bit target and stops the package compiling under GOARCH=386
// or GOARCH=arm. Widened, the constant folds to maxInt there, which is the
// right answer — an int that narrow cannot address more documents than that.
const maxDocCount = int(min(uint64(maxInt), math.MaxUint32))

// segSection names one file inside a segment directory and the kind byte its
// frame carries.
type segSection struct {
	name string
	kind byte
}

// segSections are the files a segment directory holds, in write order, and the
// only entries weft's writer ever creates inside one. The writer, the reader
// and Commit's ownership check all read this one list, so adding a section is a
// single edit rather than four that can drift apart. Milestone 3 added docoff
// and keys through exactly that one edit, which is the claim this list was
// written to make good on.
var segSections = []segSection{
	{metaFile, kindMeta},
	{docsFile, kindDocs},
	{postingsFile, kindPostings},
	{termsFile, kindTerms},
	{docoffFile, kindDocoff},
	{keysFile, kindKeys},
}

// ---------------------------------------------------------------------------
// writing
// ---------------------------------------------------------------------------

// segWriter streams one framed section file to disk, fsynced on close.
//
// An earlier version accumulated the whole section in memory and wrote it at
// the end, on the grounds that the section describes an index which is itself
// entirely in memory. Merge is what retires that argument: it reads segments
// from disk and writes a segment to disk, and the corpus it moves is precisely
// the one that does not fit. Nothing about a merge needs the result in RAM, so
// the writer must not insist on it.
//
// Two things the buffer used to provide for free are kept explicitly. n is the
// running byte count, which is what off() returns and what the terms index and
// both v2 offset tables record; and crc is the running frame checksum, folded
// forward as bytes leave rather than computed over a finished buffer.
//
// err holds the first write failure and every method after it is a no-op, so
// the encoders stay free of error plumbing and close reports once — the same
// contract the buffered version had.
type segWriter struct {
	f   *os.File
	w   *bufio.Writer
	n   int    // bytes emitted, frame header included
	crc uint32 // running frame checksum
	err error  // first write error; close reports it

	// unit is the running checksum of the unit currently open — see beginUnit.
	// The frame checksum above covers the whole file, which means verifying it
	// costs a full read; these cover one record, block or entry each, so a
	// reader that touches one can check one.
	unit   uint32
	inUnit bool

	// scratch backs varint encoding and the chunked string checksum. A fixed
	// array on the struct, so neither allocates: crc32.Update needs a []byte
	// and converting a document's text to one would copy every byte of the
	// corpus, which is the cost this type was just rewritten to stop paying.
	scratch [512]byte
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
	// 32 KiB rather than bufio's 4 KiB default: a segment writes six of these
	// at once, so the buffers together stay under 200 KiB while cutting the
	// write syscalls per section by eight.
	w := &segWriter{f: f, w: bufio.NewWriterSize(f, 1<<15)}
	w.write(segMagic)
	w.uvarint(formatVersion)
	w.scratch[0] = kind
	w.write(w.scratch[:1])
	return w, nil
}

// write emits b, folding it into the frame checksum and into the open unit's.
// Every other encoder funnels through here or through writeString, so a unit
// cannot be left half covered by someone adding a fifth encoder beside them.
func (w *segWriter) write(b []byte) {
	if w.err != nil {
		return
	}
	w.crc = crc32.Update(w.crc, segCRC, b)
	if w.inUnit {
		w.unit = crc32.Update(w.unit, segCRC, b)
	}
	n, err := w.w.Write(b)
	w.n += n
	w.err = err
}

// writeString emits s without turning it into a byte slice.
//
// crc32.Update takes []byte and bufio can take the string directly, so the
// checksum is run over a fixed scratch buffer in chunks. []byte(s) would be
// correct and would allocate a second copy of every document's text — the
// whole corpus, once, per commit.
func (w *segWriter) writeString(s string) {
	if w.err != nil {
		return
	}
	for i := 0; i < len(s); i += len(w.scratch) {
		n := copy(w.scratch[:], s[i:])
		w.crc = crc32.Update(w.crc, segCRC, w.scratch[:n])
		if w.inUnit {
			w.unit = crc32.Update(w.unit, segCRC, w.scratch[:n])
		}
	}
	n, err := w.w.WriteString(s)
	w.n += n
	w.err = err
}

// The varint encoders share the scratch array rather than declaring a local
// one. A local escapes — write hands the slice to bufio, and escape analysis
// cannot see that bufio copies it — so a ten-byte allocation lands on every
// call. That is per posting, twice, which on a corpus of any size is the
// dominant allocation in a commit: measured at 35 MB writing an 8.8 MB segment.
// Reuse is safe because nothing here retains the bytes past the Write.
func (w *segWriter) uvarint(v uint64) {
	w.write(w.scratch[:binary.PutUvarint(w.scratch[:], v)])
}

func (w *segWriter) varint(v int64) {
	w.write(w.scratch[:binary.PutVarint(w.scratch[:], v)])
}

func (w *segWriter) str(s string) {
	w.uvarint(uint64(len(s)))
	w.writeString(s)
}

// beginUnit opens a checksummed unit, binding seed into it before any content.
//
// The seed is what makes the checksum say more than "these bytes are intact".
// A document record does not carry its own DocID — its position in the file is
// what named it — so a lazy reader following a damaged offset table would
// decode a perfectly healthy record under someone else's id and return a
// plausible wrong answer. Binding the id in means the record proves which
// document it is, and position stops being the only witness. Postings blocks
// take their own file offset for the same reason, terms entries their index.
// The seed goes through scratch rather than a local array for the reason the
// varint encoders give: a local escapes, and this runs once per record, once
// per block and once per terms entry.
func (w *segWriter) beginUnit(seed uint64) {
	binary.LittleEndian.PutUint64(w.scratch[:8], seed)
	w.unit, w.inUnit = crc32.Update(0, segCRC, w.scratch[:8]), true
}

// endUnit seals the unit with its checksum. The checksum bytes are outside the
// unit they seal, so inUnit is cleared before they are written — but they are
// inside the frame, so they still go through write.
func (w *segWriter) endUnit() {
	c := w.unit
	w.inUnit = false
	binary.LittleEndian.PutUint32(w.scratch[:4], c)
	w.write(w.scratch[:4])
}

// off is the absolute file offset of the next byte written, header included.
func (w *segWriter) off() int { return w.n }

// close seals the frame with its checksum and flushes to disk. The Sync is
// not optional: the manifest rename publishes this file, and a rename can
// survive a crash that unsynced contents did not — a manifest pointing at
// hollow files is exactly the mixed state Commit promises cannot exist.
//
// The frame checksum is written straight to the buffered writer rather than
// through write, because it cannot cover itself.
func (w *segWriter) close() error {
	if w.err == nil {
		binary.LittleEndian.PutUint32(w.scratch[:4], w.crc)
		_, w.err = w.w.Write(w.scratch[:4])
	}
	if w.err == nil {
		w.err = w.w.Flush()
	}
	if w.err == nil {
		w.err = w.f.Sync()
	}
	// Close runs either way: a failed write still leaves a descriptor open.
	cerr := w.f.Close()
	if w.err != nil {
		return w.err
	}
	return cerr
}

// writeSegment lays src down as one segment: every entry in segSections — meta,
// docs, postings, terms, docoff and keys — its own framed file in segRoot.
//
// Whatever lock src needs is the caller's to hold, and both callers hold ix.mu
// for writing across the whole encode: the statistics in meta and the documents
// they describe cannot then come from different moments, which is the atomicity
// FINDINGS section 4.4 asked for, statistics snapshot included.
//
// A source that cannot read something reports it on itself rather than here —
// see mergedSource.err — because a section file half-written is still a file
// this function has to close.
func writeSegment(segRoot *os.Root, src segSource) error {
	ws := make([]*segWriter, len(segSections))
	for i, s := range segSections {
		w, err := newSegWriter(segRoot, s.name, s.kind)
		if err != nil {
			for _, open := range ws[:i] {
				open.f.Close()
			}
			return err
		}
		ws[i] = w
	}
	meta, docs, post, terms, docoff, keys := ws[0], ws[1], ws[2], ws[3], ws[4], ws[5]

	// No locking here: Commit and Merge both hold the write lock for their whole
	// duration and sync.RWMutex is not reentrant, so taking a read lock inside
	// would deadlock against the caller that already holds the write one. An
	// earlier version locked here because Commit did not.
	func() {
		totalLen, vecDim := src.totals()
		meta.uvarint(uint64(src.count()))
		meta.uvarint(uint64(totalLen))
		meta.uvarint(uint64(vecDim))
		// docoff records where each record landed, so it is written from what
		// encodeDocs returns rather than by predicting record sizes twice.
		offs, lens, docKeys := encodeDocs(docs, src)
		encodeDocOffsets(docoff, offs, lens)
		encodeKeys(keys, docKeys)
		encodePostings(post, terms, src)
	}()

	for i, w := range ws {
		if err := w.close(); err != nil {
			for _, rest := range ws[i+1:] {
				rest.f.Close()
			}
			return fmt.Errorf("%s: %w", segSections[i].name, err)
		}
	}
	return nil
}

// encodeDocs lays src's documents out in segment-local id order — the position
// in the file is the id, so the id is never written and cannot disagree with
// itself. Ids inside a segment count from zero and the manifest is what says
// where the segment sits in the index, so a segment's bytes do not depend on
// what was committed before it — which is what lets an old generation survive a
// new one untouched.
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
//
// The returned offsets are absolute file positions, one per id, in id order —
// what the docoff section records. They are collected here rather than
// recomputed because a second implementation predicting record sizes would be
// a second thing to keep in step with this one. The lengths and keys ride back
// for the same reason: docoff and keys need them, and asking src again would
// decode the corpus twice.
func encodeDocs(w *segWriter, src segSource) (offs, lens []int, keys []string) {
	n := src.count()
	w.uvarint(uint64(n))
	offs, lens, keys = make([]int, n), make([]int, n), make([]string, n)
	for i := range n {
		d := src.doc(i)
		lens[i] = src.docLen(i)
		keys[i] = d.Key
		offs[i] = w.off()
		w.beginUnit(uint64(i))
		w.str(d.Key)
		w.str(d.Text)
		w.uvarint(uint64(lens[i]))
		w.uvarint(uint64(len(d.Vector)))
		for _, c := range d.Vector {
			// scratch, not a local: a local escapes — see the varint encoders —
			// and this is the innermost loop in the whole writer. On the
			// milestone 4 corpus it runs 148,232 × 768 times per commit.
			binary.LittleEndian.PutUint32(w.scratch[:4], math.Float32bits(c))
			w.write(w.scratch[:4])
		}
		w.uvarint(uint64(len(d.Links)))
		for _, l := range d.Links {
			w.str(l)
		}
		w.varint(d.Time.Unix())
		w.uvarint(uint64(d.Time.Nanosecond()))
		w.endUnit()
	}
	return offs, lens, keys
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
func encodePostings(post, terms *segWriter, src segSource) {
	keys := src.termList()
	post.uvarint(uint64(len(keys)))
	terms.uvarint(uint64(len(keys)))

	for i, t := range keys {
		// The entry's index seeds its checksum, so an entry cannot be moved to
		// another position and still verify.
		terms.beginUnit(uint64(i))
		terms.str(t)
		terms.uvarint(uint64(post.off()))
		terms.endUnit()

		pl := src.postings(t)
		post.uvarint(uint64((len(pl) + blockSize - 1) / blockSize))

		for start := 0; start < len(pl); start += blockSize {
			blk := pl[start:min(start+blockSize, len(pl))]
			maxTF, minDL := 0, maxInt
			for _, p := range blk {
				maxTF = max(maxTF, p.Freq)
				minDL = min(minDL, src.docLen(int(p.Doc)))
			}
			// A block's own file offset seeds its checksum: blocks are
			// independently decodable by design (D-001), so nothing else stops
			// one being served in place of another term's.
			post.beginUnit(uint64(post.off()))
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
			post.endUnit()
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

	// scratch backs the unit checksum's seed, for the reason segWriter.scratch
	// exists: crc32.Update takes a []byte and a local array escapes, which is a
	// heap allocation per record decoded — and scorer/vector decodes every
	// record in the corpus on every query.
	scratch [8]byte
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

// unit verifies the checksum standing at the reader's position against the
// bytes from start up to it, bound to seed. It advances past the checksum.
//
// This is the check that survives lazy loading. parseSection's frame checksum
// covers the whole file, so computing it means reading every byte — the cost
// this milestone exists to remove — and a reader that maps a segment and
// touches one record never computes it at all. Without a per-unit checksum the
// remaining defences are the decoder's semantic invariants, and those cannot
// see everything: decodePostings states outright that it never re-tokenizes a
// document to check its postings, so a flipped byte of document text is
// invisible to every rule in this file.
func (r *segReader) unit(what string, start int, seed uint64) error {
	binary.LittleEndian.PutUint64(r.scratch[:], seed)
	want := crc32.Update(crc32.Update(0, segCRC, r.scratch[:]), segCRC, r.b[start:r.off])
	got, err := r.u32(what + " checksum")
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s: %s at byte %d fails its checksum: %w", r.name, what, start, ErrCorrupt)
	}
	return nil
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
// handed a half-deleted segment and quietly overwrite it. An entry that is not
// a regular file — a directory, whose read fails with EISDIR, or a symlink,
// whose read fails because the root refuses to follow it out of the index
// directory — is the same judgement, and gets the same classification as the
// plain file Open refuses where a segment directory belongs: the manifest
// already called this directory an index, so a foreign layout inside it is
// damage rather than absence. A regular file that still will not open is the
// filesystem refusing us, not corruption, and reports as itself.
//
// The kind question is asked with Lstat rather than Stat, because Stat follows
// the very link that made the read fail: it either resolves out of the root and
// fails too, leaving a planted symlink reported as a raw path error, or it
// resolves to a regular file and calls the link one.
// frame says whether to compute the whole-file checksum first. It runs before
// the header is parsed, and that order is the point: parseSection carried the
// same rule for the same reason — a flipped bit in the version byte must not
// masquerade as ErrBadVersion. On the lazy path nobody computes the checksum,
// so that flip does report a wrong version, and Scrub is what calls it damage.
func openSection(root *os.Root, name string, kind byte, frame bool) (*segReader, []byte, error) {
	f, err := root.Open(name)
	if err != nil {
		fi, serr := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) || (serr == nil && !fi.Mode().IsRegular()) {
			return nil, nil, fmt.Errorf("%s: the manifest names this segment but no section file stands here: %w", name, ErrCorrupt)
		}
		return nil, nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	// A directory opens and then fails to Stat as a regular file, so the kind
	// question is asked here as well as above: root.Open succeeds on a
	// directory on some platforms and the read is what fails.
	if !fi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s: the manifest names this segment but no section file stands here: %w", name, ErrCorrupt)
	}
	if fi.Size() > int64(maxInt) {
		return nil, nil, fmt.Errorf("%s: %d bytes does not fit in memory on this platform: %w", name, fi.Size(), ErrCorrupt)
	}

	b, err := mapFile(f, int(fi.Size()))
	if err != nil {
		return nil, nil, err
	}
	// The descriptor closes on return and the mapping outlives it, which is
	// what mmap promises: the region stays valid until it is unmapped, and on
	// POSIX it also survives the file being unlinked. That is what lets Commit
	// prune a generation an open reader is still holding.
	if frame {
		if err := verifyFrame(name, b); err != nil {
			unmapFile(b) //nolint:errcheck,gosec // the error path already has one
			return nil, nil, err
		}
	}
	r, err := parseFrame(name, b, kind)
	if err != nil {
		unmapFile(b) //nolint:errcheck,gosec // the error path already has one
		return nil, nil, err
	}
	return r, b, nil
}

// parseSection verifies a file's frame, checksum included. The order of checks
// is deliberate: magic first, so a file that was never weft's reads as corrupt
// rather than as a version problem; CRC second, so a flipped bit in the version
// byte cannot masquerade as ErrBadVersion; version third, once the bytes are
// known intact; kind last, catching intact files standing in the wrong place.
//
// The manifest is read through here and stays that way — it is a handful of
// bytes and every Open reads all of them. Segment sections are not: verifying
// their checksum means reading every byte, so they go through parseFrame and
// their whole-file check belongs to Scrub. See verifyFrame.
func parseSection(name string, b []byte, kind byte) (*segReader, error) {
	if err := verifyFrame(name, b); err != nil {
		return nil, err
	}
	return parseFrame(name, b, kind)
}

// verifyFrame computes a file's whole-file checksum.
//
// Split out of parseSection because it is the one check whose cost is the size
// of the file. Milestone 2 could afford it on every Open; milestone 3 cannot,
// and moving it here rather than deleting it is what keeps Scrub able to make
// the same promise on request.
//
// It runs before the version is read, which is why parseFrame's callers can be
// handed a file whose version byte is damaged and report ErrBadVersion for what
// is really corruption. That is a real consequence of the split: on the lazy
// path a flipped version byte is a wrong version until Scrub says otherwise.
func verifyFrame(name string, b []byte) error {
	if len(b) < segHeaderLen+crc32.Size {
		return fmt.Errorf("%s: %d bytes is shorter than the file frame: %w", name, len(b), ErrCorrupt)
	}
	if !bytes.Equal(b[:len(segMagic)], segMagic) {
		return fmt.Errorf("%s: bad magic: %w", name, ErrCorrupt)
	}
	body := b[:len(b)-crc32.Size]
	if crc32.Checksum(body, segCRC) != binary.LittleEndian.Uint32(b[len(b)-crc32.Size:]) {
		return fmt.Errorf("%s: checksum mismatch: %w", name, ErrCorrupt)
	}
	return nil
}

// parseFrame validates a file's header — magic, version, kind — and returns a
// reader over its payload, without computing the whole-file checksum.
func parseFrame(name string, b []byte, kind byte) (*segReader, error) {
	if len(b) < segHeaderLen+crc32.Size {
		return nil, fmt.Errorf("%s: %d bytes is shorter than the file frame: %w", name, len(b), ErrCorrupt)
	}
	if !bytes.Equal(b[:len(segMagic)], segMagic) {
		return nil, fmt.Errorf("%s: bad magic: %w", name, ErrCorrupt)
	}
	body := b[:len(b)-crc32.Size]
	v, n := binary.Uvarint(body[len(segMagic):])
	if n <= 0 || len(segMagic)+n+1 > len(body) {
		return nil, fmt.Errorf("%s: unreadable version: %w", name, ErrCorrupt)
	}
	if v != formatVersion {
		return nil, fmt.Errorf("%s: format version %d, this build reads %d: %w", name, v, formatVersion, ErrBadVersion)
	}
	// The version is one byte, and only one byte. binary.Uvarint accepts
	// overlong encodings — 0x82 0x00 also decodes to 2 — which would make the
	// header seven bytes while segHeaderLen, and every absolute offset the
	// terms index and the two v2 seek sections record against it, still says
	// six. Two byte strings meaning the same index is one too many for a format
	// whose writer is expected to be deterministic.
	if n != segHeaderLen-len(segMagic)-1 {
		return nil, fmt.Errorf("%s: format version %d written in %d bytes, not 1: %w", name, v, n, ErrCorrupt)
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
	if docCount, err = r.intn("doc count", maxDocCount); err != nil {
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
	n, err := r.intn("document count", maxDocCount)
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
		d, dl, err := decodeDocRecord(r, i)
		if err != nil {
			return nil, err
		}
		if _, dup := ix.byKey[d.Key]; dup {
			return nil, fmt.Errorf("%s: duplicate key %q: %w", r.name, d.Key, ErrCorrupt)
		}
		if dl > maxInt-ix.totalLen {
			return nil, fmt.Errorf("%s: document lengths overflow their sum: %w", r.name, ErrCorrupt)
		}
		if vn := len(d.Vector); vn > 0 {
			if ix.vecDim == 0 {
				ix.vecDim = vn
			} else if vn != ix.vecDim {
				return nil, fmt.Errorf("%s: document %d vector is %d wide, corpus is %d: %w", r.name, i, vn, ix.vecDim, ErrCorrupt)
			}
		}

		ix.byKey[d.Key] = DocID(len(ix.docs))
		ix.docs = append(ix.docs, d)
		ix.docLen = append(ix.docLen, dl)
		ix.totalLen += dl
	}
	return ix, r.done()
}

// decodeDocRecord reads one document from wherever r is positioned and returns
// it with its stored token count. i names the record in error messages only.
//
// It is split out of decodeDocs because milestone 3 reads records out of order
// and one at a time — docoff hands over an offset, this decodes what stands
// there — and one decoder serving both paths is what keeps a lazily read
// document identical to an eagerly read one. Everything checkable from the
// record alone is checked here; everything that needs the corpus stays in
// decodeDocs, which is the only reason this returns rather than stores.
func decodeDocRecord(r *segReader, i int) (Document, int, error) {
	start := r.off
	var d Document
	var err error
	if d.Key, err = r.str("document key"); err != nil {
		return Document{}, 0, err
	}
	if d.Key == "" {
		return Document{}, 0, fmt.Errorf("%s: document %d has an empty key, which Add refuses: %w", r.name, i, ErrCorrupt)
	}
	if d.Text, err = r.str("document text"); err != nil {
		return Document{}, 0, err
	}
	dl, err := r.intn("document length", maxInt)
	if err != nil {
		return Document{}, 0, err
	}

	vn, err := r.intn("vector width", (len(r.b)-r.off)/4)
	if err != nil {
		return Document{}, 0, err
	}
	if vn > 0 {
		d.Vector = make([]float32, vn)
		for j := range d.Vector {
			bits, err := r.u32("vector component")
			if err != nil {
				return Document{}, 0, err
			}
			c := math.Float32frombits(bits)
			if f := float64(c); math.IsNaN(f) || math.IsInf(f, 0) {
				return Document{}, 0, fmt.Errorf("%s: document %d vector component %d is %v, which Add refuses: %w", r.name, i, j, c, ErrCorrupt)
			}
			d.Vector[j] = c
		}
	}

	ln, err := r.intn("link count", len(r.b)-r.off)
	if err != nil {
		return Document{}, 0, err
	}
	if ln > 0 {
		// Same ceiling as the document hint, for the same reason: a link is
		// one byte on disk and sixteen in a slice header.
		d.Links = make([]string, 0, min(ln, 1<<12))
		for range ln {
			l, err := r.str("link key")
			if err != nil {
				return Document{}, 0, err
			}
			d.Links = append(d.Links, l)
		}
	}

	sec, err := r.varint("time seconds")
	if err != nil {
		return Document{}, 0, err
	}
	nsec, err := r.intn("time nanoseconds", int(time.Second/time.Nanosecond)-1)
	if err != nil {
		return Document{}, 0, err
	}
	d.Time = time.Unix(sec, int64(nsec)).UTC()

	// Seeded with i, so this both proves the bytes are intact and proves the
	// record is the one asked for. Reading document 2's record as document 1 —
	// which is all a damaged docoff entry amounts to — fails here rather than
	// returning a healthy-looking wrong document.
	if err := r.unit(fmt.Sprintf("document %d", i), start, uint64(i)); err != nil {
		return Document{}, 0, err
	}
	return d, dl, nil
}

// decodeTermIndex reads the whole terms section into a term -> postings offset
// map, verifying each entry's checksum and the sorted order as it goes.
//
// Eager, and allowed to be: the terms index is bounded by the vocabulary, not
// the corpus — 2.7 MB against 626 MiB of documents on the milestone 4 index.
// The alternative is a third fixed-width table and a binary search, which is a
// format section bought before anything measured a need. If a corpus ever turns
// up whose vocabulary is the problem, this is the function that says so.
//
// The offsets are not checked against the postings file here. Nothing has read
// that file yet — checking would mean walking it, which is the cost being
// avoided — so a wrong offset surfaces as a block that fails its own checksum
// when the term is looked up, and Scrub is what checks them all.
func decodeTermIndex(terms *segReader) (map[string]int, error) {
	n, err := terms.intn("term count", len(terms.b))
	if err != nil {
		return nil, err
	}
	// Same capacity discipline as decodeDocs: a hostile count cannot make this
	// allocate more than the payload could back. An entry is at least 3 bytes.
	out := make(map[string]int, min(n, len(terms.b)/3, 1<<16))
	prev := ""
	for i := range n {
		start := terms.off
		term, err := terms.str("term")
		if err != nil {
			return nil, err
		}
		if term == "" {
			return nil, fmt.Errorf("%s: term %d is empty, which the tokenizer never emits: %w", terms.name, i, ErrCorrupt)
		}
		if term <= prev {
			return nil, fmt.Errorf("%s: term %q out of order after %q: %w", terms.name, term, prev, ErrCorrupt)
		}
		prev = term
		off, err := terms.intn("term offset", maxInt)
		if err != nil {
			return nil, err
		}
		if err := terms.unit(fmt.Sprintf("term entry %d", i), start, uint64(i)); err != nil {
			return nil, err
		}
		out[term] = off
	}
	return out, terms.done()
}

// decodeTermPostings reads one term's postings from wherever r is positioned.
//
// It is decodePostings' inner loop with the corpus-wide checks left out, and
// the difference is the point. Each block still re-derives its own metadata and
// still verifies its own checksum, because those are answerable from the block.
// What is not answerable from one term is whether a document's frequencies add
// up across every term that names it, so that check stays where it can be made:
// in Scrub, which reads all of them.
func decodeTermPostings(post *segReader, term string, docCount int) ([]Posting, error) {
	nblocks, err := post.intn("block count", len(post.b))
	if err != nil {
		return nil, err
	}
	if nblocks == 0 {
		return nil, fmt.Errorf("%s: term %q has no blocks; a term with no postings is never written: %w", post.name, term, ErrCorrupt)
	}

	var pl []Posting
	prev := uint64(0)
	for b := range nblocks {
		blockStart := post.off
		cnt, err := post.intn("block posting count", blockSize)
		if err != nil {
			return nil, err
		}
		if cnt == 0 || (b < nblocks-1 && cnt != blockSize) {
			return nil, fmt.Errorf("%s: term %q block %d holds %d postings: %w", post.name, term, b, cnt, ErrCorrupt)
		}
		maxDoc, err := post.uvarint("block maxDocID")
		if err != nil {
			return nil, err
		}
		maxTF, err := post.intn("block maxTF", maxInt)
		if err != nil {
			return nil, err
		}
		if _, err := post.intn("block minDocLen", maxInt); err != nil {
			return nil, err
		}

		gotMaxTF := 0
		for j := range cnt {
			delta, err := post.uvarint("posting docID delta")
			if err != nil {
				return nil, err
			}
			var id uint64
			switch {
			case j > 0:
				if delta == 0 {
					return nil, fmt.Errorf("%s: term %q repeats a docID; postings are strictly ascending: %w", post.name, term, ErrCorrupt)
				}
				if delta > math.MaxUint64-prev {
					return nil, fmt.Errorf("%s: term %q docID delta %d overflows past %d: %w", post.name, term, delta, prev, ErrCorrupt)
				}
				id = prev + delta
			case b == 0:
				id = delta
			default:
				id = delta
				if id <= prev {
					return nil, fmt.Errorf("%s: term %q block %d starts at document %d, block %d ended at %d; postings are strictly ascending: %w",
						post.name, term, b, id, b-1, prev, ErrCorrupt)
				}
			}
			if id >= uint64(docCount) {
				return nil, fmt.Errorf("%s: term %q names document %d of a %d-document segment: %w", post.name, term, id, docCount, ErrCorrupt)
			}
			prev = id
			freq, err := post.intn("posting frequency", maxInt)
			if err != nil {
				return nil, err
			}
			if freq == 0 {
				return nil, fmt.Errorf("%s: term %q in document %d has frequency 0, which Add never writes: %w", post.name, term, id, ErrCorrupt)
			}
			pl = append(pl, Posting{Doc: DocID(id), Freq: freq})
			gotMaxTF = max(gotMaxTF, freq)
		}
		if last := uint64(pl[len(pl)-1].Doc); last != maxDoc {
			return nil, fmt.Errorf("%s: term %q block %d records maxDocID %d, contents end at %d: %w", post.name, term, b, maxDoc, last, ErrCorrupt)
		}
		if gotMaxTF != maxTF {
			return nil, fmt.Errorf("%s: term %q block %d records maxTF %d, contents say %d: %w", post.name, term, b, maxTF, gotMaxTF, ErrCorrupt)
		}
		if err := post.unit(fmt.Sprintf("term %q block %d", term, b), blockStart, uint64(segHeaderLen+blockStart)); err != nil {
			return nil, err
		}
	}
	return pl, nil
}

// decodePostings walks the terms index and the postings file in lockstep,
// filling host.postings. The terms index leads: it holds the term strings and
// the absolute offset each entry must sit at, so a wrong offset is caught here
// rather than at the point of use, which is where the lazy path has to catch it.
//
// The D-001 block metadata is re-derived from each block's contents and
// compared against what the block records. Unread fields rot silently — D-001
// requires tests to catch that, and the decoder checking means the rot cannot
// even wait for a test run.
//
// What is deliberately not checked is that a term matches the text of the
// documents it names: nothing here re-tokenizes Text to compare per-document
// term frequencies. A doctored file can therefore file a document's postings
// under a token its text does not contain, and text search will return it for
// that token. That is accepted, for the same reason encodeDocs stores docLen
// rather than recomputing it — a segment records what was indexed, not what
// this build's tokenizer would index today, and a reader demanding the two
// agree would refuse every segment written before a tokenizer change. The
// invariants below are the ones the scorers actually rest on; term-to-text
// correspondence is not one of them, and buying it would cost a full
// re-tokenization of the corpus.
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
	// Bounding each posting on its own by docLen would let a doctored file give
	// a one-token document a frequency of 1 under two different terms — each
	// legal by itself, together describing a document that cannot exist. BM25
	// would then divide real frequencies by a length that never held them and
	// return scores that look reasonable and are wrong. So each document's
	// remaining token budget is tracked as its postings are read, and the total
	// is checked once they all have been.
	sumFreq := make([]int, len(host.docs))

	prevTerm := ""
	for i := 0; i < pn; i++ {
		entryStart := terms.off
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
		// A term string is whatever the file says it is — nothing here
		// re-derives one — so the checksum is the only thing standing between a
		// flipped byte and a corpus that answers a query nobody indexed.
		if err := terms.unit(fmt.Sprintf("term entry %d", i), entryStart, uint64(i)); err != nil {
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
			blockStart := post.off
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
				// A term cannot occur more often than its document has tokens,
				// and a document's terms cannot claim more tokens between them
				// either. Checked against what is left of docLen rather than
				// against docLen itself, which also keeps the running sum from
				// wrapping: freq is only ever added when it fits, so sumFreq
				// cannot pass docLen, let alone MaxInt. Summing first and
				// checking after would let four frequencies near MaxInt wrap
				// back onto a total that agrees with docLen, and postings Add
				// could never produce would load as healthy.
				if freq > host.docLen[id]-sumFreq[id] {
					return fmt.Errorf("%s: term %q occurs %d times in document %d, which has %d of its %d tokens left to account for: %w",
						post.name, term, freq, id, host.docLen[id]-sumFreq[id], host.docLen[id], ErrCorrupt)
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
			// Every rule above re-derives a block's metadata from its own
			// contents, which is why the byte-flip sweep finds no gap here
			// today. It finds none because Open decodes every block; a reader
			// that skips one re-derives nothing about it, and this is what
			// stands in that reader's place.
			if err := post.unit(fmt.Sprintf("term %q block %d", term, b), blockStart, uint64(segHeaderLen+blockStart)); err != nil {
				return err
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
