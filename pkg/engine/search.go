package engine

import (
	"context"
	"errors"
	"fmt"
)

// Sentinel errors from Search. Both report a collaborator the caller left nil,
// which is a configuration mistake and not a query that found nothing.
var (
	ErrNoFuser = errors.New("engine: nil Fuser")

	// ErrNilScorer reports a nil entry in Search's scorer list. Calling
	// Candidates on it would panic, and skipping it would quietly rank without a
	// signal the caller believes is switched on — a scorer list assembled at
	// runtime is exactly where an optional scorer goes missing.
	ErrNilScorer = errors.New("engine: nil Scorer")
)

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
//
// Precondition: every scorer must read the same Index. DocID is dense and
// index-relative, so scorers built against different indexes return IDs from
// namespaces that both start at 0; fusion would read the collision as two
// scorers agreeing on one document, and the winning IDs would resolve against
// neither corpus. This is unchecked on purpose. Checking it means asking a
// scorer which index it reads, which is a method on the Scorer interface — the
// one change that breaks every existing implementation, and one that a scorer
// computing purely from Query could not answer at all. The general fix is for
// DocID to carry its namespace, which milestone 2 needs regardless
// (docs/FINDINGS.md sections 3.4 and 4.3).
func Search(ctx context.Context, q Query, k int, fuse Fuser, scorers ...Scorer) ([]Candidate, error) {
	if fuse == nil {
		return nil, ErrNoFuser
	}
	if k <= 0 {
		return nil, nil
	}
	// Checked before any scorer runs, for the same reason the fuser is: a caller
	// that misconfigured its scorer list should not pay for three full corpus
	// scans before hearing about it.
	for i, s := range scorers {
		if s == nil {
			return nil, fmt.Errorf("scorer %d: %w", i, ErrNilScorer)
		}
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
