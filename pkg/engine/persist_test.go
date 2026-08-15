// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// copyTree copies one flat segment directory to a new name, which is how a test
// builds a directory that the API refuses to produce: two segments holding the
// same key. Scrub is documented as what a caller runs before trusting a copied
// directory, so a copied directory is what it has to be tested on.
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Mkdir(to, 0o700); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// republish writes a manifest naming segs, replacing whatever the directory
// published before. It is how a test reaches the states a writer cannot reach
// but a damaged or spliced directory can.
func republish(t *testing.T, dir string, gen uint64, segs []segInfo) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := writeManifest(root, gen, segs); err != nil {
		t.Fatal(err)
	}
}

// dirNames lists dir's entries, sorted, for asserting exactly what a commit
// leaves behind.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

func TestOpenWithoutACommitReportsNotExist(t *testing.T) {
	if _, err := Open(t.TempDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty dir: got %v, want fs.ErrNotExist", err)
	}
	if _, err := Open(filepath.Join(t.TempDir(), "never-made")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing dir: got %v, want fs.ErrNotExist", err)
	}
}

func TestCommitEmptyIndex(t *testing.T) {
	dir := t.TempDir()
	if err := New().Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	ix, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if ix.Len() != 0 {
		t.Fatalf("restored %d documents from an empty commit", ix.Len())
	}
	if _, err := ix.Add(Document{Key: "a", Text: "still works"}); err != nil {
		t.Fatalf("Add after empty restore: %v", err)
	}
}

// TestCommitWithNothingPendingPublishesNothing is the other half of the empty
// commit above: the first one writes an index Open can read, and every one after
// it has nothing left to write.
//
// An empty generation is not free. It joins the manifest and the segment list,
// and every point query walks that list — so a caller polling Commit would slow
// its own reads down without adding a document, without bound.
func TestCommitWithNothingPendingPublishesNothing(t *testing.T) {
	dir := t.TempDir()
	ix := New()
	addAll(t, ix, []Document{{Key: "only", Text: "the one document there is"}})
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	want := dirNames(t, dir)

	for i := range 3 {
		if err := ix.Commit(dir); err != nil {
			t.Fatalf("Commit %d with nothing pending: %v", i+2, err)
		}
	}
	if got := dirNames(t, dir); !slices.Equal(got, want) {
		t.Errorf("three commits with nothing pending changed the directory:\n got %v\nwant %v", got, want)
	}

	// And the index still reads, so returning early did not leave it half-way
	// through a commit it decided not to make.
	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown
	if _, ok := got.Resolve("only"); !ok {
		t.Error("the committed document did not survive the commits that wrote nothing")
	}
}

