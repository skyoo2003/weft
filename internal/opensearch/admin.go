// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/query"
)

// This file is the half of engine a search request never touches: making writes
// durable, rewriting segments, verifying a directory, and reading the index's own
// bookkeeping.
//
// Five of these carry OpenSearch names — _flush, _refresh, _forcemerge, _count,
// _analyze — because what weft does under them is honestly what those names
// mean. The rest sit under /_weft/, and the prefix is the decision weft_graph
// already made: OpenSearch has no route that walks a term space or parses weft's
// query string, so inventing an OpenSearch spelling for one would be a second
// untruth where D-026 allows exactly one.

// defaultTermLimit bounds a term walk that did not ask for a size. The term
// space of a real corpus is larger than anything a client wants in one response.
const defaultTermLimit = 100

// flush makes everything written so far durable.
//
// _refresh is routed here too. In weft a search already reads the live index, so
// the only thing "refresh" can mean is the commit — which is exactly what
// refresh=true on a write already does.
func (s *Server) flush(w http.ResponseWriter, r *http.Request) error {
	x, err := s.index(r)
	if err != nil {
		return err
	}
	if err := x.Commit(r.Context()); err != nil {
		return fmt.Errorf("flush %q: %w", x.Name(), err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"_shards": oneShard})
	return nil
}

// forceMerge rewrites the segments and commits the result.
func (s *Server) forceMerge(w http.ResponseWriter, r *http.Request) error {
	x, err := s.index(r)
	if err != nil {
		return err
	}
	// engine.Merge takes no target. Accepting max_num_segments and merging to
	// whatever it merges to would leave a client setting a number that is read,
	// ignored, and never mentioned again.
	if v := r.URL.Query().Get("max_num_segments"); v != "" {
		return badRequest(kindIllegalArgument,
			"max_num_segments is not supported: engine.Merge rewrites the segment set and takes no target, "+
				"so a number here would be accepted and ignored. Drop it, got %q", v)
	}
	if err := x.Merge(r.Context()); err != nil {
		return fmt.Errorf("forcemerge %q: %w", x.Name(), err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"_shards": oneShard})
	return nil
}

// count is the live document population.
func (s *Server) count(w http.ResponseWriter, r *http.Request) error {
	x, err := s.index(r)
	if err != nil {
		return err
	}
	body, err := readAll(r)
	if err != nil {
		return err
	}
	// OpenSearch counts a query's matches. weft's search is a top-k fusion, and
	// there is no k-independent count of what a fused ranking would have held —
	// so a query here has no answer, and returning the corpus size instead would
	// be a number shaped like one.
	if len(strings.TrimSpace(string(body))) > 0 {
		return badRequest(kindIllegalArgument,
			"_count does not take a query here: a fused ranking is a top-k, so the number of documents it "+
				"would have contained is not defined without a k. Run _search and read hits.total, which "+
				"says whether it is exact")
	}

	docs, _ := x.Engine().Stats()
	writeJSON(w, http.StatusOK, map[string]any{"count": docs, "_shards": oneShard})
	return nil
}

