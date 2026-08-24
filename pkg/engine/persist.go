// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

	// Format v3's approximate vector index: centroids, and the segment-local
	// DocIDs assigned to each. It is what lets a vector query score a partition
	// of the corpus rather than all of it — the half of "works past memory" that
	// milestone 3a left open, since mapping the vectors moved them off the Go
	// heap without reducing what a query touches.
	ivfFile = "ivf"
)

// segDirName names the directory of one segment generation.
func segDirName(gen uint64) string { return fmt.Sprintf("%s%06d", segPrefix, gen) }

// writeManifest publishes gen and its segment list, atomically.
//
// The temp file is created exclusively, so a leftover from a crashed writer has
// to be cleared rather than truncated open — and removing a symlink removes the
// link, never its target. The rename is the commit point, for a merge exactly as
// for a commit.
// deadN is how many tombstones the generation's `dead-<gen>` file holds. It
// rides here rather than being left to that file alone so the two have to agree:
// nothing names that file, so without a count over here one lost to a bad prune
// or a partial copy would read as "nothing was deleted" and hand back every
// document it named. Format v3 manifests carry no such field and read as zero.
func writeManifest(root *os.Root, gen uint64, segs []segInfo, deadN int) error {
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
	// After the segment list, so a v3 payload is a prefix of a v4 one. Nothing
	// reads it that way — the version decides what is there — but a format whose
	// sections only ever grow at the end is the one FORMAT.md §7.7 found cheap.
	w.uvarint(uint64(deadN))
	if err := w.close(); err != nil {
		return fmt.Errorf("%s: %w", manifestName, err)
	}
	if err := root.Rename(tmp, manifestName); err != nil {
		return fmt.Errorf("publishing manifest: %w", err)
	}
	syncDir(root)
	return nil
}

