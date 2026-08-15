// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// S2Fields is the field set the citation graph and the vector arm both need, in
// one request. Asking for them separately would double a job already measured in
// hours.
//
// specter_v2 rather than the default specter_v1: the endpoint returns whichever
// is asked for, and which model produced the vectors is part of the measurement's
// provenance, so it is named explicitly rather than defaulted.
const S2Fields = "externalIds,references.externalIds,embedding.specter_v2"

// S2Model is the embedding model S2Fields asks for, and the label prepare expects
// to see back on every vector.
//
// It is reported rather than enforced: the query vectors come from a separate script
// pinned to this model, so a silent substitution would put two embedding spaces in
// one measurement with nothing downstream able to notice — but a hard reject on a
// label this repo has only seen in a fixture would drop every vector in the corpus
// the first time the endpoint spells it differently. prepare tallies what arrived.
const S2Model = "specter_v2"

// S2BatchLimit is the endpoint's documented maximum ids per request.
const S2BatchLimit = 500

var (
	// ErrS2Length reports a response whose length does not match the request. The
	// endpoint returns results positionally, with null for ids it could not find,
	// and that alignment is the only thing tying a response entry back to the
	// document it was asked about. A length mismatch means every subsequent paper
	// is attributed to the wrong document — silently, and with plausible-looking
	// citation edges.
	ErrS2Length = errors.New("eval: Semantic Scholar returned a different number of results than ids requested")

	ErrS2Status = errors.New("eval: Semantic Scholar returned an error status")
)

// S2Paper is the distilled form of one batch response entry.
type S2Paper struct {
	// CorpusID is Semantic Scholar's own key as a decimal string, or empty if the
	// response carried no CorpusId.
	CorpusID string

	// Refs are the CorpusIds this paper cites. References without a CorpusId are
	// dropped: they cannot match anything in the corpus, so they would be dangling
	// links that carry no information and cost memory.
	Refs []string

	// Vector is the SPECTER embedding, or nil if the paper has none.
	Vector []float32

	// Model is the embedding model the response says produced Vector, empty if it
	// carried none. Provenance: S2Fields asks for S2Model, and a response quietly
	// answering with a different one puts two embedding spaces in one corpus.
	Model string

	// VectorRejected records that a vector was present but not usable, in either of
	// the two ways it can be: a non-finite component, which engine.Add refuses
	// outright, or every component zero, which engine.Add accepts and
	// pkg/scorer/vector then skips for having no direction. Distinguishing this from
	// "no vector" matters when reporting vector coverage: one is the API having
	// nothing, the other is us discarding something. Counting either as coverage
	// would be worse than both — a document the vector arm is measured over and
	// never scores.
	VectorRejected bool
}

// S2Record is one corpus document's joined graph and vector data, as written to
// the resumable prepare cache.
//
// Model is stored beside the vector rather than tallied in memory because this
// cache is resumable and a tally is not. prepare is an hours-long job that is
// expected to be interrupted and rerun; a run that dies mid-fetch prints no
// summary, so the models behind the batches it did write are lost, and the run that
// finishes the job reports only the batches it fetched itself. If the endpoint
// answered an earlier batch with a model other than S2Model, the final report comes
// out clean and build combines two embedding spaces in one index — invalidating the
// vector arm the graph delta is measured against, with nothing in any output to say
// so. Persisted, the provenance of every cached vector outlives the interruption
// that lost the tally.
//
// Absent in a cache written before this field existed, where it reads back as "" and
// means the provenance was never recorded — which is not the same as a wrong model
// and is not reported as one.
type S2Record struct {
	Key      string    `json:"key"` // cord_uid
	CorpusID string    `json:"corpus_id,omitempty"`
	Refs     []string  `json:"refs,omitempty"`
	Vector   []float32 `json:"vec,omitempty"`
	Model    string    `json:"model,omitempty"`
}

// s2Response mirrors only the fields S2Fields asks for.
type s2Response []*struct {
	ExternalIDs map[string]json.RawMessage `json:"externalIds"`
	References  []struct {
		ExternalIDs map[string]json.RawMessage `json:"externalIds"`
	} `json:"references"`
	Embedding *struct {
		Model  string    `json:"model"`
		Vector []float64 `json:"vector"`
	} `json:"embedding"`
}

