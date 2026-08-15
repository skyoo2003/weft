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

// Commit writes the index's entire current state into dir as a new segment
// generation and atomically makes it the one Open sees.
//
// Atomicity is the MANIFEST flip: the segment's files are written and fsynced
// first, then the manifest is renamed into place, so a crash at any point
// leaves either the previous commit or this one — never a mix, and never a
// half-visible segment. That is the guarantee against process death; against
// power loss it is best-effort (fsync, no platform-specific write barriers),
// and docs/FORMAT.md states the boundary.
//
// Each commit rewrites the whole corpus and replaces the previous generation.
//
// ponytail: O(corpus) per commit. Incremental segments — write only new
// documents, read across many — arrive with milestone 3, where corpus size is
// the point; the manifest already carries a segment list, so that change
// extends the format's contents rather than the format.
//
// Commit is safe to run alongside Add and queries — the state is captured
// under the same read lock every query takes, so a commit is a point-in-time
// snapshot with its BM25 statistics intact — but not alongside another Commit
// on the same directory. weft has a single writer by design.
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
	gen, _, err := readManifest(root)
	if errors.Is(err, fs.ErrNotExist) {
		gen = 0
		// No manifest means nothing here is published. It does not mean this
		// directory is weft's.
		if err := refuseForeignEntries(root); err != nil {
			return fmt.Errorf("commit %s: %w", dir, err)
		}
	} else if err != nil {
		return fmt.Errorf("commit %s: %w", dir, err)
	}

	// readManifest has already established that the live segment is named for
	// gen and that gen+1 does not wrap, and refuseForeignEntries has done the
	// same job for a directory with no manifest at all, so the directory cleared
	// below is this commit's own debris and never the published one.
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
	if err := ix.writeSegment(segRoot); err != nil {
		return fmt.Errorf("commit %s: %w", seg, err)
	}
	// The segment directory's entries need to reach disk before the manifest
	// claims they exist — and so does the segment directory's own entry in dir.
	// Syncing only the inside leaves the rename free to land first, and a
	// manifest naming a directory whose entry never made it is exactly the
	// mixed state the rename exists to rule out.
	syncDir(segRoot)
	syncDir(root)

	// The temp manifest is created exclusively, so a leftover from a crashed
	// commit has to be cleared here rather than truncated open. Removing a
	// symlink removes the link, never its target.
	tmp := manifestName + ".tmp"
	if err := root.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("commit %s: clearing stale %s: %w", dir, tmp, err)
	}
	w, err := newSegWriter(root, tmp, kindManifest)
	if err != nil {
		return fmt.Errorf("commit %s: %w", dir, err)
	}
	w.uvarint(gen + 1)
	w.uvarint(1)
	w.str(seg)
	if err := w.close(); err != nil {
		return fmt.Errorf("commit %s: %s: %w", dir, manifestName, err)
	}
	if err := root.Rename(tmp, manifestName); err != nil {
		return fmt.Errorf("commit %s: publishing manifest: %w", dir, err)
	}
	syncDir(root)

	// The previous generation is unreachable now. Pruning it is best-effort:
	// the commit above is already durable, and a leftover directory nothing
	// names is invisible — failing to delete it must not turn a successful
	// commit into a reported failure.
	prune(root, seg)
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

	_, segs, err := readManifest(root)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	ix, err := loadSegment(root, segs[0], false)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", segs[0], err)
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
	for _, seg := range segs {
		if _, err := loadSegment(root, seg, true); err != nil {
			return fmt.Errorf("scrub %s: %w", seg, err)
		}
	}
	return nil
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
func readManifest(root *os.Root) (gen uint64, segs []string, err error) {
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
	for range n {
		s, err := r.str("segment name")
		if err != nil {
			return 0, nil, err
		}
		if !strings.HasPrefix(s, segPrefix) || strings.ContainsAny(s, `/\`) || s != filepath.Base(s) {
			return 0, nil, fmt.Errorf("%s: segment name %q: %w", manifestName, s, ErrCorrupt)
		}
		segs = append(segs, s)
	}
	if err := r.done(); err != nil {
		return 0, nil, err
	}
	if want := segDirName(gen); len(segs) != 1 || segs[0] != want {
		return 0, nil, fmt.Errorf("%s: generation %d names %q; format v1 writes exactly one segment, called %s: %w",
			manifestName, gen, segs, want, ErrCorrupt)
	}
	// Generation counts commits one at a time from a first commit that publishes
	// 1, so neither end of the counter is a state a writer produced. Zero
	// describes a commit that never happened: accepting it would load a foreign
	// v1 layout, and let the next Commit publish over it and then sweep away a
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

// prune removes every segment directory except keep, plus a stale manifest
// temp file. Best-effort by design: everything it deletes is unreachable —
// nothing reads a segment the manifest does not name — so failing to delete
// must not fail the commit that already succeeded.
//
// Only Commit calls this, and only after its own rename: weft's single writer
// is the one party that knows no other segment is being written right now.
func prune(root *os.Root, keep string) {
	root.Remove(manifestName + ".tmp") //nolint:errcheck,gosec // best-effort by design, see above
	entries, err := readDir(root, ".")
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), segPrefix) && e.Name() != keep {
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
