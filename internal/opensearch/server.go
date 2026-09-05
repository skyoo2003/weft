// SPDX-License-Identifier: Apache-2.0

// Package opensearch is the HTTP surface weft's library does not have.
//
// It lives under internal/ on purpose. A server is not part of the library
// contract, so keeping it out of pkg/ leaves engine's exported API — and the two
// golden files guarding it — untouched by anything decided here. That is the
// same argument docs/ARCHITECTURE.md makes for internal/eval, and it is written
// down as D-025.
//
// # What it claims to be, and what it does instead
//
// GET / reports OpenSearch 2.19.0. That is not true, and it is the only untrue
// thing this package says: every official client branches on the distribution
// and version before it will talk at all. Past the handshake the rule reverses —
// a query type weft cannot express returns 400 or 501 and never 200 with an
// empty hit list, because a search that quietly returns nothing is the failure
// pkg/query refuses everywhere else, relocated into someone else's dashboard.
// D-026 is the argument.
//
// # The mapping, in one paragraph
//
// A JSON body's "text" key becomes engine.Document.Text and every other scalar
// key becomes an engine.Field, because weft's index has exactly two term spaces
// and an HTTP body has none. A match query becomes one query.Glob stream per
// token, fused by fusion.Fuse — rank fusion is a union of votes, which is what an
// OR-ed match already means, so nothing in pkg/ had to learn what a match is.
package opensearch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// Version and Distribution are the handshake, and the handshake is a lie.
	// See D-026 for why it is this version and why it is confined to GET /.
	Version      = "2.19.0"
	Distribution = "opensearch"

	// DefaultMaxBody caps a request body. Every handler reads through
	// http.MaxBytesReader, so a client cannot make this process allocate more
	// than this per request whatever Content-Length claims.
	DefaultMaxBody = 100 << 20
)

// apiError is a refusal with the three things a client needs: a status, a type
// it can branch on, and a reason a person can act on.
//
// The reason follows the shape cmd/weft-eval uses for its own failures — what
// happened, why it matters, what to do instead — because an operator reading a
// 400 is in the position an operator reading a failed build is in.
type apiError struct {
	status int
	kind   string
	reason string
}

func (e *apiError) Error() string { return e.reason }

func badRequest(kind, format string, a ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, kind: kind, reason: fmt.Sprintf(format, a...)}
}

// hit is one search result.
type hit struct {
	Index  string          `json:"_index"`
	ID     string          `json:"_id"`
	Score  float64         `json:"_score"`
	Source json.RawMessage `json:"_source"`
}

func newHit(x *Index, id string, score float64) hit {
	h := hit{Index: x.Name(), ID: id, Score: score}
	if body, ok := x.Source().Get(id); ok {
		h.Source = body
	} else {
		// A document in the index with no body in the store. LoadSource refuses
		// to start from that state, so reaching it means the drift appeared
		// while the process was running — reported as an empty object rather
		// than as a missing key, so the hit is still JSON a client can read.
		h.Source = json.RawMessage(`{}`)
	}
	return h
}

