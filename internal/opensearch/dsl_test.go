// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// Milestone 25's judgment sentence, run rather than asserted:
//
//	every row of the PRD's query-DSL table has a test, and every refusing row
//	answers 4xx or 501 rather than 200 with an empty hit list.
//
// TestEveryRowOfTheDSLTable is the first half. TestTheRefusalRateIsCounted is the
// second — it counts the table rather than trusting the number written into the
// PRD, because a row that quietly became implemented would otherwise still be
// published as refused.

// dslCorpus is the one fixture these tests share: five documents with a mapped
// number, a mapped date, a keyword and a phrase, which is the smallest corpus that
// tells each row of the table apart from its neighbour.
func dslCorpus(t *testing.T) *httptest.Server {
	t.Helper()
	srv, _ := newTestServer(t)

	status, raw := do(t, srv, http.MethodPut, "/papers", `{"mappings":{"properties":{
		"views":{"type":"integer"},
		"published":{"type":"date"},
		"status":{"type":"keyword"},
		"title":{"type":"text"},
		"vec":{"type":"knn_vector","dimension":3}}}}`)
	if status != http.StatusOK {
		t.Fatalf("create papers: status %d (body %s)", status, raw)
	}

	for _, d := range []struct{ id, body string }{
		{"rrf", `{"text":"reciprocal rank fusion combines rankings","title":"fusion","status":"published","views":42,"published":"2024-03-01","vec":[1,0,0]}`},
		{"bm25", `{"text":"bm25 ranks documents by term frequency","title":"ranking","status":"published","views":7,"published":"2023-01-15","vec":[0.9,0.1,0]}`},
		{"pottery", `{"text":"an unrelated document about pottery","title":"pots","status":"draft","views":1,"published":"2022-06-30","vec":[0,1,0]}`},
		{"vector", `{"text":"dense vector retrieval combines with lexical search","title":"vectors","status":"draft","views":100,"published":"2025-11-11","vec":[0,0,1]}`},
		{"graph", `{"text":"graph proximity as a ranking signal","title":"graphs","status":"published","views":3,"published":"2021-02-02","vec":[0,0.1,0.9]}`},
	} {
		if status, raw := do(t, srv, http.MethodPut, "/papers/_doc/"+d.id, d.body); status != http.StatusCreated {
			t.Fatalf("index %s: status %d (body %s)", d.id, status, raw)
		}
	}
	return srv
}

// hitIDs runs a search and returns the ids it found, sorted — so a test asserts on
// a set rather than on a ranking it did not ask about.
func hitIDs(t *testing.T, srv *httptest.Server, body string) []string {
	t.Helper()
	status, raw := do(t, srv, http.MethodPost, "/papers/_search", body)
	if status != http.StatusOK {
		t.Fatalf("%s: status %d (body %s)", body, status, raw)
	}
	ids := decodeIDs(t, raw)
	slices.Sort(ids)
	return ids
}

