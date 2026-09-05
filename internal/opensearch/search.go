// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/query"
	"github.com/skyoo2003/weft/pkg/scorer/recency"
	"github.com/skyoo2003/weft/pkg/scorer/vector"
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
//
// from is capped against the same allocation and by the same number, because
// paging here is `k = from + size` and then a slice: a deep page costs what the
// whole prefix before it costs.
const maxResultWindow = 10000

// searchRequest is the part of the search DSL this server reads.
//
// Every field here exists to be *refused* except Query, Size and From. That is
// not waste: a body with `aggs` in it has to produce a 501, and a struct that did
// not name the field would silently drop it and answer 200 — which is the failure
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
	PIT          json.RawMessage            `json:"pit"`
}

// plan is what one search body resolves to: either every live document, or a set
// of streams plus the constraints between them.
//
// matchAll is a flag rather than a scorer because weft has no scorer that
// nominates every document — Glob("*") would come close and would silently stop
// at query.MaxTerms, which is a wrong answer on any corpus with more than 4096
// distinct terms.
//
// must, mustNot and filter are positions in scorers, which is pkg/query's
// convention and the reason none of this needed code in pkg/: a constraint is an
// index into a list the caller already fixed, so the fuser never learns what a
// scorer is.
type plan struct {
	matchAll bool
	scorers  []engine.Scorer

	must, mustNot, filter []int

	// weights is one per scorer, or nil for an unweighted fusion.
	//
	// Positional, which is fusion.FuseWeighted's convention and the reason a
	// weight can exist at all without fusion learning what a scorer is: the
	// caller fixed the order, so a weight is an index into a list it already
	// wrote. docs/DECISIONS.md D-005 is why weighted fusion exists.
	weights []float64

	// q carries the query-time input a scorer reads — the vector for a knn
	// clause. engine.Query is closed (D-021), so a signal needing a per-query
	// input either finds a field here or is built into its scorer's constructor;
	// both are places that already exist.
	q engine.Query

	from, size int

	// depth overrides from+size when a clause asks every stream for more
	// candidates than the page needs. A knn clause's k is the one that does.
	depth int

	// fuse replaces everything fuser() would have assembled, and only one caller
	// sets it: /_weft/query, whose constraints were fixed by pkg/query.Parse over
	// the very stream list it produced. nil everywhere else.
	fuse engine.Fuser
}

// fuser assembles this plan's constraints over fusion.Fuse.
//
// The nesting order is the whole of bool's semantics and it is not arbitrary:
//
//	Must( blank( MustNot( Fuse ) ) )
//
// Must runs first and sees every stream at its original position, so a `filter`
// stream is still there to intersect with. blank then empties the filters — after
// they have narrowed and before anything is ranked — which is what makes a filter
// narrow without voting. query.Must and query.MustNot each hand on a slice of the
// same length and order, so the positions stay meaningful the whole way down;
// MustNot is innermost because it is the only one that removes streams.
//
// A weight of 0 in fusion.FuseWeighted looks like the shorter spelling of blank
// and is not: 0 removes the document from the fused result entirely, so a filter
// written that way excludes everything it was meant to keep. PRD section 4
// registers that trap in the bool.filter row.
//
// Weights are positional over the *original* stream list, and MustNot is the one
// wrapper that hands on a shorter one. They cannot meet: weights are only set by a
// hybrid clause, which is a whole query rather than a clause of a bool, so a plan
// carrying weights has no must_not positions. TestHybridWeightsAndMustNotDoNotMeet
// is what holds that rather than this comment.
func (p plan) fuser() engine.Fuser {
	// A route that brought its own Fuser wins outright. /_weft/query is the one
	// that does: pkg/query.Parse hands back the constraints as a Fuser over the
	// positions it just fixed, and rebuilding those from must and mustNot here
	// would be this file holding a second opinion about a query it did not parse.
	if p.fuse != nil {
		return p.fuse
	}
	base := engine.Fuser(fusion.Fuse)
	if p.weights != nil {
		base = fusion.FuseWeighted(p.weights...)
	}
	inner := query.MustNot(base, p.mustNot...)
	// Only when something else is voting. A bool holding nothing but filters has
	// no other ranking, and blanking there would answer nothing to a query that
	// named documents.
	if len(p.filter) > 0 && len(p.filter) < len(p.scorers) {
		inner = blank(inner, p.filter)
	}
	return query.Must(inner, p.must...)
}

// blank hands fuse the streams with the named ones emptied.
//
// The slice is cloned rather than mutated: a Fuser is handed engine.Search's own
// slice, and clearing an entry of it would take the stream away from whatever
// looks at it next.
func blank(fuse engine.Fuser, streams []int) engine.Fuser {
	return func(all [][]engine.Candidate, k int) []engine.Candidate {
		out := slices.Clone(all)
		for _, s := range streams {
			if s >= 0 && s < len(out) {
				out[s] = nil
			}
		}
		return fuse(out, k)
	}
}

// The words this file reads out of a request body more than once.
//
// Spelled as constants because each of them is both a key the client wrote and a
// key this file looks up, and the two have to agree: a `gte` that is checked for
// under one spelling and read under another silently drops the bound and answers
// a wider range than the one asked for.
const (
	optValue = "value"

	boundGTE = "gte"
	boundGT  = "gt"
	boundLTE = "lte"
	boundLT  = "lt"

	// The clause names this file both dispatches on and quotes back in an error.
	// Two spellings of one name is how a clause comes to be routed under one and
	// refused under the other.
	clauseBool     = "bool"
	clauseMatchAll = "match_all"
	clauseHybrid   = "hybrid"
	clauseKnn      = "knn"
	clausePrefix   = "prefix"
	clauseWildcard = "wildcard"

	occMust    = "must"
	occMustNot = "must_not"
	occShould  = "should"
	occFilter  = "filter"
)

// notThisEngine is the query shapes that ask for a score this engine does not
// compute: an expression evaluated per document, whose result is then compared
// with a BM25 score. Rank fusion never compares scores across streams, so there is
// no place to put the answer.
var notThisEngine = []string{"script_score", "neural", "neural_sparse", "rank_feature", "distance_feature"}

// notImplemented names the queries this engine has no way to express, with the
// reason each one cannot be built rather than a date it might be.
//
// A 501 here and a 400 elsewhere is the distinction D-026 asks for: 400 is "this
// server will not", 501 is "this engine cannot". Both are refusals and neither is
// a 200 with an empty hit list.
var notImplemented = map[string]string{
	"ids": "an ids query is not implemented: document keys are not a term space in this index — engine.Index.Resolve " +
		"reaches one key at a time and no scorer nominates a set of them. Fetch them through _doc",
	"query_string": "query_string is not implemented: weft has a query string of its own (pkg/query.Parse) and it is " +
		"not Lucene's — they disagree about +, OR and parentheses — so reading this one would run a query other " +
		"than the one written. Build the clauses as a bool query",
	"simple_query_string": "simple_query_string is not implemented, for the reason query_string is not: weft's own " +
		"query string is a different language, and translating between them silently changes the query",
	"match_phrase_prefix": "match_phrase_prefix is not implemented: a phrase is decided by re-reading the document " +
		"text, because a Posting carries a frequency and not a position, and there is no way to re-read a " +
		"prefix that has not been resolved to terms yet",
	"more_like_this": "more_like_this is not implemented: it needs a term vector per document, and the index stores " +
		"postings per term",
}

