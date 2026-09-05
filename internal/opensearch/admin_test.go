// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These are the parts of engine a client could not reach at all: making writes
// durable, rewriting segments, verifying a directory, and reading the index's own
// bookkeeping. Four routes carry OpenSearch names because what weft does under
// them is honestly what those names mean; the rest are under /_weft/, because
// inventing an OpenSearch spelling for something OpenSearch does not have would
// be a second untruth, and D-026 allows exactly one.

func TestFlushCommits(t *testing.T) {
	srv := dslCorpus(t)

	for _, path := range []string{"/papers/_flush", "/papers/_refresh"} {
		t.Run(path, func(t *testing.T) {
			status, raw := do(t, srv, http.MethodPost, path, "")
			if status != http.StatusOK {
				t.Fatalf("status %d, want 200 (body %s)", status, raw)
			}
			if !strings.Contains(string(raw), "_shards") {
				t.Errorf("the response is not shaped like OpenSearch's:\n%s", raw)
			}
		})
	}
}

// TestForceMergeTakesNoTarget. OpenSearch's max_num_segments asks for a specific
// number of segments and engine.Merge does not take one — accepting the
// parameter and merging to whatever it merges to would leave a client setting a
// number that is silently ignored.
func TestForceMergeTakesNoTarget(t *testing.T) {
	srv := dslCorpus(t)

	status, raw := do(t, srv, http.MethodPost, "/papers/_forcemerge", "")
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %s)", status, raw)
	}

	status, raw = do(t, srv, http.MethodPost, "/papers/_forcemerge?max_num_segments=2", "")
	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", status, raw)
	}
	if !strings.Contains(string(raw), "max_num_segments") {
		t.Errorf("the error does not name the parameter it refused:\n%s", raw)
	}
}

// TestForceMergeKeepsTheCorpus. A merge that lost a document would be the worst
// silent failure available here, so the count is checked across it.
func TestForceMergeKeepsTheCorpus(t *testing.T) {
	srv := dslCorpus(t)

	before := countOf(t, srv)
	if status, raw := do(t, srv, http.MethodPost, "/papers/_forcemerge", ""); status != http.StatusOK {
		t.Fatalf("forcemerge: status %d (body %s)", status, raw)
	}
	if after := countOf(t, srv); after != before {
		t.Errorf("the corpus was %d documents before the merge and %d after", before, after)
	}
}

func countOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	status, raw := do(t, srv, http.MethodGet, "/papers/_count", "")
	if status != http.StatusOK {
		t.Fatalf("_count: status %d (body %s)", status, raw)
	}
	var got struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode _count: %v (body %s)", err, raw)
	}
	return got.Count
}

func TestCount(t *testing.T) {
	srv := dslCorpus(t)

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		status, raw := do(t, srv, method, "/papers/_count", "")
		if status != http.StatusOK {
			t.Fatalf("%s: status %d, want 200 (body %s)", method, status, raw)
		}
		if !strings.Contains(string(raw), `"count":5`) {
			t.Errorf("%s: the count is not the corpus size:\n%s", method, raw)
		}
	}
}

// TestCountWithAQueryIsRefused. OpenSearch counts a query's matches; weft's
// search is a top-k fusion, and there is no k-independent count of what a fused
// ranking would have contained. Answering with the corpus size would be a number
// that looks like an answer.
func TestCountWithAQueryIsRefused(t *testing.T) {
	srv := dslCorpus(t)

	status, raw := do(t, srv, http.MethodPost, "/papers/_count", `{"query":{"match":{"text":"fusion"}}}`)

	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", status, raw)
	}
	if !strings.Contains(string(raw), "_search") {
		t.Errorf("the error does not say where the answer is:\n%s", raw)
	}
}