func decodeIDs(t *testing.T, raw []byte) []string {
	t.Helper()
	var res struct {
		Hits struct {
			Hits []struct {
				ID string `json:"_id"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	ids := make([]string, 0, len(res.Hits.Hits))
	for _, h := range res.Hits.Hits {
		ids = append(ids, h.ID)
	}
	return ids
}

func wantIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("found %v, want %v", got, want)
	}
}

// TestEveryRowOfTheDSLTable is the milestone's first clause: every row of PRD
// section 4 is exercised, and the ones that map are checked against documents
// rather than against a status code.
func TestEveryRowOfTheDSLTable(t *testing.T) {
	srv := dslCorpus(t)

	t.Run("match operator or", func(t *testing.T) {
		// Fusion is the OR: a document holding either token is a candidate.
		wantIDs(t, hitIDs(t, srv, `{"query":{"match":{"text":"pottery graph"}}}`), "pottery", "graph")
	})

	t.Run("match operator and", func(t *testing.T) {
		wantIDs(t, hitIDs(t, srv, `{"query":{"match":{"text":{"query":"combines rankings","operator":"and"}}}}`), "rrf")
		// And it is narrower than the same clause with the default operator, which
		// is the whole point of the row.
		wantIDs(t, hitIDs(t, srv, `{"query":{"match":{"text":"combines rankings"}}}`), "rrf", "vector")
	})

	t.Run("term", func(t *testing.T) {
		wantIDs(t, hitIDs(t, srv, `{"query":{"term":{"status":"draft"}}}`), "pottery", "vector")
		// On a mapped number the value is looked up as query.EncodeInt wrote it,
		// not as the digits a tokenizer would have made.
		wantIDs(t, hitIDs(t, srv, `{"query":{"term":{"views":42}}}`), "rrf")
	})

	t.Run("terms", func(t *testing.T) {
		wantIDs(t, hitIDs(t, srv, `{"query":{"terms":{"title":["pots","graphs"]}}}`), "pottery", "graph")
	})

	t.Run("prefix", func(t *testing.T) {
		wantIDs(t, hitIDs(t, srv, `{"query":{"prefix":{"text":"rank"}}}`), "rrf", "bm25", "graph")
	})

	t.Run("wildcard", func(t *testing.T) {
		wantIDs(t, hitIDs(t, srv, `{"query":{"wildcard":{"title":"*s"}}}`), "pottery", "graph", "vector")
	})

	t.Run("fuzzy", func(t *testing.T) {
		// One edit from "pottery".
		wantIDs(t, hitIDs(t, srv, `{"query":{"fuzzy":{"text":{"value":"potery","fuzziness":1}}}}`), "pottery")
	})

	t.Run("match with fuzziness", func(t *testing.T) {
		wantIDs(t, hitIDs(t, srv, `{"query":{"match":{"text":{"query":"potery","fuzziness":"AUTO"}}}}`), "pottery")
	})

	t.Run("match_phrase", func(t *testing.T) {
		wantIDs(t, hitIDs(t, srv, `{"query":{"match_phrase":{"text":"reciprocal rank fusion"}}}`), "rrf")
		// The words are all present in another order and the phrase is not.
		wantIDs(t, hitIDs(t, srv, `{"query":{"match_phrase":{"text":"fusion rank reciprocal"}}}`))
	})

	t.Run("range on a number", func(t *testing.T) {
		wantIDs(t, hitIDs(t, srv, `{"query":{"range":{"views":{"gte":7,"lte":42}}}}`), "bm25", "rrf")
		// gt and lt step to the next representable value, which for an integer is
		// exact rather than approximate — so the same interval excludes both ends.
		wantIDs(t, hitIDs(t, srv, `{"query":{"range":{"views":{"gt":7,"lt":42}}}}`))
	})

	t.Run("range on a date", func(t *testing.T) {
		wantIDs(t, hitIDs(t, srv, `{"query":{"range":{"published":{"gte":"2024-01-01"}}}}`), "rrf", "vector")
	})

	t.Run("exists", func(t *testing.T) {
		wantIDs(t, hitIDs(t, srv, `{"query":{"exists":{"field":"status"}}}`),
			"rrf", "bm25", "pottery", "vector", "graph")
	})

	t.Run("bool must", func(t *testing.T) {
		wantIDs(t, hitIDs(t, srv, `{"query":{"bool":{"must":[
			{"match":{"text":"ranking"}},{"term":{"status":"published"}}]}}}`), "graph")
	})

	t.Run("bool must_not", func(t *testing.T) {
		wantIDs(t, hitIDs(t, srv, `{"query":{"bool":{
			"should":[{"match":{"text":"ranking rankings"}}],
			"must_not":[{"term":{"status":"draft"}}]}}}`), "rrf", "graph")
	})

	t.Run("bool should", func(t *testing.T) {
		// should on its own is the fusion doing nothing special.
		wantIDs(t, hitIDs(t, srv, `{"query":{"bool":{"should":[
			{"match":{"text":"pottery"}},{"match":{"text":"graph"}}]}}}`), "pottery", "graph")
	})

	t.Run("bool filter", func(t *testing.T) {
		wantIDs(t, hitIDs(t, srv, `{"query":{"bool":{
			"must":[{"match":{"text":"ranking rankings retrieval"}}],
			"filter":[{"range":{"views":{"gte":40}}}]}}}`), "rrf", "vector")
	})

	t.Run("bool filter alone still ranks", func(t *testing.T) {
		// A bool holding nothing but filters has no other ranking, so the filters
		// are not blanked — otherwise this would answer nothing to a query that
		// named two documents.
		wantIDs(t, hitIDs(t, srv, `{"query":{"bool":{"filter":[{"term":{"status":"draft"}}]}}}`),
			"pottery", "vector")
	})

	t.Run("match_all", func(t *testing.T) {
		wantIDs(t, hitIDs(t, srv, `{"query":{"match_all":{}}}`),
			"rrf", "bm25", "pottery", "vector", "graph")
	})

	t.Run("from and size", func(t *testing.T) {
		first := hitIDs(t, srv, `{"query":{"match_all":{}},"size":2}`)
		if len(first) != 2 {
			t.Fatalf("size 2 returned %d hits", len(first))
		}
		second := hitIDs(t, srv, `{"query":{"match_all":{}},"from":2,"size":2}`)
		if len(second) != 2 {
			t.Fatalf("from 2 size 2 returned %d hits", len(second))
		}
		for _, id := range second {
			if slices.Contains(first, id) {
				t.Errorf("%q is on both pages; from did not advance", id)
			}
		}
	})
}

// A filter narrows without voting, and that is a different thing from a weight of
// zero — which would remove the document entirely. The two are told apart by
// asking whether the filtered document is still ranked by the clause that
// nominated it.
func TestAFilterNarrowsWithoutVoting(t *testing.T) {
	srv := dslCorpus(t)

	// Both drafts pass the filter. vector matches four of the text clause's tokens
	// and pottery matches one, so if the filter voted — an equal vote to both —
	// pottery would climb on the strength of matching it.
	status, raw := do(t, srv, http.MethodPost, "/papers/_search", `{"query":{"bool":{
		"must":[{"match":{"text":"pottery retrieval vector dense"}}],
		"filter":[{"term":{"status":"draft"}}]}}}`)
	if status != http.StatusOK {
		t.Fatalf("status %d (body %s)", status, raw)
	}
	ids := decodeIDs(t, raw)
	if len(ids) != 2 {
		t.Fatalf("found %d hits, want the two drafts (body %s)", len(ids), raw)
	}
	if ids[0] != "vector" {
		t.Errorf("top hit is %q, want \"vector\": the filter voted", ids[0])
	}
}

// A required multi-token clause is a disjunction, and query.Must intersects. anyOf
// is what keeps "this match must have matched" from silently becoming "every word
// of it must have matched".
func TestARequiredMatchIsNotSilentlyAnAnd(t *testing.T) {
	srv := dslCorpus(t)

	// Neither document holds both words; each holds one. A must over the two token
	// streams directly would find nothing.
	wantIDs(t, hitIDs(t, srv, `{"query":{"bool":{
		"must":[{"match":{"text":"pottery graph"}}],
		"filter":[{"exists":{"field":"status"}}]}}}`), "pottery", "graph")

	// And operator "and" still means and.
	wantIDs(t, hitIDs(t, srv, `{"query":{"bool":{
		"must":[{"match":{"text":{"query":"pottery graph","operator":"and"}}}]}}}`))
}

// A pattern naming more terms than query.Glob will scan is refused rather than
// answered from the truncated prefix. Inside a Go program the caller chose the
// pattern; over HTTP a truncated match presented as a complete one is a wrong
// answer with a 200 on it.
func TestAPatternOverTheVocabularyCapIsRefused(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", "")

	// One document holding more distinct terms than the cap.
	body := `{"text":"`
	for i := range 5000 {
		body += fmt.Sprintf("w%d ", i)
	}
	body += `"}`
	if status, raw := do(t, srv, http.MethodPut, "/papers/_doc/1", body); status != http.StatusCreated {
		t.Fatalf("index: status %d (body %s)", status, raw)
	}

	if status, raw := do(t, srv, http.MethodPost, "/papers/_search",
		`{"query":{"wildcard":{"text":"w*"}}}`); status != http.StatusBadRequest {
		t.Errorf("wildcard over the cap: status %d, want 400 (body %s)", status, raw)
	}
	// A narrower head is under the cap and answers.
	if status, raw := do(t, srv, http.MethodPost, "/papers/_search",
		`{"query":{"prefix":{"text":"w49"}}}`); status != http.StatusOK {
		t.Errorf("narrow prefix: status %d, want 200 (body %s)", status, raw)
	}
}

// A term whose value cannot have been indexed is refused, because a 200 with no
// hits for it is exactly the silent nothing this package refuses everywhere else.
// A match on the same value is zero hits, because free text is different: a person
// typing punctuation into a search box is ordinary rather than mistaken.
func TestATermThatCannotMatchIsRefusedAndAMatchIsNot(t *testing.T) {
	srv := dslCorpus(t)

	if status, raw := do(t, srv, http.MethodPost, "/papers/_search",
		`{"query":{"term":{"status":"   "}}}`); status != http.StatusBadRequest {
		t.Errorf("term on whitespace: status %d, want 400 (body %s)", status, raw)
	}
	if status, raw := do(t, srv, http.MethodPost, "/papers/_search",
		`{"query":{"match":{"text":"   "}}}`); status != http.StatusOK {
		t.Errorf("match on whitespace: status %d, want 200 (body %s)", status, raw)
	}
}

// The mapping is the whole reason a range works, so it has to survive a restart. A
// mapping lost on reopen would leave every later document encoded by a different
// rule than the ones before it, with no error at either end.
func TestAMappingSurvivesAReopen(t *testing.T) {
	dir := t.TempDir()
	reg, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	srv := httptest.NewServer(NewServer(reg, 1<<20))

	do(t, srv, http.MethodPut, "/papers", `{"mappings":{"properties":{"views":{"type":"integer"}}}}`)
	do(t, srv, http.MethodPut, "/papers/_doc/1?refresh=true", `{"text":"a","views":9}`)
	srv.Close()
	if err := reg.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	srv2 := httptest.NewServer(NewServer(reopened, 1<<20))
	t.Cleanup(func() {
		srv2.Close()
		reopened.Close() //nolint:errcheck // the test is over
	})

	x, ok := reopened.Get("papers")
	if !ok {
		t.Fatal("papers did not reopen")
	}
	if got := x.Mapping().Type("views"); got != typeInteger {
		t.Fatalf("views is mapped as %q after a reopen, want %q", got, typeInteger)
	}
	status, raw := do(t, srv2, http.MethodPost, "/papers/_search", `{"query":{"range":{"views":{"gte":5}}}}`)
	if status != http.StatusOK {
		t.Fatalf("range after reopen: status %d (body %s)", status, raw)
	}
	if ids := decodeIDs(t, raw); len(ids) != 1 || ids[0] != "1" {
		t.Errorf("range after reopen found %v, want [1]", ids)
	}
}

// Re-mapping a field is refused, because the documents already holding it were
// indexed by the old rule: after a change the field holds two encodings and a
// range query reads half of it.
func TestRemappingAFieldIsRefused(t *testing.T) {
	srv, reg := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", `{"mappings":{"properties":{"views":{"type":"integer"}}}}`)

	if status, raw := do(t, srv, http.MethodPut, "/papers/_mapping",
		`{"properties":{"views":{"type":"keyword"}}}`); status != http.StatusBadRequest {
		t.Errorf("re-map: status %d, want 400 (body %s)", status, raw)
	}
	// A mapping is applied as a unit: this one names a good field and a conflicting
	// one, and neither lands.
	if status, raw := do(t, srv, http.MethodPut, "/papers/_mapping",
		`{"properties":{"fresh":{"type":"keyword"},"views":{"type":"text"}}}`); status != http.StatusBadRequest {
		t.Errorf("partial re-map: status %d, want 400 (body %s)", status, raw)
	}
	x, _ := reg.Get("papers")
	if got := x.Mapping().Type("fresh"); got != "" {
		t.Errorf("fresh is mapped as %q after a refused request; the mapping was applied in halves", got)
	}
	// The same type again is not a change and is accepted.
	if status, raw := do(t, srv, http.MethodPut, "/papers/_mapping",
		`{"properties":{"views":{"type":"integer"},"tag":{"type":"keyword"}}}`); status != http.StatusOK {
		t.Errorf("add a field: status %d, want 200 (body %s)", status, raw)
	}
}

// A knn_vector field lands on engine.Document.Vector rather than in the term
// space. Milestone 26 is what queries it; what this milestone owes is that the
// vector is stored, and that a malformed one is refused per document rather than
// per commit — engine.Add refuses a mismatched width for the whole write.
func TestAKnnVectorFieldIsStoredAndCheckedByWidth(t *testing.T) {
	srv, reg := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", `{"mappings":{"properties":{"vec":{"type":"knn_vector","dimension":3}}}}`)

	if status, raw := do(t, srv, http.MethodPut, "/papers/_doc/1", `{"text":"a","vec":[1,0,0]}`); status != http.StatusCreated {
		t.Fatalf("index with a vector: status %d (body %s)", status, raw)
	}
	x, ok := reg.Get("papers")
	if !ok {
		t.Fatal("no papers index")
	}
	id, live := x.Engine().Resolve("1")
	if !live {
		t.Fatal("document 1 is not live")
	}
	if v, ok := x.Engine().Vector(id); !ok || len(v) != 3 {
		t.Fatalf("stored vector is %v (ok %v), want three components", v, ok)
	}

	for _, bad := range []string{`{"vec":[1,0]}`, `{"vec":"not a vector"}`, `{"vec":[1,0,"x"]}`} {
		if status, raw := do(t, srv, http.MethodPut, "/papers/_doc/2", bad); status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (body %s)", bad, status, raw)
		}
	}
	// A second knn_vector field is refused: a document carries one vector.
	if status, raw := do(t, srv, http.MethodPut, "/papers/_mapping",
		`{"properties":{"other":{"type":"knn_vector","dimension":3}}}`); status != http.StatusBadRequest {
		t.Errorf("a second vector field: status %d, want 400 (body %s)", status, raw)
	}
}

// _bulk is one hold of the writer for the whole batch, and it reports per item.
func TestBulkAppliesEveryActionAndReportsPerItem(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", "")
	do(t, srv, http.MethodPut, "/papers/_doc/existing", `{"text":"already here"}`)

	body := `{"index":{"_index":"papers","_id":"a"}}
{"text":"alpha"}
{"create":{"_index":"papers","_id":"b"}}
{"text":"beta"}
{"create":{"_index":"papers","_id":"existing"}}
{"text":"this one conflicts"}
{"delete":{"_index":"papers","_id":"gone"}}
{"index":{"_index":"nosuch","_id":"c"}}
{"text":"gamma"}
`
	status, raw := do(t, srv, http.MethodPost, "/_bulk", body)
	if status != http.StatusOK {
		t.Fatalf("bulk: status %d (body %s)", status, raw)
	}
	var res struct {
		Errors bool                        `json:"errors"`
		Items  []map[string]map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode: %v (body %s)", err, raw)
	}
	if !res.Errors {
		t.Error("errors is false, but three of the five actions could not succeed")
	}
	if len(res.Items) != 5 {
		t.Fatalf("%d items, want 5 (body %s)", len(res.Items), raw)
	}
	for i, want := range []struct {
		op     string
		status float64
	}{
		{opIndex, 201},
		{opCreate, 201},
		{opCreate, 409},
		{opDelete, 404},
		{opIndex, 404},
	} {
		got, ok := res.Items[i][want.op]
		if !ok {
			t.Errorf("item %d is not a %q (got %v)", i, want.op, res.Items[i])
			continue
		}
		if got["status"] != want.status {
			t.Errorf("item %d (%s): status %v, want %v", i, want.op, got["status"], want.status)
		}
	}
	// The two that succeeded are searchable, and the three that did not are absent.
	wantIDs(t, hitIDs(t, srv, `{"query":{"match":{"text":"alpha beta gamma"}}}`), "a", "b")
}

// An action line with no source line after it is a parse failure for the whole
// batch and not a partial one: every pair after it would be read off by one, so
// the documents would land under the wrong ids.
func TestBulkRefusesABatchItWouldReadOffByOne(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", "")

	for _, body := range []string{
		"{\"index\":{\"_index\":\"papers\",\"_id\":\"a\"}}\n",
		"{\"index\":{\"_index\":\"papers\",\"_id\":\"a\"}}\n{\"text\":\"a\"}\n{\"index\":{\"_index\":\"papers\",\"_id\":\"b\"}}\n",
		"{\"update\":{\"_index\":\"papers\",\"_id\":\"a\"}}\n{\"doc\":{\"text\":\"a\"}}\n",
		"{\"index\":{\"_index\":\"papers\"}}\n{\"text\":\"a\"}\n",
		"not json\n",
	} {
		if status, raw := do(t, srv, http.MethodPost, "/_bulk", body); status != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400 (body %s)", body, status, raw)
		}
	}
	// And nothing landed.
	if status, raw := do(t, srv, http.MethodGet, "/papers/_doc/a", ""); status != http.StatusNotFound {
		t.Errorf("a document survived a refused batch: status %d (body %s)", status, raw)
	}
}

// Scroll and point-in-time are routed so they are refused by name. A 404 would
// read as "wrong URL" and send a client looking for a typo it does not have.
func TestCursorRoutesAreRefusedByName(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", "")

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/_search/scroll", `{"scroll_id":"x"}`},
		{http.MethodDelete, "/_search/scroll", ""},
		{http.MethodPost, "/papers/_pit", ""},
		{http.MethodPost, "/papers/_search?scroll=1m", `{"query":{"match_all":{}}}`},
	} {
		status, raw := do(t, srv, tc.method, tc.path, tc.body)
		if status != http.StatusNotImplemented {
			t.Errorf("%s %s: status %d, want 501 (body %s)", tc.method, tc.path, status, raw)
		}
	}
}

// dslTable is PRD section 4, transcribed. It is here rather than only in the
// document so the refusal rate the milestone publishes is counted from something
// a test can fail on: a row that quietly became implemented would otherwise stay
// published as refused, and a row that quietly started answering 200 with nothing
// would be the failure this whole surface is built to prevent.
var dslTable = []struct {
	row     string
	body    string
	refused bool
}{
	{"match (operator or)", `{"query":{"match":{"text":"fusion"}}}`, false},
	{"match (operator and)", `{"query":{"match":{"text":{"query":"fusion","operator":"and"}}}}`, false},
	{"term", `{"query":{"term":{"status":"draft"}}}`, false},
	{"terms", `{"query":{"terms":{"status":["draft"]}}}`, false},
	{"prefix", `{"query":{"prefix":{"text":"fus"}}}`, false},
	{"wildcard", `{"query":{"wildcard":{"text":"fus*"}}}`, false},
	{"fuzzy", `{"query":{"fuzzy":{"text":{"value":"fusiom","fuzziness":1}}}}`, false},
	{"match + fuzziness", `{"query":{"match":{"text":{"query":"fusiom","fuzziness":1}}}}`, false},
	{"match_phrase", `{"query":{"match_phrase":{"text":"rank fusion"}}}`, false},
	{"range", `{"query":{"range":{"views":{"gte":1}}}}`, false},
	{"exists", `{"query":{"exists":{"field":"status"}}}`, false},
	{"bool.must", `{"query":{"bool":{"must":[{"match":{"text":"fusion"}}]}}}`, false},
	{"bool.must_not", `{"query":{"bool":{"should":[{"match":{"text":"fusion"}}],"must_not":[{"term":{"status":"draft"}}]}}}`, false},
	{"bool.should", `{"query":{"bool":{"should":[{"match":{"text":"fusion"}}]}}}`, false},
	{"bool.filter", `{"query":{"bool":{"must":[{"match":{"text":"fusion"}}],"filter":[{"term":{"status":"published"}}]}}}`, false},
	{"match_all", `{"query":{"match_all":{}}}`, false},
	{"from/size", `{"query":{"match_all":{}},"from":1,"size":2}`, false},
	{"knn", `{"query":{"knn":{"vec":{"vector":[1,0,0],"k":2}}}}`, false},
	{"hybrid", `{"query":{"hybrid":{"queries":[{"match":{"text":"fusion"}},{"knn":{"vec":{"vector":[1,0,0],"k":3}}}]}}}`, false},
	{"function_score decay", `{"query":{"function_score":{"query":{"match_all":{}}}}}`, true},
	{"nested bool", `{"query":{"bool":{"must":[{"bool":{"must":[{"match":{"text":"a"}}]}}]}}}`, true},
	{"aggs", `{"aggs":{"x":{"terms":{"field":"status"}}}}`, true},
	{"sort (other than _score)", `{"query":{"match_all":{}},"sort":[{"views":"asc"}]}`, true},
	{"highlight", `{"query":{"match":{"text":"fusion"}},"highlight":{"fields":{"text":{}}}}`, true},
	{"scroll / PIT", `{"query":{"match_all":{}},"pit":{"id":"x"}}`, true},
}

// TestTheRefusalRateIsCounted is milestone 25's judgment sentence. Every row
// answers what the table says it answers, and the rate is logged so the number
// published in the PRD is one this test produced rather than one somebody counted
// by eye.
//
// The falsification clause is the other half: if more than half the rows refuse,
// "compatible" is the wrong word and the documentation has to say "wire subset"
// instead. That threshold is asserted here so it cannot be missed by not looking.
func TestTheRefusalRateIsCounted(t *testing.T) {
	srv := dslCorpus(t)

	refused := 0
	for _, tc := range dslTable {
		t.Run(tc.row, func(t *testing.T) {
			status, raw := do(t, srv, http.MethodPost, "/papers/_search", tc.body)
			switch {
			case tc.refused && status == http.StatusOK:
				t.Fatalf("row %q answered 200; a refusal is 4xx or 501 and never an empty hit list (body %s)", tc.row, raw)
			case tc.refused && status < 400:
				t.Fatalf("row %q answered %d, want 4xx or 501 (body %s)", tc.row, status, raw)
			case !tc.refused && status != http.StatusOK:
				t.Fatalf("row %q answered %d, want 200 (body %s)", tc.row, status, raw)
			}
		})
		if tc.refused {
			refused++
		}
	}

	rate := float64(refused) / float64(len(dslTable))
	t.Logf("refusal rate: %d of %d rows = %.1f%%", refused, len(dslTable), rate*100)
	if rate > 0.5 {
		t.Errorf("%.1f%% of the DSL table refuses, which is over half: the falsification clause fires and "+
			"the documentation has to say \"OpenSearch wire subset\" rather than \"OpenSearch compatible\"",
			rate*100)
	}
}
