// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"encoding/json"
	"net/http"
	"testing"
)

// These close a gap the coverage profile named rather than a gap a failing
// request named: matchText and scalarText sat at 18.8% and 50%, and both are on
// the honest-refusal surface this milestone's whole claim rests on. Written
// after the implementation, and said so — they are not RED-first like the rest
// of this package.

func TestMatchAcceptsTheLongFormAndRefusesItsOptions(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", "")
	do(t, srv, http.MethodPut, "/papers/_doc/1", `{"text":"reciprocal rank fusion"}`)

	// The long form with nothing but query is the short form.
	status, raw := do(t, srv, http.MethodPost, "/papers/_search",
		`{"query":{"match":{"text":{"query":"fusion"}}}}`)
	if status != http.StatusOK {
		t.Fatalf("long-form match: status %d, want 200 (body %s)", status, raw)
	}
	var res struct {
		Hits struct {
			Hits []struct {
				ID string `json:"_id"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Hits.Hits) != 1 || res.Hits.Hits[0].ID != "1" {
		t.Errorf("long-form match found %+v, want the one document", res.Hits.Hits)
	}

	// Every option on it is named rather than dropped. `operator: and` read
	// silently as `or` returns more documents than the client asked for and
	// looks like a working search, which is the shape D-026 exists to refuse.
	for _, body := range []string{
		`{"query":{"match":{"text":{"query":"fusion","operator":"and"}}}}`,
		`{"query":{"match":{"text":{"query":"fusion","fuzziness":"AUTO"}}}}`,
		`{"query":{"match":{"text":{"query":"fusion","minimum_should_match":2}}}}`,
		`{"query":{"match":{"text":{"boost":2}}}}`,
		`{"query":{"match":{"text":{"query":42}}}}`,
		`{"query":{"match":{"text":"a","title":"b"}}}`,
		`{"query":{"match":"not an object"}}`,
	} {
		status, raw := do(t, srv, http.MethodPost, "/papers/_search", body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (body %s)", body, status, raw)
		}
	}
}

// A match whose text tokenizes to nothing is zero hits and not an error: the
// client asked for nothing, and it asked clearly.
func TestAMatchOnPunctuationFindsNothingWithoutFailing(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", "")
	do(t, srv, http.MethodPut, "/papers/_doc/1", `{"text":"reciprocal rank fusion"}`)

	status, raw := do(t, srv, http.MethodPost, "/papers/_search", `{"query":{"match":{"text":"   "}}}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %s)", status, raw)
	}
	var res struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
			Hits []json.RawMessage `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Hits.Total.Value != 0 || len(res.Hits.Hits) != 0 {
		t.Errorf("total %d over %d hits, want 0 and 0", res.Hits.Total.Value, len(res.Hits.Hits))
	}
	if res.Hits.Hits == nil {
		t.Error("hits is null; it has to be an empty array, because clients index into it")
	}
}

// The body-to-Document rule, at its edges. A number becomes the term it reads
// as, which is not a numeric index and does not pretend to be one — a range over
// it is milestone 25. A nested object is kept in _source and not indexed, which
// is what OpenSearch spells "index": false.
func TestScalarsAreIndexedAndStructuresAreOnlyStored(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", "")

	body := `{"text":"a paper","views":42,"active":true,"authors":["ann","bo"],"meta":{"doi":"10.1/x"}}`
	if status, raw := do(t, srv, http.MethodPut, "/papers/_doc/1", body); status != http.StatusCreated {
		t.Fatalf("index: status %d (body %s)", status, raw)
	}

	// _source keeps everything, indexed or not.
	_, raw := do(t, srv, http.MethodGet, "/papers/_doc/1", "")
	var got struct {
		Source json.RawMessage `json:"_source"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got.Source) != body {
		t.Errorf("_source = %s, want the body unchanged", got.Source)
	}

	for _, tc := range []struct {
		name  string
		query string
		want  int
	}{
		{"a number is its own term", `{"query":{"match":{"views":"42"}}}`, 1},
		{"a boolean is its own term", `{"query":{"match":{"active":"true"}}}`, 1},
		{"an array is not indexed", `{"query":{"match":{"authors":"ann"}}}`, 0},
		{"a nested object is not indexed", `{"query":{"match":{"meta":"10.1/x"}}}`, 0},
		{"a field is its own term space", `{"query":{"match":{"text":"42"}}}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := do(t, srv, http.MethodPost, "/papers/_search", tc.query)
			if status != http.StatusOK {
				t.Fatalf("status %d (body %s)", status, raw)
			}
			var res struct {
				Hits struct {
					Hits []json.RawMessage `json:"hits"`
				} `json:"hits"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(res.Hits.Hits) != tc.want {
				t.Errorf("%d hits, want %d (body %s)", len(res.Hits.Hits), tc.want, raw)
			}
		})
	}
}

