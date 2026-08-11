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
	qNorm := norm(q.Vector)
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
		dNorm := norm(d.Vector)
		if dNorm == 0 {
			continue // Zero document vector: no direction, and no 0/0.
		}
		// Cosine is in [-1,1]; negatives are kept and sort to the bottom rather
		// than being clamped, because fusion consumes rank, not magnitude.
		cands = append(cands, engine.Candidate{
			Doc:   id,
			Score: dot(q.Vector, d.Vector) / (qNorm * dNorm),
		})
	}
	return engine.TopK(cands, k), nil
}

// dot accumulates in float64 even though the inputs are float32, so long
// vectors do not lose precision to repeated float32 rounding.
func dot(a, b []float32) float64 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

func norm(v []float32) float64 { return math.Sqrt(dot(v, v)) }
