// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"testing"
	"time"
)

// Milestone 7's first repetition ran on a machine whose lid closed twenty-three
// minutes in. It slept and dark-woke for thirteen hours, and **nothing in the report
// said so**: every rung's elapsed matched its own schedule to within a second, shed
// was zero on the rungs below the knee, and the headline p99 came out 0.8% from the
// figure milestone 5 published.
//
// The reason is that time.Since reads the monotonic clock, and on Darwin the
// monotonic clock does not advance while the system is asleep. The instrument
// measures the time the process experienced, which is the right quantity for a
// latency — and is exactly why it cannot see time the process did not experience.
//
// The wall clock can. It ran 13h52m while the monotonic clock ran 92m, and the gap
// between the two is the whole of what was missed. The run was caught only because a
// `date` happened to be piped either side of it; milestone 5's ladder had no such
// accident, and whether it slept is now unanswerable from its published record.
//
// So: read both, subtract, and publish the difference.

func TestUnaccountedIsWallMinusMonotonic(t *testing.T) {
	tests := []struct {
		name       string
		mono, wall time.Duration
		want       time.Duration
	}{
		{
			name: "a span nothing interrupted accounts for all of its wall time",
			mono: 100 * time.Millisecond, wall: 100 * time.Millisecond,
			want: 0,
		},
		{
			name: "a small gap is reported rather than swallowed; the tolerance decides what it means",
			mono: 100 * time.Millisecond, wall: 600 * time.Millisecond,
			want: 500 * time.Millisecond,
		},
		{
			name: "the run this was written for",
			mono: 92 * time.Minute, wall: 13*time.Hour + 52*time.Minute,
			want: 12*time.Hour + 20*time.Minute,
		},
		{
			name: "a wall clock that stepped backwards is not a suspension",
			mono: 100 * time.Millisecond, wall: 50 * time.Millisecond,
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := unaccounted(tt.mono, tt.wall); got != tt.want {
				t.Errorf("unaccounted(%v, %v) = %v, want %v", tt.mono, tt.wall, got, tt.want)
			}
		})
	}
}

// TestSuspendToleranceSeparatesClockJitterFromASleepingMachine pins the threshold
// against the two things it has to tell apart.
//
// Below it: an NTP step, which moves the wall clock by milliseconds to seconds on a
// laptop and says nothing about whether the process ran. Above it: a machine that
// stopped. There is no suspension worth reporting that is smaller than the gap
// between those two, and no clock adjustment worth reporting at all.
func TestSuspendToleranceSeparatesClockJitterFromASleepingMachine(t *testing.T) {
	if jitter := 2 * time.Second; jitter > SuspendTolerance {
		t.Errorf("a %v clock adjustment reads as a suspension; the tolerance is too tight "+
			"and every rung on a laptop will be flagged", jitter)
	}
	if slept := unaccounted(92*time.Minute, 13*time.Hour+52*time.Minute); slept <= SuspendTolerance {
		t.Errorf("%v of a sleeping machine does not read as a suspension; the tolerance is "+
			"too loose and the run this was written for would pass again", slept)
	}
}

// TestElapsedReadsBothClocksOverARealSpan is the part no table can check: that
// Elapsed takes its two readings off two different clocks rather than the same one
// twice.
//
// A span with nothing suspending it must account for itself. The assertion is
// deliberately loose — the point is that the two readings agree to well inside the
// tolerance, not that they agree to the nanosecond.
func TestElapsedReadsBothClocksOverARealSpan(t *testing.T) {
	start := time.Now()
	time.Sleep(20 * time.Millisecond)
	mono, unacc := Elapsed(start)

	if mono < 20*time.Millisecond {
		t.Errorf("elapsed = %v over a 20ms sleep; that is not the span that ran", mono)
	}
	if unacc > SuspendTolerance {
		t.Errorf("a 20ms span reported %v unaccounted; the two readings are not tracking "+
			"the same interval", unacc)
	}
}
