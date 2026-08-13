package engine

import (
	"bytes"
	"errors"
	"fmt"
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
	if err := root.Mkdir(seg, 0o755); err != nil {
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
	// claims they exist.
	syncDir(segRoot)

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
	segRoot, err := root.OpenRoot(segs[0])
	if err != nil {
		// Same reasoning as openSection: the manifest named it, so a segment
		// that is missing — or is a plain file standing where its directory
		// belongs, which is a foreign layout and not an index of ours — is
		// damage rather than an index that was never written. A directory that
		// is genuinely there and still will not open is the filesystem
		// refusing us, not corruption, and reports as itself.
		fi, serr := root.Stat(segs[0])
		if errors.Is(err, fs.ErrNotExist) || (serr == nil && !fi.IsDir()) {
			return nil, fmt.Errorf("open %s: the manifest names this segment but no directory stands there: %w", segs[0], ErrCorrupt)
		}
		return nil, fmt.Errorf("open %s: %w", segs[0], err)
	}
	defer segRoot.Close()

	metaR, err := openSection(segRoot, metaFile, kindMeta)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", segs[0], err)
	}
	docsR, err := openSection(segRoot, docsFile, kindDocs)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", segs[0], err)
	}
	postR, err := openSection(segRoot, postingsFile, kindPostings)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", segs[0], err)
	}
	termsR, err := openSection(segRoot, termsFile, kindTerms)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", segs[0], err)
	}

	docCount, totalLen, vecDim, err := decodeMeta(metaR)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", segs[0], err)
	}
	ix, err := decodeDocs(docsR)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", segs[0], err)
	}
	// meta is the statistics snapshot BM25 trusts, so it does not get to
	// disagree with the documents it describes.
	if docCount != len(ix.docs) || totalLen != ix.totalLen || vecDim != ix.vecDim {
		return nil, fmt.Errorf("open %s: meta says %d docs/%d tokens/%d dims, documents hold %d/%d/%d: %w",
			segs[0], docCount, totalLen, vecDim, len(ix.docs), ix.totalLen, ix.vecDim, ErrCorrupt)
	}
	if err := decodePostings(postR, termsR, ix); err != nil {
		return nil, fmt.Errorf("open %s: %w", segs[0], err)
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

// segFiles are the four section files a segment directory holds, and the only
// entries weft's writer ever creates inside one.
var segFiles = []string{metaFile, docsFile, postingsFile, termsFile}

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
// A commit that crashed before its rename must still be recoverable, so the
// test is what such a commit leaves behind rather than mere absence: a seg-*
// entry may exist, but only as a real directory holding nothing but regular
// section files, and MANIFEST.tmp may exist, but only holding what segWriter
// leaves — nothing, since it buffers until close, or a prefix of the frame it
// was writing when the machine died. Anything else and Commit refuses before it
// mutates a thing.
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
			b, err := root.ReadFile(e.Name())
			if err != nil {
				return err
			}
			// Either the file starts with weft's magic, or — for a write torn
			// shorter than the magic itself — the magic starts with the file. An
			// empty file, which is what a crash before close leaves, is the
			// second case.
			if !bytes.HasPrefix(b, segMagic) && !bytes.HasPrefix(segMagic, b) {
				return fmt.Errorf("%s was not written by weft and no manifest claims it, so weft will not delete it", e.Name())
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
				// The name alone is not enough: the RemoveAll that clears this
				// directory is recursive, so a *directory* called meta or docs
				// would take everything beneath it along.
				if !slices.Contains(segFiles, f.Name()) || !f.Type().IsRegular() {
					return fmt.Errorf("%s holds %s, which weft never wrote, and no manifest claims it, so weft will not delete it", e.Name(), f.Name())
				}
			}
		}
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
	root.Remove(manifestName + ".tmp")
	entries, err := readDir(root, ".")
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), segPrefix) && e.Name() != keep {
			root.RemoveAll(e.Name())
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
		d.Sync()
		d.Close()
	}
}
