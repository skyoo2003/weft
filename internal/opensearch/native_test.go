// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
)

// Milestone 30. The compatibility surface speaks somebody else's language, and
// this one speaks weft's.
//
// Two things live here that the OpenSearch DSL has no way to say, and they are
// the whole of what this round buys:
//
//  1. A request *is* a list of streams. `hybrid` is a wrapper around one and
//     `weights` was an extension to it; here the list is the query, and a weight
//     is an index into a list the client wrote — which is D-005's repayment
//     arriving as the protocol's own shape rather than as a clause inside it.
//  2. The pre-fusion rank of every stream, per hit. `examples/breakdown` and
//     `weft search -breakdown` both show it and an OpenSearch response has
//     nowhere to put it.
//
// The route is `/{index}/_weft/search` and not `/{index}/_search`, for the reason
// D-033 gives: what OpenSearch has no name for gets a name OpenSearch does not
// use. It cannot collide with an index either — validName rejects a leading
// underscore, so no index can be called `_weft`.

// nativeResponse runs a native search and returns the decoded response.
func nativeResponse(t *testing.T, srv *httptest.Server, body string) nativeResult {
	t.Helper()
	status, raw := do(t, srv, http.MethodPost, "/papers/_weft/search", body)
	if status != http.StatusOK {
		t.Fatalf("%s: status %d (body %s)", body, status, raw)
	}
	var res nativeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return res
}

// nativeResult is the response shape these tests read. Deliberately spelled out
// here rather than shared with the implementation: a test that decodes with the
// production struct agrees with it by construction and checks nothing about the
// wire.
type nativeResult struct {
	Hits struct {
		Total struct {
			Value    int    `json:"value"`
			Relation string `json:"relation"`
		} `json:"total"`
		Hits []struct {
			ID string `json:"_id"`
			// One entry per stream, in the order the request named them. A null
			// is the stream having no opinion — the `-` column in
			// examples/breakdown.
			Breakdown []*int `json:"breakdown"`
		} `json:"hits"`
	} `json:"hits"`
}

func (r nativeResult) ids() []string {
	out := make([]string, 0, len(r.Hits.Hits))
	for _, h := range r.Hits.Hits {
		out = append(out, h.ID)
	}
	return out
}

