// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

// feed runs the CLI with a body on stdin.
func feed(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errs bytes.Buffer
	code = run(args, strings.NewReader(stdin), &out, &errs)
	return code, out.String(), errs.String()
}

// three documents, between them carrying every field a scorer reads.
const threeDocs = `{"key":"rrf","text":"reciprocal rank fusion","vector":[1,0,0],"links":["bm25"],"time":"2026-08-01T00:00:00Z"}
{"key":"bm25","text":"bm25 ranks documents by term frequency","vector":[0.8,0.6,0],"time":"2026-07-29T00:00:00Z"}
{"key":"hnsw","text":"approximate nearest neighbour graphs","vector":[0,1,0],"time":"2026-07-31T00:00:00Z","fields":[{"name":"title","text":"small world"}]}
`

// openIndexed opens what a run of `weft index` left behind. Failing here rather
// than in the caller keeps the assertion that matters at the top of each test.
func openIndexed(t *testing.T, dir string, opts ...engine.Option) *engine.Index {
	t.Helper()
	ix, err := engine.Open(dir, opts...)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix
}

// TestIndexCommitsWhatItRead is the round trip: documents in on stdin, an index
// on disk that a second process can open.
func TestIndexCommitsWhatItRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ix")

	code, stdout, stderr := feed(t, threeDocs, "index", "-data", dir)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "added 3") {
		t.Errorf("stdout does not report three additions:\n%s", stdout)
	}

	ix := openIndexed(t, dir)
	docs, _ := ix.Stats()
	if docs != 3 {
		t.Errorf("Stats says %d documents, want 3", docs)
	}
	if _, ok := ix.Resolve("hnsw"); !ok {
		t.Error(`the committed index has no document keyed "hnsw"`)
	}
}

// TestIndexUpdatesAnExistingKey pins the upsert. A second run carrying a key the
// index already holds must replace that document rather than add a second one
// under the same name — which engine.Add refuses outright with ErrDuplicateKey,
// so getting this wrong is a run that fails rather than one that lies.
func TestIndexUpdatesAnExistingKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ix")
	if code, _, stderr := feed(t, threeDocs, "index", "-data", dir); code != 0 {
		t.Fatalf("first index: exit %d, stderr:\n%s", code, stderr)
	}

	const revised = `{"key":"rrf","text":"reciprocal rank fusion revised","time":"2026-08-02T00:00:00Z"}
`
	code, stdout, stderr := feed(t, revised, "index", "-data", dir)
	if code != 0 {
		t.Fatalf("second index: exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "updated 1") {
		t.Errorf("stdout does not report the update:\n%s", stdout)
	}

	ix := openIndexed(t, dir)
	if docs, _ := ix.Stats(); docs != 3 {
		t.Errorf("Stats says %d documents after an update, want 3", docs)
	}
	if n := ix.PostingCount("revised"); n != 1 {
		t.Errorf(`PostingCount("revised") = %d, want 1 — the new text was not indexed`, n)
	}
}

// TestIndexDeletes covers the other half of a corpus that changes.
func TestIndexDeletes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ix")
	if code, _, stderr := feed(t, threeDocs, "index", "-data", dir); code != 0 {
		t.Fatalf("index: exit %d, stderr:\n%s", code, stderr)
	}

	code, stdout, stderr := feed(t, "", "index", "-data", dir, "-delete", "bm25")
	if code != 0 {
		t.Fatalf("delete: exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "deleted 1") {
		t.Errorf("stdout does not report the deletion:\n%s", stdout)
	}

	ix := openIndexed(t, dir)
	if _, ok := ix.Resolve("bm25"); ok {
		t.Error("the deleted document still resolves")
	}
	if docs, _ := ix.Stats(); docs != 2 {
		t.Errorf("Stats says %d live documents, want 2", docs)
	}
}

// TestIndexNamesTheBadLine is the refusal this command exists to get right. A
// malformed line in the middle of a large file has to say which line, because
// the alternative is a corpus that is silently short by one document.
func TestIndexNamesTheBadLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ix")
	const broken = `{"key":"a","text":"fine"}
{"key":"b","text":"also fine"}
{"key":"c","text":
`
	code, _, stderr := feed(t, broken, "index", "-data", dir)

	if code == 0 {
		t.Fatal("a malformed document was accepted")
	}
	if !strings.Contains(stderr, "line 3") {
		t.Errorf("the error does not name the line that failed:\n%s", stderr)
	}
}

