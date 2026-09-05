// SPDX-License-Identifier: Apache-2.0

package query_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/query"
)

// catalogue is a corpus with a body, a title and a price, so one query string
// can exercise every clause kind against one index.
func catalogue(t *testing.T) *engine.Index {
	t.Helper()
	ix := engine.New()
	for _, d := range []struct {
		key, text, title string
		price            int64
		draft            bool
	}{
		{key: "cheap-covid", text: "airborne transmission of the virus", title: "covid study", price: 100},
		{key: "dear-covid", text: "airborne transmission indoors", title: "covid review", price: 9000},
		{key: "cheap-draft", text: "airborne transmission notes", title: "covid draft", price: 50, draft: true},
		{key: "cheap-other", text: "unrelated airborne matters", title: "influenza study", price: 200},
		{key: "split-phrase", text: "airborne and then transmission", title: "covid notes", price: 120},
	} {
		fields := []engine.Field{
			{Name: "title", Text: d.title},
			{Name: "price", Text: query.EncodeInt(d.price)},
		}
		text := d.text
		if d.draft {
			text += " draft"
		}
		if _, err := ix.Add(engine.Document{Key: d.key, Text: text, Fields: fields}); err != nil {
			t.Fatalf("Add(%q): %v", d.key, err)
		}
	}
	return ix
}

func run(t *testing.T, ix *engine.Index, q string, k int) []string {
	t.Helper()
	p, err := query.Parse(ix, q)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	got, err := engine.Search(context.Background(), engine.Query{}, k, p.Fuse(fusion.Fuse), p.Scorers...)
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	return sorted(keys(t, ix, got))
}

// TestParseSpeaksTheSyntaxItDocuments walks every clause kind the doc comment
// advertises, against one corpus, because a syntax is only a syntax if the whole
// of it works together.
func TestParseSpeaksTheSyntaxItDocuments(t *testing.T) {
	ix := catalogue(t)
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{`virus`, []string{"cheap-covid"}},
		{`title:influenza`, []string{"cheap-other"}},
		{`airborne*`, []string{"cheap-covid", "dear-covid", "cheap-draft", "cheap-other", "split-phrase"}},
		{`title:covid`, []string{"cheap-covid", "dear-covid", "cheap-draft", "split-phrase"}},
		{`"airborne transmission"`, []string{"cheap-covid", "dear-covid", "cheap-draft"}},
		// "covid" is a title term and not a Text one, so an unscoped fuzzy clause
		// finds nothing however generous its budget — the scoping is real.
		{`covd~2`, nil},
		{`title:covd~`, []string{"cheap-covid", "dear-covid", "cheap-draft", "split-phrase"}},
		{`price:[` + query.EncodeInt(0) + ` TO ` + query.EncodeInt(150) + `]`,
			[]string{"cheap-covid", "cheap-draft", "split-phrase"}},
		{`price:[` + query.EncodeInt(1000) + ` TO *]`, []string{"dear-covid"}},
		{`price:[* TO ` + query.EncodeInt(60) + `]`, []string{"cheap-draft"}},
		{``, nil},
		{`   `, nil},
	} {
		t.Run(tc.q, func(t *testing.T) {
			if got := run(t, ix, tc.q, 100); !slices.Equal(got, sorted(tc.want)) {
				t.Errorf("got %v, want %v", got, sorted(tc.want))
			}
		})
	}
}

// TestPlusAndMinusAreIntersectionAndDifference is the half a term list cannot
// express. Without them every clause is a vote and a document matching one of
// them is in the result.
func TestPlusAndMinusAreIntersectionAndDifference(t *testing.T) {
	ix := catalogue(t)
	for _, tc := range []struct {
		q    string
		want []string
	}{
		// No prefix at all: a union, so the unrelated document rides in on the
		// first clause's vote. That is the trap the package exists to close and
		// it has to still be true here.
		{`title:influenza virus`, []string{"cheap-covid", "cheap-other"}},
		{`+title:covid +virus`, []string{"cheap-covid"}},
		{`+airborne* -draft`, []string{"cheap-covid", "dear-covid", "cheap-other", "split-phrase"}},
		{`+title:covid -draft -price:[` + query.EncodeInt(1000) + ` TO *]`,
			[]string{"cheap-covid", "split-phrase"}},
		// A required clause still votes, which is what `+` means and what
		// distinguishes it from a filter.
		{`+"airborne transmission" -draft`, []string{"cheap-covid", "dear-covid"}},
	} {
		t.Run(tc.q, func(t *testing.T) {
			if got := run(t, ix, tc.q, 100); !slices.Equal(got, sorted(tc.want)) {
				t.Errorf("got %v, want %v", got, sorted(tc.want))
			}
		})
	}
}