// Accepting a mapping and dropping it is the worst of the three options: a range
// query later matches nothing, with nothing to report. Refused instead, naming
// the milestone that will honour it.
func TestCreateIndexRefusesAMappingItCannotHonour(t *testing.T) {
	srv, _ := newTestServer(t)

	status, raw := do(t, srv, http.MethodPut, "/papers",
		`{"mappings":{"properties":{"views":{"type":"integer"}}}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("create with mappings: status %d, want 400 (body %s)", status, raw)
	}

	// settings are accepted and ignored: shards and replicas have nowhere to
	// land in a single-node engine, and refusing them would refuse every client
	// that sends its defaults.
	if status, raw := do(t, srv, http.MethodPut, "/papers",
		`{"settings":{"number_of_shards":1}}`); status != http.StatusOK {
		t.Errorf("create with settings: status %d, want 200 (body %s)", status, raw)
	}

	if status, raw := do(t, srv, http.MethodPut, "/other", `not json`); status != http.StatusBadRequest {
		t.Errorf("create with a body that is not JSON: status %d, want 400 (body %s)", status, raw)
	}
}

func TestIndexingABodyThatIsNotAnObjectIsRefused(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", "")

	for _, body := range []string{`[1,2,3]`, `"a string"`, `not json`} {
		if status, raw := do(t, srv, http.MethodPut, "/papers/_doc/1", body); status != http.StatusBadRequest {
			t.Errorf("PUT %s: status %d, want 400 (body %s)", body, status, raw)
		}
	}
}

func TestRefreshRefusesAValueItCannotMean(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", "")

	if status, raw := do(t, srv, http.MethodPut, "/papers/_doc/1?refresh=maybe", `{"text":"a"}`); status != http.StatusBadRequest {
		t.Errorf("refresh=maybe: status %d, want 400 (body %s)", status, raw)
	}
	// And the write did not happen. A 400 over a document that is already
	// indexed tells the client the write failed when it did not, which is worse
	// than either answer on its own.
	if status, raw := do(t, srv, http.MethodGet, "/papers/_doc/1", ""); status != http.StatusNotFound {
		t.Errorf("after a refused refresh the document exists: status %d, want 404 (body %s)", status, raw)
	}
}

// CodeQL found this before a user did: engine.NewCollector does
// make([]Candidate, 0, k) with the k Search was given, so a size taken straight
// from a request body is a remote allocation primitive — {"size": 2000000000}
// asks this process for about 32 GB. The cap belongs here rather than in pkg/:
// it is a policy about requests, not about the library, and OpenSearch answers
// the same question the same way with index.max_result_window.
func TestASizeThatWouldAllocateTheHeapIsRefused(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", "")
	do(t, srv, http.MethodPut, "/papers/_doc/1", `{"text":"a"}`)

	for _, size := range []string{"10001", "1000000", "2000000000"} {
		body := `{"query":{"match":{"text":"a"}},"size":` + size + `}`
		status, raw := do(t, srv, http.MethodPost, "/papers/_search", body)
		if status != http.StatusBadRequest {
			t.Errorf("size %s: status %d, want 400 (body %s)", size, status, raw)
		}
	}
	// match_all reaches the same allocation by a different route.
	if status, raw := do(t, srv, http.MethodPost, "/papers/_search", `{"size":2000000000}`); status != http.StatusBadRequest {
		t.Errorf("match_all with size 2000000000: status %d, want 400 (body %s)", status, raw)
	}
	// The window itself is still allowed.
	if status, raw := do(t, srv, http.MethodPost, "/papers/_search",
		`{"query":{"match":{"text":"a"}},"size":10000}`); status != http.StatusOK {
		t.Errorf("size at the window: status %d, want 200 (body %s)", status, raw)
	}
}
