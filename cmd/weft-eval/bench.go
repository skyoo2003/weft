// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skyoo2003/weft/internal/eval"
	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/scorer/text"
	"github.com/skyoo2003/weft/pkg/scorer/vector"
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

// benchLadder is the arrival rates a run sweeps, as fractions of the throughput a
// sequential replay achieved.
//
// Five rungs rather than a search for the knee: the knee is what the run is trying
// to report, and a driver that hunted for it would be choosing its own load point.
// 200% is included because a rung past saturation is what makes the ones below it
// legible — a ladder that never bends leaves "below saturation" unevidenced.
var benchLadder = []float64{0.125, 0.25, 0.5, 1.0, 2.0}

// benchInflight caps concurrent requests. Four per core is enough that the cap is
// not what limits throughput at the rungs below saturation, and low enough that the
// 200% rung sheds instead of exhausting memory: a query's working set is 210 MiB on
// the evaluation corpus (docs/FINDINGS.md milestone 3b), so unbounded in-flight
// requests at twice saturation is an out-of-memory rather than a measurement.
func benchInflight() int { return 4 * runtime.GOMAXPROCS(0) }

// benchSample is one request's outcome.
//
// lat is measured from the time the request was *due*, not from the time it
// started running. Under an open loop those differ whenever the server is behind,
// and the difference is the whole of what coordinated omission hides.
type benchSample struct {
	lat time.Duration

	// gcPause is the process-wide stop-the-world time that elapsed inside this
	// request's window. A pause stops every goroutine, so it is time this request
	// provably spent stopped whatever else it was doing, and subtracting it gives
	// the counterfactual the milestone asks for.
	//
	// It is not all of what the collector costs. Mark assist charges allocation to
	// the goroutine performing it — here, the query — and none of that is
	// stop-the-world or visible in this field. gcCPUShare is what reports it.
	gcPause time.Duration
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

// splitByGC returns each sample's latency twice: as measured, and with the
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
func splitByGC(samples []benchSample) (raw, exGC []time.Duration) {
	for _, s := range samples {
		raw = append(raw, s.lat)
		d := s.lat - s.gcPause
		if d < 0 {
			d = 0
		}
		exGC = append(exGC, d)
	}
	return raw, exGC
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
			before := gcPauseTotal()
			do(i)
			s := benchSample{lat: time.Since(due), gcPause: gcPauseTotal() - before}
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

// ---------------------------------------------------------------- command

// benchArms are the two the milestone publishes.
//
// `text` is the headline because it is the arm bleve can be compared against: its
// vector search needs a build tag and a cgo faiss, and pulling that in would make
// the comparison "weft against a Go wrapper around faiss" rather than against a Go
// search engine. `text+vector` is measured anyway and printed beside it, because
// the arm a user would actually deploy is not the one the comparison can use, and
// publishing only the flattering half of that is not what this repository does.
const (
	benchArmText   = "text"
	benchArmVector = "text+vector"
)

func benchScorers(ix *engine.Index, arm string) ([]engine.Scorer, error) {
	ts := text.New(ix)
	switch arm {
	case benchArmText:
		return []engine.Scorer{ts}, nil
	case benchArmVector:
		return []engine.Scorer{ts, vector.New(ix)}, nil
	}
	return nil, fmt.Errorf("-arm=%q: want %q or %q", arm, benchArmText, benchArmVector)
}

func bench(ctx context.Context, args []string) error {
	var (
		rate        float64
		rotations   int
		inflight    int
		arm         string
		anySnapshot bool
		cpuprofile  string
	)
	data, flagErr := dataFlags(cmdBench, args, func(fs *flag.FlagSet) {
		fs.Float64Var(&rate, "rate", 0, "arrival rate in queries/sec; 0 sweeps the ladder")
		fs.IntVar(&rotations, "rotations", 200, "passes over the query set per rung (50 queries x 200 = 10,000 samples)")
		fs.IntVar(&inflight, "inflight", 0, "cap on concurrent requests; 0 is 4 per core")
		fs.StringVar(&arm, "arm", benchArmText, "scorers to load: text or text+vector")
		fs.StringVar(&cpuprofile, "cpuprofile", "", "write a CPU profile of the whole run here")
		snapshotFlag(fs, &anySnapshot)
	})
	if flagErr != nil {
		return flagErr
	}
	if rotations <= 0 {
		return fmt.Errorf("-rotations=%d: a run needs at least one pass over the query set", rotations)
	}
	if inflight <= 0 {
		inflight = benchInflight()
	}
	if !anySnapshot {
		if err := verifySnapshot(*data, queriesFile, qrelsFile); err != nil {
			return err
		}
	}

	ix, err := openIndex(*data, anySnapshot)
	if err != nil {
		return err
	}
	defer ix.Close() //nolint:errcheck // nothing left to do about it on the way out
	qs, err := loadQueries(*data)
	if err != nil {
		return err
	}
	if len(qs) == 0 {
		return errors.New("no queries with judgments: there is nothing to replay")
	}
	scorers, err := benchScorers(ix, arm)
	if err != nil {
		return err
	}

	if cpuprofile != "" {
		f, err := os.Create(cpuprofile)
		if err != nil {
			return fmt.Errorf("cpuprofile: %w", err)
		}
		defer f.Close() //nolint:errcheck // the profile is written by StopCPUProfile
		if err := pprof.StartCPUProfile(f); err != nil {
			return fmt.Errorf("cpuprofile: %w", err)
		}
		defer pprof.StopCPUProfile()
	}

	// Errors are counted rather than returned. A run is thousands of requests and
	// one failing is a fact about the corpus, not a reason to discard the
	// distribution the others produced — but a run where most of them failed is
	// measuring an error path, so the count is printed beside every rung and a run
	// that is mostly errors says so at the end.
	var failed atomic.Int64
	do := func(i int) {
		q := qs[i%len(qs)].Query
		if _, err := engine.Search(ctx, q, frozenK, fusion.Fuse, scorers...); err != nil {
			failed.Add(1)
		}
	}

	// Cold first, and only once: these are the numbers that exist for exactly one
	// pass, because the second pass finds every page the first one faulted in. A
	// p99 is impossible here — 50 samples — so what is printed is the maximum and
	// the fault counts, which is what section 1's second weak link asked for.
	benchCold(ctx, qs, do)

	// The unloaded median, sequentially, after the cache is warm. It is the
	// denominator of the ladder and the reference the saturation rule reads.
	unloaded := benchUnloaded(ctx, qs, do)
	if unloaded <= 0 {
		return errors.New("the sequential replay measured no time at all")
	}
	log.Printf("unloaded: p50 %v over %d queries, so %.1f q/s sequentially",
		unloaded.Round(time.Microsecond), len(qs), float64(time.Second)/float64(unloaded))

	rates := []float64{rate}
	if rate == 0 {
		base := float64(time.Second) / float64(unloaded)
		rates = rates[:0]
		for _, f := range benchLadder {
			rates = append(rates, base*f)
		}
	}

	n := rotations * len(qs)
	fmt.Printf("\nweft  %s  warm  n=%d/rung  inflight=%d  GOMAXPROCS=%d\n",
		arm, n, inflight, runtime.GOMAXPROCS(0))

	p50s := make([]time.Duration, 0, len(rates))
	reports := make([]benchReport, 0, len(rates))
	for _, r := range rates {
		rep := benchRung(ctx, r, n, inflight, do)
		rep.rate = r
		reports = append(reports, rep)
		p50s = append(p50s, rep.all.p50)
		rep.print()
		if ctx.Err() != nil {
			break
		}
	}

	sat := saturationRate(rates[:len(p50s)], p50s, unloaded)
	head := headlineRate(rates[:len(p50s)], sat)
	if sat == 0 {
		fmt.Printf("\nsaturation: not reached on this ladder; headline is the top rung %.2f/s\n", head)
	} else {
		fmt.Printf("\nsaturation: %.2f/s (first rung past 2x the unloaded p50 of %v); headline is %.2f/s\n",
			sat, unloaded.Round(time.Microsecond), head)
	}
	for _, rep := range reports {
		if rep.rate == head {
			fmt.Printf("HEADLINE   %s  rate=%.2f/s  p99=%s  p99 minus STW=%s  GC CPU %.1f%%\n",
				arm, rep.rate, fmtQ(rep.all.p99, rep.all.p99ok),
				fmtQ(rep.exGC.p99, rep.exGC.p99ok), 100*rep.gcCPUEnd)
		}
	}
	if f := failed.Load(); f > 0 {
		fmt.Printf("\nWARNING: %d requests returned an error and are counted in the distributions above\n", f)
	}
	return ctx.Err()
}

// benchReport is one rung.
type benchReport struct {
	rate       float64
	all, exGC  benchQuantiles
	shed       int
	faults     procFaultCounts
	gcCycles   uint64
	pause      time.Duration
	gcCPUStart float64
	gcCPUEnd   float64
	rss        int64
	elapsed    time.Duration
}

// benchRung applies one arrival rate and collects everything measured around it.
func benchRung(ctx context.Context, rate float64, n, inflight int, do func(int)) benchReport {
	faultsBefore, cyclesBefore := procFaults(), gcCycles()
	pausesBefore, cpuBefore := gcPauseTotal(), gcCPUShare()
	start := time.Now()

	samples, shed := driveOpenLoop(ctx, rate, n, inflight, do)

	elapsed := time.Since(start)
	raw, exGC := splitByGC(samples)
	return benchReport{
		all:        summarize(raw),
		exGC:       summarize(exGC),
		shed:       shed,
		faults:     procFaults().sub(faultsBefore),
		gcCycles:   gcCycles() - cyclesBefore,
		pause:      gcPauseTotal() - pausesBefore,
		gcCPUStart: cpuBefore,
		gcCPUEnd:   gcCPUShare(),
		rss:        maxRSS(),
		elapsed:    elapsed,
	}
}

// benchCold measures the one pass whose page faults are real.
//
// Everything else in this report is warm by construction: 50 queries replayed 200
// times finds the page cache fully populated from the second rotation on, and the
// tail section 1 predicts — 210 MiB of distinct pages per query — disappears into
// it. That disappearance is a result rather than a flaw in the instrument, and it
// is only visible against this.
func benchCold(ctx context.Context, qs []eval.Query, do func(int)) {
	before := procFaults()
	var worst time.Duration
	start := time.Now()
	for i := range qs {
		if ctx.Err() != nil {
			return
		}
		t := time.Now()
		do(i)
		if d := time.Since(t); d > worst {
			worst = d
		}
	}
	d := procFaults().sub(before)
	fmt.Printf("cold  n=%d  total=%v  worst=%v  minflt=%d  majflt=%d\n",
		len(qs), time.Since(start).Round(time.Millisecond), worst.Round(time.Microsecond), d.minor, d.major)
}

// benchUnloaded is the sequential median, which the ladder is scaled from and the
// saturation rule is measured against. Warm: benchCold ran first.
func benchUnloaded(ctx context.Context, qs []eval.Query, do func(int)) time.Duration {
	lats := make([]time.Duration, 0, len(qs))
	for i := range qs {
		if ctx.Err() != nil {
			break
		}
		t := time.Now()
		do(i)
		lats = append(lats, time.Since(t))
	}
	slices.Sort(lats)
	return quantile(lats, 0.5)
}

// gcPauseMetric is the stop-the-world histogram. Summed to a total rather than read
// as quantiles: the buckets are exponential and a quantile off them is an
// interpolation, which is the thing quantile() refuses to print for latencies. The
// total is exact and answers the question the milestone asks — what share of a run's
// wall clock the collector took the world for.
const gcPauseMetric = "/gc/pauses:seconds"

func gcPauseTotal() time.Duration {
	s := []metrics.Sample{{Name: gcPauseMetric}}
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

// gcCPUShare is the fraction of the process's CPU time the collector has taken,
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
// Cumulative over the process, so a run's figure is a difference, and it is a ratio
// of two totals rather than a rate: what a reader wants is "how much of this
// machine went to the collector", which is dimensionless.
func gcCPUShare() float64 {
	s := []metrics.Sample{
		{Name: "/cpu/classes/gc/total:cpu-seconds"},
		{Name: "/cpu/classes/total:cpu-seconds"},
	}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindFloat64 || s[1].Value.Kind() != metrics.KindFloat64 {
		return 0
	}
	total := s[1].Value.Float64()
	if total <= 0 {
		return 0
	}
	return s[0].Value.Float64() / total
}

// fmtQ renders a quantile, or a dash when the sample is too thin to support it.
func fmtQ(d time.Duration, ok bool) string {
	if !ok {
		return "  --  "
	}
	return d.Round(time.Microsecond).String()
}

func (r benchReport) print() {
	fmt.Printf("\nrate=%.2f/s  n=%d  shed=%d  elapsed=%v\n",
		r.rate, r.all.n, r.shed, r.elapsed.Round(time.Millisecond))
	fmt.Printf("  latency   p50 %s  p95 %s  p99 %s  p99.9 %s  max %v\n",
		fmtQ(r.all.p50, r.all.p50ok), fmtQ(r.all.p95, r.all.p95ok),
		fmtQ(r.all.p99, r.all.p99ok), fmtQ(r.all.p999, r.all.p999ok),
		r.all.max.Round(time.Microsecond))
	fmt.Printf("  minus STW p50 %s  p95 %s  p99 %s  p99.9 %s\n",
		fmtQ(r.exGC.p50, r.exGC.p50ok), fmtQ(r.exGC.p95, r.exGC.p95ok),
		fmtQ(r.exGC.p99, r.exGC.p99ok), fmtQ(r.exGC.p999, r.exGC.p999ok))
	fmt.Printf("  gc        cycles %d  STW %v (%.3f%% of elapsed)  GC CPU share %.1f%%\n",
		r.gcCycles, r.pause.Round(time.Microsecond),
		100*float64(r.pause)/float64(max(r.elapsed, 1)), 100*r.gcCPUEnd)
	fmt.Printf("  rusage    minflt %d  majflt %d  nvcsw %d  nivcsw %d  maxrss %.1f MiB\n",
		r.faults.minor, r.faults.major, r.faults.nvcsw, r.faults.nivcsw,
		float64(r.rss)/(1<<20))
}