// parseSearch turns a request body into a plan, or into the error the client
// should see instead.
func parseSearch(x *Index, raw []byte) (plan, *apiError) {
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
		{req.PIT != nil, http.StatusNotImplemented,
			"point-in-time is not implemented: a commit replaces the segment set under every reader, and there " +
				"is no way to hold the previous one open"},
		{req.Collapse != nil, http.StatusNotImplemented,
			"collapse is not implemented"},
		{req.Sort != nil, http.StatusBadRequest,
			"sort is not supported: results come back by fused score and there is nothing else to order them by. Remove the sort clause"},
	} {
		if r.present {
			return plan{}, &apiError{status: r.status, kind: kindIllegalArgument, reason: r.reason}
		}
	}

	p := plan{size: defaultSize}
	if err := readWindow(&p, req); err != nil {
		return plan{}, err
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
			"a query holds exactly one clause and this one holds %d (%v); combine them in a bool query",
			len(names), names)
	}

	c := &compiler{ix: x.Engine(), m: x.Mapping(), p: &p}
	for name, body := range req.Query {
		if name == clauseMatchAll {
			p.matchAll = true
			return p, nil
		}
		if name == clauseBool {
			if err := c.boolQuery(body); err != nil {
				return plan{}, err
			}
			return p, nil
		}
		if name == clauseHybrid {
			if err := c.hybridQuery(body); err != nil {
				return plan{}, err
			}
			return p, nil
		}
		cl, err := c.clause(name, body)
		if err != nil {
			return plan{}, err
		}
		at := c.add(cl.scorers...)
		if cl.requireAll {
			// `operator: and`, or a term whose value tokenized to more than one
			// token. Every stream is required, which is query.Must over all of
			// their positions and no new fusion.
			p.must = append(p.must, at...)
		}
	}
	return p, nil
}

// readWindow reads from and size, and refuses a window that would allocate the
// heap before a document is scored.
//
// Split out of parseSearch and not inlined at the call site: it is the only part
// of parsing a search body that is about *this process* rather than about the
// query, and it is the part CodeQL had an opinion on.
func readWindow(p *plan, req searchRequest) *apiError {
	if req.Size != nil {
		if *req.Size < 0 {
			return badRequest(kindIllegalArgument, "size must not be negative, got %d", *req.Size)
		}
		if *req.Size > maxResultWindow {
			return badRequest(kindIllegalArgument,
				"size %d is over the result window of %d: the top-k collector allocates for the size it is "+
					"given, so this is refused before it is allocated rather than after. Page with a narrower "+
					"query, or raise the window in a build you control",
				*req.Size, maxResultWindow)
		}
		p.size = *req.Size
	}
	if req.From != nil {
		if *req.From < 0 {
			return badRequest(kindIllegalArgument, "from must not be negative, got %d", *req.From)
		}
		if *req.From+p.size > maxResultWindow {
			return badRequest(kindIllegalArgument,
				"from %d plus size %d is over the result window of %d: a page here is the whole prefix "+
					"fetched and then cut, so a deep page costs what every page before it costs. This is "+
					"refused rather than allocated — narrow the query",
				*req.From, p.size, maxResultWindow)
		}
		p.from = *req.From
	}
	return nil
}

// compiler accumulates one search body's streams and constraints.
type compiler struct {
	ix *engine.Index
	m  *Mapping
	p  *plan
}

// add appends scorers to the plan and returns the positions they took.
func (c *compiler) add(scorers ...engine.Scorer) []int {
	at := make([]int, 0, len(scorers))
	for _, s := range scorers {
		at = append(at, len(c.p.scorers))
		c.p.scorers = append(c.p.scorers, s)
	}
	return at
}

// compiled is one query clause: the streams it becomes, and whether all of them
// have to match for the clause to be satisfied.
//
// requireAll is the difference between `operator: or` and `operator: and`, and
// also what a multi-token `term` means. It is carried out of the clause rather
// than acted on inside it because only the caller knows whether the clause is
// being ranked (top level, `should`) or required (`must`, `filter`).
type compiled struct {
	scorers    []engine.Scorer
	requireAll bool
}

// clause builds one query clause. A nested bool is refused here, so everything
// that gets past this function is a leaf.
func (c *compiler) clause(name string, body json.RawMessage) (compiled, *apiError) {
	switch name {
	case "match":
		return c.match(body)
	case "match_phrase":
		return c.matchPhrase(body)
	case "term":
		return c.term(body)
	case "terms":
		return c.terms(body)
	case clausePrefix, clauseWildcard:
		return c.pattern(body, name)
	case "fuzzy":
		return c.fuzzy(body)
	case "range":
		return c.rangeQuery(body)
	case "exists":
		return c.exists(body)
	case clauseKnn:
		return c.knn(body)
	case clauseWeftGraph:
		return c.weftGraph(body)
	case "function_score":
		return c.functionScore(body)
	case clauseHybrid:
		return compiled{}, badRequest("parsing_exception",
			"a hybrid inside another clause is not supported: hybrid *is* the fusion of the whole query, so "+
				"nesting one would be asking for a fusion of fusions. Put it at the top level")
	case clauseBool:
		return compiled{}, badRequest("parsing_exception",
			"a bool inside a bool is not supported: there is no boolean algebra here — a query is a flat list "+
				"of streams with an intersection and a difference over it, which is pkg/query's argument, and "+
				"nesting would need a query tree and an evaluator over it. That is a different engine. "+
				"Flatten the clauses into the outer bool")
	case clauseMatchAll:
		return compiled{}, badRequest("parsing_exception",
			"match_all is a whole query and not a clause: inside a bool it would be a stream naming every "+
				"document, and no scorer here does that — a Glob over everything stops at query.MaxTerms. "+
				"Drop it; a bool with only filters already matches on those alone")
	}
	if reason, ok := notImplemented[name]; ok {
		return compiled{}, &apiError{status: http.StatusNotImplemented, kind: kindIllegalArgument, reason: reason}
	}
	if slices.Contains(notThisEngine, name) {
		return compiled{}, badRequest("parsing_exception",
			"the %q query is not supported: it scores by evaluating an expression per document, and fusion "+
				"ranks by position — engine.Candidate.Score is not comparable across streams, so there is "+
				"nothing for a computed score to be compared against. A signal that needs its own scoring "+
				"is a scorer, and a scorer is a stream: name it in a hybrid query", name)
	}
	return compiled{}, badRequest("parsing_exception", "no query registered for %q", name)
}

