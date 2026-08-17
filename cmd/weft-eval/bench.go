// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"math"
	"runtime/metrics"
	"slices"
	"sync"
	"time"
)

// bench is milestone 5's instrument: what a query costs when there are many of
// them, which is a different question from what one costs.
//
// `recall` measures a query once and reports its bytes, its candidates and its
// latency. Those are averages of a sequential replay, and a mean is exactly the
// statistic a tail is invisible in. The milestone's outcome sentence asks for a
// p99 including GC pause, and neither half of that is derivable from what already
// exists here: 50 queries cannot support a 99th percentile, and a sequential
// replay never has two queries in flight, which is the only state in which a
// stop-the-world pause can be charged to a request that did not cause it.
//
// Three things it prints, and why each is here rather than assumed:
//
//	latency        the distribution, measured from the intended send time
//	GC-free/-hit   the same distribution split by whether a cycle ran during it
//	rusage         page faults, context switches and peak RSS for the run
//
// The second is the one the milestone is named for. Section 1 of the plan predicts
// that weft's tail is a working-set problem rather than a GC problem — a small live
// heap makes collections frequent and cheap, while 210 MiB of pages per query makes
// a miss expensive — and the split is what decides that. The third is what says
// which kind of miss: minor faults are page-cache hits, major faults are the disk.

// benchSample is one request's outcome.
//
// lat is measured from the time the request was *due*, not from the time it
// started running. Under an open loop those differ whenever the server is behind,
// and the difference is the whole of what coordinated omission hides.
type benchSample struct {
	lat   time.Duration
	gcHit bool
}

// benchQuantiles is a latency distribution reduced to what gets printed.
//
// Every field is a real observation rather than an interpolation, and the ok flags
// say which are supported by enough samples to mean anything — see
// printableQuantile. A quantile that is not ok is left out of the report rather
// than printed with a caveat, because a caveat next to a number is not what a
// reader carries away from a table.
type benchQuantiles struct {
	n                           int
	p50, p95, p99, p999, max    time.Duration
	p50ok, p95ok, p99ok, p999ok bool
}

