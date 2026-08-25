// SPDX-License-Identifier: Apache-2.0

// This file is milestone 12's question, asked in code rather than in prose:
// can an outsider express a query-time input and a text-side constraint without
// modifying weft?
//
// It is written from engine_test, the outsider's position, for the reason
// adoption_test.go next door gives: a separate package can reach nothing this
// repository does not export, so a test that compiles here is a test an adopter
// could have written. Both shapes below come from the two blind trials recorded
// in docs/ADOPTION.md section 7, and neither is the subject's code copied over —
// each is the smallest arrangement that still fails when the property fails.
package engine_test

import (
	"context"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/scorer/graph"
	"github.com/skyoo2003/weft/pkg/scorer/text"
)

// expressiveCorpus is shaped around the two traps the trials had to
// discriminate. "tools" holds both query terms without holding the phrase, so
// BM25 ranks it and an exact-phrase constraint must not; "cafe" shares no term
// with the query at all, so it can only enter a ranking through a scorer that
// was told about it out of band.
func expressiveCorpus(t *testing.T) *engine.Index {
	t.Helper()
	ix := engine.New()
	for _, d := range []engine.Document{
		{Key: "survey", Text: "machine learning models rank search results", Links: []string{"tools"}},
		{Key: "tools", Text: "learning about machine tools and workshop practice", Links: []string{"ranking"}},
		{Key: "ranking", Text: "ranking models for search results"},
		{Key: "cafe", Text: "coffee and pastries by the harbour"},
	} {
		if _, err := ix.Add(d); err != nil {
			t.Fatalf("Add(%q): %v", d.Key, err)
		}
	}
	return ix
}

func resolve(t *testing.T, ix *engine.Index, key string) engine.DocID {
	t.Helper()
	id, ok := ix.Resolve(key)
	if !ok {
		t.Fatalf("corpus is missing document %q", key)
	}
	return id
}

// ---------------------------------------------------------------------------
// Shape (a): one query-time value, two external scorers, a store built once.
// ---------------------------------------------------------------------------

// topicStore is the corpus-sized side store both external scorers below read.
// It is the half of an outsider's input that does not change per query, and the
// trial's task made it expensive on purpose: it must be built once, not once per
// search.
type topicStore struct {
	topic map[engine.DocID]string
	byKey map[string]string
	built int // proof the construction did not move into the query path
}

func newTopicStore(ix *engine.Index, byKey map[string]string) *topicStore {
	s := &topicStore{topic: make(map[engine.DocID]string, len(byKey)), byKey: byKey}
	s.built++
	for key, topic := range byKey {
		if id, ok := ix.Resolve(key); ok {
			s.topic[id] = topic
		}
	}
	return s
}

// affinity and promoted are two different external scorers that need the *same*
// per-query value: the key of the document the searcher is currently looking at.
// That is the arrangement engine.Query's doc comment warns about when it says
// two scorers sharing one field is how one of them silently stops working.
//
// The value being a document Key is what makes the collision real rather than
// hypothetical. It is the obvious thing for a "more like this one" signal to
// carry, and it is exactly what Query.Seeds already means to the graph scorer.
type affinity struct {
	store *topicStore
	pivot string
}

func (a *affinity) Name() string { return "affinity" }

func (a *affinity) Candidates(_ context.Context, q engine.Query, k int) ([]engine.Candidate, error) {
	return sameTopic(a.store, a.pivot, q, k, 1)
}

type promoted struct {
	store *topicStore
	pivot string
}

func (p *promoted) Name() string { return "promoted" }

func (p *promoted) Candidates(_ context.Context, q engine.Query, k int) ([]engine.Candidate, error) {
	return sameTopic(p.store, p.pivot, q, k, 2)
}

// sameTopic ranks every document sharing the pivot document's topic. The two
// scorers differ only in the scale they score on, which is the point: fusion
// reads rank and never Candidate.Score, so two external signals may disagree
// about units without either having to know the other exists.
func sameTopic(store *topicStore, pivot string, q engine.Query, k int, score float64) ([]engine.Candidate, error) {
	if pivot == "" && len(q.Seeds) > 0 {
		pivot = q.Seeds[0]
	}
	want, ok := store.byKey[pivot]
	if !ok {
		return nil, nil
	}
	cands := make([]engine.Candidate, 0, len(store.topic))
	for id, topic := range store.topic {
		if topic == want {
			cands = append(cands, engine.Candidate{Doc: id, Score: score})
		}
	}
	return engine.TopK(cands, k), nil
}

