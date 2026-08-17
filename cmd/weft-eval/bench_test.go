// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The load driver's logic is tested here without an index, and that separation is
// deliberate: everything a published p99 depends on — how a quantile is taken, when
// one is too thin to print, whether a stalled server drags the load down with it —
// is arithmetic and scheduling, not search. A test needing .eval-data would run
// nowhere but this machine, and these are the assertions that have to survive CI.

// TestQuantileAtTheBoundaries pins nearest-rank against the cases where the
// definitions disagree.
//
// The choice is nearest-rank on the sorted sample, ceil(q*n) clamped into range,
// because the alternative — interpolating between neighbours — invents a latency
// no request experienced. A published tail figure should be a number the server
// actually produced.
func TestQuantileAtTheBoundaries(t *testing.T) {
	ms := func(n ...int) []time.Duration {
		out := make([]time.Duration, len(n))
		for i, v := range n {
			out[i] = time.Duration(v) * time.Millisecond
		}
		return out
	}
	tests := []struct {
		name   string
		sorted []time.Duration
		q      float64
		want   time.Duration
	}{
		{"single sample is every quantile", ms(7), 0.99, 7 * time.Millisecond},
		{"two samples, median is the lower", ms(1, 2), 0.5, 1 * time.Millisecond},
		{"two samples, p99 is the upper", ms(1, 2), 0.99, 2 * time.Millisecond},
		{"exact percentile lands on a real sample", ms(1, 2, 3, 4), 0.5, 2 * time.Millisecond},
		{"p100 is the maximum", ms(1, 2, 3, 4), 1.0, 4 * time.Millisecond},
		{"q at or below zero is the minimum", ms(1, 2, 3, 4), 0, 1 * time.Millisecond},
		// 0ms..99ms. Nearest-rank puts p99 at rank ceil(0.99*100)=99, which is
		// the 99th smallest and therefore 98ms — not the maximum. A quantile that
		// answered 99ms here would be reporting the top sample as the tail, which
		// is how a p99 quietly becomes a max.
		{"p99 of a hundred is the 99th smallest", ms(fill(100)...), 0.99, 98 * time.Millisecond},
		{"empty sample has no quantile", nil, 0.5, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := quantile(tc.sorted, tc.q); got != tc.want {
				t.Errorf("quantile(%v, %v) = %v, want %v", tc.sorted, tc.q, got, tc.want)
			}
		})
	}
}

// fill returns 0..n-1, which ms turns into 0ms..(n-1)ms.
func fill(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// TestQuantileIsNotPrintedWhenTooThin is journey 3: a quantile the sample cannot
// support is absent rather than wrong.
//
// The rule is registered in the plan and encoded here rather than chosen at print
// time: a quantile needs at least 100 samples beyond it, so p99 needs 10,000 and
// p99.9 needs 100,000. Below that the figure is an order statistic of a handful of
// observations and moves by milliseconds between runs — the exact way a tail number
// becomes something nobody can reproduce.
func TestQuantileIsNotPrintedWhenTooThin(t *testing.T) {
	tests := []struct {
		n    int
		q    float64
		want bool
	}{
		{200, 0.50, true},
		{199, 0.50, false},
		{2000, 0.95, true},
		{1999, 0.95, false},
		{10000, 0.99, true},
		{9999, 0.99, false},
		{100000, 0.999, true},
		{99999, 0.999, false},
		{0, 0.50, false},
	}
	for _, tc := range tests {
		if got := printableQuantile(tc.n, tc.q); got != tc.want {
			t.Errorf("printableQuantile(%d, %v) = %v, want %v", tc.n, tc.q, got, tc.want)
		}
	}
}

// TestOpenLoopDoesNotLetTheServerSlowTheLoad is journey 4, and it is the assertion
// the whole milestone rests on.
//
// A closed-loop driver sends the next request when the last one returns, so a server
// that stalls receives less load and reports one slow request. An open loop sends on
// a clock and measures from the intended send time, so a stall shows up in every
// request that was due during it. That difference is coordinated omission, and a
// milestone publishing p99 cannot be measured by the instrument that hides it.
//
// The synthetic server below serializes on a mutex and stalls once. Under an open
// loop the requests due during the stall queue behind it and inherit its wait; under
// a closed loop there would be exactly one slow sample. The assertion is on the count
// of slow samples, not on their values, because the values are wall-clock and the
// count is what distinguishes the two designs.
func TestOpenLoopDoesNotLetTheServerSlowTheLoad(t *testing.T) {
	const (
		n     = 40
		rate  = 200.0 // one every 5ms
		stall = 150 * time.Millisecond
	)
	var mu sync.Mutex
	do := func(i int) {
		mu.Lock()
		defer mu.Unlock()
		if i == 5 {
			time.Sleep(stall)
		}
	}

	samples, shed := driveOpenLoop(context.Background(), rate, n, 256, do)
	if len(samples)+shed != n {
		t.Fatalf("samples %d + shed %d != %d requested", len(samples), shed, n)
	}

	// Requests 6..~35 are due during the stall and wait for it. A closed loop
	// would produce one. Ten is far below the ~29 an open loop produces and far
	// above the one it must beat.
	var slow int
	for _, s := range samples {
		if s.lat >= stall/2 {
			slow++
		}
	}
	if slow < 10 {
		t.Errorf("%d samples over %v; a closed loop yields 1, an open loop many. "+
			"latency is not being measured from the intended send time", slow, stall/2)
	}
}

// TestOpenLoopShedsRatherThanBlocks pins the one place the driver is allowed to stop
// sending.
//
// Unbounded in-flight requests are the honest open loop and also the way a 210 MiB
// working set becomes an OOM at twice saturation. The cap is a real limit, so what
// matters is that exceeding it is counted and reported instead of quietly turning the
// driver back into a closed loop: a driver that blocked on a full slot would be
// waiting for the server again, which is exactly the bias the test above forbids.
func TestOpenLoopShedsRatherThanBlocks(t *testing.T) {
	const n = 60
	start := time.Now()
	samples, shed := driveOpenLoop(context.Background(), 1000, n, 2, func(int) {
		time.Sleep(30 * time.Millisecond)
	})
	elapsed := time.Since(start)

	if shed == 0 {
		t.Errorf("in-flight cap 2 against 1000/s of 30ms work shed nothing")
	}
	if len(samples)+shed != n {
		t.Errorf("samples %d + shed %d != %d requested", len(samples), shed, n)
	}
	// n/rate is 60ms of sending. Blocking on the cap would take n*30ms/2 = 900ms.
	if elapsed > 500*time.Millisecond {
		t.Errorf("sending %d at 1000/s took %v; the driver blocked on the cap "+
			"instead of shedding, which makes it closed-loop under load", n, elapsed)
	}
}

// TestOpenLoopRespectsCancellation keeps a long ladder interruptible. A rung is
// minutes of wall clock and Ctrl-C has to end it.
func TestOpenLoopRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	start := time.Now()
	samples, _ := driveOpenLoop(ctx, 100, 10000, 64, func(int) {})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("10000 samples at 100/s ran %v after cancellation at 50ms", elapsed)
	}
	if len(samples) >= 10000 {
		t.Errorf("cancellation did not stop the send loop: %d samples", len(samples))
	}
}

