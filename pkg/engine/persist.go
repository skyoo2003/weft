// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Sentinel errors from Open, and from Commit when an existing directory is
// unreadable. Both are properties of bytes on disk, not of the index.
var (
	// ErrCorrupt reports a file that failed its checksum, ended mid-value, or
	// decoded into a state the write path could never have produced. Callers
	// get an error, never a panic and never a plausible-looking index that
	// violates an invariant the scorers rely on.
	ErrCorrupt = errors.New("engine: index file is corrupt")

	// ErrBadVersion reports a file written by a format this build does not
	// read. Refusing outright beats guessing: a version is only bumped when
	// the bytes mean something different, and "probably compatible" is how a
	// wrong index gets loaded silently.
	ErrBadVersion = errors.New("engine: unsupported index format version")
)

const (
	manifestName = "MANIFEST"
	segPrefix    = "seg-"

	metaFile     = "meta"
	docsFile     = "docs"
	postingsFile = "postings"
	termsFile    = "terms"

	// Format v2's seek structures. docoff maps a DocID to its record's offset
	// in docs; keys maps a sorted Key to its DocID. Both exist so a lazy reader
	// can reach one document without decoding the ones in front of it, which
	// v1's layout made impossible rather than merely slow.
	docoffFile = "docoff"
	keysFile   = "keys"
)

// segDirName names the directory of one segment generation.
func segDirName(gen uint64) string { return fmt.Sprintf("%s%06d", segPrefix, gen) }

// writeManifest publishes gen and its segment list, atomically.
//
// The temp file is created exclusively, so a leftover from a crashed writer has
// to be cleared rather than truncated open — and removing a symlink removes the
// link, never its target. The rename is the commit point, for a merge exactly as
// for a commit.
func writeManifest(root *os.Root, gen uint64, segs []segInfo) error {
	tmp := manifestName + ".tmp"
	if err := root.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clearing stale %s: %w", tmp, err)
	}
	w, err := newSegWriter(root, tmp, kindManifest)
	if err != nil {
		return err
	}
	w.uvarint(gen)
	w.uvarint(uint64(len(segs)))
	for _, s := range segs {
		w.str(s.name)
		w.uvarint(uint64(s.base))
		w.uvarint(uint64(s.count))
	}
	if err := w.close(); err != nil {
		return fmt.Errorf("%s: %w", manifestName, err)
	}
	if err := root.Rename(tmp, manifestName); err != nil {
		return fmt.Errorf("publishing manifest: %w", err)
	}
	syncDir(root)
	return nil
}

// segInfo is one entry in the manifest's segment list: where the segment's
// files are, and which slice of the index's DocID space it owns.
type segInfo struct {
	name  string
	base  DocID
	count int
}

