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

// Scorer ranks documents by harmonic decay of age, 1/(1 + age/HalfLife). Not
// exponential — see Candidates for why 2^(-age/HalfLife) was rejected.
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
// The literal 2^(-age/HalfLife) is wrong here: it underflows to exactly zero at
// about 88 years, and a corpus of ties is ordered by DocID, so an ancient
// document would outrank a recent one for having been indexed first. Every
// arithmetic choice below defends the same property — the score must stay
// strictly decreasing in age for every age a timestamp can express. The swap is
// rank-neutral where both forms are representable; docs/FINDINGS.md section 5
// has the rest.
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
		// Age in seconds, computed in float64 throughout. Two int64 forms are
		// wrong: now.Sub saturates at ±292 years and ties everything past it,
		// and subtracting Unix seconds before widening wraps for a sufficiently
		// old timestamp, whereupon the guard below reads the negative as a future
		// date and scores the oldest document brand new. Widening first cannot
		// overflow, and stays exact to the second for about 285 million years —
		// to the second, not the nanosecond. float64 carries 53 bits, so the ulp
		// of an age in seconds passes 1 ns at ~104 days old and 1 µs at ~285
		// years; closer than that, scores tie and TopK's DocID tiebreak puts the
		// older document first. Inherent to seconds-as-float64, not to this
		// expression, and orders of magnitude past where the exponential broke.
		age := (float64(now.Unix()) - float64(d.Time.Unix())) +
			float64(now.Nanosecond()-d.Time.Nanosecond())/1e9
		if age < 0 {
			// A future timestamp caps at brand new rather than scoring above 1,
			// so a wrong clock cannot buy a document an unbeatable rank.
			age = 0
		}
		cands = append(cands, engine.Candidate{
			Doc:   id,
			Score: 1 / (1 + age/HalfLife.Seconds()),
		})
	}

	// Without this, a cancellation arriving after the last document still pays
	// for TopK's O(n log n) sort and then reports success past the deadline.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return engine.TopK(cands, k), nil
}
