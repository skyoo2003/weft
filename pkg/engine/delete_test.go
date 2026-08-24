// SPDX-License-Identifier: Apache-2.0

// Update, and the one thing deletion makes newly possible: a Key that more than
// one record carries.
//
// Those are one file because they are one problem. An update of a document that
// is already on disk cannot rewrite the segment holding it, so it tombstones the
// old record and appends a new one — and from then on two records, possibly in
// one segment, answer to the same Key. Every structure that assumed a key
// appeared once has to say what it means now, and this file is where each of
// them says it.
package engine_test

import (
	"errors"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

// updatedDoc is deletionDoc with different text and a different vector, so a
// successful update is visible in the inverted index, in the vector candidates
// and in the record itself.
func updatedDoc(i int) engine.Document {
	d := deletionDoc(i)
	d.Text = "gamma delta " + deletionTerm(i) + "x"
	d.Vector = []float32{9, 9, 9, 9}
	return d
}

// assertReadsBack is the "an update gives the latest read" assertion, whole: the
// new text is what the record holds, the new term finds it, and the term it no
// longer holds does not.
func assertReadsBack(t *testing.T, ix *engine.Index, i int) {
	t.Helper()
	key := deletionKey(i)
	id, ok := ix.Resolve(key)
	if !ok {
		t.Fatalf("Resolve(%q) after updating it: not found", key)
	}
	d, ok := ix.Doc(id)
	if !ok {
		t.Fatalf("Doc(%d) after updating %q: not found", id, key)
	}
	if want := updatedDoc(i).Text; d.Text != want {
		t.Errorf("Doc(%d).Text = %q, want %q — the read is stale", id, d.Text, want)
	}

	// The new term reaches it and the retired one does not. A stale posting is
	// the half of an update that a record-level check cannot see: the document
	// reads correctly and still answers a query for text it no longer contains.
	found := false
	for _, p := range ix.Lookup("gamma") {
		found = found || p.Doc == id
	}
	if !found {
		t.Errorf("Lookup(%q) does not name document %d after its update", "gamma", id)
	}
	for _, p := range ix.Lookup(deletionTerm(i)) {
		if p.Doc == id {
			t.Errorf("Lookup(%q) still names document %d, whose text no longer holds it", deletionTerm(i), p.Doc)
		}
	}
}

// TestUpdateOfAPendingDocumentKeepsItsID is the cheap half of an update, and the
// reason it is worth a separate path: nothing has been written, so the record can
// be replaced where it stands and no id is spent.
func TestUpdateOfAPendingDocumentKeepsItsID(t *testing.T) {
	ix := engine.New()
	deletionCorpus(t, ix)

	before, ok := ix.Resolve(deletionKey(5))
	if !ok {
		t.Fatal("Resolve before updating: not found")
	}
	after, err := ix.Update(updatedDoc(5))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if after != before {
		t.Errorf("Update moved a pending document from %d to %d; nothing was written, so nothing had to move", before, after)
	}
	if got := ix.Len(); got != deletionCorpusSize {
		t.Errorf("Len() = %d after updating in place, want %d — an id was spent", got, deletionCorpusSize)
	}
	docs, _ := ix.Stats()
	if docs != deletionCorpusSize {
		t.Errorf("Stats() counts %d documents after an in-place update, want %d", docs, deletionCorpusSize)
	}
	assertReadsBack(t, ix, 5)
}

// TestUpdateOfACommittedDocumentSpendsAnID is the other half. The record is in a
// mapped segment and segments are immutable, so the old one becomes a tombstone
// and the new one is appended.
func TestUpdateOfACommittedDocumentSpendsAnID(t *testing.T) {
	dir := t.TempDir()
	ix := engine.New()
	deletionCorpus(t, ix)
	if err := ix.Commit(t.Context(), dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	defer ix.Close() //nolint:errcheck // teardown

	before, ok := ix.Resolve(deletionKey(5))
	if !ok {
		t.Fatal("Resolve before updating: not found")
	}
	after, err := ix.Update(updatedDoc(5))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if after == before {
		t.Errorf("Update reused id %d for a committed document; a segment cannot be rewritten", before)
	}
	if _, ok := ix.Doc(before); ok {
		t.Errorf("Doc(%d) still answers for the record an update replaced", before)
	}
	// The population did not change even though the id space grew, which is the
	// split Len and Stats now carry.
	docs, _ := ix.Stats()
	if docs != deletionCorpusSize {
		t.Errorf("Stats() counts %d documents after an update, want %d", docs, deletionCorpusSize)
	}
	if got := ix.Len(); got != deletionCorpusSize+1 {
		t.Errorf("Len() = %d, want %d — an update of a committed document spends an id", got, deletionCorpusSize+1)
	}
	assertReadsBack(t, ix, 5)
}

// TestUpdateOfAnUnknownKeyIsRefused. An update that quietly inserted would make a
// typo in a key indistinguishable from a new document, which is the same
// argument ErrDuplicateKey rests on from the other side.
func TestUpdateOfAnUnknownKeyIsRefused(t *testing.T) {
	ix := engine.New()
	deletionCorpus(t, ix)

	if _, err := ix.Update(engine.Document{Key: "nobody", Text: "x"}); !errors.Is(err, engine.ErrNoSuchKey) {
		t.Errorf("Update of an unknown key: got %v, want ErrNoSuchKey", err)
	}
	if got := ix.Len(); got != deletionCorpusSize {
		t.Errorf("Len() = %d after a refused update, want %d — it inserted", got, deletionCorpusSize)
	}
	// And a deleted key is an unknown key, which is what makes Delete-then-Update
	// wrong and Delete-then-Add right.
	if !ix.Delete(deletionKey(2)) {
		t.Fatal("Delete: false")
	}
	if _, err := ix.Update(updatedDoc(2)); !errors.Is(err, engine.ErrNoSuchKey) {
		t.Errorf("Update of a deleted key: got %v, want ErrNoSuchKey", err)
	}
}

// TestAKeyIsReusableOnceItsDocumentIsDeleted is the case that makes a segment
// hold one Key twice, and it is the ordinary spelling of an update for a caller
// who would rather not learn a second method.
//
// Both records land in the same commit, so the segment's keys section is where
// this either works or produces a wrong answer: it is a sorted table reached by
// binary search, and a duplicate in it does not fail, it resolves to whichever
// of the two the search happens to land on.
func TestAKeyIsReusableOnceItsDocumentIsDeleted(t *testing.T) {
	dir := t.TempDir()
	ix := engine.New()
	deletionCorpus(t, ix)

	key := deletionKey(5)
	old, ok := ix.Resolve(key)
	if !ok {
		t.Fatal("Resolve before deleting: not found")
	}
	if !ix.Delete(key) {
		t.Fatal("Delete: false")
	}
	// The same key again, in the same uncommitted batch.
	fresh, err := ix.Add(updatedDoc(5))
	if err != nil {
		t.Fatalf("Add after deleting the key: %v — a key whose only holder is a tombstone is not taken", err)
	}
	if fresh == old {
		t.Fatalf("Add reused id %d, which a tombstone still names", old)
	}
	if err := ix.Commit(t.Context(), dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Scrub is the assertion that the bytes are legal, not merely that this
	// process can read them back.
	if err := engine.Scrub(dir); err != nil {
		t.Fatalf("Scrub a segment holding one key twice: %v", err)
	}
	reopened, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close() //nolint:errcheck // teardown

	got, ok := reopened.Resolve(key)
	if !ok {
		t.Fatalf("Resolve(%q) after reopening: not found", key)
	}
	if got != fresh {
		t.Errorf("Resolve(%q) = %d, want %d — the binary search found the tombstoned record", key, got, fresh)
	}
	assertReadsBack(t, reopened, 5)
}

// TestAnUpdateReadsBackAcrossCommitsAndMerges is the milestone's update metric
// in one test: the latest read survives a commit boundary and a merge.
//
// The merge is the half that could quietly regress. It rewrites the oldest
// segments into one, and the tombstoned record is copied forward with them — so
// the merged segment is the first one to hold both records of a key without
// either having been written beside the other.
func TestAnUpdateReadsBackAcrossCommitsAndMerges(t *testing.T) {
	dir := t.TempDir()
	ix := engine.New()

	// One document per commit, so the segment count passes maxSegments.
	for i := range deletionCorpusSize {
		if _, err := ix.Add(deletionDoc(i)); err != nil {
			t.Fatalf("Add(%q): %v", deletionKey(i), err)
		}
		if err := ix.Commit(t.Context(), dir); err != nil {
			t.Fatalf("Commit %d: %v", i, err)
		}
	}
	// Updated across segment boundaries: an early document, a late one, and one
	// updated twice so the second update takes the pending in-place path over a
	// document the first update had just appended.
	for _, i := range []int{1, 5, 40} {
		if _, err := ix.Update(updatedDoc(i)); err != nil {
			t.Fatalf("Update(%q): %v", deletionKey(i), err)
		}
	}
	if _, err := ix.Update(updatedDoc(5)); err != nil {
		t.Fatalf("second Update(%q): %v", deletionKey(5), err)
	}
	if err := ix.Commit(t.Context(), dir); err != nil {
		t.Fatalf("Commit after updating: %v", err)
	}
	for _, i := range []int{1, 5, 40} {
		assertReadsBack(t, ix, i)
	}
	if err := ix.Merge(); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	for _, i := range []int{1, 5, 40} {
		assertReadsBack(t, ix, i)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := engine.Scrub(dir); err != nil {
		t.Fatalf("Scrub after a merge over updated documents: %v", err)
	}
	reopened, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("Open after merge: %v", err)
	}
	defer reopened.Close() //nolint:errcheck // teardown
	for _, i := range []int{1, 5, 40} {
		assertReadsBack(t, reopened, i)
	}
	// The corpus is the size it started at: three documents were replaced, not
	// added.
	docs, _ := reopened.Stats()
	if docs != deletionCorpusSize {
		t.Errorf("Stats() counts %d documents after three updates, want %d", docs, deletionCorpusSize)
	}
}