// Commit writes everything added since the last commit into dir as a new
// segment generation and atomically publishes it.
//
// Atomicity is the MANIFEST flip: the segment's files are written and fsynced
// first, then the manifest is renamed into place, so a crash at any point
// leaves either the previous commit or this one — never a mix, and never a
// half-visible segment. That is the guarantee against process death; against
// power loss it is best-effort (fsync, no platform-specific write barriers),
// and docs/FORMAT.md states the boundary.
//
// A commit writes only the documents added since the last one, and the previous
// generations' files are not touched. What it costs is therefore the size of
// the addition rather than the size of the corpus — D-003's repayment, whose
// trigger was this milestone. The previous generation's bytes surviving a new
// commit is asserted, not assumed: rewriting them identically would look the
// same from outside and would be exactly the work being removed.
//
// On success the new segment joins the index in place, so a second Commit does
// not write the same documents again.
//
// ponytail: Commit holds the write lock for its whole duration, so queries wait
// on it where before they ran alongside. The ceiling is the time to write one
// commit's worth of new documents, which incremental commit is what bounds. The
// upgrade is to encode under the read lock and swap under the write lock,
// counting the documents captured so a concurrent Add is neither written twice
// nor dropped — worth doing when a load test shows the pause, and not before.
//
// Commit is not safe alongside another Commit on the same directory. weft has a
// single writer by design.
func (ix *Index) Commit(dir string) error {
	// 0o700, not 0o755: this directory holds the caller's corpus, and nothing
	// weft does needs another user on the machine to read it. Owner-only rather
	// than 0o750, because the segment files inside are written 0o644 — leaving
	// the group traversal bit set would hand the whole corpus to every other
	// member of the caller's primary group, which on a shared machine is not a
	// set weft gets to assume anything about.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	defer root.Close()

	// The previous generation number comes from the manifest. A missing
	// manifest is a first commit; a corrupt one is not — guessing a generation
	// on top of a directory in an unknown state could orphan a commit the
	// caller believes exists, so Commit refuses and reports.
	gen, live, err := readManifest(root)
	if errors.Is(err, fs.ErrNotExist) {
		gen, live = 0, nil
		// No manifest means nothing here is published. It does not mean this
		// directory is weft's.
		if err := refuseForeignEntries(root); err != nil {
			return fmt.Errorf("commit %s: %w", dir, err)
		}
	} else if err != nil {
		return fmt.Errorf("commit %s: %w", dir, err)
	}

	// The write lock, for the whole commit. See the ponytail note above.
	ix.mu.Lock()
	defer ix.mu.Unlock()

	// The destination has to be the directory this index's committed segments
	// came from. The count comparison below cannot tell two unrelated
	// directories of the same size apart, and committing into the wrong one
	// joins this index's pending documents to that directory's history: the live
	// object keeps answering from the segments it opened, a reopen answers from
	// the ones on disk, and the next Merge writes the disagreement down.
	//
	// By identity rather than by string, because the same directory reached
	// through two paths is still the same directory. An index with nothing
	// committed has no directory yet and may pick any.
	if ix.dir != "" {
		same, err := sameDir(ix.dir, dir)
		if err != nil {
			return fmt.Errorf("commit %s: %w", dir, err)
		}
		if !same {
			return fmt.Errorf("commit %s: this index holds the segments of %s: %w", dir, ix.dir, ErrCorrupt)
		}
	}

	// The manifest is the authority on what is already stored, and the index in
	// memory has to agree with it — otherwise the segment written below would
	// be given a base the directory does not expect, and the ids of everything
	// in it would be wrong. A disagreement means this index was not the one
	// that wrote this directory.
	var stored uint64
	if n := len(live); n > 0 {
		stored = uint64(live[n-1].base) + uint64(live[n-1].count)
	}
	if stored != uint64(ix.base) {
		return fmt.Errorf("commit %s: the directory holds %d documents, this index has %d committed: %w",
			dir, stored, ix.base, ErrCorrupt)
	}

	// Nothing pending: an empty generation would grow the manifest and the
	// segment list without holding a document, and nothing bounds how often a
	// caller may ask. Every point query walks that list, so the cost of a no-op
	// Commit would land on every read after it.
	//
	// Checked after the agreement above, not before, so a Commit against the
	// wrong directory still reports rather than quietly succeeding. And only
	// when a generation already exists: the first Commit on an empty index is
	// how an empty index gets written at all, which restore asks for.
	if len(ix.docs) == 0 && gen > 0 {
		return nil
	}

	// readManifest has already established that the newest live segment is
	// named for gen and that gen+1 does not wrap, and refuseForeignEntries has
	// done the same job for a directory with no manifest at all, so the
	// directory cleared below is this commit's own debris and never a published
	// one.
	seg := segDirName(gen + 1)
	// A crash after writing segment files but before the manifest flip leaves
	// this directory half-written. It was never visible — no manifest names it
	// — so replacing it wholesale is safe.
	if err := root.RemoveAll(seg); err != nil {
		return fmt.Errorf("commit %s: clearing stale segment: %w", dir, err)
	}
	if err := root.Mkdir(seg, 0o700); err != nil {
		return fmt.Errorf("commit %s: %w", dir, err)
	}
	segRoot, err := root.OpenRoot(seg)
	if err != nil {
		return fmt.Errorf("commit %s: %w", dir, err)
	}
	defer segRoot.Close()
	if err := writeSegment(segRoot, &pendingSource{ix: ix}); err != nil {
		return fmt.Errorf("commit %s: %w", seg, err)
	}
	// The segment directory's entries need to reach disk before the manifest
	// claims they exist — and so does the segment directory's own entry in dir.
	// Syncing only the inside leaves the rename free to land first, and a
	// manifest naming a directory whose entry never made it is exactly the
	// mixed state the rename exists to rule out.
	syncDir(segRoot)
	syncDir(root)

	published := append(slices.Clone(live), segInfo{name: seg, base: ix.base, count: len(ix.docs)})
	if err := writeManifest(root, gen+1, published); err != nil {
		return fmt.Errorf("commit %s: %w", dir, err)
	}

	// The commit is durable. Adopt what was just written so a second Commit
	// does not write these documents again, and so reads of them go through the
	// mapping like every other committed document. Failing here leaves the
	// directory correct and the in-memory index stale, which is why it is an
	// error rather than something swallowed: the caller has to Open again.
	if err := ix.adopt(root, published[len(published)-1]); err != nil {
		return fmt.Errorf("commit %s: %w", seg, err)
	}
	// Absolute, for the reason openIndex gives.
	if ix.dir, err = filepath.Abs(dir); err != nil {
		return fmt.Errorf("commit %s: %w", dir, err)
	}

	// The previous generation is unreachable now. Pruning it is best-effort:
	// the commit above is already durable, and a leftover directory nothing
	// names is invisible — failing to delete it must not turn a successful
	// commit into a reported failure.
	prune(root, published)
	return nil
}

