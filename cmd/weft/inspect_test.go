// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// TestInspectStats prints Len beside Stats rather than instead of it. Since
// deletion exists Len is one past the highest DocID and counts the tombstones
// with it, so an index that has been deleted from is larger by Len than it is by
// Stats — and a reader who has only ever seen one of the two numbers cannot know
// that.
func TestInspectStats(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "inspect", "-data", dir, "-stats")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"3 documents", "average length", "id space"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the stats output has no %q:\n%s", want, stdout)
		}
	}
}

// TestInspectStatsIsTheDefault. A subcommand that does nothing when given only
// the flag it cannot work without is a subcommand that looks broken.
func TestInspectStatsIsTheDefault(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "inspect", "-data", dir)

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "3 documents") {
		t.Errorf("no stats printed:\n%s", stdout)
	}
}

// TestInspectDocument covers the five methods that answer "what is stored under
// this key": Resolve, Doc, DocLen, Vector and Neighbors.
func TestInspectDocument(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "inspect", "-data", dir, "-doc", "rrf")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"rrf", "reciprocal rank fusion", "tokens", "vector", "bm25"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the document report has no %q:\n%s", want, stdout)
		}
	}
}

// TestInspectUnknownDocumentIsNotAnEmptyReport. Printing a document with no
// fields for a key that is not there is the failure this repository refuses
// everywhere: an answer indistinguishable from an answer about nothing.
func TestInspectUnknownDocumentIsNotAnEmptyReport(t *testing.T) {
	dir := indexed(t)

	code, _, stderr := feed(t, "", "inspect", "-data", dir, "-doc", "nosuch")

	if code == 0 {
		t.Fatal("a key that is not in the index was reported on as though it were")
	}
	if !strings.Contains(stderr, "nosuch") {
		t.Errorf("the missing key is not named:\n%s", stderr)
	}
}

// TestInspectTerm reaches Lookup and PostingCount, which are how a caller finds
// out whether a query returned nothing because the term is absent or because the
// ranking put its documents below k.
func TestInspectTerm(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "inspect", "-data", dir, "-term", "fusion")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "1 posting") {
		t.Errorf("the posting count is not reported:\n%s", stdout)
	}
	if !strings.Contains(stdout, "rrf") {
		t.Errorf("the document holding the term is not named:\n%s", stdout)
	}
}

// TestInspectTermThatIsNotThere. Zero postings is a real answer, and a different
// one from a term that could not be looked up at all.
func TestInspectTermThatIsNotThere(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "inspect", "-data", dir, "-term", "absent")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "0 posting") {
		t.Errorf("an absent term should be reported as zero postings:\n%s", stdout)
	}
}

func TestInspectTerms(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "inspect", "-data", dir, "-terms", "ran")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "rank") || !strings.Contains(stdout, "ranks") {
		t.Errorf("the prefix walk missed a term:\n%s", stdout)
	}
}

// TestInspectTermsInAField is engine.FieldTerm reaching a command. A field's
// terms live in the same posting space under a name, and spelling that name is
// the one thing a caller cannot do from outside.
func TestInspectTermsInAField(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "inspect", "-data", dir, "-terms", "", "-field", "title")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"small", "world"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the field's terms are missing %q:\n%s", want, stdout)
		}
	}
	// The field-scoped walk must not answer with Document.Text's terms, or a
	// caller cannot tell the two spaces apart.
	if strings.Contains(stdout, "reciprocal") {
		t.Errorf("a field-scoped walk returned Document.Text's terms:\n%s", stdout)
	}
}

// TestInspectPostings walks the block cursor, which is the reader the scorers
// use and the only one that shows postings arriving a block at a time.
func TestInspectPostings(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "inspect", "-data", dir, "-postings", "fusion")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "block") {
		t.Errorf("the block walk reported no blocks:\n%s", stdout)
	}
	if !strings.Contains(stdout, "rrf") {
		t.Errorf("the document in the postings is not named:\n%s", stdout)
	}
}

func TestInspectNearest(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "inspect", "-data", dir, "-nearest", "0,1,0")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "hnsw") {
		t.Errorf("the nearest document by vector is not first:\n%s", stdout)
	}
}

// TestInspectGraph reaches the Adjacency side of pkg/scorer/graph — all of it,
// since Len, Edges, Degree, In and Out are that type's entire surface and no
// command could call any of them.
func TestInspectGraph(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "inspect", "-data", dir, "-graph")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"node", "edge"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the adjacency report has no %q:\n%s", want, stdout)
		}
	}
}

func TestInspectGraphAroundOneDocument(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "inspect", "-data", dir, "-graph", "-doc", "rrf")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"degree", "out", "in"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the per-document adjacency report has no %q:\n%s", want, stdout)
		}
	}
	// rrf links to bm25, so bm25 is what rrf has going out.
	if !strings.Contains(stdout, "bm25") {
		t.Errorf("the neighbour is not named:\n%s", stdout)
	}
}

// TestAnalyzeWithoutAnIndex reaches engine.Tokenize, the package-level function.
// It is the default tokenizer with no index behind it, which is the only way to
// answer "what would this text become" before deciding what to build.
func TestAnalyzeWithoutAnIndex(t *testing.T) {
	code, stdout, stderr := feed(t, "", "inspect", "-analyze", "Ranking, Fusion")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "ranking") || !strings.Contains(stdout, "fusion") {
		t.Errorf("the default tokenizer's output is not there:\n%s", stdout)
	}
	if strings.Contains(stdout, "Ranking,") {
		t.Errorf("the text was not tokenized at all:\n%s", stdout)
	}
}

func TestAnalyzeWithAnotherTokenizer(t *testing.T) {
	code, stdout, stderr := feed(t, "", "inspect", "-analyze", "검색엔진", "-tokenizer", "bigram")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "검색") || !strings.Contains(stdout, "색엔") {
		t.Errorf("the bigram tokenizer's overlapping pairs are not there:\n%s", stdout)
	}
}

// TestAnalyzeThroughTheIndex uses Index.Tokenize instead, which is the tokenizer
// the index was committed with rather than the default. The two differ exactly
// when it matters most.
func TestAnalyzeThroughTheIndex(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "inspect", "-data", dir, "-analyze", "Ranking, Fusion")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "ranking") {
		t.Errorf("nothing was tokenized:\n%s", stdout)
	}
}

// TestInspectNeedsAnIndexForEverythingElse. -analyze is the one question here
// that has an answer without one.
func TestInspectNeedsAnIndexForEverythingElse(t *testing.T) {
	code, _, stderr := feed(t, "", "inspect", "-term", "fusion")

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "-data") {
		t.Errorf("the message does not name the missing flag:\n%s", stderr)
	}
}
