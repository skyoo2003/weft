// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/skyoo2003/weft/internal/eval"
	"github.com/skyoo2003/weft/internal/loadgen"
	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/scorer/text"
	"github.com/skyoo2003/weft/pkg/scorer/vector"
)

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

// benchArmOK is the arm check on its own, so benchFlags can apply it before the
// index is mapped. Split from benchScorers rather than duplicated: two spellings of
// the accepted set is how one of them comes to accept something the other does not.
func benchArmOK(arm string) error {
	if arm != benchArmText && arm != benchArmVector {
		return fmt.Errorf("-arm=%q: want %q or %q", arm, benchArmText, benchArmVector)
	}
	return nil
}

func benchScorers(ix *engine.Index, arm string) ([]engine.Scorer, error) {
	if err := benchArmOK(arm); err != nil {
		return nil, err
	}
	ts := text.New(ix)
	if arm == benchArmVector {
		return []engine.Scorer{ts, vector.New(ix)}, nil
	}
	return []engine.Scorer{ts}, nil
}

// benchOpts is what the flags decide, validated.
type benchOpts struct {
	data string
	rate float64
	// rates is an operator-named ladder, from -rates. Non-empty means the sweep is not
	// the rule's own, and benchRates says so to benchSummary.
	rates       []float64
	rotations   int
	inflight    int
	arm         string
	anySnapshot bool
	cpuprofile  string
	writes      bool
	writedocs   int
}

// benchFlags parses and validates, so that every "this value cannot be measured"
// answer is given before the index is mapped rather than after.
func benchFlags(args []string) (benchOpts, error) {
	var o benchOpts
	var ratesRaw string
	data, flagErr := dataFlags(cmdBench, args, func(fs *flag.FlagSet) {
		fs.Float64Var(&o.rate, "rate", 0, "arrival rate in queries/sec; 0 sweeps the ladder")
		fs.StringVar(&ratesRaw, "rates", "", "comma-separated arrival rates to run in order, instead of the sweep -rate 0 derives; the load-point rule is not applied to a ladder you chose")
		fs.IntVar(&o.rotations, "rotations", 200, "passes over the query set per rung (50 queries x 200 = 10,000 samples)")
		fs.IntVar(&o.inflight, "inflight", 0, "cap on concurrent requests; 0 is 4 per core")
		fs.StringVar(&o.arm, "arm", benchArmText, "scorers to load: text or text+vector")
		fs.StringVar(&o.cpuprofile, "cpuprofile", "", "write a CPU profile of the whole run here")
		fs.BoolVar(&o.writes, "writes", false, "instead of the ladder, drop one Commit into a read load and price the write lock (copies the index first)")
		fs.IntVar(&o.writedocs, "writedocs", 1, "documents the -writes commit adds; past ivfMinDocs the commit trains a partition, which is the expensive case")
		snapshotFlag(fs, &o.anySnapshot)
	})
	if flagErr != nil {
		return o, flagErr
	}
	o.data = *data
	if o.rotations <= 0 {
		return o, fmt.Errorf("-rotations=%d: a run needs at least one pass over the query set", o.rotations)
	}
	// Rejected rather than clamped, and rejected here rather than absorbed by the
	// driver. A negative rate makes every due time earlier than the last, so the
	// whole rung is dispatched at once and its latencies — measured from a due time
	// minutes in the past — climb linearly into the thousands of seconds. That
	// prints as a latency distribution with nothing in it saying the schedule was
	// nonsense.
	//
	// Infinity is rejected on the same ground and is not caught by the same test:
	// strconv.ParseFloat accepts "Inf", and Drive's `rate > 0` backstop admits it.
	// The interval is then float64(time.Second)/+Inf, which truncates to zero, so
	// every request in the rung is due at the start — the burst the negative case
	// produces, reached by the opposite arithmetic. Anything at or above ~2e9
	// truncates to a zero interval too, which is why the ceiling is a real rate
	// rather than only the non-finite ones: no machine answers a query in under a
	// nanosecond, so a rate that implies one is a typo.
	const maxRate = 1e9
	if o.rate < 0 || math.IsNaN(o.rate) || o.rate > maxRate {
		return o, fmt.Errorf("-rate=%v: an arrival rate is positive and below %g, or 0 to sweep the ladder", o.rate, maxRate)
	}
	if ratesRaw != "" {
		// Both flags answer "which rates run", so one of them would win silently. Which
		// one is not the point; that the operator cannot tell is.
		if o.rate != 0 {
			return o, fmt.Errorf("-rate=%v and -rates=%q both name the rates to run: pass one", o.rate, ratesRaw)
		}
		for _, f := range strings.Split(ratesRaw, ",") {
			// Each entry gets the bounds a lone -rate gets, and gets them here rather
			// than at dispatch. A list is not a place where a zero or a NaN stops
			// dispatching the whole rung at once, and a four-rung sweep whose third
			// entry was a typo is ninety minutes spent before anything says so.
			r, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
			if err != nil {
				return o, fmt.Errorf("-rates=%q: %q is not a number", ratesRaw, strings.TrimSpace(f))
			}
			if !(r > 0) || math.IsNaN(r) || r > maxRate { //nolint:staticcheck // written this way to reject NaN too
				return o, fmt.Errorf("-rates=%q: %v is not an arrival rate — positive and below %g", ratesRaw, r, maxRate)
			}
			o.rates = append(o.rates, r)
		}
		if len(o.rates) == 0 {
			return o, fmt.Errorf("-rates=%q names no rates", ratesRaw)
		}
	}
	if o.inflight <= 0 {
		o.inflight = loadgen.DefaultInflight()
	}
	if o.writedocs < 1 {
		return o, fmt.Errorf("-writedocs=%d: a commit needs at least one document to be a commit", o.writedocs)
	}
	// Here rather than only in benchScorers, which cannot run until the index is
	// open: a typo in -arm otherwise costs a snapshot hash over the 980 KB qrels and
	// a mapping of the whole evaluation index before it is reported.
	if err := benchArmOK(o.arm); err != nil {
		return o, err
	}
	return o, nil
}

