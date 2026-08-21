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

// The question milestone 7 closed on is what a repetition has to hold constant: the
// same rate gave 37.9 ms as a ladder's fourth rung and 1.539 s as a rung on its own
// (FINDINGS milestone 7 §1). Deciding that needs the same arrival rate behind two
// different prefixes, and `-rate 0` cannot express it — the sweep derives its five
// rates from whatever that run's sequential throughput happens to be, so the "with
// prefix" arm lands on a different rate than the "without" arm and prefix is
// confounded with rate.
//
// `-rates` names them. What it must not do is let a hand-picked ladder wear a rule's
// label: an operator who chose the rungs has chosen the load point at one remove, and
// PERF.md §3 rule 1 exists against exactly that.

// TestBenchFlagsParsesAnExplicitLadder is the flag doing its job, whitespace and all —
// the rates get copied out of a report and a report has spaces in it.
func TestBenchFlagsParsesAnExplicitLadder(t *testing.T) {
	o, err := benchFlags([]string{"-data", t.TempDir(), "-rates", "3.21, 6.42,12.84 ,25.67"})
	if err != nil {
		t.Fatalf("benchFlags: %v", err)
	}
	want := []float64{3.21, 6.42, 12.84, 25.67}
	if len(o.rates) != len(want) {
		t.Fatalf("parsed %v, want %v", o.rates, want)
	}
	for i := range want {
		if o.rates[i] != want[i] {
			t.Errorf("rate %d = %v, want %v", i, o.rates[i], want[i])
		}
	}
}

// TestBenchFlagsRefusesAnExplicitLadderBesideAnExplicitRate: the two flags answer the
// same question and one of them would win silently.
func TestBenchFlagsRefusesAnExplicitLadderBesideAnExplicitRate(t *testing.T) {
	_, err := benchFlags([]string{"-data", t.TempDir(), "-rate", "25.67", "-rates", "3.21,25.67"})
	if err == nil {
		t.Fatal("both -rate and -rates were accepted; one of them was about to be ignored")
	}
	if !strings.Contains(err.Error(), "-rate") || !strings.Contains(err.Error(), "-rates") {
		t.Errorf("error = %v, want it to name both flags", err)
	}
}

// TestBenchFlagsRefusesARateInsideALadderItWouldRejectAlone holds the list to the same
// bounds a single -rate gets.
//
// The reason is in benchFlags' own comment about -rate: a non-positive or non-finite
// rate makes an interval that dispatches the whole rung at once and prints the result
// as a distribution. A list is not a place where that stops being true, and a
// four-rung sweep whose third entry was a typo is ninety minutes spent before
// anything says so.
func TestBenchFlagsRefusesARateInsideALadderItWouldRejectAlone(t *testing.T) {
	for _, bad := range []string{"abc", "-5", "0", "3.21,0", "3.21,,6.42", "NaN", "Inf", "3.21,2e9", "  "} {
		t.Run(bad, func(t *testing.T) {
			if _, err := benchFlags([]string{"-data", t.TempDir(), "-rates", bad}); err == nil {
				t.Errorf("-rates=%q was accepted", bad)
			}
		})
	}
}

// TestBenchRatesPrefersTheExplicitLadderAndDisownsTheRule pins both halves: which
// rates run, and whether the run may say a rule chose among them.
func TestBenchRatesPrefersTheExplicitLadderAndDisownsTheRule(t *testing.T) {
	unloaded := 40 * time.Millisecond // 25 q/s sequential

	got, ruleLadder := benchRates(0, []float64{3.21, 25.67}, unloaded)
	if len(got) != 2 || got[0] != 3.21 || got[1] != 25.67 {
		t.Errorf("explicit ladder = %v, want [3.21 25.67]", got)
	}
	if ruleLadder {
		t.Error("an operator-chosen ladder claimed to be the rule's own")
	}

	got, ruleLadder = benchRates(25.67, nil, unloaded)
	if len(got) != 1 || got[0] != 25.67 {
		t.Errorf("single rate = %v, want [25.67]", got)
	}
	if ruleLadder {
		t.Error("an operator-chosen rate claimed to be the rule's own")
	}

	got, ruleLadder = benchRates(0, nil, unloaded)
	if len(got) != len(loadgen.Ladder) {
		t.Errorf("sweep = %v, want %d rungs", got, len(loadgen.Ladder))
	}
	if !ruleLadder {
		t.Error("the sweep disowned the rule; then nothing can ever publish a headline")
	}
}

// TestBenchSummaryPublishesNoHeadlineForAnOperatorChosenLadder is the guard the flag
// needs. A four-rung ladder someone typed satisfies every shape check — two or more
// rungs, all of them measured — and would otherwise be quoted as though rule 1 had
// selected it from a sweep.
func TestBenchSummaryPublishesNoHeadlineForAnOperatorChosenLadder(t *testing.T) {
	rates := []float64{3.21, 6.42, 12.84, 25.67}
	p50s := benchMillis(40, 41, 43, 900)
	var w bytes.Buffer

	benchSummary(&w, benchArmText, false, rates, p50s, benchLadderReports(rates, p50s), 40*time.Millisecond)

	got := w.String()
	if strings.Contains(got, "HEADLINE") || strings.Contains(got, "saturation:") {
		t.Errorf("a hand-picked ladder was published under the rule's label:\n%s", got)
	}
	// Suppressing the claim is not suppressing the measurement — the experiment this
	// flag exists for reads the rungs.
	if !strings.Contains(got, "25.67") {
		t.Errorf("the rungs printed no figures:\n%s", got)
	}
}

