// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

// Milestone 26 is the only round of this PRD that buys new evidence. 24 and 25
// re-implement what other engines already have; this one re-runs milestone 1's
// claim on the far side of a process boundary:
//
//	a fourth signal costs the same as the first, and the fusion never learns
//	what any of them is.
//
// TestAFourthSignalOverHTTPIsUnderOneHundredLines is that judgment, counted from
// the source rather than asserted in a comment.

// knn is a stream like any other, which is the whole claim in one test: the
// vector search is nominated, fused and paged by the same code path as a match.
func TestKnnIsAStreamLikeAnyOther(t *testing.T) {
	srv := dslCorpus(t)

	// [1,0,0] is rrf's vector exactly and bm25's nearly. Nothing about the text
	// enters this query.
	wantIDs(t, hitIDs(t, srv, `{"query":{"knn":{"vec":{"vector":[1,0,0],"k":2}}},"size":2}`), "rrf", "bm25")

	// The other end of the space finds the other end of the corpus.
	wantIDs(t, hitIDs(t, srv, `{"query":{"knn":{"vec":{"vector":[0,0,1],"k":2}}},"size":2}`), "vector", "graph")
}

// A hybrid query is two streams fused, and the fusion is fusion.Fuse — the same
// function, called the same way, with nothing in it that knows a vector from a
// posting list.
func TestHybridFusesTextAndVectors(t *testing.T) {
	srv := dslCorpus(t)

	// Text alone ranks pottery first for "pottery"; vectors alone rank rrf first
	// for [1,0,0]. The hybrid holds both, so both appear.
	ids := hitIDs(t, srv, `{"query":{"hybrid":{"queries":[
		{"match":{"text":"pottery"}},
		{"knn":{"vec":{"vector":[1,0,0],"k":2}}}]}},"size":5}`)
	for _, want := range []string{"pottery", "rrf"} {
		if !slices.Contains(ids, want) {
			t.Errorf("hybrid found %v, missing %q — a stream was dropped", ids, want)
		}
	}
}

// Weights are positional, which is what lets them exist without fusion knowing
// what a stream holds. Turning one sub-query's weight down has to move the
// ranking without changing which documents are eligible.
func TestHybridWeightsMoveTheRankingWithoutNamingASignal(t *testing.T) {
	srv := dslCorpus(t)

	body := func(textWeight, vectorWeight string) string {
		return `{"query":{"hybrid":{"queries":[
			{"match":{"text":"pottery"}},
			{"knn":{"vec":{"vector":[1,0,0],"k":3}}}],
			"weights":[` + textWeight + `,` + vectorWeight + `]}},"size":5}`
	}
	first := func(b string) string {
		t.Helper()
		status, raw := do(t, srv, http.MethodPost, "/papers/_search", b)
		if status != http.StatusOK {
			t.Fatalf("status %d (body %s)", status, raw)
		}
		ids := decodeIDs(t, raw)
		if len(ids) == 0 {
			t.Fatalf("no hits for %s", b)
		}
		return ids[0]
	}

	// The eligible set does not change — a weight ranks and does not admit — and
	// the order does, which is the whole of what a weight is for.
	if got := first(body("10", "0.01")); got != "pottery" {
		t.Errorf("the text-weighted hybrid puts %q first, want \"pottery\"", got)
	}
	if got := first(body("0.01", "10")); got != "rrf" {
		t.Errorf("the vector-weighted hybrid puts %q first, want \"rrf\"", got)
	}
	heavy := hitIDs(t, srv, body("10", "0.01"))
	light := hitIDs(t, srv, body("0.01", "10"))
	if !slices.Equal(heavy, light) {
		t.Errorf("weighting changed which documents are eligible: %v then %v", heavy, light)
	}
}