func bench(ctx context.Context, args []string) error {
	o, err := benchFlags(args)
	if err != nil {
		return err
	}
	if !o.anySnapshot {
		if err := verifySnapshot(o.data, queriesFile, qrelsFile); err != nil {
			return err
		}
	}

	ix, err := openIndex(o.data, o.anySnapshot)
	if err != nil {
		return err
	}
	defer ix.Close() //nolint:errcheck // nothing left to do about it on the way out
	qs, err := loadQueries(o.data)
	if err != nil {
		return err
	}
	if len(qs) == 0 {
		return errors.New("no queries with judgments: there is nothing to replay")
	}
	// Checked once here, for both arms. -rotations is validated at flag time but the
	// query count is not known then, and the product is what reaches
	// make([]Sample, 0, n) inside Drive: an int that overflowed is a negative
	// capacity and a panic after the index has already been mapped and replayed.
	n := o.rotations * len(qs)
	if n <= 0 {
		return fmt.Errorf("-rotations=%d over %d queries is %d requests: the product overflowed",
			o.rotations, len(qs), n)
	}
	scorers, err := benchScorers(ix, o.arm)
	if err != nil {
		return err
	}

	stopProfile, err := startCPUProfile(o.cpuprofile)
	if err != nil {
		return err
	}
	defer stopProfile()

	var failed atomic.Int64
	do := benchDo(ctx, qs, scorers, &failed)

	unloaded, err := benchWarmup(ctx, qs, do, &failed)
	if err != nil {
		return err
	}
	if o.writes {
		return benchWrites(ctx, o, qs, n, unloaded)
	}
	rates, ruleLadder := benchRates(o.rate, o.rates, unloaded)

	fmt.Printf("\nweft  %s  warm  n=%d/rung  inflight=%d  GOMAXPROCS=%d\n",
		o.arm, n, o.inflight, runtime.GOMAXPROCS(0))

	p50s := make([]time.Duration, 0, len(rates))
	reports := make([]benchReport, 0, len(rates))
	for _, r := range rates {
		rep := benchRung(ctx, r, n, o.inflight, do)
		rep.rate = r
		reports = append(reports, rep)
		p50s = append(p50s, rep.all.P50)
		rep.print()
		if ctx.Err() != nil {
			break
		}
	}

	// The full `rates`, not rates[:len(p50s)]. Trimming them here is what made an
	// interrupted ladder indistinguishable from a complete shorter one by the time the
	// rule saw it.
	benchSummary(os.Stdout, o.arm, ruleLadder, rates, p50s, reports, unloaded)
	if f := failed.Load(); f > 0 {
		fmt.Printf("\nWARNING: %d requests returned an error and are counted in the distributions above\n", f)
	}
	return ctx.Err()
}