// adopt maps the segment a commit just published and folds it into the index,
// clearing the pending documents it now holds. Requires ix.mu.
func (ix *Index) adopt(root *os.Root, info segInfo) error {
	s, err := openSegment(root, info.name, info.base)
	if err != nil {
		return err
	}
	ix.segs = append(ix.segs, s)
	ix.base = DocID(uint64(info.base) + uint64(info.count))
	// Cleared, not just reslept to zero: the backing array survives, and every
	// Document still in it keeps its text, vector and links reachable. Reads have
	// moved to the mapping by now, so leaving them would hold the corpus twice —
	// once in the page cache where this milestone put it and once on the Go heap
	// it was put there to stay off.
	clear(ix.docs)
	ix.docs = ix.docs[:0]
	ix.docLen = ix.docLen[:0]
	ix.totalLen = 0
	clear(ix.byKey)
	clear(ix.postings)
	return nil
}

// Open loads the last committed generation from dir.
//
// The returned index is a fully in-memory copy, independent of the files it
// came from and ready for further Adds and Commits. Lazy loading is milestone
// 3's work.
//
// A missing directory or manifest reports fs.ErrNotExist. Damaged or foreign
// files report ErrCorrupt; files from a different format version report
// ErrBadVersion. Segment directories the manifest does not name — the debris
// of a commit that never finished — are ignored.
//
// Open never deletes anything. Sweeping the debris is Commit's job, because
// "the manifest does not name it" is also true of the segment a commit that is
// still running has written but not yet published: a reader that swept would
// delete a live writer's work and leave the directory naming a segment that no
// longer exists. Until the next Commit, unnamed debris costs disk and nothing
// else.
func Open(dir string) (*Index, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	defer root.Close()

	// A segment the manifest names and nobody can open is the one failure that
	// can mean the directory is fine and this reader is simply late: Merge
	// publishes its replacement before it prunes, so the manifest on disk now
	// names segments that are all there. Re-read it and try again.
	//
	// Bounded, because a merge finishes. A manifest that keeps naming a segment
	// nothing can open is damage, and has to be reported as damage rather than
	// spun on.
	for attempt := 0; ; attempt++ {
		_, segs, err := readManifest(root)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", dir, err)
		}
		ix, err := mapGeneration(root, dir, segs)
		if err == nil {
			return ix, nil
		}
		if !errors.Is(err, errSegmentGone) || attempt+1 == openAttempts {
			return nil, err
		}
	}
}

