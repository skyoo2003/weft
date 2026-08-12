package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

	// The previous generation number comes from the manifest. A missing
	// manifest is a first commit; a corrupt one is not — guessing a generation
	// on top of a directory in an unknown state could orphan a commit the
	// caller believes exists, so Commit refuses and reports.
	gen, _, err := readManifest(dir)
	if errors.Is(err, fs.ErrNotExist) {
		gen = 0
	} else if err != nil {
		return fmt.Errorf("commit %s: %w", dir, err)
	}

	// readManifest has already established that the live segment is named for
	// gen, so the directory cleared below is this commit's own debris and never
	// the published one.
	seg := segDirName(gen + 1)
	segDir := filepath.Join(dir, seg)
	// A crash after writing segment files but before the manifest flip leaves
	// this directory half-written. It was never visible — no manifest names it
	// — so replacing it wholesale is safe.
	if err := os.RemoveAll(segDir); err != nil {
		return fmt.Errorf("commit %s: clearing stale segment: %w", dir, err)
	}
	if err := os.Mkdir(segDir, 0o755); err != nil {
		return fmt.Errorf("commit %s: %w", dir, err)
	}
	if err := ix.writeSegment(segDir); err != nil {
		return fmt.Errorf("commit %s: %w", seg, err)
	}
	// The segment directory's entries need to reach disk before the manifest
	// claims they exist.
	syncDir(segDir)

	// The temp manifest is created exclusively, so a leftover from a crashed
	// commit has to be cleared here rather than truncated open. Removing a
	// symlink removes the link, never its target.
	tmp := filepath.Join(dir, manifestName+".tmp")
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("commit %s: clearing stale %s: %w", dir, manifestName+".tmp", err)
	}
	w, err := newSegWriter(tmp, kindManifest)
	if err != nil {
		return fmt.Errorf("commit %s: %w", dir, err)
	}
	w.uvarint(gen + 1)
	w.uvarint(1)
	w.str(seg)
	if err := w.close(); err != nil {
		return fmt.Errorf("commit %s: %s: %w", dir, manifestName, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, manifestName)); err != nil {
		return fmt.Errorf("commit %s: publishing manifest: %w", dir, err)
	}
	syncDir(dir)

	// The previous generation is unreachable now. Pruning it is best-effort:
	// the commit above is already durable, and a leftover directory nothing
	// names is invisible — failing to delete it must not turn a successful
	// commit into a reported failure.
	prune(dir, seg)
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
	_, segs, err := readManifest(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	segDir := filepath.Join(dir, segs[0])

	metaR, err := openSection(segDir, metaFile, kindMeta)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", segs[0], err)
	}
	docsR, err := openSection(segDir, docsFile, kindDocs)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", segs[0], err)
	}
	postR, err := openSection(segDir, postingsFile, kindPostings)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", segs[0], err)
	}
	termsR, err := openSection(segDir, termsFile, kindTerms)
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
func readManifest(dir string) (gen uint64, segs []string, err error) {
	b, err := os.ReadFile(filepath.Join(dir, manifestName))
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
	return gen, segs, nil
}

// prune removes every segment directory except keep, plus a stale manifest
// temp file. Best-effort by design: everything it deletes is unreachable —
// nothing reads a segment the manifest does not name — so failing to delete
// must not fail the commit that already succeeded.
//
// Only Commit calls this, and only after its own rename: weft's single writer
// is the one party that knows no other segment is being written right now.
func prune(dir, keep string) {
	os.Remove(filepath.Join(dir, manifestName+".tmp"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), segPrefix) && e.Name() != keep {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// syncDir asks the OS to persist dir's entries — the existence of the files,
// where segWriter.close persisted their contents. Errors are dropped: syncing
// a directory is not supported everywhere (not on Windows, not on some
// filesystems), and where it fails the process-crash guarantee Commit
// documents still holds; only the power-loss window widens, which is already
// declared best-effort.
func syncDir(path string) {
	if d, err := os.Open(path); err == nil {
		d.Sync()
		d.Close()
	}
}