// oneField reads the `{"field": <value>}` shape every leaf clause has.
func oneField(kind string, body json.RawMessage) (field string, value json.RawMessage, err *apiError) {
	var clause map[string]json.RawMessage
	if e := json.Unmarshal(body, &clause); e != nil {
		return "", nil, badRequest("parsing_exception", "the %s clause did not decode as an object: %v", kind, e)
	}
	if len(clause) != 1 {
		return "", nil, badRequest("parsing_exception",
			"a %s clause names exactly one field and this one names %d", kind, len(clause))
	}
	for name, v := range clause {
		return name, v, nil
	}
	return "", nil, badRequest("parsing_exception", "the %s clause is empty", kind)
}

// termSpace is the index term space a JSON field name refers to. Document.Text is
// unnamed, which is what engine.FieldTerm spells as the empty string.
func termSpace(field string) string {
	if field == textField {
		return ""
	}
	return field
}

func numericKind(kind string) bool {
	return kind == typeInteger || kind == typeLong || kind == typeDate
}

// --------------------------------------------------------------------- match

// match turns one match clause into one stream per token.
//
// This is the whole of the mapping for the commonest query there is, and it needs
// no code in pkg/: rank fusion is a union of votes, which is what an OR-ed match
// already means, so a token is a stream and the default operator is the fuser
// doing nothing special. `operator: and` is those same streams with every
// position required, which is query.Must — still no new fusion, only a constraint
// over positions the caller fixed.
func (c *compiler) match(body json.RawMessage) (compiled, *apiError) {
	field, value, err := oneField("match", body)
	if err != nil {
		return compiled{}, err
	}
	text, opts, err := matchText(field, value)
	if err != nil {
		return compiled{}, err
	}

	requireAll := false
	if raw, ok := opts["operator"]; ok {
		var op string
		if e := json.Unmarshal(raw, &op); e != nil {
			return compiled{}, badRequest("parsing_exception", "operator is a string, got %s", raw)
		}
		switch op {
		case "or", "OR":
		case "and", "AND":
			requireAll = true
		default:
			return compiled{}, badRequest("parsing_exception", "operator is \"or\" or \"and\", got %q", op)
		}
	}

	space := termSpace(field)
	tokens := c.ix.Tokenize(text)
	if len(tokens) == 0 {
		// No streams, which runSearch answers as zero hits. Not silent and not the
		// same as `term`, which refuses: a match is free text, and a person typing
		// punctuation into a search box is ordinary rather than mistaken.
		return compiled{}, nil
	}

	dist := 0
	if raw, ok := opts["fuzziness"]; ok {
		var err *apiError
		if dist, err = parseFuzziness(raw, tokens[0]); err != nil {
			return compiled{}, err
		}
	}

	scorers := make([]engine.Scorer, 0, len(tokens))
	for _, tok := range tokens {
		if dist > 0 {
			scorers = append(scorers, query.Fuzzy(c.ix, space, tok, dist))
			continue
		}
		// Glob rather than a term scorer, because a pattern with no metacharacter
		// matches itself and nothing else — one clause kind fewer, exactly as
		// pkg/query.Parse argues.
		scorers = append(scorers, query.Glob(c.ix, space, termLiteral(tok)))
	}
	return compiled{scorers: scorers, requireAll: requireAll}, nil
}

// matchOptions is the set of long-form options match reads. Anything else is
// refused by name rather than ignored.
var matchOptions = []string{"query", "operator", "fuzziness"}

// matchText reads either the short form ("field": "some words") or the long form
// ("field": {"query": "some words", ...}), and refuses every option it does not
// implement rather than ignoring it.
func matchText(field string, value json.RawMessage) (text string, opts map[string]json.RawMessage, err *apiError) {
	if e := json.Unmarshal(value, &text); e == nil {
		return text, nil, nil
	}

	var long map[string]json.RawMessage
	if e := json.Unmarshal(value, &long); e != nil {
		return "", nil, badRequest("parsing_exception",
			"match on %q takes a string or an object with a query field, got %s", field, value)
	}
	q, ok := long["query"]
	if !ok {
		return "", nil, badRequest("parsing_exception", "match on %q has no query field", field)
	}
	if e := json.Unmarshal(q, &text); e != nil {
		return "", nil, badRequest("parsing_exception", "the query field of match on %q is not a string", field)
	}
	for opt := range long {
		if slices.Contains(matchOptions, opt) {
			continue
		}
		// Named rather than ignored. `minimum_should_match: 2` silently dropped
		// returns more documents than the client asked for and still looks like a
		// working search.
		return "", nil, badRequest("parsing_exception",
			"match option %q is not supported: this server reads %v", opt, matchOptions)
	}
	return text, long, nil
}

// matchPhrase keeps only the documents whose text holds the words consecutively.
//
// Document.Text only, which is query.Phrase's own limit and is repeated here as a
// refusal rather than inherited as a surprise: Phrase re-reads the one text a
// document has always had, so a phrase clause naming a field would be answered
// from a different field's content.
func (c *compiler) matchPhrase(body json.RawMessage) (compiled, *apiError) {
	field, value, err := oneField("match_phrase", body)
	if err != nil {
		return compiled{}, err
	}
	if termSpace(field) != "" {
		return compiled{}, badRequest("parsing_exception",
			"match_phrase on %q is not supported: a phrase is decided by re-reading engine.Document.Text, "+
				"because a Posting carries a frequency and not a position (docs/FORMAT.md section 8), so a "+
				"phrase clause on a named field would be answered from a different field's text. Use %q, or "+
				"a match with operator \"and\"", field, textField)
	}
	text, _, err := matchText(field, value)
	if err != nil {
		return compiled{}, err
	}
	words := c.ix.Tokenize(text)
	if len(words) == 0 {
		return compiled{}, nil
	}
	// Over a Glob on the first word rather than over the corpus, which is the
	// shape pkg/query.Parse uses: Phrase filters an inner scorer's candidates, so
	// it needs one nominating every document holding the phrase's words, and any
	// single word's Glob is a superset of that.
	//
	// ponytail: the first word, not the rarest. The rarest would bound the record
	// decodes by the shortest posting list instead of by the leading word's; the
	// lever is engine.Index.PostingCount and the trigger is a phrase query showing
	// up in a profile.
	inner := query.Glob(c.ix, "", termLiteral(words[0]))
	return compiled{scorers: []engine.Scorer{query.Phrase(c.ix, inner, text)}}, nil
}

// ---------------------------------------------------------------- term-shaped