// TestUnmanifestedSegmentIsInvisible is the atomicity pass line: a segment
// written but never named by a MANIFEST — a commit that died before its
// rename — must be indistinguishable from a segment that never existed. Open
// ignores it and leaves it alone (it cannot tell debris from a commit still
// running); the next Commit is what sweeps it.
func TestUnmanifestedSegmentIsInvisible(t *testing.T) {
	committed := New()
	addAll(t, committed, []Document{{Key: "safe", Text: "the committed corpus"}})
	dir := t.TempDir()
	if err := committed.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Simulate the interrupted second commit: full, valid segment files for a
	// different corpus, written exactly as Commit writes them — and no
	// manifest flip.
	orphan := New()
	addAll(t, orphan, []Document{{Key: "ghost", Text: "the corpus that never landed"}})
	orphanRoot := makeSegDir(t, dir, segDirName(2))
	if err := writeSegment(orphanRoot, &pendingSource{ix: orphan}); err != nil {
		t.Fatal(err)
	}

	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, ok := got.Resolve("ghost"); ok {
		t.Fatal("Open surfaced a document from a segment no manifest names")
	}
	if _, ok := got.Resolve("safe"); !ok {
		t.Fatal("Open lost the committed corpus")
	}
	// A reader must not delete a directory a concurrent Commit could be
	// filling in right now, so the orphan is still there after the Open.
	if names := dirNames(t, dir); !slices.Equal(names, []string{"MANIFEST", "seg-000001", "seg-000002"}) {
		t.Fatalf("Open touched the directory: %v", names)
	}
	// The next commit is generation 2, so it writes seg-000002 — the orphan's
	// own name — and the RemoveAll before the write is what clears it. Same
	// mechanism as milestone 2; what changed is that seg-000001 now stays,
	// because incremental commit leaves older generations live and sweeping one
	// would take the front of the corpus with it.
	if _, err := got.Add(Document{Key: "later", Text: "added after the restart"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := got.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if names := dirNames(t, dir); !slices.Equal(names, []string{"MANIFEST", "seg-000001", "seg-000002"}) {
		t.Fatalf("orphan segment not swept by the next commit: %v", names)
	}
	// The name survived; the contents did not.
	if _, ok := got.Resolve("ghost"); ok {
		t.Fatal("the orphan's documents survived the commit that overwrote it")
	}
	if _, ok := got.Resolve("later"); !ok {
		t.Fatal("the second commit's own document is missing")
	}
	if _, ok := got.Resolve("safe"); !ok {
		t.Fatal("the first generation was lost")
	}
}

// TestSuccessiveCommitsAccumulateGenerations is the milestone 2 test of the
// same name turned around. It asserted that only the newest generation
// survived, which was true while a commit rewrote the whole corpus; now every
// generation holds a slice of it and deleting one would delete documents.
func TestSuccessiveCommitsAccumulateGenerations(t *testing.T) {
	ix := New()
	dir := t.TempDir()
	for i, key := range []string{"one", "two", "three"} {
		addAll(t, ix, []Document{{Key: key, Text: "generation " + key}})
		if err := ix.Commit(dir); err != nil {
			t.Fatalf("Commit %d: %v", i+1, err)
		}
	}
	if names := dirNames(t, dir); !slices.Equal(names, []string{"MANIFEST", "seg-000001", "seg-000002", "seg-000003"}) {
		t.Fatalf("after three commits: %v, want all three generations", names)
	}
	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	assertReadAPIsAgree(t, ix, got)
}

func TestCommitAfterOpenContinuesTheGenerations(t *testing.T) {
	first := New()
	addAll(t, first, []Document{{Key: "a", Text: "written before the restart"}})
	dir := t.TempDir()
	if err := first.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// The restart: reopen, keep writing, commit again.
	second, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	addAll(t, second, []Document{{Key: "b", Text: "written after the restart"}})
	if err := second.Commit(dir); err != nil {
		t.Fatalf("second Commit: %v", err)
	}

	got, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	assertReadAPIsAgree(t, second, got)
	if names := dirNames(t, dir); !slices.Equal(names, []string{"MANIFEST", "seg-000001", "seg-000002"}) {
		t.Fatalf("generations did not continue past the restart: %v", names)
	}
}

// TestCommitRefusesACorruptManifest pins the decision that Commit does not
// guess: a corrupt manifest means the directory's state is unknown, and
// writing a fresh generation on top could orphan a commit the caller believes
// exists.
func TestCommitRefusesACorruptManifest(t *testing.T) {
	ix := New()
	addAll(t, ix, []Document{{Key: "a", Text: "committed fine"}})
	dir := t.TempDir()
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	path := filepath.Join(dir, manifestName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ix.Commit(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Commit over a corrupt manifest: got %v, want ErrCorrupt", err)
	}
}

// TestMissingSectionIsCorruptNotAbsent separates the two failures a caller
// branches on. "Nothing committed here yet" is fs.ErrNotExist and invites
// starting fresh; a manifest naming a segment whose files are gone is damage,
// and reporting it as absence would let a caller overwrite a recoverable
// index without ever seeing an error.
func TestMissingSectionIsCorruptNotAbsent(t *testing.T) {
	for _, s := range segSections {
		name := s.name
		t.Run(name, func(t *testing.T) {
			dir, segDir := commitTiny(t)
			if err := os.Remove(filepath.Join(segDir, name)); err != nil {
				t.Fatal(err)
			}
			_, err := Open(dir)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("missing %s: got %v, want ErrCorrupt", name, err)
			}
			if errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("missing %s also reads as fs.ErrNotExist: %v", name, err)
			}
		})
	}
}

// TestCommitRefusesAManifestNamingTheNextGeneration guards the RemoveAll that
// clears a half-written segment: a manifest whose generation number and
// segment name disagree would aim it at the segment that is currently
// published, destroying the live commit before the new one is durable.
func TestCommitRefusesAManifestNamingTheNextGeneration(t *testing.T) {
	dir, segDir := commitTiny(t)
	rewriteSection(t, filepath.Join(dir, manifestName), kindManifest, func(w *segWriter) {
		w.uvarint(0) // generation 0, so the next commit would write seg-000001
		w.uvarint(1)
		w.str(segDirName(1))
	})
	ix := New()
	addAll(t, ix, []Document{{Key: "new", Text: "the corpus that must not overwrite"}})
	if err := ix.Commit(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Commit: got %v, want ErrCorrupt", err)
	}
	if _, err := os.Stat(filepath.Join(segDir, docsFile)); err != nil {
		t.Fatalf("the published segment was destroyed anyway: %v", err)
	}
}

// TestCommitRefusesAManifestListingTwoSegments closes the gap between the two
// readers of a manifest: Open has always refused a v1 manifest listing anything
// but one segment, while Commit read the same file and published over it. The
// commit that followed pruned every segment the old manifest named, turning a
// directory that could still be repaired into one that could not.
func TestCommitRefusesAManifestListingTwoSegments(t *testing.T) {
	dir, segDir := commitTiny(t)
	rewriteSection(t, filepath.Join(dir, manifestName), kindManifest, func(w *segWriter) {
		w.uvarint(1)
		w.uvarint(2)
		w.str(segDirName(1))
		w.str(segDirName(2))
	})
	ix := New()
	addAll(t, ix, []Document{{Key: "new", Text: "nothing here may be published"}})
	if err := ix.Commit(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Commit: got %v, want ErrCorrupt", err)
	}
	if _, err := os.Stat(filepath.Join(segDir, docsFile)); err != nil {
		t.Fatalf("the published segment was pruned anyway: %v", err)
	}
}

// TestCommitRefusesAnExhaustedGeneration guards Commit's gen+1. A manifest at
// the last generation a uint64 can hold wraps it to zero, which would aim the
// pre-write RemoveAll at seg-000000 and then publish a generation below the one
// it replaced — the strictly increasing counter the whole format rests on.
func TestCommitRefusesAnExhaustedGeneration(t *testing.T) {
	dir := t.TempDir()
	last := uint64(math.MaxUint64)
	rewriteSection(t, filepath.Join(dir, manifestName), kindManifest, func(w *segWriter) {
		w.uvarint(last)
		w.uvarint(1)
		w.str(segDirName(last))
	})
	// The directory gen+1 wraps onto, holding something worth not deleting.
	bystander := filepath.Join(dir, segDirName(0), "notes.txt")
	if err := os.MkdirAll(filepath.Dir(bystander), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bystander, []byte("not weft's"), 0o644); err != nil {
		t.Fatal(err)
	}

	ix := New()
	addAll(t, ix, []Document{{Key: "a", Text: "must not be published as generation 0"}})
	if err := ix.Commit(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Commit: got %v, want ErrCorrupt", err)
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Fatalf("the wrapped generation's directory was cleared anyway: %v", err)
	}
	// readManifest is shared, so the same impossible counter is refused on read.
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open: got %v, want ErrCorrupt", err)
	}
}

// TestFirstCommitRefusesAForeignSegmentDirectory covers the one commit with no
// manifest to prove the directory is weft's. Commit clears its target seg-* name
// before writing and prunes every other one afterwards, so pointed at a
// directory that merely happens to hold such a name — a caller passing a home
// or documents directory — it would recursively delete data it never wrote.
func TestFirstCommitRefusesAForeignSegmentDirectory(t *testing.T) {
	dir := t.TempDir()
	bystander := filepath.Join(dir, segDirName(1), "vacation-photos")
	if err := os.MkdirAll(filepath.Dir(bystander), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bystander, []byte("not weft's"), 0o644); err != nil {
		t.Fatal(err)
	}

	ix := New()
	addAll(t, ix, []Document{{Key: "a", Text: "x"}})
	if err := ix.Commit(dir); err == nil {
		t.Fatal("Commit claimed a directory holding files weft never wrote")
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Fatalf("Commit deleted a directory it does not own: %v", err)
	}
	// A refused commit mutates nothing at all, not even the parts it got to.
	if names := dirNames(t, dir); !slices.Equal(names, []string{segDirName(1)}) {
		t.Fatalf("a refused commit left something behind: %v", names)
	}
}

