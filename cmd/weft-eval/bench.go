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
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
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

// benchOpts is what the flags decide, validated.
type benchOpts struct {
	data        string
	rate        float64
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
	data, flagErr := dataFlags(cmdBench, args, func(fs *flag.FlagSet) {
		fs.Float64Var(&o.rate, "rate", 0, "arrival rate in queries/sec; 0 sweeps the ladder")
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
	if o.rate < 0 || math.IsNaN(o.rate) {
		return o, fmt.Errorf("-rate=%v: an arrival rate is positive, or 0 to sweep the ladder", o.rate)
	}
	if o.inflight <= 0 {
		o.inflight = loadgen.DefaultInflight()
	}
	if o.writedocs < 1 {
		return o, fmt.Errorf("-writedocs=%d: a commit needs at least one document to be a commit", o.writedocs)
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
	scorers, err := benchScorers(ix, o.arm)
	if err != nil {
		return err
	}

	stopProfile, err := startCPUProfile(o.cpuprofile)
	if err != nil {
		return err
	}
	defer stopProfile()

	// Errors are counted rather than returned. A run is thousands of requests and
	// one failing is a fact about the corpus, not a reason to discard the
	// distribution the others produced — but a run where most of them failed is
	// measuring an error path, so the count is printed beside every rung and a run
	// that is mostly errors says so at the end.
	// A cancelled run is not a failing one: after Ctrl-C every in-flight Search
	// returns context.Canceled, and counting those would end a deliberately
	// interrupted ladder with a WARNING naming thousands of errors that were the
	// interruption. Same condition bench/ applies on the bleve side.
	var failed atomic.Int64
	do := func(i int) {
		q := qs[i%len(qs)].Query
		if _, err := engine.Search(ctx, q, frozenK, fusion.Fuse, scorers...); err != nil && ctx.Err() == nil {
			failed.Add(1)
		}
	}

	unloaded, err := benchWarmup(ctx, qs, do, &failed)
	if err != nil {
		return err
	}
	if o.writes {
		return benchWrites(ctx, o, qs, unloaded)
	}
	rates := benchRates(o.rate, unloaded)

	n := o.rotations * len(qs)
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

	benchSummary(o.arm, rates[:len(p50s)], p50s, reports, unloaded)
	if f := failed.Load(); f > 0 {
		fmt.Printf("\nWARNING: %d requests returned an error and are counted in the distributions above\n", f)
	}
	return ctx.Err()
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
	unloaded := benchUnloaded(ctx, qs, do)
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
			"ladder would be scaled from an error path rather than from a query", f, 2*len(qs))
	}
	log.Printf("unloaded: p50 %v over %d queries, so %.1f q/s sequentially",
		unloaded.Round(time.Microsecond), len(qs), float64(time.Second)/float64(unloaded))
	return unloaded, nil
}

// benchRates is the ladder, scaled from the sequential throughput, or the single
// rung an explicit -rate asks for.
func benchRates(rate float64, unloaded time.Duration) []float64 {
	if rate != 0 {
		return []float64{rate}
	}
	base := float64(time.Second) / float64(unloaded)
	rates := make([]float64, 0, len(loadgen.Ladder))
	for _, f := range loadgen.Ladder {
		rates = append(rates, base*f)
	}
	return rates
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
func benchSummary(arm string, rates []float64, p50s []time.Duration, reports []benchReport, unloaded time.Duration) {
	sat := loadgen.SaturationRate(rates, p50s, unloaded)
	head := loadgen.HeadlineRate(rates, sat)
	if sat == 0 {
		fmt.Printf("\nsaturation: not reached on this ladder; headline is the top rung %.2f/s\n", head)
	} else {
		fmt.Printf("\nsaturation: %.2f/s (first rung past 2x the unloaded p50 of %v); headline is %.2f/s\n",
			sat, unloaded.Round(time.Microsecond), head)
	}
	for i := range reports {
		rep := &reports[i]
		if rep.rate != head {
			continue
		}
		fmt.Printf("HEADLINE   %s  rate=%.2f/s  p99=%s  p99 minus STW=%s  GC CPU %.1f%%\n",
			arm, rep.rate, fmtQ(rep.all.P99, rep.all.P99ok),
			fmtQ(rep.exGC.P99, rep.exGC.P99ok), 100*rep.gcCPU)
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
}

// benchRung applies one arrival rate and collects everything measured around it.
func benchRung(ctx context.Context, rate float64, n, inflight int, do func(int)) benchReport {
	faultsBefore, cyclesBefore := loadgen.ProcFaults(), loadgen.GCCycles()
	pausesBefore := loadgen.GCPauseTotal()
	// Both CPU totals, not their ratio: the ratio of two running totals cannot be
	// subtracted, and by the fifth rung it is dominated by the index mapping and the
	// four rungs before this one rather than by what the collector is doing now.
	gcCPU0, totalCPU0 := loadgen.GCCPUSeconds()
	start := time.Now()

	samples, shed := loadgen.Drive(ctx, rate, n, inflight, do)

	elapsed := time.Since(start)
	gcCPU1, totalCPU1 := loadgen.GCCPUSeconds()
	raw, exGC := loadgen.SplitByGC(samples)
	return benchReport{
		all:      loadgen.Summarize(raw),
		exGC:     loadgen.Summarize(exGC),
		shed:     shed,
		faults:   loadgen.ProcFaults().Sub(faultsBefore),
		gcCycles: loadgen.GCCycles() - cyclesBefore,
		pause:    loadgen.GCPauseTotal() - pausesBefore,
		gcCPU:    loadgen.GCCPUShareBetween(gcCPU0, totalCPU0, gcCPU1, totalCPU1),
		peakRSS:  loadgen.MaxRSS(),
		elapsed:  elapsed,
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
	d := loadgen.ProcFaults().Sub(before)
	fmt.Printf("cold  n=%d  total=%v  worst=%v  minflt=%d  majflt=%d\n",
		len(qs), time.Since(start).Round(time.Millisecond), worst.Round(time.Microsecond), d.Minor, d.Major)
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
	return loadgen.Quantile(lats, 0.5)
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
func benchWrites(ctx context.Context, o benchOpts, qs []eval.Query, unloaded time.Duration) error {
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
	wdo := func(i int) {
		if _, err := engine.Search(ctx, qs[i%len(qs)].Query, frozenK, fusion.Fuse, scorers...); err != nil && ctx.Err() == nil {
			log.Printf("search: %v", err)
		}
	}

	// Half of the ladder's lowest rung, which is a rate the previous run showed
	// weft sustains with nothing shed. The arm is about the lock, not about the
	// knee, so it is measured well below it.
	rate := float64(time.Second) / float64(unloaded) * 0.125
	n := o.rotations * len(qs)
	fmt.Printf("\nweft  %s  writes  rate=%.2f/s  n=%d  inflight=%d  commit adds %d document(s)\n",
		o.arm, rate, n, o.inflight, o.writedocs)

	// The commit is fired a third of the way in, from its own goroutine, and the
	// window it held is recorded in the same clock the driver stamps Due with.
	var from, to atomic.Int64
	runStart := time.Now()
	fireAt := time.Duration(float64(n) / rate / 3 * float64(time.Second))
	go benchCommitProbe(ctx, wix, dst, o.writedocs, fireAt, runStart, &from, &to)

	samples, shed := loadgen.Drive(ctx, rate, n, o.inflight, wdo)
	lo, hi := time.Duration(from.Load()), time.Duration(to.Load())
	if hi == 0 {
		return errors.New("the commit did not finish inside the run: nothing to attribute")
	}
	during, outside := loadgen.SplitByWindow(samples, lo, hi)
	dq, oq := loadgen.Summarize(during), loadgen.Summarize(outside)

	fmt.Printf("commit window  [%v, %v)  =  %v\n",
		lo.Round(time.Millisecond), hi.Round(time.Millisecond), (hi - lo).Round(time.Millisecond))
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
	return ctx.Err()
}

// benchVecDim is the width the corpus established, discovered rather than assumed:
// a document with a vector of the wrong width is refused by Add (ErrDimMismatch),
// and a text-only corpus wants no vector at all.
//
// engine exposes no VecDim, so it is read off a document instead. Doc(0) is the
// first document the index holds and it is as good as any — the width is a corpus
// property, not a per-document one, which is what ErrDimMismatch enforces.
func benchVecDim(ix *engine.Index) int {
	if d, ok := ix.Doc(0); ok {
		return len(d.Vector)
	}
	return 0
}

// benchCopyIndex duplicates the index so the write arm never opens the published
// one, and returns where it put it.
//
// Not removed afterwards. A 626 MiB copy takes long enough that a second run wants
// it, and leaving it under a name nobody would mistake for the real index is what
// keeps that safe.
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
	log.Printf("copied in %s; it is left in place for a rerun", time.Since(start).Round(time.Second))
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
	fireAt time.Duration, runStart time.Time, from, to *atomic.Int64,
) {
	select {
	case <-time.After(fireAt):
	case <-ctx.Done():
		return
	}
	dim := benchVecDim(wix)
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
			log.Printf("add %d: %v", i, err)
			return
		}
	}
	from.Store(int64(time.Since(runStart)))
	t := time.Now()
	if err := wix.Commit(dst); err != nil {
		log.Printf("commit: %v", err)
	}
	to.Store(int64(time.Since(runStart)))
	log.Printf("commit held the write lock for %v", time.Since(t).Round(time.Millisecond))
}