// term finds one literal value in one field.
func (c *compiler) term(body json.RawMessage) (compiled, *apiError) {
	field, value, err := oneField("term", body)
	if err != nil {
		return compiled{}, err
	}
	raw, err := termRaw(field, value)
	if err != nil {
		return compiled{}, err
	}
	scorers, err := c.literal(field, raw)
	if err != nil {
		return compiled{}, err
	}
	// Every stream, because a term is one value: a keyword holding two tokens is
	// found as a conjunction of them. termScorers is where that is priced.
	return compiled{scorers: scorers, requireAll: len(scorers) > 1}, nil
}

// terms finds any of several literal values in one field.
func (c *compiler) terms(body json.RawMessage) (compiled, *apiError) {
	field, value, err := oneField("terms", body)
	if err != nil {
		return compiled{}, err
	}
	var values []json.RawMessage
	if e := json.Unmarshal(value, &values); e != nil {
		return compiled{}, badRequest("parsing_exception",
			"a terms clause takes an array of values, got %s (terms lookup against another index is not supported)", value)
	}
	if len(values) == 0 {
		return compiled{}, badRequest("parsing_exception",
			"a terms clause with no values can match nothing, which is not a question this server answers with 200")
	}
	var out []engine.Scorer
	for _, v := range values {
		raw, err := termRaw(field, v)
		if err != nil {
			return compiled{}, err
		}
		scorers, err := c.literal(field, raw)
		if err != nil {
			return compiled{}, err
		}
		out = append(out, scorers...)
	}
	// A union: any of the values. A multi-token value loses its conjunction here,
	// which is published in docs/LIMITATIONS.md rather than papered over — the
	// alternative is one anyOf per value, and that would make `terms` rank by
	// value count instead of by term rarity.
	return compiled{scorers: out}, nil
}

// literal is one value looked up in one field, encoded the way it was indexed.
//
// A mapped number or date does not go through the tokenizer at all: it was written
// as query.EncodeInt's output and has to be read back the same way, which is the
// contract that function names. Everything else is tokenized, because D-023 says a
// query tokenized differently from the documents finds nothing and has nothing to
// report.
func (c *compiler) literal(field string, raw json.RawMessage) ([]engine.Scorer, *apiError) {
	if kind := c.m.Type(field); numericKind(kind) {
		text, indexed, err := indexTerm(field, kind, raw)
		if err != nil {
			return nil, badRequest("parsing_exception", "%v", err)
		}
		if !indexed {
			return nil, badRequest("parsing_exception", "%q is mapped as %q and %s is not a value of one", field, kind, raw)
		}
		return []engine.Scorer{query.Glob(c.ix, termSpace(field), termLiteral(text))}, nil
	}
	text, err := termText(field, raw)
	if err != nil {
		return nil, err
	}
	return termScorers(c.ix, termSpace(field), text)
}

// termRaw unwraps `{"field": {"value": x}}` to x and leaves a plain scalar alone.
//
// The long form is what a client sends when it also wants a boost, so the options
// beside `value` are refused by name here rather than dropped.
func termRaw(field string, value json.RawMessage) (json.RawMessage, *apiError) {
	var long map[string]json.RawMessage
	if err := json.Unmarshal(value, &long); err != nil {
		return value, nil // a scalar, which is the short form
	}
	v, ok := long[optValue]
	if !ok {
		return nil, badRequest("parsing_exception", "a term clause on %q has no value field", field)
	}
	for opt := range long {
		if opt != optValue {
			return nil, badRequest("parsing_exception",
				"term option %q is not supported: this server reads value and nothing else", opt)
		}
	}
	return v, nil
}

// termText renders a scalar term value as the text a tokenizer will see.
func termText(field string, raw json.RawMessage) (string, *apiError) {
	if text, ok := scalarText(raw); ok {
		return text, nil
	}
	return "", badRequest("parsing_exception",
		"a term value on %q is a string, a number or a boolean, got %s", field, raw)
}

// pattern is prefix and wildcard, which are one query with two ways of writing
// the star.
func (c *compiler) pattern(body json.RawMessage, kind string) (compiled, *apiError) {
	field, value, err := oneField(kind, body)
	if err != nil {
		return compiled{}, err
	}
	raw, err := termRaw(field, value)
	if err != nil {
		return compiled{}, err
	}
	text, err := termText(field, raw)
	if err != nil {
		return compiled{}, err
	}
	if mapped := c.m.Type(field); numericKind(mapped) {
		return compiled{}, badRequest("parsing_exception",
			"%s on %q is not supported: the field is mapped as %q, so its terms are query.EncodeInt's "+
				"fixed-width hex and a pattern over them would match by accident rather than by value. "+
				"Use a range", kind, field, mapped)
	}

	space := termSpace(field)
	pattern := text
	if kind == clausePrefix {
		pattern = termLiteral(text) + "*"
	}
	// A wildcard's `*` and `?` are meant, so its value goes through unescaped.
	// path.Match also reads `[` as a character class where OpenSearch reads it
	// literally; that difference is published in PRD section 4 rather than
	// papered over with a rewrite that would then disagree with query.Glob.
	if err := vocabularyFits(c.ix, space, pattern); err != nil {
		return compiled{}, err
	}
	return compiled{scorers: []engine.Scorer{query.Glob(c.ix, space, pattern)}}, nil
}

// fuzzy is edit-distance matching over one term.
func (c *compiler) fuzzy(body json.RawMessage) (compiled, *apiError) {
	field, value, apiErr := oneField("fuzzy", body)
	if apiErr != nil {
		return compiled{}, apiErr
	}

	term, dist := "", 1
	var long map[string]json.RawMessage
	if err := json.Unmarshal(value, &long); err == nil {
		raw, ok := long[optValue]
		if !ok {
			return compiled{}, badRequest("parsing_exception", "a fuzzy clause on %q has no value field", field)
		}
		if err := json.Unmarshal(raw, &term); err != nil {
			return compiled{}, badRequest("parsing_exception", "the value of a fuzzy clause on %q is not a string", field)
		}
		for opt := range long {
			if opt != optValue && opt != "fuzziness" {
				return compiled{}, badRequest("parsing_exception",
					"fuzzy option %q is not supported: this server reads value and fuzziness", opt)
			}
		}
		if raw, ok := long["fuzziness"]; ok {
			if dist, apiErr = parseFuzziness(raw, term); apiErr != nil {
				return compiled{}, apiErr
			}
		}
	} else if err := json.Unmarshal(value, &term); err != nil {
		return compiled{}, badRequest("parsing_exception",
			"a fuzzy clause on %q takes a string or an object with a value field, got %s", field, value)
	}

	tokens := c.ix.Tokenize(term)
	if len(tokens) != 1 {
		return compiled{}, badRequest("parsing_exception",
			"a fuzzy clause is one term and %q tokenizes to %d: the edit distance between a query and a "+
				"phrase is not what this measures", term, len(tokens))
	}
	if dist == 0 {
		// Distance zero is an exact term, and query.Fuzzy's own floor is one edit.
		return compiled{scorers: []engine.Scorer{query.Glob(c.ix, termSpace(field), termLiteral(tokens[0]))}}, nil
	}
	return compiled{scorers: []engine.Scorer{query.Fuzzy(c.ix, termSpace(field), tokens[0], dist)}}, nil
}

