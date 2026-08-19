// SPDX-License-Identifier: Apache-2.0

// Package engine holds the types every scorer shares and the in-memory index
// they read from.
//
// engine imports no other weft package, and nothing in here names a specific
// scorer. That is not a style preference: it is the architecture claim of
// milestone 1, and `go list -deps` is what enforces it.
package engine

import "time"

// DocID is the index-internal identity of a document, assigned by Index.Add in
// insertion order. It is a dense small integer so scorers can use it to index
// into slices.
//
// A DocID means nothing outside the Index that assigned it. Two indexes both
// start at 0, so the same DocID names a different document in each, and the
// value carries nothing that says which index it came from. Anything that
// compares DocIDs — TopK, any Fuser — is therefore correct only when every
// stream came from one index. Search states this as a precondition; making it
// checkable is milestone 2 work, alongside the segment identity that deletion
// and merge need anyway.
type DocID uint32

// Document is what a caller hands to Index.Add. Every field feeds a different
// scorer, and the index itself understands none of them beyond Text: Vector is
// read by the vector scorer, Links by the graph scorer, Time by the recency
// scorer.
//
// These are the fields weft's own scorers read, and adding a fifth of those
// means adding a field here. A scorer written outside this module cannot do
// that, and does not need to: nothing in Scorer says where a scorer's data comes
// from. Keep your own table keyed by Document.Key — a map, a database, whatever
// you already have — and use Index.Resolve to turn a Key into the DocID a
// Candidate carries. Only data that must survive Commit needs a field here,
// because Commit writes documents and knows of nothing else, so a caller-held
// table is rebuilt after every Open. Key is what makes that rebuild safe: it
// still names the same document afterwards.
//
// Both trials in docs/ADOPTION.md reached for a field here first, and this
// paragraph is what they were missing.
type Document struct {
	// Key is the caller's own stable identifier. Links reference other
	// documents by Key, not DocID, so a document may link to a document that
	// has not been added yet.
	Key string

	Text   string
	Vector []float32

	// Links are Keys of related documents. Keys that never get added stay
	// dangling and are skipped at traversal time rather than rejected here.
	Links []string

	Time time.Time
}

// Candidate is one document with one scorer's opinion of it.
type Candidate struct {
	Doc DocID

	// Score is on whatever scale the producing scorer uses: BM25 is unbounded,
	// cosine is [-1,1], graph proximity is (0,1]. Fusion deliberately does not
	// know which, so it must not compare Score across streams. Rank is the only
	// cross-stream currency.
	Score float64
}

// Query carries the input of every scorer in this module. A scorer reads the
// fields it understands and ignores the rest.
//
// Like Document, it is closed: a scorer outside this module cannot add a field,
// so a signal needing an input that changes per query — a searcher's location, a
// tenant, a personalization profile — receives it another way. Bind it when you
// construct the scorer and construct one per search. recency.NewAt does exactly
// this with a clock, and a scorer value is small enough that building one per
// query costs an allocation rather than any corpus work. Search also passes its
// context to every Candidates call unmodified, so a context value reaches a
// scorer too; prefer the constructor, because a missing context value is a
// runtime surprise where a missing constructor argument will not compile.
//
// Do not reuse Seeds for this. The graph scorer reads it, and two scorers
// sharing one field is how one of them silently stops working.
type Query struct {
	Text   string
	Vector []float32

	// Seeds are document Keys the graph scorer should start from. When empty,
	// the graph scorer falls back to whatever seed scorer it was constructed
	// with. This is the escape hatch that keeps graph proximity usable as an
	// independent scorer instead of a function of text results.
	Seeds []string
}
