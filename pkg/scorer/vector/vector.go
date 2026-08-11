// Package vector ranks documents by cosine similarity to Query.Vector.
//
// Embeddings are supplied by the caller (PRD: model inference is out of scope).
// This package only compares vectors it is handed.
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
// ponytail: brute-force full scan, O(n·d) per query. HNSW or IVF is milestone 3
// — building an approximate index before the architecture is proven would mean
// optimizing a structure that may not survive.
func (s *Scorer) Candidates(ctx context.Context, q engine.Query, k int) ([]engine.Candidate, error) {
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

	n := s.ix.Len()
	cands := make([]engine.Candidate, 0, n)
	for i := range n {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := engine.DocID(i)
		d, ok := s.ix.Doc(id)
		if !ok || len(d.Vector) == 0 {
			continue // No vector on this document: this scorer has no opinion.
		}
		if len(d.Vector) != len(q.Vector) {
			return nil, fmt.Errorf("doc %q has %d dims, query has %d: %w",
				d.Key, len(d.Vector), len(q.Vector), ErrDimMismatch)
		}
		dNorm, err := norm(ctx, d.Vector)
		if err != nil {
			return nil, err
		}
		if dNorm == 0 {
			continue // Zero document vector: no direction, and no 0/0.
		}
		sum, err := dot(ctx, q.Vector, d.Vector)
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
