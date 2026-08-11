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
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
)

// HalfLife is how long a document takes to lose half its score. Once — the
// decay is harmonic, not exponential, so see Candidates before assuming it
// halves again.
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

// Candidates implements engine.Scorer, scoring 1/(1 + age/HalfLife) so a
// document one half-life old scores 0.5 and nothing ever reaches zero or goes
// negative. At two half-lives the score is 1/3, not 0.25.
//
// The literal reading of "half-life" is 2^(-age/HalfLife), and it is wrong here:
// that underflows float64 to exactly zero at about 88 years, so every document
// older than that ties at zero and TopK falls back to ordering them by DocID.
// A century-old document would then outrank a decade-old one for having been
// indexed first. 1/(1+x) bottoms out at 2.8e-4 over the whole representable
// time.Duration range and stays ordered.
//
// The swap costs nothing in rank terms: both forms decrease strictly with age,
// so they order identically wherever the exponential is representable, and
// fusion reads rank rather than score. Which decay shape actually ranks better
// is a milestone 4 question — see docs/FINDINGS.md section 5.
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
			Score: 1 / (1 + age.Hours()/HalfLife.Hours()),
		})
	}
	return engine.TopK(cands, k), nil
}