func TestAnalyze(t *testing.T) {
	srv := dslCorpus(t)

	status, raw := do(t, srv, http.MethodPost, "/papers/_analyze", `{"text":"Ranking, Fusion"}`)

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %s)", status, raw)
	}
	for _, want := range []string{`"ranking"`, `"fusion"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the tokens are missing %s:\n%s", want, raw)
		}
	}
}

// TestAnalyzeRefusesAnAnalyzer. One index has one tokenizer (D-022), fixed when
// it was committed. Naming another is a request this server cannot honour, and
// honouring it approximately would report terms the index does not hold.
func TestAnalyzeRefusesAnAnalyzer(t *testing.T) {
	srv := dslCorpus(t)

	status, raw := do(t, srv, http.MethodPost, "/papers/_analyze",
		`{"text":"Ranking","analyzer":"standard"}`)

	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", status, raw)
	}
	if !strings.Contains(string(raw), "analyzer") {
		t.Errorf("the error does not name what it refused:\n%s", raw)
	}
}

func TestWeftTerms(t *testing.T) {
	srv := dslCorpus(t)

	status, raw := do(t, srv, http.MethodGet, "/papers/_weft/terms?prefix=ran", "")

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %s)", status, raw)
	}
	if !strings.Contains(string(raw), "rank") {
		t.Errorf("the term walk found nothing under the prefix:\n%s", raw)
	}
	if !strings.Contains(string(raw), "doc_count") {
		t.Errorf("the terms carry no document count:\n%s", raw)
	}
}

// TestWeftTermsInAField reaches engine.FieldTerm, the one spelling a client
// cannot produce on its own.
func TestWeftTermsInAField(t *testing.T) {
	srv := dslCorpus(t)

	status, raw := do(t, srv, http.MethodGet, "/papers/_weft/terms?field=title&prefix=", "")

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %s)", status, raw)
	}
	if !strings.Contains(string(raw), "fusion") {
		t.Errorf("the field's terms are not there:\n%s", raw)
	}
}

func TestWeftPostings(t *testing.T) {
	srv := dslCorpus(t)

	status, raw := do(t, srv, http.MethodGet, "/papers/_weft/postings?term=fusion", "")

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %s)", status, raw)
	}
	for _, want := range []string{`"rrf"`, "freq", "doc_count"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the postings response is missing %s:\n%s", want, raw)
		}
	}
}

// TestWeftLimitIsCapped pins the ceiling on a number a request names.
//
// It is not closing a hole, and the comment on intParam says why at length:
// both slices were already bounded by something other than the limit. What this
// pins is that the bound stays explicit — a later change that widens the walk
// would otherwise reach an allocation with nothing between it and a ten-digit
// query parameter, and nobody would be told.
func TestWeftLimitIsCapped(t *testing.T) {
	srv := dslCorpus(t)

	for _, path := range []string{
		"/papers/_weft/terms?limit=2000000000",
		"/papers/_weft/postings?term=fusion&limit=2000000000",
	} {
		t.Run(path, func(t *testing.T) {
			status, raw := do(t, srv, http.MethodGet, path, "")

			if status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 (body %s)", status, raw)
			}
			if !strings.Contains(string(raw), "10000") {
				t.Errorf("the refusal does not name the cap:\n%s", raw)
			}
		})
	}
}

func TestWeftPostingsNeedsATerm(t *testing.T) {
	srv := dslCorpus(t)

	status, raw := do(t, srv, http.MethodGet, "/papers/_weft/postings", "")

	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", status, raw)
	}
	if !strings.Contains(string(raw), "term") {
		t.Errorf("the error does not name the missing parameter:\n%s", raw)
	}
}

func TestWeftScrub(t *testing.T) {
	srv := dslCorpus(t)

	// A scrub reads what is committed, so it is run after a flush — which is
	// what a client would do, and what makes the check about the segments rather
	// than about their absence.
	if status, raw := do(t, srv, http.MethodPost, "/papers/_flush", ""); status != http.StatusOK {
		t.Fatalf("flush: status %d (body %s)", status, raw)
	}

	status, raw := do(t, srv, http.MethodPost, "/papers/_weft/scrub", "")

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %s)", status, raw)
	}
	if !strings.Contains(string(raw), "scrubbed") {
		t.Errorf("the response does not say what was checked:\n%s", raw)
	}
}

// TestWeftQuery is weft's own query string over HTTP. query_string stays a 501,
// and this is why that is not a contradiction: the refusal is about reading
// Lucene's language as if it were this one, and here a client has asked for this
// one by name.
func TestWeftQuery(t *testing.T) {
	srv := dslCorpus(t)

	status, raw := do(t, srv, http.MethodPost, "/papers/_weft/query", `{"q":"+fusion","size":5}`)

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %s)", status, raw)
	}
	ids := decodeIDs(t, raw)
	if len(ids) == 0 {
		t.Fatalf("no hits:\n%s", raw)
	}
	for _, id := range ids {
		if id == "pottery" {
			t.Errorf("a required clause did not exclude a document that fails it: %v", ids)
		}
	}
}

// TestWeftQueryReportsASyntaxError. pkg/query refuses to answer a malformed
// query with zero results, and this route must not soften that.
func TestWeftQueryReportsASyntaxError(t *testing.T) {
	srv := dslCorpus(t)

	status, raw := do(t, srv, http.MethodPost, "/papers/_weft/query", `{"q":"\"unterminated"}`)

	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", status, raw)
	}
	if strings.TrimSpace(string(raw)) == "" {
		t.Error("the refusal carried no reason")
	}
}

func TestWeftQueryNeedsAQuery(t *testing.T) {
	srv := dslCorpus(t)

	status, raw := do(t, srv, http.MethodPost, "/papers/_weft/query", `{}`)

	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", status, raw)
	}
	// The reason is a JSON string, so the quotes around the field name arrive
	// escaped: `\"q\" is required`.
	if !strings.Contains(string(raw), `q\" is required`) {
		t.Errorf("the error does not name the missing field:\n%s", raw)
	}
}
