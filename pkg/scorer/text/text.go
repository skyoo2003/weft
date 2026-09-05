// SPDX-License-Identifier: Apache-2.0

// Package text scores documents with BM25 over the index's inverted index.
//
// It is one of four interchangeable scorers. Nothing outside this package knows
// it computes BM25, and this package knows nothing about the other scorers or
// about fusion.
package text

import (
	"context"
	"fmt"
	"math"

	"github.com/skyoo2003/weft/pkg/engine"
)

// BM25 parameters, fixed at the conventional defaults.
//
// ponytail: constants, not knobs. Milestone 4 is where there is a quality
// metric to tune them against; a config struct before then is a setting nobody
// can evaluate.
const (
	K1 = 1.2
	B  = 0.75
)

// Scorer ranks documents by BM25 over Query.Text.
type Scorer struct {
	ix *engine.Index

	// field is the Document field this scorer reads, or "" for Document.Text.
	//
	// Bound at construction and never per query, which is the rule Query's own
	// documentation states for any input that does not change between queries. A
	// caller searching two fields builds two scorers and passes both to Search,
	// where they are two streams and fusion decides between them — which is the
	// same shape as text and vector, one level down.
	field string

	// b is the length-normalization coefficient, B for Text and 0 for a field.
	// NewField argues that.
	b float64
}

// New returns a text scorer reading ix.
func New(ix *engine.Index) *Scorer { return &Scorer{ix: ix, b: B} }

// NewField returns a text scorer reading one named Document field instead of
// Document.Text.
//
// A field's tokens are indexed under engine.FieldTerm, so this looks up
// `field\x00token` where New looks up `token`. Nothing else about the scoring
// changes and nothing in engine had to learn what a field is — which is the
// whole of why fields cost a term space rather than a section.
//
// # It does not normalize by length, and that is the interesting decision
//
// engine.Document.Fields records the constraint this works around: a document's
// stored length counts **every field's tokens plus Text's**, because field terms
// are ordinary postings and Scrub checks that a document's frequencies summed
// across all terms equal its stored length. There is no per-field length on
// disk to normalize against.
//
// Normalizing a title match by the whole document's length is worse than not
// normalizing at all. A five-token title inside a five-thousand-token document
// would be divided by a thousand times the length it actually has, so every
// title match in a long document scores near zero and the ranking becomes a
// ranking of document brevity. So B is 0 here: `1 - B + B·|D|/avgdl` collapses
// to 1, and the score is IDF times saturating term frequency.
//
// What that costs is real and is not hidden: two documents whose fields match
// equally well are not separated by the field being shorter in one of them. The
// fix is a per-field length in the record, which is a **format v6** and is not
// bought before something measures that it is needed.
//
// An empty name is New — engine.FieldTerm returns the token unchanged — so a
// caller building scorers from configuration needs no branch.
func NewField(ix *engine.Index, field string) *Scorer {
	return &Scorer{ix: ix, field: field}
}

// Name implements engine.Scorer. A field-scoped scorer names its field, so a
// caller fusing several of them can tell the streams apart in a diagnostic.
func (s *Scorer) Name() string {
	if s.field == "" {
		return "text"
	}
	return "text:" + s.field
}

