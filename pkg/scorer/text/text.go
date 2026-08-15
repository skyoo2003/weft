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
}

// New returns a text scorer reading ix.
func New(ix *engine.Index) *Scorer { return &Scorer{ix: ix} }

// Name implements engine.Scorer.
func (s *Scorer) Name() string { return "text" }

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
	// This bounds the wait at one tokenization, not inside it: Tokenize is shared
	// with Index.Add, which has no context, so polling within it would mean
	// either widening engine's API or keeping a second tokenizer here — and one
	// tokenizer, living in engine, is what keeps engine from importing a scorer
	// (docs/FINDINGS.md section 2.2).
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	terms := engine.Tokenize(q.Text)
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

	// Duplicate query terms are summed twice, which is the formula taken
	// literally: the sum is over occurrences in Q, not over the distinct set.
	//
	// Sized to the corpus rather than grown from nothing: one common term
	// produces one entry per matching document, so an unhinted map re-buckets
	// its way up through every doubling on exactly the queries that cost most.
	acc := make(map[engine.DocID]float64, docs)
	for _, term := range terms {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		posts := s.ix.Lookup(term)
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
			norm := 1.0
			if avgdl > 0 {
				norm = 1 - B + B*float64(s.ix.DocLen(p.Doc))/avgdl
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