// nativeRefusal runs a native search that is expected to fail and returns the
// status and the reason, so a test can assert the client was told why.
func nativeRefusal(t *testing.T, srv *httptest.Server, body string) (status int, reason string) {
	t.Helper()
	status, raw := do(t, srv, http.MethodPost, "/papers/_weft/search", body)
	var res struct {
		Error struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return status, res.Error.Reason
}

// ------------------------------------------------------- journey 1: streams

// The request is the stream list. No hybrid clause wraps it, and every leaf the
// compatibility surface can compile is a stream here — which is why this is
// eleven clause kinds rather than the four signals: `clause` already knows them
// all, and a second parser would be a second opinion about the same JSON.
func TestNativeSearchNamesStreamsByPosition(t *testing.T) {
	srv := dslCorpus(t)

	res := nativeResponse(t, srv, `{"streams":[
		{"match":{"text":"pottery"}},
		{"knn":{"vec":{"vector":[1,0,0],"k":2}}}],"size":5}`)

	// Text alone finds pottery; the vector end of the space finds rrf. Both
	// arrive, which is fusion doing what a hybrid clause did without a hybrid
	// clause being written.
	for _, want := range []string{"pottery", "rrf"} {
		if !slices.Contains(res.ids(), want) {
			t.Errorf("native search found %v, missing %q — a stream was dropped", res.ids(), want)
		}
	}
}

// The compatibility surface and the native surface are the same engine, and this
// is the test that says so: the same two clauses, once as a hybrid query and once
// as a stream list, rank the same documents.
//
// It is also the round's standing guarantee in miniature. If this passes and
// `make compat` passes, the native surface cost the compatibility surface
// nothing.
func TestTheNativeSurfaceIsTheSameEngineAsTheCompatSurface(t *testing.T) {
	srv := dslCorpus(t)

	viaCompat := hitIDs(t, srv, `{"query":{"hybrid":{"queries":[
		{"match":{"text":"pottery"}},
		{"knn":{"vec":{"vector":[1,0,0],"k":2}}}]}},"size":5}`)

	native := nativeResponse(t, srv, `{"streams":[
		{"match":{"text":"pottery"}},
		{"knn":{"vec":{"vector":[1,0,0],"k":2}}}],"size":5}`).ids()
	slices.Sort(native)

	if !slices.Equal(viaCompat, native) {
		t.Errorf("compat surface found %v and the native surface found %v: the two are supposed to be one "+
			"engine with two spellings", viaCompat, native)
	}
}

// A weight is positional, and that is what lets it exist at all without fusion
// learning what a stream holds. Turning one stream's weight to a hundredth has to
// move the ranking without changing which documents are eligible.
func TestNativeWeightsArePositional(t *testing.T) {
	srv := dslCorpus(t)

	streams := `{"match":{"text":"pottery"}},{"match":{"text":"fusion"}}`
	even := nativeResponse(t, srv, `{"streams":[`+streams+`],"size":5}`)
	first := nativeResponse(t, srv, `{"streams":[`+streams+`],"weights":[1,0.01],"size":5}`)
	second := nativeResponse(t, srv, `{"streams":[`+streams+`],"weights":[0.01,1],"size":5}`)

	// Each stream nominates one document and each nominates it at rank 0, so an
	// unweighted fusion is a tie broken by something neither stream said. The
	// assertion is therefore between the two *weighted* runs rather than against
	// the unweighted one: whichever way the tie falls, discounting one side has
	// to lead with one document and discounting the other has to lead with the
	// other. A test that read the unweighted leader would be asserting on the
	// tiebreak.
	if first.ids()[0] == second.ids()[0] {
		t.Errorf("weights [1,0.01] and [0.01,1] both lead with %q: the weights did not reach "+
			"fusion.FuseWeighted", first.ids()[0])
	}

	// And a weight votes, it does not filter. Discounting a stream to a hundredth
	// must not remove anything it nominated — 0 would, which is D-029's trap and
	// the reason a filter is not spelled as a weight.
	evenIDs, tiltedIDs := even.ids(), second.ids()
	slices.Sort(evenIDs)
	slices.Sort(tiltedIDs)
	if !slices.Equal(evenIDs, tiltedIDs) {
		t.Errorf("weights changed the eligible set from %v to %v: a weight votes, it does not filter",
			evenIDs, tiltedIDs)
	}
}

// An unequal list would weight a stream the client did not mean, silently. The
// compatibility surface refuses this on a hybrid clause and the native surface
// refuses it for the same reason.
func TestNativeWeightsMustMatchTheStreamCount(t *testing.T) {
	srv := dslCorpus(t)

	status, reason := nativeRefusal(t, srv, `{"streams":[
		{"match":{"text":"pottery"}},
		{"match":{"text":"fusion"}}],"weights":[1,1,1]}`)
	if status != http.StatusBadRequest {
		t.Errorf("three weights over two streams: status %d, want 400", status)
	}
	if !strings.Contains(reason, "positional") {
		t.Errorf("the refusal does not say why an unequal list is wrong: %q", reason)
	}
}

// ----------------------------------------------------- journey 2: breakdown

// The field the OpenSearch response has no room for: where each stream put this
// document before fusion saw any of them.
//
// pottery is only in stream 0 and rrf is only in stream 1, so each one's
// breakdown has exactly one number and exactly one null — a null being the
// stream withholding an opinion, which is the `-` in examples/breakdown and not
// a zero.
func TestNativeBreakdownReportsPreFusionRanks(t *testing.T) {
	srv := dslCorpus(t)

	res := nativeResponse(t, srv, `{"streams":[
		{"match":{"text":"pottery"}},
		{"match":{"text":"fusion"}}],"size":5,"breakdown":true}`)

	got := map[string][]*int{}
	for _, h := range res.Hits.Hits {
		got[h.ID] = h.Breakdown
	}
	if len(got) == 0 {
		t.Fatal("no hits, so there is nothing to break down")
	}

	for id, want := range map[string][2]bool{
		// [stream 0 nominated it, stream 1 nominated it]
		"pottery": {true, false},
		"rrf":     {false, true},
	} {
		b, ok := got[id]
		if !ok {
			t.Errorf("%q is not in the hits %v", id, res.ids())
			continue
		}
		if len(b) != 2 {
			t.Errorf("%q has %d breakdown entries, want one per stream (2)", id, len(b))
			continue
		}
		for i, present := range want {
			if present && b[i] == nil {
				t.Errorf("%q: stream %d nominated it but the breakdown says null", id, i)
			}
			if !present && b[i] != nil {
				t.Errorf("%q: stream %d did not nominate it but the breakdown says rank %d", id, i, *b[i])
			}
		}
	}
}

// A column per stream the client wrote, not per scorer the clause became.
//
// This is D-028's trap arriving in the breakdown. A two-token match is two
// query.Glob streams, so the fused stream list is longer than the list in the
// request — and a breakdown that reported scorer positions under stream labels
// would be off by one from the second entry onwards, silently, for exactly the
// queries a person is most likely to send.
//
// "rank fusion" is two tokens and both are in rrf alone; "pottery" is one token
// and is in pottery alone. So entry 0 nominates rrf, entry 1 nominates pottery,
// and the answer has two columns however many scorers ran underneath.
func TestNativeBreakdownHasOneColumnPerStreamNotPerScorer(t *testing.T) {
	srv := dslCorpus(t)

	res := nativeResponse(t, srv, `{"streams":[
		{"match":{"text":"rank fusion"}},
		{"match":{"text":"pottery"}}],"size":5,"breakdown":true}`)

	got := map[string][]*int{}
	for _, h := range res.Hits.Hits {
		got[h.ID] = h.Breakdown
	}
	for id, want := range map[string][2]bool{
		"rrf":     {true, false},
		"pottery": {false, true},
	} {
		b, ok := got[id]
		if !ok {
			t.Errorf("%q is not in the hits %v", id, res.ids())
			continue
		}
		if len(b) != 2 {
			t.Errorf("%q has %d breakdown columns; the request named 2 streams, and a column per scorer "+
				"would put the second stream's rank under the first stream's label", id, len(b))
			continue
		}
		for i, present := range want {
			if present && b[i] == nil {
				t.Errorf("%q: stream %d nominated it but the breakdown says null", id, i)
			}
			if !present && b[i] != nil {
				t.Errorf("%q: stream %d did not nominate it but the breakdown says rank %d", id, i, *b[i])
			}
		}
	}
}

// Not asked for, not paid for. A response without breakdown carries no breakdown
// field at all rather than a column of nulls.
func TestNativeBreakdownIsAbsentUnlessAsked(t *testing.T) {
	srv := dslCorpus(t)

	status, raw := do(t, srv, http.MethodPost, "/papers/_weft/search",
		`{"streams":[{"match":{"text":"pottery"}}],"size":5}`)
	if status != http.StatusOK {
		t.Fatalf("status %d (body %s)", status, raw)
	}
	if strings.Contains(string(raw), "breakdown") {
		t.Errorf("a response that was not asked for a breakdown carries one: %s", raw)
	}
}

// --------------------------------------------------------- journey 3: depth

// depth is the fusion depth and size is the page. On the compatibility surface
// they are tangled — a knn clause's k raises the depth of every stream, and there
// is no way to ask for a deeper fusion of a text-only query. Here they are two
// numbers.
//
// hits.total is how many candidates the fusion produced before the page was cut,
// so holding size at 1 and moving depth is a direct reading of the pool.
func TestNativeDepthIsSeparateFromSize(t *testing.T) {
	srv := dslCorpus(t)

	shallow := nativeResponse(t, srv, `{"streams":[{"prefix":{"text":"rank"}}],"size":1,"depth":1}`)
	deep := nativeResponse(t, srv, `{"streams":[{"prefix":{"text":"rank"}}],"size":1,"depth":3}`)

	if got := len(shallow.Hits.Hits); got != 1 {
		t.Errorf("size 1 returned %d hits", got)
	}
	if got := len(deep.Hits.Hits); got != 1 {
		t.Errorf("size 1 with depth 3 returned %d hits; depth is the pool, not the page", got)
	}
	if shallow.Hits.Total.Value >= deep.Hits.Total.Value {
		t.Errorf("depth 1 fused %d candidates and depth 3 fused %d: depth did not reach the collector",
			shallow.Hits.Total.Value, deep.Hits.Total.Value)
	}
}

// ------------------------------------------------------ journey 4: refusals

// A stream kind nobody registered is a 400 naming what is registered, not a 200
// over the streams that did parse. This is D-026's rule on the native surface:
// the failure pkg/query refuses everywhere else is a query that quietly returns
// nothing.
func TestNativeRefusesAnUnknownStream(t *testing.T) {
	srv := dslCorpus(t)

	status, reason := nativeRefusal(t, srv, `{"streams":[
		{"match":{"text":"pottery"}},
		{"mach":{"text":"fusion"}}]}`)
	if status != http.StatusBadRequest {
		t.Errorf("an unregistered stream kind: status %d, want 400", status)
	}
	if !strings.Contains(reason, "mach") {
		t.Errorf("the refusal does not quote the name that was wrong: %q", reason)
	}
}

// No streams is not an empty ranking. A client that sent this meant to send
// something.
func TestNativeRefusesAnEmptyStreamList(t *testing.T) {
	srv := dslCorpus(t)

	for _, body := range []string{`{"streams":[]}`, `{"size":5}`} {
		status, reason := nativeRefusal(t, srv, body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, status)
		}
		if reason == "" {
			t.Errorf("%s: refused with no reason", body)
		}
	}
}