// benchDo is the request the driver sends: one Search, with errors counted rather
// than returned or logged.
//
// The index arrives through `scorers` as a parameter rather than through a shared
// closure, because the two arms measure two different indexes — the ladder the
// published one, -writes its own copy — and a `do` that reached the wrong one would
// have the write-lock arm timing reads that never contend with its commit. One
// constructor rather than one per arm: the second copy had already diverged, logging
// each failure instead of counting it.
//
// Errors are counted rather than returned. A run is thousands of requests and one
// failing is a fact about the corpus, not a reason to discard the distribution the
// others produced — but a run where most of them failed is measuring an error path,
// so the count is reported at the end. Counted rather than logged for a second
// reason: a log write inside the request serialises every goroutine on the log mutex
// and lands in Sample.Lat, so a failing arm would report the cost of its own
// reporting.
//
// A cancelled run is not a failing one: after Ctrl-C every in-flight Search returns
// context.Canceled, and counting those would end a deliberately interrupted ladder
// with a WARNING naming thousands of errors that were the interruption. Same
// condition bench/ applies on the bleve side.
func benchDo(ctx context.Context, qs []eval.Query, scorers []engine.Scorer, failed *atomic.Int64) func(int) {
	return func(i int) {
		q := qs[i%len(qs)].Query
		if _, err := engine.Search(ctx, q, frozenK, fusion.Fuse, scorers...); err != nil && ctx.Err() == nil {
			failed.Add(1)
		}
	}
}

// benchWarmup runs the two preliminaries and returns the number the ladder is
// scaled from.
func benchWarmup(ctx context.Context, qs []eval.Query, do func(int), failed *atomic.Int64) (time.Duration, error) {
	// Cold first, and only once: these are the numbers that exist for exactly one
	// pass, because the second pass finds every page the first one faulted in. A
	// p99 is impossible here — 50 samples — so what is printed is the maximum and
	// the fault counts, which is what section 1's second weak link asked for.
	benchCold(ctx, qs, do)

	// The unloaded median, sequentially, after the cache is warm. It is the
	// denominator of the ladder and the reference the saturation rule reads.
	unloaded := benchUnloaded(ctx, do)
	// Cancellation first. An interrupted warmup leaves a short or empty sample, and
	// reporting that as "the replay measured no time at all" names a condition that
	// did not occur — the ladder path returns ctx.Err() for the same event.
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if unloaded <= 0 {
		return 0, errors.New("the sequential replay measured no time at all")
	}
	// Fatal here, unlike inside the ladder, because these hundred requests are not
	// a distribution the run reports — they are what every rate on the ladder is
	// derived from. A misconfigured arm that errors in microseconds makes `unloaded`
	// the median of an error path, `base` enormous, and every rung below a schedule
	// no server was ever asked to meet. The count printed at the end would say so
	// after ninety minutes of measuring nothing.
	if f := failed.Load(); f > 0 && ctx.Err() == nil {
		return 0, fmt.Errorf("%d of the %d cold and sequential requests returned an error, so the "+
			"ladder would be scaled from an error path rather than from a query",
			f, len(qs)+benchUnloadedSamples)
	}
	log.Printf("unloaded: p50 %v over %d requests, so %.1f q/s sequentially",
		unloaded.Round(time.Microsecond), benchUnloadedSamples, float64(time.Second)/float64(unloaded))
	return unloaded, nil
}

// benchRates is the ladder, and whether it is the rule's own.
//
// Three sources, and only one of them earns a headline. `loadgen.Ladder` scaled by
// this run's sequential throughput is the sweep docs/PERF.md §3 rule 1 was written
// about. A single `-rate` and a named `-rates` list are both the operator's choice,
// and rule 1 exists because choosing the load point by hand is how a performance
// figure is made to say what its author wants — choosing the rungs and letting the
// rule pick among them is the same act at one remove.
//
// So `ruleLadder` travels with the rates rather than being re-derived at the bottom
// of benchSummary, where it would be a second spelling of the same condition.
func benchRates(rate float64, explicit []float64, unloaded time.Duration) (rates []float64, ruleLadder bool) {
	switch {
	case len(explicit) > 0:
		return explicit, false
	case rate != 0:
		return []float64{rate}, false
	}
	base := float64(time.Second) / float64(unloaded)
	rates = make([]float64, 0, len(loadgen.Ladder))
	for _, f := range loadgen.Ladder {
		rates = append(rates, base*f)
	}
	return rates, true
}

