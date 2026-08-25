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

	// What the graph scorer does when no outsider has touched Seeds. The
	// arrangement that works has to leave this stream exactly as it is.
	wantGraph, err := graph.New(ix, txt).Candidates(t.Context(), q, 4)
	if err != nil {
		t.Fatalf("graph baseline: %v", err)
	}
	if len(wantGraph) == 0 {
		t.Fatal("the graph baseline is empty, so this test could not detect it moving")
	}

	// The arrangement an outsider reaches for first, pinned here as the trap it
	// is: put the per-query value in the one Query field that takes
	// caller-supplied document keys. It works for the external scorers and
	// breaks the in-tree scorer nobody was thinking about.
	seeded := q
	seeded.Seeds = []string{pivot}

	viaSeeds, err := engine.Search(t.Context(), seeded, 4, fusion.Fuse,
		txt, graph.New(ix, txt), &affinity{store: store}, &promoted{store: store})
	if err != nil {
		t.Fatalf("Search via Seeds: %v", err)
	}
	if !contains(viaSeeds, cafe) {
		t.Fatalf("Query.Seeds did not carry the value to the external scorers: %+v", viaSeeds)
	}
	seededGraph, err := graph.New(ix, txt).Candidates(t.Context(), seeded, 4)
	if err != nil {
		t.Fatalf("graph under the Seeds arrangement: %v", err)
	}
	if sameStream(seededGraph, wantGraph) {
		t.Fatal("Query.Seeds no longer collides with the graph scorer — the warning in engine.Query's doc comment, and the reason to bind at construction, would both be stale")
	}

	// The arrangement engine.Query's doc comment prescribes: bind the value when
	// you construct the scorer, and construct one per search. Query is untouched,
	// so no other scorer can observe that an outsider needed an input at all.
	bound, err := engine.Search(t.Context(), q, 4, fusion.Fuse,
		txt, graph.New(ix, txt),
		&affinity{store: store, pivot: pivot}, &promoted{store: store, pivot: pivot})
	if err != nil {
		t.Fatalf("Search with the value bound at construction: %v", err)
	}
	if !contains(bound, cafe) {
		t.Fatalf("binding at construction did not carry the value to both external scorers: %+v", bound)
	}
	boundGraph, err := graph.New(ix, txt).Candidates(t.Context(), q, 4)
	if err != nil {
		t.Fatalf("graph under the bound arrangement: %v", err)
	}
	if !sameStream(boundGraph, wantGraph) {
		t.Fatalf("binding at construction moved the graph stream: got %+v, want %+v", boundGraph, wantGraph)
	}

	// The half of the input that is not per-query. Two scorers and three Search
	// calls later it must still have been built once: a corpus-sized store
	// rebuilt per query is the cost this task existed to make visible.
	if store.built != 1 {
		t.Fatalf("the corpus-sized store was built %d times, not once", store.built)
	}
}

// sameStream compares two candidate streams by document and order. Score is
// deliberately not compared: the graph scorer's proximity score is a function of
// which seed it started from, so two streams naming the same documents in the
// same order is the strongest claim this comparison can make.
func sameStream(got, want []engine.Candidate) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].Doc != want[i].Doc {
			return false
		}
	}
	return true
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

	// The arrangement the documentation leads to, pinned here as the trap it is:
	// a custom Scorer, fused with the built-in one the adopter wants ranking
	// from. RRF is a union of votes, so absence from the constraint stream costs
	// "tools" nothing in the text stream and the refused document comes back.
	//
	// This is the assertion that a constraint expressed as a Scorer is a ranking
	// preference and not a filter. It fails loudly if fusion.Fuse ever starts
	// intersecting, which would be a change of contract worth failing on.
	leaked, err := engine.Search(t.Context(), q, 4, fusion.Fuse, txt, ph)
	if err != nil {
		t.Fatalf("Search with Fuse: %v", err)
	}
	if !contains(leaked, tools) {
		t.Fatal("fusion.Fuse excluded a document the constraint refused — it is no longer a union of votes, and the custom Fuser below is no longer the reason an exclusion holds")
	}

	// What actually holds an exclusion, and it needs no new exported name:
	// Search takes a Fuser, so the caller supplies one that reads the last
	// stream as a restriction. The value attaches to the position, which is the
	// same answer FuseWeighted gave in milestone 4.
	got, err := engine.Search(t.Context(), q, 4, restrictFuse, txt, ph)
	if err != nil {
		t.Fatalf("Search with restrictFuse: %v", err)
	}
	if !contains(got, survey) {
		t.Fatalf("the phrase match is missing from the restricted result: %+v", got)
	}
	if contains(got, tools) {
		t.Fatalf("a document the constraint refused survived the restricting Fuser: %+v", got)
	}
}

// restrictFuse fuses with RRF, treating the last stream as a restriction rather
// than as a vote: a document absent from it is dropped from every other stream
// before fusion.
//
// It is an ordinary engine.Fuser, which is the point. Search takes the fuser as
// a parameter precisely so that the fusion strategy is the caller's, and an
// adopter needing an intersection writes one instead of asking for a Query field
// or a wider Scorer. The convention is positional — the restriction goes last —
// and it is the caller's own, invisible to every scorer.
func restrictFuse(streams [][]engine.Candidate, k int) []engine.Candidate {
	if len(streams) == 0 {
		return fusion.Fuse(streams, k)
	}
	restrict := streams[len(streams)-1]
	allow := make(map[engine.DocID]bool, len(restrict))
	for _, c := range restrict {
		allow[c.Doc] = true
	}
	filtered := make([][]engine.Candidate, len(streams))
	for i, s := range streams[:len(streams)-1] {
		keep := make([]engine.Candidate, 0, len(s))
		for _, c := range s {
			if allow[c.Doc] {
				keep = append(keep, c)
			}
		}
		filtered[i] = keep
	}
	filtered[len(streams)-1] = restrict
	return fusion.Fuse(filtered, k)
}
