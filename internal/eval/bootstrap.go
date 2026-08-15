// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
)

// Sentinel errors from BootstrapCI.
var (
	// ErrUnpaired reports two runs that do not cover the same query set. The
	// pairing is the whole point: per-query nDCG varies far more across queries
	// than between arms, so a two-sample interval over 50 queries is wide enough
	// to hide any effect this milestone could plausibly find. Silently
	// intersecting the two sets instead would change what the interval is an
	// interval of, without changing how it gets reported.
	ErrUnpaired = errors.New("eval: runs do not cover the same queries")

	ErrNoIters = errors.New("eval: bootstrap iterations must be positive")
)

// Interval is a paired bootstrap estimate of one arm's improvement over another.
type Interval struct {
	// Delta is the observed mean of arm minus base — the headline number, not an
	// estimate. The interval below says how much of it survives resampling.
	Delta float64

	// Lo and Hi are the 2.5th and 97.5th percentiles of the resampled mean
	// difference. An interval containing 0 means the sign of Delta is not
	// established by this query set, and the milestone 4 judgment then reads
	// "undetermined" rather than "improved" — see the plan's Task 5.
	Lo, Hi float64

	Iters   int
	Queries int
}

// ContainsZero reports whether the interval fails to establish a sign.
func (iv Interval) ContainsZero() bool { return iv.Lo <= 0 && iv.Hi >= 0 }

// BootstrapCI returns a 95% paired bootstrap interval for arm minus base, where
// both maps hold per-query nDCG keyed by query id (Run.PerQuery).
//
// Why this exists at all: 50 queries is a small sample, and TREC-COVID's
// per-query nDCG spans most of [0,1]. A delta of a point or two in the mean is
// entirely reachable by chance at that sample size, so reporting the mean alone
// would let the graph signal's falsification condition be decided by noise. This
// is the one part of the harness the plan marks as unavailable for
// simplification.
//
// Method: resample query ids with replacement, n draws for n queries, and take
// the mean of the paired differences each time. The percentile interval is the
// 2.5th and 97.5th order statistics of those means — nearest-rank, no
// interpolation and no bias correction. Interpolating between order statistics
// changes the fourth decimal of an interval whose width is dominated by having
// 50 queries; BCa would be defensible but is a different amount of code to get
// wrong, and the judgment rule only reads whether the interval crosses zero.
//
// seed makes the result reproducible, which the milestone needs: `make eval` has
// to reprint the published numbers, and a bootstrap seeded from the clock prints
// a slightly different interval every run.
func BootstrapCI(base, arm map[string]float64, iters int, seed uint64) (Interval, error) {
	if iters <= 0 {
		return Interval{}, fmt.Errorf("iters is %d: %w", iters, ErrNoIters)
	}
	if len(base) == 0 || len(base) != len(arm) {
		return Interval{}, fmt.Errorf("base has %d queries, arm has %d: %w",
			len(base), len(arm), ErrUnpaired)
	}

	// Sorted, not map order. Go randomises map iteration, so building the delta
	// slice by ranging over either map would make the resampling depend on run
	// order and the seed would not reproduce anything.
	ids := make([]string, 0, len(base))
	for id := range base {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	deltas := make([]float64, len(ids))
	for i, id := range ids {
		a, ok := arm[id]
		if !ok {
			return Interval{}, fmt.Errorf("query %q is in base but not arm: %w", id, ErrUnpaired)
		}
		deltas[i] = a - base[id]
	}

	var observed float64
	for _, d := range deltas {
		observed += d
	}
	observed /= float64(len(deltas))

	// A fixed second stream constant so callers pass one seed. Any odd value
	// works; this is the golden-ratio constant.
	r := rand.New(rand.NewPCG(seed, 0x9E3779B97F4A7C15))

	means := make([]float64, iters)
	for i := range means {
		var sum float64
		for range deltas {
			sum += deltas[r.IntN(len(deltas))]
		}
		means[i] = sum / float64(len(deltas))
	}
	slices.Sort(means)

	return Interval{
		Delta:   observed,
		Lo:      means[percentileIndex(iters, 0.025)],
		Hi:      means[percentileIndex(iters, 0.975)],
		Iters:   iters,
		Queries: len(deltas),
	}, nil
}

// percentileIndex is the nearest-rank index into a sorted slice of n values.
//
// Nearest rank is the ⌈p·n⌉-th value counting from 1, so the zero-based index is
// ⌈p·n⌉ − 1. Truncating p·n instead is off by one whenever p·n lands on a whole
// number, which at the 10,000 iterations this harness actually runs is both
// percentiles: it took the 251st value for the 2.5th and the 9,751st for the 97.5th,
// shifting the whole interval up by one order statistic. The shift is small — a few
// ten-thousandths on these runs — but BootstrapCI's doc comment names the method,
// and an interval that does not compute the method it claims is exactly the kind of
// quiet mismatch this milestone exists to not ship.
//
// Clamped at both ends so a small iters cannot index out of range: at iters = 1
// both percentiles resolve to the single value, which is the honest answer for a
// bootstrap that was not really run.
func percentileIndex(n int, p float64) int {
	i := int(math.Ceil(p*float64(n))) - 1
	if i >= n {
		i = n - 1
	}
	if i < 0 {
		i = 0
	}
	return i
}