// writeDead publishes the tombstone set as gen's `dead-<gen>` file.
//
// Exclusively created, like every other file weft writes, so a leftover from a
// crashed writer has to be cleared rather than truncated open — and clearing it
// is safe because no manifest names gen yet. It is fsynced by segWriter.close
// before the manifest rename that publishes it, which is what puts it on the
// durable side of the commit point along with the segment.
//
// Always written, even for an empty set. The manifest's count is what a reader
// checks the file against, and "the count is zero so the file is optional" would
// make the one state that must not be ambiguous — no tombstones — the same state
// as a file that went missing.
func writeDead(root *os.Root, gen uint64, ids []DocID) error {
	name := deadFileName(gen)
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clearing stale %s: %w", name, err)
	}
	w, err := newSegWriter(root, name, kindDead)
	if err != nil {
		return err
	}
	encodeDead(w, ids)
	if err := w.close(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// readDead reads gen's tombstone list. want is what the manifest says it holds
// and total is the corpus size its ids are ranged against.
//
// A v3 generation has no such file and no count to demand one, so want is zero
// and this is not called at all — which is what makes a version 3 directory
// readable by this build with nothing converted.
//
// The frame checksum is verified here rather than deferred to Scrub, unlike the
// sections inside a segment. What that costs is the size of the tombstone set,
// and what it buys is that the one structure standing between a query and a
// deleted document cannot be quietly wrong.
func readDead(root *os.Root, gen uint64, want, total int) ([]DocID, error) {
	name := deadFileName(gen)
	r, b, err := openSection(root, name, kindDead, true)
	if err != nil {
		// Absence is damage here and never a window. A merge prunes the previous
		// generation's file, not this one's, so a manifest claiming tombstones
		// with no file to back them is a directory that has lost them — and
		// carrying on would resurrect exactly the documents it named.
		if errors.Is(err, errSegmentGone) {
			return nil, fmt.Errorf("%s: the manifest counts %d tombstones and no file holds them: %w",
				name, want, ErrCorrupt)
		}
		return nil, err
	}
	defer unmapFile(b) //nolint:errcheck // nothing left to do about it here
	return parseDead(r, want, total)
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
// Queries run alongside a commit. The encode reads index state and writes files,
// so it holds ix.mu in read mode; what needs exclusion is the swap that follows
// the manifest rename, and that is one mapping and one clear. The load test the
// note below asked for is `weft-eval bench -writes`, docs/PERF.md §5.4 registers
// the procedure, and docs/FINDINGS.md milestone 9 §3 is the read wait it found.
//
// ponytail: the upgrade that note described came with a cheaper design than it
// predicted. It asked for the documents captured to be counted, so an Add
// arriving mid-encode is neither written twice nor dropped; Index.wmu excludes
// Add for the whole commit instead, so the pending segment cannot change and
// there is nothing to count. What that costs is an Add blocked for a whole
// commit — which is what it already was — and Index.wmu carries both the ceiling
// and the way out of it.
//
// Cancelling ctx calls the commit off, and where the cancellation lands decides
// what that means — the rename being the commit point is the whole rule:
//
//   - Before the rename, Commit reports ctx.Err() and the index is untouched. The
//     pending documents are still pending and the caller may commit again. What
//     may be left in dir is an unnamed seg-<gen+1>, which is the debris of a
//     commit that never finished and is already a defined state: Open ignores it
//     and the next Commit sweeps it. Cancellation adds no cleanup path of its own.
//   - After the rename, the commit has succeeded and ctx.Err() is ignored for the
//     rest of the call. Stopping there would leave the directory publishing a
//     generation this index does not know it holds, which is exactly the mixed
//     state the rename exists to rule out — so cancellation is not allowed to
//     reach a state a crash cannot.
//
// The poll is not per document. It is at entry, at each section boundary of the
// encode, at each of the five Lloyd passes and every ivfAssignPoll documents of
// the assignment pass, and immediately before the rename. A section is therefore
// the granularity, and what that buys depends on the batch. Where a partition is
// trained it is sub-second: every unpolled stretch is inside buildIVF and none is
// longer than one walk of the training sample. A batch carrying no vectors skips
// buildIVF, and there the longest unpolled stretch is the docs section — a commit
// that big is not called off until that section ends. Polling per record would
// put a branch in the writer's hottest loop to close that gap.
//
// Merge takes no context. The same restructuring applies to it and nothing
// measures it; see the note there.
//
// Commit is not safe alongside another Commit on the same directory. weft has a
// single writer by design.
func (ix *Index) Commit(ctx context.Context, dir string) error {
	// Before the MkdirAll, so a commit refused on the way in creates nothing. A
	// caller who cancelled already is owed no directory.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("commit %s: %w", dir, err)
	}
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

	// wmu, for the whole commit, and it is what makes the split below work rather
	// than merely reorder it. See Index.wmu: without it a concurrent Add reaching
	// mu.Lock would put every later query behind itself, and the two sections
	// would buy nothing.
	//
	// Taken before the manifest is read, and that order is load-bearing rather
	// than tidy. Merge is this index's other public mutator and it holds this
	// same lock while it publishes generation gen+1 and prunes what it replaced.
	// A snapshot taken outside the lock can therefore be a generation stale by
	// the time this commit is admitted, and both of the destructive things it
	// decides would then be aimed wrongly: the RemoveAll below clears
	// seg-<gen+1>, which is the segment that merge just published as live, and
	// the manifest written after it names the source directories the merge has
	// already deleted. The rename is the commit point, so neither is
	// recoverable.
	ix.wmu.Lock()
	defer ix.wmu.Unlock()

	// Section one: read the directory, encode the segment, rename the manifest.
	// Under the read lock, because everything in it reads index state and writes
	// files. This is where the whole cost of a commit is — buildIVF alone was
	// 11.014 of the 11.063 seconds docs/FINDINGS.md milestone 5 §3.3 measured — and
	// it is now time queries no longer wait through.
	segs, gen, pending, err := ix.commitGeneration(ctx, root, dir)
	if err != nil {
		return err
	}

	// Section two: the swap, exclusive. A mapping and a clear, and nothing that
	// touches the filesystem beyond opening the segment just written.
	//
	// Between the two sections the index cannot change: every mutator takes wmu
	// first and wmu is still ours, so what section one read is still true here.
	// That is what makes the gap safe, and why sync.RWMutex having no upgrade
	// operation costs nothing.
	//
	// ctx is not polled from here on, and that is the contract rather than an
	// oversight. Section one returned without error, so the rename landed and the
	// commit is durable on disk; abandoning the adopt would leave dir publishing a
	// generation this index has no mapping for, and the next Commit would then
	// fail the stored==base agreement and report the directory as corrupt. A
	// cancellation must not be able to produce a state a crash cannot.
	if pending {
		if err := ix.adoptGeneration(root, dir, segs[len(segs)-1]); err != nil {
			return err
		}
	}

	// Section three: the sweep, with ix.mu released. Everything it removes is
	// unreachable — nothing reads a segment the manifest does not name — and it
	// touches no index state, so no query waits through it.
	//
	// wmu is still held, and that is load-bearing rather than slack left to be
	// tidied away: released here, a Merge could be admitted, publish its merged
	// segment, and leave this sweep deleting a generation that is live. What it
	// costs is an Add waiting the sweep out, which is the ceiling Index.wmu
	// already carries.
	//
	// Best-effort: the commit above is already durable, and failing to delete a
	// directory nothing names must not turn a successful commit into a reported
	// failure.
	prune(root, segs, gen)
	return nil
}

// commitGeneration is Commit's first section: it reads the directory, encodes the
// pending documents into seg-<gen+1> and renames the manifest onto it.
//
// It returns the segment list now published, the generation that now stands, and
// whether the last entry in the list is this commit's own — false for a commit
// that wrote no segment, which is now two cases rather than one: nothing to do
// at all, and a generation published for tombstones alone.
//
// Requires ix.wmu, which is what lets it take ix.mu in read mode: the pending
// segment it encodes cannot change while a mutator is excluded, so there is no
// snapshot to take and no captured count to carry. It is a closure's worth of
// work in a method rather than in Commit's body because the read lock has to be
// released before the write lock is taken, and one deferred unlock per section
// is what keeps every error path from having to remember to do it by hand.
//
// Every ctx poll a commit makes is inside here, which is the same statement as
// "the rename is the commit point": this returns having renamed or having done
// nothing visible.
func (ix *Index) commitGeneration(ctx context.Context, root *os.Root, dir string) (segs []segInfo, liveGen uint64, pending bool, err error) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	// The previous generation number comes from the manifest. A missing
	// manifest is a first commit; a corrupt one is not — guessing a generation
	// on top of a directory in an unknown state could orphan a commit the
	// caller believes exists, so Commit refuses and reports.
	gen, live, deadN, err := readManifest(root)
	if errors.Is(err, fs.ErrNotExist) {
		gen, live, deadN = 0, nil, 0
		// No manifest means nothing here is published. It does not mean this
		// directory is weft's.
		if err := refuseForeignEntries(root); err != nil {
			return nil, 0, false, fmt.Errorf("commit %s: %w", dir, err)
		}
	} else if err != nil {
		return nil, 0, false, fmt.Errorf("commit %s: %w", dir, err)
	}

	// The destination has to be the directory this index's committed segments
	// came from. The count comparison below cannot tell two unrelated
	// directories of the same size apart, and committing into the wrong one
	// joins this index's pending documents to that directory's history: the live
	// object keeps answering from the segments it opened, a reopen answers from
	// the ones on disk, and the next Merge writes the disagreement down.
	//
	// By identity rather than by string, because the same directory reached
	// through two paths is still the same directory — and because two different
	// directories reached through the same path are not. An index with nothing
	// committed has no directory yet and may pick any.
	if err := ix.sameDir(root); err != nil {
		return nil, 0, false, fmt.Errorf("commit %s: %w", dir, err)
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
		return nil, 0, false, fmt.Errorf("commit %s: the directory holds %d documents, this index has %d committed: %w",
			dir, stored, ix.base, ErrCorrupt)
	}

	// Whether there is a segment to write. An empty generation would grow the
	// manifest and the segment list without holding a document, and nothing
	// bounds how often a caller may ask; every point query walks that list, so
	// the cost of a no-op Commit would land on every read after it.
	//
	// Decided after the agreement above, not before, so a Commit against the
	// wrong directory still reports rather than quietly succeeding. The gen == 0
	// arm is what lets an empty index be written at all, which restore asks for.
	writeSeg := len(ix.docs) > 0 || gen == 0

	// A commit with no segment to write still has something to publish when
	// documents have been deleted since the last one. The tombstone set is not
	// in any segment — it spans the index and lives at the root — so this is the
	// one kind of generation that changes what the directory says without adding
	// a document to it.
	//
	// Compared by count rather than by contents, which is exact because the set
	// only ever grows: Delete adds and nothing removes, so two sizes agreeing is
	// two sets agreeing. See deadSet.
	if !writeSeg && ix.dead.n == deadN {
		// The sweep still runs, which is why this returns the live list rather
		// than nothing. A commit that died between writing its segment and
		// renaming the manifest leaves a directory nothing names, as large as the
		// batch it was writing, and Open documents that debris as costing disk
		// only until the next Commit. Skipping the sweep here makes that false
		// exactly when it matters: the reopened index has nothing pending, so the
		// orphan survives every commit until one happens to carry a document.
		// Same keep set and same best-effort contract as a commit that published
		// something — everything it removes is unreachable.
		return live, gen, false, nil
	}

	published := live
	if writeSeg {
		// readManifest has already established that no live segment is named for
		// gen+1 and that gen+1 does not wrap, and refuseForeignEntries has done
		// the same job for a directory with no manifest at all, so the directory
		// cleared below is this commit's own debris and never a published one.
		seg := segDirName(gen + 1)
		// A crash after writing segment files but before the manifest flip leaves
		// this directory half-written. It was never visible — no manifest names it
		// — so replacing it wholesale is safe.
		if err := root.RemoveAll(seg); err != nil {
			return nil, 0, false, fmt.Errorf("commit %s: clearing stale segment: %w", dir, err)
		}
		if err := root.Mkdir(seg, 0o700); err != nil {
			return nil, 0, false, fmt.Errorf("commit %s: %w", dir, err)
		}
		segRoot, err := root.OpenRoot(seg)
		if err != nil {
			return nil, 0, false, fmt.Errorf("commit %s: %w", dir, err)
		}
		defer segRoot.Close()
		if err := writeSegment(ctx, segRoot, &pendingSource{ix: ix}); err != nil {
			return nil, 0, false, fmt.Errorf("commit %s: %w", seg, err)
		}
		// The segment directory's entries need to reach disk before the manifest
		// claims they exist — and so does the segment directory's own entry in dir.
		// Syncing only the inside leaves the rename free to land first, and a
		// manifest naming a directory whose entry never made it is exactly the
		// mixed state the rename exists to rule out.
		syncDir(segRoot)
		syncDir(root)
		published = append(slices.Clone(live), segInfo{name: seg, base: ix.base, count: len(ix.docs)})
	}

	// The tombstone set, written before the rename for the same reason the
	// segment is: everything the manifest will claim has to be durable before the
	// manifest claims it. It is republished whole rather than appended to, so the
	// live generation's file is the whole set and a reader needs exactly one.
	//
	// ponytail: that is a rewrite of the entire set on every commit, about a byte
	// a tombstone. The way out is for the manifest to name the file it inherits,
	// which costs a name to validate; owed when a corpus is deleted from hard
	// enough for the copy to show up beside the segment write.
	if err := writeDead(root, gen+1, ix.dead.all()); err != nil {
		return nil, 0, false, fmt.Errorf("commit %s: %w", dir, err)
	}
	syncDir(root)

	// The last chance to call the commit off, and the reason it is here rather
	// than a line later: the rename is the commit point, so this is the boundary
	// between "nothing happened" and "it is done". A cancellation that arrives
	// after this poll is ignored for the rest of the call.
	//
	// Everything written so far stays as an unnamed seg-<gen+1> and an unnamed
	// dead-<gen+1>, which is the debris Open already documents and the next
	// Commit already sweeps.
	if err := ctx.Err(); err != nil {
		return nil, 0, false, fmt.Errorf("commit %s: %w", dir, err)
	}
	if err := writeManifest(root, gen+1, published, ix.dead.n); err != nil {
		return nil, 0, false, fmt.Errorf("commit %s: %w", dir, err)
	}
	return published, gen + 1, writeSeg, nil
}

