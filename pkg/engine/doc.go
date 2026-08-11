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
type DocID uint32

// Document is what a caller hands to Index.Add. Every field feeds a different
// scorer, and the index itself understands none of them beyond Text: Vector is
// read by the vector scorer, Links by the graph scorer, Time by the recency
// scorer. Adding a fifth scorer means adding a field here, not touching
// anything else.
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

// Query carries every scorer's input in one value. A scorer reads the fields it
// understands and ignores the rest.
type Query struct {
	Text   string
	Vector []float32

	// Seeds are document Keys the graph scorer should start from. When empty,
	// the graph scorer falls back to whatever seed scorer it was constructed
	// with. This is the escape hatch that keeps graph proximity usable as an
	// independent scorer instead of a function of text results.
	Seeds []string
}