// depth is a number a request names and engine.NewCollector allocates for, so it
// is refused before it is allocated — the same boundary maxResultWindow already
// is for from and size, and the same one CodeQL found the day the HTTP surface
// created it.
func TestNativeRefusesADepthOverTheWindow(t *testing.T) {
	srv := dslCorpus(t)

	status, reason := nativeRefusal(t, srv,
		`{"streams":[{"match":{"text":"pottery"}}],"depth":2000000000}`)
	if status != http.StatusBadRequest {
		t.Errorf("a depth of two billion: status %d, want 400", status)
	}
	if !strings.Contains(reason, "10000") {
		t.Errorf("the refusal does not name the window it is enforcing: %q", reason)
	}
}

// A nested bool, a hybrid inside a stream, a query_string — the native surface
// inherits every refusal `clause` already makes, because it is the same function.
// This checks the inheritance rather than re-listing the table.
func TestNativeInheritsTheClauseRefusals(t *testing.T) {
	srv := dslCorpus(t)

	for _, tc := range []struct{ name, body, want string }{
		{"hybrid inside a stream", `{"streams":[{"hybrid":{"queries":[{"match":{"text":"a"}}]}}]}`, "hybrid"},
		{clauseQueryString, `{"streams":[{"query_string":{"query":"a"}}]}`, "query string"},
		{"nested bool", `{"streams":[{"bool":{"must":[{"bool":{}}]}}]}`, "bool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, reason := nativeRefusal(t, srv, tc.body)
			if status == http.StatusOK {
				t.Fatalf("%s answered 200; a refusal that becomes an empty result is the failure "+
					"D-026 exists to prevent", tc.name)
			}
			if !strings.Contains(reason, tc.want) {
				t.Errorf("%s: reason %q does not mention %q", tc.name, reason, tc.want)
			}
		})
	}
}

