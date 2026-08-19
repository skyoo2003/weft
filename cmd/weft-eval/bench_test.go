// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/skyoo2003/weft/internal/loadgen"
)

// benchSummary prints the one line milestone 5 quoted and milestone 7 will quote
// three times over. What it must never do is attach the words "saturation" and
// "HEADLINE" to a load point no rule selected — that is the difference between a
// measured claim and a hand-picked one wearing its label, and docs/PERF.md §3 exists
// because a performance number can otherwise be made to say whatever its author
// wants.
//
// The writer is a parameter for this test. Printing to os.Stdout is what the command
// wants and is also why nothing here was ever asserted: a rule registered before the
// numbers existed, checked by no test, is a rule right up until the day it is not.

// benchLadderReports builds one rung per measured median. Only the fields
// benchSummary reads are set; print() is not called on these.
func benchLadderReports(rates []float64, p50s []time.Duration) []benchReport {
	out := make([]benchReport, 0, len(p50s))
	for i := range p50s {
		out = append(out, benchReport{
			rate: rates[i],
			all:  loadgen.Quantiles{N: 10000, P50: p50s[i], P99: 100 * time.Millisecond, P99ok: true},
			exGC: loadgen.Quantiles{N: 10000, P99: 99 * time.Millisecond, P99ok: true},
		})
	}
	return out
}

func benchMillis(n ...int) []time.Duration {
	out := make([]time.Duration, len(n))
	for i, v := range n {
		out[i] = time.Duration(v) * time.Millisecond
	}
	return out
}

// TestBenchSummaryQuotesARuleSelectedHeadlineOnAFullLadder is the case the rule was
// written for, here so that the suppression tests below cannot pass by suppressing
// everything.
func TestBenchSummaryQuotesARuleSelectedHeadlineOnAFullLadder(t *testing.T) {
	rates := []float64{1, 2, 4, 8, 16}
	p50s := benchMillis(40, 41, 43, 50, 900)
	var w bytes.Buffer

	benchSummary(&w, benchArmText, rates, p50s, benchLadderReports(rates, p50s), 40*time.Millisecond)

	got := w.String()
	if !strings.Contains(got, "saturation:") {
		t.Errorf("a complete ladder printed no saturation line:\n%s", got)
	}
	if !strings.Contains(got, "HEADLINE") {
		t.Errorf("a complete ladder printed no headline:\n%s", got)
	}
}

// TestBenchSummaryPublishesNoHeadlineForALadderCutShort is the defect.
//
// A run interrupted during rung three of five slices its rates down to the three it
// measured, so by the time the rule sees them they are indistinguishable from a
// complete three-rung ladder. It printed "saturation: not reached on this ladder"
// about a ladder whose top two rungs — the ones that would have saturated — were
// never run, and quoted a headline off a rung whose sample was truncated when the
// interrupt landed.
//
// Milestone 7's campaign is fourteen hours long. This is a Ctrl-C away.
func TestBenchSummaryPublishesNoHeadlineForALadderCutShort(t *testing.T) {
	rates := []float64{1, 2, 4, 8, 16}
	p50s := benchMillis(40, 41, 43) // interrupted during rung 3 of 5
	var w bytes.Buffer

	benchSummary(&w, benchArmText, rates, p50s, benchLadderReports(rates, p50s), 40*time.Millisecond)

	got := w.String()
	if strings.Contains(got, "HEADLINE") {
		t.Errorf("an interrupted ladder published a headline; the rule had no ladder to "+
			"apply and the rung it quoted was truncated:\n%s", got)
	}
	if strings.Contains(got, "saturation:") {
		t.Errorf("an interrupted ladder made a claim about saturation; the two rungs that "+
			"would have saturated were never run:\n%s", got)
	}
	if !strings.Contains(got, "3") || !strings.Contains(got, "5") {
		t.Errorf("the summary does not say how much of the ladder was measured:\n%s", got)
	}
}

// TestBenchSummaryPublishesNoHeadlineForAnExplicitRate keeps the guard that already
// existed, now asked of the same predicate rather than of a length check spelled here.
func TestBenchSummaryPublishesNoHeadlineForAnExplicitRate(t *testing.T) {
	rates := []float64{3.41}
	p50s := benchMillis(40)
	var w bytes.Buffer

	benchSummary(&w, benchArmText, rates, p50s, benchLadderReports(rates, p50s), 15*time.Millisecond)

	got := w.String()
	if strings.Contains(got, "HEADLINE") || strings.Contains(got, "saturation:") {
		t.Errorf("an operator-chosen rate was published under a rule's label:\n%s", got)
	}
	// The rung's own figures still print. Suppressing the claim is not suppressing the
	// measurement — milestone 7 reads its second and third repetitions off exactly this.
	if !strings.Contains(got, "3.41") {
		t.Errorf("the rung measured at an explicit rate printed no figures at all:\n%s", got)
	}
}

// TestBenchSummaryPrintsNothingWhenNothingWasMeasured covers the interrupt that lands
// before the first rung finishes.
func TestBenchSummaryPrintsNothingWhenNothingWasMeasured(t *testing.T) {
	var w bytes.Buffer
	benchSummary(&w, benchArmText, []float64{1, 2, 4, 8, 16}, nil, nil, 40*time.Millisecond)
	if w.Len() != 0 {
		t.Errorf("a run with no completed rung printed %q", w.String())
	}
}
