// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// The graph signal is the one weft has had since milestone 1 and this surface has
// never carried — not the query half and not the indexing half.
// engine.Document.Links had no way to be filled over the wire at all, which makes
// this the case D-025 registered as the sign that decision had gone wrong,
// running in the other direction: not the server outgrowing the library, but the
// server unable to reach half of it.
//
// What these tests pin is that closing the gap costs the same two things
// milestone 26 measured for recency — a binding the mapping has to carry,
// because the wire has no place to say "these strings are edges", and a clause.
// Not a change to how anything is fused.

// graphCorpus is five documents with one cycle, so a walk from any seed has
// somewhere to go and the direction of an edge is observable.
//
//	rrf -> bm25 -> tfidf -> rrf     (a cycle)
//	vector -> bm25                  (two documents point at bm25)
//	orphan                          (no edges at all)
func graphCorpus(t *testing.T) *httptest.Server {
	t.Helper()
	srv, _ := newTestServer(t)

	status, raw := do(t, srv, http.MethodPut, "/papers", `{"mappings":{"properties":{
		"title":{"type":"text"},
		"cites":{"type":"keyword","links":true}}}}`)
	if status != http.StatusOK {
		t.Fatalf("create papers: status %d (body %s)", status, raw)
	}

	for _, d := range []struct{ id, body string }{
		{"rrf", `{"text":"reciprocal rank fusion","cites":["bm25"]}`},
		{"bm25", `{"text":"bm25 ranks documents","cites":["tfidf"]}`},
		{"tfidf", `{"text":"tf idf term weighting","cites":["rrf"]}`},
		{"vector", `{"text":"dense vector retrieval","cites":["bm25"]}`},
		{"orphan", `{"text":"an unrelated document about pottery"}`},
	} {
		if status, raw := do(t, srv, http.MethodPut, "/papers/_doc/"+d.id, d.body); status != http.StatusCreated {
			t.Fatalf("index %s: status %d (body %s)", d.id, status, raw)
		}
	}
	return srv
}

// TestLinksIsAMappingBinding. A JSON array of strings is a JSON array of
// strings; nothing on the wire says which one is a set of edges, so the mapping
// says it — exactly as D-031 has the mapping say which date is the recency
// signal.
func TestLinksIsAMappingBinding(t *testing.T) {
	srv := graphCorpus(t)

	status, raw := do(t, srv, http.MethodGet, "/papers/_mapping", "")
	if status != http.StatusOK {
		t.Fatalf("status %d (body %s)", status, raw)
	}
	if !strings.Contains(string(raw), `"links":true`) {
		t.Errorf("the mapping does not report the binding:\n%s", raw)
	}
}

// TestLinksBelongsToAKeywordField. A link is a document key, and a key is not
// analysed text: binding links to a text field would mean edges named by
// whatever the tokenizer made of them, which reach nothing and say nothing.
func TestLinksBelongsToAKeywordField(t *testing.T) {
	srv, _ := newTestServer(t)

	status, raw := do(t, srv, http.MethodPut, "/bad", `{"mappings":{"properties":{
		"cites":{"type":"text","links":true}}}}`)

	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", status, raw)
	}
	if !strings.Contains(string(raw), "keyword") {
		t.Errorf("the error does not say what the field would have to be:\n%s", raw)
	}
}

// TestGraphWalksFromItsSeeds is the query half. Seeds are document keys, which
// is engine.Query.Seeds — the escape hatch that keeps graph proximity usable
// without another scorer nominating where to start.
func TestGraphWalksFromItsSeeds(t *testing.T) {
	srv := graphCorpus(t)

	ids := hitIDs(t, srv, `{"query":{"weft_graph":{"seeds":["rrf"]}}}`)

	if !slices.Contains(ids, "bm25") {
		t.Errorf("the walk did not reach what rrf cites: %v", ids)
	}
	// graph.New excludes the seeds themselves, which is the difference between
	// "documents like this one" and "this one".
	if slices.Contains(ids, "rrf") {
		t.Errorf("the seed came back as its own neighbour: %v", ids)
	}
	if slices.Contains(ids, "orphan") {
		t.Errorf("a document with no edges was reached by a graph walk: %v", ids)
	}
}

func TestGraphCanIncludeItsSeeds(t *testing.T) {
	srv := graphCorpus(t)

	ids := hitIDs(t, srv, `{"query":{"weft_graph":{"seeds":["rrf"],"include_seeds":true}}}`)

	if !slices.Contains(ids, "rrf") {
		t.Errorf("include_seeds did not bring the seed back: %v", ids)
	}
}

