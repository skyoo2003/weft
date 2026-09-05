// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// fullSort is what TopK did before it learned to select, kept here as the
// reference the fast path is checked against.
//
// A second implementation inside a test is usually a smell — a copy that drifts
// from the thing under test. Here it is the point: TopK now has two paths, one
// of which sorts everything and one of which throws most of it away, and the
// whole claim is that they cannot disagree. The reference is the simple one,
// stated once, in three lines.
func fullSort(cands []Candidate, k int) []Candidate {
	out := slices.Clone(cands)
	slices.SortFunc(out, rank)
	return out[:min(len(out), k)]
}

// TestTopKSelectsExactlyWhatSortingWouldHaveChosen is the check that licenses
// the heap.
//
// The selection path never looks at n−k candidates again once it has rejected
// them, so a comparator inversion or an off-by-one in the sift shows up as a
// *plausible* wrong answer: a ranking that is ordered, that is the right length,
// and that is missing a document. Nothing downstream can see that — fusion
// consumes ranks and cannot know what was left out — so the only place it can be
// caught is here, against a reference that considers everything.
func TestTopKSelectsExactlyWhatSortingWouldHaveChosen(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 5))
	for _, n := range []int{1, 2, 3, 7, 8, 9, 64, 1000} {
		cands := make([]Candidate, n)
		for i := range cands {
			cands[i] = Candidate{Doc: DocID(i), Score: rng.Float64()}
		}
		for _, k := range []int{1, 2, 3, 7, 8, n - 1, n, n + 1, 2 * n} {
			if k <= 0 {
				continue
			}
			want := fullSort(cands, k)
			got := TopK(slices.Clone(cands), k)
			if !slices.Equal(got, want) {
				t.Fatalf("n=%d k=%d: TopK selected %v, sorting selected %v", n, k, got, want)
			}
		}
	}
}

// TestTopKAgreesWithSortingOnTies is the case random scores never produce and
// the one the DocID tiebreak exists for. With every score equal the answer is
// decided entirely by the tiebreak, so a heap that lost it would return k
// arbitrary documents in a stable-looking order.
func TestTopKAgreesWithSortingOnTies(t *testing.T) {
	for _, tie := range []float64{0, 1, math.Inf(1), math.Inf(-1)} {
		cands := make([]Candidate, 100)
		for i := range cands {
			// Ids descending, so insertion order is the opposite of the answer.
			cands[i] = Candidate{Doc: DocID(len(cands) - i), Score: tie}
		}
		for _, k := range []int{1, 5, 50, 99} {
			want := fullSort(cands, k)
			got := TopK(slices.Clone(cands), k)
			if !slices.Equal(got, want) {
				t.Errorf("score=%v k=%d: TopK = %v, want %v", tie, k, got, want)
			}
		}
	}
}

// TestTopKKeepsNaNAtTheBottomWhenSelecting. cmp.Compare puts NaN below every
// other value, and the selection path has to inherit that rather than reinvent
// it: a NaN reaching rank 1 is a plausible result fusion would pass on.
func TestTopKKeepsNaNAtTheBottomWhenSelecting(t *testing.T) {
	cands := []Candidate{
		{Doc: 0, Score: math.NaN()},
		{Doc: 1, Score: 0.5},
		{Doc: 2, Score: math.NaN()},
		{Doc: 3, Score: 0.9},
		{Doc: 4, Score: 0.1},
	}
	got := TopK(slices.Clone(cands), 3)
	want := []Candidate{{Doc: 3, Score: 0.9}, {Doc: 1, Score: 0.5}, {Doc: 4, Score: 0.1}}
	if !slices.Equal(got, want) {
		t.Errorf("TopK = %v, want %v — no NaN may displace a real score", got, want)
	}
	// And where NaN cannot be avoided it still ties on DocID rather than on
	// arrival order.
	all := TopK(slices.Clone(cands), 5)
	if all[3].Doc != 0 || all[4].Doc != 2 {
		t.Errorf("the NaN tail is %v then %v, want documents 0 then 2", all[3].Doc, all[4].Doc)
	}
}

// TestTopKCapsTheResult guards the contract a bare cands[:k] would break: the
// spare capacity is not the caller's to write into, because it is the array the
// caller just handed in.
func TestTopKCapsTheResult(t *testing.T) {
	cands := make([]Candidate, 10)
	for i := range cands {
		cands[i] = Candidate{Doc: DocID(i), Score: float64(10 - i)}
	}
	got := TopK(cands, 3)
	if cap(got) != 3 {
		t.Errorf("cap = %d, want 3", cap(got))
	}
	_ = append(got, Candidate{Doc: 99}) //nolint:gocritic // the point is that it must copy
	if cands[3].Doc == 99 {
		t.Error("appending to the result wrote over element 3 of the input")
	}
}

// BenchmarkTopK is the measurement the selection path was bought on: k far below
// n is the shape a corpus-sized candidate stream has, and k == n is the shape
// pkg/query's scorers have, which must not regress.
func BenchmarkTopK(b *testing.B) {
	const n = 50000
	rng := rand.New(rand.NewPCG(11, 13))
	src := make([]Candidate, n)
	for i := range src {
		src[i] = Candidate{Doc: DocID(i), Score: rng.Float64()}
	}
	scratch := make([]Candidate, n)

	for _, k := range []int{10, 100, 1000, n} {
		b.Run(fmt.Sprintf("k=%d", k), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				copy(scratch, src)
				TopK(scratch, k)
			}
		})
	}
}