// openAttempts is how many times Open re-reads the manifest before calling a
// vanished segment damage. Two retries covers a reader overtaken by a merge and
// then by a second one; a third would be a directory nobody is merging.
const openAttempts = 3

// mapGeneration maps every segment one manifest names, in order.
func mapGeneration(root *os.Root, dir string, segs []segInfo) (*Index, error) {
	ix := New()
	var err error
	for _, info := range segs {
		s, err := openSegment(root, info.name, info.base)
		if err != nil {
			ix.Close() //nolint:errcheck // already returning an error
			return nil, fmt.Errorf("open %s: %w", info.name, err)
		}
		// The manifest says how many documents a segment holds and meta says it
		// again. They are written by the same commit and read by different
		// paths, so a disagreement means one of them is not describing this
		// segment — and the manifest's number is what every base after it was
		// computed from.
		if s.count != info.count {
			ix.segs = append(ix.segs, s)
			ix.Close() //nolint:errcheck // ditto
			return nil, fmt.Errorf("open %s: the manifest says %d documents, meta says %d: %w",
				info.name, info.count, s.count, ErrCorrupt)
		}
		ix.segs = append(ix.segs, s)
		// The pending segment starts where the last committed one ends, so ids
		// stay dense across the join and an Add after an Open cannot collide
		// with a document already on disk.
		ix.base = DocID(uint64(s.base) + uint64(s.count))
		// One width per index, not per segment. Add enforces it while a corpus
		// is being built and nothing re-checked it while one is being assembled,
		// so a copied or doctored manifest could join two embedding spaces into
		// an index that opens cleanly and then aborts on whichever vector query
		// reaches across the join.
		if s.vecDim != 0 {
			if ix.vecDim != 0 && s.vecDim != ix.vecDim {
				ix.Close() //nolint:errcheck // already returning an error
				return nil, fmt.Errorf("open %s: vectors are %d wide, the segments before it hold %d: %w",
					info.name, s.vecDim, ix.vecDim, ErrCorrupt)
			}
			ix.vecDim = s.vecDim
		}
	}
	// Absolute, because this is remembered and used later: Merge reopens it and
	// Commit stats it, and a relative path stops naming this index the moment the
	// process changes directory. The mappings would still be valid, so the
	// failure would be caused by nothing but the deferred resolution.
	if ix.dir, err = filepath.Abs(dir); err != nil {
		ix.Close() //nolint:errcheck // already returning an error
		return nil, err
	}
	return ix, nil
}

