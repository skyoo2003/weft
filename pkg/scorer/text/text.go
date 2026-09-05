// SPDX-License-Identifier: Apache-2.0

// Package text scores documents with BM25 over the index's inverted index.
//
// It is one of four interchangeable scorers. Nothing outside this package knows
// it computes BM25, and this package knows nothing about the other scorers or
// about fusion.
package text

import (
	"context"
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
	//
	// Sized to the widest posting list this query will actually walk, not to the
	// corpus, and not to whichever term happened to be typed first.
	//
	// The corpus hint that used to stand here was insurance against one common
	// term re-bucketing its way up through every doubling — a real cost, on the
	// queries that are already the most expensive — and it charged every other
	// query for it. BenchmarkCandidates priced the premium: a query for a term
	// **no document holds** allocated 4.73 MB and 143 µs building a map that
	// never received an entry, and that was the floor under every query on the
	// corpus whatever it reached. At the 27 queries/s where docs/FINDINGS.md
	// milestone 5 §3.2 saw sustained load collapse, that floor is 128 MB/s of
	// garbage produced before a single document is scored.
	//
	// Sizing from the first term instead only moves the cost: on a three-term
	// query whose rarest term comes first it grew its way up to the widest list
	// and used 42% *more* memory than the corpus hint did. PostingBound is what
	// makes the third option affordable — one varint per term per segment, no
	// postings decoded — so the hint is the real answer rather than a guess at
	// it, and the widest list is what the union cannot exceed by more than the
	// other terms' disjoint documents.
	//
	// Bounded by the corpus, because the bound is per term and a query naming a
	// term twice would otherwise ask for twice the corpus.
	widest := 0
	for _, term := range terms {
		widest = max(widest, s.ix.PostingBound(term))
	}
	acc := make(map[engine.DocID]float64, min(widest, docs))
	// One buffer for every term, not one list per term. A term's postings are walked and
	// finished with before the next term is looked up, so what has to be live is the
	// longest list rather than the sum of them — and the sum is what LookupInto's doc
	// comment prices at 53.2% of a query's allocation on the evaluation corpus.
	//
	// Reassigned from the return value rather than only passed in: LookupInto grows the
	// array when a longer list arrives, and dropping the grown slice would allocate that
	// growth again on the next query term.
	var posts []engine.Posting
	for _, term := range terms {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		posts = s.ix.LookupInto(term, posts)
		if len(posts) == 0 {
			continue // n(q) = 0: a term in no document contributes nothing.
		}
		// Postings are fetched after Stats, so a concurrent Add can leave more
		// postings for a term than there were documents. Unclamped that makes
		// N - n(q) negative, which drives the log below 1 and the IDF negative —
		// exactly what the ln(1 + ...) form was chosen to rule out. Clamping
		// keeps IDF > 0 always; a true snapshot is milestone 2 work
		// (docs/FINDINGS.md section 4.4).
		nq := math.Min(float64(len(posts)), n)
		idf := math.Log(1 + (n-nq+0.5)/(nq+0.5))

		for i, p := range posts {
			// Cancellation has to be observable inside this loop, not just once per
			// term. A single-term query over a common term is both the largest
			// posting list and the only case with no further per-term check, so
			// without this the scorer finishes the whole scan and returns results
			// after the caller has given up. Polling every 1024 postings keeps
			// ctx.Err's lock off the per-posting path.
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			f := float64(p.Freq)
			// avgdl == 0 means every document is empty, so there is nothing to
			// normalize against; norm stays 1 rather than dividing by zero.
			//
			// ponytail: DocLen takes the index-wide RLock once per posting, so a
			// million-posting term is a million lock acquisitions and the scan
			// gets slower as cores are added. Batch it — a length snapshot read
			// under one lock, the same aliasing contract Lookup already has —
			// when scorer throughput is measured rather than assumed.
			// s.b, not B: a field-scoped scorer sets it to 0 because the only
			// length on disk is the whole document's, and normalizing a title
			// by it ranks by document brevity. NewField argues that.
			norm := 1.0
			if avgdl > 0 && s.b > 0 {
				norm = 1 - s.b + s.b*float64(s.ix.DocLen(p.Doc))/avgdl
			}
			acc[p.Doc] += idf * f * (K1 + 1) / (f + K1*norm)
		}
	}
	if len(acc) == 0 {
		return nil, nil
	}
	// TopK sorts, so a cancellation arriving after the last poll would otherwise
	// still pay for an O(n log n) sort of results nobody will read.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cands := make([]engine.Candidate, 0, len(acc))
	for doc, score := range acc {
		cands = append(cands, engine.Candidate{Doc: doc, Score: score})
	}
	return engine.TopK(cands, k), nil
}