// TestBenchSummaryQuotesARuleSelectedHeadlineOnAFullLadder is the case the rule was
// written for, here so that the suppression tests below cannot pass by suppressing
// everything.
func TestBenchSummaryQuotesARuleSelectedHeadlineOnAFullLadder(t *testing.T) {
	rates := []float64{1, 2, 4, 8, 16}
	p50s := benchMillis(40, 41, 43, 50, 900)
	var w bytes.Buffer

	benchSummary(&w, benchArmText, true, rates, p50s, benchLadderReports(rates, p50s), 40*time.Millisecond)

	got := w.String()
	if !strings.Contains(got, "saturation:") {
		t.Errorf("a complete ladder printed no saturation line:\n%s", got)
	}
	if !strings.Contains(got, "HEADLINE") {
		t.Errorf("a complete ladder printed no headline:\n%s", got)
	}
}

// TestBenchSummaryOnALadderThatNeverSaturatesQuotesItsTopRung covers the other half
// of the rule, and it is not a hypothetical branch: a ladder whose every rung stays
// under twice the unloaded median has no midpoint to quote, so the honest answer is
// the most load actually applied, said in words rather than left to be inferred.
func TestBenchSummaryOnALadderThatNeverSaturatesQuotesItsTopRung(t *testing.T) {
	rates := []float64{1, 2, 4, 8, 16}
	p50s := benchMillis(40, 41, 43, 50, 60) // none past 2x the 40ms unloaded median
	var w bytes.Buffer

	benchSummary(&w, benchArmText, true, rates, p50s, benchLadderReports(rates, p50s), 40*time.Millisecond)

	got := w.String()
	if !strings.Contains(got, "not reached") {
		t.Errorf("a ladder that never saturated did not say so:\n%s", got)
	}
	if !strings.Contains(got, "HEADLINE") || !strings.Contains(got, "16.00") {
		t.Errorf("the headline is not the top rung the run actually applied:\n%s", got)
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

	benchSummary(&w, benchArmText, true, rates, p50s, benchLadderReports(rates, p50s), 40*time.Millisecond)

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

// TestBenchSummaryPublishesNoHeadlineWhenARungWasSuspended is milestone 7's first
// repetition, turned into an assertion.
//
// That run's lid closed twenty-three minutes into a ninety-two minute ladder. Every
// rung then matched its own schedule to within a second, shed was zero below the
// knee, and the headline landed 0.8% from milestone 5's published figure — a report
// with no symptom anywhere in it. A complete ladder measured across thirteen hours of
// sleep is not a slightly worse measurement, it is not one, and the summary must say
// so rather than quote a headline off it.
func TestBenchSummaryPublishesNoHeadlineWhenARungWasSuspended(t *testing.T) {
	rates := []float64{1, 2, 4, 8, 16}
	p50s := benchMillis(40, 41, 43, 50, 900)
	reports := benchLadderReports(rates, p50s)
	reports[0].unaccounted = 12*time.Hour + 20*time.Minute

	var w bytes.Buffer
	benchSummary(&w, benchArmText, true, rates, p50s, reports, 40*time.Millisecond)

	got := w.String()
	if strings.Contains(got, "HEADLINE") || strings.Contains(got, "saturation:") {
		t.Errorf("a ladder that ran across a suspension published a rule's claim:\n%s", got)
	}
	if !strings.Contains(got, "12h20m0s") {
		t.Errorf("the summary does not say how much time the process did not run:\n%s", got)
	}
}

// TestBenchSummaryIsUnmovedByAGapInsideTheTolerance keeps the guard from firing on
// the clock adjustment every laptop makes.
func TestBenchSummaryIsUnmovedByAGapInsideTheTolerance(t *testing.T) {
	rates := []float64{1, 2, 4, 8, 16}
	p50s := benchMillis(40, 41, 43, 50, 900)
	reports := benchLadderReports(rates, p50s)
	reports[0].unaccounted = 2 * time.Second

	var w bytes.Buffer
	benchSummary(&w, benchArmText, true, rates, p50s, reports, 40*time.Millisecond)

	if got := w.String(); !strings.Contains(got, "HEADLINE") {
		t.Errorf("a 2s clock adjustment suppressed the headline; every rung on a laptop "+
			"would be thrown away:\n%s", got)
	}
}

// TestBenchSummaryPublishesNoHeadlineForAnExplicitRate keeps the guard that already
// existed, now asked of the same predicate rather than of a length check spelled here.
func TestBenchSummaryPublishesNoHeadlineForAnExplicitRate(t *testing.T) {
	rates := []float64{3.41}
	p50s := benchMillis(40)
	var w bytes.Buffer

	benchSummary(&w, benchArmText, true, rates, p50s, benchLadderReports(rates, p50s), 15*time.Millisecond)

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
	benchSummary(&w, benchArmText, true, []float64{1, 2, 4, 8, 16}, nil, nil, 40*time.Millisecond)
	if w.Len() != 0 {
		t.Errorf("a run with no completed rung printed %q", w.String())
	}
}
