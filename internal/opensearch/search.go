// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/query"
)

// textField is the one JSON key that maps to engine.Document.Text rather than to
// a named field.
//
// weft's index has two term spaces and only two: Document.Text, unnamed, and
// Document.Fields, each name its own space (milestone 18 — a field is a term
// space, not a section). A body arriving over HTTP has neither distinction in
// it, so the server needs one rule, and this is it. `match` on "text" therefore
// searches the unnamed space and `match` on anything else searches that field's.
const textField = "text"

// defaultSize is OpenSearch's, so a client that sends no size gets the number of
// hits it would get from OpenSearch.
const defaultSize = 10

// maxResultWindow caps size, and it is a security boundary rather than a
// preference.
//
// engine.NewCollector does make([]Candidate, 0, k) with the k engine.Search was
// handed, so a size read straight out of a request body is a remote allocation
// primitive: {"size": 2000000000} asks this process for roughly 32 GB before a
// single document is scored. CodeQL's go/uncontrolled-allocation-size found that
// path the day the HTTP surface created it — the allocation is in pkg/engine and
// unchanged, and what was new is a caller who does not own the number.
//
// The cap lives here rather than in pkg/ because it is a policy about requests
// and not about the library: an embedder passing its own k is not a threat to
// itself. 10,000 is OpenSearch's index.max_result_window, so a client that
// already handles that error handles this one.
const maxResultWindow = 10000

// searchRequest is the part of the search DSL this server reads.
//
// Every field here exists to be *refused* except Query and Size. That is not
// waste: a body with `aggs` in it has to produce a 501, and a struct that did not
// name the field would silently drop it and answer 200 — which is the failure
// D-026 exists to prevent, and the one pkg/query's documentation calls the
// failure this project refuses everywhere else.
type searchRequest struct {
	Query        map[string]json.RawMessage `json:"query"`
	Size         *int                       `json:"size"`
	From         *int                       `json:"from"`
	Sort         json.RawMessage            `json:"sort"`
	Aggs         json.RawMessage            `json:"aggs"`
	Aggregations json.RawMessage            `json:"aggregations"`
	Highlight    json.RawMessage            `json:"highlight"`
	SearchAfter  json.RawMessage            `json:"search_after"`
	Collapse     json.RawMessage            `json:"collapse"`
}

// plan is what one search body resolves to: either every live document, or a set
// of streams for engine.Search to fuse.
//
// matchAll is a flag rather than a scorer because weft has no scorer that
// nominates every document — Glob("*") would come close and would silently stop
// at query.MaxTerms, which is a wrong answer on any corpus with more than 4096
// distinct terms.
type plan struct {
	matchAll bool
	scorers  []engine.Scorer
	size     int
}

// milestone25 names the query types that map onto pkg/query cleanly and are not
// built yet. They get a 400 that says so rather than a 501, because 501 should
// mean "not in this engine" and these are "not in this milestone".
var milestone25 = []string{
	"bool", "term", "terms", "prefix", "wildcard", "range", "fuzzy",
	"match_phrase", "match_phrase_prefix", "multi_match", "query_string",
	"simple_query_string", "ids", "exists",
}

// milestone26 is the shapes the milestone after that is about.
var milestone26 = []string{"knn", "hybrid", "neural", "function_score", "script_score"}