// analyze tokenizes text with the index's own tokenizer.
func (s *Server) analyze(w http.ResponseWriter, r *http.Request) error {
	x, err := s.index(r)
	if err != nil {
		return err
	}
	body, err := readAll(r)
	if err != nil {
		return err
	}
	var req struct {
		Text      string          `json:"text"`
		Analyzer  string          `json:"analyzer"`
		Tokenizer json.RawMessage `json:"tokenizer"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return badRequest("parsing_exception", "the _analyze body did not decode as JSON: %v", err)
		}
	}
	// One index has one tokenizer, fixed at the commit that wrote it (D-022). A
	// named analyzer is a request this server cannot honour, and honouring it
	// approximately would report terms the index does not hold.
	if req.Analyzer != "" || len(req.Tokenizer) > 0 {
		return badRequest(kindIllegalArgument,
			"a named analyzer or tokenizer is not supported: an index has one tokenizer, fixed at the commit "+
				"that wrote it, and analyzing with another would report terms this index does not hold. "+
				"Omit it to use the index's own")
	}

	tokens := x.Engine().Tokenize(req.Text)
	out := make([]map[string]any, 0, len(tokens))
	for i, t := range tokens {
		out = append(out, map[string]any{"token": t, "position": i})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
	return nil
}

// weftTerms walks the term space from a prefix.
func (s *Server) weftTerms(w http.ResponseWriter, r *http.Request) error {
	x, err := s.index(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	limit, apiErr := intParam(q.Get("limit"), defaultTermLimit)
	if apiErr != nil {
		return apiErr
	}

	// engine.FieldTerm is the spelling a client cannot produce on its own: a
	// field's terms are ordinary postings under a name, and the name is the
	// library's to write.
	prefix := q.Get("prefix")
	if field := q.Get("field"); field != "" {
		prefix = engine.FieldTerm(field, prefix)
	}

	ix := x.Engine()
	found := ix.Terms(prefix, limit)
	out := make([]map[string]any, 0, len(found))
	// One buffer across the whole walk, which is what LookupInto exists for
	// beside Lookup.
	var buf []engine.Posting
	for _, t := range found {
		buf = ix.LookupInto(t, buf[:0])
		out = append(out, map[string]any{"term": displayTerm(t), "doc_count": len(buf)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"terms": out})
	return nil
}

// weftPostings is one term's postings, with the ids resolved back to keys.
func (s *Server) weftPostings(w http.ResponseWriter, r *http.Request) error {
	x, err := s.index(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	term := q.Get("term")
	if term == "" {
		return badRequest(kindIllegalArgument,
			"term is required: without one this route would report the postings of the empty string, "+
				"which is not a term any document holds")
	}
	limit, apiErr := intParam(q.Get("limit"), defaultTermLimit)
	if apiErr != nil {
		return apiErr
	}
	if field := q.Get("field"); field != "" {
		term = engine.FieldTerm(field, term)
	}

	ix := x.Engine()
	// PostingCount first because it is the cheap answer, and it is the whole
	// answer to "is this term in the index at all".
	n := ix.PostingCount(term)
	postings := ix.Lookup(term)
	out := make([]map[string]any, 0, min(len(postings), limit))
	for i, p := range postings {
		if i >= limit {
			break
		}
		d, ok := ix.Doc(p.Doc)
		if !ok {
			continue // deleted between the lookup and here
		}
		out = append(out, map[string]any{"_id": d.Key, "freq": p.Freq})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"term": displayTerm(term), "doc_count": n, "postings": out,
	})
	return nil
}

// weftScrub verifies the committed directory.
func (s *Server) weftScrub(w http.ResponseWriter, r *http.Request) error {
	x, err := s.index(r)
	if err != nil {
		return err
	}
	if err := x.Scrub(); err != nil {
		// Damage is a 500 rather than a 400: nothing about the request was wrong,
		// and a client reading this needs it to be loud.
		return fmt.Errorf("scrub %q: %w", x.Name(), err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"scrubbed": x.Name()})
	return nil
}

// weftQuery runs weft's own query string.
//
// query_string stays a 501 and this is not a contradiction. That refusal is
// about reading Lucene's language as if it were this one — the two disagree
// about +, OR and parentheses, so translating silently runs a different query.
// Here a client has asked for weft's language by name, on a route OpenSearch
// does not have.
func (s *Server) weftQuery(w http.ResponseWriter, r *http.Request) error {
	x, err := s.index(r)
	if err != nil {
		return err
	}
	body, err := readAll(r)
	if err != nil {
		return err
	}
	var req struct {
		Q    string `json:"q"`
		Size *int   `json:"size"`
		From *int   `json:"from"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return badRequest("parsing_exception", "the body did not decode as JSON: %v", err)
		}
	}
	if req.Q == "" {
		return badRequest(kindIllegalArgument,
			`"q" is required and holds a weft query string; pkg/query.Parse documents the syntax`)
	}

	p := plan{size: defaultSize}
	if req.Size != nil {
		p.size = *req.Size
	}
	if req.From != nil {
		p.from = *req.From
	}
	if p.size < 0 || p.from < 0 || p.from+p.size > maxResultWindow {
		return badRequest(kindIllegalArgument,
			"from %d plus size %d is outside the result window of %d", p.from, p.size, maxResultWindow)
	}

	parsed, err := query.Parse(x.Engine(), req.Q)
	if err != nil {
		// pkg/query's own words. It refuses a malformed query rather than
		// answering it with nothing, and this route is not the place to soften
		// that into an empty ranking.
		return badRequest(kindIllegalArgument, "%v", err)
	}
	p.scorers = parsed.Scorers
	// Parse's own Fuser rather than the plan's. The plan's is bool's nesting and
	// this query has no bool: its constraints are in the query string, and the
	// Fuser that carries them is the one Parse hands back.
	p.fuse = parsed.Fuse(engine.Fuser(fusion.Fuse))

	hits, total, exact, err := runSearch(r.Context(), x, p)
	if err != nil {
		return fmt.Errorf("search %q: %w", x.Name(), err)
	}
	writeHits(w, hits, total, exact, 0)
	return nil
}

// intParam reads a positive integer query parameter, or its default.
func intParam(raw string, fallback int) (int, *apiError) {
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, badRequest(kindIllegalArgument, "limit is a positive integer, got %q", raw)
	}
	return n, nil
}

// displayTerm strips a field prefix back off, so a field walk reads as that
// field's own terms rather than as the encoding they are stored under.
func displayTerm(term string) string {
	if i := strings.LastIndexByte(term, 0); i >= 0 {
		return term[i+1:]
	}
	return term
}