// TestFirstCommitOverwritesItsOwnDebris is the other half of that guard. A first
// commit that died before its rename leaves a segment directory holding nothing
// but section files, and refusing that would wedge the index permanently —
// every later commit failing — where the point is only to spare data weft never
// wrote.
func TestFirstCommitOverwritesItsOwnDebris(t *testing.T) {
	dir := t.TempDir()
	orphan := New()
	addAll(t, orphan, []Document{{Key: "ghost", Text: "the corpus that never landed"}})
	if err := writeSegment(makeSegDir(t, dir, segDirName(1)), &pendingSource{ix: orphan}); err != nil {
		t.Fatal(err)
	}

	ix := New()
	addAll(t, ix, []Document{{Key: "real", Text: "the corpus that did"}})
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit over its own unpublished debris: %v", err)
	}
	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, ok := got.Resolve("ghost"); ok {
		t.Fatal("the abandoned segment survived the commit that replaced it")
	}
	if _, ok := got.Resolve("real"); !ok {
		t.Fatal("Open lost the committed corpus")
	}
}

// TestAFileWhereASegmentBelongsIsCorrupt pins Open's error classification. A
// manifest naming a segment has already said this directory is an index, so a
// plain file standing at that name is a foreign or damaged layout. Reporting the
// raw ENOTDIR would leave errors.Is(err, ErrCorrupt) false, contrary to what
// Open documents, and a caller branching on absence must not be handed one
// either.
func TestAFileWhereASegmentBelongsIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	rewriteSection(t, filepath.Join(dir, manifestName), kindManifest, func(w *segWriter) {
		w.uvarint(1)
		w.uvarint(1)
		w.str(segDirName(1))
	})
	if err := os.WriteFile(filepath.Join(dir, segDirName(1)), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open: got %v, want ErrCorrupt", err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a file where the segment belongs also reads as fs.ErrNotExist: %v", err)
	}
}

// TestCommitDoesNotFollowASymlinkedTempManifest guards the one place weft
// writes to a predictable path in a directory it does not own. os.Create
// follows a symlink standing there, which would turn "can write in the index
// directory" into "can overwrite any file this process can reach".
func TestCommitDoesNotFollowASymlinkedTempManifest(t *testing.T) {
	const bystander = "a file weft has no business writing"
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "bystander")
	if err := os.WriteFile(outside, []byte(bystander), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, manifestName+".tmp")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}

	ix := New()
	addAll(t, ix, []Document{{Key: "a", Text: "x"}})
	// Whether the commit succeeds is not the point — it may legitimately fail
	// on a directory somebody else is meddling with. What it must never do is
	// write through the link.
	commitErr := ix.Commit(dir)

	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("the bystander file is gone (commit returned %v): %v", commitErr, err)
	}
	if string(got) != bystander {
		t.Fatalf("Commit wrote through the symlink; the bystander now holds %q", got)
	}
}

// TestSymlinkedSegmentIsRefused covers what the manifest's name check cannot.
// "seg-000001" is a syntactically perfect segment name; whether the entry
// standing there is a directory or a link to somebody else's index is a
// question about the filesystem, not about the manifest's bytes. Every path
// weft opens is resolved inside a root on the index directory, so the answer
// comes from the OS rather than from a check of ours that a rename could race.
func TestSymlinkedSegmentIsRefused(t *testing.T) {
	// A real, valid, committed index — the thing worth stealing.
	elsewhere, victimSeg := commitTiny(t)

	// A second index directory whose manifest is honest and whose segment is a
	// link into the first.
	dir := t.TempDir()
	rewriteSection(t, filepath.Join(dir, manifestName), kindManifest, func(w *segWriter) {
		w.uvarint(1)
		w.uvarint(1)
		w.str(segDirName(1))
	})
	if err := os.Symlink(victimSeg, filepath.Join(dir, segDirName(1))); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}

	// The link resolves and the documents behind it are readable, so a refusal
	// below is weft declining to follow it rather than the setup not working.
	if _, err := os.ReadFile(filepath.Join(dir, segDirName(1), docsFile)); err != nil {
		t.Fatalf("the symlink does not lead anywhere, so this test proves nothing: %v", err)
	}
	if _, err := Open(elsewhere); err != nil {
		t.Fatalf("the index being linked to is not loadable, so this test proves nothing: %v", err)
	}

	// Refusing is half of it. The refusal also has to read as ErrCorrupt: a
	// symlink standing where a segment belongs is a foreign layout, and Open
	// documents foreign layouts as corruption. Reported raw, the caller
	// branching on errors.Is(err, ErrCorrupt) falls through to "unknown error"
	// for the one case the root exists to catch.
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("symlinked segment: got %v, want ErrCorrupt", err)
	}

	// The same question one level down: a section file linked out of the
	// segment directory.
	honest := t.TempDir()
	rewriteSection(t, filepath.Join(honest, manifestName), kindManifest, func(w *segWriter) {
		w.uvarint(1)
		w.uvarint(1)
		w.str(segDirName(1))
	})
	if err := os.Mkdir(filepath.Join(honest, segDirName(1)), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range segSections {
		if err := os.Symlink(filepath.Join(victimSeg, s.name), filepath.Join(honest, segDirName(1), s.name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(honest); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("symlinked section files: got %v, want ErrCorrupt", err)
	}
}

// TestAnEntryOfTheWrongKindAtTheManifestIsCorrupt extends the classification the
// section files and the segment directory already get to the entry point. A
// directory named MANIFEST is a foreign layout, and reported raw it is neither
// ErrCorrupt nor fs.ErrNotExist — so Open's caller has no branch to take, and
// Commit's "no manifest means a first commit" does not recognise it either.
func TestAnEntryOfTheWrongKindAtTheManifestIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, manifestName), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a directory at MANIFEST: got %v, want ErrCorrupt", err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a directory at MANIFEST also reads as fs.ErrNotExist: %v", err)
	}
	ix := New()
	addAll(t, ix, []Document{{Key: "a", Text: "x"}})
	if err := ix.Commit(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Commit over a directory at MANIFEST: got %v, want ErrCorrupt", err)
	}
}