// adoptGeneration is Commit's second section: the swap, taken exclusively.
//
// Requires ix.wmu, and a method for the same reason commitGeneration is one —
// the read lock has to be released before this one is taken, and one deferred
// unlock per section is what keeps every error path from having to remember to
// release by hand.
func (ix *Index) adoptGeneration(root *os.Root, dir string, info segInfo) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	// The commit is durable. Adopt what was just written so a second Commit does
	// not write these documents again, and so reads of them go through the mapping
	// like every other committed document. Failing here leaves the directory
	// correct and the in-memory index stale, which is why it is an error rather
	// than something swallowed: the caller has to Open again.
	if err := ix.adopt(root, info); err != nil {
		return fmt.Errorf("commit %s: %w", info.name, err)
	}
	if err := ix.rememberDir(root, dir); err != nil {
		return fmt.Errorf("commit %s: %w", dir, err)
	}
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

// Open maps the last committed generation from dir. The returned index is ready
// for further Adds and Commits.
//
// The caller must Close it. What Open returns is no longer a copy: milestone 3
// replaced the eager decode with mappings that stay live for as long as the
// index does, and a mapping is not memory the garbage collector owns — dropping
// the last reference to an index leaks six regions per segment for the life of
// the process. Close is the only thing that releases them.
//
// The files those mappings point at may be replaced or unlinked underneath a
// reader; the mapping survives it, which is what lets Commit and Merge prune a
// generation somebody is still reading. Independence from the *directory* holds.
// Independence from the bytes does not.
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
		gen, segs, deadN, err := readManifest(root)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", dir, err)
		}
		ix, err := mapGeneration(root, dir, segs, gen, deadN)
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
func mapGeneration(root *os.Root, dir string, segs []segInfo, gen uint64, deadN int) (*Index, error) {
	ix := New()
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
	// The tombstones last, because they are ranged against the corpus the
	// segments just established and because reading a document's length is what
	// restores the token total the statistics subtract.
	//
	// A generation with no tombstones has nothing to read: deadN is zero for
	// every version 3 manifest, which is what makes a directory written before
	// this format readable with nothing converted.
	if deadN > 0 {
		ids, err := readDead(root, gen, deadN, int(ix.base))
		if err != nil {
			ix.Close() //nolint:errcheck // already returning an error
			return nil, fmt.Errorf("open %s: %w", dir, err)
		}
		for _, id := range ids {
			// The length before the mark, the order Delete uses and for the same
			// reason: docLenAt answers 0 once an id is a tombstone, so marking
			// first would restore a token total that has nothing subtracted from
			// it and rescale every BM25 score in the reopened index.
			ix.dead.mark(id, ix.docLenAt(id))
		}
	}
	if err := ix.rememberDir(root, dir); err != nil {
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

	// The same window Open re-reads its way out of, for the same reason. A merge
	// publishes its replacement before it prunes the segments it replaced, so a
	// scrub holding the pre-merge list reaches a directory that is no longer
	// there while the index itself is sound throughout. Reporting that as
	// corruption turns a scheduled integrity check into a false alarm, which is
	// worse than no check: "is this directory sound" is the only question Scrub
	// is asked.
	//
	// Bounded the same way, and for the same reason: a merge finishes, and a
	// manifest that keeps naming a segment nothing can open is damage.
	for attempt := 0; ; attempt++ {
		gen, segs, deadN, err := readManifest(root)
		if err != nil {
			return fmt.Errorf("scrub %s: %w", dir, err)
		}
		err = scrubGeneration(root, segs, gen, deadN)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errSegmentGone) || attempt+1 == openAttempts {
			return err
		}
	}
}

