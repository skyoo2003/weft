package eval

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseS2Batch(t *testing.T) {
	raw := []byte(`[
	  {"externalIds": {"CorpusId": 111, "DOI": "10.1000/a"},
	   "references": [{"externalIds": {"CorpusId": 222}},
	                  {"externalIds": {"DOI": "10.1000/no-corpus-id"}},
	                  {"externalIds": {"CorpusId": 333}}],
	   "embedding": {"model": "specter_v2", "vector": [1.5, -2.5]}},
	  null,
	  {"externalIds": {"DOI": "10.1000/c"}, "references": [], "embedding": null}
	]`)

	got, err := parseS2Batch(raw, 3)
	if err != nil {
		t.Fatalf("parseS2Batch: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}

	first := got[0]
	if first.CorpusID != "111" {
		t.Errorf("CorpusID = %q, want 111", first.CorpusID)
	}
	// The reference without a CorpusId is dropped: it cannot match anything in the
	// corpus, so keeping it would be a dangling link carrying no information.
	if want := []string{"222", "333"}; !slices.Equal(first.Refs, want) {
		t.Errorf("Refs = %v, want %v", first.Refs, want)
	}
	if want := []float32{1.5, -2.5}; !slices.Equal(first.Vector, want) {
		t.Errorf("Vector = %v, want %v", first.Vector, want)
	}

	// A null entry keeps its position. That alignment is the only thing tying a
	// response back to the document it was asked about.
	if got[1] != nil {
		t.Errorf("entry 1 = %+v, want nil for a paper the endpoint could not find", got[1])
	}

	if got[2].CorpusID != "" || len(got[2].Refs) != 0 || got[2].Vector != nil {
		t.Errorf("entry 2 = %+v, want empty", got[2])
	}
}

// TestParseS2BatchRejectsMisalignedResponse is the guard that matters most here.
// The endpoint answers positionally, so a length mismatch silently reattributes
// every subsequent paper — including its citation edges — to the wrong document.
func TestParseS2BatchRejectsMisalignedResponse(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		n    int
	}{
		{"too few", `[{"externalIds": {"CorpusId": 1}}]`, 2},
		{"too many", `[{"externalIds": {"CorpusId": 1}}, null]`, 1},
		{"empty array for one id", `[]`, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseS2Batch([]byte(tc.raw), tc.n); !errors.Is(err, ErrS2Length) {
				t.Errorf("error = %v, want ErrS2Length", err)
			}
		})
	}
}

func TestParseS2BatchRejectsNonJSON(t *testing.T) {
	if _, err := parseS2Batch([]byte(`{"error": "rate limited"}`), 1); !errors.Is(err, ErrBadRecord) {
		t.Errorf("error = %v, want ErrBadRecord", err)
	}
}

// TestParseS2BatchRejectsNonFiniteVector matters because engine.Add refuses a
// non-finite component outright, and one such vector in a 171K-document build
// would abort the whole index.
func TestParseS2BatchRejectsNonFiniteVector(t *testing.T) {
	// JSON has no NaN or Infinity literal, and encoding/json rejects a number
	// outside float64 outright (1e400 is a decode error, not +Inf). The reachable
	// case is narrower and less obvious: 1e300 is a valid, finite float64 that
	// becomes +Inf the moment it is narrowed to float32.
	raw := []byte(`[{"externalIds": {"CorpusId": 1}, "embedding": {"model": "specter_v2", "vector": [1e300, 2.0]}}]`)
	got, err := parseS2Batch(raw, 1)
	if err != nil {
		t.Fatalf("parseS2Batch: %v", err)
	}
	if got[0].Vector != nil {
		t.Errorf("Vector = %v, want nil", got[0].Vector)
	}
	if !got[0].VectorRejected {
		t.Error("VectorRejected = false; a discarded vector must be distinguishable from an absent one")
	}
	// The rest of the record survives: the paper still contributes citation edges.
	if got[0].CorpusID != "1" {
		t.Errorf("CorpusID = %q, want 1", got[0].CorpusID)
	}
}