// Scrub verifies every byte of the last committed generation in dir and reports
// the first thing wrong with it.
//
// It exists because Open stopped doing this. Milestone 2 verified an index
// completely on every Open and got it for free, since Open read every byte
// anyway; a lazy reader does not, and cannot verify what it never loads. The
// checks did not go away, they split — the frame header and meta at Open, each
// record, block and entry against its own checksum as it is touched, and
// everything here.
//
// The gap that leaves is worth stating plainly: a unit nothing ever reads is
// never verified. Rot in a document no query reaches will sit there until
// somebody runs this. Scrub is what a caller runs after a suspicious crash, on
// a schedule, or before trusting a copied directory — not on the hot path.
//
// It reports ErrCorrupt for damage, ErrBadVersion for a format this build does
// not read, and fs.ErrNotExist for a directory with no manifest, exactly as
// Open does.
func Scrub(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("scrub %s: %w", dir, err)
	}
	defer root.Close()

	_, segs, err := readManifest(root)
	if err != nil {
		return fmt.Errorf("scrub %s: %w", dir, err)
	}
	// Keys are unique across the whole index and not merely inside a segment.
	// Resolve rests on that — it is why the first segment to answer is treated
	// as the only one that can — and Add enforces it against the segments, so a
	// duplicate cannot arrive through the API and can arrive by copying a
	// segment directory in. A copied directory is what this function is for.
	//
	// This holds one entry per key, which is the vocabulary of keys rather than
	// the corpus, and small beside the segment loadSegment already materializes.
	keys := make(map[string]string, 0)
	vecDim := 0
	for _, seg := range segs {
		ix, err := loadSegment(root, seg.name, true)
		if err != nil {
			return fmt.Errorf("scrub %s: %w", seg.name, err)
		}
		// The manifest's count and meta's are written by the same commit and
		// read by different paths, and every base after this segment was
		// computed from the manifest's. Open compares the two; without the same
		// comparison here, Scrub reports success for a directory Open refuses,
		// which is backwards for the check a caller runs before trusting one.
		if len(ix.docs) != seg.count {
			return fmt.Errorf("scrub %s: the manifest says %d documents, the segment holds %d: %w",
				seg.name, seg.count, len(ix.docs), ErrCorrupt)
		}
		// The same one-width-per-index rule Open now applies. loadSegment
		// establishes each segment's width from its own documents; nothing
		// compared one segment's against the next's.
		if ix.vecDim != 0 {
			if vecDim != 0 && ix.vecDim != vecDim {
				return fmt.Errorf("scrub %s: vectors are %d wide, an earlier segment's are %d: %w",
					seg.name, ix.vecDim, vecDim, ErrCorrupt)
			}
			vecDim = ix.vecDim
		}
		for key := range ix.byKey {
			if prev, dup := keys[key]; dup {
				return fmt.Errorf("scrub %s: key %q is also held by %s: %w",
					seg.name, key, prev, ErrCorrupt)
			}
			keys[key] = seg.name
		}
	}
	return nil
}