// TestADirectoryWhereASectionBelongsIsCorrupt is the section-level twin of the
// test above. A manifest naming a segment has said this directory is an index,
// so a directory standing where one of its four files belongs is a foreign or
// damaged layout too. Left unwrapped, the raw EISDIR would leave
// errors.Is(err, ErrCorrupt) false, contrary to what Open documents.
func TestADirectoryWhereASectionBelongsIsCorrupt(t *testing.T) {
	for _, s := range segSections {
		name := s.name
		t.Run(name, func(t *testing.T) {
			dir, segDir := commitTiny(t)
			path := filepath.Join(segDir, name)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := Open(dir)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("a directory at %s: got %v, want ErrCorrupt", name, err)
			}
			if errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a directory at %s also reads as fs.ErrNotExist: %v", name, err)
			}
		})
	}
}

// TestGenerationZeroIsRefused covers the other end of the counter. The first
// commit publishes generation 1, so a checksum-valid manifest claiming 0 —
// naming seg-000000, which is what makes it survive the name check — describes a
// state no sequence of commits could produce. Accepting it lets Open load a
// foreign format-v1 layout, and lets Commit publish over it and sweep away a
// directory weft never wrote.
func TestGenerationZeroIsRefused(t *testing.T) {
	dir := t.TempDir()
	rewriteSection(t, filepath.Join(dir, manifestName), kindManifest, func(w *segWriter) {
		w.uvarint(0)
		w.uvarint(1)
		w.str(segDirName(0))
	})
	bystander := filepath.Join(dir, segDirName(0), "notes.txt")
	if err := os.MkdirAll(filepath.Dir(bystander), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bystander, []byte("not weft's"), 0o644); err != nil {
		t.Fatal(err)
	}

	ix := New()
	addAll(t, ix, []Document{{Key: "a", Text: "must not supersede a state weft never wrote"}})
	if err := ix.Commit(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Commit: got %v, want ErrCorrupt", err)
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Fatalf("the swept directory was not weft's to sweep: %v", err)
	}
	// readManifest is shared, so the same impossible counter is refused on read.
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open: got %v, want ErrCorrupt", err)
	}
}

// TestFirstCommitRefusesADirectoryInsideASegmentName closes the gap a name-only
// ownership check leaves. weft's writer puts nothing but the four section files
// in a segment directory, so a *directory* called meta, docs, postings or terms
// is somebody else's — and the RemoveAll that clears the target segment is
// recursive, so accepting it deletes everything beneath.
func TestFirstCommitRefusesADirectoryInsideASegmentName(t *testing.T) {
	for _, s := range segSections {
		name := s.name
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			bystander := filepath.Join(dir, segDirName(1), name, "vacation-photos")
			if err := os.MkdirAll(filepath.Dir(bystander), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bystander, []byte("not weft's"), 0o644); err != nil {
				t.Fatal(err)
			}

			ix := New()
			addAll(t, ix, []Document{{Key: "a", Text: "x"}})
			if err := ix.Commit(dir); err == nil {
				t.Fatalf("Commit claimed a directory holding a %s directory weft never wrote", name)
			}
			if _, err := os.Stat(bystander); err != nil {
				t.Fatalf("Commit recursively deleted a directory it does not own: %v", err)
			}
		})
	}
}

// TestFirstCommitRefusesAForeignSectionFile closes the last of the name-only
// ownership gaps. A regular file called meta, docs, postings or terms has the
// right name and is the right kind of entry, and is still not necessarily
// weft's: the magic is what says so. What a crashed commit leaves is again the
// yardstick — segWriter creates each section exclusively and buffers until
// close, so its debris is an empty file or a prefix of a weft frame.
func TestFirstCommitRefusesAForeignSectionFile(t *testing.T) {
	const bystander = "somebody's data, under a name weft happens to reserve"
	commitInto := func(t *testing.T, dir string) error {
		t.Helper()
		ix := New()
		addAll(t, ix, []Document{{Key: "a", Text: "x"}})
		return ix.Commit(dir)
	}
	plant := func(t *testing.T, dir, name string, content []byte) string {
		t.Helper()
		path := filepath.Join(dir, segDirName(1), name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	for _, s := range segSections {
		name := s.name
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := plant(t, dir, name, []byte(bystander))
			if err := commitInto(t, dir); err == nil {
				t.Fatalf("Commit claimed a directory holding a %s file weft never wrote", name)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("Commit deleted a file it does not own: %v", err)
			}
			if string(got) != bystander {
				t.Fatalf("the bystander %s now holds %q", name, got)
			}
		})
	}

	// A commit that died after creating a section file and before writing it
	// out, and one that died mid-write with less than the magic on disk. Both
	// have to stay overwritable, or a crash would wedge the directory.
	t.Run("own debris", func(t *testing.T) {
		dir := t.TempDir()
		plant(t, dir, metaFile, nil)
		plant(t, dir, docsFile, segMagic[:2])
		if err := commitInto(t, dir); err != nil {
			t.Fatalf("Commit over its own unpublished debris: %v", err)
		}
		if _, err := Open(dir); err != nil {
			t.Fatalf("Open: %v", err)
		}
	})
}