// scrubGeneration verifies every segment one manifest names.
func scrubGeneration(root *os.Root, segs []segInfo, gen uint64, deadN int) error {
	// The tombstone file first, and read in full: parseDead is the whole of its
	// verification — count against the manifest, ids strictly ascending, every id
	// inside the corpus — and the walk below needs the set anyway, because a key
	// two segments both carry is damage only when both documents are live.
	var total int
	if n := len(segs); n > 0 {
		total = int(segs[n-1].base) + segs[n-1].count
	}
	var dead deadSet
	if deadN > 0 {
		ids, err := readDead(root, gen, deadN, total)
		if err != nil {
			return fmt.Errorf("scrub %s: %w", deadFileName(gen), err)
		}
		for _, id := range ids {
			// Length zero: nothing here computes a statistic, so the token total a
			// live index maintains has no meaning for a scrub and asking for it
			// would mean mapping docoff a second time.
			dead.mark(id, 0)
		}
	}
	return scrubSegments(root, segs, &dead)
}

func scrubSegments(root *os.Root, segs []segInfo, dead *deadSet) error {
	// Keys are unique across the whole index and not merely inside a segment.
	// Resolve rests on that — it is why the first segment to answer is treated
	// as the only one that can — and Add enforces it against the segments, so a
	// duplicate cannot arrive through the API and can arrive by copying a
	// segment directory in. A copied directory is what this function is for.
	//
	// One entry per document, which is the largest thing a scrub keeps and is
	// the size of the document count rather than of the corpus. Each segment's
	// walk fills it and the next one reads it, so a key two segments claim is
	// caught where a per-segment check cannot see it.
	found := make(map[string]scrubbedKey)
	vecDim := 0
	for _, seg := range segs {
		width, err := scrubSegment(root, seg, found, dead)
		if err != nil {
			return fmt.Errorf("scrub %s: %w", seg.name, err)
		}
		// The same one-width-per-index rule Open applies. Each segment's width
		// comes from its own documents; nothing compared one segment's against
		// the next's.
		if width != 0 {
			if vecDim != 0 && width != vecDim {
				return fmt.Errorf("scrub %s: vectors are %d wide, an earlier segment's are %d: %w",
					seg.name, width, vecDim, ErrCorrupt)
			}
			vecDim = width
		}
	}
	return nil
}