// startCPUProfile turns the flag into a stop function, so the caller has one defer
// rather than two whose order decides whether the profile is written at all.
func startCPUProfile(path string) (func(), error) {
	if path == "" {
		return func() {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("cpuprofile: %w", err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		f.Close() //nolint:errcheck // the create failed to be useful; this is cleanup
		return nil, fmt.Errorf("cpuprofile: %w", err)
	}
	return func() {
		// Stop before close: StopCPUProfile is what flushes the samples.
		pprof.StopCPUProfile()
		f.Close() //nolint:errcheck // everything worth reporting was written above
	}, nil
}

// benchSummary applies the load-point rule and prints the one line the milestone
// quotes. Separate from bench so the rule is read in one place rather than at the
// bottom of a function that also parses flags and opens an index.
//
// `rates` is the ladder that was *intended*, not the rungs that were reached: the
// difference between the two is the only evidence a run was cut short, and
// loadgen.RuleApplies is what reads it.
func benchSummary(w io.Writer, arm string, ruleLadder bool, rates []float64, p50s []time.Duration,
	reports []benchReport, unloaded time.Duration,
) {
	rung := func(label string, rep *benchReport) {
		fmt.Fprintf(w, "%s   %s  rate=%.2f/s  p99=%s  p99 minus STW=%s  GC CPU %.1f%%\n",
			label, arm, rep.rate, fmtQ(rep.all.P99, rep.all.P99ok),
			fmtQ(rep.exGC.P99, rep.exGC.P99ok), 100*rep.gcCPU)
	}

	// A suspended rung is checked before the ladder's shape, because it is the stronger
	// failure: a complete five-rung sweep across a sleeping machine satisfies
	// RuleApplies and would otherwise be quoted. Every rung is inspected rather than
	// the headline's alone — the ladder is one process, and a machine that slept
	// during rung one was not the same machine for rung five.
	for i := range reports {
		if reports[i].unaccounted <= loadgen.SuspendTolerance {
			continue
		}
		fmt.Fprintf(w, "\nDISCARD this run: the process did not run for %v of the rung at "+
			"%.2f/s, so the ladder was measured across a suspension. There is no headline. "+
			"Re-run it on a machine that stays awake — `caffeinate -dimsu make bench` — and "+
			"publish the discard.\n",
			reports[i].unaccounted.Round(time.Second), reports[i].rate)
		return
	}

	// The rungs' own figures are printed above by rep.print() and are not in doubt.
	// What is suppressed here is the claim that a rule selected one of them.
	if !ruleLadder || !loadgen.RuleApplies(rates, p50s) {
		if len(reports) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%d of %d rungs measured — a ladder you named, an explicit -rate, or a "+
			"ladder cut short — so the load-point rule has nothing to apply and there is no "+
			"saturation point and no headline; sweep with -rate 0 and let it finish to give the "+
			"rule a ladder\n",
			len(reports), len(rates))
		for i := range reports {
			rung("rung    ", &reports[i])
		}
		return
	}
	sat := loadgen.SaturationRate(rates, p50s, unloaded)
	head := loadgen.HeadlineRate(rates, sat)
	if sat == 0 {
		fmt.Fprintf(w, "\nsaturation: not reached on this ladder; headline is the top rung %.2f/s\n", head)
	} else {
		fmt.Fprintf(w, "\nsaturation: %.2f/s (first rung past 2x the unloaded p50 of %v); headline is %.2f/s\n",
			sat, unloaded.Round(time.Microsecond), head)
	}
	for i := range reports {
		if reports[i].rate == head {
			rung("HEADLINE", &reports[i])
		}
	}
}

// benchReport is one rung.
//
// Every field but rss is a difference taken around the rung. rss is the exception
// and is named for it: ru_maxrss is a high-water mark the kernel never lowers, so
// there is no "during this rung" reading of it — what it says is the peak the
// process has reached by the time this rung ended, cold pass included.
type benchReport struct {
	rate      float64
	all, exGC loadgen.Quantiles
	shed      int
	faults    loadgen.FaultCounts
	gcCycles  uint64
	pause     time.Duration
	gcCPU     float64
	peakRSS   int64
	elapsed   time.Duration

	// unaccounted is wall time this rung cannot account for — the process was not
	// running for it. Past loadgen.SuspendTolerance the rung is not a measurement and
	// the summary refuses to quote it. loadgen.Elapsed says why this is not derivable
	// from elapsed alone.
	unaccounted time.Duration
}

// benchRung applies one arrival rate and collects everything measured around it.
func benchRung(ctx context.Context, rate float64, n, inflight int, do func(int)) benchReport {
	var progress loadgen.Progress
	do = progress.Count(do)

	faultsBefore, cyclesBefore := loadgen.ProcFaults(), loadgen.GCCycles()
	pausesBefore := loadgen.GCPauseTotal()
	// Both CPU totals, not their ratio: the ratio of two running totals cannot be
	// subtracted, and by the fifth rung it is dominated by the index mapping and the
	// four rungs before this one rather than by what the collector is doing now.
	gcCPU0, totalCPU0 := loadgen.GCCPUSeconds()
	start := time.Now()
	stopProgress := progress.Report(os.Stdout, n, loadgen.ProgressEvery)

	samples, shed := loadgen.Drive(ctx, rate, n, inflight, do)

	// Stopped here rather than deferred, and the position is the same argument the
	// snapshot ordering below makes. Deferred, the reporter would still be ticking
	// while the counters are read — charging the rung with its own progress lines —
	// and its next line would land in the middle of the distribution table print()
	// writes. Stop is synchronous, so past this point nothing else is writing.
	stopProgress()

	// Every after-snapshot taken here, before any of them is reduced. Read inside the
	// composite literal below they were evaluated in lexical order — which put both
	// Summarize calls, four fresh 10,000-element slices and ~260k comparisons of
	// sorting, ahead of the fault, cycle and pause reads. Those three then charged the
	// rung with the reporter's own page faults and any collection its allocations
	// provoked, while gcCPU1 — captured first — excluded all of it: one report whose
	// GC CPU share and GC cycle count described different intervals.
	elapsed, unaccounted := loadgen.Elapsed(start)
	gcCPU1, totalCPU1 := loadgen.GCCPUSeconds()
	faultsAfter, cyclesAfter := loadgen.ProcFaults(), loadgen.GCCycles()
	pausesAfter, peakRSS := loadgen.GCPauseTotal(), loadgen.MaxRSS()

	raw, exGC := loadgen.SplitByGC(samples)
	return benchReport{
		all:         loadgen.Summarize(raw),
		exGC:        loadgen.Summarize(exGC),
		shed:        shed,
		faults:      faultsAfter.Sub(faultsBefore),
		gcCycles:    cyclesAfter - cyclesBefore,
		pause:       pausesAfter - pausesBefore,
		gcCPU:       loadgen.GCCPUShareBetween(gcCPU0, totalCPU0, gcCPU1, totalCPU1),
		peakRSS:     peakRSS,
		elapsed:     elapsed,
		unaccounted: unaccounted,
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
	before := loadgen.ProcFaults()
	var worst time.Duration
	var n int
	start := time.Now()
	for i := range qs {
		if ctx.Err() != nil {
			break
		}
		t := time.Now()
		do(i)
		n++
		if d := time.Since(t); d > worst {
			worst = d
		}
	}
	// Printed even when cut short, with the count reached rather than len(qs). The
	// cold pass is the slowest part of a run and so the likeliest place an operator
	// interrupts, and it is the only pass whose major faults are ever real — every
	// later number is warm by construction. Returning silently threw away the run's
	// one piece of storage evidence.
	d := loadgen.ProcFaults().Sub(before)
	fmt.Printf("cold  n=%d  total=%v  worst=%v  minflt=%d  majflt=%d\n",
		n, time.Since(start).Round(time.Millisecond), worst.Round(time.Microsecond), d.Minor, d.Major)
}

// benchUnloadedSamples is how many sequential requests the unloaded median is taken
// over, rotating through the query set.
//
// 200, not one pass over the 50 judged queries, and the number comes from the
// package's own rule rather than from taste: loadgen.Printable wants 100 samples
// beyond a quantile, which puts a p50 at 200 and declares a 50-sample one
// unprintable. This figure is not a footnote that could be left with a caveat — it
// is the denominator of all five arrival rates and the reference SaturationRate
// compares every rung against, so a few milliseconds of run-to-run drift in it
// shifts the whole ladder and can move which rung is called saturated. Four
// rotations of a warm query set costs seconds.
const benchUnloadedSamples = 200

// benchUnloaded is the sequential median, which the ladder is scaled from and the
// saturation rule is measured against. Warm: benchCold ran first.
func benchUnloaded(ctx context.Context, do func(int)) time.Duration {
	lats := make([]time.Duration, 0, benchUnloadedSamples)
	for i := range benchUnloadedSamples {
		if ctx.Err() != nil {
			break
		}
		t := time.Now()
		do(i)
		lats = append(lats, time.Since(t))
	}
	// Summarize rather than a local sort plus Quantile: it is what the bleve side of
	// the comparison calls for the same statistic, and one spelling of the median is
	// one fewer way the two denominators can diverge.
	return loadgen.Summarize(lats).P50
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
		r.rate, r.all.N, r.shed, r.elapsed.Round(time.Millisecond))
	// First, not last, and in words rather than a field. Everything printed below it
	// is arithmetic over samples taken while the process was stopped for part of this
	// span, and a reader who takes the distribution before reaching the caveat has
	// taken the wrong thing.
	if r.unaccounted > loadgen.SuspendTolerance {
		fmt.Printf("  SUSPENDED the process did not run for %v of this rung — the machine slept. "+
			"Nothing below is a measurement.\n", r.unaccounted.Round(time.Second))
	}
	fmt.Printf("  latency   p50 %s  p95 %s  p99 %s  p99.9 %s  max %v\n",
		fmtQ(r.all.P50, r.all.P50ok), fmtQ(r.all.P95, r.all.P95ok),
		fmtQ(r.all.P99, r.all.P99ok), fmtQ(r.all.P999, r.all.P999ok),
		r.all.Max.Round(time.Microsecond))
	fmt.Printf("  minus STW p50 %s  p95 %s  p99 %s  p99.9 %s\n",
		fmtQ(r.exGC.P50, r.exGC.P50ok), fmtQ(r.exGC.P95, r.exGC.P95ok),
		fmtQ(r.exGC.P99, r.exGC.P99ok), fmtQ(r.exGC.P999, r.exGC.P999ok))
	fmt.Printf("  gc        cycles %d  STW %v (%.3f%% of elapsed)  GC CPU share %.1f%%\n",
		r.gcCycles, r.pause.Round(time.Microsecond),
		100*float64(r.pause)/float64(max(r.elapsed, 1)), 100*r.gcCPU)
	// Omitted rather than printed as zeros where getrusage does not exist. Four
	// zeroes and a 0.0 MiB peak read as measurements, and a reader would compare them
	// against the Linux figures in docs/PERF.md; absent is the honest answer, and on
	// unix a rung cannot leave all five at zero.
	if r.faults == (loadgen.FaultCounts{}) && r.peakRSS == 0 {
		return
	}
	fmt.Printf("  rusage    minflt %d  majflt %d  nvcsw %d  nivcsw %d  peakrss %.1f MiB (process)\n",
		r.faults.Minor, r.faults.Major, r.faults.Nvcsw, r.faults.Nivcsw,
		float64(r.peakRSS)/(1<<20))
}

// ---------------------------------------------------------------- writes

// benchWriteCopy is where the -writes arm does its damage.
//
// A Commit rewrites the index it is given, and the index this command opens by
// default is .eval-data/index — the corpus every number in docs/EVAL.md is
// measured against, whose provenance file exists so that nobody publishes a figure
// from a different one. Adding a document to it would leave 171,333 documents, a
// new generation, and every published nDCG quietly measuring a corpus that is not
// the one it names.
//
// So the arm copies first and never touches the original. The copy is the whole
// corpus — 626 MiB on the evaluation index — which is a real cost and the reason
// this is a flag rather than part of the default ladder.
const benchWriteCopy = "bench-write-copy"

// benchWrites runs a read load with one Commit dropped into the middle of it, and
// reports what the reads due during that commit paid.
//
// This is the pass line milestone 3b section 4.3 handed forward. It measured a
// 68-second IVF training inside Commit and observed that Commit holds the write
// lock for all of it, so every Search, Doc, Lookup and Nearest waits — then said
// the ceiling is "the one a load test will find". This is that load test.
//
// The arm is deliberately not part of the ladder. It answers a different question
// with a different shape of answer: not a distribution over rates, but two
// distributions at one rate, split by whether a request was due while the lock was
// held.
// It takes no `do` from the caller. The ladder's closure is bound to the published
// index, and the whole point of this arm is that reads and the commit contend for
// one lock — so it builds its own against the copy. A parameter that looked
// harmless here would have measured two different indexes and reported the
// difference as the lock's cost.
func benchWrites(ctx context.Context, o benchOpts, qs []eval.Query, n int, unloaded time.Duration) error {
	dst, err := benchCopyIndex(o.data)
	if err != nil {
		return err
	}

	wix, err := engine.Open(dst)
	if err != nil {
		return fmt.Errorf("open the copy: %w", err)
	}
	defer wix.Close() //nolint:errcheck // nothing left to do on the way out

	// Reads go to the copy too. Measuring reads against one index while committing
	// to another would be measuring nothing: the lock the arm exists to price is
	// the one those very reads contend for.
	scorers, err := benchScorers(wix, o.arm)
	if err != nil {
		return err
	}
	var failed atomic.Int64
	wdo := benchDo(ctx, qs, scorers, &failed)

	// Half of the ladder's lowest rung, which is a rate the previous run showed
	// weft sustains with nothing shed. The arm is about the lock, not about the
	// knee, so it is measured well below it — but an explicit -rate is honoured
	// rather than silently discarded, which is what this did before: the flag parsed,
	// validated, and then never reached the driver.
	rate := float64(time.Second) / float64(unloaded) * 0.125
	if o.rate != 0 {
		rate = o.rate
	}
	fmt.Printf("\nweft  %s  writes  rate=%.2f/s  n=%d  inflight=%d  commit adds %d document(s)\n",
		o.arm, rate, n, o.inflight, o.writedocs)

	// One rotation over the copy before anything is measured. benchWarmup's cold and
	// warm passes ran against the published index; this is a different mapping of a
	// different 626 MiB of bytes, and its first-touch faults would otherwise land
	// inside the measured distribution. Worse, they would land almost entirely in the
	// `outside` cohort — the commit fires a third of the way in — which is the
	// baseline the commit's cost is compared against, so the bias pointed at making
	// the write lock look cheap.
	for i := range qs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wdo(i)
	}

	// The commit is fired a third of the way in, from its own goroutine, and the
	// window it held is recorded in the same clock the driver stamps Due with.
	runStart := time.Now()
	fireAt := time.Duration(float64(n) / rate / 3 * float64(time.Second))
	probe := make(chan benchCommitWindow, 1)
	go func() { probe <- benchCommitProbe(ctx, wix, dst, o.writedocs, fireAt, runStart) }()

	samples, shed := loadgen.Drive(ctx, rate, n, o.inflight, wdo)

	// Joined, and joined before the window is read. Unsynchronised, an unfinished
	// commit read as `to == 0` and threw the whole run away with an error blaming the
	// commit — while the goroutine ran on past the return into the deferred
	// wix.Close(), which zeroes the index the commit was still writing. A commit that
	// outlasts the read load is the interesting case, not a failure, so this waits for
	// it: Commit is not cancellable, and closing an index under one is not a thing to
	// race even on the way out.
	if ctx.Err() != nil {
		log.Printf("interrupted; waiting for the commit under measurement to finish before closing the copy")
	}
	w := <-probe

	switch {
	case w.err != nil:
		// A failed commit used to publish a window anyway: the error was logged, `to`
		// was stamped regardless, and the report printed the lock cost of a commit
		// that aborted at its first check under the label of one that succeeded — with
		// exit status 0.
		return fmt.Errorf("the commit under measurement failed, so there is no lock cost to attribute: %w", w.err)
	case ctx.Err() != nil:
		return ctx.Err()
	case !w.fired:
		return errors.New("the commit never fired inside the run: nothing to attribute")
	}

	lo, hi := w.from, w.to
	during, outside := loadgen.SplitByWindow(samples, lo, hi)
	dq, oq := loadgen.Summarize(during), loadgen.Summarize(outside)

	fmt.Printf("commit window  [%v, %v)  =  %v\n",
		lo.Round(time.Millisecond), hi.Round(time.Millisecond), (hi - lo).Round(time.Millisecond))
	// Said rather than left to be noticed. Past the end of the send schedule there are
	// no requests left to be due, so the `during` cohort covers only the part of the
	// window the run overlapped and its maximum is a floor on the lock's cost.
	if sched := time.Duration(float64(n) / rate * float64(time.Second)); hi > sched {
		fmt.Printf("  NOTE: the commit outlasted the %v send schedule, so `during` covers only "+
			"the %v of it the run overlapped\n",
			sched.Round(time.Millisecond), (sched - lo).Round(time.Millisecond))
	}
	fmt.Printf("  during    p50 %s  p95 %s  max %v  (n=%d)\n",
		fmtQ(dq.P50, dq.P50ok), fmtQ(dq.P95, dq.P95ok), dq.Max.Round(time.Millisecond), dq.N)
	fmt.Printf("  outside   p50 %s  p95 %s  max %v  (n=%d)\n",
		fmtQ(oq.P50, oq.P50ok), fmtQ(oq.P95, oq.P95ok), oq.Max.Round(time.Millisecond), oq.N)
	fmt.Printf("  shed %d\n", shed)
	// The maxima are what this arm is for. A quantile over the requests due inside
	// a single lock window is an order statistic of however many that was — often
	// too few for even a p50 to print — but the worst one is exactly the number
	// milestone 3b asked for: how long a read can be made to wait.
	fmt.Printf("\nthe worst read due during the commit waited %v; the worst outside it waited %v\n",
		dq.Max.Round(time.Millisecond), oq.Max.Round(time.Millisecond))
	if f := failed.Load(); f > 0 {
		fmt.Printf("\nWARNING: %d requests returned an error and are counted in the distributions above\n", f)
	}
	return nil
}

