// SPDX-License-Identifier: Apache-2.0

// Package loadgen is milestone 5's instrument: what a query costs when there are
// many of them, which is a different question from what one costs.
//
// `recall` measures a query once and reports its bytes, its candidates and its
// latency. Those are averages of a sequential replay, and a mean is exactly the
// statistic a tail is invisible in. The milestone's outcome sentence asks for a
// p99 including GC pause, and neither half of that is derivable from what already
// exists here: 50 queries cannot support a 99th percentile, and a sequential
// replay never has two queries in flight, which is the only state in which a
// stop-the-world pause can be charged to a request that did not cause it.
//
// Three things a report built on this prints, and why each is here rather than
// assumed:
//
//	latency        the distribution, measured from the intended send time
//	minus STW      the same samples with their charged stop-the-world time removed
//	rusage         page faults, context switches and peak RSS for the run
//
// The second is the one the milestone is named for. Section 1 of the plan predicts
// that weft's tail is a working-set problem rather than a GC problem — a small live
// heap makes collections frequent and cheap, while 210 MiB of pages per query makes
// a miss expensive — and the gap between the two distributions is what decides
// that. The third is what says which kind of miss: minor faults are page-cache
// hits, major faults are the disk.
package loadgen

import (
	"context"
	"math"
	"runtime"
	"runtime/metrics"
	"slices"
	"sync"
	"time"
)

// Ladder is the arrival rates a run sweeps, as fractions of the throughput a
// sequential replay achieved.
//
// Five rungs rather than a search for the knee: the knee is what the run is trying
// to report, and a driver that hunted for it would be choosing its own load point.
// 200% is included because a rung past saturation is what makes the ones below it
// legible — a ladder that never bends leaves "below saturation" unevidenced.
var Ladder = []float64{0.125, 0.25, 0.5, 1.0, 2.0}

// DefaultInflight caps concurrent requests. Four per core is enough that the cap is
// not what limits throughput at the rungs below saturation, and low enough that the
// 200% rung sheds instead of exhausting memory: a query's working set is 210 MiB on
// the evaluation corpus (docs/FINDINGS.md milestone 3b), so unbounded in-flight
// requests at twice saturation is an out-of-memory rather than a measurement.
func DefaultInflight() int { return 4 * runtime.GOMAXPROCS(0) }

// Sample is one request's outcome.
//
// lat is measured from the time the request was *due*, not from the time it
// started running. Under an open loop those differ whenever the server is behind,
// and the difference is the whole of what coordinated omission hides.
type Sample struct {
	Lat time.Duration

	// gcPause is the process-wide stop-the-world time that elapsed inside this
	// request's window. A pause stops every goroutine, so it is time this request
	// provably spent stopped whatever else it was doing, and subtracting it gives
	// the counterfactual the milestone asks for.
	//
	// It is not all of what the collector costs. Mark assist charges allocation to
	// the goroutine performing it — here, the query — and none of that is
	// stop-the-world or visible in this field. GCCPUShare is what reports it.
	GCPause time.Duration

	// Due is when this request was scheduled to be sent, as an offset from the
	// run's start.
	//
	// Samples come back in completion order rather than send order, so without it
	// "which requests were in flight while something else was happening" is not
	// answerable from the slice. That question is the whole of the write-lock
	// measurement milestone 3b section 4.3 asked this milestone for: a Commit is
	// one event, and what it costs is the difference between the reads due during
	// it and the reads due either side. SplitByWindow is the arithmetic.
	Due time.Duration
}

// SplitByWindow separates the latencies of requests due inside [from, to) from the
// rest, both offsets from the run's start.
//
// Half-open, and the boundary is a real decision rather than a convention: a
// request due exactly when a commit began waited for it, and one due exactly when
// the commit ended did not. Getting that backwards moves one sample, which matters
// at the edges of a window that only a few requests fall into.
//
// Latencies rather than samples, because the callers of this are the same
// Summarize the rest of the report goes through.
func SplitByWindow(samples []Sample, from, to time.Duration) (during, outside []time.Duration) {
	for _, s := range samples {
		if s.Due >= from && s.Due < to {
			during = append(during, s.Lat)
		} else {
			outside = append(outside, s.Lat)
		}
	}
	return during, outside
}