// rangeOptions is what a range clause may hold: the four bounds, and nothing that
// would change what a bound means.
var rangeOptions = []string{boundGTE, boundGT, boundLTE, boundLT}

// rangeQuery is an interval over one field's terms.
//
// The bounds are encoded by the field's mapping, which is the whole reason
// mappings exist here — PRD section 5. A range over an unmapped field is a range
// over the alphabet, which query.Range says of itself and which is occasionally
// what somebody wants; a *numeric* bound over an unmapped field is refused,
// because "42" sorts before "7" and answering it would be nonsense with no error.
func (c *compiler) rangeQuery(body json.RawMessage) (compiled, *apiError) {
	field, value, apiErr := oneField("range", body)
	if apiErr != nil {
		return compiled{}, apiErr
	}
	var bounds map[string]json.RawMessage
	if err := json.Unmarshal(value, &bounds); err != nil {
		return compiled{}, badRequest("parsing_exception", "a range clause on %q is an object of bounds, got %s", field, value)
	}
	for opt := range bounds {
		if !slices.Contains(rangeOptions, opt) {
			// format, time_zone, relation and boost each change what a bound means
			// or how it is read. Accepting one and ignoring it answers a different
			// interval than the one written.
			return compiled{}, badRequest("parsing_exception",
				"range option %q is not supported: this server reads %v, and a bound is read by the field's "+
					"mapping rather than by a per-query format", opt, rangeOptions)
		}
	}
	if len(bounds) == 0 {
		return compiled{}, badRequest("parsing_exception",
			"a range clause on %q names no bound; \"this field is present\" is an exists query", field)
	}
	if bounds[boundGTE] != nil && bounds[boundGT] != nil {
		return compiled{}, badRequest("parsing_exception", "a range on %q names both gte and gt", field)
	}
	if bounds[boundLTE] != nil && bounds[boundLT] != nil {
		return compiled{}, badRequest("parsing_exception", "a range on %q names both lte and lt", field)
	}

	kind := c.m.Type(field)
	lo, hi := "", ""
	for _, b := range []struct {
		key       string
		exclusive bool
		upper     bool
		into      *string
	}{
		{boundGTE, false, false, &lo},
		{boundGT, true, false, &lo},
		{boundLTE, false, true, &hi},
		{boundLT, true, true, &hi},
	} {
		raw, ok := bounds[b.key]
		if !ok {
			continue
		}
		encoded, err := rangeBound(field, kind, raw, b.exclusive, b.upper)
		if err != nil {
			return compiled{}, badRequest("parsing_exception", "%v", err)
		}
		*b.into = encoded
	}
	if lo != "" && hi != "" && lo > hi {
		return compiled{}, badRequest("parsing_exception",
			"the lower bound of the range on %q is above the upper, so it names no term at all", field)
	}
	return compiled{scorers: []engine.Scorer{query.Range(c.ix, termSpace(field), lo, hi)}}, nil
}

// exists is "this field holds any term at all", which query.Range spells as an
// unbounded range — the row PRD section 4 does not have and the clause every
// client sends beside a filter.
func (c *compiler) exists(body json.RawMessage) (compiled, *apiError) {
	var clause struct {
		Field string `json:"field"`
	}
	if err := json.Unmarshal(body, &clause); err != nil {
		return compiled{}, badRequest("parsing_exception", "an exists clause names a field, got %s", body)
	}
	if clause.Field == "" {
		return compiled{}, badRequest("parsing_exception", "an exists clause names a field and this one names none")
	}
	return compiled{scorers: []engine.Scorer{query.Range(c.ix, termSpace(clause.Field), "", "")}}, nil
}

// ---------------------------------------------------------------- knn, hybrid

// knn is the vector stream, and it is the shortest clause in this file.
//
// scorer/vector reads engine.Query.Vector and nominates the nearest documents by
// cosine. Nothing else has to happen: the stream fuses beside the text streams
// because fusion.Fuse cannot tell them apart, which is milestone 1's claim being
// re-run on the other side of a process boundary.
func (c *compiler) knn(body json.RawMessage) (compiled, *apiError) {
	field, value, apiErr := oneField(clauseKnn, body)
	if apiErr != nil {
		return compiled{}, apiErr
	}
	mapped, dim, ok := c.m.Vector()
	if !ok {
		return compiled{}, badRequest(kindIllegalArgument,
			"no field in this index is mapped as %s, so there are no vectors to search: declare one with a "+
				"dimension before indexing", typeKNNVector)
	}
	if field != mapped {
		return compiled{}, badRequest(kindIllegalArgument,
			"%q is not the vector field: a document carries one vector (engine.Document.Vector) and this "+
				"index binds it to %q", field, mapped)
	}

	var clause struct {
		Vector    []float32       `json:"vector"`
		K         *int            `json:"k"`
		Filter    json.RawMessage `json:"filter"`
		MinScore  json.RawMessage `json:"min_score"`
		MaxDist   json.RawMessage `json:"max_distance"`
		MethodPar json.RawMessage `json:"method_parameters"`
	}
	if err := json.Unmarshal(value, &clause); err != nil {
		return compiled{}, badRequest("parsing_exception", "the knn clause on %q did not decode: %v", field, err)
	}
	for _, r := range []struct {
		present bool
		reason  string
	}{
		{clause.Filter != nil, "knn.filter is not supported: put the filter in a bool beside the knn clause, " +
			"where it narrows the fused result rather than one stream — a filter applied inside the vector " +
			"search would change which candidates the fusion never sees"},
		{clause.MinScore != nil || clause.MaxDist != nil, "min_score and max_distance are not supported: " +
			"fusion ranks by position and a threshold on one stream's score decides membership by a number " +
			"the other streams are not on"},
		{clause.MethodPar != nil, "method_parameters are not supported: the index's own nprobe is fixed at " +
			"build time (docs/FORMAT.md, IVF-flat), so a per-query override would be accepted and ignored"},
	} {
		if r.present {
			return compiled{}, badRequest(kindIllegalArgument, "%s", r.reason)
		}
	}
	if len(clause.Vector) != dim {
		return compiled{}, badRequest(kindIllegalArgument,
			"the query vector has %d components and %q is mapped with dimension %d: cosine between vectors "+
				"of different widths is not defined (engine.ErrDimMismatch)", len(clause.Vector), field, dim)
	}
	if c.p.q.Vector != nil {
		return compiled{}, badRequest(kindIllegalArgument,
			"a query holds one knn clause: engine.Query carries one vector, so a second would replace the first")
	}
	c.p.q.Vector = clause.Vector

	// k asks the vector stream for k candidates. Here every stream is asked for
	// the same depth — engine.Search hands one k to all of them — so a k larger
	// than the page raises the depth for the whole fusion rather than for this
	// stream alone. Said out loud because the alternative is reading k and
	// quietly doing something else with it.
	if clause.K != nil {
		if *clause.K <= 0 {
			return compiled{}, badRequest(kindIllegalArgument, "knn k must be positive, got %d", *clause.K)
		}
		if *clause.K > maxResultWindow {
			return compiled{}, badRequest(kindIllegalArgument,
				"knn k of %d is over the result window of %d, and k is the depth every stream is asked for",
				*clause.K, maxResultWindow)
		}
		c.p.depth = max(c.p.depth, *clause.K)
	}
	return compiled{scorers: []engine.Scorer{vector.New(c.ix)}}, nil
}