// TestSplitByGCSeparatesTheTwoDistributions is journey 2.
//
// "p99 including GC pause" is only meaningful beside a p99 that excludes it, and the
// gap between them is the figure the milestone publishes. What makes that honest is
// the contract on the classification, stated here and in the report: a sample is
// GC-hit when the cycle counter moved during it, which is not the same claim as "this
// request was slowed by a stop-the-world pause". Go does not expose the second.
func TestSplitByGCSeparatesTheTwoDistributions(t *testing.T) {
	in := []benchSample{
		{lat: 10 * time.Millisecond, gcHit: false},
		{lat: 90 * time.Millisecond, gcHit: true},
		{lat: 20 * time.Millisecond, gcHit: false},
		{lat: 80 * time.Millisecond, gcHit: true},
	}
	free, hit := splitByGC(in)
	if len(free) != 2 || len(hit) != 2 {
		t.Fatalf("split %d free / %d hit, want 2/2", len(free), len(hit))
	}
	if free[0] != 10*time.Millisecond || hit[0] != 90*time.Millisecond {
		t.Errorf("split did not preserve latencies: free %v hit %v", free, hit)
	}
	// Empty in, empty out — a run with no GC at all must not report a GC-hit
	// distribution built from zero samples.
	if f, h := splitByGC(nil); len(f) != 0 || len(h) != 0 {
		t.Errorf("splitByGC(nil) = %v, %v; want empty", f, h)
	}
}

// TestSummarizeDoesNotMutateTheCaller matters because the driver hands the same
// slice to three summaries — all, GC-free and GC-hit — and a summarizer that sorted
// in place would reorder samples whose order still carries the send sequence.
func TestSummarizeDoesNotMutateTheCaller(t *testing.T) {
	in := []time.Duration{3, 1, 2}
	summarize(in)
	if in[0] != 3 || in[1] != 1 || in[2] != 2 {
		t.Errorf("summarize sorted the caller's slice: %v", in)
	}
}

// TestSaturationIsTheFirstRungPastTwiceTheUnloadedMedian pins the load-point rule,
// which the plan fixed before any number existed.
//
// The headline p99 is reported at one rung of the ladder, and which rung must not be
// chosen after seeing the p99s. Saturation is where p50 passes twice the unloaded
// median; the headline is the rung nearest half that rate.
func TestSaturationIsTheFirstRungPastTwiceTheUnloadedMedian(t *testing.T) {
	unloaded := 10 * time.Millisecond
	tests := []struct {
		name    string
		rates   []float64
		p50s    []time.Duration
		wantSat float64
		wantHL  float64
	}{
		{
			name:  "third rung saturates",
			rates: []float64{1, 2, 4, 8},
			p50s: []time.Duration{
				10 * time.Millisecond, 11 * time.Millisecond,
				60 * time.Millisecond, 400 * time.Millisecond,
			},
			wantSat: 4, wantHL: 2,
		},
		{
			name:  "nothing saturates, headline is the top rung",
			rates: []float64{1, 2, 4},
			p50s: []time.Duration{
				10 * time.Millisecond, 10 * time.Millisecond, 11 * time.Millisecond,
			},
			wantSat: 0, wantHL: 4,
		},
		{
			name:    "exactly twice does not saturate; past it does",
			rates:   []float64{1, 2},
			p50s:    []time.Duration{20 * time.Millisecond, 21 * time.Millisecond},
			wantSat: 2, wantHL: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sat := saturationRate(tc.rates, tc.p50s, unloaded)
			if sat != tc.wantSat {
				t.Errorf("saturationRate = %v, want %v", sat, tc.wantSat)
			}
			if hl := headlineRate(tc.rates, sat); hl != tc.wantHL {
				t.Errorf("headlineRate = %v, want %v", hl, tc.wantHL)
			}
		})
	}
}