// quantile returns the q-th quantile of an already-sorted sample by nearest rank.
//
// Nearest rank rather than linear interpolation: the interpolating definitions
// return a value between two observations, which for a latency report is a number
// no request ever experienced. rank is ceil(q*n) clamped into [1, n], so p0 is the
// minimum, p100 is the maximum, and every answer is a sample.
//
// An empty sample has no quantile and returns zero. Callers distinguish that from a
// zero latency through n, which every report carries beside the figures.
func quantile(sorted []time.Duration, q float64) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil(q * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// printableQuantile reports whether n samples can support the q-th quantile.
//
// The rule is 100 samples beyond it, fixed in the plan before any number existed:
// p50 needs 200, p95 needs 2,000, p99 needs 10,000, p99.9 needs 100,000. Below that
// the figure is an order statistic of a handful of observations and moves by
// milliseconds between runs, which is how a tail number becomes one nobody can
// reproduce.
//
// The tolerance is not decoration. Neither 0.99 nor 0.999 is representable in
// binary, so n*(1-q) at the exact boundary lands a few parts in 10^13 either side
// of 100 — at n=100,000 and q=0.999 it computes as 99.99999999999454. The nearest
// real n that should fail misses by 0.001, three orders of magnitude outside this
// window, so the tolerance separates float noise from a genuine shortfall and
// nothing else.
func printableQuantile(n int, q float64) bool {
	return float64(n)*(1-q) >= 100-1e-6
}

// summarize reduces latencies to the printed distribution.
//
// It sorts a copy. The caller holds one slice and asks for three summaries — all
// samples, GC-free and GC-hit — and sorting in place would reorder a slice whose
// order still carries the send sequence, which is the only thing that could later
// answer "did the tail cluster in time or spread across the run".
func summarize(lats []time.Duration) benchQuantiles {
	n := len(lats)
	s := slices.Clone(lats)
	slices.Sort(s)
	q := benchQuantiles{
		n:      n,
		p50:    quantile(s, 0.50),
		p95:    quantile(s, 0.95),
		p99:    quantile(s, 0.99),
		p999:   quantile(s, 0.999),
		p50ok:  printableQuantile(n, 0.50),
		p95ok:  printableQuantile(n, 0.95),
		p99ok:  printableQuantile(n, 0.99),
		p999ok: printableQuantile(n, 0.999),
	}
	if n > 0 {
		q.max = s[n-1]
	}
	return q
}

// splitByGC separates the samples a garbage collection ran during from the rest.
//
// The contract is narrower than the name suggests and the report says so too: a
// sample is GC-hit when the cycle counter moved between the request's start and its
// finish. That is not the same claim as "this request was slowed by a
// stop-the-world pause" — Go exposes no per-goroutine attribution of pause time,
// and a request can overlap a cycle without being descheduled by it. What the two
// distributions support is the weaker and still useful statement the milestone
// needs: the gap between them is what overlapping a collection is worth, and if the
// gap is small then the tail is not the collector's.
func splitByGC(samples []benchSample) (free, hit []time.Duration) {
	for _, s := range samples {
		if s.gcHit {
			hit = append(hit, s.lat)
		} else {
			free = append(free, s.lat)
		}
	}
	return free, hit
}

// procFaultCounts is what the kernel says a run was doing, beside what the
// latency distribution says it cost. Both platform files return it, which is why
// it lives here rather than behind a build tag.
//
// The fields are documented where they are read, in rusage_unix.go. What belongs
// here is why the type exists at all: latency alone cannot separate a query
// waiting on a disk from a query waiting on a core from a query doing arithmetic,
// and milestone 5's section 1 makes a prediction that needs exactly that
// separation.
type procFaultCounts struct {
	minor, major  int64
	nvcsw, nivcsw int64
}

// sub returns the counts accumulated between two snapshots.
//
// Every figure the report prints is a difference. The absolute counters include
// mapping the index, loading the query set and starting the Go runtime — all of it
// before the first request is sent — so a report quoting them raw would charge the
// load with the process's whole history.
func (c procFaultCounts) sub(prev procFaultCounts) procFaultCounts {
	return procFaultCounts{
		minor:  c.minor - prev.minor,
		major:  c.major - prev.major,
		nvcsw:  c.nvcsw - prev.nvcsw,
		nivcsw: c.nivcsw - prev.nivcsw,
	}
}

// gcCycleMetric is the completed-collection counter. Read through runtime/metrics
// rather than runtime.ReadMemStats, which stops the world to answer: an instrument
// that pauses the program once per request would be measuring pauses it caused.
const gcCycleMetric = "/gc/cycles/total:gc-cycles"

// gcCycles is the number of collections completed so far.
func gcCycles() uint64 {
	s := []metrics.Sample{{Name: gcCycleMetric}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return s[0].Value.Uint64()
}

// driveOpenLoop sends n requests at a fixed arrival rate and returns what they cost.
//
// Open loop is the whole point. A closed-loop driver sends the next request when the
// last one returns, so a server that stalls receives less load and the stall appears
// as one slow request; every request that *would* have arrived during it was never
// sent, and the p99 that comes out is a p99 of a load the server chose. Here the
// schedule is arithmetic on the start time — request i is due at start + i/rate,
// whatever request i-1 is doing — and each latency is measured from that due time,
// so a stall lands on every request it delayed.
//
// maxInflight is the one place the loop is allowed to stop sending, and exceeding it
// is counted and returned rather than waited on. Waiting would make the driver
// closed-loop again at exactly the load where that bias matters most; shedding keeps
// the schedule honest and turns overload into a number the report can print. The cap
// exists because in-flight requests are not free — a query's working set is hundreds
// of megabytes on the evaluation corpus, so an unbounded loop at twice saturation is
// an out-of-memory rather than a measurement.
//
// do must be safe for concurrent use. It is handed the request's index so a caller
// can rotate through a query set without sharing a cursor.
func driveOpenLoop(ctx context.Context, rate float64, n, maxInflight int, do func(int)) ([]benchSample, int) {
	if maxInflight < 1 {
		maxInflight = 1
	}
	interval := time.Duration(float64(time.Second) / rate)

	var (
		mu      sync.Mutex
		samples = make([]benchSample, 0, n)
		shed    int
		wg      sync.WaitGroup
	)
	slots := make(chan struct{}, maxInflight)
	start := time.Now()

	for i := range n {
		due := start.Add(time.Duration(i) * interval)
		if d := time.Until(due); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				// Requests never sent are not shed requests: shedding is a
				// property of the load the run applied, and a cancelled run
				// applied less load rather than overloading. The report prints
				// the sample count alongside, which is what says the run was
				// cut short.
				wg.Wait()
				return samples, shed
			}
		}
		select {
		case slots <- struct{}{}:
		default:
			mu.Lock()
			shed++
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			before := gcCycles()
			do(i)
			s := benchSample{lat: time.Since(due), gcHit: gcCycles() != before}
			mu.Lock()
			samples = append(samples, s)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return samples, shed
}

// saturationRate is the lowest rate on the ladder whose median passed twice the
// unloaded median, or 0 if none did.
//
// The rule is registered before the numbers exist, which is the point of it being a
// function rather than a judgment at reporting time. A run's headline p99 has to be
// quoted at some load, and choosing that load after seeing the p99s is how a
// performance claim is made to say whatever its author wants. Twice the unloaded
// median is the definition of "the queue has started to build"; strictly past, so a
// rung sitting exactly at twice is still counted as below saturation and its numbers
// remain quotable.
func saturationRate(rates []float64, p50s []time.Duration, unloaded time.Duration) float64 {
	for i := range rates {
		if i >= len(p50s) {
			break
		}
		if p50s[i] > 2*unloaded {
			return rates[i]
		}
	}
	return 0
}

// headlineRate is the rung the report quotes: the one nearest half of saturation.
//
// Half, because a load point at saturation measures the queue and a load point far
// below it measures nothing the server would not do idle. A ladder that never
// saturated has no such midpoint, and the honest answer there is its top rung — the
// most load actually applied — with the report saying saturation was not reached.
func headlineRate(rates []float64, sat float64) float64 {
	if len(rates) == 0 {
		return 0
	}
	if sat == 0 {
		return rates[len(rates)-1]
	}
	target := sat / 2
	best := rates[0]
	for _, r := range rates[1:] {
		if math.Abs(r-target) < math.Abs(best-target) {
			best = r
		}
	}
	return best
}