// shardInfo is what a single-node engine has to say about sharding. Constant,
// because inventing a number here would be a second source of truth about a
// thing that does not exist.
type shardInfo struct {
	Total      int `json:"total"`
	Successful int `json:"successful"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
}

var oneShard = shardInfo{Total: 1, Successful: 1}

// clusterName is what a single-node weft calls itself, in the three places a
// client looks for it.
const clusterName = "weft"

// docResult answers a write or a delete. A struct rather than a map literal
// because the same five keys are spelled in three handlers, and a typo in one
// of them is a field a client silently does not find.
type docResult struct {
	Index   string    `json:"_index"`
	ID      string    `json:"_id"`
	Version int       `json:"_version"`
	Result  string    `json:"result"`
	Shards  shardInfo `json:"_shards"`
}

// getResult answers a document read, present or absent.
type getResult struct {
	Index   string          `json:"_index"`
	ID      string          `json:"_id"`
	Version int             `json:"_version,omitempty"`
	Found   bool            `json:"found"`
	Source  json.RawMessage `json:"_source,omitempty"`
}

// Server routes the subset of the OpenSearch API this milestone speaks.
type Server struct {
	reg     *Registry
	maxBody int64
	mux     *http.ServeMux
}

// NewServer wires the routes. maxBody caps every request body; pass
// DefaultMaxBody unless you are a test.
//
// Routing is http.ServeMux's method-and-wildcard patterns, which is why this
// package adds no dependency and `go list -m all` still prints one module.
func NewServer(reg *Registry, maxBody int64) *Server {
	s := &Server{reg: reg, maxBody: maxBody, mux: http.NewServeMux()}

	s.mux.HandleFunc("GET /{$}", s.handle(s.root))
	s.mux.HandleFunc("GET /_cluster/health", s.handle(s.health))

	s.mux.HandleFunc("PUT /{index}", s.handle(s.createIndex))
	s.mux.HandleFunc("HEAD /{index}", s.handle(s.headIndex))
	s.mux.HandleFunc("DELETE /{index}", s.handle(s.deleteIndex))

	s.mux.HandleFunc("PUT /{index}/_doc/{id}", s.handle(s.putDoc))
	s.mux.HandleFunc("POST /{index}/_doc/{id}", s.handle(s.putDoc))
	s.mux.HandleFunc("GET /{index}/_doc/{id}", s.handle(s.getDoc))
	s.mux.HandleFunc("DELETE /{index}/_doc/{id}", s.handle(s.deleteDoc))

	s.mux.HandleFunc("POST /{index}/_search", s.handle(s.search))
	s.mux.HandleFunc("GET /{index}/_search", s.handle(s.search))

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// handle turns a handler that returns an error into one that writes the error.
//
// Every refusal in this package goes through here, so there is exactly one place
// that decides what a client sees — and exactly one place to check that no
// failure path can end in a 200.
func (s *Server) handle(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBody)
		if err := fn(w, r); err != nil {
			s.writeError(w, err)
		}
	}
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	status, kind := http.StatusInternalServerError, "internal_server_error"

	var api *apiError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &api):
		status, kind = api.status, api.kind
	case errors.As(err, &tooLarge):
		status, kind = http.StatusRequestEntityTooLarge, "request_entity_too_large_exception"
	case errors.Is(err, ErrNoSuchIndex):
		status, kind = http.StatusNotFound, "index_not_found_exception"
	case errors.Is(err, ErrIndexExists):
		status, kind = http.StatusBadRequest, "resource_already_exists_exception"
	case errors.Is(err, ErrBadIndexName):
		status, kind = http.StatusBadRequest, "invalid_index_name_exception"
	case errors.Is(err, ErrClosed):
		status, kind = http.StatusServiceUnavailable, "cluster_block_exception"
	}

	writeJSON(w, status, map[string]any{
		"error":  map[string]string{"type": kind, "reason": err.Error()},
		"status": status,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	// The status line is already sent, so a failure here cannot be reported to
	// the client and there is nothing left to do about it.
	_ = json.NewEncoder(w).Encode(v)
}

// ------------------------------------------------------------------ handshake

func (s *Server) root(w http.ResponseWriter, _ *http.Request) error {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":         clusterName,
		"cluster_name": clusterName,
		"cluster_uuid": "weft-single-node",
		"tagline":      "The weft thread. One weft crosses and binds them all.",
		"version": map[string]any{
			"distribution":                        Distribution,
			"number":                              Version,
			"build_type":                          "tar",
			"build_flavor":                        "default",
			"build_snapshot":                      false,
			"lucene_version":                      "9.12.0",
			"minimum_wire_compatibility_version":  "7.10.0",
			"minimum_index_compatibility_version": "7.0.0",
		},
	})
	return nil
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) error {
	names := s.reg.Names()
	writeJSON(w, http.StatusOK, map[string]any{
		"cluster_name":                    clusterName,
		"status":                          "green",
		"timed_out":                       false,
		"number_of_nodes":                 1,
		"number_of_data_nodes":            1,
		"active_primary_shards":           len(names),
		"active_shards":                   len(names),
		"relocating_shards":               0,
		"initializing_shards":             0,
		"unassigned_shards":               0,
		"active_shards_percent_as_number": 100.0,
	})
	return nil
}

// ------------------------------------------------------------------- indexes

func (s *Server) createIndex(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("index")
	// The body is read and inspected rather than ignored: a client that sent
	// mappings has to get them refused, because accepting and dropping them
	// means a range query later matching nothing with nothing to report.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(body) > 0 {
		var settings map[string]json.RawMessage
		if err := json.Unmarshal(body, &settings); err != nil {
			return badRequest("parsing_exception", "the create-index body did not decode as JSON: %v", err)
		}
		for key := range settings {
			if key == "settings" {
				continue // shards and replicas have nowhere to land; accepted and ignored
			}
			return badRequest("illegal_argument_exception",
				"%q on create index is not supported yet: field mappings land in milestone 25, and accepting them "+
					"now would mean a range query silently matching nothing", key)
		}
	}

	if _, err := s.reg.Create(name); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"acknowledged":        true,
		"shards_acknowledged": true,
		"index":               name,
	})
	return nil
}

func (s *Server) headIndex(w http.ResponseWriter, r *http.Request) error {
	if _, ok := s.reg.Get(r.PathValue("index")); !ok {
		w.WriteHeader(http.StatusNotFound)
		return nil
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) deleteIndex(w http.ResponseWriter, r *http.Request) error {
	if err := s.reg.Drop(r.PathValue("index")); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"acknowledged": true})
	return nil
}

// ----------------------------------------------------------------- documents

func (s *Server) index(r *http.Request) (*Index, error) {
	name := r.PathValue("index")
	x, ok := s.reg.Get(name)
	if !ok {
		return nil, fmt.Errorf("no such index [%s]: %w", name, ErrNoSuchIndex)
	}
	return x, nil
}

func (s *Server) putDoc(w http.ResponseWriter, r *http.Request) error {
	x, err := s.index(r)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	if id == "" {
		return badRequest("illegal_argument_exception",
			"a document id is required: weft keys documents by the id you give them and generates none")
	}
	commit, apiErr := parseRefresh(r)
	if apiErr != nil {
		return apiErr
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	d, err := documentFrom(id, body)
	if err != nil {
		return badRequest("mapper_parsing_exception", "%v", err)
	}

	created, err := x.Put(d, body)
	if err != nil {
		return err
	}
	if commit {
		if err := x.Commit(r.Context()); err != nil {
			return err
		}
	}

	result, status := "updated", http.StatusOK
	if created {
		result, status = "created", http.StatusCreated
	}
	writeJSON(w, status, docResult{Index: x.Name(), ID: id, Version: 1, Result: result, Shards: oneShard})
	return nil
}

func (s *Server) getDoc(w http.ResponseWriter, r *http.Request) error {
	x, err := s.index(r)
	if err != nil {
		return err
	}
	id := r.PathValue("id")

	// The index decides existence, not the store: a deleted document leaves its
	// record on disk (docs/LIMITATIONS.md — deletion reclaims nothing) and only
	// Resolve knows it is gone.
	if _, live := x.Engine().Resolve(id); !live {
		writeJSON(w, http.StatusNotFound, getResult{Index: x.Name(), ID: id, Found: false})
		return nil
	}
	body, ok := x.Source().Get(id)
	if !ok {
		body = json.RawMessage(`{}`)
	}
	writeJSON(w, http.StatusOK, getResult{Index: x.Name(), ID: id, Version: 1, Found: true, Source: body})
	return nil
}

func (s *Server) deleteDoc(w http.ResponseWriter, r *http.Request) error {
	x, err := s.index(r)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	commit, apiErr := parseRefresh(r)
	if apiErr != nil {
		return apiErr
	}

	found, err := x.DeleteDoc(id)
	if err != nil {
		return err
	}
	if commit {
		if err := x.Commit(r.Context()); err != nil {
			return err
		}
	}

	result, status := "deleted", http.StatusOK
	if !found {
		result, status = "not_found", http.StatusNotFound
	}
	writeJSON(w, status, docResult{Index: x.Name(), ID: id, Version: 1, Result: result, Shards: oneShard})
	return nil
}

// parseRefresh reads ?refresh and reports whether the write should be committed.
//
// The word means something different on each side of this call, and the
// difference is published rather than papered over: in OpenSearch refresh makes a
// write *visible*, and in weft a write is visible the moment Add returns. What
// weft has that OpenSearch's refresh does not map onto is durability, so
// refresh=true is a Commit. That is the honest reading and also the expensive
// one — a commit holds the writer for as long as it takes, 11 seconds for 20,000
// documents (docs/LIMITATIONS.md).
//
// Called before the write, not after. Validating afterwards means a bad value
// returns 400 over a document that is already indexed, which is the one shape
// worse than either answer alone: the client is told the write failed and it did
// not.
func parseRefresh(r *http.Request) (commit bool, err *apiError) {
	switch v := r.URL.Query().Get("refresh"); v {
	case "", "false":
		return false, nil
	case "true", "wait_for":
		return true, nil
	default:
		return false, badRequest("illegal_argument_exception",
			"refresh takes true, false or wait_for, got %q", v)
	}
}

// -------------------------------------------------------------------- search

func (s *Server) search(w http.ResponseWriter, r *http.Request) error {
	start := time.Now()

	x, err := s.index(r)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}

	p, apiErr := parseSearch(x.Engine(), body)
	if apiErr != nil {
		return apiErr
	}
	hits, total, exact, err := runSearch(r.Context(), x, p)
	if err != nil {
		return fmt.Errorf("search %q: %w", x.Name(), err)
	}

	relation := "gte"
	if exact {
		relation = "eq"
	}
	var maxScore any // null, which is what OpenSearch sends when there are no hits
	if len(hits) > 0 {
		maxScore = hits[0].Score
	}
	if hits == nil {
		hits = []hit{} // an empty array, not null: clients index into it
	}

	// Measured rather than reported as zero. A constant here would put a lie in
	// every client's latency dashboard, which is the failure D-026 confines to
	// the handshake.
	writeJSON(w, http.StatusOK, map[string]any{
		"took":      time.Since(start).Milliseconds(),
		"timed_out": false,
		"_shards":   oneShard,
		"hits": map[string]any{
			"total":     map[string]any{"value": total, "relation": relation},
			"max_score": maxScore,
			"hits":      hits,
		},
	})
	return nil
}