// TestGraphNeedsSeeds. Over HTTP there is no "the scorer written before this
// one" to fall back to, so an empty seed list is a stream that comes back empty
// for a reason the response cannot show. Refused instead.
func TestGraphNeedsSeeds(t *testing.T) {
	srv := graphCorpus(t)

	status, raw := do(t, srv, http.MethodPost, "/papers/_search", `{"query":{"weft_graph":{}}}`)

	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", status, raw)
	}
	if !strings.Contains(string(raw), "seeds") {
		t.Errorf("the error does not name what is missing:\n%s", raw)
	}
}

// TestGraphNeedsALinksField is the mapping requirement stated as a refusal. An
// index whose mapping binds nothing to links holds no edges, so this query could
// only ever answer nothing — and answering nothing successfully is the failure
// D-026 exists to prevent.
func TestGraphNeedsALinksField(t *testing.T) {
	srv := dslCorpus(t)

	status, raw := do(t, srv, http.MethodPost, "/papers/_search",
		`{"query":{"weft_graph":{"seeds":["rrf"]}}}`)

	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", status, raw)
	}
	if !strings.Contains(string(raw), "links") {
		t.Errorf("the error does not say what the mapping is missing:\n%s", raw)
	}
}

// TestGraphSeedThatIsNotThere. A seed naming no document is a walk with no
// starting point, and it is a client's typo far more often than it is a corpus
// that has moved on.
func TestGraphSeedThatIsNotThere(t *testing.T) {
	srv := graphCorpus(t)

	status, raw := do(t, srv, http.MethodPost, "/papers/_search",
		`{"query":{"weft_graph":{"seeds":["nosuch"]}}}`)

	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", status, raw)
	}
	if !strings.Contains(string(raw), "nosuch") {
		t.Errorf("the error does not quote the seed back:\n%s", raw)
	}
}

// TestGraphPersonalizedPageRank reaches graph.NewPPR and its two options, which
// are half of pkg/scorer/graph's exported surface.
func TestGraphPersonalizedPageRank(t *testing.T) {
	srv := graphCorpus(t)

	ids := hitIDs(t, srv,
		`{"query":{"weft_graph":{"seeds":["rrf"],"ppr":true,"restart":0.2,"precision":0.001}}}`)

	if len(ids) == 0 {
		t.Error("the personalized pagerank walk produced nothing")
	}
}

// TestGraphOptionsWithoutPersonalizedPageRank. restart and precision configure
// the PPR walk and nothing else, so accepting them beside the plain walk would
// leave a client tuning a number that changes nothing.
func TestGraphOptionsWithoutPersonalizedPageRank(t *testing.T) {
	srv := graphCorpus(t)

	status, raw := do(t, srv, http.MethodPost, "/papers/_search",
		`{"query":{"weft_graph":{"seeds":["rrf"],"restart":0.2}}}`)

	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", status, raw)
	}
	if !strings.Contains(string(raw), "restart") {
		t.Errorf("the error does not name the option that would have been ignored:\n%s", raw)
	}
}

// TestGraphFusesLikeEverythingElse is milestone 26's property re-run for a
// fourth kind of stream: a signal joins the fusion by being a stream, and the
// code that fuses does not learn a fifth thing.
func TestGraphFusesLikeEverythingElse(t *testing.T) {
	srv := graphCorpus(t)

	ids := hitIDs(t, srv, `{"query":{"hybrid":{"queries":[
		{"match":{"text":"pottery"}},
		{"weft_graph":{"seeds":["rrf"]}}]}}}`)

	// The text stream finds the orphan and the graph stream cannot; the graph
	// stream finds bm25 and the text stream cannot. Fusion is a union of votes,
	// so both are here.
	for _, want := range []string{"orphan", "bm25"} {
		if !slices.Contains(ids, want) {
			t.Errorf("the fused result is missing %q, which one of the two streams found alone: %v", want, ids)
		}
	}
}

// TestGraphIsWeightedByPosition. hybrid's weights attach to a position, not to a
// kind of scorer, and a graph stream is not an exception — which is what keeps
// fusion from needing to know one is present.
func TestGraphIsWeightedByPosition(t *testing.T) {
	srv := graphCorpus(t)

	status, raw := do(t, srv, http.MethodPost, "/papers/_search", `{"query":{"hybrid":{"queries":[
		{"match":{"text":"pottery"}},
		{"weft_graph":{"seeds":["rrf"]}}],"weights":[1,0.1]}}}`)

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %s)", status, raw)
	}
}