// sameDir reports whether two paths name the same directory. Both must exist:
// Commit creates its destination before asking.
func sameDir(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

// loadSegment reads one segment directory into an Index. frame says whether to
// compute each section's whole-file checksum — the one check whose cost is the
// size of the index, and therefore the single difference between what Open does
// and what Scrub does.
//
// Both paths run every other check. A segment that loads here has had its
// documents, postings, block metadata and both seek sections re-derived and
// compared against what the file records; frame only adds the ability to notice
// damage in bytes no decoder reads.
func loadSegment(root *os.Root, seg string, frame bool) (*Index, error) {
	segRoot, err := root.OpenRoot(seg)
	if err != nil {
		// Same reasoning as openSection: the manifest named it, so a segment
		// that is missing — or is a plain file standing where its directory
		// belongs, or a symlink the root refuses to follow out of the index
		// directory, both foreign layouts and not an index of ours — is damage
		// rather than an index that was never written. A directory that is
		// genuinely there and still will not open is the filesystem refusing
		// us, not corruption, and reports as itself. Lstat, not Stat, for the
		// reason openSection gives: Stat follows the link in question.
		fi, serr := root.Lstat(seg)
		if errors.Is(err, fs.ErrNotExist) || (serr == nil && !fi.IsDir()) {
			return nil, fmt.Errorf("the manifest names this segment but no directory stands there: %w", ErrCorrupt)
		}
		return nil, err
	}
	defer segRoot.Close()

	rs := make([]*segReader, len(segSections))
	// Every mapping is released before this returns. The decoders below copy
	// what they keep — segReader.str allocates a string, vectors are built with
	// make — so nothing in the returned Index points into a region that is
	// about to be gone. That stops being true the moment the index reads
	// lazily, and the mappings will have to outlive this function then.
	maps := make([][]byte, 0, len(segSections))
	defer func() {
		for _, b := range maps {
			unmapFile(b) //nolint:errcheck,gosec // nothing left to do about it here
		}
	}()
	for i, s := range segSections {
		r, b, err := openSection(segRoot, s.name, s.kind, frame)
		if err != nil {
			return nil, err
		}
		maps = append(maps, b)
		rs[i] = r
	}
	metaR, docsR, postR, termsR, docoffR, keysR := rs[0], rs[1], rs[2], rs[3], rs[4], rs[5]

	docCount, totalLen, vecDim, err := decodeMeta(metaR)
	if err != nil {
		return nil, err
	}
	ix, err := decodeDocs(docsR)
	if err != nil {
		return nil, err
	}
	// The seek sections are checked against the documents they claim to index.
	// Nothing on this path reads them afterwards — the index is still decoded
	// eagerly — so without this they would be write-only until the lazy reader
	// arrives and discovers they have been wrong all along. That is D-001's rot
	// problem in a new place, and it gets D-001's answer: re-derive what the
	// section records and refuse a disagreement.
	if err := verifySeekSections(docoffR, keysR, docsR, ix); err != nil {
		return nil, err
	}
	// meta is the statistics snapshot BM25 trusts, so it does not get to
	// disagree with the documents it describes.
	if docCount != len(ix.docs) || totalLen != ix.totalLen || vecDim != ix.vecDim {
		return nil, fmt.Errorf("meta says %d docs/%d tokens/%d dims, documents hold %d/%d/%d: %w",
			docCount, totalLen, vecDim, len(ix.docs), ix.totalLen, ix.vecDim, ErrCorrupt)
	}
	if err := decodePostings(postR, termsR, ix); err != nil {
		return nil, err
	}
	return ix, nil
}

// readManifest reads and decodes dir's manifest. Segment names come off disk,
// so they are validated like everything else that does: a name with a path
// separator in it would let a doctored manifest read files outside dir.
//
// Version 1's writer contract — exactly one segment, named from the generation
// — is enforced here rather than at each call site, because the two callers
// need it for different reasons and only one of them used to check. Open needs
// the count. Commit needs the name: it clears the directory it is about to
// write, so a manifest whose generation and segment name disagree can aim that
// RemoveAll at the segment that is still published, destroying the live commit
// before its replacement is durable.
func readManifest(root *os.Root) (gen uint64, segs []segInfo, err error) {
	b, err := root.ReadFile(manifestName)
	if err != nil {
		// An entry of the wrong kind at the entry point is classified the way
		// openSection classifies one at a section's path, and for the same
		// reason: a directory or a symlink standing here is a foreign layout,
		// not an index that was never written, and reporting the raw EISDIR
		// would leave it neither ErrCorrupt nor fs.ErrNotExist — so Open's
		// caller gets no branch to take and Commit's "no manifest means a first
		// commit" reads it as neither. A manifest that is genuinely absent
		// still fails the Lstat and still reports fs.ErrNotExist, which is what
		// that branch is for.
		if fi, lerr := root.Lstat(manifestName); lerr == nil && !fi.Mode().IsRegular() {
			return 0, nil, fmt.Errorf("%s: not a regular file: %w", manifestName, ErrCorrupt)
		}
		return 0, nil, err
	}
	r, err := parseSection(manifestName, b, kindManifest)
	if err != nil {
		return 0, nil, err
	}
	if gen, err = r.uvarint("generation"); err != nil {
		return 0, nil, err
	}
	n, err := r.intn("segment count", len(r.b))
	if err != nil {
		return 0, nil, err
	}
	// Bases are checked as they are read rather than afterwards: a segment list
	// that does not tile [0, total) contiguously and in order would give two
	// segments overlapping ids, and Index.segFor would answer with whichever it
	// walked into first — a wrong document, not an error.
	nextBase := uint64(0)
	for range n {
		name, err := r.str("segment name")
		if err != nil {
			return 0, nil, err
		}
		if !strings.HasPrefix(name, segPrefix) || strings.ContainsAny(name, `/\`) || name != filepath.Base(name) {
			return 0, nil, fmt.Errorf("%s: segment name %q: %w", manifestName, name, ErrCorrupt)
		}
		base, err := r.uvarint("segment base")
		if err != nil {
			return 0, nil, err
		}
		count, err := r.intn("segment document count", maxDocCount)
		if err != nil {
			return 0, nil, err
		}
		if base != nextBase {
			return 0, nil, fmt.Errorf("%s: segment %q starts at document %d, the one before it ended at %d: %w",
				manifestName, name, base, nextBase, ErrCorrupt)
		}
		if base > uint64(maxDocCount)-uint64(count) {
			return 0, nil, fmt.Errorf("%s: segment %q runs past the document ceiling: %w", manifestName, name, ErrCorrupt)
		}
		nextBase = base + uint64(count)
		segs = append(segs, segInfo{name: name, base: DocID(base), count: count})
	}
	if err := r.done(); err != nil {
		return 0, nil, err
	}
	if len(segs) == 0 {
		return 0, nil, fmt.Errorf("%s: generation %d names no segments; a commit always publishes one: %w", manifestName, gen, ErrCorrupt)
	}
	// Some published segment is named for the generation that published it, and
	// none is named for the next one. That is what Commit and Merge both need:
	// each clears seg-<gen+1> before writing it, so a manifest that already
	// named it would have that RemoveAll aimed at live data.
	//
	// Milestone 2 could say something stronger — the *last* segment is named for
	// gen — because a commit published exactly one. A merge publishes its result
	// at the front of the list, since it replaces the oldest run, so the
	// generation's own segment is no longer always last.
	written, next := segDirName(gen), segDirName(gen+1)
	found := false
	for _, s := range segs {
		if s.name == next {
			return 0, nil, fmt.Errorf("%s: generation %d already names %s, which the next write clears: %w",
				manifestName, gen, next, ErrCorrupt)
		}
		found = found || s.name == written
	}
	if !found {
		return 0, nil, fmt.Errorf("%s: generation %d names no segment called %s: %w", manifestName, gen, written, ErrCorrupt)
	}
	seen := make(map[string]struct{}, len(segs))
	for _, s := range segs {
		if _, dup := seen[s.name]; dup {
			return 0, nil, fmt.Errorf("%s: segment %q listed twice: %w", manifestName, s.name, ErrCorrupt)
		}
		seen[s.name] = struct{}{}
	}
	// Generation counts commits one at a time from a first commit that publishes
	// 1, so neither end of the counter is a state a writer produced. Zero
	// describes a commit that never happened: accepting it would load a foreign
	// layout, and let the next Commit publish over it and then sweep away a
	// seg-000000 weft never wrote. MaxUint64 is the other end, and refusing it is
	// also what keeps Commit's gen+1 from wrapping back onto zero — a wrapped
	// generation would aim the pre-write RemoveAll at seg-000000 and then publish
	// a generation lower than the one it replaced, breaking the strictly
	// increasing counter the format rests on.
	if gen == 0 || gen == math.MaxUint64 {
		return 0, nil, fmt.Errorf("%s: generation %d, which no sequence of commits could reach: %w", manifestName, gen, ErrCorrupt)
	}
	return gen, segs, nil
}

// refuseForeignEntries establishes that a directory with no manifest is
// weft's to overwrite, before Commit deletes anything in it.
//
// Commit removes every seg-* entry it finds — this generation's target before
// writing it, the rest in prune afterwards — and a manifest is what proves
// those names are weft's own. Without one there is nothing to say that
// "seg-000001" is debris rather than a caller's directory sitting under a name
// weft happens to reserve, and a first Commit aimed at, say, a home directory
// would recursively delete data it never wrote.
//
// The same holds for MANIFEST.tmp, the other name Commit deletes on sight.
//
// A commit that crashed before its rename must still be recoverable, so the test
// is what such a commit leaves behind rather than mere absence: a seg-* entry may
// exist, but only as a real directory holding nothing but regular section files,
// and MANIFEST.tmp may exist too — and every one of those files has to carry
// weft's magic, because a name and a file type are things a caller's own data can
// have by coincidence and four bytes of magic are not. Anything else and Commit
// refuses before it mutates a thing.
func refuseForeignEntries(root *os.Root) error {
	entries, err := readDir(root, ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch {
		case e.Name() == manifestName+".tmp":
			// A symlink is not a regular file here — ReadDir reports the link
			// itself — so one planted at this name is somebody else's, which is
			// also what stops the read below from following it.
			if !e.Type().IsRegular() {
				return fmt.Errorf("%s is not a file and no manifest claims it, so weft will not delete it", e.Name())
			}
			if err := refuseForeignFile(root, e.Name()); err != nil {
				return err
			}
		case !strings.HasPrefix(e.Name(), segPrefix):
			continue
		// Again: a symlink is not a directory here, so one standing at a
		// segment's name is somebody else's.
		case !e.IsDir():
			return fmt.Errorf("%s is not a segment directory and no manifest claims it, so weft will not delete it", e.Name())
		default:
			inner, err := readDir(root, e.Name())
			if err != nil {
				return err
			}
			for _, f := range inner {
				// The name alone is not enough, in either direction: the RemoveAll
				// that clears this directory is recursive, so a *directory* called
				// meta or docs would take everything beneath it along, and a plain
				// file under one of those names is only weft's if it says so.
				known := slices.ContainsFunc(segSections, func(s segSection) bool { return s.name == f.Name() })
				if !known || !f.Type().IsRegular() {
					return fmt.Errorf("%s holds %s, which weft never wrote, and no manifest claims it, so weft will not delete it", e.Name(), f.Name())
				}
				if err := refuseForeignFile(root, filepath.Join(e.Name(), f.Name())); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// refuseForeignFile reports a file under a name weft reserves whose first bytes
// are not the start of a weft frame.
//
// segWriter creates each file exclusively and buffers until close, so what a
// crashed commit leaves is an empty file or the head of the frame it was writing
// — either way a prefix of the magic, which is all this reads. The magic is the
// ownership signal and the only one available: the kind byte says which section
// a file is, not whose it is, and a torn write may stop before reaching it. A
// frame that is intact enough to carry the magic but broken past it is still
// weft's own debris, and Open's checksum is what refuses it as an index.
func refuseForeignFile(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, len(segMagic))
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	if !bytes.HasPrefix(segMagic, head[:n]) {
		return fmt.Errorf("%s was not written by weft and no manifest claims it, so weft will not delete it", name)
	}
	return nil
}

// readDir lists the entries of name under root.
func readDir(root *os.Root, name string) ([]fs.DirEntry, error) {
	d, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.ReadDir(-1)
}

// prune removes every segment directory the manifest does not name, plus a
// stale manifest temp file. Best-effort by design: everything it deletes is
// unreachable — nothing reads a segment the manifest does not name — so failing
// to delete must not fail the commit that already succeeded.
//
// The keep set is the whole published list, not just the newest. Incremental
// commit means older generations are still live, and deleting one would take
// the front of the corpus with it.
//
// Commit and Merge call this, and only after their own rename: weft's single
// writer is the one party that knows no other segment is being written right
// now.
func prune(root *os.Root, keep []segInfo) {
	root.Remove(manifestName + ".tmp") //nolint:errcheck,gosec // best-effort by design, see above
	live := make(map[string]struct{}, len(keep))
	for _, s := range keep {
		live[s.name] = struct{}{}
	}
	entries, err := readDir(root, ".")
	if err != nil {
		return
	}
	for _, e := range entries {
		if _, ok := live[e.Name()]; ok {
			continue
		}
		if e.IsDir() && strings.HasPrefix(e.Name(), segPrefix) {
			root.RemoveAll(e.Name()) //nolint:errcheck,gosec // best-effort by design, see above
		}
	}
}

// syncDir asks the OS to persist dir's entries — the existence of the files,
// where segWriter.close persisted their contents. Errors are dropped: syncing
// a directory is not supported everywhere (not on Windows, not on some
// filesystems), and where it fails the process-crash guarantee Commit
// documents still holds; only the power-loss window widens, which is already
// declared best-effort.
func syncDir(root *os.Root) {
	if d, err := root.Open("."); err == nil {
		d.Sync() //nolint:errcheck,gosec // dropped on purpose, see above
		d.Close()
	}
}
