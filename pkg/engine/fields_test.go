// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func fieldDoc(key, text string, fields ...Field) Document {
	return Document{Key: key, Text: text, Fields: fields}
}

// fieldCorpus commits the documents and reopens them.
//
// Fields are the first thing this format appends to a *record* rather than to a
// section, so the record's own seeded checksum and docoff's offsets both move
// with them. A test that only checked the in-memory half would pass on bytes
// that cannot be read back.
func fieldCorpus(t *testing.T, docs ...Document) *Index {
	t.Helper()
	dir := t.TempDir()
	pending := New()
	for _, d := range docs {
		if _, err := pending.Add(d); err != nil {
			t.Fatalf("Add(%q): %v", d.Key, err)
		}
	}
	if err := pending.Commit(context.Background(), dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	t.Cleanup(func() { _ = pending.Close() })

	opened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	return opened
}

// TestFieldsSurviveACommit is the round trip. Everything else in this file is
// about what fields *mean*; this is about whether the bytes come back.
func TestFieldsSurviveACommit(t *testing.T) {
	want := []Document{
		fieldDoc("a", "body text here",
			Field{Name: "title", Text: "a study of transmission"},
			Field{Name: "author", Text: "kim"}),
		fieldDoc("b", "other body"), // no fields at all, beside one that has them
		fieldDoc("c", "", Field{Name: "title", Text: "no body only title"}),
	}
	ix := fieldCorpus(t, want...)

	for i, w := range want {
		got, ok := ix.Doc(DocID(i))
		if !ok {
			t.Fatalf("Doc(%d) absent", i)
		}
		if !slices.Equal(got.Fields, w.Fields) {
			t.Errorf("Doc(%d).Fields = %v, want %v", i, got.Fields, w.Fields)
		}
		if got.Text != w.Text {
			t.Errorf("Doc(%d).Text = %q, want %q", i, got.Text, w.Text)
		}
	}
}

// TestAFieldIsItsOwnTermSpace is the whole design in one assertion. A field's
// tokens are indexed under FieldTerm, so they are terms of their own — a query
// for the plain token does not reach them and the reverse.
func TestAFieldIsItsOwnTermSpace(t *testing.T) {
	ix := fieldCorpus(t,
		fieldDoc("titled", "", Field{Name: "title", Text: "transmission"}),
		fieldDoc("bodied", "transmission"),
	)

	if got := ix.Lookup("transmission"); len(got) != 1 || got[0].Doc != 1 {
		t.Errorf("Lookup(transmission) = %v, want only the document whose Text holds it", got)
	}
	scoped := ix.Lookup(FieldTerm("title", "transmission"))
	if len(scoped) != 1 || scoped[0].Doc != 0 {
		t.Errorf("Lookup(title\\x00transmission) = %v, want only the titled document", scoped)
	}
	if got := ix.Lookup(FieldTerm("abstract", "transmission")); got != nil {
		t.Errorf("Lookup on a field no document has = %v, want nothing", got)
	}
}

// TestDocLenCountsEveryFieldsTokens pins the consequence Document.Fields
// documents, and it is not a preference: Scrub adds a document's frequencies
// across every term and refuses a disagreement with its stored length, so field
// terms being ordinary postings forces the length to include them.
//
// Committing is what makes this a test rather than a restatement — a segment
// whose lengths and postings disagreed would be written and then refused.
func TestDocLenCountsEveryFieldsTokens(t *testing.T) {
	ix := fieldCorpus(t,
		fieldDoc("both", "one two",
			Field{Name: "title", Text: "three four five"}),
		fieldDoc("textonly", "one two"),
	)

	if got := ix.DocLen(0); got != 5 {
		t.Errorf("DocLen(both) = %d, want 5 — two of Text and three of the title", got)
	}
	if got := ix.DocLen(1); got != 2 {
		t.Errorf("DocLen(textonly) = %d, want 2", got)
	}
	if _, avg := ix.Stats(); avg != 3.5 {
		t.Errorf("AvgDocLen = %v, want 3.5", avg)
	}
}

// TestFieldNamesNoLookupCouldReachAreRefused. Each of the three is a term space
// nobody can name — see ErrBadField — so accepting one would index text into a
// space no query can ask about, which is silent.
func TestFieldNamesNoLookupCouldReachAreRefused(t *testing.T) {
	for name, fields := range map[string][]Field{
		"empty name":       {{Name: "", Text: "x"}},
		"separator inside": {{Name: "a\x00b", Text: "x"}},
		"repeated name":    {{Name: "title", Text: "x"}, {Name: "title", Text: "y"}},
	} {
		t.Run(name, func(t *testing.T) {
			ix := New()
			if _, err := ix.Add(fieldDoc("k", "body", fields...)); !errors.Is(err, ErrBadField) {
				t.Errorf("Add: got %v, want ErrBadField", err)
			}
			if ix.Len() != 0 {
				t.Errorf("Len = %d after a refused Add, want 0", ix.Len())
			}
			// Update refuses the same three, and refusing after the document
			// exists is the case that matters: a rejected update must leave what
			// it could not replace exactly as it was.
			if _, err := ix.Add(fieldDoc("k", "body")); err != nil {
				t.Fatalf("Add: %v", err)
			}
			if _, err := ix.Update(fieldDoc("k", "changed", fields...)); !errors.Is(err, ErrBadField) {
				t.Errorf("Update: got %v, want ErrBadField", err)
			}
			if d, _ := ix.Doc(0); d.Text != "body" {
				t.Errorf("Doc(0).Text = %q after a refused Update, want the original", d.Text)
			}
		})
	}
}

// TestUpdateDropsTheOldFieldsPostings is the failure a second copy of the
// tokenizing rule would have caused. replacePending decides what to drop by
// re-tokenizing the *old* document, and a walk of its Text alone leaves every
// field posting behind — so the document keeps answering a title query for a
// title it no longer has, with the record right and the posting stale.
func TestUpdateDropsTheOldFieldsPostings(t *testing.T) {
	ix := New()
	if _, err := ix.Add(fieldDoc("k", "body", Field{Name: "title", Text: "obsolete"})); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := ix.Update(fieldDoc("k", "body", Field{Name: "title", Text: "current"})); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := ix.Lookup(FieldTerm("title", "obsolete")); got != nil {
		t.Errorf("the replaced title still answers: %v", got)
	}
	if got := ix.Lookup(FieldTerm("title", "current")); len(got) != 1 {
		t.Errorf("the new title does not answer: %v", got)
	}
	// Dropping the field entirely has to drop its terms too.
	if _, err := ix.Update(fieldDoc("k", "body")); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := ix.Lookup(FieldTerm("title", "current")); got != nil {
		t.Errorf("a removed field still answers: %v", got)
	}
	if got := ix.DocLen(0); got != 1 {
		t.Errorf("DocLen = %d after the field was removed, want 1", got)
	}
}

// TestFieldTermIsTheJoinAScorerNeeds. It is exported so a scorer outside this
// module can build the same key Add indexed under; an empty name is Text's own
// space, which is what lets one code path serve both.
func TestFieldTermIsTheJoinAScorerNeeds(t *testing.T) {
	if got := FieldTerm("", "tok"); got != "tok" {
		t.Errorf("FieldTerm(\"\", tok) = %q, want the token unchanged", got)
	}
	if got := FieldTerm("title", "tok"); got != "title\x00tok" {
		t.Errorf("FieldTerm(title, tok) = %q", got)
	}
}

// TestAnOlderSegmentReadsAsHavingNoFields is the debt FORMAT.md section 7.6 says
// every version past 2 owes: a reader that understands both, rather than a
// converter. A document written before fields existed has none, which needs no
// branch beyond the count it does not carry.
func TestAnOlderSegmentReadsAsHavingNoFields(t *testing.T) {
	dir := t.TempDir()
	ix := New()
	for _, d := range []Document{{Key: "a", Text: "alpha"}, {Key: "b", Text: "beta"}} {
		if _, err := ix.Add(d); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if err := ix.Commit(context.Background(), dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	_ = ix.Close()
	downgradeToV3(t, dir)

	old, err := Open(dir)
	if err != nil {
		t.Fatalf("Open a version 3 directory: %v", err)
	}
	defer old.Close() //nolint:errcheck // teardown
	for i, key := range []string{"a", "b"} {
		d, ok := old.Doc(DocID(i))
		if !ok || d.Key != key {
			t.Fatalf("Doc(%d) = %q, %v; want %q, true", i, d.Key, ok, key)
		}
		if len(d.Fields) != 0 {
			t.Errorf("Doc(%d).Fields = %v, want none", i, d.Fields)
		}
	}
}