// ------------------------------------------------- the architecture, locked

// nativeFusionFunctions is the native surface's half of what
// TestTheFusionCodeDoesNotKnowAboutTheFourthSignal locks on search.go: the
// functions that decide how streams combine, which must not be able to name what
// any of them holds.
//
// capture is the one this round adds. It wraps a Fuser to record the streams that
// were fused so the breakdown can be built from them — and it has to do that
// without learning that stream 2 is a vector, or the breakdown becomes a fifth
// place that would need editing to add a sixth signal.
var nativeFusionFunctions = []string{"capture", "rankOf"}

func TestTheNativeFusionPathKnowsNoScorer(t *testing.T) {
	src, err := os.ReadFile("native.go")
	if err != nil {
		t.Fatalf("read native.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "native.go", src, 0)
	if err != nil {
		t.Fatalf("parse native.go: %v", err)
	}

	found := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !slices.Contains(nativeFusionFunctions, fn.Name.Name) {
			continue
		}
		found++
		body := string(src[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset])
		// The same list TestTheFusionCodeDoesNotKnowAboutTheFourthSignal uses on
		// search.go, shared rather than copied: two copies would mean a fifth
		// signal reaching one surface's list and not the other's.
		for _, word := range forbiddenInFusion {
			if strings.Contains(body, word) {
				t.Errorf("the native fusion function %s mentions %q: a breakdown reports positions, and a "+
					"position is the one thing that stays true when a sixth signal is added",
					fn.Name.Name, word)
			}
		}
	}
	if found != len(nativeFusionFunctions) {
		t.Errorf("found %d of the %d native fusion functions in native.go; nativeFusionFunctions has gone "+
			"stale and this test is checking less than it says", found, len(nativeFusionFunctions))
	}
}
