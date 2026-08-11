// Package recency ranks documents by how recent Document.Time is.
//
// This package is not a feature. It is the milestone 1 measurement: it was
// written after text, vector and graph were already fusing, to find out what a
// fourth scorer actually costs. If adding it had required editing fusion or
// engine, the architecture hypothesis would have been wrong.
// pkg/engine/architecture_test.go is the scoreboard; docs/FINDINGS.md is the
// verdict.
package recency

import (
	"context"
	"math"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
)

// HalfLife is how long a document takes to lose half its score.
const HalfLife = 30 * 24 * time.Hour

// Scorer ranks documents by exponential decay of age.
type Scorer struct {
	ix  *engine.Index
	now func() time.Time
}

// New returns a recency scorer reading ix, against the wall clock.
func New(ix *engine.Index) *Scorer { return &Scorer{ix: ix, now: time.Now} }

// NewAt is New with the clock pinned, so rankings are reproducible in tests.
func NewAt(ix *engine.Index, now time.Time) *Scorer {
	return &Scorer{ix: ix, now: func() time.Time { return now }}
}

// Name implements engine.Scorer.
func (s *Scorer) Name() string { return "recency" }

// Candidates implements engine.Scorer, scoring 2^(-age/HalfLife) so a document
// one half-life old scores 0.5, two half-lives 0.25, and nothing ever reaches
// zero or goes negative.
func (s *Scorer) Candidates(ctx context.Context, q engine.Query, k int) ([]engine.Candidate, error) {
	if k <= 0 {
		return nil, nil
	}
	now := s.now()

	n := s.ix.Len()
	cands := make([]engine.Candidate, 0, n)
	for i := range n {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := engine.DocID(i)
		d, ok := s.ix.Doc(id)
		if !ok || d.Time.IsZero() {
			continue // No timestamp: no opinion, same as a missing vector.
		}
		age := now.Sub(d.Time)
		if age < 0 {
			// A future timestamp caps at brand new rather than scoring above 1,
			// so a wrong clock cannot buy a document an unbeatable rank.
			age = 0
		}
		cands = append(cands, engine.Candidate{
			Doc:   id,
			Score: math.Exp2(-age.Hours() / HalfLife.Hours()),
		})
	}
	return engine.TopK(cands, k), nil
}