// Candidates implements engine.Scorer.
//
// The scoring is standard BM25 with the log(1 + ...) IDF form:
//
//	IDF(q)   = ln(1 + (N - n(q) + 0.5) / (n(q) + 0.5))
//	score(D) = Σ IDF(q) · f(q,D)·(K1+1) / (f(q,D) + K1·(1 - B + B·|D|/avgdl))
//
// That IDF form is chosen over the classic ln((N - n + 0.5)/(n + 0.5)) because
// the classic one goes negative for a term appearing in more than half the
// corpus, which lets a common term subtract from a document's score. Here the
// argument to ln is always > 1, so IDF is always > 0.
func (s *Scorer) Candidates(ctx context.Context, q engine.Query, k int) ([]engine.Candidate, error) {
	if k <= 0 {
		return nil, nil
	}
	// Before tokenizing, not after. Query.Text is caller-supplied and unbounded,
	// and a text of pure punctuation tokenizes to nothing, so the early return
	// below would otherwise report success on a context that was already dead.
	//
	// This bounds the wait at one tokenization, not inside it: the tokenizer is
	// shared with Index.Add, which has no context, so polling within it would mean
	// either widening engine's API or keeping a second tokenizer here — and one
	// tokenizer, living in engine, is what keeps engine from importing a scorer
	// (docs/FINDINGS.md section 2.2).
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Asked of the index rather than held here or received through Query: index
	// time and query time have to split text the same way, and the index is what
	// knows which way. Same shape as the ix.Stats and ix.LookupInto calls below.
	terms := s.ix.Tokenize(q.Text)
	if len(terms) == 0 {
		return nil, nil
	}
	// One call, one lock: N and avgdl must describe the same moment or the
	// normalization term is computed against a corpus size that never existed.
	docs, avgdl := s.ix.Stats()
	if docs == 0 {
		return nil, nil
	}
	n := float64(docs)

	// Scoped in place, before anything looks a term up. The plain scorer passes
	// an empty field name and engine.FieldTerm hands the token back unchanged, so
	// both forms take one path — and the scoping happens once rather than at each
	// of the two places below that read a term, which is one place for it to be
	// forgotten instead of two.
	//
	// In place is safe: Tokenize's contract is that the slice it returns becomes
	// the caller's.
	for i, tok := range terms {
		terms[i] = engine.FieldTerm(s.field, tok)
	}

	// Duplicate query terms are summed twice, which is the formula taken
	// literally: the sum is over occurrences in Q, not over the distinct set.

	// One cursor per term and one block-sized buffer behind each, walked in step.
	// A document's score is complete the moment every cursor has passed it, so it
	// is offered and forgotten — there is no accumulator and no candidate slice
	// built out of one, and no posting list held whole.
	//
	// That is three corpus-sized structures removed, and docs/FINDINGS.md
	// milestone 22 is why: measured against bleve, weft was faster on every query
	// shape and allocated 156x more on the widest, 2.82 MB against 18 KB. All of
	// the difference was sized by the corpus rather than by the answer.
	//
	// Term-at-a-time is what this replaces, and the invariant it held is worth
	// naming because it is the one being traded: it walked a term to exhaustion
	// and reused one buffer, so a query held the longest list rather than the sum
	// of them. Document-at-a-time cannot walk one term to exhaustion — a document
	// may be reached by any of them — so it would have held the sum, which is
	// worse for a query whose terms are all common. A block at a time is what
	// makes it hold neither: the cost is blockSize postings per term whatever the
	// corpus holds.
	cs, err := s.open(ctx, terms, n)
	if err != nil {
		return nil, err
	}
	if len(cs) == 0 {
		return nil, nil
	}

	return s.walk(ctx, cs, avgdl, k)
}

// blockSize is how many postings a cursor is handed at once. It is the format's
// block size; a buffer shorter than that makes engine.BlockCursor.Next grow one,
// which is the allocation this whole path exists to avoid.
//
// ponytail: a constant duplicated from the format rather than read from it.
// engine does not export it, and exporting a number a caller must not depend on
// to be correct — Next grows a short buffer rather than failing — would be
// exporting a performance hint as an API.
const blockSize = 128

// termCursor is one query term's position in the document-at-a-time walk: a
// block of postings, an index into it, and the IDF that block's term carries.
type termCursor struct {
	cur *engine.BlockCursor
	idf float64
	buf []engine.Posting
	blk []engine.Posting
	at  int
	ok  bool
}

func (c *termCursor) doc() engine.DocID { return c.blk[c.at].Doc }
func (c *termCursor) freq() int         { return c.blk[c.at].Freq }

// fill takes the next block, or reports that the term is finished.
func (c *termCursor) fill() bool {
	c.blk = c.cur.Next(c.buf)
	c.at = 0
	c.ok = len(c.blk) > 0
	return c.ok
}

// advance steps to the next posting, refilling from the cursor at a block
// boundary.
func (c *termCursor) advance() bool {
	c.at++
	if c.at < len(c.blk) {
		return true
	}
	return c.fill()
}