// scrubbedKey is where a key was found: the segment holding it, and the
// segment-local id of the record whose first field carries it.
type scrubbedKey struct {
	seg string
	id  DocID

	// dead is whether that record is a tombstone, and it is what turns the
	// uniqueness rule from "a key appears once" into "a key names at most one
	// *live* document". Deleting a document and adding its key again leaves the
	// old record exactly where it was — nothing renumbers or reclaims — so a
	// directory that has been updated legitimately holds two records for one key.
	dead bool
}

// scrubSegment verifies every byte of one segment without holding it, and
// returns the vector width its documents establish.
//
// This is what loading a segment used to be, minus the segment. The eager path
// decoded every document, vector, link and posting onto the Go heap in order to
// check them — and the index a caller scrubs is the one that is mapped rather
// than loaded because it does not fit, so "is this index intact" could be
// answered by the process dying. Every unit here is checkable on its own, so
// each is decoded, checked and dropped.
//
// What outlives a document is its key and its token count: the keys because
// uniqueness is an index-wide rule and the caller carries the map from one
// segment to the next, the lengths because the postings are checked against them
// and the postings file is read after the documents. Both are the size of the
// document count.
func scrubSegment(root *os.Root, info segInfo, found map[string]scrubbedKey, dead *deadSet) (int, error) {
	segRoot, err := root.OpenRoot(info.name)
	if err != nil {
		// Same reasoning as openSection: the manifest named it, so a segment
		// that is missing — or is a plain file standing where its directory
		// belongs, or a symlink the root refuses to follow out of the index
		// directory, both foreign layouts and not an index of ours — is damage
		// rather than an index that was never written. A directory that is
		// genuinely there and still will not open is the filesystem refusing us,
		// not corruption, and reports as itself. Lstat, not Stat, for the reason
		// openSection gives: Stat follows the link in question.
		//
		// Absence is errSegmentGone rather than a bare ErrCorrupt, the same
		// distinction openSegment draws: it is the one failure here that can
		// mean the directory is sound and this reader is simply late. A foreign
		// layout standing at the name is not — a merge leaves no plain file
		// behind.
		fi, serr := root.Lstat(info.name)
		if errors.Is(err, fs.ErrNotExist) {
			return 0, errSegmentGone
		}
		if serr == nil && !fi.IsDir() {
			return 0, fmt.Errorf("the manifest names this segment but no directory stands there: %w", ErrCorrupt)
		}
		return 0, err
	}
	defer segRoot.Close()

	// Every mapping is released before this returns, and nothing outliving the
	// call points into one: a key is a string the decoder copied, a token count
	// is an int.
	maps := make([][]byte, 0, len(segSections))
	defer func() {
		for _, b := range maps {
			unmapFile(b) //nolint:errcheck,gosec // nothing left to do about it here
		}
	}()
	// meta first and alone, for the reason openSegment gives: its frame says
	// which sections this segment owes, and a v3 segment missing one is damage
	// rather than an older vintage.
	metaR, metaB, err := openSection(segRoot, metaFile, kindMeta, true)
	if err != nil {
		return 0, err
	}
	maps = append(maps, metaB)

	secs := segSectionsFor(metaR.version)
	rs := make([]*segReader, len(secs))
	rs[0] = metaR
	for i, s := range secs[1:] {
		// Every frame checksum. That is the one check whose cost is the size of
		// its section, and therefore the single difference between what Open
		// reads and what this does: damage in bytes no decoder reaches is
		// visible here and nowhere else.
		r, b, err := openSection(segRoot, s.name, s.kind, true)
		if err != nil {
			return 0, err
		}
		maps = append(maps, b)
		if r.version != metaR.version {
			return 0, fmt.Errorf("%s: format version %d beside %s at version %d: %w",
				s.name, r.version, metaFile, metaR.version, ErrCorrupt)
		}
		rs[i+1] = r
	}
	docsR, postR, termsR, docoffR, keysR := rs[1], rs[2], rs[3], rs[4], rs[5]

	docCount, totalLen, vecDim, liveCount, err := decodeMeta(metaR)
	if err != nil {
		return 0, err
	}
	offs, err := parseDocOffsets(docoffR)
	if err != nil {
		return 0, err
	}
	// The partition, before the documents. It is checked against meta's numbers
	// rather than against the documents themselves — see scrubIVF for the one
	// thing that leaves unchecked and why — so nothing here needs the walk
	// below to have happened first, and a segment whose ivf section is damaged
	// is named as such rather than after a full corpus decode.
	// meta's count is what sizes the partition's duplicate-detection bitmap and
	// what every list's ids are ranged against, and nothing has contradicted it
	// yet — the docs walk that would is below. A meta claiming maxDocCount would
	// have scrubIVF allocate a byte per claimed document before anything said
	// otherwise. docoff is already parsed and Open makes exactly this comparison,
	// so the guard costs nothing but the line.
	if docCount != offs.n {
		return 0, fmt.Errorf("%s: meta says %d documents, %s indexes %d: %w",
			metaFile, docCount, docoffFile, offs.n, ErrCorrupt)
	}
	if ivfR := ivfReader(rs); ivfR != nil {
		if err := scrubIVF(ivfR, docCount, vecDim); err != nil {
			return 0, err
		}
	}

	docLen, sumLen, width, err := scrubDocs(docsR, offs, info, found, dead)
	if err != nil {
		return 0, err
	}
	// The manifest's count and meta's are written by the same commit and read by
	// different paths, and every base after this segment was computed from the
	// manifest's. Open compares the two; without the same comparison here, Scrub
	// reports success for a directory Open refuses, which is backwards for the
	// check a caller runs before trusting one.
	if len(docLen) != info.count {
		return 0, fmt.Errorf("the manifest says %d documents, the segment holds %d: %w",
			info.count, len(docLen), ErrCorrupt)
	}
	// meta is the statistics snapshot BM25 trusts, so it does not get to
	// disagree with the documents it describes.
	if docCount != len(docLen) || totalLen != sumLen || vecDim != width {
		return 0, fmt.Errorf("meta says %d docs/%d tokens/%d dims, documents hold %d/%d/%d: %w",
			docCount, totalLen, vecDim, len(docLen), sumLen, width, ErrCorrupt)
	}
	// Against meta's live count, not the document count: the keys section indexes
	// the documents that had no tombstone when the segment was written. decodeMeta
	// has already ranged that number against the document count, and the walk
	// above has just confirmed the document count against the records themselves.
	if err := verifyKeyTable(keysR, info.name, liveCount, found); err != nil {
		return 0, err
	}
	if err := decodePostings(postR, termsR, docLen); err != nil {
		return 0, err
	}
	return width, nil
}

