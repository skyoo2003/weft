// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skyoo2003/weft/internal/eval"
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

// The two tests below are the defect milestone 8's pass line ran into.
//
// ru_maxrss is a high-water mark the kernel never lowers, which bench.go's own comment
// on benchReport.rss has said since milestone 5. What follows from it had not been
// written down anywhere a reader of the report would meet it: the figure a rung prints
// is the peak the *process* has reached by the end of that rung, so a pass line of the
// form "RSS ≤ 250 MiB at this rate" cannot be judged from it on a ladder.
//
// It is not hypothetical. The ladder that judged 27.28 q/s printed 345.2 MiB there and
// 345.2 MiB two rungs earlier at half the rate — the same mark, set at 13.64 q/s, read
// twice. The rung under test added nothing to it and its own peak is unmeasured.
//
// The writer is a parameter for the same reason benchSummary's is: printing to
// os.Stdout is what the command wants and is also why none of this was ever asserted.

// TestBenchWarmupDoesNotCallAQuietMachineSuspended is the false-positive half of
// milestone 32's third change.
//
// The suspension check now spans the cold pass and the sequential replay, and it is
// fatal — an unloaded median measured across a sleep would scale every rung on the
// ladder from a machine that was not running. Whether it *fires* correctly is
// loadgen.unaccounted's own test, which can lie to a clock this one cannot; what has to
// hold here is that an ordinary warm-up on an awake machine is not refused. A check
// comparing against the wrong bound would pass review and fail every run.
func TestBenchWarmupDoesNotCallAQuietMachineSuspended(t *testing.T) {
	qs := []eval.Query{{ID: "1"}, {ID: "2"}}
	var failed atomic.Int64

	unloaded, err := benchWarmup(context.Background(), qs, func(int) {
		time.Sleep(time.Millisecond)
	}, &failed)
	if err != nil {
		t.Fatalf("a warm-up on an awake machine was refused: %v", err)
	}
	if unloaded <= 0 {
		t.Errorf("unloaded median %v; the ladder would derive every rate from it", unloaded)
	}
}

// The preflight gate is the pass line docs/PERF.md §5.9 and D-036 registered, and it
// spent four ladders as a comment. What follows holds it to its own wording — the
// threshold is loadgen.SaturationRate's constant, it is strictly past, and it reads
// every rung it was given.

// TestBenchFlagsParsesPreflight: off unless asked for. A gate that defaulted on would
// turn every exploratory `-rate` run into a failed command.
func TestBenchFlagsParsesPreflight(t *testing.T) {
	o, err := benchFlags([]string{"-data", t.TempDir()})
	if err != nil {
		t.Fatalf("benchFlags: %v", err)
	}
	if o.preflight {
		t.Error("-preflight defaulted on; an ordinary ladder would exit non-zero for saturating")
	}
	o, err = benchFlags([]string{"-data", t.TempDir(), "-preflight"})
	if err != nil {
		t.Fatalf("benchFlags -preflight: %v", err)
	}
	if !o.preflight {
		t.Error("-preflight was accepted and ignored")
	}
}