// open builds one cursor per query term that any document holds.
//
// Split out of Candidates because the two halves answer different questions and
// the gate said so: what a term's IDF is, and what a document's score is. They
// share only the cursor list.
func (s *Scorer) open(ctx context.Context, terms []string, n float64) ([]termCursor, error) {
	cs := make([]termCursor, 0, len(terms))
	for _, term := range terms {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// The count comes from the index rather than from the walk, because IDF has
		// to be known before the first posting is scored and a cursor cannot say
		// how long it is without being walked. That is the one extra pass
		// document-at-a-time pays over term-at-a-time, and PostingCount is the
		// cheap kind: postings decoded and counted, never collected.
		//
		// Exact rather than bounded, deliberately. A term's blocks are all full but
		// the last, so an upper bound is one varint away — and an IDF computed from
		// one drifts every score in the corpus by an amount that depends on where
		// the block boundaries fell, which is a ranking that moves when a document
		// is added elsewhere.
		nq := math.Min(float64(s.ix.PostingCount(term)), n)
		if nq == 0 {
			continue // n(q) = 0: a term in no document contributes nothing.
		}
		c := termCursor{
			cur: s.ix.BlockCursor(term),
			// Clamped for the reason the old path clamped: a concurrent Add can
			// leave more postings for a term than there were documents when Stats
			// answered, and an unclamped count drives the log below 1 and the IDF
			// negative — exactly what the ln(1 + ...) form was chosen to rule out.
			idf: math.Log(1 + (n-nq+0.5)/(nq+0.5)),
			buf: make([]engine.Posting, blockSize),
		}
		if !c.fill() {
			if err := c.cur.Err(); err != nil {
				return nil, fmt.Errorf("term %q: %w", term, err)
			}
			continue
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// walk is the document-at-a-time traversal: every cursor in step, one document
// scored to completion at a time, nothing kept but the best k.
func (s *Scorer) walk(ctx context.Context, cs []termCursor, avgdl float64, k int) ([]engine.Candidate, error) {
	// Bounded by k rather than by how many documents match, which is the point.
	out := engine.NewCollector(k)
	for steps := 0; ; steps++ {
		// The smallest DocID any cursor still points at. A linear scan over the
		// cursors, because there is one per query term and a heap over a handful of
		// elements costs more than it saves.
		doc, live := engine.DocID(0), false
		for i := range cs {
			if cs[i].ok && (!live || cs[i].doc() < doc) {
				doc, live = cs[i].doc(), true
			}
		}
		if !live {
			break
		}
		// Cancellation has to be observable inside this loop and not only per
		// term: a single-term query over a common term is the largest walk and has
		// no other check. Every 1024 documents, which keeps ctx.Err's lock off the
		// per-document path.
		if steps&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}

		// avgdl == 0 means every document is empty, so there is nothing to
		// normalize against; norm stays 1 rather than dividing by zero.
		//
		// One DocLen per *document* now, where term-at-a-time paid one per
		// posting — a document matching three query terms took the index-wide read
		// lock three times for one length. The ponytail note that stood here asked
		// for a batched length read; document-at-a-time answers the same question
		// by needing the length once.
		//
		// s.b, not B: a field-scoped scorer sets it to 0 because the only length on
		// disk is the whole document's, and normalizing a title by it ranks by
		// document brevity. NewField argues that.
		norm := 1.0
		if avgdl > 0 && s.b > 0 {
			norm = 1 - s.b + s.b*float64(s.ix.DocLen(doc))/avgdl
		}

		score := 0.0
		for i := range cs {
			if !cs[i].ok || cs[i].doc() != doc {
				continue
			}
			f := float64(cs[i].freq())
			score += cs[i].idf * f * (K1 + 1) / (f + K1*norm)
			if !cs[i].advance() {
				if err := cs[i].cur.Err(); err != nil {
					return nil, err
				}
			}
		}
		// Complete: every cursor is past this document, so nothing can add to it.
		out.Offer(doc, score)
	}

	// Before Take, which sorts what it kept. A cancellation arriving after the
	// last poll would otherwise still buy a sort nobody will read — the same
	// placement the term-at-a-time path gave TopK.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out.Take(), nil
}
