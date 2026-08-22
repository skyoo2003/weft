// SPDX-License-Identifier: Apache-2.0

package loadgen

import "time"

// SuspendTolerance is how far a span's wall clock may run ahead of its monotonic
// clock before the gap is a suspension rather than a clock adjustment.
//
// Five seconds separates the two things it has to tell apart and nothing lives
// between them. Below it is an NTP step, which moves the wall clock by milliseconds
// to a second or two on a laptop and says nothing about whether the process ran.
// Above it is a machine that stopped, and the smallest stop worth reporting — a
// display sleep that took the CPU with it — is already minutes.
const SuspendTolerance = 5 * time.Second

// Elapsed returns how long a span took on the monotonic clock, and how much wall
// time that span cannot account for.
//
// The first is what every latency in this package is measured against and is the
// right quantity: it is the time the process experienced. The second exists because
// that is also its blind spot.
//
// On Darwin the monotonic clock does not advance while the system is asleep. A ladder
// measured across thirteen hours of clamshell sleep therefore reports every rung
// finishing exactly on its own schedule, with no shed, no fault spike and a plausible
// tail — a report with no symptom in it anywhere. Not hypothetical: milestone 7's
// first repetition was that run, and it was caught only because a `date` happened to
// be piped either side of the command.
//
// time.Time carries both readings, and Sub uses the monotonic one only when both
// operands have it. Round(0) strips it, so the second subtraction below is the wall
// clock's answer to the same question. Where the two disagree, the process was not
// running.
func Elapsed(start time.Time) (elapsed, unaccountedFor time.Duration) {
	now := time.Now()
	mono := now.Sub(start)
	wall := now.Round(0).Sub(start.Round(0))
	return mono, unaccounted(mono, wall)
}

// unaccounted is the arithmetic on its own, so the judgment can be tested without a
// clock that can be made to lie.
//
// Clamped at zero rather than returned signed. A wall clock behind the monotonic one
// stepped backwards — an NTP correction, or someone setting the date — and that is a
// fact about the clock rather than about whether the process ran. Reporting it as
// negative suspension would put a number in the report that has no reading.
func unaccounted(mono, wall time.Duration) time.Duration {
	if d := wall - mono; d > 0 {
		return d
	}
	return 0
}