// corpusID extracts CorpusId from an externalIds map.
//
// The values are json.RawMessage rather than a typed field because the endpoint
// returns CorpusId as a JSON number and every other identifier as a string.
// Decoding into map[string]string fails on the number; decoding into
// map[string]any turns the id into a float64, and a large CorpusId then formats
// in exponential notation.
func corpusID(ext map[string]json.RawMessage) string {
	raw, ok := ext["CorpusId"]
	if !ok {
		return ""
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// parseS2Batch decodes a batch response, checking it aligns with the n ids that
// were requested. Entries the endpoint could not find come back nil, in place.
func parseS2Batch(raw []byte, n int) ([]*S2Paper, error) {
	var resp s2Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRecord, err)
	}
	if len(resp) != n {
		return nil, fmt.Errorf("%d results for %d ids: %w", len(resp), n, ErrS2Length)
	}

	out := make([]*S2Paper, n)
	for i, entry := range resp {
		if entry == nil {
			continue // Not found. The caller keeps the position.
		}
		p := &S2Paper{CorpusID: corpusID(entry.ExternalIDs)}
		for _, r := range entry.References {
			if id := corpusID(r.ExternalIDs); id != "" {
				p.Refs = append(p.Refs, id)
			}
		}
		if e := entry.Embedding; e != nil && len(e.Vector) > 0 {
			// Carried out, not asserted on. The request names a model but the
			// response is what actually arrived, and the two silently disagreeing is
			// how a corpus ends up holding two embedding spaces — which reads as a
			// weak vector scorer rather than as a data error. Rejecting here on a
			// label this code has only ever seen in a fixture would be worse: one
			// unexpected spelling and the corpus loses every vector it has. The
			// caller tallies the label and says so.
			p.Model = e.Model
			v := make([]float32, len(e.Vector))
			var nonzero bool
			for j, c := range e.Vector {
				// Checked after the narrowing, not before. encoding/json already
				// refuses a number outside float64, so a non-finite float64 cannot
				// reach here at all — but float32 has a far smaller range, and a
				// perfectly ordinary JSON value like 1e300 decodes to a finite
				// float64 and then narrows to +Inf. That is the value engine.Add
				// rejects, and one of them would abort a 171K-document build.
				f := float32(c)
				if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
					v = nil
					break
				}
				if f != 0 {
					nonzero = true
				}
				v[j] = f
			}
			// An all-zero vector is rejected alongside the non-finite ones, and for
			// the opposite reason: engine.Add takes it happily, and pkg/scorer/vector
			// then skips the document at query time because a zero norm has no
			// direction to compare against. Kept, it is a vector that counts toward
			// build's coverage and toward nothing else — which is how a text+vector
			// arm gets published over a corpus whose vector stream has almost no
			// opinions, reading as a weak scorer rather than as absent data. That is
			// docs/EVAL.md section 4.1 again, one layer down. Rejected, it lands in
			// the count prepare already prints for the vectors it discarded.
			if !nonzero {
				v = nil
			}
			p.Vector, p.VectorRejected = v, v == nil
		}
		out[i] = p
	}
	return out, nil
}

// S2Client fetches from the Semantic Scholar graph API. Use NewS2Client; the zero
// value has no HTTP client.
//
// Batch is safe to call from several goroutines, and the point of saying so is Pace:
// the pacing gap is enforced across all of them, so parallelising a fetch changes how
// many requests are in flight but not how fast they leave. The exported fields are
// read-only once the client is in use — set them before the first Batch, not between
// two of them.
type S2Client struct {
	HTTP    *http.Client
	BaseURL string

	// APIKey may be empty. The unauthenticated endpoint serves the same fields
	// under a much lower shared rate limit, which is why Pace and MaxRetries below
	// are not optional niceties.
	APIKey string

	// Pace is the minimum gap between requests. With no key the endpoint shares
	// one budget across every anonymous caller, so going faster earns 429s that
	// cost more than the wait would have.
	Pace time.Duration

	MaxRetries int

	// Log receives one line per retry. Rate-limit waits that are not visible look
	// exactly like a hung process on a job measured in hours.
	Log func(format string, args ...any)

	// last is when the previous request went out, guarded by mu. The lock is held
	// across the pacing sleep rather than only around the assignment, because that
	// is what makes Pace a property of the client instead of of one goroutine:
	// callers queue behind it and go out one per Pace. Guarding the assignment
	// alone would leave every waiter measuring its gap from the same stale
	// timestamp, all deciding at once that they had waited long enough, and the
	// burst would land on the shared anonymous budget Pace exists to stay under.
	mu   sync.Mutex
	last time.Time
}

// NewS2Client returns a client with defaults tuned for the unauthenticated
// endpoint. apiKey may be empty.
func NewS2Client(apiKey string, log func(string, ...any)) *S2Client {
	return &S2Client{
		// Long timeout on purpose: a 500-id request carrying 500 SPECTER vectors
		// is roughly 8.5 MB and takes about 18 seconds when the endpoint is healthy.
		HTTP:       &http.Client{Timeout: 4 * time.Minute},
		BaseURL:    "https://api.semanticscholar.org/graph/v1/paper/batch",
		APIKey:     apiKey,
		Pace:       1100 * time.Millisecond,
		MaxRetries: 8,
		Log:        log,
	}
}

