// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer is the only helper these tests share. Everything else is spelled
// out per test, because a fixture that sets up three documents for a test that
// needs one hides which document mattered.
func newTestServer(t *testing.T) (*httptest.Server, *Registry) {
	t.Helper()
	reg, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	// 1 MiB rather than the production limit: a test that allocated the real one
	// would spend a hundred megabytes to assert a status code.
	srv := httptest.NewServer(NewServer(reg, 1<<20))
	t.Cleanup(func() {
		srv.Close()
		reg.Close() //nolint:errcheck // the test is over
	})
	return srv, reg
}

func do(t *testing.T, srv *httptest.Server, method, path, body string) (status int, out []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

// D-026: the version is the one thing this server says that is not true, and it
// is said here because every official client branches on it.
func TestRootAnnouncesTheDistributionAClientChecks(t *testing.T) {
	srv, _ := newTestServer(t)

	status, raw := do(t, srv, http.MethodGet, "/", "")
	if status != http.StatusOK {
		t.Fatalf("GET /: status %d, want 200 (body %s)", status, raw)
	}
	var root struct {
		Version struct {
			Number       string `json:"number"`
			Distribution string `json:"distribution"`
		} `json:"version"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("decode GET /: %v (body %s)", err, raw)
	}
	if root.Version.Distribution != "opensearch" {
		t.Errorf("version.distribution = %q, want %q", root.Version.Distribution, "opensearch")
	}
	if root.Version.Number != Version {
		t.Errorf("version.number = %q, want %q", root.Version.Number, Version)
	}
}

func TestClusterHealthIsGreen(t *testing.T) {
	srv, _ := newTestServer(t)

	status, raw := do(t, srv, http.MethodGet, "/_cluster/health", "")
	if status != http.StatusOK {
		t.Fatalf("GET /_cluster/health: status %d, want 200 (body %s)", status, raw)
	}
	var health struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &health); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if health.Status != "green" {
		t.Errorf("status = %q, want green", health.Status)
	}
}

func TestAnIndexRoundTripsThroughTheDocumentAPI(t *testing.T) {
	srv, _ := newTestServer(t)

	if status, raw := do(t, srv, http.MethodPut, "/papers", ""); status != http.StatusOK {
		t.Fatalf("PUT /papers: status %d, want 200 (body %s)", status, raw)
	}
	if status, _ := do(t, srv, http.MethodHead, "/papers", ""); status != http.StatusOK {
		t.Errorf("HEAD /papers: status %d, want 200", status)
	}
	if status, _ := do(t, srv, http.MethodHead, "/absent", ""); status != http.StatusNotFound {
		t.Errorf("HEAD /absent: status %d, want 404", status)
	}

	body := `{"text":"reciprocal rank fusion","title":"rrf"}`
	status, raw := do(t, srv, http.MethodPut, "/papers/_doc/1", body)
	if status != http.StatusCreated {
		t.Fatalf("PUT /papers/_doc/1: status %d, want 201 (body %s)", status, raw)
	}
	var wrote struct {
		Index  string `json:"_index"`
		ID     string `json:"_id"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(raw, &wrote); err != nil {
		t.Fatalf("decode write response: %v (body %s)", err, raw)
	}
	if wrote.Index != "papers" || wrote.ID != "1" || wrote.Result != "created" {
		t.Errorf("write response = %+v, want papers/1/created", wrote)
	}

	status, raw = do(t, srv, http.MethodGet, "/papers/_doc/1", "")
	if status != http.StatusOK {
		t.Fatalf("GET /papers/_doc/1: status %d, want 200 (body %s)", status, raw)
	}
	var got struct {
		Found  bool            `json:"found"`
		Source json.RawMessage `json:"_source"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if !got.Found {
		t.Error("found = false, want true")
	}
	if string(got.Source) != body {
		t.Errorf("_source = %s, want the body that was indexed (%s)", got.Source, body)
	}

	if status, _ := do(t, srv, http.MethodDelete, "/papers/_doc/1", ""); status != http.StatusOK {
		t.Errorf("DELETE /papers/_doc/1: status %d, want 200", status)
	}
	if status, _ := do(t, srv, http.MethodGet, "/papers/_doc/1", ""); status != http.StatusNotFound {
		t.Errorf("GET after delete: status %d, want 404", status)
	}
	if status, _ := do(t, srv, http.MethodDelete, "/papers", ""); status != http.StatusOK {
		t.Errorf("DELETE /papers: status %d, want 200", status)
	}
	if status, _ := do(t, srv, http.MethodHead, "/papers", ""); status != http.StatusNotFound {
		t.Errorf("HEAD after index delete: status %d, want 404", status)
	}
}

func TestCreatingAnIndexTwiceIsRefused(t *testing.T) {
	srv, _ := newTestServer(t)

	do(t, srv, http.MethodPut, "/papers", "")
	status, raw := do(t, srv, http.MethodPut, "/papers", "")
	if status != http.StatusBadRequest {
		t.Fatalf("PUT /papers twice: status %d, want 400 (body %s)", status, raw)
	}
	var e struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
		Status int `json:"status"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode error envelope: %v (body %s)", err, raw)
	}
	if e.Error.Type == "" || e.Status != http.StatusBadRequest {
		t.Errorf("error envelope = %+v, want a type and a matching status", e)
	}
}

// match is fusion over the query's tokens: one stream per token, and a document
// both streams rank beats a document only one of them ranks. That is the whole
// of the mapping, and the reason match needs no special case in the fuser.
func TestMatchIsFusionOverItsTokens(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", "")

	for id, text := range map[string]string{
		"both":    "reciprocal rank fusion combines rankings",
		"rank":    "rank correlation between assessors",
		"fusion":  "fusion energy confinement",
		"neither": "an unrelated document about pottery",
	} {
		body := `{"text":"` + text + `"}`
		if status, raw := do(t, srv, http.MethodPut, "/papers/_doc/"+id, body); status != http.StatusCreated {
			t.Fatalf("index %q: status %d (body %s)", id, status, raw)
		}
	}

	status, raw := do(t, srv, http.MethodPost, "/papers/_search",
		`{"query":{"match":{"text":"rank fusion"}},"size":10}`)
	if status != http.StatusOK {
		t.Fatalf("_search: status %d, want 200 (body %s)", status, raw)
	}

	var res struct {
		TimedOut bool `json:"timed_out"`
		Hits     struct {
			Total struct {
				Value    int    `json:"value"`
				Relation string `json:"relation"`
			} `json:"total"`
			Hits []struct {
				Index  string          `json:"_index"`
				ID     string          `json:"_id"`
				Score  float64         `json:"_score"`
				Source json.RawMessage `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode search response: %v (body %s)", err, raw)
	}
	if res.TimedOut {
		t.Error("timed_out = true, want false")
	}
	if len(res.Hits.Hits) == 0 {
		t.Fatalf("no hits (body %s)", raw)
	}
	if res.Hits.Hits[0].ID != "both" {
		var order []string
		for _, h := range res.Hits.Hits {
			order = append(order, h.ID)
		}
		t.Errorf("ranking = %v, want %q first: it is the only document both token streams rank", order, "both")
	}
	if res.Hits.Hits[0].Index != "papers" {
		t.Errorf("_index = %q, want papers", res.Hits.Hits[0].Index)
	}
	if len(res.Hits.Hits[0].Source) == 0 {
		t.Error("_source is empty; a hit has to carry the body the client indexed")
	}
	if res.Hits.Total.Relation != "eq" {
		t.Errorf("hits.total.relation = %q, want eq", res.Hits.Total.Relation)
	}
	for _, h := range res.Hits.Hits {
		if h.ID == "neither" {
			t.Error(`"neither" matched a query holding none of its terms`)
		}
	}
}

// D-026's rule, tested row by row. A query type this server cannot express is a
// 4xx or a 501 and never a 200 with an empty hit list — that second shape is
// exactly the silent wrong answer pkg/query refuses, relocated into someone
// else's dashboard.
func TestAnUnsupportedQueryIsNotAnEmptyResult(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", "")
	do(t, srv, http.MethodPut, "/papers/_doc/1", `{"text":"one document so a match would succeed"}`)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"aggs", `{"aggs":{"x":{"terms":{"field":"text"}}}}`},
		{"aggregations", `{"aggregations":{"x":{"terms":{"field":"text"}}}}`},
		{"highlight", `{"query":{"match":{"text":"document"}},"highlight":{"fields":{"text":{}}}}`},
		{"sort", `{"query":{"match":{"text":"document"}},"sort":[{"text":"asc"}]}`},
		{"knn", `{"query":{"knn":{"v":{"vector":[1,0],"k":2}}}}`},
		{"two query types at once", `{"query":{"match":{"text":"a"},"term":{"text":"b"}}}`},
		// Milestone 25 implemented bool, term, prefix, range and from. What stayed
		// refused is the part of each this engine has no way to express, and that
		// is what these rows check instead.
		{"nested bool", `{"query":{"bool":{"must":[{"bool":{"must":[{"match":{"text":"a"}}]}}]}}}`},
		{"minimum_should_match", `{"query":{"bool":{"should":[{"match":{"text":"a"}}],"minimum_should_match":1}}}`},
		{"boost", `{"query":{"bool":{"must":[{"match":{"text":"a"}}],"boost":2}}}`},
		{"deep paging", `{"query":{"match":{"text":"document"}},"from":10000,"size":10}`},
		{"ids", `{"query":{"ids":{"values":["1"]}}}`},
		{"query_string", `{"query":{"query_string":{"query":"document"}}}`},
		{"search_after", `{"query":{"match":{"text":"a"}},"search_after":[1]}`},
		{"numeric range on an unmapped field", `{"query":{"range":{"n":{"gte":1}}}}`},
		{"range format", `{"query":{"range":{"n":{"gte":"a","format":"epoch_millis"}}}}`},
		{"match_phrase on a named field", `{"query":{"match_phrase":{"title":"a b"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := do(t, srv, http.MethodPost, "/papers/_search", tc.body)
			if status == http.StatusOK {
				t.Fatalf("status 200 for an unsupported query; body %s", raw)
			}
			if status != http.StatusBadRequest && status != http.StatusNotImplemented {
				t.Fatalf("status %d, want 400 or 501 (body %s)", status, raw)
			}
			var e struct {
				Error struct {
					Type   string `json:"type"`
					Reason string `json:"reason"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &e); err != nil {
				t.Fatalf("decode error envelope: %v (body %s)", err, raw)
			}
			if e.Error.Reason == "" {
				t.Errorf("error.reason is empty; a refusal has to say what it refused (body %s)", raw)
			}
		})
	}
}

// The most basic request anyone makes. It has to work or say why, not 404.
func TestAnEmptySearchBodyMatchesEverything(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", "")
	do(t, srv, http.MethodPut, "/papers/_doc/1", `{"text":"first"}`)
	do(t, srv, http.MethodPut, "/papers/_doc/2", `{"text":"second"}`)

	for _, body := range []string{"", `{}`, `{"query":{"match_all":{}}}`} {
		status, raw := do(t, srv, http.MethodPost, "/papers/_search", body)
		if status != http.StatusOK {
			t.Fatalf("_search with body %q: status %d (body %s)", body, status, raw)
		}
		var res struct {
			Hits struct {
				Total struct {
					Value int `json:"value"`
				} `json:"total"`
			} `json:"hits"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if res.Hits.Total.Value != 2 {
			t.Errorf("body %q: hits.total.value = %d, want 2", body, res.Hits.Total.Value)
		}
	}
}

func TestSearchingAnIndexThatDoesNotExistIsNotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	status, raw := do(t, srv, http.MethodPost, "/absent/_search", `{"query":{"match":{"text":"a"}}}`)
	if status != http.StatusNotFound {
		t.Fatalf("_search on a missing index: status %d, want 404 (body %s)", status, raw)
	}
}

func TestAnUnknownRouteIsNotTwoHundred(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, path := range []string{"/papers/_bulk", "/_nodes", "/papers/_doc/1/_explain"} {
		if status, raw := do(t, srv, http.MethodGet, path, ""); status == http.StatusOK {
			t.Errorf("GET %s: status 200 for a route this server does not serve (body %s)", path, raw)
		}
	}
}

func TestABodyOverTheLimitIsRefused(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", "")

	big := `{"text":"` + strings.Repeat("x", 2<<20) + `"}`
	status, _ := do(t, srv, http.MethodPut, "/papers/_doc/1", big)
	if status != http.StatusRequestEntityTooLarge && status != http.StatusBadRequest {
		t.Errorf("a body over the limit: status %d, want 413 or 400", status)
	}
}

// refresh=true means durability here and visibility in OpenSearch, because weft
// makes a write visible the moment Add returns. The mapping is stated in the
// docs; this is the half of it a test can check.
func TestRefreshTrueMakesTheWriteSurviveAReopen(t *testing.T) {
	reg, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	root := reg.root
	srv := httptest.NewServer(NewServer(reg, 1<<20))

	do(t, srv, http.MethodPut, "/papers", "")
	if status, raw := do(t, srv, http.MethodPut, "/papers/_doc/1?refresh=true", `{"text":"durable"}`); status != http.StatusCreated {
		t.Fatalf("PUT with refresh=true: status %d (body %s)", status, raw)
	}
	srv.Close()
	if err := reg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenRegistry(root)
	if err != nil {
		t.Fatalf("OpenRegistry after restart: %v", err)
	}
	defer reopened.Close() //nolint:errcheck // the test is over
	x, ok := reopened.Get("papers")
	if !ok {
		t.Fatal("the index did not survive the restart")
	}
	if live, _ := x.Engine().Stats(); live != 1 {
		t.Errorf("live documents after restart = %d, want 1", live)
	}
	if _, ok := x.Source().Get("1"); !ok {
		t.Error("the _source did not survive the restart")
	}
}
