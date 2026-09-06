// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"encoding/json"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

// fuzzIndex is a one-document index built without going through HTTP, because a
// fuzz target should not spend a TCP connection per input.
func fuzzIndex(f *testing.F) *Index {
	f.Helper()
	reg, err := OpenRegistry(f.TempDir())
	if err != nil {
		f.Fatalf("OpenRegistry: %v", err)
	}
	f.Cleanup(func() { reg.Close() }) //nolint:errcheck // the fuzz run is over

	x, err := reg.Create("papers")
	if err != nil {
		f.Fatalf("create: %v", err)
	}
	if err := x.Remap(mappingDoc{Properties: map[string]property{
		"views": {Type: typeInteger},
		"when":  {Type: typeDate},
		"tag":   {Type: typeKeyword},
	}}); err != nil {
		f.Fatalf("remap: %v", err)
	}
	d, err := documentFrom("1", []byte(`{"text":"reciprocal rank fusion","tag":"published","views":42,"when":"2024-03-01"}`), x.Mapping())
	if err != nil {
		f.Fatalf("documentFrom: %v", err)
	}
	if _, err := x.Put(d, []byte(`{"text":"reciprocal rank fusion"}`)); err != nil {
		f.Fatalf("put: %v", err)
	}
	return x
}

// FuzzParseDSL points a fuzzer at the one place in this process that reads bytes a
// stranger chose.
//
// The segment decoder has had FuzzSegmentDecoding since milestone 2 because a
// corrupt file is the library's hostile input. The DSL parser is the server's, and
// it is worse in one way: a file has to be put on the disk, and a search body only
// has to be sent.
//
// # What a failure looks like
//
// A panic. Not a wrong answer — the corpus here holds one document and the
// property is not about which documents come back. parseSearch either returns a
// plan or returns an apiError and there is no third outcome; an index out of range
// in a bounds slice, a nil map write, or a stack overflow from a nesting depth
// nobody bounded would each show up as one.
//
// # Why the plan is also run
//
// Parsing is half of it. A plan that parses and then panics in fusion — a stream
// index past the end of the list, a filter position that no longer exists after a
// clause was collapsed into an anyOf — is the failure this package's position
// arithmetic can actually have, and it is only reachable by executing. query.Must
// and query.MustNot answer an out-of-range index with nil rather than with a
// panic, and that is a property this asserts rather than assumes.
func FuzzParseDSL(f *testing.F) {
	for _, tc := range dslTable {
		f.Add(tc.body)
	}
	f.Add(`{"query":{"bool":{"must":[],"should":[],"filter":[],"must_not":[]}}}`)
	f.Add(`{"query":{"match":{"text":{"query":"a","operator":"and","fuzziness":"AUTO"}}}}`)
	f.Add(`{"query":{"range":{"views":{"gt":-9223372036854775808,"lt":9223372036854775807}}}}`)
	f.Add(`{"query":{"terms":{"text":[1,true,"a",null]}}}`)
	f.Add(`{"query":{"wildcard":{"text":"[a-"}}}`)
	f.Add(`{"size":0,"from":0}`)
	f.Add("")

	x := fuzzIndex(f)

	f.Fuzz(func(t *testing.T, body string) {
		p, apiErr := parseSearch(x, []byte(body))
		if apiErr != nil {
			// Every refusal carries a status a client can branch on and a reason a
			// person can act on. A 2xx is not among them — that is the shape D-026
			// exists to prevent, and it is checked here as well as in the table
			// test because a fuzzer reaches bodies the table does not.
			if apiErr.status < 400 {
				t.Fatalf("refusal with status %d for %q", apiErr.status, body)
			}
			if apiErr.reason == "" {
				t.Fatalf("refusal with an empty reason for %q", body)
			}
			return
		}
		// Executed, not only parsed. An error out of runSearch is a legitimate
		// outcome — a malformed glob is reported by Candidates rather than at parse
		// time — so what this asserts is that the line after it is reached at all.
		_, _, _, _ = runSearch(t.Context(), x, p)
	})
}