// benchVecDimScan bounds how far benchVecDim looks for a vector.
const benchVecDimScan = 1000

// benchVecDim is the width the corpus established, discovered rather than assumed:
// a document with a vector of the wrong width is refused by Add (ErrDimMismatch),
// and a text-only corpus wants no vector at all.
//
// engine exposes no VecDim, so it is read off documents instead — but not off Doc(0)
// alone, which is what this did first and got wrong. Add only checks a width when
// there is one (`if len(d.Vector) > 0`, pkg/engine/index.go), so ErrDimMismatch does
// not make every document carry a vector, and the evaluation corpus deliberately
// holds ones that do not: the build logs "%d skipped for width" and leaves those
// text-only. A vector-less document 0 therefore reported width 0, the probe added
// text-only documents, and Commit skipped the IVF training -writedocs exists to
// reach — the arm priced the cheap commit under the label of the expensive one.
//
// ponytail: bounded scan for the first non-empty vector. A corpus whose first 1,000
// documents are all text-only still reads as text-only. The real fix is an
// engine.Index.VecDim accessor — ix.vecDim already holds exactly this — which widens
// a public API guarded by a golden file, so it belongs to a commit that says so.
func benchVecDim(ix *engine.Index) int {
	for id := range min(benchVecDimScan, ix.Len()) {
		if d, ok := ix.Doc(engine.DocID(id)); ok && len(d.Vector) > 0 {
			return len(d.Vector)
		}
	}
	return 0
}