// decayFunctions is what OpenSearch calls a decay, and weft has one of them.
//
// gauss, exp and linear differ in the shape of the curve; scorer/recency is an
// exponential half-life (HalfLife, thirty days). The three names are accepted and
// the *parameters* are not — see functionScore — because a name this server can
// answer approximately is better than three names it refuses, and a parameter it
// would silently ignore is not.
var decayFunctions = []string{"gauss", "exp", "linear"}

// functionScore is the fourth signal's clause: recency, as a stream.
//
// This is milestone 26's measurement and the reason the milestone exists. Adding
// it required no new fusion, no new constraint, and no change to how a plan is
// executed — a recency stream goes into the same list as a match and a knn, and
// engine.Search fuses the three without being told there are three kinds.
//
// What it does refuse is a decay's parameters. OpenSearch's decay takes origin,
// scale, offset and decay; scorer/recency takes a fixed half-life and the time it
// is evaluated at. Reading scale and ignoring it would rank by a curve the client
// did not ask for, so the parameters are named and refused, which is D-026's rule
// applied to a clause option instead of to a query type.
func (c *compiler) functionScore(body json.RawMessage) (compiled, *apiError) {
	var clause map[string]json.RawMessage
	if err := json.Unmarshal(body, &clause); err != nil {
		return compiled{}, badRequest("parsing_exception", "the function_score clause did not decode: %v", err)
	}

	var decay json.RawMessage
	for name, raw := range clause {
		if !slices.Contains(decayFunctions, name) {
			return compiled{}, badRequest(kindIllegalArgument,
				"function_score option %q is not supported: the only scoring function here is a decay over "+
					"the field bound to the document's time, because a score computed per document has "+
					"nothing to be compared against — fusion ranks by position. This server reads %v",
				name, decayFunctions)
		}
		if decay != nil {
			return compiled{}, badRequest(kindIllegalArgument, "a function_score names one decay and this one names several")
		}
		decay = raw
	}
	if decay == nil {
		return compiled{}, badRequest(kindIllegalArgument,
			"a function_score names one of %v over the recency field", decayFunctions)
	}

	field, params, apiErr := oneField("function_score", decay)
	if apiErr != nil {
		return compiled{}, apiErr
	}
	var opts map[string]json.RawMessage
	if err := json.Unmarshal(params, &opts); err != nil {
		return compiled{}, badRequest("parsing_exception", "the decay on %q takes an object, got %s", field, params)
	}
	if len(opts) > 0 {
		return compiled{}, badRequest(kindIllegalArgument,
			"decay parameters on %q are not supported: scorer/recency is an exponential with a fixed "+
				"half-life of %s, so origin, scale, offset and decay would be read and not honoured — which "+
				"would rank by a curve nobody asked for. Send an empty object",
			field, recency.HalfLife)
	}

	bound, ok := c.m.Recency()
	if !ok {
		return compiled{}, badRequest(kindIllegalArgument,
			"no field in this index is mapped with \"recency\": true, so no document carries a time to decay "+
				"from. Add it to a date field's mapping and re-index")
	}
	if field != bound {
		return compiled{}, badRequest(kindIllegalArgument,
			"%q is not the recency field: a document carries one time (engine.Document.Time) and this index "+
				"binds it to %q", field, bound)
	}
	// NewAt rather than New, so the request's own clock is what the ranking is
	// relative to. That is the query-time input D-021 said belongs in a scorer's
	// constructor rather than in engine.Query, and this is a caller doing it.
	return compiled{scorers: []engine.Scorer{recency.NewAt(c.ix, time.Now())}}, nil
}

// hybridBody is a hybrid clause: several queries, fused.
//
// `weights` is weft's and not OpenSearch's, where the weighting lives in a search
// pipeline's normalization processor. It is here because the pipeline is the thing
// this server exists to make unnecessary — PRD section 1 — and because D-005 made
// weighted fusion the repayment for milestone 4: an unweighted vote from a stream
// with nothing to say is what cost 0.1202 nDCG@10 there.
type hybridBody struct {
	Queries  []map[string]json.RawMessage `json:"queries"`
	Weights  []float64                    `json:"weights"`
	Pipeline json.RawMessage              `json:"search_pipeline"`
}

// hybridQuery compiles a hybrid clause: one stream set per sub-query, fused.
//
// This is the milestone's whole subject. What it demonstrates is not that a
// hybrid query works — bleve and OpenSearch both have one — but *what it cost*:
// every sub-query goes through the same clause compiler as a top-level query, the
// streams go into the same list, and the fusion below is the same fusion. There is
// no branch anywhere in this function on what kind of signal a sub-query produced.
func (c *compiler) hybridQuery(body json.RawMessage) *apiError {
	var h hybridBody
	if err := json.Unmarshal(body, &h); err != nil {
		return badRequest("parsing_exception", "the hybrid clause did not decode as an object: %v", err)
	}
	if h.Pipeline != nil {
		return badRequest(kindIllegalArgument,
			"search_pipeline is not supported: normalizing two streams onto a common scale is the step this "+
				"server does not have, because rank fusion reads position and never score. Weight the "+
				"sub-queries with \"weights\" instead")
	}
	if len(h.Queries) == 0 {
		return badRequest("parsing_exception", "a hybrid clause names no queries, so there is nothing to fuse")
	}
	if h.Weights != nil && len(h.Weights) != len(h.Queries) {
		return badRequest(kindIllegalArgument,
			"a hybrid clause has %d queries and %d weights: a weight is positional, so an unequal list "+
				"would weight a query the client did not mean", len(h.Queries), len(h.Weights))
	}

	for i, one := range h.Queries {
		if len(one) != 1 {
			names := make([]string, 0, len(one))
			for name := range one {
				names = append(names, name)
			}
			slices.Sort(names)
			return badRequest("parsing_exception",
				"query %d of the hybrid holds %d clauses and it holds exactly one (%v)", i, len(names), names)
		}
		for name, sub := range one {
			if name == clauseBool {
				// A bool sub-query is compiled in place, so its constraints land
				// on the same plan. That is the one nesting this engine allows,
				// and it allows it because a bool contributes positions rather
				// than a fusion of its own.
				before := len(c.p.scorers)
				if err := c.boolQuery(sub); err != nil {
					return err
				}
				c.weigh(h.Weights, i, before)
				continue
			}
			cl, err := c.clause(name, sub)
			if err != nil {
				return err
			}
			if len(cl.scorers) == 0 {
				return badRequest("parsing_exception",
					"query %d of the hybrid (%s) holds no term this index could have indexed, so it "+
						"contributes no stream and the weights would shift under the client", i, name)
			}
			before := len(c.p.scorers)
			at := c.add(cl.scorers...)
			if cl.requireAll {
				c.p.must = append(c.p.must, at...)
			}
			c.weigh(h.Weights, i, before)
		}
	}
	return nil
}

