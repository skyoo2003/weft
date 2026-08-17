// SPDX-License-Identifier: Apache-2.0

package loadgen

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
			if got := Quantile(tc.sorted, tc.q); got != tc.want {
				t.Errorf("Quantile(%v, %v) = %v, want %v", tc.sorted, tc.q, got, tc.want)
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
		if got := Printable(tc.n, tc.q); got != tc.want {
			t.Errorf("Printable(%d, %v) = %v, want %v", tc.n, tc.q, got, tc.want)
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

	samples, shed := Drive(context.Background(), rate, n, 256, do)
	if len(samples)+shed != n {
		t.Fatalf("samples %d + shed %d != %d requested", len(samples), shed, n)
	}

	// Requests 6..~35 are due during the stall and wait for it. A closed loop
	// would produce one. Ten is far below the ~29 an open loop produces and far
	// above the one it must beat.
	var slow int
	for _, s := range samples {
		if s.Lat >= stall/2 {
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
	samples, shed := Drive(context.Background(), 1000, n, 2, func(int) {
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
	samples, _ := Drive(ctx, 100, 10000, 64, func(int) {})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("10000 samples at 100/s ran %v after cancellation at 50ms", elapsed)
	}
	if len(samples) >= 10000 {
		t.Errorf("cancellation did not stop the send loop: %d samples", len(samples))
	}
}

// TestGCPauseIsChargedPerSampleNotClassified is journey 2, and it is the second
// design this milestone had — the first one was measured and did not work.
//
// The first design classified: a sample was GC-hit when the cycle counter moved
// during it, and the report compared the hit and free distributions. A smoke run
// against the evaluation corpus made 485 collections over 200 queries, so every
// single sample was GC-hit and the free distribution was empty. The classification
// is not wrong, it is degenerate at any allocation rate weft actually produces: at
// 2.4 cycles per query "did a collection run during this request" carries no
// information.
//
// Charging does work. A stop-the-world pause stops every goroutine, so the pause
// time that elapsed inside a request's window is time that request provably spent
// stopped, whatever else it was doing. Subtracting it gives the counterfactual the
// milestone's outcome sentence asks for — what the tail would be without the
// collector — and the gap between the two p99s is the answer, per sample rather
// than per cohort.
//
// What this still does not capture is stated in the report beside the number: mark
// assist is not stop-the-world, so a request charged zero pause can still have spent
// its time marking. That is why GCCPUShare is reported alongside and why the smoke
// run's 0.05% STW sat under a doubled median.
func TestGCPauseIsChargedPerSampleNotClassified(t *testing.T) {
	in := []Sample{
		{Lat: 10 * time.Millisecond, GCPause: 0},
		{Lat: 90 * time.Millisecond, GCPause: 30 * time.Millisecond},
		{Lat: 20 * time.Millisecond, GCPause: 2 * time.Millisecond},
	}
	raw, exGC := SplitByGC(in)
	if len(raw) != 3 || len(exGC) != 3 {
		t.Fatalf("split %d raw / %d ex-GC, want 3/3: every sample appears in both", len(raw), len(exGC))
	}
	if raw[1] != 90*time.Millisecond {
		t.Errorf("raw[1] = %v, want the unmodified latency", raw[1])
	}
	if exGC[1] != 60*time.Millisecond {
		t.Errorf("exGC[1] = %v, want 90ms - 30ms of charged pause", exGC[1])
	}
	if exGC[0] != 10*time.Millisecond {
		t.Errorf("exGC[0] = %v, want the latency unchanged when no pause was charged", exGC[0])
	}

	// A pause longer than the request it is charged to. Reachable: the pause total
	// is process-wide and the sample's window is read from two clock calls, so
	// rounding and a pause straddling the boundary can cross over. Clamping at zero
	// rather than letting a negative latency into a distribution that gets sorted
	// and quantiled.
	if _, ex := SplitByGC([]Sample{{Lat: time.Millisecond, GCPause: 5 * time.Millisecond}}); ex[0] != 0 {
		t.Errorf("over-charged sample = %v, want 0 rather than a negative latency", ex[0])
	}

	if r, e := SplitByGC(nil); len(r) != 0 || len(e) != 0 {
		t.Errorf("SplitByGC(nil) = %v, %v; want empty", r, e)
	}
}

// TestGCCPUShareIsAFraction covers the metric that explains the smoke run: a median
// that doubled under 14% of capacity while stop-the-world totalled 0.05% of the
// wall clock.
//
// Pause time is the collector's most visible cost and not its largest. Mark assist
// charges allocation to the goroutine doing it, which is the query, and none of that
// appears in /gc/pauses:seconds. Without this the report would publish a small STW
// figure next to an inflated latency and leave the gap unexplained — the reader's
// obvious conclusion being that the collector is cheap here, which the assist number
// contradicts.
func TestGCCPUShareIsAFraction(t *testing.T) {
	share := GCCPUShare()
	if share < 0 || share > 1 {
		t.Errorf("GCCPUShare() = %v, want a fraction in [0,1]", share)
	}
}

// TestSummarizeDoesNotMutateTheCaller matters because the driver hands the same
// slice to three summaries — all, GC-free and GC-hit — and a summarizer that sorted
// in place would reorder samples whose order still carries the send sequence.
func TestSummarizeDoesNotMutateTheCaller(t *testing.T) {
	in := []time.Duration{3, 1, 2}
	Summarize(in)
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
			sat := SaturationRate(tc.rates, tc.p50s, unloaded)
			if sat != tc.wantSat {
				t.Errorf("SaturationRate = %v, want %v", sat, tc.wantSat)
			}
			if hl := HeadlineRate(tc.rates, sat); hl != tc.wantHL {
				t.Errorf("HeadlineRate = %v, want %v", hl, tc.wantHL)
			}
		})
	}
}