// TestPreflightRefusesAMachineThatSaturates is the gate doing the job the comment in
// the Makefile promised it would do after a fourth void ladder.
//
// The boundary case is the one worth having: a rung sitting at exactly twice the
// unloaded median passes, because SaturationRate is strictly past and a gate that drew
// the line differently would be a second spelling of saturation.
func TestPreflightRefusesAMachineThatSaturates(t *testing.T) {
	const unloaded = 35 * time.Millisecond
	for _, tc := range []struct {
		name string
		p50  time.Duration
		shed int
		want bool // want an error
	}{
		{"comfortably under", 37 * time.Millisecond, 0, false},
		{"exactly twice is not saturation", 70 * time.Millisecond, 0, false},
		{"a hair past twice", 71 * time.Millisecond, 0, true},
		{"the void machine, 78x", 2730 * time.Millisecond, 0, true},
		{"shed one, p50 fine", 37 * time.Millisecond, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reports := benchLadderReports([]float64{27.28}, []time.Duration{tc.p50})
			reports[0].shed = tc.shed
			err := preflightGate(reports, unloaded)
			if (err != nil) != tc.want {
				t.Fatalf("preflightGate(p50 %v, shed %d) = %v, want error: %v",
					tc.p50, tc.shed, err, tc.want)
			}
			if err == nil {
				return
			}
			// The message is the whole product on the failing path: an operator reading
			// it decides whether to spend three hours.
			for _, want := range []string{"27.28", "FAILED"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

// TestPreflightReadsEveryRungItWasGiven: -preflight is a gate on whatever rates it was
// handed, and a probe that checked only the first would pass a ladder whose second rung
// collapsed.
func TestPreflightReadsEveryRungItWasGiven(t *testing.T) {
	const unloaded = 35 * time.Millisecond
	reports := benchLadderReports(
		[]float64{3.41, 27.28},
		benchMillis(37, 2730),
	)
	err := preflightGate(reports, unloaded)
	if err == nil {
		t.Fatal("a ladder whose second rung sat at 78x the unloaded median passed the gate")
	}
	if !strings.Contains(err.Error(), "27.28") {
		t.Errorf("error %q blames a rung other than the one that failed", err)
	}
}

// TestPreflightRefusesARunThatMeasuredNothing. An interrupted run reaches the gate with
// no rungs, and a loop over nothing reports success — which is the exact failure the
// gate exists to prevent, arrived at from the other side.
func TestPreflightRefusesARunThatMeasuredNothing(t *testing.T) {
	if err := preflightGate(nil, 35*time.Millisecond); err == nil {
		t.Error("preflight passed a run that measured no rungs")
	}
}

// TestPreflightVerdictIsSilentWhenNotAskedFor: an ordinary ladder must not exit
// non-zero for saturating, which is what the top rung of a five-rung sweep is *for*.
// The gate is a mode, not a rule the command acquired.
func TestPreflightVerdictIsSilentWhenNotAskedFor(t *testing.T) {
	// A ladder that would fail the gate twice over: past twice the median, and shed.
	reports := benchLadderReports([]float64{27.28}, benchMillis(2730))
	reports[0].shed = 1438
	if err := preflightVerdict(false, reports, 35*time.Millisecond); err != nil {
		t.Errorf("a run without -preflight was judged by the gate anyway: %v", err)
	}
	if err := preflightVerdict(true, reports, 35*time.Millisecond); err == nil {
		t.Error("the same rungs passed with -preflight on")
	}
}

// TestBenchReportPrintsTheSendLoopsOwnLateness is the column that says whether the
// column above it is the server's. Without it a generator running behind its own
// schedule charges its lateness to the engine and the report reads as an engine that
// got slower — which is a reading four void rounds could not rule out from the data.
func TestBenchReportPrintsTheSendLoopsOwnLateness(t *testing.T) {
	var w bytes.Buffer
	r := benchReport{
		rate:     27.28,
		all:      loadgen.Quantiles{N: 12000, P50: 37 * time.Millisecond, P50ok: true},
		dispatch: loadgen.Quantiles{N: 12000, P50: 437 * time.Microsecond, P50ok: true, Max: 1218 * time.Microsecond},
		faults:   loadgen.FaultCounts{Minor: 10},
		peakRSS:  345 << 20,
		elapsed:  6 * time.Minute,
	}

	r.print(&w)

	got := w.String()
	for _, want := range []string{"dispatch", "437µs", "1.218ms"} {
		if !strings.Contains(got, want) {
			t.Errorf("the report does not carry %q:\n%s", want, got)
		}
	}
}

// TestBenchReportOmitsTheDispatchQuantileWhenTooThin: the same rule every other
// quantile on the line follows. A rung cut short has a handful of dispatch samples, and
// a p50 printed off them reads as a loop that was on schedule.
func TestBenchReportOmitsTheDispatchQuantileWhenTooThin(t *testing.T) {
	var w bytes.Buffer
	r := benchReport{
		rate:     27.28,
		all:      loadgen.Quantiles{N: 12, P50: 37 * time.Millisecond},
		dispatch: loadgen.Quantiles{N: 12, P50: 400 * time.Microsecond, Max: time.Millisecond},
		elapsed:  time.Second,
	}

	r.print(&w)

	if strings.Contains(w.String(), "400µs") {
		t.Errorf("a 12-sample dispatch p50 was printed as a measurement:\n%s", w.String())
	}
}

// TestBenchReportSaysWhichRungRaisedThePeak: the rung that sets the mark is the one
// whose memory it describes, and it is the only rung that can say so.
func TestBenchReportSaysWhichRungRaisedThePeak(t *testing.T) {
	var w bytes.Buffer
	r := benchReport{
		rate:      13.64,
		all:       loadgen.Quantiles{N: 10000, P50: 41 * time.Millisecond, P50ok: true},
		faults:    loadgen.FaultCounts{Minor: 52890},
		peakRSS:   345 << 20,
		rssRaised: 206 << 20,
		elapsed:   12 * time.Minute,
	}

	r.print(&w)

	got := w.String()
	if !strings.Contains(got, "206") {
		t.Errorf("a rung that raised the process peak by 206 MiB did not say so:\n%s", got)
	}
}

// TestBenchReportRefusesToLetAnEarlierRungsPeakReadAsItsOwn is the same defect from the
// side that misleads. A rung that adds nothing to the mark still prints the mark, and a
// reader holding that against a per-rung threshold is judging a different rung. What
// this rung's own peak was is not knowable from getrusage at all — so the line has to
// say the figure is not its own rather than leave it to be assumed.
func TestBenchReportRefusesToLetAnEarlierRungsPeakReadAsItsOwn(t *testing.T) {
	var w bytes.Buffer
	r := benchReport{
		rate:      27.28,
		all:       loadgen.Quantiles{N: 10000, P50: 37 * time.Millisecond, P50ok: true},
		faults:    loadgen.FaultCounts{Minor: 10},
		peakRSS:   345 << 20,
		rssRaised: 0,
		elapsed:   6 * time.Minute,
	}

	r.print(&w)

	got := w.String()
	if !strings.Contains(got, "unchanged") {
		t.Errorf("a rung that did not raise the mark let the mark read as its own:\n%s", got)
	}
}

// TestBenchReportSaysWhatAQueryAllocated is the other half of the attribution the peak
// mark cannot give. `ru_maxrss` says what the process reached and refuses to say which
// rung got it there; a rung's allocation total says what the work cost, and dividing it
// by the samples that did the work turns a ladder-wide figure into a per-query one that
// a fix can be held against. FINDINGS milestone 8 section 9 is a mark left unattributed
// because nothing on the line said this.
//
// Shed requests are not in the denominator and must not be: the driver sheds by never
// dispatching, so a shed request allocates nothing and would only dilute the figure.
func TestBenchReportSaysWhatAQueryAllocated(t *testing.T) {
	var w bytes.Buffer
	const samples = 10000
	r := benchReport{
		rate:       13.64,
		all:        loadgen.Quantiles{N: samples, P50: 41 * time.Millisecond, P50ok: true},
		shed:       0,
		faults:     loadgen.FaultCounts{Minor: 52890},
		allocBytes: samples * 100 * 1024,
		allocs:     samples * 40,
		peakRSS:    345 << 20,
		rssRaised:  206 << 20,
		elapsed:    12 * time.Minute,
	}

	r.print(&w)

	got := w.String()
	if !strings.Contains(got, "100.0") {
		t.Errorf("a rung that allocated 100.0 KiB per query did not say so:\n%s", got)
	}
	if !strings.Contains(got, "40 allocs") {
		t.Errorf("a rung that made 40 allocations per query did not say so:\n%s", got)
	}
}

// TestBenchReportOmitsTheAllocationLineWhenNothingWasMeasured keeps the division honest.
// A rung interrupted before its first sample has a total and no denominator, and the
// only two things that could be printed there are a crash and a zero — the second being
// the same mistake the rusage omission already refuses, a figure that reads as a
// measurement when nothing was measured.
func TestBenchReportOmitsTheAllocationLineWhenNothingWasMeasured(t *testing.T) {
	var w bytes.Buffer
	r := benchReport{
		rate:       13.64,
		all:        loadgen.Quantiles{},
		allocBytes: 4096,
		allocs:     8,
		faults:     loadgen.FaultCounts{Minor: 3},
		peakRSS:    120 << 20,
		elapsed:    time.Second,
	}

	r.print(&w)

	if got := w.String(); strings.Contains(got, "/query") {
		t.Errorf("a rung with no samples printed a per-query figure anyway:\n%s", got)
	}
}

// TestWriteAllocProfile is the escalation the rung allocation line earns rather than
// replaces. That line says a query allocated 15.7 MiB and cannot say what did; the
// `allocs` profile is by call site, so it can. Cumulative since process start, which is
// why a twenty-second run answers the question and no ladder is needed — and also why the
// profile carries the index mapping and the cold pass beside the rung, which is a caveat
// on reading it rather than on writing it.
//
// The failure mode worth a test is the silent one: a path that cannot be written, found
// after the run rather than before it. An empty path writes nothing and is not an error,
// because that is every run that did not ask for a profile.
func TestWriteAllocProfile(t *testing.T) {
	t.Run("writes a profile a reader can open", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "allocs.pb.gz")
		if err := writeAllocProfile(path); err != nil {
			t.Fatalf("writeAllocProfile: %v", err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		// pprof writes gzipped protobuf. Two bytes is the whole check: a truncated or
		// unflushed write is what this catches, and parsing the profile here would mean
		// vendoring a reader for it.
		if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
			t.Errorf("wrote %d bytes not beginning with the gzip magic: % x", len(b), b[:min(len(b), 8)])
		}
	})

	t.Run("refuses a path it cannot write", func(t *testing.T) {
		// A directory that does not exist, so the failure is the create rather than a
		// permission the test would have to arrange.
		path := filepath.Join(t.TempDir(), "nope", "allocs.pb.gz")
		if err := writeAllocProfile(path); err == nil {
			t.Error("writing to a directory that does not exist reported success")
		}
	})

	t.Run("an empty path is not an error", func(t *testing.T) {
		if err := writeAllocProfile(""); err != nil {
			t.Errorf("no -memprofile asked for, and it failed: %v", err)
		}
	})
}

func TestBenchFlagsParsesAMemProfilePath(t *testing.T) {
	o, err := benchFlags([]string{"-memprofile", "/tmp/allocs.pb.gz"})
	if err != nil {
		t.Fatalf("benchFlags: %v", err)
	}
	if o.memprofile != "/tmp/allocs.pb.gz" {
		t.Errorf("memprofile = %q, want the path given", o.memprofile)
	}
}