// weigh gives every stream a sub-query produced that sub-query's weight.
//
// One weight per *stream* rather than per sub-query, because that is
// fusion.FuseWeighted's contract: a weight is attached to a position, which is how
// fusion stays ignorant of what produced it. A match over three tokens is three
// streams and takes the weight three times — so a client weighting text against
// vectors is weighting the clause and not the token count.
//
// ponytail: the same weight repeated rather than divided by the stream count. A
// three-token match therefore votes three times at full weight, which is what an
// unweighted hybrid already did; changing it would be a different fusion, and the
// place to decide that is a measurement rather than this function.
func (c *compiler) weigh(weights []float64, i, before int) {
	if weights == nil {
		return
	}
	for len(c.p.weights) < before {
		c.p.weights = append(c.p.weights, 1)
	}
	for range len(c.p.scorers) - before {
		c.p.weights = append(c.p.weights, weights[i])
	}
}

// ---------------------------------------------------------------------- bool

// boolBody is the one clause that holds other clauses, and it holds them one
// level deep.
type boolBody struct {
	Must               json.RawMessage `json:"must"`
	MustNot            json.RawMessage `json:"must_not"`
	Should             json.RawMessage `json:"should"`
	Filter             json.RawMessage `json:"filter"`
	MinimumShouldMatch json.RawMessage `json:"minimum_should_match"`
	Boost              json.RawMessage `json:"boost"`
}

// boolQuery compiles a bool clause into streams and the constraints over them.
//
// Each occurrence type becomes a different kind of position, and none of them
// becomes a new fuser:
//
//	must      the clause's stream is required — query.Must
//	filter    required, then emptied before ranking — query.Must, then blank
//	must_not  every stream excludes — query.MustNot
//	should    a stream and nothing else — fusion is already a union of votes
//
// That last line is why this milestone needed no fusion code: `should` is what
// rank fusion does when it is left alone.
func (c *compiler) boolQuery(body json.RawMessage) *apiError {
	var b boolBody
	if err := json.Unmarshal(body, &b); err != nil {
		return badRequest("parsing_exception", "the bool clause did not decode as an object: %v", err)
	}
	if b.MinimumShouldMatch != nil {
		return badRequest("parsing_exception",
			"minimum_should_match is not supported: a should clause here is a stream in a rank fusion, and "+
				"fusion counts votes to rank rather than to admit — there is nowhere to put a threshold "+
				"that would not be a different fusion operator. Move the required clauses into must")
	}
	if b.Boost != nil {
		return badRequest("parsing_exception",
			"boost is not supported: fusion ranks by position and never by score, so a per-clause multiplier "+
				"has nothing to multiply — engine.Candidate.Score is not comparable across streams")
	}

	for _, part := range []struct {
		raw  json.RawMessage
		name string
	}{
		{b.Must, occMust},
		{b.Filter, occFilter},
		{b.MustNot, occMustNot},
		{b.Should, occShould},
	} {
		if part.raw == nil {
			continue
		}
		clauses, err := boolClauses(part.name, part.raw)
		if err != nil {
			return err
		}
		for _, one := range clauses {
			if err := c.occurrence(part.name, one); err != nil {
				return err
			}
		}
	}
	if len(c.p.scorers) == 0 {
		return badRequest("parsing_exception",
			"the bool clause names no stream at all, so there is nothing to search: that is a query this "+
				"server refuses rather than answers with an empty page")
	}
	return nil
}

// boolClauses reads an occurrence list, which OpenSearch lets a client write as
// one object or as an array of them.
func boolClauses(name string, raw json.RawMessage) ([]map[string]json.RawMessage, *apiError) {
	var many []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	var one map[string]json.RawMessage
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, badRequest("parsing_exception", "bool.%s is a clause or an array of clauses, got %s", name, raw)
	}
	return []map[string]json.RawMessage{one}, nil
}

// occurrence compiles one clause of a bool and files its positions.
func (c *compiler) occurrence(kind string, one map[string]json.RawMessage) *apiError {
	if len(one) != 1 {
		names := make([]string, 0, len(one))
		for name := range one {
			names = append(names, name)
		}
		slices.Sort(names)
		return badRequest("parsing_exception",
			"a clause in bool.%s holds exactly one query and this one holds %d (%v)", kind, len(names), names)
	}

	for name, body := range one {
		cl, err := c.clause(name, body)
		if err != nil {
			return err
		}
		if len(cl.scorers) == 0 {
			// A clause whose text tokenized to nothing. In should it is simply
			// absent; anywhere else it is a constraint that cannot be evaluated,
			// and pkg/query's rule for those is that they are not satisfied — so
			// the client hears it rather than getting an empty page.
			if kind == occShould {
				return nil
			}
			return badRequest("parsing_exception",
				"the %s clause in bool.%s holds no term this index could have indexed, so it can never be "+
					"satisfied and every result would be excluded by it", name, kind)
		}

		switch kind {
		case occShould:
			if cl.requireAll {
				return badRequest("parsing_exception",
					"a conjunction inside bool.should is not expressible: should *is* the fusion — a union "+
						"of votes over streams — so requiring several of those streams is a constraint on "+
						"the whole query rather than on one clause of it. Move the clause into must")
			}
			c.add(cl.scorers...)
		case occMust, occFilter:
			// One position, so the clause can be required as a unit. A multi-token
			// `match` in a must means "this match matched", which is a disjunction
			// — and query.Must intersects, so requiring its positions one by one
			// would silently turn it into an `and`.
			at := c.add(c.unit(cl)...)
			c.p.must = append(c.p.must, at...)
			if kind == occFilter {
				c.p.filter = append(c.p.filter, at...)
			}
		case occMustNot:
			// Every position, and no unit: excluding a document present in any of
			// the streams is exactly what "this clause did not match" means for a
			// disjunction, and query.MustNot already bans the union.
			c.p.mustNot = append(c.p.mustNot, c.add(cl.scorers...)...)
		}
	}
	return nil
}