// scrubDocs walks the docs section front to back, checking each record against
// its own checksum, against the docoff entry pointing at it and against the
// records before it. It returns what the rest of the segment is checked against:
// the token counts, their sum, and the vector width.
//
// The invariants re-checked here are the ones the write path enforces — keys
// unique and non-empty, vector components finite and uniformly wide — because
// scorer/vector builds on ErrNonFiniteVector's promise by not re-checking
// documents, and the decoder re-validating is what keeps that promise true for
// restored corpora.
func scrubDocs(docsR *segReader, offs docOffsets, info segInfo, found map[string]scrubbedKey, dead *deadSet) (docLen []int, totalLen, vecDim int, err error) {
	seg := info.name
	n, err := docsR.intn("document count", maxDocCount)
	if err != nil {
		return nil, 0, 0, err
	}
	if offs.n != n {
		return nil, 0, 0, fmt.Errorf("%s indexes %d documents, %s holds %d: %w",
			offs.name, offs.n, docsR.name, n, ErrCorrupt)
	}

	// One int per document. The count is bounded by maxDocCount and by what the
	// docoff table physically holds — sixteen bytes an entry, checked above — so
	// a hostile count cannot make this allocate more than the segment could back.
	docLen = make([]int, n)
	for i := range n {
		// Where the record actually begins, absolute, the way docoff records it.
		// The entry is compared against this rather than followed and decoded:
		// the same question, since a record's checksum is seeded with its own id
		// and only that record's bytes verify under it — and without the second
		// full decode of the corpus that following every entry costs.
		at := segHeaderLen + docsR.off
		off, ok := offs.at(DocID(i))
		if !ok {
			return nil, 0, 0, fmt.Errorf("%s: offset %d is unusable on this platform: %w", offs.name, i, ErrCorrupt)
		}
		if off != at {
			return nil, 0, 0, fmt.Errorf("%s: document %d is recorded at offset %d, its record begins at %d: %w",
				offs.name, i, off, at, ErrCorrupt)
		}
		// Bounded by the record, not by what is left of the section — the same
		// reason segment.recordAt gives, and the same table. Narrowing the view
		// rather than building a second reader keeps this loop allocation-free,
		// which is what lets a scrub walk a corpus it cannot hold.
		full := docsR.b
		if next, ok := offs.at(DocID(i + 1)); ok {
			if next < off || next-segHeaderLen > len(full) {
				return nil, 0, 0, fmt.Errorf("%s: document %d runs from %d to %d, which is not a record: %w",
					offs.name, i, off, next, ErrCorrupt)
			}
			docsR.b = full[:next-segHeaderLen]
		}
		d, dl, err := decodeDocRecord(docsR, i)
		docsR.b = full
		if err != nil {
			return nil, 0, 0, err
		}
		// The token count is written twice — in the record and in the table — so
		// the two are compared. A copy nobody checks is what D-001 calls rot, and
		// this one is the copy every BM25 score is computed from.
		if got := offs.docLen(DocID(i)); got != dl {
			return nil, 0, 0, fmt.Errorf("%s: document %d is %d tokens, its record says %d: %w",
				offs.name, i, got, dl, ErrCorrupt)
		}
		// Two records may carry one key; two *live* records may not. Resolve
		// answers with the live one and Add refuses a key that already has one, so
		// a second live holder is a state neither could have produced — while a
		// dead holder beside a live one is what every update leaves behind.
		//
		// The live record wins the slot whichever order the two are walked in, and
		// that is what verifyKeyTable then checks the keys table against: the table
		// indexes the documents that were live when the segment was written.
		self := scrubbedKey{seg: seg, id: DocID(i), dead: dead.has(info.base + DocID(i))}
		if prev, dup := found[d.Key]; dup {
			if !prev.dead && !self.dead {
				if prev.seg == seg {
					return nil, 0, 0, fmt.Errorf("%s: live documents %d and %d both hold key %q: %w",
						docsR.name, prev.id, i, d.Key, ErrCorrupt)
				}
				return nil, 0, 0, fmt.Errorf("key %q is also held by a live document in %s: %w", d.Key, prev.seg, ErrCorrupt)
			}
			if self.dead {
				self = prev
			}
		}
		if dl > maxInt-totalLen {
			return nil, 0, 0, fmt.Errorf("%s: document lengths overflow their sum: %w", docsR.name, ErrCorrupt)
		}
		if vn := len(d.Vector); vn > 0 {
			if vecDim == 0 {
				vecDim = vn
			} else if vn != vecDim {
				return nil, 0, 0, fmt.Errorf("%s: document %d vector is %d wide, corpus is %d: %w", docsR.name, i, vn, vecDim, ErrCorrupt)
			}
		}
		found[d.Key] = self
		docLen[i] = dl
		totalLen += dl
	}
	return docLen, totalLen, vecDim, docsR.done()
}