// FuzzParseNative is the same argument for the third body this server reads.
//
// The native surface shares a compiler with the DSL, so what is new here is not
// the clauses — it is the position arithmetic around them. A request names N
// entries, the compiler turns them into M scorers with M >= N, the fuser is
// handed M streams, and the breakdown has to come back out with N columns. Four
// lengths that have to agree, and the two that disagreed on the first multi-token
// query are the ones TestNativeBreakdownHasOneColumnPerStreamNotPerScorer now
// pins: `groups` exists because they did.
//
// # What a failure looks like
//
// An index out of range in rankOf, a column count that is not the entry count, or
// a rank pointing past the end of a stream. None of those is reachable from the
// table tests, because a table test writes the entries it already believes in.
func FuzzParseNative(f *testing.F) {
	f.Add(`{"streams":[{"match":{"text":"rank fusion"}}]}`)
	f.Add(`{"streams":[{"match":{"text":"a b c"}},{"term":{"tag":"published"}}],"weights":[1,0.1],"breakdown":true}`)
	f.Add(`{"streams":[{"bool":{"must":[{"match":{"text":"a"}}],"filter":[{"term":{"tag":"x"}}]}}],"breakdown":true}`)
	f.Add(`{"streams":[{"range":{"views":{"gte":0}}}],"size":1,"from":0,"depth":10000,"breakdown":true}`)
	f.Add(`{"streams":[],"weights":[1]}`)
	f.Add(`{"streams":[{"match":{"text":"a"}}],"depth":-1}`)
	f.Add("")

	x := fuzzIndex(f)

	f.Fuzz(func(t *testing.T, body string) {
		var req nativeRequest
		if body != "" && json.Unmarshal([]byte(body), &req) != nil {
			return // the handler answers this with a 400 and never reaches the plan
		}
		if len(req.Streams) == 0 {
			return
		}

		p := plan{size: defaultSize}
		if apiErr := readNativeWindow(&p, req); apiErr != nil {
			if apiErr.status < 400 || apiErr.reason == "" {
				t.Fatalf("refusal with status %d and reason %q for %q", apiErr.status, apiErr.reason, body)
			}
			return
		}
		c := &compiler{ix: x.Engine(), m: x.Mapping(), p: &p}
		groups, apiErr := c.streams(req.Streams, req.Weights, "stream")
		if apiErr != nil {
			if apiErr.status < 400 || apiErr.reason == "" {
				t.Fatalf("refusal with status %d and reason %q for %q", apiErr.status, apiErr.reason, body)
			}
			return
		}
		// One entry per scorer, which is what the breakdown's column count rests
		// on. A clause that added a stream without the loop noticing would break
		// this here rather than in a client's display.
		if len(groups) != len(p.scorers) {
			t.Fatalf("%d groups for %d scorers in %q", len(groups), len(p.scorers), body)
		}

		ranks := map[engine.DocID][]int{}
		p.fuse = capture(p.fuser(), ranks)
		hits, _, _, err := runSearch(t.Context(), x, p)
		if err != nil {
			return // a malformed glob is reported by Candidates, not at parse time
		}
		attach(x, hits, ranks, groups, len(req.Streams))

		for _, h := range hits {
			if len(h.Breakdown) != len(req.Streams) {
				t.Fatalf("hit %q has %d breakdown columns for %d streams in %q",
					h.ID, len(h.Breakdown), len(req.Streams), body)
			}
			for i, at := range h.Breakdown {
				if at != nil && *at < 0 {
					t.Fatalf("hit %q reports rank %d in stream %d for %q", h.ID, *at, i, body)
				}
			}
		}
	})
}

// FuzzParseBulk is the same argument for the other body this server reads.
//
// _bulk is line-oriented, which adds a failure the search body does not have: the
// action line and the source line are paired by position, so an off-by-one in the
// scan indexes documents under each other's ids without an error anywhere. What
// this asserts is the invariant that pairing rests on — one target per action, and
// every action carrying the id the handler will write it under.
func FuzzParseBulk(f *testing.F) {
	f.Add("{\"index\":{\"_index\":\"papers\",\"_id\":\"a\"}}\n{\"text\":\"a\"}\n")
	f.Add("{\"delete\":{\"_index\":\"papers\",\"_id\":\"a\"}}\n")
	f.Add("{\"create\":{\"_id\":\"a\"}}\n{}\n")
	f.Add("{}\n{}\n")
	f.Add("\n\n\n")
	f.Add("")

	f.Fuzz(func(t *testing.T, body string) {
		actions, targets, apiErr := parseBulk([]byte(body), "papers")
		if apiErr != nil {
			if apiErr.status < 400 || apiErr.reason == "" {
				t.Fatalf("refusal with status %d and reason %q for %q", apiErr.status, apiErr.reason, body)
			}
			return
		}
		// The handler reads the two slices in lockstep, so a batch where they
		// disagree in length would write a document against the wrong index name.
		if len(actions) != len(targets) {
			t.Fatalf("%d actions and %d targets for %q", len(actions), len(targets), body)
		}
		for i, a := range actions {
			if a.id == "" || targets[i] == "" {
				t.Fatalf("action %d has an empty id or target for %q", i, body)
			}
			if a.op != opDelete && a.body == nil {
				t.Fatalf("action %d is a write with no source line for %q", i, body)
			}
			// Parsing does not resolve the document. documentFrom is what refuses a
			// source line that is not an object, and it runs against the index's
			// mapping — so an action arriving here already mapped would have been
			// mapped by nothing.
			if a.doc.Key != "" {
				t.Fatalf("action %d arrived pre-resolved for %q", i, body)
			}
		}
	})
}