// benchCommitWindow is what the probe reports back: the interval the writer held the
// lock, in the same clock the driver stamps Sample.Due with, and whether the commit it
// was measuring got that far.
//
// A value returned over a channel rather than two atomic.Int64s the caller polls. The
// polling version could not distinguish "still running" from "never fired" from
// "failed", and answered all three with a zero that the report read as a window.
type benchCommitWindow struct {
	from, to time.Duration
	// fired says the probe reached the write path rather than returning on
	// cancellation. Without it a from of 0 is ambiguous with a commit fired at the
	// very start of the run.
	fired bool
	err   error
}

// benchCopyIndex duplicates the index so the write arm never opens the published
// one, and returns where it put it.
//
// Rebuilt every run, not reused: the previous run's probe documents are committed
// into it, so a second run's Add would return ErrDuplicateKey on the first key it
// tried. The copy is left on disk afterwards only so that a failed run can be looked
// at, under a name nobody would mistake for the real index — not as a cache, which an
// earlier version of this comment claimed and the RemoveAll below contradicts.
func benchCopyIndex(data string) (string, error) {
	src := filepath.Join(data, indexDir)
	dst := filepath.Join(data, benchWriteCopy)
	if err := os.RemoveAll(dst); err != nil {
		return "", fmt.Errorf("clear %s: %w", dst, err)
	}
	log.Printf("copying %s to %s so the published index is not written to", src, dst)
	start := time.Now()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		return "", fmt.Errorf("copy index: %w", err)
	}
	log.Printf("copied in %s; left in place, and rebuilt from scratch on the next run",
		time.Since(start).Round(time.Second))
	return dst, nil
}