// Quantiles is a latency distribution reduced to what gets printed.
//
// Every field is a real observation rather than an interpolation, and the ok flags
// say which are supported by enough samples to mean anything — see
// Printable. A quantile that is not ok is left out of the report rather
// than printed with a caveat, because a caveat next to a number is not what a
// reader carries away from a table.
type Quantiles struct {
	N                           int
	P50, P95, P99, P999, Max    time.Duration
	P50ok, P95ok, P99ok, P999ok bool
}

// Quantile returns the q-th quantile of an already-sorted sample by nearest rank.
//
// Nearest rank rather than linear interpolation: the interpolating definitions
// return a value between two observations, which for a latency report is a number
// no request ever experienced. rank is ceil(q*n) clamped into [1, n], so p0 is the
// minimum, p100 is the maximum, and every answer is a sample.
//
// An empty sample has no quantile and returns zero. Callers distinguish that from a
// zero latency through n, which every report carries beside the figures.
func Quantile(sorted []time.Duration, q float64) time.Duration {
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

// Printable reports whether n samples can support the q-th quantile.
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
func Printable(n int, q float64) bool {
	return float64(n)*(1-q) >= 100-1e-6
}

// Summarize reduces latencies to the printed distribution.
//
// It sorts a copy. The caller holds the samples once and asks for two summaries —
// as measured and minus stop-the-world — off slices that SplitByGC returns
// index-aligned with each other and with the samples they came from. Sorting in
// place would destroy that alignment, and with it the only thing that could later
// answer "was the tail one request or the same request twice".
//
// The order it preserves is completion order, not send order: Drive appends a
// sample when its request returns. Send order is recoverable from the schedule —
// request i was due at start + i/rate — and is not carried here.
func Summarize(lats []time.Duration) Quantiles {
	n := len(lats)
	s := slices.Clone(lats)
	slices.Sort(s)
	q := Quantiles{
		N:      n,
		P50:    Quantile(s, 0.50),
		P95:    Quantile(s, 0.95),
		P99:    Quantile(s, 0.99),
		P999:   Quantile(s, 0.999),
		P50ok:  Printable(n, 0.50),
		P95ok:  Printable(n, 0.95),
		P99ok:  Printable(n, 0.99),
		P999ok: Printable(n, 0.999),
	}
	if n > 0 {
		q.Max = s[n-1]
	}
	return q
}

// SplitByGC returns each sample's latency twice: as measured, and with the
// stop-the-world time charged to it removed.
//
// Both slices hold every sample, which is the difference between this and the
// design it replaced. Classifying samples as GC-hit or GC-free was measured first
// and was degenerate: a smoke run made 485 collections over 200 queries, so every
// sample was hit and the free cohort was empty. At the allocation rate a query
// actually produces — 43.6 MiB for text, 181.9 MiB for text+vector — "did a
// collection run during this request" is true of everything and separates nothing.
//
// Charging separates. p99(raw) is the tail including pause, p99(exGC) is the tail
// without it, and the gap is what the milestone publishes. The subtraction clamps
// at zero: the pause total is process-wide and the window is two clock reads, so a
// pause straddling the boundary can be charged past the request's own duration, and
// a negative duration in a slice that gets sorted and quantiled is a wrong number
// rather than a visible error.
func SplitByGC(samples []Sample) (raw, exGC []time.Duration) {
	if len(samples) == 0 {
		return nil, nil
	}
	raw = make([]time.Duration, len(samples))
	exGC = make([]time.Duration, len(samples))
	for i, s := range samples {
		raw[i] = s.Lat
		d := s.Lat - s.GCPause
		if d < 0 {
			d = 0
		}
		exGC[i] = d
	}
	return raw, exGC
}

// FaultCounts is what the kernel says a run was doing, beside what the
// latency distribution says it cost. Both platform files return it, which is why
// it lives here rather than behind a build tag.
//
// The fields are documented where they are read, in rusage_unix.go. What belongs
// here is why the type exists at all: latency alone cannot separate a query
// waiting on a disk from a query waiting on a core from a query doing arithmetic,
// and milestone 5's section 1 makes a prediction that needs exactly that
// separation.
type FaultCounts struct {
	Minor, Major  int64
	Nvcsw, Nivcsw int64
}

// Sub returns the counts accumulated between two snapshots.
//
// Every figure the report prints is a difference. The absolute counters include
// mapping the index, loading the query set and starting the Go runtime — all of it
// before the first request is sent — so a report quoting them raw would charge the
// load with the process's whole history.
// Sub is exported because the caller holds the snapshots: a run's figure is the
// difference around it, never a raw counter.
func (c FaultCounts) Sub(prev FaultCounts) FaultCounts {
	return FaultCounts{
		Minor:  c.Minor - prev.Minor,
		Major:  c.Major - prev.Major,
		Nvcsw:  c.Nvcsw - prev.Nvcsw,
		Nivcsw: c.Nivcsw - prev.Nivcsw,
	}
}

// gcCycleMetric is the completed-collection counter. Read through runtime/metrics
// rather than runtime.ReadMemStats, which stops the world to answer: an instrument
// that pauses the program once per request would be measuring pauses it caused.
const gcCycleMetric = "/gc/cycles/total:gc-cycles"

// GCCycles is the number of collections completed so far.
func GCCycles() uint64 {
	s := []metrics.Sample{{Name: gcCycleMetric}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return s[0].Value.Uint64()
}

// Drive sends n requests at a fixed arrival rate and returns what they cost.
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
//
// rate must be positive; a non-positive rate has no schedule and returns nothing.
// Callers validate the flag and say so in words — this is the backstop that keeps
// a bad value out of time.Duration(float64(time.Second)/rate), whose float-to-int
// conversion at ±Inf is undefined in Go and differs between amd64 and arm64.
func Drive(ctx context.Context, rate float64, n, maxInflight int, do func(int)) (samples []Sample, shed int) {
	if maxInflight < 1 {
		maxInflight = 1
	}
	if !(rate > 0) { //nolint:staticcheck // written this way to reject NaN as well as <= 0
		return nil, 0
	}
	interval := time.Duration(float64(time.Second) / rate)

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	samples = make([]Sample, 0, n)
	slots := make(chan struct{}, maxInflight)
	start := time.Now()

	for i := range n {
		// Checked every iteration rather than only inside the wait below. Once the
		// send loop falls behind its own schedule — which is what a rate above what
		// the loop can dispatch produces — time.Until(due) is never positive, the
		// timer branch is never taken, and a cancellation checked only there would
		// not be seen until the rung had sent all n requests.
		if ctx.Err() != nil {
			// Requests never sent are not shed requests: shedding is a property
			// of the load the run applied, and a cancelled run applied less load
			// rather than overloading. The report prints the sample count
			// alongside, which is what says the run was cut short.
			wg.Wait()
			return samples, shed
		}
		offset := time.Duration(i) * interval
		due := start.Add(offset)
		if d := time.Until(due); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
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
			before := GCPauseTotal()
			do(i)
			s := Sample{Lat: time.Since(due), GCPause: GCPauseTotal() - before, Due: offset}
			mu.Lock()
			samples = append(samples, s)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return samples, shed
}

// SaturationRate is the lowest rate on the ladder whose median passed twice the
// unloaded median, or 0 if none did.
//
// The rule is registered before the numbers exist, which is the point of it being a
// function rather than a judgment at reporting time. A run's headline p99 has to be
// quoted at some load, and choosing that load after seeing the p99s is how a
// performance claim is made to say whatever its author wants. Twice the unloaded
// median is the definition of "the queue has started to build"; strictly past, so a
// rung sitting exactly at twice is still counted as below saturation and its numbers
// remain quotable.
func SaturationRate(rates []float64, p50s []time.Duration, unloaded time.Duration) float64 {
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

// HeadlineRate is the rung the report quotes: the one nearest half of saturation.
//
// Half, because a load point at saturation measures the queue and a load point far
// below it measures nothing the server would not do idle. A ladder that never
// saturated has no such midpoint, and the honest answer there is its top rung — the
// most load actually applied — with the report saying saturation was not reached.
func HeadlineRate(rates []float64, sat float64) float64 {
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

// gcPauseMetric is the stop-the-world histogram. Summed to a total rather than read
// as quantiles: the buckets are exponential and a quantile off them is an
// interpolation, which is the thing Quantile() refuses to print for latencies. The
// total is exact and answers the question the milestone asks — what share of a run's
// wall clock the collector took the world for.
const gcPauseMetric = "/gc/pauses:seconds"

// pauseBuf is the read buffer, reused, and that is not micro-optimisation: it is
// what keeps the instrument out of its own measurement.
//
// metrics.Read fills a Float64Histogram in place when the Sample it is handed
// already carries one of the right shape, and allocates a fresh 163-bucket one when
// it does not. GCPauseTotal is called twice per request — a ladder is 100,000 calls
// — and a freshly allocated []metrics.Sample each time measures at 1,504 B and 3
// allocs per call against 0 and 0 for a reused one. That is ~150 MiB of garbage per
// ladder produced by the pause counter, driving collections the report then charges
// to the query. Same sentence the package already applies to ReadMemStats: an
// instrument that makes the thing it measures is not one.
//
// ponytail: one global buffer under one mutex, not a sync.Pool. A Pool is drained at
// every GC, which on this workload is a few times a second — so it reallocates
// exactly when the collector is busiest, which is the case this exists for. The lock
// adds no real serialisation either: metrics.Read already takes a runtime-wide
// semaphore, so concurrent callers were queued before this mutex existed. If a
// profile ever shows this contended, shard it per-P.
var (
	pauseMu  sync.Mutex
	pauseBuf = []metrics.Sample{{Name: gcPauseMetric}}
)

func GCPauseTotal() time.Duration {
	pauseMu.Lock()
	defer pauseMu.Unlock()
	s := pauseBuf
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindFloat64Histogram {
		return 0
	}
	h := s[0].Value.Float64Histogram()
	var total float64
	for i, c := range h.Counts {
		// Bucket i spans [Buckets[i], Buckets[i+1]). Charged at its lower edge,
		// which under-reports rather than over-reports: a pause total that
		// flattered the collector would be the one way this number could mislead
		// in the direction the milestone's prediction wants.
		if c > 0 && i < len(h.Buckets) && !math.IsInf(h.Buckets[i], -1) {
			total += float64(c) * h.Buckets[i]
		}
	}
	return time.Duration(total * float64(time.Second))
}

// GCCPUShare is the fraction of the process's CPU time the collector has taken,
// pauses and assists together.
//
// It is here because the pause total is the collector's most visible cost and not
// its largest. A smoke run of this command against the evaluation corpus put
// stop-the-world at 0.051% of the wall clock while the median query ran at 67.8 ms
// against an unloaded 35.1 ms, at 14% of measured capacity. Nothing in
// /gc/pauses:seconds explains that gap; mark assist does, and assist is charged to
// the goroutine that allocated — which is the query. A report that published the
// STW figure alone would invite exactly the wrong conclusion.
//
// Cumulative over the process, so this is the whole-process figure and a *run's*
// figure is GCCPUShareBetween of two snapshots. It is a ratio of two totals rather
// than a rate: what a reader wants is "how much of this machine went to the
// collector", which is dimensionless.
func GCCPUShare() float64 {
	gc, total := GCCPUSeconds()
	if total <= 0 {
		return 0
	}
	return gc / total
}

// GCCPUSeconds is the collector's CPU time and the process's total, both cumulative
// since the process started.
//
// Two numbers rather than their ratio because the ratio does not subtract. A rung's
// share is not share₁ - share₀: both are dominated by everything the process did
// before the rung began — mapping the index, the cold pass, every earlier rung — and
// by the fifth rung the running ratio has stopped moving whatever the collector is
// doing now. GCCPUShareBetween is the arithmetic that does hold.
func GCCPUSeconds() (gc, total float64) {
	s := []metrics.Sample{
		{Name: "/cpu/classes/gc/total:cpu-seconds"},
		{Name: "/cpu/classes/total:cpu-seconds"},
	}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindFloat64 || s[1].Value.Kind() != metrics.KindFloat64 {
		return 0, 0
	}
	return s[0].Value.Float64(), s[1].Value.Float64()
}

// GCCPUShareBetween is the collector's share of the CPU the process burned between
// two GCCPUSeconds snapshots — the ratio of the differences, which is the only form
// of it that describes one rung rather than the process's whole history.
func GCCPUShareBetween(gc0, total0, gc1, total1 float64) float64 {
	d := total1 - total0
	if d <= 0 {
		return 0
	}
	return (gc1 - gc0) / d
}
