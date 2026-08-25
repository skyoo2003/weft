// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"errors"
	"path/filepath"
	"testing"
	"unicode"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/scorer/text"
)

// The Korean corpus, and the reason each document is in it.
//
// koreanTarget holds the query string with a particle stuck to it —
// "검색엔진을" against a query of "검색엔진" — which is the whole shape of the
// problem. engine.Tokenize cuts only where a rune is neither a letter nor a
// digit, and a Korean particle is letters, so the corpus term and the query term
// are two different strings and BM25 never compares them.
//
// The two decoys are what turns ">0 hits" into "the intended document and
// nothing else". They are Korean, they are the same length class as the target,
// and they share no character bigram with it — so a replaced tokenizer that
// matched anything Korean at all, rather than the target specifically, would
// surface them and fail the assertion. Checked rather than asserted: see
// TestKoreanDecoysShareNoBigramWithTheTarget.
const (
	koreanTarget = "검색엔진을 만들었다"
	koreanDecoy1 = "고양이가 창밖을 바라본다"
	koreanDecoy2 = "오늘 점심으로 국수를 삶는다"

	koreanQuery = "검색엔진"
)

// hangulBigrams is what an adopter plugs into the seam, and it is deliberately
// the smallest thing that answers the query above: engine.Tokenize's own
// splitting, with every token that begins with a Hangul syllable broken into
// character bigrams. "검색엔진을" becomes 검색, 색엔, 엔진, 진을, so the query's
// 검색, 색엔, 엔진 are three terms the corpus holds.
//
// It lives in a test file and in ExampleWithTokenizer, and not in pkg/. Shipping
// a second tokenizer would make the seam a menu — an adopter would have to
// decide which of weft's two to use, rather than plug in the one their language
// needs. Morphological analysis is out of scope and this is not it: a bigram
// index over-matches, which is a quality claim milestone 13 does not make.
//
// ponytail: a token that merely *starts* with a Hangul syllable is bigrammed
// whole, mixed script included. A real analyzer segments by script run. This is
// ten lines in a test, and the seam is what is under test.
func hangulBigrams(s string) []string {
	var out []string
	for _, tok := range engine.Tokenize(s) {
		r := []rune(tok)
		if len(r) < 2 || !unicode.Is(unicode.Hangul, r[0]) {
			out = append(out, tok)
			continue
		}
		for i := 0; i+1 < len(r); i++ {
			out = append(out, string(r[i:i+2]))
		}
	}
	return out
}

// koreanCorpus adds the target and both decoys to ix, in that order.
func koreanCorpus(t *testing.T, ix *engine.Index) {
	t.Helper()
	for _, d := range []engine.Document{
		{Key: "target", Text: koreanTarget},
		{Key: "decoy1", Text: koreanDecoy1},
		{Key: "decoy2", Text: koreanDecoy2},
	} {
		if _, err := ix.Add(d); err != nil {
			t.Fatalf("Add(%q): %v", d.Key, err)
		}
	}
}

// TestKoreanDecoysShareNoBigramWithTheTarget checks the corpus is still shaped
// the way the assertions below rely on.
//
// It is not decoration. A future edit to any of the three strings could quietly
// give a decoy a bigram the target also holds, and the pass line
// "the top hit is the target and no decoy is present" would then be measuring
// the corpus rather than the seam.
func TestKoreanDecoysShareNoBigramWithTheTarget(t *testing.T) {
	target := map[string]bool{}
	for _, b := range hangulBigrams(koreanTarget) {
		target[b] = true
	}
	for _, decoy := range []string{koreanDecoy1, koreanDecoy2} {
		for _, b := range hangulBigrams(decoy) {
			if target[b] {
				t.Errorf("decoy %q shares bigram %q with the target — the corpus no longer isolates it", decoy, b)
			}
		}
	}
	// And the query has to be reachable from the target under the replacement,
	// or the trial would pass for the wrong reason.
	for _, b := range hangulBigrams(koreanQuery) {
		if !target[b] {
			t.Errorf("query bigram %q is not in the target — the trial would prove nothing", b)
		}
	}
}

// TestKoreanQueryFindsNothingWithTheDefaultTokenizer is the first half of the
// milestone's outcome clause, and it is the half that has always been true.
//
// Zero candidates, not "few": the query term and the corpus term differ by a
// particle, so no posting list is even consulted.
func TestKoreanQueryFindsNothingWithTheDefaultTokenizer(t *testing.T) {
	ix := engine.New()
	koreanCorpus(t, ix)

	cands, err := text.New(ix).Candidates(t.Context(), engine.Query{Text: koreanQuery}, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("the default tokenizer found %d candidates for %q; the corpus term is %q and they are different strings",
			len(cands), koreanQuery, "검색엔진을")
	}
}

