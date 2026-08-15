// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

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
