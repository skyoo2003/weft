// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
)

// Sentinel errors from Search. Both report a collaborator the caller left nil,
// which is a configuration mistake and not a query that found nothing.
var (
	ErrNoFuser = errors.New("engine: nil Fuser")

	// ErrNilScorer reports a nil entry in Search's scorer list. Calling
	// Candidates on it would panic, and skipping it would quietly rank without a
	// signal the caller believes is switched on — a scorer list assembled at
	// runtime is exactly where an optional scorer goes missing.
	//
	// It covers the typed nil too. `var s *text.Scorer` assigned into a Scorer
	// slot is not == nil — the interface carries a type descriptor — and that is
	// the form runtime configuration actually produces: an optional scorer
	// declared up front and left unassigned. An earlier round documented that
	// hole rather than closing it, on the grounds that reflect was a heavy
	// import for a leaf package to carry. It traded away the promise index.go
	// makes above its own sentinels — library code here never panics — and a
	// guard that rejects the nil a caller did not write while panicking on the
	// one they did is worse than either alternative. One reflect call per
	// scorer per query, against three full corpus scans.
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
// k does two jobs: it is what each scorer is asked for and it is the size of the
// fused result. Fusing deeper than you display is usually right, and it matters
// most for the scorer you just added. A signal orthogonal to the built-in ones —
// popularity, price, licence — surfaces documents the other streams rank below
// their own cut, so at a shared k those documents appear in one stream only, and
// RRF is built so a single vote does not win. Pass a k above your display size
// and slice the result. docs/ADOPTION.md section 6.2 is a trial subject meeting
// this and having to reshape its program to find it.
//
// ctx reaches every scorer's Candidates unmodified, so a context value reaches a
// scorer too. Query documents the type-checked alternative and why to prefer it.
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
	// Checked before any scorer runs, for the same reason the fuser is: a caller
	// that misconfigured its scorer list should not pay for three full corpus
	// scans before hearing about it. And before the k check below, for the same
	// reason again: a nil scorer is a configuration mistake whatever k happens
	// to be, so a caller computing k dynamically must not have it hidden until
	// the first time k goes positive.
	for i, s := range scorers {
		// Pointer is the only kind worth reflecting on. A nil map, slice or func
		// receiver does not fault on a method call the way a dereferenced nil
		// pointer does, and no scorer in the tree is any of those.
		if v := reflect.ValueOf(s); s == nil || (v.Kind() == reflect.Pointer && v.IsNil()) {
			return nil, fmt.Errorf("scorer %d: %w", i, ErrNilScorer)
		}
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

	// Every scorer checks this before its own TopK, so that a cancellation
	// arriving after its last poll does not buy an O(n log n) sort nobody reads.
	// Fusion does the same shape of work — a map aggregation across every stream
	// and another sort — and Fuser takes no context, so it cannot check for
	// itself. This is the only place it can happen.
	//
	// Before the fuser, and deliberately not after it as well. Every context
	// check in this tree buys something: it declines work that is about to be
	// wasted. A check on the way out declines nothing — the fusion is paid for
	// and the candidates are correct — so it would trade a complete answer for
	// an error, on a caller who is free to drop the result themselves. Search
	// therefore promises that a cancelled query does not do fusion, not that a
	// cancelled query never returns one.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ponytail: each scorer is asked for exactly k. Over-fetching (asking for
	// k*m and fusing deeper streams) is known to improve RRF quality; deferred
	// to milestone 4, where there is a quality metric to justify it against.
	return fuse(streams, k), nil
}
