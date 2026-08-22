// SPDX-License-Identifier: Apache-2.0

// Package vector ranks documents by cosine similarity to Query.Vector.
//
// Embeddings are supplied by the caller: generating them is out of scope for
// weft, so this package only compares vectors it is handed.
package vector

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/skyoo2003/weft/pkg/engine"
)

// ErrDimMismatch reports a document whose vector width differs from the query's.
// It is an error rather than a skip because it means the caller mixed embedding
// models, and silently ranking the subset that happens to match would hide that.
var ErrDimMismatch = errors.New("vector: dimension mismatch")

// Scorer ranks documents by cosine similarity.
type Scorer struct {
	ix *engine.Index
}

// New returns a vector scorer reading ix.
func New(ix *engine.Index) *Scorer { return &Scorer{ix: ix} }

// Name implements engine.Scorer.
func (s *Scorer) Name() string { return "vector" }

// Candidates implements engine.Scorer.
//
// The loop runs over engine.Index.Nearest rather than over every DocID, which
// is milestone 3b's repayment of the full scan this used to be. What that
// changes here is one line: the index says which documents are geometrically
// plausible, and every rule below about norms, widths and cancellation is
// untouched, because the metric never left this file. Nearest returns a
// superset — documents with no vector, with a zero vector, or held by a segment
// with no partition are all in it — so the skips below still have work to do.
//
// It is an approximate index, and the honest consequence is that the top k here
// is the top k among the candidates rather than the top k in the corpus.
// docs/EVAL.md carries the measured recall against a brute-force scan and the
// nDCG that survived it; a segment with no partition answers with every id, so
// on those the result is exact.
//
// ponytail: a candidate costs a whole record. engine.Index.Doc decodes the key,
// the text and the links to reach the vector, so the arithmetic fell by the
// partition's factor and the allocation only fell by it — it did not change
// shape. The repayment is a `vectors` section the reader can touch without the
// rest of the record, and the trigger is a measured per-query working set more
// than twice what §1 of the milestone 3b plan predicted; docs/FINDINGS.md
// records which way that measurement came out.
func (s *Scorer) Candidates(ctx context.Context, q engine.Query, k int) ([]engine.Candidate, error) {
	// Before the early return, not after. A text-only query gives this scorer no
	// opinion, and returning that as a success on a context that was already
	// dead makes a blown deadline indistinguishable from an honest miss — which
	// then propagates, since graph.seedDocs can be reading this scorer.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if k <= 0 || len(q.Vector) == 0 {
		return nil, nil
	}
	qNorm, err := norm(ctx, q.Vector)
	if err != nil {
		return nil, err
	}
	if math.IsNaN(qNorm) || math.IsInf(qNorm, 0) {
		// A NaN or infinite component would make every cosine score NaN, and
		// NaN scores sort arbitrarily — a NaN document can land above one
		// scoring 0.9, which fusion then reports as a plausible result. This is
		// a caller bug, so it is an error rather than an empty result.
		// engine.Add rejects such vectors, so only the query can carry one.
		return nil, fmt.Errorf("query vector: %w", engine.ErrNonFiniteVector)
	}
	if qNorm == 0 {
		// A zero query vector has no direction. No opinion, and no division by
		// zero below.
		return nil, nil
	}

	ids := s.ix.Nearest(q.Vector, k)
	cands := make([]engine.Candidate, 0, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Vector rather than Doc, and that difference is the whole of milestone 8's
		// memory work: Doc decodes the key, the text and the links to hand back a
		// document this loop reads one field of, once per candidate. See
		// engine.Index.Vector.
		dv, ok := s.ix.Vector(id)
		if !ok {
			continue // No vector on this document: this scorer has no opinion.
		}
		if len(dv) != len(q.Vector) {
			// The id rather than the key: naming the document would cost the decode
			// this loop exists to avoid, on a path that is failing anyway.
			// engine.Doc turns the id back into a key for a caller that wants one.
			return nil, fmt.Errorf("doc %d has %d dims, query has %d: %w",
				id, len(dv), len(q.Vector), ErrDimMismatch)
		}
		dNorm, err := norm(ctx, dv)
		if err != nil {
			return nil, err
		}
		if dNorm == 0 {
			continue // Zero document vector: no direction, and no 0/0.
		}
		sum, err := dot(ctx, q.Vector, dv)
		if err != nil {
			return nil, err
		}
		// Cosine is in [-1,1]; negatives are kept and sort to the bottom rather
		// than being clamped, because fusion consumes rank, not magnitude.
		cands = append(cands, engine.Candidate{
			Doc:   id,
			Score: sum / (qNorm * dNorm),
		})
	}

	// TopK sorts, so a cancellation arriving after the last document would
	// otherwise still pay for an O(n log n) sort of results nobody will read,
	// and then report success past the deadline.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return engine.TopK(cands, k), nil
}

// dot accumulates in float64 even though the inputs are float32, so long
// vectors do not lose precision to repeated float32 rounding.
//
// It takes a ctx because the width of a vector is caller-supplied and this is
// the scorer's dominant cost: the per-document check in Candidates leaves O(d)
// work between polls, and the query norm runs before that loop starts, so
// without a poll here an already-cancelled context still pays for a full scan.
// Every 1024 components, matching the posting and link scans in the other
// scorers.
func dot(ctx context.Context, a, b []float32) (float64, error) {
	var sum float64
	for i := range a {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		sum += float64(a[i]) * float64(b[i])
	}
	return sum, nil
}

func norm(ctx context.Context, v []float32) (float64, error) {
	sum, err := dot(ctx, v, v)
	return math.Sqrt(sum), err
}