// unit collapses a clause into the streams a constraint can name.
//
// One stream is itself. A conjunction is its streams, which query.Must can require
// directly. A disjunction of several streams is the case that needs anyOf: it has
// to become one stream, so that requiring it means "any of these matched" rather
// than "all of them did".
func (c *compiler) unit(cl compiled) []engine.Scorer {
	if len(cl.scorers) == 1 || cl.requireAll {
		return cl.scorers
	}
	return []engine.Scorer{&anyOf{inner: cl.scorers}}
}

// anyOf presents several streams as one: a document is a candidate if any of them
// nominated it, scored by the sum of what they said.
//
// It exists for one reason — a required clause has to be *one* position, because
// query.Must intersects the positions it is given. It is a dozen lines in the
// server rather than a new operator in pkg/, and that is the point: the constraint
// vocabulary in pkg/query did not have to grow a disjunction for the HTTP surface
// to have one.
//
// The sum is not comparable to any other stream's score and does not need to be:
// fusion.Fuse reads rank, and every candidate here comes from term scorers over
// one clause, which are on one scale by construction.
type anyOf struct{ inner []engine.Scorer }

func (a *anyOf) Name() string { return "anyOf" }

// Candidates returns **every** nominated document and does not truncate to k,
// which is what query.Must needs — a restriction truncated to k excludes every
// document below its own cut, and that is a wrong answer rather than a narrow one.
// The scorers inside are pkg/query's, which hold the same rule.
func (a *anyOf) Candidates(ctx context.Context, q engine.Query, k int) ([]engine.Candidate, error) {
	acc := make(map[engine.DocID]float64)
	for _, s := range a.inner {
		cands, err := s.Candidates(ctx, q, k)
		if err != nil {
			return nil, err
		}
		for _, c := range cands {
			acc[c.Doc] += c.Score
		}
	}
	if len(acc) == 0 {
		return nil, nil
	}
	out := make([]engine.Candidate, 0, len(acc))
	for doc, score := range acc {
		out = append(out, engine.Candidate{Doc: doc, Score: score})
	}
	return engine.TopK(out, len(out)), nil
}

// -------------------------------------------------------------------- running

// runSearch executes a plan and reports the page, the total, and whether that
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
	depth := max(p.from+p.size, p.depth)
	if depth > maxResultWindow {
		depth = maxResultWindow
	}

	if p.matchAll {
		hits, total = matchAllHits(x, depth)
		return page(hits, p), total, true, nil
	}
	if len(p.scorers) == 0 {
		// A match whose text tokenized to nothing. Zero hits is the right answer
		// and it is not a silent one: the client asked for nothing.
		return nil, 0, true, nil
	}

	cands, err := engine.Search(ctx, p.q, depth, p.fuser(), p.scorers...)
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
	// prefix is a lower bound on how many documents matched, and calling that "eq"
	// would be a number this server cannot stand behind.
	return page(hits, p), len(hits), len(cands) < depth, nil
}

// page cuts the requested window out of the fetched prefix.
//
// from is applied after fusion rather than pushed into it, because there is
// nothing to push it into: the top-k collector has no notion of an offset, so a
// page is the whole prefix with its head removed. maxResultWindow is where that
// price stops being paid.
func page(hits []hit, p plan) []hit {
	if p.from >= len(hits) {
		return nil
	}
	hits = hits[p.from:]
	if len(hits) > p.size {
		hits = hits[:p.size]
	}
	return hits
}

// matchAllHits walks the id space.
//
// ponytail: a linear scan over Len(), which is one past the highest DocID and
// counts tombstones with it, so this is O(corpus) per match_all query. The
// upgrade path is an iterator on engine, and the trigger is match_all appearing
// in anything but a smoke test. It is here because "search this index" with no
// body is the first request every human makes, and a 400 for it would be a worse
// answer than a slow one.
func matchAllHits(x *Index, depth int) (hits []hit, total int) {
	ix := x.Engine()
	total, _ = ix.Stats()
	if depth <= 0 {
		return nil, total
	}
	for i := range ix.Len() {
		d, ok := ix.Doc(engine.DocID(i))
		if !ok {
			continue // a tombstone
		}
		hits = append(hits, newHit(x, d.Key, 1.0))
		if len(hits) == depth {
			break
		}
	}
	return hits, total
}

// ------------------------------------------------------------------ indexing

// scalarText renders a JSON scalar as the text weft will tokenize for it.
//
// Strings are themselves. Numbers and booleans become their literal, so
// {"views": 42} is findable as the term "42" — not a numeric index, and
// deliberately not pretending to be one: docs/LIMITATIONS.md prices that, and a
// mapping is what turns the field into query.EncodeInt's output instead. Anything
// else — an object, an array, null — is kept in _source and not indexed, which is
// what OpenSearch spells "index": false.
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

// documentFrom builds the engine.Document for one JSON body, under a mapping.
//
// Fields are added in sorted key order so two identical bodies produce identical
// documents: engine.Document.Fields is a slice, and a map's iteration order would
// otherwise make the index depend on nothing.
func documentFrom(id string, body json.RawMessage, m *Mapping) (engine.Document, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return engine.Document{}, fmt.Errorf("the document body did not decode as a JSON object: %w", err)
	}

	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	slices.Sort(names)

	vecField, dim, hasVec := m.Vector()
	timeField, hasTime := m.Recency()
	linkField, hasLinks := m.Links()
	d := engine.Document{Key: id}
	for _, name := range names {
		if hasLinks && name == linkField {
			links, err := linksOf(name, raw[name])
			if err != nil {
				return engine.Document{}, err
			}
			d.Links = links
			// Set *and* indexed, which is the choice the recency field already
			// made and for the same reason: a client that declared this field
			// wants both, and neither is derivable from the other. The ids go in
			// as a field's text so that `term` and `exists` over the field still
			// answer — what they would otherwise do is match nothing and say
			// nothing, which is the failure this surface refuses everywhere else.
			//
			// The ids are tokenized on the way in, so an id the tokenizer splits
			// is reachable by its parts rather than whole. That is the keyword
			// limitation D-022 already carries — one tokenizer per index — and not
			// a new one.
			d.Fields = append(d.Fields, engine.Field{Name: name, Text: strings.Join(links, " ")})
			continue
		}
		if hasVec && name == vecField {
			v, err := jsonVector(name, dim, raw[name])
			if err != nil {
				return engine.Document{}, err
			}
			d.Vector = v
			continue
		}
		if hasTime && name == timeField {
			// Set *and* indexed: the same value is the recency scorer's Time and a
			// term a range query can reach, because a client that mapped a date
			// wants both and neither is derivable from the other.
			t, err := jsonTime(raw[name])
			if err != nil {
				return engine.Document{}, fmt.Errorf("%q is the recency field and this value is not a date: %w", name, err)
			}
			d.Time = t
		}
		text, indexed, err := indexTerm(name, m.Type(name), raw[name])
		if err != nil {
			return engine.Document{}, err
		}
		if !indexed {
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