// TestFirstCommitRefusesAForeignTempManifest extends the ownership test to the
// one other name weft reserves. Commit clears MANIFEST.tmp before writing its
// own, and with no MANIFEST there is nothing to prove that file is weft's
// either. What a crashed commit leaves is the yardstick: segWriter buffers until
// close, so its debris is an empty file or a prefix of weft's frame — and that
// much still has to be overwritable, or a crash would wedge the directory.
func TestFirstCommitRefusesAForeignTempManifest(t *testing.T) {
	commitInto := func(t *testing.T, dir string) error {
		t.Helper()
		ix := New()
		addAll(t, ix, []Document{{Key: "a", Text: "x"}})
		return ix.Commit(dir)
	}
	tmp := manifestName + ".tmp"

	t.Run("foreign", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, tmp)
		const bystander = "somebody's notes, under a name weft happens to reserve"
		if err := os.WriteFile(path, []byte(bystander), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := commitInto(t, dir); err == nil {
			t.Fatal("Commit claimed a directory holding a temp manifest weft never wrote")
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("Commit deleted a file it does not own: %v", err)
		}
		if string(got) != bystander {
			t.Fatalf("the bystander file now holds %q", got)
		}
		// A refused commit mutates nothing at all, not even the parts it got to.
		if names := dirNames(t, dir); !slices.Equal(names, []string{tmp}) {
			t.Fatalf("a refused commit left something behind: %v", names)
		}
	})

	// A commit that died between creating the temp manifest and writing it out.
	t.Run("own debris", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, tmp), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := commitInto(t, dir); err != nil {
			t.Fatalf("Commit over its own unpublished debris: %v", err)
		}
		if _, err := Open(dir); err != nil {
			t.Fatalf("Open: %v", err)
		}
	})
}

func TestManifestNamingAnEscapePathIsRefused(t *testing.T) {
	// A doctored manifest must not be able to point the reader outside dir.
	dir := t.TempDir()
	rewriteSection(t, filepath.Join(dir, manifestName), kindManifest, func(w *segWriter) {
		w.uvarint(1)
		w.uvarint(1)
		w.str("seg-../../etc")
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("escape path in manifest: got %v, want ErrCorrupt", err)
	}
}

func TestManifestListingTwoSegmentsIsRefused(t *testing.T) {
	// The list exists for milestone 3; a version-1 file using it is a writer
	// that broke its contract without bumping the version.
	dir, _ := commitTiny(t)
	rewriteSection(t, filepath.Join(dir, manifestName), kindManifest, func(w *segWriter) {
		w.uvarint(1)
		w.uvarint(2)
		w.str("seg-000001")
		w.str("seg-000001")
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("two segments under version 1: got %v, want ErrCorrupt", err)
	}
}

// TestCommitDuringReads exists for the race detector: Commit takes the same
// read lock queries do, so encoding a snapshot while lookups run must be
// clean. Run with -race (the Makefile default) or it proves little.
func TestCommitDuringReads(t *testing.T) {
	ix := New()
	addAll(t, ix, []Document{{Key: "a", Text: "read me"}, {Key: "b", Text: "me too"}})
	dir := t.TempDir()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 50 {
			ix.Lookup("read")
			ix.Stats()
			ix.Doc(0)
		}
	}()
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	<-done
	if _, err := Open(dir); err != nil {
		t.Fatalf("Open: %v", err)
	}
}

// --------------------------------------------------------------------------
// Reproducers for the PR #10 review. Each is a state a caller can reach — or a
// directory a caller can be handed — where the answer served is wrong rather
// than absent.
// --------------------------------------------------------------------------

// TestCommitAfterAVectorlessBatchStaysScrubbable is incremental ingestion of a
// corpus whose vectors do not arrive with every batch.
//
// ix.vecDim is the width the corpus established and Add enforces it, so it
// deliberately outlives a commit. What must not outlive one is its appearance in
// the next segment's meta: a batch holding no vectors writes documents that
// decode with width zero, and meta claiming the corpus width makes Scrub reject
// a segment the writer itself produced.
func TestCommitAfterAVectorlessBatchStaysScrubbable(t *testing.T) {
	dir := t.TempDir()
	ix := New()
	addAll(t, ix, []Document{{Key: "with", Text: "a vector", Vector: []float32{1, 0, 0, 1}}})
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	addAll(t, ix, []Document{{Key: "without", Text: "no vector at all"}})
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("second Commit: %v", err)
	}
	if err := Scrub(dir); err != nil {
		t.Fatalf("Scrub after a batch that carried no vectors: %v", err)
	}
	// And the width is still enforced, so narrowing what meta records did not
	// narrow what the corpus accepts.
	if _, err := ix.Add(Document{Key: "wrong", Text: "x", Vector: []float32{1, 2}}); !errors.Is(err, ErrDimMismatch) {
		t.Errorf("Add of a 2-wide vector into a 4-wide corpus: got %v, want ErrDimMismatch", err)
	}
}

// TestOpenRefusesDamagedMeta is the one section whose cost argument does not
// apply.
//
// Open skips frame checksums because computing them is the size of the index.
// meta is three uvarints, read in full on every Open, and every BM25 score in
// the index is normalized by the number in it. Nothing else can catch a change
// to it: no unit seals meta, and the value is simply believed downstream.
func TestOpenRefusesDamagedMeta(t *testing.T) {
	dir := t.TempDir()
	ix := New()
	addAll(t, ix, []Document{{Key: "one", Text: "a b c"}})
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// meta is uvarint(count), uvarint(totalLen), uvarint(vecDim): 01 03 00 for
	// this corpus. Doubling the token count still decodes, so only the frame
	// checksum stands between it and every score in the index.
	path := filepath.Join(dir, segDirName(1), metaFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if b[segHeaderLen] != 1 || b[segHeaderLen+1] != 3 {
		t.Fatalf("meta payload is %v, this test is patching the wrong byte", b[segHeaderLen:segHeaderLen+3])
	}
	b[segHeaderLen+1] = 6
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Open(dir)
	if err == nil {
		defer got.Close() //nolint:errcheck // teardown
		_, avg := got.Stats()
		t.Fatalf("Open accepted damaged meta; every query now normalizes by an average length of %v", avg)
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open with damaged meta: got %v, want ErrCorrupt", err)
	}
}

// TestScrubRefusesAManifestCountThatDisagreesWithMeta closes a gap between the
// two verification paths.
//
// Open compares the manifest's count against meta's, because every later
// segment's base was computed from the manifest's number. Scrub loads each
// segment and throws the result away without making that comparison — so it
// reports success for a directory Open then refuses, which is the exact opposite
// of what a caller runs it for.
func TestScrubRefusesAManifestCountThatDisagreesWithMeta(t *testing.T) {
	dir := t.TempDir()
	ix := New()
	addAll(t, ix, []Document{{Key: "one", Text: "a b c"}})
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The same segment, republished under a manifest claiming it holds two.
	republish(t, dir, 1, []segInfo{{name: segDirName(1), base: 0, count: 2}})

	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open with a manifest count meta contradicts: got %v, want ErrCorrupt", err)
	}
	if err := Scrub(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Scrub passed a directory Open refuses: got %v, want ErrCorrupt", err)
	}
}