// Weights are positions in the original stream list and query.MustNot is the one
// wrapper that hands on a shorter list. The two never meet because a hybrid is a
// whole query rather than a clause of a bool — asserted here rather than trusted
// to the comment that says so.
func TestHybridWeightsAndMustNotDoNotMeet(t *testing.T) {
	srv := dslCorpus(t)

	status, raw := do(t, srv, http.MethodPost, "/papers/_search", `{"query":{"bool":{"should":[
		{"hybrid":{"queries":[{"match":{"text":"a"}}]}}]}}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("a nested hybrid: status %d, want 400 (body %s)", status, raw)
	}

	// And a hybrid holding a bool keeps the bool's constraints, which is the one
	// nesting that is allowed — a bool contributes positions rather than a fusion.
	wantIDs(t, hitIDs(t, srv, `{"query":{"hybrid":{"queries":[
		{"bool":{"must":[{"match":{"text":"ranking"}}],"must_not":[{"term":{"status":"draft"}}]}}]}},"size":5}`),
		"graph")
}

// A knn clause is refused where it cannot be honoured, rather than answered from
// a field that holds no vectors.
func TestKnnRefusesWhatItCannotSearch(t *testing.T) {
	srv := dslCorpus(t)

	for _, tc := range []struct{ name, body string }{
		{"wrong field", `{"query":{"knn":{"title":{"vector":[1,0,0],"k":2}}}}`},
		{"wrong width", `{"query":{"knn":{"vec":{"vector":[1,0],"k":2}}}}`},
		{"a filter inside the stream", `{"query":{"knn":{"vec":{"vector":[1,0,0],"k":2,"filter":{"term":{"status":"draft"}}}}}}`},
		{"min_score", `{"query":{"knn":{"vec":{"vector":[1,0,0],"k":2,"min_score":0.5}}}}`},
		{"k over the window", `{"query":{"knn":{"vec":{"vector":[1,0,0],"k":100000}}}}`},
		{"two vectors", `{"query":{"hybrid":{"queries":[
			{"knn":{"vec":{"vector":[1,0,0],"k":2}}},
			{"knn":{"vec":{"vector":[0,1,0],"k":2}}}]}}}`},
		{"mismatched weights", `{"query":{"hybrid":{"queries":[{"match":{"text":"a"}}],"weights":[1,2]}}}`},
		{"a search pipeline", `{"query":{"hybrid":{"queries":[{"match":{"text":"a"}}],"search_pipeline":"norm"}}}`},
		{"no queries", `{"query":{"hybrid":{"queries":[]}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := do(t, srv, http.MethodPost, "/papers/_search", tc.body)
			if status == http.StatusOK {
				t.Fatalf("answered 200; a refusal is 4xx or 501 and never an empty hit list (body %s)", raw)
			}
			if status < 400 {
				t.Fatalf("status %d, want 4xx or 501 (body %s)", status, raw)
			}
		})
	}
}

// An index with no vector field says so rather than answering nothing.
func TestKnnOnAnIndexWithNoVectorsSaysSo(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/plain", "")
	do(t, srv, http.MethodPut, "/plain/_doc/1", `{"text":"a"}`)

	status, raw := do(t, srv, http.MethodPost, "/plain/_search", `{"query":{"knn":{"v":{"vector":[1,0],"k":2}}}}`)
	if status != http.StatusBadRequest {
		t.Errorf("status %d, want 400 (body %s)", status, raw)
	}
	if !strings.Contains(string(raw), typeKNNVector) {
		t.Errorf("the refusal does not name the mapping type that would fix it: %s", raw)
	}
}

// ------------------------------------------------------------- the judgment

// fusionFunctions is the code in this package that decides how streams are
// combined. Milestone 26's claim is that adding a fourth signal changed none of
// it, and this list is what "none of it" means.
var fusionFunctions = []string{"fuser", "blank", "unit", "weigh", "Candidates"}

// signalClauses is the query clause per signal — one function each, and its
// length is the measurement.
//
// text is `match`, vector is `knn`, and recency is `functionScore`, added by this
// milestone. graph has no HTTP clause at all: D-005 left it at a weight the README
// tells callers to turn down, and giving it a clause would be publishing a signal
// the project's own evaluation says not to use.
var signalClauses = map[string]string{
	"text":    "match",
	"vector":  "knn",
	"recency": "functionScore",
}

