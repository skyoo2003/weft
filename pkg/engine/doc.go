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
// Text, Vector, Links and Time are the four a scorer reads; Key is the caller's
// identifier and no scorer ranks on it. A fifth signal *inside this module* means
// a fifth field here. A scorer written outside it cannot add one, and does not
// need to: nothing in Scorer says where a scorer's data comes from. Keep your own
// table keyed by Document.Key — a map, a database, whatever you already have —
// and use Index.Resolve to turn a Key into the DocID a Candidate carries.
//
// What that costs, stated plainly rather than left as a field to reach for:
// Commit writes documents and knows of nothing else, so a caller-held table is
// not persisted and is rebuilt after every Open. Key is what makes the rebuild
// safe — it still names the same document afterwards, because neither Commit nor
// Merge renumbers a DocID.
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

	// Fields are named texts indexed beside Text, so a query can ask about one
	// of them alone.
	//
	// A slice and not a map, because a map has no order and Commit is
	// byte-deterministic: two indexes holding the same documents have to produce
	// the same segment, and a map would make the bytes depend on Go's hash seed.
	// The order is the caller's and is preserved.
	//
	// This is the one field on Document that is not a signal of its own. It is
	// more Text, scoped: every field is split by the same Tokenizer and its terms
	// go into the same postings under a name, which is what FieldTerm spells. So
	// a field costs a term space and not a section, and nothing in the index has
	// to learn what a field means.
	//
	// What that buys and what it costs, both stated plainly:
	//
	//   - Text is *not* one of them. A query for a term in Text does not match
	//     the same term in a field, and the reverse. A caller wanting both puts
	//     the text in both places, which doubles that document's tokens.
	//   - **DocLen counts every field's tokens.** A document's length is all of
	//     its indexed text, which is forced rather than chosen: field terms are
	//     ordinary postings, and Scrub checks that a document's frequencies
	//     summed across every term equal its stored length. So adding a field
	//     lengthens the document for BM25's purposes, and scorer/text's
	//     field-scoped form answers that by not normalizing by length at all.
	//   - Field names are validated where a document enters: non-empty, free of
	//     the separator byte, and unique within one document. ErrBadField.
	Fields []Field
}

// Field is one named text of a Document.
type Field struct {
	// Name is what a query scopes to. It must be non-empty, must not contain a
	// NUL byte, and must be unique within one Document — Add and Update refuse
	// all three, and a segment holding any of them is refused as corrupt,
	// because a restored index must not hold what a live one cannot.
	Name string
	Text string
}

// fieldSep separates a field's name from a token in the term space.
//
// NUL, because the terms a Tokenizer produces are the strings this has to stay
// out of, and no plausible tokenizer emits one: the default splits at every
// non-letter and non-digit, and a tokenizer yielding a NUL inside a token has
// produced something no query will type. It is one byte, so a field's terms cost
// one byte each over the same terms unscoped.
//
// It is not a checked invariant on the token side. A caller-supplied Tokenizer
// that did emit NUL could produce a plain term equal to some field's term, and
// the two would share a posting list. docs/FORMAT.md section 8 records that;
// checking it would be a byte scan of every token of every document.
const fieldSep = "\x00"

// FieldTerm is the term a token of a named field is indexed under.
//
// It is exported because a scorer outside this module has to be able to build
// the same key in order to look one up — Index.Lookup takes a term, and there is
// no field-aware lookup for it to call instead. That is the same arrangement
// Index.Tokenize makes for the tokenizer: the index owns the rule, and a scorer
// asks rather than reimplementing.
//
// An empty name returns the token unchanged, which is Text's own term space.
// That is not a special case bolted on; it is what makes "no field" spellable in
// the same expression as "some field", so a scorer needs one code path.
func FieldTerm(name, token string) string {
	if name == "" {
		return token
	}
	return name + fieldSep + token
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
// Two scorers needing the *same* per-query value are the same shape, not a new
// one: construct both from that one value. And keep the half of a scorer's input
// that does not change per query out of that path — a corpus-sized side store is
// built once and handed to each scorer, so a per-query construction costs an
// allocation instead of a rebuild.
//
// Do not reuse Seeds for this. The graph scorer reads it, and two scorers
// sharing one field is how one of them silently stops working — with no error,
// because Seeds carries document Keys and a "more like this one" signal from
// outside carries a document Key too, so each side reads a value that is
// well-formed for the other. TestOneQueryTimeValueReachesTwoExternalScorers pins
// both halves of that.
type Query struct {
	Text   string
	Vector []float32

	// Seeds are document Keys the graph scorer should start from. When empty,
	// the graph scorer falls back to whatever seed scorer it was constructed
	// with. This is the escape hatch that keeps graph proximity usable as an
	// independent scorer instead of a function of text results.
	Seeds []string
}