// TestKoreanQueryFindsTheTargetWithAReplacedTokenizer is the second half, and
// the assertion is stronger than the PRD's ">0 hits".
//
// Two things are checked and the second is what the decoys bought: at least one
// candidate, the highest-ranked one is the target, and no decoy appears at all.
// ">0 hits" would pass for a tokenizer that matched every Korean document.
func TestKoreanQueryFindsTheTargetWithAReplacedTokenizer(t *testing.T) {
	ix := engine.New(engine.WithTokenizer(hangulBigrams))
	koreanCorpus(t, ix)

	cands, err := text.New(ix).Candidates(t.Context(), engine.Query{Text: koreanQuery}, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cands) == 0 {
		t.Fatal("the replaced tokenizer found nothing; the seam is not carrying the query side")
	}
	want, ok := ix.Resolve("target")
	if !ok {
		t.Fatal("the corpus is missing the target document")
	}
	if cands[0].Doc != want {
		t.Errorf("top candidate is %d, want the target %d", cands[0].Doc, want)
	}
	for _, c := range cands {
		d, ok := ix.Doc(c.Doc)
		if !ok {
			t.Fatalf("candidate %d resolves to no document", c.Doc)
		}
		if d.Key != "target" {
			t.Errorf("decoy %q reached the ranking; the trial is measuring the tokenizer's appetite, not the seam", d.Key)
		}
	}
}