// TestIndexRefusesAnEmptyKey lets engine's own error through rather than
// restating it. ErrEmptyKey is what the library calls this, and a command that
// invented its own wording would leave a caller searching for the wrong string.
func TestIndexRefusesAnEmptyKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ix")

	code, _, stderr := feed(t, `{"key":"","text":"nameless"}`+"\n", "index", "-data", dir)

	if code == 0 {
		t.Fatal("a document with no key was accepted")
	}
	if !strings.Contains(stderr, engine.ErrEmptyKey.Error()) {
		t.Errorf("engine's own error is not in the message:\n%s", stderr)
	}
}

// TestIndexRequiresADirectory. There is no in-memory mode for this command: an
// index built and written nowhere is a process that did nothing.
func TestIndexRequiresADirectory(t *testing.T) {
	code, _, stderr := feed(t, threeDocs, "index")

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "-data") {
		t.Errorf("the message does not name the missing flag:\n%s", stderr)
	}
}

// TestIndexMergesWhenAsked. Merge is not visible in Stats — it rewrites segments
// without changing what the index holds — so the check is that the index still
// opens and still answers afterwards.
func TestIndexMergesWhenAsked(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ix")
	if code, _, stderr := feed(t, threeDocs, "index", "-data", dir); code != 0 {
		t.Fatalf("index: exit %d, stderr:\n%s", code, stderr)
	}

	const more = `{"key":"ivf","text":"inverted file index","time":"2026-07-30T00:00:00Z"}
`
	code, stdout, stderr := feed(t, more, "index", "-data", dir, "-merge")
	if code != 0 {
		t.Fatalf("merge: exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "merged") {
		t.Errorf("stdout does not report the merge:\n%s", stdout)
	}

	ix := openIndexed(t, dir)
	if docs, _ := ix.Stats(); docs != 4 {
		t.Errorf("Stats says %d documents after a merge, want 4", docs)
	}
}

// TestIndexTokenizerIsRecorded pins the replacement point at both ends. An index
// built with one tokenizer and opened with another is refused by engine itself —
// ErrTokenizerMismatch — and this flag is how a command reaches that.
//
// The text is chosen so the two tokenizers disagree about how many tokens it
// holds, not merely about what they are. engine detects the mismatch by
// re-tokenizing one sampled document and comparing its length against the length
// on disk, so "Hello, World" is invisible to it: the default drops the comma and
// lowercases, the whitespace one keeps both, and each still produces two tokens.
// A hyphen is what splits the counts — the default cuts at it and whitespace
// does not.
func TestIndexTokenizerIsRecorded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ix")

	code, _, stderr := feed(t, `{"key":"a","text":"weft-cli tool"}`+"\n",
		"index", "-data", dir, "-tokenizer", "whitespace")
	if code != 0 {
		t.Fatalf("index: exit %d, stderr:\n%s", code, stderr)
	}

	ix := openIndexed(t, dir, engine.WithTokenizer(tokenizers["whitespace"]))
	if n := ix.PostingCount("weft-cli"); n != 1 {
		t.Errorf(`PostingCount("weft-cli") = %d, want 1 — the text was not split on whitespace`, n)
	}

	_, err := engine.Open(dir)
	if !errors.Is(err, engine.ErrTokenizerMismatch) {
		t.Errorf("opening with the default tokenizer returned %v, want ErrTokenizerMismatch", err)
	}
}

// TestIndexRefusesAnUnknownTokenizer. Falling back to the default would build an
// index with a tokenizer the caller did not ask for, and nothing later would say
// so.
func TestIndexRefusesAnUnknownTokenizer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ix")

	code, _, stderr := feed(t, threeDocs, "index", "-data", dir, "-tokenizer", "korean")

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "korean") {
		t.Errorf("the rejected name is not quoted back:\n%s", stderr)
	}
}