// TestOneQueryTimeValueReachesTwoExternalScorers asks the milestone 12 question
// for the Query side: one value, two external scorers, and an in-tree scorer
// that must not notice.
//
// The third scorer is graph, and it is here as the victim rather than as a
// signal. graph reads Query.Seeds, so if an outsider routes its own per-query
// input through Seeds the traversal silently starts somewhere else — which is
// the failure engine.Query's doc comment describes and which nothing in this
// tree had a test for.
func TestOneQueryTimeValueReachesTwoExternalScorers(t *testing.T) {
	ix := expressiveCorpus(t)
	txt := text.New(ix)
	// The topics are the outsider's own data, so they are assigned by fiat here.
	// What matters is that "cafe" shares the pivot's topic while sharing no term
	// with the query and no link with anything: text and graph are both blind to
	// it, so it can only reach the ranking through the two external scorers.
	store := newTopicStore(ix, map[string]string{
		"survey": "search", "tools": "workshop", "ranking": "workshop", "cafe": "search",
	})

	q := engine.Query{Text: "machine learning"}
	cafe := resolve(t, ix, "cafe")

	// The document the searcher is looking at, and the one value both external
	// scorers need.
	const pivot = "survey"

	// What the graph scorer does when no outsider has touched Seeds. Every
	// arrangement below has to leave this stream exactly as it is.
	wantGraph, err := graph.New(ix, txt).Candidates(t.Context(), q, 4)
	if err != nil {
		t.Fatalf("graph baseline: %v", err)
	}

	// The arrangement an outsider reaches for first: put the per-query value in
	// the one Query field that takes caller-supplied document keys.
	shared := q
	shared.Seeds = []string{pivot}

	got, err := engine.Search(t.Context(), shared, 4, fusion.Fuse,
		txt, graph.New(ix, txt), &affinity{store: store}, &promoted{store: store})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !contains(got, cafe) {
		t.Fatalf("the two external scorers did not reach the ranking: %+v", got)
	}

	gotGraph, err := graph.New(ix, txt).Candidates(t.Context(), shared, 4)
	if err != nil {
		t.Fatalf("graph under the shared arrangement: %v", err)
	}
	if len(gotGraph) != len(wantGraph) {
		t.Fatalf("routing an external scorer's input through Query.Seeds moved the graph stream: got %+v, want %+v", gotGraph, wantGraph)
	}
	for i := range gotGraph {
		if gotGraph[i].Doc != wantGraph[i].Doc {
			t.Fatalf("routing an external scorer's input through Query.Seeds moved the graph stream: got %+v, want %+v", gotGraph, wantGraph)
		}
	}

	if store.built != 1 {
		t.Fatalf("the corpus-sized store was built %d times, not once", store.built)
	}
}

// ---------------------------------------------------------------------------
// Shape (b): a text-side constraint, expressed by wrapping the text stream.
// ---------------------------------------------------------------------------

// phrase keeps only the documents whose text holds the query's terms adjacent
// and in order.
//
// It wraps another scorer instead of sweeping the corpus, and that is the whole
// design. engine.Posting carries Doc and Freq and no position, so an exact
// phrase can only be decided from the document text — one record decode per
// document considered, which milestone 5 section 3.2 measured as where
// throughput collapses. Wrapping bounds that decode to the inner scorer's
// candidates; sweeping every DocID does not.
type phrase struct {
	inner engine.Scorer
	ix    *engine.Index
	depth int // how deep to ask the inner scorer, chosen here rather than by the caller
}

func (p *phrase) Name() string { return "phrase" }

func (p *phrase) Candidates(ctx context.Context, q engine.Query, k int) ([]engine.Candidate, error) {
	if k <= 0 {
		return nil, nil
	}
	want := engine.Tokenize(q.Text)
	if len(want) == 0 {
		return nil, nil
	}
	inner, err := p.inner.Candidates(ctx, q, p.depth)
	if err != nil {
		return nil, err
	}
	kept := make([]engine.Candidate, 0, len(inner))
	for _, c := range inner {
		d, ok := p.ix.Doc(c.Doc)
		if !ok {
			continue
		}
		if adjacent(engine.Tokenize(d.Text), want) {
			kept = append(kept, c)
		}
	}
	return engine.TopK(kept, k), nil
}

func adjacent(hay, needle []string) bool {
outer:
	for i := 0; i+len(needle) <= len(hay); i++ {
		for j, w := range needle {
			if hay[i+j] != w {
				continue outer
			}
		}
		return true
	}
	return false
}

// TestAConstraintExcludesThroughFusion is the constraint half of the milestone.
//
// "tools" holds both query terms and not the phrase, so BM25 ranks it and the
// constraint must not. The assertion is that it is absent from the *fused*
// result, because a constraint that only holds inside its own stream is not a
// constraint an adopter can use.
func TestAConstraintExcludesThroughFusion(t *testing.T) {
	ix := expressiveCorpus(t)
	txt := text.New(ix)
	ph := &phrase{inner: txt, ix: ix, depth: 8}

	q := engine.Query{Text: "machine learning"}
	survey, tools := resolve(t, ix, "survey"), resolve(t, ix, "tools")

	// The constraint stream on its own discriminates.
	only, err := ph.Candidates(t.Context(), q, 4)
	if err != nil {
		t.Fatalf("phrase alone: %v", err)
	}
	if !contains(only, survey) || contains(only, tools) {
		t.Fatalf("the phrase constraint does not discriminate on its own: %+v", only)
	}

	// The arrangement the documentation leads to: a custom Scorer, fused with
	// the built-in one the adopter actually wants ranking from.
	got, err := engine.Search(t.Context(), q, 4, fusion.Fuse, txt, ph)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !contains(got, survey) {
		t.Fatalf("the phrase match is missing from the fused result: %+v", got)
	}
	if contains(got, tools) {
		t.Fatalf("a document the constraint refused is in the fused result: %+v", got)
	}
}
