// SPDX-License-Identifier: Apache-2.0

// Package query_test is external on purpose. pkg/query imports no scorer — the
// same claim pkg/fusion makes and `make deps` checks — and a test inside the
// package would put scorer/text on its import graph.
package query_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/query"
	"github.com/skyoo2003/weft/pkg/scorer/text"
)

// corpus builds an index from key/text pairs, in order, so a DocID is the
// position of its pair.
func corpus(t *testing.T, pairs ...string) *engine.Index {
	t.Helper()
	ix := engine.New()
	for i := 0; i < len(pairs); i += 2 {
		if _, err := ix.Add(engine.Document{Key: pairs[i], Text: pairs[i+1]}); err != nil {
			t.Fatalf("Add(%q): %v", pairs[i], err)
		}
	}
	return ix
}

func keys(t *testing.T, ix *engine.Index, cands []engine.Candidate) []string {
	t.Helper()
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		d, ok := ix.Doc(c.Doc)
		if !ok {
			t.Fatalf("Doc(%d) absent", c.Doc)
		}
		out = append(out, d.Key)
	}
	return out
}

func candidates(t *testing.T, s engine.Scorer, q engine.Query, k int) []engine.Candidate {
	t.Helper()
	cands, err := s.Candidates(context.Background(), q, k)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	return cands
}

func sorted(names []string) []string {
	out := slices.Clone(names)
	slices.Sort(out)
	return out
}

// TestUnfilteredFusionLetsRefusedDocumentsBack is the trap, demonstrated before
// the fix is shown to close it. It is the reason this package exists, and
// docs/ADOPTION.md section 8 is a trial subject losing time to it.
//
// Without this half, a reader has the assertion that Must works and no evidence
// that anything was wrong with plain fusion — which is the half that decides
// whether to reach for this package at all.
func TestUnfilteredFusionLetsRefusedDocumentsBack(t *testing.T) {
	ix := corpus(t,
		"hit", "alpha beta gamma",
		"miss", "alpha delta epsilon",
	)
	txt := text.New(ix)
	only := query.Glob(ix, "", "beta") // a restriction admitting only "hit"

	got, err := engine.Search(context.Background(), engine.Query{Text: "alpha"}, 10,
		engine.Fuser(fusion.Fuse), txt, only)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if names := keys(t, ix, got); !slices.Contains(names, "miss") {
		t.Fatalf("plain fusion returned %v; the document the restriction refused is "+
			"supposed to come back on the text scorer's vote, which is the trap", names)
	}

	filtered, err := engine.Search(context.Background(), engine.Query{Text: "alpha"}, 10,
		query.Must(fusion.Fuse, 1), txt, only)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if names := keys(t, ix, filtered); !slices.Equal(names, []string{"hit"}) {
		t.Errorf("Must fusion returned %v, want [hit]", names)
	}
}

func TestMustIntersectsEveryNamedStream(t *testing.T) {
	ix := corpus(t,
		"all", "red green blue",
		"two", "red green",
		"one", "red",
	)
	red, green, blue := query.Glob(ix, "", "red"), query.Glob(ix, "", "green"), query.Glob(ix, "", "blue")

	for _, tc := range []struct {
		name    string
		streams []int
		want    []string
	}{
		{"no constraint is fuse unchanged", nil, []string{"all", "two", "one"}},
		{"one", []int{2}, []string{"all"}},
		{"two", []int{1, 2}, []string{"all"}},
		{"the widest", []int{0}, []string{"all", "two", "one"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fuse := query.Must(fusion.Fuse, tc.streams...)
			got, err := engine.Search(context.Background(), engine.Query{}, 10, fuse, red, green, blue)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if names := sorted(keys(t, ix, got)); !slices.Equal(names, sorted(tc.want)) {
				t.Errorf("got %v, want %v", names, sorted(tc.want))
			}
		})
	}
}

func TestMustNotDropsAndDoesNotVote(t *testing.T) {
	ix := corpus(t,
		"keep", "alpha",
		"drop", "alpha draft",
	)
	fuse := query.MustNot(fusion.Fuse, 1)
	got, err := engine.Search(context.Background(), engine.Query{}, 10, fuse,
		query.Glob(ix, "", "alpha"), query.Glob(ix, "", "draft"))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if names := keys(t, ix, got); !slices.Equal(names, []string{"keep"}) {
		t.Errorf("got %v, want [keep]", names)
	}
}