// TestScrubRefusesAKeyHeldByTwoSegments is the invariant Add enforces and
// nothing re-checks after the fact.
//
// Keys are unique across the whole index — Resolve rests on it, and it is why
// the first segment to answer is treated as the only one that can. Add refuses a
// duplicate against the segments, so this state is unreachable through the API
// and perfectly reachable by copying a segment directory in. Scrub is documented
// as what a caller runs before trusting a copied directory.
func TestScrubRefusesAKeyHeldByTwoSegments(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	a := New()
	addAll(t, a, []Document{{Key: "dup", Text: "the first corpus"}})
	if err := a.Commit(one); err != nil {
		t.Fatalf("Commit a: %v", err)
	}
	b := New()
	addAll(t, b, []Document{{Key: "dup", Text: "the second corpus"}})
	if err := b.Commit(two); err != nil {
		t.Fatalf("Commit b: %v", err)
	}

	copyTree(t, filepath.Join(two, segDirName(1)), filepath.Join(one, segDirName(2)))
	republish(t, one, 2, []segInfo{
		{name: segDirName(1), base: 0, count: 1},
		{name: segDirName(2), base: 1, count: 1},
	})

	if err := Scrub(one); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Scrub with %q in two segments: got %v, want ErrCorrupt", "dup", err)
	}
}