// sameDir reports whether root is the directory this index's committed segments
// came from, and requires ix.mu.
//
// It asks the open root rather than a path, which is what makes the answer about
// the directory being written into rather than about whatever the name resolves
// to a moment later. The remembered side is the identity captured when this
// index was opened or adopted: a path stops naming a directory the moment
// somebody renames it, and stat'ing the same stale path twice compares the
// replacement with itself and agrees.
//
// An index with nothing committed has no directory yet and may pick any.
func (ix *Index) sameDir(root *os.Root) error {
	if ix.dirID == nil {
		return nil
	}
	fi, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(ix.dirID, fi) {
		return fmt.Errorf("this index holds the segments of another directory, which used to be %s: %w", ix.dir, ErrCorrupt)
	}
	return nil
}

// rememberDir records a directory and its identity. Requires ix.mu.
//
// Absolute, because this is remembered and used later: Merge reopens it, and a
// relative path stops naming this index the moment the process changes
// directory. The mappings would still be valid, so the failure would be caused
// by nothing but the deferred resolution.
func (ix *Index) rememberDir(root *os.Root, dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	fi, err := root.Stat(".")
	if err != nil {
		return err
	}
	ix.dir, ix.dirID = abs, fi
	return nil
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
func readManifest(root *os.Root) (gen uint64, segs []segInfo, deadN int, err error) {
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
			return 0, nil, 0, fmt.Errorf("%s: not a regular file: %w", manifestName, ErrCorrupt)
		}
		return 0, nil, 0, err
	}
	r, err := parseSection(manifestName, b, kindManifest)
	if err != nil {
		return 0, nil, 0, err
	}
	if gen, err = r.uvarint("generation"); err != nil {
		return 0, nil, 0, err
	}
	n, err := r.intn("segment count", len(r.b))
	if err != nil {
		return 0, nil, 0, err
	}
	// Bases are checked as they are read rather than afterwards: a segment list
	// that does not tile [0, total) contiguously and in order would give two
	// segments overlapping ids, and Index.segFor would answer with whichever it
	// walked into first — a wrong document, not an error.
	nextBase := uint64(0)
	for range n {
		name, err := r.str("segment name")
		if err != nil {
			return 0, nil, 0, err
		}
		if !strings.HasPrefix(name, segPrefix) || strings.ContainsAny(name, `/\`) || name != filepath.Base(name) {
			return 0, nil, 0, fmt.Errorf("%s: segment name %q: %w", manifestName, name, ErrCorrupt)
		}
		base, err := r.uvarint("segment base")
		if err != nil {
			return 0, nil, 0, err
		}
		count, err := r.intn("segment document count", maxDocCount)
		if err != nil {
			return 0, nil, 0, err
		}
		if base != nextBase {
			return 0, nil, 0, fmt.Errorf("%s: segment %q starts at document %d, the one before it ended at %d: %w",
				manifestName, name, base, nextBase, ErrCorrupt)
		}
		if base > uint64(maxDocCount)-uint64(count) {
			return 0, nil, 0, fmt.Errorf("%s: segment %q runs past the document ceiling: %w", manifestName, name, ErrCorrupt)
		}
		nextBase = base + uint64(count)
		segs = append(segs, segInfo{name: name, base: DocID(base), count: count})
	}
	// The tombstone count, which exists only from version 4. The version decides
	// whether it is there rather than the payload's remaining length, for the
	// reason FORMAT.md §7.1 gives about guessing: a v3 manifest with four stray
	// bytes after its segment list is damage, and reading them as a count would
	// turn that into a demand for a `dead-` file the directory never had.
	if r.version >= 4 {
		if deadN, err = r.intn("tombstone count", maxDocCount); err != nil {
			return 0, nil, 0, err
		}
	}
	if err := r.done(); err != nil {
		return 0, nil, 0, err
	}
	if len(segs) == 0 {
		return 0, nil, 0, fmt.Errorf("%s: generation %d names no segments; a commit always publishes one: %w", manifestName, gen, ErrCorrupt)
	}
	// No segment is named for the next generation, and none is named past this
	// one. The first half is what Commit and Merge both need: each clears
	// seg-<gen+1> before writing it, so a manifest that already named it would
	// have that RemoveAll aimed at live data.
	//
	// Milestone 2 asked for something narrower — *some* segment is named for gen —
	// and version 4 cannot keep it. A commit that only deletes documents publishes
	// a generation and no segment, because the tombstone set is what changed and
	// that lives at the root; the segment list it republishes is the one it
	// inherited, and none of those names carries the new number. What replaces the
	// rule is strictly stronger on the axis that mattered: every name is one some
	// commit produced, and no name is past the generation claiming to have written
	// it. A doctored manifest can no longer smuggle in a segment named for a
	// generation that has not happened.
	next := segDirName(gen + 1)
	for _, s := range segs {
		if s.name == next {
			return 0, nil, 0, fmt.Errorf("%s: generation %d already names %s, which the next write clears: %w",
				manifestName, gen, next, ErrCorrupt)
		}
		m, ok := segGen(s.name)
		if !ok {
			return 0, nil, 0, fmt.Errorf("%s: segment %q is not a name any commit produced: %w", manifestName, s.name, ErrCorrupt)
		}
		if m > gen {
			return 0, nil, 0, fmt.Errorf("%s: generation %d names %s, which a later generation would have written: %w",
				manifestName, gen, s.name, ErrCorrupt)
		}
	}
	seen := make(map[string]struct{}, len(segs))
	for _, s := range segs {
		if _, dup := seen[s.name]; dup {
			return 0, nil, 0, fmt.Errorf("%s: segment %q listed twice: %w", manifestName, s.name, ErrCorrupt)
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
		return 0, nil, 0, fmt.Errorf("%s: generation %d, which no sequence of commits could reach: %w", manifestName, gen, ErrCorrupt)
	}
	return gen, segs, deadN, nil
}

// segGen is the generation a segment directory name carries, and false for a
// name no writer produced.
//
// The round trip through segDirName is the check, not a numeric parse: uvarint
// overlong encodings are refused elsewhere in this format for exactly this
// reason, and "seg-0000001" and "seg-000001" naming one generation would be the
// same defect one layer up — two byte strings meaning one index.
func segGen(name string) (uint64, bool) {
	digits, ok := strings.CutPrefix(name, segPrefix)
	if !ok {
		return 0, false
	}
	gen, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || segDirName(gen) != name {
		return 0, false
	}
	return gen, true
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
		// A tombstone file is the third name Commit deletes on sight — prune
		// removes every generation's but the live one — so it owes the same proof
		// of ownership the other two do.
		case strings.HasPrefix(e.Name(), deadPrefix):
			if !e.Type().IsRegular() {
				return fmt.Errorf("%s is not a file and no manifest claims it, so weft will not delete it", e.Name())
			}
			if err := refuseForeignFile(root, e.Name()); err != nil {
				return err
			}
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
// gen is the generation the manifest now publishes, and it is what says which
// `dead-` file to keep. Exactly one is live — Commit and Merge both republish
// the whole tombstone set under their own generation — so every other one is a
// previous generation's, unreachable for the same reason an unnamed segment is.
func prune(root *os.Root, keep []segInfo, gen uint64) {
	root.Remove(manifestName + ".tmp") //nolint:errcheck,gosec // best-effort by design, see above
	live := make(map[string]struct{}, len(keep)+1)
	for _, s := range keep {
		live[s.name] = struct{}{}
	}
	live[deadFileName(gen)] = struct{}{}
	entries, err := readDir(root, ".")
	if err != nil {
		return
	}
	for _, e := range entries {
		if _, ok := live[e.Name()]; ok {
			continue
		}
		switch {
		case e.IsDir() && strings.HasPrefix(e.Name(), segPrefix):
			root.RemoveAll(e.Name()) //nolint:errcheck,gosec // best-effort by design, see above
		case !e.IsDir() && strings.HasPrefix(e.Name(), deadPrefix):
			root.Remove(e.Name()) //nolint:errcheck,gosec // best-effort by design, see above
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