// Batch resolves up to S2BatchLimit prefixed lookup ids — "CorpusId:215736195",
// "DOI:10.1000/x" — and returns results positionally aligned with refs.
func (c *S2Client) Batch(ctx context.Context, refs []string) ([]*S2Paper, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if len(refs) > S2BatchLimit {
		return nil, fmt.Errorf("%d ids exceeds the batch limit of %d", len(refs), S2BatchLimit)
	}

	body, err := json.Marshal(struct {
		IDs []string `json:"ids"`
	}{refs})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	backoff := 2 * time.Second
	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if err := c.wait(ctx); err != nil {
			return nil, err
		}
		raw, status, retryAfter, err := c.do(ctx, body)
		switch {
		case status != 0 && status != http.StatusOK && status != http.StatusTooManyRequests && status < 500:
			// 400-class other than 429: the request itself is wrong, so waiting
			// changes nothing.
			//
			// Classified ahead of err on purpose. do reads the body even on an error
			// status, and that read can fail on a connection the server is already
			// tearing down — a diagnostic that went missing, not a reason to reconsider
			// the status. Matching err first sent such a 400 down the retry path and
			// spent every one of MaxRetries+1 attempts, backing off up to two minutes
			// each, on an answer that cannot change and on a rate limit shared with
			// every other anonymous caller. Status 0 (no response at all) and 200 (a
			// body worth re-reading) still fall through to the err case below.
			detail := truncate(raw, 200)
			if err != nil {
				detail = fmt.Sprintf("%s [body read failed: %v]", detail, err)
			}
			return nil, fmt.Errorf("status %d: %s: %w", status, detail, ErrS2Status)
		case err != nil:
			lastErr = err
		case status == http.StatusOK:
			papers, parseErr := parseS2Batch(raw, len(refs))
			if parseErr != nil {
				// Not retried. A response that arrived as HTTP 200 and still
				// disagrees about length or shape is a contract change, and
				// repeating it only produces the same wrong answer more slowly.
				return nil, parseErr
			}
			return papers, nil
		default:
			// 429 and 5xx: the two classes worth waiting on. Nothing else reaches
			// here — every other status returned above, and a status of 0 is only
			// ever reported alongside an error, which the case above matched.
			lastErr = fmt.Errorf("status %d: %w", status, ErrS2Status)
		}

		// The wait spaces out the *next* attempt, so after the last one there is
		// nothing left to space out. Sleeping here would add the whole backoff — up
		// to two minutes, or whatever a server-supplied Retry-After asks for — to a
		// failure that is already final, with the operator watching a job that has
		// stopped doing anything.
		if attempt == c.MaxRetries {
			break
		}

		sleep := backoff
		if retryAfter > 0 {
			sleep = retryAfter
		}
		if c.Log != nil {
			c.Log("s2: attempt %d/%d failed (%v), waiting %s",
				attempt+1, c.MaxRetries+1, lastErr, sleep.Round(time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sleep):
		}
		if backoff < 2*time.Minute {
			backoff *= 2
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", c.MaxRetries+1, lastErr)
}

func (c *S2Client) do(ctx context.Context, body []byte) (raw []byte, status int, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"?fields="+S2Fields, bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("x-api-key", c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	// Nothing to report that the read below does not report first: this handle is
	// open for reading, and its Close has no buffered write to fail on.
	defer func() { _ = resp.Body.Close() }()

	// The body is read even on an error status: the endpoint puts its reason
	// there, and a request rejected for a fixable reason should say which.
	raw, err = readAllLimited(resp)
	if s := resp.Header.Get("Retry-After"); s != "" {
		if secs, convErr := strconv.Atoi(s); convErr == nil && secs > 0 {
			retryAfter = time.Duration(secs) * time.Second
		}
	}
	return raw, resp.StatusCode, retryAfter, err
}

// readAllLimited caps the response at 128 MiB. A 500-id response is about 8.5 MB,
// so this only trips on something pathological — and an unbounded read of a
// remote body is how a long job becomes an out-of-memory kill.
func readAllLimited(resp *http.Response) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(http.MaxBytesReader(nil, resp.Body, 128<<20)); err != nil {
		return buf.Bytes(), err
	}
	return buf.Bytes(), nil
}

// wait enforces Pace between requests, and is where cancellation is observed on
// the happy path.
func (c *S2Client) wait(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.last.IsZero() {
		if gap := c.Pace - time.Since(c.last); gap > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(gap):
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.last = time.Now()
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
