// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"testing"
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