// TestAFourthSignalOverHTTPIsUnderOneHundredLines is milestone 26's judgment
// sentence, counted rather than asserted.
//
// It is deliberately the same shape as milestone 1's
// TestFourthScorerIsUnderOneHundredLines in pkg/engine, because it is the same
// claim asked one process boundary further out: if wiring a signal into the HTTP
// surface cost more than wiring it into a Go program, the architecture argument
// would stop at the library and this PRD's premise would be wrong.
//
// A hundred lines is the budget milestone 1 set and this one inherits. What it
// counts is the clause function — the parsing, the refusals and the scorer
// construction — because that is what a person adding a fifth signal has to write.
func TestAFourthSignalOverHTTPIsUnderOneHundredLines(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "search.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse search.go: %v", err)
	}

	lines := map[string]int{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		lines[fn.Name.Name] = fset.Position(fn.End()).Line - fset.Position(fn.Pos()).Line + 1
	}

	signals := make([]string, 0, len(signalClauses))
	for signal := range signalClauses {
		signals = append(signals, signal)
	}
	slices.Sort(signals) // so the log below reads the same on every run

	for _, signal := range signals {
		clause := signalClauses[signal]
		n, ok := lines[clause]
		if !ok {
			t.Errorf("%s has no clause function %q in search.go", signal, clause)
			continue
		}
		t.Logf("%-8s %-14s %3d lines", signal, clause, n)
		if n >= 100 {
			t.Errorf("the %s clause is %d lines, over the hundred milestone 1 budgeted: a signal that costs "+
				"more over HTTP than in a Go program means the architecture claim stops at the library",
				signal, n)
		}
	}
}

// TestTheFusionCodeDoesNotKnowAboutTheFourthSignal is the other half of the
// judgment: not only was the fourth signal cheap, the fusion did not learn about
// it.
//
// Checked structurally rather than by a line count. If any function that combines
// streams mentioned a signal by name — the scorer packages, the vector, the
// timestamp — then fusion would be branching on what a stream holds, and
// "fusion cannot see any scorer" would be false one layer above pkg/.
// forbiddenInFusion is the words a fusion function must not contain. Each names a
// signal, the constructor that builds one, or the query-time input only one of
// them reads.
//
// Spelled as constructors rather than as bare package names on purpose: a
// forbidden list holding "text." also matches "context.Context", which is a false
// positive that would make these tests look stricter than they are.
//
// One list for two tests — this one reads search.go and
// TestTheNativeFusionPathKnowsNoScorer reads native.go. Two copies would mean a
// fifth signal added to the list on one surface and not the other, which is the
// same drift the list exists to prevent.
var forbiddenInFusion = []string{
	"vector.New", "recency.New", "text.New", "graph.New",
	"knn", "gauss", ".Vector", ".Time", "Document.",
}

