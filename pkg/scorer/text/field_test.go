// SPDX-License-Identifier: Apache-2.0

package text

import (
	"context"
	"strings"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

func fieldIndex(t *testing.T, docs ...engine.Document) *engine.Index {
	t.Helper()
	ix := engine.New()
	for _, d := range docs {
		if _, err := ix.Add(d); err != nil {
			t.Fatalf("Add(%q): %v", d.Key, err)
		}
	}
	return ix
}

func rankedKeys(t *testing.T, ix *engine.Index, s engine.Scorer, text string, k int) []string {
	t.Helper()
	cands, err := s.Candidates(context.Background(), engine.Query{Text: text}, k)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
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

// TestAFieldScorerSeesOnlyItsField is the point of the whole format change: a
// query scoped to the title does not match the same word in the body, and the
// plain scorer does not match the title.
func TestAFieldScorerSeesOnlyItsField(t *testing.T) {
	ix := fieldIndex(t,
		engine.Document{Key: "titled", Text: "unrelated body",
			Fields: []engine.Field{{Name: "title", Text: "transmission"}}},
		engine.Document{Key: "bodied", Text: "transmission in the body"},
	)

	if got := rankedKeys(t, ix, NewField(ix, "title"), "transmission", 10); len(got) != 1 || got[0] != "titled" {
		t.Errorf("the title scorer ranked %v, want [titled]", got)
	}
	if got := rankedKeys(t, ix, New(ix), "transmission", 10); len(got) != 1 || got[0] != "bodied" {
		t.Errorf("the text scorer ranked %v, want [bodied]", got)
	}
	if got := rankedKeys(t, ix, NewField(ix, "author"), "transmission", 10); len(got) != 0 {
		t.Errorf("a field no document has ranked %v, want nothing", got)
	}
}

// TestAFieldScorerDoesNotRankByDocumentBrevity is what B=0 buys, and it is the
// one decision in NewField worth a test of its own.
//
// A document's stored length counts every field's tokens plus Text's, so there
// is no per-field length to normalize against. Normalizing a title match by the
// whole document's length would divide a five-token title by a five-thousand
// token document — every title match in a long document scores near zero, and
// the ranking becomes a ranking of how short the body is. Two identical titles
// have to rank identically however different their bodies are.
func TestAFieldScorerDoesNotRankByDocumentBrevity(t *testing.T) {
	title := []engine.Field{{Name: "title", Text: "transmission dynamics"}}
	ix := fieldIndex(t,
		engine.Document{Key: "short", Text: "brief", Fields: title},
		engine.Document{Key: "long", Text: strings.Repeat("filler ", 500), Fields: title},
	)

	cands, err := NewField(ix, "title").Candidates(context.Background(),
		engine.Query{Text: "transmission dynamics"}, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want both documents", len(cands))
	}
	if cands[0].Score != cands[1].Score {
		t.Errorf("identical titles scored %v and %v; a field scorer must not rank by "+
			"the length of a body it never read", cands[0].Score, cands[1].Score)
	}

	// And the plain scorer *does* normalize, which is what makes the difference
	// above a decision rather than an accident.
	plain := fieldIndex(t,
		engine.Document{Key: "short", Text: "transmission dynamics"},
		engine.Document{Key: "long", Text: "transmission dynamics " + strings.Repeat("filler ", 500)},
	)
	pc, err := New(plain).Candidates(context.Background(), engine.Query{Text: "transmission dynamics"}, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(pc) != 2 || pc[0].Score == pc[1].Score {
		t.Errorf("the text scorer scored %v; it normalizes by length and these two differ in it", pc)
	}
}

// TestTwoFieldScorersAreTwoStreams. Searching two fields is not a feature of one
// scorer, it is two scorers and fusion between them — which is the milestone 1
// claim holding one level down.
func TestTwoFieldScorersAreTwoStreams(t *testing.T) {
	ix := fieldIndex(t,
		engine.Document{Key: "both", Text: "body", Fields: []engine.Field{
			{Name: "title", Text: "kim"}, {Name: "author", Text: "kim"}}},
		engine.Document{Key: "author-only", Text: "body", Fields: []engine.Field{
			{Name: "author", Text: "kim"}}},
	)
	title, author := NewField(ix, "title"), NewField(ix, "author")

	if got := rankedKeys(t, ix, title, "kim", 10); len(got) != 1 || got[0] != "both" {
		t.Errorf("title scorer ranked %v, want [both]", got)
	}
	if got := rankedKeys(t, ix, author, "kim", 10); len(got) != 2 {
		t.Errorf("author scorer ranked %v, want both documents", got)
	}
	if title.Name() != "text:title" || author.Name() != "text:author" {
		t.Errorf("names are %q and %q; a caller fusing several has to tell the streams apart",
			title.Name(), author.Name())
	}
	if New(ix).Name() != "text" {
		t.Errorf("the plain scorer is named %q, want text — milestone 4's published arms use it", New(ix).Name())
	}
}

// TestAnEmptyFieldNameIsThePlainScorer keeps the branch out of a caller building
// scorers from configuration.
func TestAnEmptyFieldNameIsThePlainScorer(t *testing.T) {
	ix := fieldIndex(t, engine.Document{Key: "a", Text: "transmission"})
	if got := rankedKeys(t, ix, NewField(ix, ""), "transmission", 10); len(got) != 1 {
		t.Errorf("NewField with no name ranked %v, want the document its Text matches", got)
	}
}
