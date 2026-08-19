// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"testing"
	"time"
)

// The load-point rule (SaturationRate, HeadlineRate) answers "which rung does the
// one-line summary quote". Both of them answer it for any slice they are handed,
// including one that is not a ladder — and that is where a published figure comes to
// carry a claim nobody measured.
//
// Two ways a caller ends up without a ladder:
//
//	an explicit -rate     one rung, chosen by the operator. SaturationRate returns it
//	                      whenever its p50 passed twice the unloaded median, because
//	                      there is nothing for it to be *first* past, and HeadlineRate
//	                      returns it on both branches. Already guarded at both call
//	                      sites, by a length check each spells for itself.
//
//	a ladder cut short    Ctrl-C during rung three of five. Nothing notices: the
//	                      caller slices its rates down to the rungs it got, so by the
//	                      time the rule sees them they look like a complete three-rung
//	                      ladder. It then prints "saturation: not reached on this
//	                      ladder" about a ladder two rungs of which were never run,
//	                      and quotes a headline off a rung whose sample was truncated
//	                      mid-flight.
//
// Milestone 7's campaign is fourteen hours of these runs, so the second case is not
// hypothetical. RuleApplies is the one predicate both callers ask, so that the answer
// cannot drift between the two commands the comparison is made of.

func TestRuleAppliesOnlyToACompleteLadder(t *testing.T) {
	ms := func(n ...int) []time.Duration {
		out := make([]time.Duration, len(n))
		for i, v := range n {
			out[i] = time.Duration(v) * time.Millisecond
		}
		return out
	}
	tests := []struct {
		name  string
		rates []float64
		p50s  []time.Duration
		want  bool
	}{
		{
			name:  "a full five-rung sweep is what the rule was written for",
			rates: []float64{1, 2, 4, 8, 16},
			p50s:  ms(10, 11, 13, 20, 90),
			want:  true,
		},
		{
			name:  "a ladder cut short is not a ladder, however many rungs it got",
			rates: []float64{1, 2, 4, 8, 16},
			p50s:  ms(10, 11, 13),
			want:  false,
		},
		{
			name:  "one rung short is still short",
			rates: []float64{1, 2, 4, 8, 16},
			p50s:  ms(10, 11, 13, 20),
			want:  false,
		},
		{
			name:  "an explicit -rate is one operator-chosen rung, not a measured load point",
			rates: []float64{3.41},
			p50s:  ms(40),
			want:  false,
		},
		{
			name:  "two rungs are the fewest the rule can say anything about",
			rates: []float64{1, 2},
			p50s:  ms(10, 90),
			want:  true,
		},
		{
			name:  "nothing measured",
			rates: []float64{1, 2, 4, 8, 16},
			p50s:  nil,
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RuleApplies(tt.rates, tt.p50s); got != tt.want {
				t.Errorf("RuleApplies(%v, %v) = %v, want %v", tt.rates, tt.p50s, got, tt.want)
			}
		})
	}
}