// TestCorpusIDAcceptsNumberAndString pins the reason externalIds is decoded as
// json.RawMessage: CorpusId arrives as a JSON number while every other identifier
// is a string, and decoding through map[string]any would render a large id in
// exponential notation.
func TestCorpusIDAcceptsNumberAndString(t *testing.T) {
	tests := []struct {
		name, raw, want string
	}{
		{"number", `[{"externalIds": {"CorpusId": 215736195}}]`, "215736195"},
		{"string", `[{"externalIds": {"CorpusId": "215736195"}}]`, "215736195"},
		{"large number keeps every digit", `[{"externalIds": {"CorpusId": 9007199254740993}}]`, "9007199254740993"},
		{"absent", `[{"externalIds": {"DOI": "10.1000/x"}}]`, ""},
		{"null externalIds", `[{"references": []}]`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseS2Batch([]byte(tc.raw), 1)
			if err != nil {
				t.Fatalf("parseS2Batch: %v", err)
			}
			if got[0].CorpusID != tc.want {
				t.Errorf("CorpusID = %q, want %q", got[0].CorpusID, tc.want)
			}
		})
	}
}

func testClient(t *testing.T, h http.Handler) *S2Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewS2Client("", func(string, ...any) {})
	c.BaseURL = srv.URL
	c.Pace = 0 // Pacing exists for the real endpoint's shared budget, not for tests.
	return c
}

func TestS2ClientRetriesRateLimit(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `[{"externalIds": {"CorpusId": 7}}]`)
	}))

	got, err := c.Batch(context.Background(), []string{"CorpusId:7"})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("server saw %d calls, want 3", calls.Load())
	}
	if len(got) != 1 || got[0].CorpusID != "7" {
		t.Errorf("got %+v, want one paper with CorpusID 7", got)
	}
}

func TestS2ClientGivesUpAfterMaxRetries(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	c.MaxRetries = 2

	if _, err := c.Batch(context.Background(), []string{"CorpusId:7"}); !errors.Is(err, ErrS2Status) {
		t.Errorf("error = %v, want ErrS2Status", err)
	}
	if calls.Load() != 3 {
		t.Errorf("server saw %d calls, want 3 (initial + 2 retries)", calls.Load())
	}
}

// TestS2ClientDoesNotRetryClientError: a 400 means the request itself is wrong, and
// repeating it burns rate limit the rest of a multi-hour job needs.
func TestS2ClientDoesNotRetryClientError(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error": "unrecognized id type"}`)
	}))

	if _, err := c.Batch(context.Background(), []string{"Nonsense:7"}); !errors.Is(err, ErrS2Status) {
		t.Errorf("error = %v, want ErrS2Status", err)
	}
	if calls.Load() != 1 {
		t.Errorf("server saw %d calls, want 1", calls.Load())
	}
}

// TestS2ClientDoesNotRetryMisalignment: a 200 that disagrees about length is a
// contract change, not a transient failure. Retrying produces the same wrong
// answer more slowly.
func TestS2ClientDoesNotRetryMisalignment(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `[]`)
	}))

	if _, err := c.Batch(context.Background(), []string{"CorpusId:7"}); !errors.Is(err, ErrS2Length) {
		t.Errorf("error = %v, want ErrS2Length", err)
	}
	if calls.Load() != 1 {
		t.Errorf("server saw %d calls, want 1", calls.Load())
	}
}

func TestS2ClientRejectsOversizedBatch(t *testing.T) {
	c := NewS2Client("", nil)
	refs := make([]string, S2BatchLimit+1)
	if _, err := c.Batch(context.Background(), refs); err == nil {
		t.Error("Batch accepted more than the documented limit")
	}
}

func TestS2ClientEmptyBatchIsNotAnError(t *testing.T) {
	c := NewS2Client("", nil)
	got, err := c.Batch(context.Background(), nil)
	if err != nil || got != nil {
		t.Errorf("Batch(nil) = %v, %v; want nil, nil", got, err)
	}
}

func TestS2ClientHonoursCancellation(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Batch(ctx, []string{"CorpusId:7"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded — a long Retry-After must not outlive the context", err)
	}
}

func TestS2ClientSendsAPIKeyAndFields(t *testing.T) {
	var seen string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("x-api-key")
		if got := r.URL.Query().Get("fields"); got != S2Fields {
			t.Errorf("fields = %q, want %q", got, S2Fields)
		}
		fmt.Fprint(w, `[{"externalIds": {"CorpusId": 7}}]`)
	}))
	c.APIKey = "secret"

	if _, err := c.Batch(context.Background(), []string{"CorpusId:7"}); err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if seen != "secret" {
		t.Errorf("x-api-key = %q, want secret", seen)
	}
}
