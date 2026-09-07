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
	memprofile  string
	writes      bool
	writedocs   int
	// preflight turns the run into a gate: the rungs are still measured and printed,
	// and the command exits non-zero unless every one of them passed the line D-024
	// registered. It does not choose the rates — the Makefile names them.
	preflight bool
	// http is the address of a running weftd. Empty is the in-process arm every
	// milestone before 27 measured. Set, the ladder drives the server through a
	// socket and reads *its* counters rather than this process's — see
	// benchhttp.go for why there is no fallback if that read fails.
	http      string
	httpIndex string
}

// benchFlags parses and validates, so that every "this value cannot be measured"
// answer is given before the index is mapped rather than after.
func benchFlags(args []string) (benchOpts, error) {
	var o benchOpts
	var ratesRaw string
	data, flagErr := dataFlags(cmdBench, args, func(fs *flag.FlagSet) {
		fs.Float64Var(&o.rate, "rate", 0, "arrival rate in queries/sec; 0 sweeps the ladder")
		fs.StringVar(&ratesRaw, "rates", "", "comma-separated arrival rates to run in order, instead of "+
			"the sweep -rate 0 derives; the load-point rule is not applied to a ladder you chose")
		// 240 rather than 200, and the twenty percent is not slack. 200 over 50 judged
		// queries is exactly 10,000, and loadgen.Printable wants exactly 10,000 for a
		// p99 — so one shed request at the top rung erased the figure the ladder exists
		// to produce, which is how milestone 7's second and third repetitions died.
		// Registered in docs/PERF.md section 5.9 before the run that uses it, with what
		// it can move: the memory clause reads a peak the extra requests can raise.
		fs.IntVar(&o.rotations, "rotations", 240, "passes over the query set per rung "+
			"(50 queries x 240 = 12,000 samples: the 10,000 a p99 needs, plus 20% headroom for shed)")
		fs.IntVar(&o.inflight, "inflight", 0, "cap on concurrent requests; 0 is 4 per core")
		fs.StringVar(&o.arm, "arm", benchArmText, "scorers to load: text or text+vector")
		fs.StringVar(&o.cpuprofile, "cpuprofile", "", "write a CPU profile of the whole run here")
		fs.StringVar(&o.memprofile, "memprofile", "", "write an allocation profile of the whole run here; "+
			"it says what the rung's KiB/query line cannot, which call sites the bytes came from")
		fs.BoolVar(&o.writes, "writes", false, "instead of the ladder, drop one Commit into a read load and price the write lock (copies the index first)")
		fs.IntVar(&o.writedocs, "writedocs", 1, "documents the -writes commit adds; past ivfMinDocs the commit trains a partition, which is the expensive case")
		fs.BoolVar(&o.preflight, "preflight", false, "exit non-zero unless every rung's p50 is at most "+
			"twice this run's unloaded p50 and nothing was shed; the gate that decides whether the "+
			"ladder is worth its window")
		fs.StringVar(&o.http, "http", "", "drive a running weftd at this address instead of searching in-process; "+
			"memory and collector figures then come from the server's GET /_nodes/stats")
		fs.StringVar(&o.httpIndex, "http-index", "papers", "index name to search when -http is set")
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
	// The write-lock probe copies the index and commits into it from this process.
	// Over HTTP the index belongs to the server, so the probe would be timing a
	// commit nobody's reads contend with — a number that looks like the published
	// 61 ms and measures nothing.
	if o.writes && o.http != "" {
		return o, errors.New("-writes and -http together: the write-lock probe commits into its own copy of " +
			"the index from this process, so over HTTP it would time a commit the measured reads never " +
			"contend with. Run -writes in-process, or drive writes at the server through _bulk")
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

	// The index is opened only on the in-process arm. Over HTTP the server holds
	// it, and mapping it here as well would put 626 MiB of the evaluation corpus
	// into a process that never searches it — and would do so *before* the address
	// is checked, so a typo in -http would cost the mapping before it was reported.
	// -data is still required either way: the query set comes from it.
	var ix *engine.Index
	if o.http == "" {
		ix, err = openIndex(o.data, o.anySnapshot)
		if err != nil {
			return err
		}
		defer ix.Close() //nolint:errcheck // nothing left to do about it on the way out
	}
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
	var scorers []engine.Scorer
	if ix != nil {
		if scorers, err = benchScorers(ix, o.arm); err != nil {
			return err
		}
	}

	stopProfile, err := startCPUProfile(o.cpuprofile)
	if err != nil {
		return err
	}
	defer stopProfile()

	var failed atomic.Int64
	do, meter, err := benchArm(ctx, o, qs, scorers, &failed)
	if err != nil {
		return err
	}

	unloaded, err := benchWarmup(ctx, qs, do, &failed)
	if err != nil {
		return err
	}
	if o.writes {
		// The profile is written on this arm too. A flag that silently does nothing on
		// one of two arms is the same silent failure an unwritable path would be, found
		// after the run instead of before it. Joined rather than short-circuited: a
		// commit probe that failed part way still allocated, so its profile is worth
		// having, and neither error hides the other.
		return errors.Join(benchWrites(ctx, o, qs, n, unloaded), writeAllocProfile(o.memprofile))
	}
	rates, ruleLadder := benchRates(o.rate, o.rates, unloaded)

	// The subject is printed because it decides what every memory column means. A
	// report that does not say which process it measured is one somebody will read
	// as the other one.
	fmt.Printf("\nweft  %s  warm  n=%d/rung  inflight=%d  GOMAXPROCS=%d  measuring=%s\n",
		o.arm, n, o.inflight, runtime.GOMAXPROCS(0), meter.subject())

	reports, p50s, err := benchLadder(ctx, o, rates, n, do, meter)
	if err != nil {
		return err
	}

	// The full `rates`, not rates[:len(p50s)]. Trimming them here is what made an
	// interrupted ladder indistinguishable from a complete shorter one by the time the
	// rule saw it.
	benchSummary(os.Stdout, o.arm, ruleLadder, rates, p50s, reports, unloaded)

	// After the summary, so a failing probe still shows the operator how bad it was.
	if err := preflightVerdict(o.preflight, reports, unloaded); err != nil {
		return err
	}
	if f := failed.Load(); f > 0 {
		fmt.Printf("\nWARNING: %d requests returned an error and are counted in the distributions above\n", f)
	}
	// After the report and regardless of interruption: the profile names call sites and
	// does not need the ladder to have finished. Its error wins over ctx.Err(), which
	// the operator who pressed Ctrl-C already knows about.
	if err := writeAllocProfile(o.memprofile); err != nil {
		return err
	}
	return ctx.Err()
}

// benchLadder runs every rung and prints each report as it lands.
//
// Printed as they land rather than at the end, because a three-hour ladder that is
// interrupted at the fourth rung should still have said what the first three were.
//
// A rung whose counters could not be read ends the run. Not skipped and not
// softened: such a rung has no memory or collector figures, and the only way to
// print one anyway is to substitute this process's — which on the HTTP arm is the
// load generator. Stopping costs the rungs after it; printing would cost the
// meaning of the ones before it.
func benchLadder(ctx context.Context, o benchOpts, rates []float64, n int,
	do func(int), meter counters,
) (reports []benchReport, p50s []time.Duration, err error) {
	p50s = make([]time.Duration, 0, len(rates))
	reports = make([]benchReport, 0, len(rates))
	for _, r := range rates {
		rep := benchRung(ctx, r, n, o.inflight, do, meter)
		if rep.statsErr != nil {
			return nil, nil, fmt.Errorf("reading counters for the %.2f q/s rung from %s: %w",
				r, meter.subject(), rep.statsErr)
		}
		rep.rate = r
		reports = append(reports, rep)
		p50s = append(p50s, rep.all.P50)
		rep.print(os.Stdout)
		if ctx.Err() != nil {
			break
		}
	}
	return reports, p50s, nil
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
// benchArm picks which process is measured: this one, or a weftd over a socket.
//
// Both halves move together and that is why they are chosen in one place. Driving
// the server while reading this process's counters would produce a report whose
// latency column is the server's and whose memory column is the load generator's,
// and nothing in the output would say so.
func benchArm(ctx context.Context, o benchOpts, qs []eval.Query, scorers []engine.Scorer,
	failed *atomic.Int64,
) (func(int), counters, error) {
	if o.http == "" {
		return benchDo(ctx, qs, scorers, failed), localCounters{}, nil
	}
	target, err := newHTTPTarget(ctx, o.http, o.httpIndex, frozenK)
	if err != nil {
		return nil, nil, err
	}
	do, err := target.driver(ctx, qs, failed)
	if err != nil {
		return nil, nil, err
	}
	return do, httpCounters{ctx: ctx, t: target}, nil
}

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
	// One span across both preliminaries below, checked once at the bottom. Until
	// milestone 32 loadgen.Elapsed was called from benchRung and nowhere else, so this
	// stretch — the only one that produces a number every rung is scaled from — was the
	// one the suspension detector could not see.
	start := time.Now()

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
	// Fatal, and for the same reason the error count below is: these requests are not a
	// distribution the run reports — the median they produce is the denominator of every
	// rung's arrival rate and the reference SaturationRate compares each rung against. A
	// machine that slept through the sequential replay scales the whole ladder from a
	// median of a machine that was not running, and nothing downstream can tell.
	//
	// Milestone 14's discarded arm is where that showed: its unloaded p50 was 39.134 ms
	// against a 32.8-35.1 ms band across every other run, the single outlier, and the
	// instrument said nothing because the check ran on rungs only.
	if _, unaccounted := loadgen.Elapsed(start); unaccounted > loadgen.SuspendTolerance {
		return 0, fmt.Errorf("the process did not run for %v of the cold pass and the sequential "+
			"replay, so the unloaded median every rung is scaled from was measured across a "+
			"suspension. Re-run it with the lid open — caffeinate does not prevent clamshell sleep",
			unaccounted.Round(time.Second))
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

// writeAllocProfile writes the `allocs` profile to path, or nothing when path is
// empty. Called on the way out of a run rather than around a rung, and both halves of
// that are deliberate.
//
// `allocs` rather than `heap`: the question this answers is what the rung's
// `KiB/query` line cannot, which call sites those bytes came from, and that is a
// cumulative-since-start question. A live-heap snapshot taken after the last rung has
// already had the collector over it and would answer a different one. Cumulative also
// means a twenty-second run is enough — no ladder is needed to attribute an allocation
// that happens once per query.
//
// What it costs is that the profile carries the index mapping, the cold pass and the
// unloaded pass beside the rungs. Those are separate call sites, so reading it is a
// matter of looking at the right subtree rather than of subtracting; `go tool pprof
// -base` against a run with no rungs is the sharper version if the subtree is ever
// ambiguous.
//
// Written last and reported, not dropped: a 97-minute run whose profile silently did
// not land is the whole run again.
func writeAllocProfile(path string) error {
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("memprofile: %w", err)
	}
	// Lookup rather than pprof.WriteHeapProfile, which writes the `heap` profile and
	// has no parameter for choosing the other one.
	if err := pprof.Lookup("allocs").WriteTo(f, 0); err != nil {
		f.Close() //nolint:errcheck // the write already failed; this is cleanup
		return fmt.Errorf("memprofile: %w", err)
	}
	// Closed here rather than deferred, because a deferred Close would drop the error
	// that says the profile is truncated — which is the one failure this function
	// exists to report.
	if err := f.Close(); err != nil {
		return fmt.Errorf("memprofile: %w", err)
	}
	return nil
}

// outf is every write below: a report line, to a writer that is os.Stdout in a
// run and a buffer in a test.
//
// The dropped error is the point of the function. A failed write to stdout has
// nowhere left to be reported — the report of it would go to the writer that
// just failed, and the next line fails the same way — so the error is dropped
// once, here, with the reason beside it, rather than at eleven call sites
// carrying eleven copies of this paragraph. Same argument as the progress line
// in internal/loadgen. Nothing that writes a file goes through here, which is
// what keeps errcheck's question live where it is worth asking.
//
// vet reads it as a printf wrapper, so the format strings are still checked.
func outf(w io.Writer, format string, a ...any) {
	fmt.Fprintf(w, format, a...) //nolint:errcheck // see above
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
		outf(w, "%s   %s  rate=%.2f/s  p99=%s  p99 minus STW=%s  GC CPU %.1f%%\n",
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
		// The remedy this used to name was `caffeinate -dimsu`, and it was wrong for
		// nine milestones: caffeinate holds PreventUserIdleSystemSleep and has no power
		// over clamshell sleep, which is what discarded both of milestone 14's arms on
		// a machine at 100% on AC. The mitigation the failure mode actually needs is
		// physical.
		outf(w, "\nDISCARD this run: the process did not run for %v of the rung at "+
			"%.2f/s, so the ladder was measured across a suspension. There is no headline. "+
			"Re-run it on a machine that stays awake — the lid open, because caffeinate does "+
			"not prevent clamshell sleep — and publish the discard.\n",
			reports[i].unaccounted.Round(time.Second), reports[i].rate)
		return
	}

	// The rungs' own figures are printed above by rep.print() and are not in doubt.
	// What is suppressed here is the claim that a rule selected one of them.
	if !ruleLadder || !loadgen.RuleApplies(rates, p50s) {
		if len(reports) == 0 {
			return
		}
		outf(w, "\n%d of %d rungs measured — a ladder you named, an explicit -rate, or a "+
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
		outf(w, "\nsaturation: not reached on this ladder; headline is the top rung %.2f/s\n", head)
	} else {
		outf(w, "\nsaturation: %.2f/s (first rung past 2x the unloaded p50 of %v); headline is %.2f/s\n",
			sat, unloaded.Round(time.Microsecond), head)
	}
	for i := range reports {
		if reports[i].rate == head {
			rung("HEADLINE", &reports[i])
		}
	}
}

// preflightVerdict is the gate as the command uses it: a no-op unless -preflight was
// passed, an error when a rung failed, and one printed line when none did.
//
// Split from preflightGate so the rule below stays a pure function of the rungs and the
// denominator — the same reason benchSummary is not written at the bottom of bench.
// Returned as an error rather than printed as a verdict, because main turns that into a
// non-zero exit, and `make bench-preflight && make bench` is the whole point: the rounds
// this replaces were not a failed comparison, they were a comparison nobody made.
func preflightVerdict(on bool, reports []benchReport, unloaded time.Duration) error {
	if !on {
		return nil
	}
	if err := preflightGate(reports, unloaded); err != nil {
		return err
	}
	fmt.Printf("\npreflight PASSED: %d rung(s) within 2x the unloaded p50 of %v, nothing shed\n",
		len(reports), unloaded.Round(time.Microsecond))
	return nil
}

// preflightGate is the pass line Makefile registered and nobody ran.
//
// Twice the unloaded median is loadgen.SaturationRate's constant rather than a number
// picked here — the first rung past twice it is saturation, and 27.28 q/s was not
// saturation on the published ladder. A machine that saturates in a one-minute probe
// cannot reproduce that ladder, and the honest move is to not spend the window. Strictly
// past, so a rung sitting exactly at twice still passes: that is the same boundary
// SaturationRate draws, and drawing it differently here would be a second spelling.
//
// Every rung rather than the first. -preflight is a gate on whatever rungs it was given,
// and a probe that checked one of them would pass a ladder whose second rung collapsed.
//
// A function rather than four lines inside bench, for the reason benchSummary is one: a
// rule checked by no test is a rule right up until the day it is not — and this rule is
// the one four void rounds were spent for want of.
func preflightGate(reports []benchReport, unloaded time.Duration) error {
	// A gate that passes without a measurement is the failure it exists to prevent. An
	// interrupted run reaches here with no rungs, and the loop below would say nothing.
	if len(reports) == 0 {
		return errors.New("preflight measured no rungs, so there is nothing for it to have passed: " +
			"the run was cut short before its first rate reported")
	}
	for i := range reports {
		r := &reports[i]
		if r.all.P50 > 2*unloaded || r.shed > 0 {
			return fmt.Errorf("preflight FAILED at %.2f q/s: p50 %v against an unloaded %v "+
				"(pass is at most 2x) and shed %d (pass is 0). This machine cannot reproduce the "+
				"published ladder right now — do not spend the window on it",
				r.rate, r.all.P50.Round(time.Microsecond), unloaded.Round(time.Microsecond), r.shed)
		}
	}
	return nil
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
	// allocBytes and allocs are what this rung allocated: differences of two
	// cumulative counters, like every field above and unlike rss. Printed per query
	// rather than per rung, because the per-rung total is a function of how long the
	// rung ran and the per-query figure is a property of the work — which is the one
	// a fix, or a claim about a fix, can be held against.
	allocBytes uint64
	allocs     uint64
	peakRSS    int64
	// rssRaised is how much *this* rung moved that mark: peakRSS at the end minus the
	// mark as it stood before the rung began. It is the only per-rung memory statement
	// getrusage can support, and without it the printed peak reads as this rung's when
	// on a ladder it is whichever rung set it — see rssAttribution.
	rssRaised int64
	elapsed   time.Duration

	// statsErr is a counter read that failed. A rung carrying one is not a
	// measurement: on the HTTP arm the numbers would have to come from the load
	// generator instead of the server, and that substitution is the one this
	// design exists to prevent. The run stops rather than printing it.
	statsErr error

	// unaccounted is wall time this rung cannot account for — the process was not
	// running for it. Past loadgen.SuspendTolerance the rung is not a measurement and
	// the summary refuses to quote it. loadgen.Elapsed says why this is not derivable
	// from elapsed alone.
	unaccounted time.Duration

	// dispatch is the send loop's own lateness, and it is the only column here that
	// describes the instrument rather than the engine. Every latency above is measured
	// from a request's due time, so a generator that fell behind its own schedule would
	// charge its lateness to the server and nothing in the report would say so. Four
	// ladders came back void with that question open; this is what closes it from
	// inside a run. loadgen.Drive is where it is sampled and why it includes the shed.
	dispatch loadgen.Quantiles
}

// benchRung applies one arrival rate and collects everything measured around it.
func benchRung(ctx context.Context, rate float64, n, inflight int, do func(int), meter counters) benchReport {
	var progress loadgen.Progress
	do = progress.Count(do)

	// One read of every counter, from whichever process the ladder is measuring.
	// The reads used to be six package-level calls here; they are one call now
	// because over HTTP they have to arrive together — six round trips would put
	// five request latencies between the fault count and the pause total, and the
	// rung would be charged with the difference.
	//
	// It is still outside the measured window on both sides: this is before
	// `start` and its pair is after `elapsed` has been taken. That is the sentence
	// internal/loadgen applies to ReadMemStats — an instrument that stops the world
	// to answer must not be on the per-request path — with the per-rung reads it
	// leaves room for. TotalAlloc, Mallocs, the cycle count and the pause total are
	// all cumulative and monotonic, so a collection landing inside the rung cannot
	// move any of the differences. The resident set is the exception and is a
	// high-water mark rather than a difference; see rssRaised.
	//
	// A failed read is fatal and is not softened into a zero. benchhttp.go's
	// opening comment is the argument: on the HTTP arm the fallback would be this
	// process's counters, which are precise, stable and about the wrong program.
	before, err := meter.snapshot()
	if err != nil {
		return benchReport{statsErr: err}
	}
	start := time.Now()
	stopProgress := progress.Report(os.Stdout, n, loadgen.ProgressEvery)

	samples, shed, late := loadgen.Drive(ctx, rate, n, inflight, do)

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
	after, err := meter.snapshot()
	if err != nil {
		return benchReport{statsErr: err}
	}

	raw, exGC := loadgen.SplitByGC(samples)
	return benchReport{
		all:         loadgen.Summarize(raw),
		exGC:        loadgen.Summarize(exGC),
		shed:        shed,
		faults:      after.faults.Sub(before.faults),
		gcCycles:    after.gcCycles - before.gcCycles,
		pause:       after.pause - before.pause,
		gcCPU:       loadgen.GCCPUShareBetween(before.gcCPU, before.cpu, after.gcCPU, after.cpu),
		allocBytes:  after.totalAlloc - before.totalAlloc,
		allocs:      after.mallocs - before.mallocs,
		peakRSS:     after.maxRSS,
		rssRaised:   after.maxRSS - before.maxRSS,
		elapsed:     elapsed,
		unaccounted: unaccounted,
		// Reduced here with the rest, which is after the second snapshot above and
		// therefore outside the measured window — the same ordering the comment above
		// gives for the two Summarize calls it already made.
		dispatch: loadgen.Summarize(late),
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

func (r benchReport) print(w io.Writer) {
	outf(w, "\nrate=%.2f/s  n=%d  shed=%d  elapsed=%v\n",
		r.rate, r.all.N, r.shed, r.elapsed.Round(time.Millisecond))
	// First, not last, and in words rather than a field. Everything printed below it
	// is arithmetic over samples taken while the process was stopped for part of this
	// span, and a reader who takes the distribution before reaching the caveat has
	// taken the wrong thing.
	if r.unaccounted > loadgen.SuspendTolerance {
		outf(w, "  SUSPENDED the process did not run for %v of this rung — the machine slept. "+
			"Nothing below is a measurement.\n", r.unaccounted.Round(time.Second))
	}
	outf(w, "  latency   p50 %s  p95 %s  p99 %s  p99.9 %s  max %v\n",
		fmtQ(r.all.P50, r.all.P50ok), fmtQ(r.all.P95, r.all.P95ok),
		fmtQ(r.all.P99, r.all.P99ok), fmtQ(r.all.P999, r.all.P999ok),
		r.all.Max.Round(time.Microsecond))
	// Directly under latency, because it is the line that says whether the line above
	// is the server's. A p50 in the hundreds of microseconds is a loop dispatching on
	// schedule; a p50 that grows with the rate is a generator reporting its own
	// lateness as the engine's.
	outf(w, "  dispatch  p50 %s  max %v  (the send loop's own lateness, shed included)\n",
		fmtQ(r.dispatch.P50, r.dispatch.P50ok), r.dispatch.Max.Round(time.Microsecond))
	outf(w, "  minus STW p50 %s  p95 %s  p99 %s  p99.9 %s\n",
		fmtQ(r.exGC.P50, r.exGC.P50ok), fmtQ(r.exGC.P95, r.exGC.P95ok),
		fmtQ(r.exGC.P99, r.exGC.P99ok), fmtQ(r.exGC.P999, r.exGC.P999ok))
	outf(w, "  gc        cycles %d  STW %v (%.3f%% of elapsed)  GC CPU share %.1f%%\n",
		r.gcCycles, r.pause.Round(time.Microsecond),
		100*float64(r.pause)/float64(max(r.elapsed, 1)), 100*r.gcCPU)
	// Omitted rather than divided by zero, and omitted rather than printed as a zero:
	// a rung interrupted before its first sample has a total and no denominator, and a
	// 0.0 KiB/query there reads as a measurement. Same rule as the rusage omission
	// below. Shed requests are not in the denominator — the driver sheds by never
	// dispatching, so they allocated nothing.
	if r.all.N > 0 {
		outf(w, "  alloc     %.1f KiB/query  %d allocs/query  (%.1f MiB this rung)\n",
			float64(r.allocBytes)/float64(r.all.N)/(1<<10), r.allocs/uint64(r.all.N),
			float64(r.allocBytes)/(1<<20))
	}
	// Omitted rather than printed as zeros where getrusage does not exist. Four
	// zeroes and a 0.0 MiB peak read as measurements, and a reader would compare them
	// against the Linux figures in docs/PERF.md; absent is the honest answer, and on
	// unix a rung cannot leave all five at zero.
	if r.faults == (loadgen.FaultCounts{}) && r.peakRSS == 0 {
		return
	}
	outf(w, "  rusage    minflt %d  majflt %d  nvcsw %d  nivcsw %d  peakrss %.1f MiB (process, %s)\n",
		r.faults.Minor, r.faults.Major, r.faults.Nvcsw, r.faults.Nivcsw,
		float64(r.peakRSS)/(1<<20), rssAttribution(r.rssRaised))
}

// rssAttribution says whether the peak beside it belongs to this rung.
//
// "(process)" alone was not enough. A reader holding a rung against a threshold of the
// form "RSS ≤ N at this rate" reads the printed figure as that rung's, and on a ladder
// it is the mark the process has reached by then — [FINDINGS](../../docs/FINDINGS.md)
// milestone 8 §7 is the pass line that ran into it, judging 27.28 q/s against 345.2 MiB
// that had been set two rungs earlier at half the rate.
//
// What a rung *can* say is how much it raised the mark, because that is a difference
// between two readings and every other field here is already one. A rung that raised it
// by nothing puts an upper bound on its own peak and no more, and saying so is the
// whole point: `ru_maxrss` has no during-this-rung reading and nothing here can invent
// one.
func rssAttribution(raised int64) string {
	if raised <= 0 {
		return "unchanged by this rung — the mark is an earlier one's, and this rung's own peak is only bounded by it"
	}
	return fmt.Sprintf("raised %.1f MiB by this rung", float64(raised)/(1<<20))
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

	if err := benchWarmCopy(ctx, qs, wdo); err != nil {
		return err
	}

	// The commit is fired a third of the way in, from its own goroutine, and the
	// window it held is recorded in the same clock the driver stamps Due with.
	runStart := time.Now()
	fireAt := time.Duration(float64(n) / rate / 3 * float64(time.Second))
	probe := make(chan benchCommitWindow, 1)
	go func() { probe <- benchCommitProbe(ctx, wix, dst, o.writedocs, fireAt, runStart) }()

	// Dispatch lateness is dropped here rather than reported. The write arm is not a
	// rung and none of the three clauses that live on the ladder is judged from it;
	// what it prices is one Commit against the reads due during it, and the send
	// loop's own punctuality is not part of that comparison.
	samples, shed, _ := loadgen.Drive(ctx, rate, n, o.inflight, wdo)

	// Joined, and joined before the window is read. Unsynchronised, an unfinished
	// commit read as `to == 0` and threw the whole run away with an error blaming the
	// commit — while the goroutine ran on past the return into the deferred
	// wix.Close(), which zeroes the index the commit was still writing. A commit that
	// outlasts the read load is the interesting case, not a failure, so this waits for
	// it.
	//
	// Since milestone 9 the commit takes the run's context, so a Ctrl-C ends it at its
	// next poll instead of running to completion — the wait is now short rather than as
	// long as the encode. What has not changed is that it has to happen: closing an
	// index under a commit is not a thing to race even on the way out, and a cancelled
	// commit still holds the writer lock until it returns.
	if ctx.Err() != nil {
		log.Printf("interrupted; waiting for the commit under measurement to stop before closing the copy")
	}
	w := <-probe

	switch {
	case ctx.Err() != nil:
		// Before the error check, not after it. A cancelled commit now reports
		// context.Canceled, and blaming the commit for the Ctrl-C that stopped it
		// would name the wrong thing — an abandoned run has nothing to attribute
		// either way. Logged rather than dropped, though: a commit can fail for a
		// reason the Ctrl-C had nothing to do with, and swallowing an ENOSPC because
		// the operator interrupted afterwards leaves nothing to read.
		if w.err != nil {
			log.Printf("the commit under measurement also failed: %v", w.err)
		}
		return ctx.Err()
	case w.err != nil:
		// A failed commit used to publish a window anyway: the error was logged, `to`
		// was stamped regardless, and the report printed the lock cost of a commit
		// that aborted at its first check under the label of one that succeeded — with
		// exit status 0.
		return fmt.Errorf("the commit under measurement failed, so there is no lock cost to attribute: %w", w.err)
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

// benchWarmCopy runs one rotation over the copy before anything is measured.
// benchWarmup's cold and warm passes ran against the published index; this is a
// different mapping of a different 626 MiB of bytes, and its first-touch faults would
// otherwise land inside the measured distribution. Worse, they would land almost
// entirely in the `outside` cohort — the commit fires a third of the way in — which is
// the baseline the commit's cost is compared against, so the bias pointed at making the
// write lock look cheap.
//
// Not benchCold, which rotates over the same queries: that one prints a `cold` line and
// a fault delta, and the writes report has no column for either. What this pass is for
// is the faults being taken here rather than later, not a number to read.
func benchWarmCopy(ctx context.Context, qs []eval.Query, do func(int)) error {
	for i := range qs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		do(i)
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
	// The run's own context. Ctrl-C during the event this arm exists to measure used
	// to mean waiting out the whole encode — 11 seconds of it on the -writedocs 20000
	// procedure — because Commit could not be called off. It can now, and the probe is
	// the caller that has a context to hand it.
	if err := wix.Commit(ctx, dst); err != nil {
		w.err = fmt.Errorf("commit: %w", err)
		return w
	}
	w.to = time.Since(runStart)
	log.Printf("the writer held the lock for %v, of which the commit itself was %v",
		(w.to - w.from).Round(time.Millisecond), time.Since(t).Round(time.Millisecond))
	return w
}