// TestCommitRefusesADirectoryItDidNotOpen is what a document count cannot tell
// apart.
//
// Commit checks that the destination holds as many documents as this index has
// committed, because the pending ids are numbered from that. Two unrelated
// directories of the same size pass that check identically, and the commit then
// joins this index's new documents to the other directory's history: the live
// object keeps answering from the segments it opened, a reopen answers from the
// ones on disk, and a later merge writes the disagreement down.
func TestCommitRefusesADirectoryItDidNotOpen(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	a := New()
	addAll(t, a, []Document{{Key: "a-one", Text: "the first corpus"}})
	if err := a.Commit(one); err != nil {
		t.Fatalf("Commit a: %v", err)
	}
	b := New()
	addAll(t, b, []Document{{Key: "b-one", Text: "the second corpus"}})
	if err := b.Commit(two); err != nil {
		t.Fatalf("Commit b: %v", err)
	}

	ix, err := Open(one)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ix.Close() //nolint:errcheck // teardown
	if _, err := ix.Add(Document{Key: "later", Text: "added to the first corpus"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Same document count, unrelated history.
	if err := ix.Commit(two); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Commit into a directory this index was not opened from: got %v, want ErrCorrupt", err)
	}
	// And the directory it did open is still where it can commit.
	if err := ix.Commit(one); err != nil {
		t.Fatalf("Commit into its own directory: %v", err)
	}
}

// TestOpenRefusesADamagedDocOffsetTable is meta's argument applied to the other
// table Open already reads a header of.
//
// A docoff entry's token count is the one derived value nothing downstream can
// contradict: the record carries its own copy, but reading it costs a document
// decode per posting, which is what this table exists to avoid. So BM25
// normalizes by whatever the mapped bytes say until somebody runs Scrub. A frame
// checksum over the table is 16 bytes a document — the same order as the terms
// index Open already decodes in full, and a scan rather than a map build.
//
// What stays lazy is re-deriving every offset from the record it points at,
// which the scrub's own walk does. That one is the size of the corpus, and it is
// Scrub's.
func TestOpenRefusesADamagedDocOffsetTable(t *testing.T) {
	dir := t.TempDir()
	ix := New()
	addAll(t, ix, []Document{{Key: "one", Text: "a b c"}})
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// uvarint(1), then one 16-byte entry: offset in the low eight, token count
	// in the high eight. The token count is what BM25 divides by.
	path := filepath.Join(dir, segDirName(1), docoffFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	at := segHeaderLen + 1 + docoffLenAt
	if b[at] != 3 {
		t.Fatalf("docoff token count is %d, this test is patching the wrong byte", b[at])
	}
	b[at] = 6
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Open(dir)
	if err == nil {
		defer got.Close() //nolint:errcheck // teardown
		t.Fatalf("Open accepted a damaged length table; DocLen(0) is now %d", got.DocLen(0))
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open with a damaged docoff table: got %v, want ErrCorrupt", err)
	}
}

// TestMergeRefusesWhenOneSegmentsPostingsAreDamaged is the gap the first pass at
// this check left open.
//
// Refusing only when a term comes back with no postings at all catches damage in
// the single segment that held it. A term in nine segments loses one segment's
// list and the other eight still make the result non-empty, so the merge
// published a replacement missing those documents' postings and pruned the
// segment that could have supplied them — an internally consistent index that
// Scrub cannot tell from a smaller corpus.
func TestMergeRefusesWhenOneSegmentsPostingsAreDamaged(t *testing.T) {
	dir := t.TempDir()
	ix := commitEach(t, dir, 9) // every document carries the term "shared"
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	before := dirNames(t, dir)

	path := filepath.Join(dir, segDirName(1), postingsFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[segHeaderLen+3] ^= 0xff
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown
	if got.Lookup("shared") == nil {
		t.Fatal("no segment answers for the shared term; this test damaged the wrong file")
	}
	if err := got.Merge(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Merge with one segment's postings damaged: got %v, want ErrCorrupt", err)
	}
	for _, name := range before {
		if !slices.Contains(dirNames(t, dir), name) {
			t.Errorf("a refused merge removed %s", name)
		}
	}
}

// TestOpenRefusesTwoVectorWidthsInOneIndex is the corpus-wide invariant Add
// enforces, checked where the corpus is assembled rather than only inside each
// segment.
//
// Two individually valid segments of different widths open without complaint and
// leave ix.vecDim as whichever came last. Every vector query then reaches a
// document from the other segment and aborts, and Add enforces a width the
// corpus does not actually have.
func TestOpenRefusesTwoVectorWidthsInOneIndex(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	a := New()
	addAll(t, a, []Document{{Key: "wide", Text: "four wide", Vector: []float32{1, 0, 0, 1}}})
	if err := a.Commit(one); err != nil {
		t.Fatalf("Commit a: %v", err)
	}
	b := New()
	addAll(t, b, []Document{{Key: "narrow", Text: "two wide", Vector: []float32{1, 0}}})
	if err := b.Commit(two); err != nil {
		t.Fatalf("Commit b: %v", err)
	}

	copyTree(t, filepath.Join(two, segDirName(1)), filepath.Join(one, segDirName(2)))
	republish(t, one, 2, []segInfo{
		{name: segDirName(1), base: 0, count: 1},
		{name: segDirName(2), base: 1, count: 1},
	})

	if ix, err := Open(one); !errors.Is(err, ErrCorrupt) {
		if err == nil {
			ix.Close() //nolint:errcheck // teardown
		}
		t.Fatalf("Open with a 4-wide and a 2-wide segment: got %v, want ErrCorrupt", err)
	}
	if err := Scrub(one); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Scrub with two vector widths: got %v, want ErrCorrupt", err)
	}
}

// TestAdoptReleasesTheCommittedBatch is milestone 3's claim, checked rather than
// assumed.
//
// Reslicing ix.docs to zero length keeps the backing array, and every Document
// in it keeps its text, vector and links reachable. Reads have moved to the
// mapping by then, so the corpus would be held twice — once in the page cache
// where it belongs and once on the Go heap the mapping exists to stay off.
func TestAdoptReleasesTheCommittedBatch(t *testing.T) {
	dir := t.TempDir()
	ix := New()
	addAll(t, ix, []Document{{Key: "held", Text: "a text worth releasing", Vector: []float32{1, 2}, Links: []string{"held"}}})
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Past the length, inside the capacity: what the garbage collector still
	// sees.
	held := ix.docs[:1:1]
	if held[0].Text != "" || held[0].Vector != nil || held[0].Links != nil {
		t.Errorf("adopt left the committed batch reachable: %+v", held[0])
	}
	// And the document is still readable, through the mapping.
	if d, ok := ix.Doc(0); !ok || d.Text != "a text worth releasing" {
		t.Errorf("Doc(0) = %+v, %v after adopt", d, ok)
	}
}

// TestOpenStoresAnAbsoluteDirectory is what a relative path costs once the
// directory is remembered rather than used immediately.
//
// Merge reopens ix.dir and Commit now stats it, both of them later than Open. A
// process that changes its working directory in between would send Merge
// somewhere else entirely, and would make Commit fail against the correct
// absolute destination because the stat of the remembered path went astray
// first.
func TestOpenStoresAnAbsoluteDirectory(t *testing.T) {
	dir := t.TempDir()
	ix := New()
	addAll(t, ix, []Document{{Key: "one", Text: "a b c"}})
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	t.Chdir(filepath.Dir(dir))
	got, err := Open(filepath.Base(dir))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown
	if !filepath.IsAbs(got.dir) {
		t.Errorf("Open remembered %q, which stops naming this index the moment the process moves", got.dir)
	}

	// The same for the path Commit remembers.
	fresh := New()
	addAll(t, fresh, []Document{{Key: "two", Text: "d e f"}})
	if err := fresh.Commit("."); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	defer fresh.Close() //nolint:errcheck // teardown
	if !filepath.IsAbs(fresh.dir) {
		t.Errorf("Commit remembered %q", fresh.dir)
	}
}

// TestOpenRereadsAManifestAMergeReplaced is the window between reading the
// manifest and mapping what it names, reproduced without racing for it.
//
// Merge publishes its replacement and then prunes the segments it replaced, so a
// reader holding the pre-merge list maps names that are no longer there.
// Mappings already taken survive the unlink; only this window does not, and
// nothing is wrong with the directory while it is open.
//
// The retry itself cannot be driven from outside — Open re-reads the manifest
// from disk, so a test cannot hand it a stale one. What is pinned here instead
// is both ends of it: the window produces errSegmentGone and nothing else, and
// a segment that stays missing is still reported rather than spun on.
func TestOpenRereadsAManifestAMergeReplaced(t *testing.T) {
	dir := t.TempDir()
	ix := commitEach(t, dir, 9)
	defer ix.Close() //nolint:errcheck // teardown

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// What a reader that got here before the merge is holding.
	_, stale, err := readManifest(root)
	if err != nil {
		t.Fatal(err)
	}

	if err := ix.Merge(); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	if _, err := mapGeneration(root, dir, stale); !errors.Is(err, errSegmentGone) {
		t.Fatalf("mapping the manifest a merge replaced: got %v, want errSegmentGone", err)
	}
	// And it is still ErrCorrupt to every caller that only asks what kind of
	// failure this is, so telling it apart cost no caller anything.
	if !errors.Is(errSegmentGone, ErrCorrupt) {
		t.Error("errSegmentGone stopped reporting as ErrCorrupt")
	}

	// The directory itself was valid throughout.
	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after the merge that made that list stale: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown
	if got.Len() != 9 {
		t.Errorf("Open recovered %d documents, want 9", got.Len())
	}

	// A segment that is gone for good is damage, and bounded retries are what
	// keep it from becoming a spin.
	if err := os.RemoveAll(filepath.Join(dir, segDirName(10))); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open with a segment that stays missing: got %v, want ErrCorrupt", err)
	}
}

// TestOpenSurvivesAConcurrentMerge is the same window reached the way a caller
// reaches it: a reader looping against a writer that commits, merges and prunes.
// It is a smoke test rather than a proof — the window is microseconds wide — and
// it is here because the failure it looks for is one no single-threaded test can
// produce.
//
// Nine segments is one over the merge threshold, so each round merges and prunes
// for real while the reader hammers the same directory.
func TestOpenSurvivesAConcurrentMerge(t *testing.T) {
	if testing.Short() {
		t.Skip("races a writer for a few hundred opens")
	}
	dir := t.TempDir()
	ix := New()
	defer ix.Close() //nolint:errcheck // teardown
	// Nine segments carrying a corpus rather than a document each: a scrub of
	// nine trivial segments finishes inside the window it is supposed to be
	// caught in, so there is nothing for a merge to overtake.
	for i := range 9 {
		ubiquitousCorpus(t, ix, i*400, 400, 200)
		if err := ix.Commit(dir); err != nil {
			t.Fatalf("Commit %d: %v", i, err)
		}
	}

	stop := make(chan struct{})
	bad := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := Open(dir)
			if err != nil {
				select {
				case bad <- err:
				default:
				}
				return
			}
			got.Close() //nolint:errcheck // the reader only cares that it opened
		}
	}()

	for i := range 30 {
		if _, err := ix.Add(Document{Key: fmt.Sprintf("round-%03d", i), Text: "shared term0 and more shared"}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if err := ix.Commit(dir); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := ix.Merge(); err != nil {
			t.Fatalf("Merge: %v", err)
		}
	}
	close(stop)
	<-done
	select {
	case err := <-bad:
		t.Fatalf("Open failed while a merge was running: %v", err)
	default:
	}
}

