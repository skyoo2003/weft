package engine

import (
	"context"
	"errors"
	"fmt"
)

// ErrNoFuser is returned by Search when called without a fusion operator.
var ErrNoFuser = errors.New("engine: nil Fuser")

// Fuser collapses N candidate streams into one ranked list of at most k.
//
// Search takes this as a parameter instead of importing a fusion package, for
// two reasons. The mechanical one: fusion has to import engine for Candidate,
// so engine importing fusion back would be a compile-time cycle. The useful
// one: it makes engine ignorant of the fusion strategy the same way fusion is
// ignorant of the scorers. Swapping RRF for something else touches no code in
// here.
type Fuser func(streams [][]Candidate, k int) []Candidate

// Search runs every scorer over q and fuses the results.
//
// Note what is absent: no scorer count, no scorer type, no branch on
// Name(). Adding a fifth scorer means passing a fifth argument at the call
// site, and nothing in this function changes. If a future scorer forces an edit
// here, that edit is the milestone 1 hypothesis failing.
func Search(ctx context.Context, q Query, k int, fuse Fuser, scorers ...Scorer) ([]Candidate, error) {
	if fuse == nil {
		return nil, ErrNoFuser
	}
	if k <= 0 {
		return nil, nil
	}

	// ponytail: scorers run sequentially. Fan out with goroutines when one slow
	// scorer measurably dominates latency — the interface already allows it,
	// since Candidates takes a ctx and shares no mutable state.
	streams := make([][]Candidate, 0, len(scorers))
	for _, s := range scorers {
		cands, err := s.Candidates(ctx, q, k)
		if err != nil {
			// Name() is diagnostics here, not control flow.
			return nil, fmt.Errorf("scorer %s: %w", s.Name(), err)
		}
		streams = append(streams, cands)
	}

	// ponytail: each scorer is asked for exactly k. Over-fetching (asking for
	// k*m and fusing deeper streams) is known to improve RRF quality; deferred
	// to milestone 4, where there is a quality metric to justify it against.
	return fuse(streams, k), nil
}