func TestTheFusionCodeDoesNotKnowAboutTheFourthSignal(t *testing.T) {
	src, err := os.ReadFile("search.go")
	if err != nil {
		t.Fatalf("read search.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "search.go", src, 0)
	if err != nil {
		t.Fatalf("parse search.go: %v", err)
	}

	found := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !slices.Contains(fusionFunctions, fn.Name.Name) {
			continue
		}
		found++
		body := string(src[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset])
		for _, word := range forbiddenInFusion {
			if strings.Contains(body, word) {
				t.Errorf("the fusion function %s mentions %q: fusion decides how streams combine and must not "+
					"know what any of them holds — that is the claim this PRD re-tests over HTTP",
					fn.Name.Name, word)
			}
		}
	}
	if found != len(fusionFunctions) {
		t.Errorf("found %d of the %d fusion functions in search.go; the list in fusionFunctions has gone stale "+
			"and this test is checking less than it says", found, len(fusionFunctions))
	}
}

// -------------------------------------------------------- the fourth signal

// recency over HTTP: function_score with a decay on the field bound to
// engine.Document.Time.
func TestRecencyIsAStreamOverHTTP(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", `{"mappings":{"properties":{
		"published":{"type":"date","recency":true}}}}`)

	for _, d := range []struct{ id, body string }{
		{"old", `{"text":"ranking signal","published":"2020-01-01"}`},
		{"recent", `{"text":"ranking signal","published":"2026-08-01"}`},
	} {
		if status, raw := do(t, srv, http.MethodPut, "/papers/_doc/"+d.id, d.body); status != http.StatusCreated {
			t.Fatalf("index %s: status %d (body %s)", d.id, status, raw)
		}
	}

	first := func(body string) []string {
		t.Helper()
		status, raw := do(t, srv, http.MethodPost, "/papers/_search", body)
		if status != http.StatusOK {
			t.Fatalf("%s: status %d (body %s)", body, status, raw)
		}
		return decodeIDs(t, raw)
	}

	// The stream on its own ranks by time.
	if ids := first(`{"query":{"function_score":{"gauss":{"published":{}}}},"size":2}`); len(ids) != 2 || ids[0] != "recent" {
		t.Fatalf("the recency stream ranks %v, want the recent document first", ids)
	}

	// The two documents hold identical text, so the text stream ties them and the
	// tie is broken by document id — which puts "old" first, because it was
	// indexed first. That is what makes this the honest test: fusing an *equal*
	// vote from recency cannot move it, and weighting the recency stream must.
	tied := first(`{"query":{"hybrid":{"queries":[
		{"match":{"text":"ranking signal"}},
		{"function_score":{"gauss":{"published":{}}}}]}},"size":2}`)
	if len(tied) != 2 {
		t.Fatalf("the unweighted hybrid found %v, want both documents", tied)
	}

	weighted := first(`{"query":{"hybrid":{"queries":[
		{"match":{"text":"ranking signal"}},
		{"function_score":{"gauss":{"published":{}}}}],
		"weights":[1,5]}},"size":2}`)
	if len(weighted) != 2 || weighted[0] != "recent" {
		t.Errorf("the recency-weighted hybrid ranks %v, want the recent document first: the fourth signal "+
			"did not vote", weighted)
	}

	// And a decay on a field that is not the one bound to Document.Time is
	// refused, rather than answered from a field the scorer never reads.
	if status, raw := do(t, srv, http.MethodPost, "/papers/_search",
		`{"query":{"function_score":{"gauss":{"other":{}}}}}`); status != http.StatusBadRequest {
		t.Errorf("decay on the wrong field: status %d, want 400 (body %s)", status, raw)
	}
}

// A second recency field is refused for the reason a second vector field is: a
// document carries one engine.Document.Time.
func TestOneRecencyFieldPerIndex(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPut, "/papers", `{"mappings":{"properties":{"published":{"type":"date","recency":true}}}}`)

	if status, raw := do(t, srv, http.MethodPut, "/papers/_mapping",
		`{"properties":{"edited":{"type":"date","recency":true}}}`); status != http.StatusBadRequest {
		t.Errorf("a second recency field: status %d, want 400 (body %s)", status, raw)
	}
	// recency on something that is not a date is refused too.
	if status, raw := do(t, srv, http.MethodPut, "/papers/_mapping",
		`{"properties":{"tag":{"type":"keyword","recency":true}}}`); status != http.StatusBadRequest {
		t.Errorf("recency on a keyword: status %d, want 400 (body %s)", status, raw)
	}

	// And an index with no recency field says so when a decay is asked for.
	if status, raw := do(t, srv, http.MethodPut, "/plain", ""); status != http.StatusOK {
		t.Fatalf("create plain: status %d (body %s)", status, raw)
	}
	status, raw := do(t, srv, http.MethodPost, "/plain/_search", `{"query":{"function_score":{"gauss":{"when":{}}}}}`)
	if status != http.StatusBadRequest {
		t.Errorf("decay with no recency field: status %d, want 400 (body %s)", status, raw)
	}
	var e struct {
		Error struct {
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(e.Error.Reason, "recency") {
		t.Errorf("the refusal does not name what would fix it: %s", e.Error.Reason)
	}
}