// TestTokenizeSeamIsSharedByIndexAndQuery is outcome clause 1 held down
// mechanically: the three index-time call sites and the one query-time call site
// pass through one function value.
//
// It records the strings the tokenizer was handed rather than counting calls,
// because the claim is about *which* text reached the seam. Update on a document
// that has not been committed yet re-tokenizes the old text too — its old terms
// are what say which posting lists to edit and nothing stores them — so a pending
// update is what makes the third index-time call site observable at all.
func TestTokenizeSeamIsSharedByIndexAndQuery(t *testing.T) {
	var saw []string
	tok := func(s string) []string {
		saw = append(saw, s)
		return engine.Tokenize(s)
	}

	ix := engine.New(engine.WithTokenizer(tok))
	if _, err := ix.Add(engine.Document{Key: "k", Text: "first text"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := ix.Update(engine.Document{Key: "k", Text: "second text"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := ix.Tokenize("query text"); len(got) != 2 {
		t.Fatalf("Index.Tokenize(%q) = %v", "query text", got)
	}

	for _, want := range []string{
		"first text",  // Add
		"second text", // Update
		"query text",  // Index.Tokenize, which is what the query side calls
	} {
		if !containsString(saw, want) {
			t.Errorf("the seam never saw %q; call sites reaching it: %q", want, saw)
		}
	}
	// replacePending: the old text, re-tokenized under the same seam. Counted
	// separately because it is the one call site a caller never asks for.
	if got := countString(saw, "first text"); got != 2 {
		t.Errorf("the old text passed the seam %d times, want 2 (Add, then Update re-tokenizing it): %q", got, saw)
	}
}

func containsString(hay []string, want string) bool {
	for _, s := range hay {
		if s == want {
			return true
		}
	}
	return false
}

func countString(hay []string, want string) int {
	n := 0
	for _, s := range hay {
		if s == want {
			n++
		}
	}
	return n
}

// TestZeroValueIndexTokenizesWithTheDefault holds the promise Index's doc
// comment already makes — the zero value is an empty index ready to use — across
// the new field.
//
// A nil tokenizer falls back to engine.Tokenize rather than panicking, which is
// the same shape initMaps keeps for the nil maps beside it.
func TestZeroValueIndexTokenizesWithTheDefault(t *testing.T) {
	var ix engine.Index
	if got, want := ix.Tokenize("Hello World"), engine.Tokenize("Hello World"); !equalTokens(got, want) {
		t.Fatalf("zero-value Index.Tokenize = %q, want the default's %q", got, want)
	}
	// And the write path, because that is where the fallback is load-bearing: Add
	// tokenizes before it takes the lock.
	if _, err := ix.Add(engine.Document{Key: "k", Text: "Hello World"}); err != nil {
		t.Fatalf("Add on a zero-value Index: %v", err)
	}
	if got := ix.DocLen(0); got != 2 {
		t.Fatalf("DocLen(0) = %d, want 2 — the zero value did not tokenize with the default", got)
	}
}

func equalTokens(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTokenizerMismatchIsRefusedAtOpen is the other half of outcome clause 1,
// read from the failure side: a directory committed with one tokenizer and
// opened with another is refused rather than answering zero hits forever.
//
// It is also the answer to Open Question 5 in executable form. The guard reads
// bytes that were already on disk — a live document's stored token count and the
// segment's terms index — so nothing about the format changed to make this
// possible.
func TestTokenizerMismatchIsRefusedAtOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ix")

	ix := engine.New(engine.WithTokenizer(hangulBigrams))
	koreanCorpus(t, ix)
	if err := ix.Commit(t.Context(), dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The default tokenizer against a bigram-indexed directory: 2 tokens
	// recomputed against 7 stored.
	got, err := engine.Open(dir)
	if err == nil {
		got.Close() //nolint:errcheck // the test has already failed
		t.Fatal("Open with the default tokenizer accepted a directory indexed with bigrams")
	}
	if !errors.Is(err, engine.ErrTokenizerMismatch) {
		t.Fatalf("Open error = %v, want ErrTokenizerMismatch", err)
	}

	// The same directory with the tokenizer it was written with opens, which is
	// what keeps the guard from being a refusal of everything.
	same, err := engine.Open(dir, engine.WithTokenizer(hangulBigrams))
	if err != nil {
		t.Fatalf("Open with the indexing tokenizer: %v", err)
	}
	defer same.Close() //nolint:errcheck // test cleanup
	cands, err := text.New(same).Candidates(t.Context(), engine.Query{Text: koreanQuery}, 10)
	if err != nil {
		t.Fatalf("Candidates after reopen: %v", err)
	}
	if len(cands) == 0 {
		t.Fatal("the reopened index answers nothing; the seam does not survive a commit")
	}
}

// TestDefaultTokenizerStillOpensADefaultDirectory is the guard's false-positive
// check at the point that matters most: every directory weft has ever written
// was written with the default tokenizer, and the guard must not refuse one.
//
// The whole-format version of this assertion is restore_test.go and
// formatv2_test.go staying green — those read version 3 and version 4 fixtures
// with nothing converted.
func TestDefaultTokenizerStillOpensADefaultDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ix")
	ix := engine.New()
	koreanCorpus(t, ix)
	if err := ix.Commit(t.Context(), dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer got.Close() //nolint:errcheck // test cleanup
	if got.Len() != 3 {
		t.Fatalf("Len = %d, want 3", got.Len())
	}
}

// TestGuardPassesACorpusWithNoTextToJudge is one of the four ceilings the guard
// publishes, held down so it cannot regress into a panic or a refusal.
//
// A corpus whose documents are all empty has no sample to recompute, and it is
// admitted: such a corpus answers zero hits under every tokenizer, so there is
// nothing for the guard to protect.
func TestGuardPassesACorpusWithNoTextToJudge(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ix")
	ix := engine.New()
	for _, key := range []string{"a", "b"} {
		if _, err := ix.Add(engine.Document{Key: key}); err != nil {
			t.Fatalf("Add(%q): %v", key, err)
		}
	}
	if err := ix.Commit(t.Context(), dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := engine.Open(dir, engine.WithTokenizer(hangulBigrams))
	if err != nil {
		t.Fatalf("Open with a different tokenizer over a textless corpus: %v", err)
	}
	defer got.Close() //nolint:errcheck // test cleanup
}

// TestTokenizeIsStillThePackageDefault keeps engine.Tokenize exported and
// unchanged, and checks that a default-constructed Index routes to it.
//
// internal/eval/bm25_test.go uses the free function as its reference
// implementation and the guard's fallback is it, so a rename here would be a
// rename of the default rather than a tidy-up.
func TestTokenizeIsStillThePackageDefault(t *testing.T) {
	got := engine.Tokenize("Weft's Index, v4!")
	want := []string{"weft", "s", "index", "v4"}
	if !equalTokens(got, want) {
		t.Fatalf("Tokenize = %q, want %q", got, want)
	}
	if got := engine.New().Tokenize("Weft's Index"); !equalTokens(got, []string{"weft", "s", "index"}) {
		t.Fatalf("New().Tokenize = %q, want the default's answer", got)
	}
}