// TestAConstraintThatCannotBeEvaluatedIsNotSatisfied pins the direction of the
// one wrong answer available here. A stream index past the end of the list is a
// mis-wired call; answering with nothing gets noticed, answering as though the
// constraint were absent does not.
func TestAConstraintThatCannotBeEvaluatedIsNotSatisfied(t *testing.T) {
	ix := corpus(t, "a", "alpha", "b", "beta")
	all := query.Glob(ix, "", "*")

	for name, fuse := range map[string]engine.Fuser{
		"Must past the end":    query.Must(fusion.Fuse, 9),
		"Must negative":        query.Must(fusion.Fuse, -1),
		"MustNot past the end": query.MustNot(fusion.Fuse, 9),
	} {
		got, err := engine.Search(context.Background(), engine.Query{}, 10, fuse, all)
		if err != nil {
			t.Fatalf("%s: Search: %v", name, err)
		}
		if len(got) != 0 {
			t.Errorf("%s returned %v, want nothing", name, keys(t, ix, got))
		}
	}
}

func TestFiltersCompose(t *testing.T) {
	ix := corpus(t,
		"want", "alpha beta",
		"nodraft", "alpha beta draft",
		"nobeta", "alpha",
	)
	fuse := query.Must(query.MustNot(fusion.Fuse, 2), 1)
	got, err := engine.Search(context.Background(), engine.Query{}, 10, fuse,
		query.Glob(ix, "", "alpha"), query.Glob(ix, "", "beta"), query.Glob(ix, "", "draft"))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if names := keys(t, ix, got); !slices.Equal(names, []string{"want"}) {
		t.Errorf("got %v, want [want]", names)
	}
}

func TestGlobMatchesThePatternsItAdvertises(t *testing.T) {
	ix := corpus(t,
		"a", "golang",
		"b", "gopher",
		"c", "running",
		"d", "organise",
		"e", "organize",
		"f", "rust",
	)
	for _, tc := range []struct {
		pattern string
		want    []string
	}{
		{"go*", []string{"a", "b"}},
		{"*ing", []string{"c"}},
		{"organi[sz]e", []string{"d", "e"}},
		{"gop?er", []string{"b"}},
		{"rust", []string{"f"}},
		{"nothing*", nil},
		{"*", []string{"a", "b", "c", "d", "e", "f"}},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			names := sorted(keys(t, ix, candidates(t, query.Glob(ix, "", tc.pattern), engine.Query{}, 10)))
			if !slices.Equal(names, sorted(tc.want)) {
				t.Errorf("Glob(%q) = %v, want %v", tc.pattern, names, sorted(tc.want))
			}
		})
	}
}

// TestGlobReportsAMalformedPattern. A typo in a character class must not read as
// a query that legitimately found nothing — those need different responses from
// whoever wrote it.
func TestGlobReportsAMalformedPattern(t *testing.T) {
	ix := corpus(t, "a", "alpha")
	_, err := query.Glob(ix, "", "[unterminated").Candidates(context.Background(), engine.Query{}, 10)
	if err == nil {
		t.Error("a malformed pattern returned no error")
	}
}

// TestGlobDoesNotTruncateToK is what makes it usable as a restriction. A
// restriction cut to k excludes every document below its own cut, which is a
// wrong answer and not a narrow one.
func TestGlobDoesNotTruncateToK(t *testing.T) {
	pairs := make([]string, 0, 40)
	for i := range 20 {
		pairs = append(pairs, string(rune('a'+i)), "shared")
	}
	ix := corpus(t, pairs...)

	if got := candidates(t, query.Glob(ix, "", "shared"), engine.Query{}, 3); len(got) != 20 {
		t.Errorf("Glob returned %d candidates for k=3, want all 20", len(got))
	}
	if got := candidates(t, query.Glob(ix, "", "shared"), engine.Query{}, 0); len(got) != 0 {
		t.Errorf("Glob returned %d candidates for k=0, want none", len(got))
	}
}