// parseSearch turns a request body into a plan, or into the error the client
// should see instead.
func parseSearch(ix *engine.Index, raw []byte) (plan, *apiError) {
	var req searchRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return plan{}, badRequest("parsing_exception", "the search body did not decode as JSON: %v", err)
		}
	}

	// Refusals first, and before anything is searched, so a body that asks for
	// two things this server cannot do is refused for the first rather than
	// answered for neither.
	for _, r := range []struct {
		present bool
		status  int
		reason  string
	}{
		{req.Aggs != nil || req.Aggregations != nil, http.StatusNotImplemented,
			"aggregations are not implemented: weft's index carries a term's document and frequency and no doc-values column to aggregate over"},
		{req.Highlight != nil, http.StatusNotImplemented,
			"highlighting is not implemented: a Posting carries a frequency and not a position, so no " +
				"offset in the source text can be decided from the index (docs/FORMAT.md section 8)"},
		{req.SearchAfter != nil, http.StatusNotImplemented,
			"search_after is not implemented: there is no sort order to resume from, because results come back by fused score only"},
		{req.Collapse != nil, http.StatusNotImplemented,
			"collapse is not implemented"},
		{req.Sort != nil, http.StatusBadRequest,
			"sort is not supported: results come back by fused score and there is nothing else to order them by. Remove the sort clause"},
		{req.From != nil && *req.From != 0, http.StatusBadRequest,
			"from is not supported yet: paging is milestone 25. Raise size instead"},
	} {
		if r.present {
			return plan{}, &apiError{status: r.status, kind: "illegal_argument_exception", reason: r.reason}
		}
	}

	p := plan{size: defaultSize}
	if req.Size != nil {
		if *req.Size < 0 {
			return plan{}, badRequest("illegal_argument_exception", "size must not be negative, got %d", *req.Size)
		}
		if *req.Size > maxResultWindow {
			return plan{}, badRequest("illegal_argument_exception",
				"size %d is over the result window of %d: the top-k collector allocates for the size it is "+
					"given, so this is refused before it is allocated rather than after. Page with a narrower "+
					"query, or raise the window in a build you control",
				*req.Size, maxResultWindow)
		}
		p.size = *req.Size
	}

	if len(req.Query) == 0 {
		p.matchAll = true
		return p, nil
	}
	if len(req.Query) > 1 {
		names := make([]string, 0, len(req.Query))
		for name := range req.Query {
			names = append(names, name)
		}
		slices.Sort(names)
		return plan{}, badRequest("parsing_exception",
			"a query holds exactly one clause and this one holds %d (%v); wrapping them in a bool query is milestone 25",
			len(names), names)
	}

	for name, body := range req.Query {
		switch {
		case name == "match_all":
			p.matchAll = true
			return p, nil
		case name == "match":
			scorers, err := matchScorers(ix, body)
			if err != nil {
				return plan{}, err
			}
			p.scorers = scorers
			return p, nil
		case slices.Contains(milestone25, name):
			return plan{}, badRequest("parsing_exception",
				"the %q query is not implemented yet: milestone 24 speaks match and match_all only, and %q lands in milestone 25", name, name)
		case slices.Contains(milestone26, name):
			return plan{}, badRequest("parsing_exception",
				"the %q query is not implemented yet: vector and hybrid queries land in milestone 26", name)
		default:
			return plan{}, badRequest("parsing_exception", "no query registered for %q", name)
		}
	}
	return plan{}, badRequest("parsing_exception", "the query clause is empty")
}

// matchScorers turns one match clause into one stream per token.
//
// This is the whole of the mapping and it needs no code in pkg/: rank fusion is
// a union of votes, which is what an OR-ed match already means, so a token is a
// stream and the default operator is the fuser doing nothing special. An
// operator of "and" would be query.Must over every stream — milestone 25, because
// it wants the bool query beside it to be worth anything.
func matchScorers(ix *engine.Index, body json.RawMessage) ([]engine.Scorer, *apiError) {
	var clause map[string]json.RawMessage
	if err := json.Unmarshal(body, &clause); err != nil {
		return nil, badRequest("parsing_exception", "the match clause did not decode as an object: %v", err)
	}
	if len(clause) != 1 {
		return nil, badRequest("parsing_exception", "a match clause names exactly one field and this one names %d", len(clause))
	}

	for name, value := range clause {
		text, err := matchText(name, value)
		if err != nil {
			return nil, err
		}
		// The index's own tokenizer, not this package's idea of one. D-023 is
		// the reason: a query tokenized differently from the documents finds
		// nothing and has nothing to report.
		tokens := ix.Tokenize(text)
		if len(tokens) == 0 {
			return nil, nil
		}
		field := name
		if name == textField {
			field = ""
		}
		scorers := make([]engine.Scorer, 0, len(tokens))
		for _, tok := range tokens {
			// Glob rather than a term scorer, because a pattern with no
			// metacharacter matches itself and nothing else — one clause kind
			// fewer, exactly as pkg/query.Parse argues.
			scorers = append(scorers, query.Glob(ix, field, tok))
		}
		return scorers, nil
	}
	return nil, badRequest("parsing_exception", "the match clause is empty")
}