// benchCommitProbe adds documents and commits once, recording the window it held
// the write lock in the same clock the driver stamps Sample.Due with.
//
// How many documents it adds is the whole variable, and it is a flag because the
// answer is not monotone in an interesting way. A commit writes only what was added
// since the last one (milestone 3a), and a new segment below ivfMinDocs carries no
// partition — so a one-document commit skips the IVF training milestone 3b measured
// at 68 seconds, and prices a completely different event.
//
// The vectors are synthetic and that is stated rather than hidden: the training
// cost is a function of count and width, not of content, and sourcing real vectors
// here would mean re-reading the corpus inside a latency measurement.
// Deterministic, so two runs commit the same bytes.
func benchCommitProbe(ctx context.Context, wix *engine.Index, dst string, docs int,
	fireAt time.Duration, runStart time.Time,
) benchCommitWindow {
	select {
	case <-time.After(fireAt):
	case <-ctx.Done():
		return benchCommitWindow{}
	}
	dim := benchVecDim(wix)

	// The window opens here, before the first Add, and that is a correction rather
	// than a detail. Add takes the same exclusive ix.mu Commit does — it holds it for
	// the map and slice mutation, tokenizing outside — so every one of these `docs`
	// calls blocks every concurrent Search exactly the way the commit does. Stamped
	// after the loop, as it was, those stalls were classified `outside` by
	// SplitByWindow: they inflated the baseline while being absent from the cohort
	// they belong to, and the misattributed span grew with -writedocs, the very
	// parameter the experiment sweeps. The window this arm reports is "the writer held
	// the lock", and Add is the writer holding the lock.
	w := benchCommitWindow{fired: true, from: time.Since(runStart)}
	for i := range docs {
		d := engine.Document{
			Key:  fmt.Sprintf("bench-write-probe-%d", i),
			Text: "a document added to price the write lock",
		}
		if dim > 0 {
			d.Vector = make([]float32, dim)
			for j := range d.Vector {
				// A different direction per document, so k-means has something
				// to partition rather than one repeated point.
				d.Vector[j] = float32((i*7+j*13)%1000) / 1000
			}
		}
		if _, err := wix.Add(d); err != nil {
			w.err = fmt.Errorf("add %d: %w", i, err)
			return w
		}
	}
	t := time.Now()
	if err := wix.Commit(dst); err != nil {
		w.err = fmt.Errorf("commit: %w", err)
		return w
	}
	w.to = time.Since(runStart)
	log.Printf("the writer held the lock for %v, of which the commit itself was %v",
		(w.to - w.from).Round(time.Millisecond), time.Since(t).Round(time.Millisecond))
	return w
}
