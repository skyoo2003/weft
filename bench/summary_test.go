// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/skyoo2003/weft/internal/loadgen"
)

// The first test in this module, and it is here for the reason the module exists.
//
// docs/PERF.md §3 rule 2 compares two p99s "measured on the same machine, corpus,
// query set, arm and load generator". A rule that suppressed an unearned headline on
// weft's side and not on bleve's would put the two sides of that comparison under
// different reporting discipline — and what the comparison produces is a ratio, so a
// claim admitted on one side alone moves it. summarize is asked the same question
// benchSummary is, through the same predicate.

func rungsAt(rates []float64, p50s []time.Duration) []rung {
	out := make([]rung, 0, len(p50s))
	for i := range p50s {
		out = append(out, rung{
			rate: rates[i],
			all:  loadgen.Quantiles{N: 10000, P50: p50s[i], P99: 60 * time.Millisecond, P99ok: true},
			exGC: loadgen.Quantiles{N: 10000, P99: 59 * time.Millisecond, P99ok: true},
		})
	}
	return out
}

func millis(n ...int) []time.Duration {
	out := make([]time.Duration, len(n))
	for i, v := range n {
		out[i] = time.Duration(v) * time.Millisecond
	}
	return out
}

func TestSummarizeQuotesARuleSelectedHeadlineOnAFullLadder(t *testing.T) {
	rates := []float64{20, 40, 80, 157, 314}
	p50s := millis(8, 8, 9, 12, 200)
	var w bytes.Buffer

	summarize(&w, rungsAt(rates, p50s), rates, p50s, 8*time.Millisecond)

	got := w.String()
	if !strings.Contains(got, "saturation:") || !strings.Contains(got, "HEADLINE") {
		t.Errorf("a complete ladder printed no rule-selected headline:\n%s", got)
	}
}

// TestSummarizeOnALadderThatNeverSaturatesQuotesItsTopRung is the branch bleve is
// likelier than weft to take: milestone 5 measured it staying flat through rungs weft
// had already collapsed on.
func TestSummarizeOnALadderThatNeverSaturatesQuotesItsTopRung(t *testing.T) {
	rates := []float64{20, 40, 80, 157, 314}
	p50s := millis(8, 8, 9, 10, 12) // none past 2x the 8ms unloaded median
	var w bytes.Buffer

	summarize(&w, rungsAt(rates, p50s), rates, p50s, 8*time.Millisecond)

	got := w.String()
	if !strings.Contains(got, "not reached") {
		t.Errorf("a ladder that never saturated did not say so:\n%s", got)
	}
	if !strings.Contains(got, "HEADLINE") || !strings.Contains(got, "314.00") {
		t.Errorf("the headline is not the top rung the run actually applied:\n%s", got)
	}
}

// TestSummarizePublishesNoHeadlineForALadderCutShort is the same defect weft's side
// carries, in the file that has to stay in step with it.
func TestSummarizePublishesNoHeadlineForALadderCutShort(t *testing.T) {
	rates := []float64{20, 40, 80, 157, 314}
	p50s := millis(8, 8, 9)
	var w bytes.Buffer

	summarize(&w, rungsAt(rates, p50s), rates, p50s, 8*time.Millisecond)

	got := w.String()
	if strings.Contains(got, "HEADLINE") || strings.Contains(got, "saturation:") {
		t.Errorf("an interrupted ladder published a rule's claim, and this side is the "+
			"denominator of the published ratio:\n%s", got)
	}
}

// TestSummarizePublishesNoHeadlineWhenARungWasSuspended holds this side to the same
// discipline. A comparison whose denominator was measured across a sleeping machine
// is not a comparison, and the ratio would carry no sign of it.
func TestSummarizePublishesNoHeadlineWhenARungWasSuspended(t *testing.T) {
	rates := []float64{20, 40, 80, 157, 314}
	p50s := millis(8, 8, 9, 12, 200)
	rs := rungsAt(rates, p50s)
	rs[0].unaccounted = 12*time.Hour + 20*time.Minute

	var w bytes.Buffer
	summarize(&w, rs, rates, p50s, 8*time.Millisecond)

	got := w.String()
	if strings.Contains(got, "HEADLINE") || strings.Contains(got, "saturation:") {
		t.Errorf("a ladder that ran across a suspension published a rule's claim:\n%s", got)
	}
	if !strings.Contains(got, "12h20m0s") {
		t.Errorf("the summary does not say how much time the process did not run:\n%s", got)
	}
}

func TestSummarizePublishesNoHeadlineForAnExplicitRate(t *testing.T) {
	rates := []float64{157}
	p50s := millis(8)
	var w bytes.Buffer

	summarize(&w, rungsAt(rates, p50s), rates, p50s, 7*time.Millisecond)

	got := w.String()
	if strings.Contains(got, "HEADLINE") || strings.Contains(got, "saturation:") {
		t.Errorf("an operator-chosen rate was published under a rule's label:\n%s", got)
	}
	if !strings.Contains(got, "157") {
		t.Errorf("the rung printed no figures at all:\n%s", got)
	}
}

func TestSummarizePrintsNothingWhenNothingWasMeasured(t *testing.T) {
	var w bytes.Buffer
	summarize(&w, nil, []float64{20, 40, 80}, nil, 8*time.Millisecond)
	if w.Len() != 0 {
		t.Errorf("a run with no completed rung printed %q", w.String())
	}
}