// TestGlobRanksByIDF is the whole of what its crude score owes: an ordering that
// is not degenerate. A term in one document has to outrank a term in all of
// them, or the stream contributes nothing a rank fusion can use.
func TestGlobRanksByIDF(t *testing.T) {
	ix := corpus(t,
		"rare", "xcommon xrarest",
		"common1", "xcommon",
		"common2", "xcommon",
		"common3", "xcommon",
	)
	names := keys(t, ix, candidates(t, query.Glob(ix, "", "x*"), engine.Query{}, 10))
	if len(names) == 0 || names[0] != "rare" {
		t.Errorf("Glob ranked %v; the document holding the rare term has to lead", names)
	}
}

func TestPhraseKeepsOnlyConsecutiveRuns(t *testing.T) {
	ix := corpus(t,
		"exact", "the quick brown fox",
		"split", "the quick red brown fox",
		"reversed", "brown quick",
		"partial", "quick only",
	)
	txt := text.New(ix)
	for _, tc := range []struct {
		phrase string
		want   []string
	}{
		{"quick brown", []string{"exact"}},
		{"the quick", []string{"exact", "split"}},
		{"brown quick", []string{"reversed"}},
		{"quick", []string{"exact", "split", "reversed", "partial"}},
		{"not present", nil},
	} {
		t.Run(tc.phrase, func(t *testing.T) {
			p := query.Phrase(ix, txt, tc.phrase)
			names := sorted(keys(t, ix, candidates(t, p, engine.Query{Text: "quick brown the"}, 10)))
			if !slices.Equal(names, sorted(tc.want)) {
				t.Errorf("Phrase(%q) = %v, want %v", tc.phrase, names, sorted(tc.want))
			}
		})
	}
}

// TestPhraseKeepsInnersScores. The phrase is a constraint, not a signal, so a
// caller wrapping the text scorer is still ranking by BM25 — and that is the
// property that makes wrapping worth doing rather than replacing.
func TestPhraseKeepsInnersScores(t *testing.T) {
	ix := corpus(t,
		"strong", "quick brown quick brown",
		"weak", "quick brown "+strings.Repeat("filler ", 50),
	)
	txt := text.New(ix)
	inner := candidates(t, txt, engine.Query{Text: "quick brown"}, 10)
	got := candidates(t, query.Phrase(ix, txt, "quick brown"), engine.Query{Text: "quick brown"}, 10)

	if len(got) != 2 {
		t.Fatalf("Phrase returned %v, want both documents", keys(t, ix, got))
	}
	byDoc := map[engine.DocID]float64{}
	for _, c := range inner {
		byDoc[c.Doc] = c.Score
	}
	for _, c := range got {
		if byDoc[c.Doc] != c.Score {
			t.Errorf("document %d scored %v, want the inner scorer's %v", c.Doc, c.Score, byDoc[c.Doc])
		}
	}
	if keys(t, ix, got)[0] != "strong" {
		t.Errorf("Phrase reordered the inner ranking: %v", keys(t, ix, got))
	}
}

// TestPhraseWithNoConstraintIsTheInnerScorer. An empty phrase is a caller asking
// to filter on nothing, and the honest answer is everything rather than nothing.
func TestPhraseWithNoConstraintIsTheInnerScorer(t *testing.T) {
	ix := corpus(t, "a", "alpha beta", "b", "alpha")
	got := candidates(t, query.Phrase(ix, text.New(ix), "  !!  "), engine.Query{Text: "alpha"}, 10)
	if len(got) != 2 {
		t.Errorf("got %v, want both documents", keys(t, ix, got))
	}
}

func TestPhraseRefusesWithoutAnInnerScorer(t *testing.T) {
	ix := corpus(t, "a", "alpha")
	_, err := query.Phrase(ix, nil, "alpha").Candidates(context.Background(), engine.Query{}, 10)
	if err == nil {
		t.Error("Phrase with a nil inner scorer returned no error")
	}
}

func TestScorersAreCancellable(t *testing.T) {
	pairs := make([]string, 0, 4096)
	for i := range 2048 {
		pairs = append(pairs, string(rune('a'+i%26))+string(rune('a'+i/26)), "shared token here")
	}
	ix := corpus(t, pairs...)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for name, s := range map[string]engine.Scorer{
		"glob":   query.Glob(ix, "", "*"),
		"phrase": query.Phrase(ix, query.Glob(ix, "", "*"), "shared token"),
	} {
		if _, err := s.Candidates(ctx, engine.Query{Text: "shared"}, 10); !errors.Is(err, context.Canceled) {
			t.Errorf("%s on a cancelled context returned %v, want context.Canceled", name, err)
		}
	}
}