// TestANoOpCommitStillSweepsDebris covers the one exit from Commit that skips
// the sweep.
//
// Open documents unnamed debris as costing disk "until the next Commit", and a
// crash between writing a segment and publishing it is exactly what leaves some.
// The reopened index has nothing pending, so the next Commit returns before it
// reaches prune and the orphan — as large as the batch that was interrupted —
// stays until some later commit happens to carry a document.
func TestANoOpCommitStillSweepsDebris(t *testing.T) {
	dir, _, ix := commitSeeded(t)
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// What a commit that died before its rename leaves behind: a complete
	// segment directory no manifest names. Copied from the live one, so the
	// bytes are weft's own and the sweep is refusing nothing.
	debris := segDirName(2)
	copyTree(t, filepath.Join(dir, segDirName(1)), filepath.Join(dir, debris))

	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown
	if err := got.Commit(dir); err != nil {
		t.Fatalf("Commit with nothing pending: %v", err)
	}
	if names := dirNames(t, dir); slices.Contains(names, debris) {
		t.Errorf("a no-op Commit left %s standing: %v", debris, names)
	}
	// And it swept debris rather than the corpus.
	if err := Scrub(dir); err != nil {
		t.Fatalf("Scrub after the sweep: %v", err)
	}
	if got.Len() != 4 {
		t.Errorf("Len = %d after a no-op Commit, want 4", got.Len())
	}
}

// TestScrubSurvivesAConcurrentMerge is Open's window on the other whole-index
// reader.
//
// Scrub reads the manifest and then loads what it names, and Merge publishes its
// replacement before pruning what it replaced — so a scrub overtaken by a merge
// reaches a segment directory that is no longer there. The directory is a valid
// index the whole time. Reporting ErrCorrupt for it turns a scheduled integrity
// check into a false alarm, which is worse than useless: the answer a caller
// runs Scrub for is precisely "is this directory sound".
//
// A smoke test rather than a proof, for the same reason and in the same shape as
// TestOpenSurvivesAConcurrentMerge — the window is not reachable from a single
// goroutine.
func TestScrubSurvivesAConcurrentMerge(t *testing.T) {
	if testing.Short() {
		t.Skip("races a writer for a few hundred scrubs")
	}
	dir := t.TempDir()
	ix := New()
	defer ix.Close() //nolint:errcheck // teardown
	// Nine segments carrying a corpus rather than a document each: a scrub of
	// nine trivial segments finishes inside the window it is supposed to be
	// caught in, so there is nothing for a merge to overtake.
	for i := range 9 {
		ubiquitousCorpus(t, ix, i*400, 400, 200)
		if err := ix.Commit(dir); err != nil {
			t.Fatalf("Commit %d: %v", i, err)
		}
	}

	stop := make(chan struct{})
	bad := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := Scrub(dir); err != nil {
				select {
				case bad <- err:
				default:
				}
				return
			}
		}
	}()

	for i := range 10 {
		ubiquitousCorpus(t, ix, 3600+i, 1, 200)
		if err := ix.Commit(dir); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := ix.Merge(); err != nil {
			t.Fatalf("Merge: %v", err)
		}
	}
	close(stop)
	<-done
	select {
	case err := <-bad:
		t.Fatalf("Scrub reported a valid directory as damaged while a merge was running: %v", err)
	default:
	}
}

// TestCommitAndMergeSerialize is the one piece of Commit that used to run
// outside the lock the rest of it holds.
//
// Commit's manifest snapshot decides two destructive things: which directory it
// clears before writing, and which segments the manifest it publishes names. A
// Merge admitted between that read and the lock makes both wrong — the clear
// lands on the segment the merge just published as live, and the manifest that
// follows names directories the merge already pruned. Neither is recoverable:
// the flip is the commit point, so the damage is durable.
//
// Both are public mutators of the same index, so serializing them is not an
// extra guarantee. It is the one the write lock was already supposed to give.
func TestCommitAndMergeSerialize(t *testing.T) {
	if testing.Short() {
		t.Skip("races a commit against a merge a few hundred times")
	}
	dir := t.TempDir()
	ix := commitEach(t, dir, 9)
	defer ix.Close() //nolint:errcheck // teardown

	// Nine segments and one over the ceiling on every round, so every merge
	// publishes and prunes for real while a commit is trying to read the manifest
	// it is replacing.
	for i := range 60 {
		if _, err := ix.Add(Document{Key: fmt.Sprintf("round-%03d", i), Text: "shared term0 and more shared"}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		errc := make(chan error, 1)
		go func() { errc <- ix.Merge() }()
		cerr := ix.Commit(dir)
		merr := <-errc
		if cerr != nil {
			t.Fatalf("round %d: Commit alongside Merge: %v", i, cerr)
		}
		if merr != nil {
			t.Fatalf("round %d: Merge alongside Commit: %v", i, merr)
		}
		if err := Scrub(dir); err != nil {
			t.Fatalf("round %d: the directory a Commit and a Merge both wrote: %v", i, err)
		}
	}

	// Every document ever added is still reachable through a fresh open — a
	// manifest naming pruned segments would have taken a generation with it.
	want := ix.Len()
	got, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // teardown
	if got.Len() != want {
		t.Errorf("Open recovered %d documents, want %d", got.Len(), want)
	}
}