// matchText reads either the short form ("field": "some words") or the long form
// ("field": {"query": "some words"}), and refuses every option on the long form
// rather than ignoring it.
func matchText(field string, value json.RawMessage) (string, *apiError) {
	var text string
	if err := json.Unmarshal(value, &text); err == nil {
		return text, nil
	}

	var long map[string]json.RawMessage
	if err := json.Unmarshal(value, &long); err != nil {
		return "", badRequest("parsing_exception",
			"match on %q takes a string or an object with a query field, got %s", field, value)
	}
	q, ok := long["query"]
	if !ok {
		return "", badRequest("parsing_exception", "match on %q has no query field", field)
	}
	if err := json.Unmarshal(q, &text); err != nil {
		return "", badRequest("parsing_exception", "the query field of match on %q is not a string", field)
	}
	for opt := range long {
		if opt == "query" {
			continue
		}
		// Named rather than ignored. `operator: and` silently read as `or`
		// returns more documents than the client asked for and looks like a
		// working search.
		return "", badRequest("parsing_exception",
			"match option %q is not supported: milestone 24 reads the query text and nothing else", opt)
	}
	return text, nil
}

// runSearch executes a plan and reports the hits, the total, and whether that
// total is exact.
func runSearch(ctx context.Context, x *Index, p plan) (hits []hit, total int, exact bool, err error) {
	ix := x.Engine()

	// Bounded here as well as refused in parseSearch, and the repetition is the
	// point. parseSearch answers the client; this answers the allocator.
	// engine.NewCollector does make([]Candidate, 0, k) with whatever k it is
	// handed, so the last place that can keep a request from choosing this
	// process's heap is the frame that makes the call — not one three frames up,
	// on the other side of a struct field. A reader checking this line should not
	// have to go and find the guard, and neither should an analyser.
	size := p.size
	if size > maxResultWindow {
		size = maxResultWindow
	}

	if p.matchAll {
		hits, total = matchAllHits(x, size)
		return hits, total, true, nil
	}
	if len(p.scorers) == 0 {
		// A match whose text tokenized to nothing. Zero hits is the right
		// answer and it is not a silent one: the client asked for nothing.
		return nil, 0, true, nil
	}

	cands, err := engine.Search(ctx, engine.Query{}, size, fusion.Fuse, p.scorers...)
	if err != nil {
		return nil, 0, false, err
	}
	hits = make([]hit, 0, len(cands))
	for _, c := range cands {
		d, ok := ix.Doc(c.Doc)
		if !ok {
			continue
		}
		hits = append(hits, newHit(x, d.Key, c.Score))
	}
	// Exact only when the cut did not bind. Search returns the top k, so a full
	// page is a lower bound on how many documents matched, and calling that "eq"
	// would be a number this server cannot stand behind.
	return hits, len(hits), len(cands) < size, nil
}

// matchAllHits walks the id space.
//
// ponytail: a linear scan over Len(), which is one past the highest DocID and
// counts tombstones with it, so this is O(corpus) per match_all query. The
// upgrade path is an iterator on engine, and the trigger is match_all appearing
// in anything but a smoke test. It is here because "search this index" with no
// body is the first request every human makes, and a 400 for it would be a worse
// answer than a slow one.
func matchAllHits(x *Index, size int) (hits []hit, total int) {
	ix := x.Engine()
	total, _ = ix.Stats()
	if size <= 0 {
		return nil, total
	}
	for i := range ix.Len() {
		d, ok := ix.Doc(engine.DocID(i))
		if !ok {
			continue // a tombstone
		}
		hits = append(hits, newHit(x, d.Key, 1.0))
		if len(hits) == size {
			break
		}
	}
	return hits, total
}

// scalarText renders a JSON scalar as the text weft will tokenize for it.
//
// Strings are themselves. Numbers and booleans become their literal, so
// {"views": 42} is findable as the term "42" — not a numeric index, and
// deliberately not pretending to be one: docs/LIMITATIONS.md prices that, and
// query.EncodeInt with a mapping to drive it is milestone 25. Anything else —
// an object, an array, null — is kept in _source and not indexed, which is what
// OpenSearch spells "index": false.
func scalarText(raw json.RawMessage) (string, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	default:
		return "", false
	}
}

// documentFrom builds the engine.Document for one JSON body.
//
// Fields are added in sorted key order so two identical bodies produce identical
// documents: engine.Document.Fields is a slice, and a map's iteration order would
// otherwise make the index depend on nothing.
func documentFrom(id string, body json.RawMessage) (engine.Document, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return engine.Document{}, fmt.Errorf("the document body did not decode as a JSON object: %w", err)
	}

	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	slices.Sort(names)

	d := engine.Document{Key: id}
	for _, name := range names {
		text, ok := scalarText(raw[name])
		if !ok {
			continue // kept in _source, not indexed
		}
		if name == textField {
			d.Text = text
			continue
		}
		d.Fields = append(d.Fields, engine.Field{Name: name, Text: text})
	}
	return d, nil
}