// TestAPhraseIsNotAWordBag. The whole reason a phrase clause exists is that the
// words are adjacent; a document holding both words apart must not match.
func TestAPhraseIsNotAWordBag(t *testing.T) {
	ix := catalogue(t)
	got := run(t, ix, `"airborne transmission"`, 100)
	if slices.Contains(got, "split-phrase") {
		t.Errorf("got %v; split-phrase holds both words but not adjacently", got)
	}
	if !slices.Contains(got, "cheap-covid") {
		t.Errorf("got %v, want the document that holds the phrase", got)
	}
}

// TestParseRefusesWhatItCannotMean. Every one of these would otherwise be a
// query that returns nothing and looks like an honest miss.
func TestParseRefusesWhatItCannotMean(t *testing.T) {
	ix := catalogue(t)
	for _, q := range []string{
		`"unterminated`,
		`"  "`, // a phrase that tokenizes to nothing
		`price:[a TO`,
		`price:[abc]`,
		`price:[ff TO 00]`,
		`covid~9`,
		`covid~0`,
	} {
		t.Run(q, func(t *testing.T) {
			if _, err := query.Parse(ix, q); err == nil {
				t.Errorf("Parse(%q) returned no error", q)
			}
		})
	}
}

// TestPlanKeepsClauseOrder is what makes a Plan inspectable: the stream indexes
// its Fuse builds refer to positions a reader can count off in the query string.
func TestPlanKeepsClauseOrder(t *testing.T) {
	ix := catalogue(t)
	p, err := query.Parse(ix, `+title:covid -draft price:[00 TO ff] covid~2 "airborne transmission"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"glob:title", "glob", "range:price", "fuzzy", "phrase"}
	got := make([]string, 0, len(p.Scorers))
	for _, s := range p.Scorers {
		got = append(got, s.Name())
	}
	if !slices.Equal(got, want) {
		t.Errorf("scorers are %v, want %v in clause order", got, want)
	}
}

// TestPlanFuseLeavesTheRankingToTheCaller. Parse decides which documents are
// eligible and never how the eligible ones are ranked — so a plan fused with a
// weighting has to answer the same set.
func TestPlanFuseLeavesTheRankingToTheCaller(t *testing.T) {
	ix := catalogue(t)
	p, err := query.Parse(ix, `+airborne* -draft`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plain, err := engine.Search(context.Background(), engine.Query{}, 100, p.Fuse(fusion.Fuse), p.Scorers...)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// MustNot removes its stream before fusing, so the weighting has one
	// position to fill rather than two.
	weighted, err := engine.Search(context.Background(), engine.Query{}, 100,
		p.Fuse(fusion.FuseWeighted(1)), p.Scorers...)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !slices.Equal(sorted(keys(t, ix, plain)), sorted(keys(t, ix, weighted))) {
		t.Errorf("the eligible set changed with the fuser: %v against %v",
			sorted(keys(t, ix, plain)), sorted(keys(t, ix, weighted)))
	}
}

// TestAnEmptyPlanIsUsable, so a caller building a query from user input does not
// need a branch for the empty box.
func TestAnEmptyPlanIsUsable(t *testing.T) {
	ix := catalogue(t)
	p, err := query.Parse(ix, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p.Scorers) != 0 {
		t.Errorf("an empty query planned %d scorers", len(p.Scorers))
	}
	got, err := engine.Search(context.Background(), engine.Query{}, 10, p.Fuse(fusion.Fuse), p.Scorers...)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an empty query returned %v", keys(t, ix, got))
	}
}

// TestAClauseWithAColonThatIsNotAField. A field is only a field when both sides
// are non-empty, so a bare colon stays part of the term — where the default
// tokenizer cuts it, which is the honest outcome.
func TestAClauseWithAColonThatIsNotAField(t *testing.T) {
	ix := catalogue(t)
	for _, q := range []string{`:virus`, `virus:`} {
		if _, err := query.Parse(ix, q); err != nil {
			t.Errorf("Parse(%q): %v", q, err)
		}
	}
	// And a colon inside a quoted phrase is just a character.
	if _, err := query.Parse(ix, `"title: covid"`); err != nil {
		t.Errorf("Parse of a phrase holding a colon: %v", err)
	}
}

// TestSplitKeepsAQuotedRunWhole, which is the one place whitespace does not
// separate clauses.
func TestSplitKeepsAQuotedRunWhole(t *testing.T) {
	ix := catalogue(t)
	p, err := query.Parse(ix, `  +title:covid   "airborne transmission"   -draft  `)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p.Scorers) != 3 {
		names := make([]string, 0, len(p.Scorers))
		for _, s := range p.Scorers {
			names = append(names, s.Name())
		}
		t.Errorf("planned %d scorers (%s), want 3", len(p.Scorers), strings.Join(names, ", "))
	}
}
