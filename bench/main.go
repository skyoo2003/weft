// SPDX-License-Identifier: Apache-2.0

// Command bench measures bleve on weft's corpus with weft's load generator, so
// milestone 5's third assertion — p99(weft) within an order of magnitude of
// p99(bleve) — rests on one instrument rather than two.
//
// Read bench/README.md before the code. It records why this is a separate module,
// what the comparison covers, and the three things it deliberately does not match:
// the hybrid arm, the analyzer, and the storage layout.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"

	"github.com/skyoo2003/weft/internal/eval"
	"github.com/skyoo2003/weft/internal/loadgen"
)

const (
	corpusFile  = "trec-covid/corpus.jsonl"
	queriesFile = "trec-covid/queries.jsonl"
	qrelsFile   = "trec-covid/qrels/test.tsv"

	// frozenK is the rank cut weft is measured at, in docs/EVAL.md and in
	// cmd/weft-eval. A comparison at a different depth would be measuring a
	// different amount of work: top-10 and top-1000 do not cost the same.
	frozenK = 10

	// unloadedSamples is how many sequential requests the unloaded median is taken
	// over, rotating through the query set. Same 200 cmd/weft-eval uses, and for the
	// same reason: loadgen.Printable wants 100 samples beyond a quantile, which puts a
	// p50 at 200, and this figure is the denominator of all five arrival rates.
	unloadedSamples = 200

	// maxRate is the ceiling an arrival rate is rejected above. Past ~2e9 the interval
	// float64(time.Second)/rate truncates to zero and the whole rung dispatches at
	// once; no engine answers a query in under a nanosecond, so a rate implying one is
	// a typo. Same ceiling cmd/weft-eval applies.
	maxRate = 1e9
)

func main() {
	log.SetFlags(log.Ltime)
	var (
		data      string
		indexPath string
		build     bool
		rate      float64
		rotations int
		inflight  int
		batchSize int
	)
	flag.StringVar(&data, "data", "../.eval-data", "directory holding the downloaded corpus")
	flag.StringVar(&indexPath, "index", ".bleve-index", "where the bleve index lives")
	flag.BoolVar(&build, "build", false, "index the corpus and exit")
	flag.Float64Var(&rate, "rate", 0, "arrival rate in queries/sec; 0 sweeps the ladder")
	flag.IntVar(&rotations, "rotations", 200, "passes over the query set per rung")
	flag.IntVar(&inflight, "inflight", 0, "cap on concurrent requests; 0 is 4 per core")
	flag.IntVar(&batchSize, "batch", 1000, "documents per bleve batch while building")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	var err error
	if build {
		err = buildIndex(ctx, filepath.Join(data, corpusFile), indexPath, batchSize)
	} else {
		err = measure(ctx, data, indexPath, rate, rotations, inflight)
	}
	stop()
	if err != nil {
		log.Fatal(err)
	}
}

// indexMapping is one text field and nothing else.
//
// weft's Document carries a Key, a Text, a Vector and Links, and the text arm
// reads only Text — built as title + " " + text, which is what BEIR scores
// trec-covid over. Mirroring that here means a single field with the default
// analyzer. Everything else bleve can do (stored fields, term vectors, facets,
// per-field analyzers) is switched off, because a comparison against an engine
// that cannot do those things should not charge bleve for carrying them.
//
// What cannot be switched off is the analyzer's stemming and stop-word removal —
// see README.md. The default `standard` analyzer does both and weft's Tokenize
// does neither.
func indexMapping() mapping.IndexMapping {
	doc := bleve.NewDocumentMapping()

	body := bleve.NewTextFieldMapping()
	body.Store = false // weft returns ids and the caller fetches; so does this
	body.IncludeTermVectors = false
	body.DocValues = false
	doc.AddFieldMappingsAt("body", body)

	m := bleve.NewIndexMapping()
	m.DefaultMapping = doc
	m.DefaultAnalyzer = "standard"
	return m
}

func buildIndex(ctx context.Context, corpusPath, indexPath string, batchSize int) (err error) {
	if _, err := os.Stat(indexPath); err == nil {
		return fmt.Errorf("%s already exists: remove it to rebuild, so a half-written "+
			"index is never measured under the label of a complete one", indexPath)
	}
	if batchSize < 1 {
		return fmt.Errorf("-batch=%d: a batch holds at least one document", batchSize)
	}

	ix, err := bleve.New(indexPath, indexMapping())
	if err != nil {
		return fmt.Errorf("create index: %w", err)
	}
	// Reported, not dropped. scorch persists on Close, so a Close that fails is
	// exactly the half-written index the os.Stat guard above exists to prevent
	// anyone from measuring — and it would otherwise be the one failure that leaves
	// a directory looking complete.
	//
	// And on any failure the directory goes with it. Batches are persisted as they are
	// flushed, so a build interrupted at document 80,000 leaves a valid, openable
	// index holding half the corpus: the os.Stat guard above then refuses to rebuild
	// it and `measure` opens it happily, which is the exact outcome that guard's own
	// comment says it exists to prevent. Removing it is what makes the guard true.
	defer func() {
		if cerr := ix.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close index: %w", cerr)
		}
		if err != nil {
			if rerr := os.RemoveAll(indexPath); rerr != nil {
				log.Printf("could not remove the partial index at %s: %v", indexPath, rerr)
			}
		}
	}()

	start := time.Now()
	batch := ix.NewBatch()
	var n int
	// Through internal/eval.ReadCorpus rather than a scanner of its own: it is the
	// reader cmd/weft-eval builds weft's index with, it carries the 8 MiB token limit
	// the abstracts need, and it refuses a corpus naming a document twice. The copy
	// this replaces omitted that refusal, so a duplicated id was indexed twice on the
	// bleve side and refused on weft's — two document counts under one ratio.
	err = eval.ReadCorpus(corpusPath, func(d eval.CorpusDoc) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Title and abstract concatenated, exactly as cmd/weft-eval builds
		// engine.Document.Text. A title-only index would understate both sides
		// differently and there would be no way to tell which.
		if err := batch.Index(d.ID, map[string]any{"body": d.Title + " " + d.Text}); err != nil {
			return fmt.Errorf("index %q: %w", d.ID, err)
		}
		n++
		if batch.Size() >= batchSize {
			if err := ix.Batch(batch); err != nil {
				return fmt.Errorf("batch at %d: %w", n, err)
			}
			batch = ix.NewBatch()
		}
		// Outside the flush branch, which is where it used to sit: flushes land on
		// multiples of -batch, so the two conditions only ever aligned when -batch
		// divided 50,000. At -batch=3 a multi-hour build printed nothing at all
		// between its first line and its last, and a slow build was indistinguishable
		// from a hung one.
		if n%50000 == 0 {
			log.Printf("indexed %d in %s", n, time.Since(start).Round(time.Second))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("corpus: %w (run `weft-eval prepare` from the repository root first)", err)
	}
	if batch.Size() > 0 {
		if err := ix.Batch(batch); err != nil {
			return fmt.Errorf("final batch: %w", err)
		}
	}

	count, err := ix.DocCount()
	if err != nil {
		return fmt.Errorf("doc count: %w", err)
	}
	size, _ := dirSize(indexPath)
	log.Printf("bleve: %d documents (%d read) in %s, %.1f MiB on disk",
		count, n, time.Since(start).Round(time.Second), float64(size)/(1<<20))
	return nil
}

// loadQueries pairs queries.jsonl with the qrels and drops the unjudged ones.
//
// Through internal/eval rather than a parser of its own, and that is a correctness
// choice rather than a tidiness one. The hand-rolled version this replaces resolved
// the qrels columns by position — fields[0] as the query id, fields[2] as the score —
// while eval.ReadQrels resolves them by header name and refuses a file whose header
// does not carry query-id, corpus-id and score. A qrels file with its columns in
// another order therefore parsed clean here and errored on weft's side, which is the
// one failure a comparison cannot absorb: two engines replaying two different query
// sets under a single ratio. eval.ReadQueries brings the duplicate-id and empty-file
// refusals with it for the same reason.
//
// Reachable across the module boundary because Go's internal rule is about the import
// path prefix, not the module: this module is github.com/skyoo2003/weft/bench, so it
// sits under the parent of internal/ and the replace directive points at ../.
//
// Judged-only, the same rule cmd/weft-eval applies, for a different reason: nDCG is
// not being computed here, but the comparison has to replay the *same* 50 queries weft
// is measured on, and "the ones with judgments" is what selects them.
func loadQueries(data string) ([]eval.EvalQuery, error) {
	qs, err := eval.ReadQueries(filepath.Join(data, queriesFile))
	if err != nil {
		return nil, err
	}
	qrels, err := eval.ReadQrels(filepath.Join(data, qrelsFile))
	if err != nil {
		return nil, err
	}
	out := make([]eval.EvalQuery, 0, len(qs))
	for _, q := range qs {
		if len(qrels[q.ID]) > 0 {
			out = append(out, q)
		}
	}
	return out, nil
}

func measure(ctx context.Context, data, indexPath string, rate float64, rotations, inflight int) error {
	if rotations <= 0 {
		return fmt.Errorf("-rotations=%d: a run needs at least one pass", rotations)
	}
	// Same rejection weft's bench makes: a negative rate puts every due time before
	// the last, dispatches the whole rung at once, and prints latencies measured
	// from a due time minutes in the past as though they were a distribution.
	// Infinity is rejected on the same ground and is not caught by the same test:
	// ParseFloat accepts "Inf", Drive's `rate > 0` backstop admits it, and
	// float64(time.Second)/+Inf truncates the interval to zero — the same burst the
	// negative case produces, reached by the opposite arithmetic. Anything at or above
	// maxRate truncates to zero too. Same ceiling weft's bench applies.
	if rate < 0 || math.IsNaN(rate) || rate > maxRate {
		return fmt.Errorf("-rate=%v: an arrival rate is positive and below %g, or 0 to sweep the ladder", rate, maxRate)
	}
	if inflight <= 0 {
		inflight = loadgen.DefaultInflight()
	}

	ix, err := bleve.Open(indexPath)
	if err != nil {
		return fmt.Errorf("open %s: %w (run with -build first)", indexPath, err)
	}
	defer ix.Close() //nolint:errcheck // nothing left to do on the way out

	count, err := ix.DocCount()
	if err != nil {
		return err
	}
	qs, err := loadQueries(data)
	if err != nil {
		return err
	}
	if len(qs) == 0 {
		return errors.New("no judged queries: there is nothing to replay")
	}
	log.Printf("bleve: %d documents, %d judged queries", count, len(qs))

	var failed atomic.Int64
	do := func(i int) {
		q := bleve.NewQueryStringQuery(qs[i%len(qs)].Text)
		req := bleve.NewSearchRequestOptions(q, frozenK, 0, false)
		if _, err := ix.SearchInContext(ctx, req); err != nil && ctx.Err() == nil {
			failed.Add(1)
		}
	}

	unloaded, err := warmup(ctx, qs, do, &failed)
	if err != nil {
		return err
	}

	rates := ladder(rate, unloaded)
	n := rotations * len(qs)
	if n <= 0 {
		return fmt.Errorf("-rotations=%d over %d queries is %d requests: the product overflowed",
			rotations, len(qs), n)
	}
	fmt.Printf("\nbleve  text  warm  n=%d/rung  inflight=%d  GOMAXPROCS=%d\n",
		n, inflight, runtime.GOMAXPROCS(0))

	p50s := make([]time.Duration, 0, len(rates))
	rungs := make([]rung, 0, len(rates))
	for _, r := range rates {
		g := measureRung(ctx, r, n, inflight, do)
		rungs = append(rungs, g)
		p50s = append(p50s, g.all.P50)
		g.print()
		if ctx.Err() != nil {
			break
		}
	}

	summarize(rungs, rates[:len(p50s)], p50s, unloaded)
	if f := failed.Load(); f > 0 {
		fmt.Printf("\nWARNING: %d searches returned an error and are counted in the distributions above\n", f)
	}
	return ctx.Err()
}

// ladder is the arrival rates a sweep applies, scaled from the sequential throughput,
// or the single rung an explicit -rate asks for.
//
// The same arithmetic cmd/weft-eval's benchRates does, off the same loadgen.Ladder
// fractions. Milestone 5's third assertion needs both sides driven at rates derived
// the same way, so this is written to be read against that function rather than
// open-coded inside the run — which is where it was, complete with a one-element slice
// truncated to zero and refilled.
func ladder(rate float64, unloaded time.Duration) []float64 {
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

// warmup is the cold pass and the warm sequential median the ladder is scaled from.
//
// Same two preliminaries weft's bench runs, in the same order and over the same
// counts, so the denominators are derived the same way on both sides — including the
// 200-request warm pass, which is what loadgen.Printable's rule asks of a p50 and what
// stops the number all five arrival rates are derived from being an order statistic of
// fifty observations.
func warmup(ctx context.Context, qs []eval.EvalQuery, do func(int), failed *atomic.Int64) (time.Duration, error) {
	coldFaults := loadgen.ProcFaults()
	coldStart := time.Now()
	var coldWorst time.Duration
	var cold int
	for i := range qs {
		if ctx.Err() != nil {
			break
		}
		t := time.Now()
		do(i)
		cold++
		if d := time.Since(t); d > coldWorst {
			coldWorst = d
		}
	}
	// Printed even when cut short, with the count reached: this is the only pass whose
	// major faults are ever real, and the cold pass is the likeliest place an operator
	// interrupts.
	cd := loadgen.ProcFaults().Sub(coldFaults)
	fmt.Printf("cold  n=%d  total=%v  worst=%v  minflt=%d  majflt=%d\n",
		cold, time.Since(coldStart).Round(time.Millisecond),
		coldWorst.Round(time.Microsecond), cd.Minor, cd.Major)

	warm := make([]time.Duration, 0, unloadedSamples)
	for i := range unloadedSamples {
		if ctx.Err() != nil {
			break
		}
		t := time.Now()
		do(i)
		warm = append(warm, time.Since(t))
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	unloaded := loadgen.Summarize(warm).P50
	if unloaded <= 0 {
		return 0, errors.New("the sequential replay measured no time at all")
	}
	// Fatal for the same reason it is on weft's side: these are not a distribution
	// the run reports, they are what every rate on the ladder is derived from, and a
	// median taken off an error path scales the whole ladder to a schedule no engine
	// was ever asked to meet.
	if f := failed.Load(); f > 0 {
		return 0, fmt.Errorf("%d of the %d cold and sequential searches returned an error, so the "+
			"ladder would be scaled from an error path rather than from a query",
			f, len(qs)+unloadedSamples)
	}
	log.Printf("unloaded: p50 %v over %d requests, so %.1f q/s sequentially",
		unloaded.Round(time.Microsecond), unloadedSamples, float64(time.Second)/float64(unloaded))
	return unloaded, nil
}

// measureRung applies one arrival rate and collects everything measured around it.
func measureRung(ctx context.Context, r float64, n, inflight int, do func(int)) rung {
	var progress loadgen.Progress
	do = progress.Count(do)

	fb, cb, pb := loadgen.ProcFaults(), loadgen.GCCycles(), loadgen.GCPauseTotal()
	// Both CPU totals rather than their ratio, for the reason cmd/weft-eval
	// states: a ratio of running totals does not subtract, so the raw figure
	// stops moving after the first rung whatever the collector then does.
	gcCPU0, totalCPU0 := loadgen.GCCPUSeconds()
	start := time.Now()
	stopProgress := progress.Report(os.Stdout, n, loadgen.ProgressEvery)
	samples, shed := loadgen.Drive(ctx, r, n, inflight, do)

	// Stopped before the counters are read, not deferred: a reporter still ticking
	// would charge this rung with its own progress lines, and its next line would land
	// inside the distribution table below. Stop is synchronous, so past here nothing
	// else writes. Same position cmd/weft-eval's benchRung puts it in.
	stopProgress()

	// Every after-snapshot taken before any of them is reduced. Read inside the
	// composite literal they were evaluated in lexical order, which put both Summarize
	// calls — four fresh 10,000-element slices and ~260k comparisons — ahead of the
	// fault, cycle and pause reads, so those three charged the rung with the reporter's
	// own page faults while gcCPU1, captured first, excluded them. Same correction
	// cmd/weft-eval's benchRung carries; an asymmetry here is a factor in the published
	// ratio.
	elapsed := time.Since(start)
	gcCPU1, totalCPU1 := loadgen.GCCPUSeconds()
	fa, ca, pa, rss := loadgen.ProcFaults(), loadgen.GCCycles(), loadgen.GCPauseTotal(), loadgen.MaxRSS()

	raw, exGC := loadgen.SplitByGC(samples)
	return rung{
		rate: r, all: loadgen.Summarize(raw), exGC: loadgen.Summarize(exGC),
		shed:    shed,
		gcCPU:   loadgen.GCCPUShareBetween(gcCPU0, totalCPU0, gcCPU1, totalCPU1),
		pause:   pa - pb,
		elapsed: elapsed, faults: fa.Sub(fb), cycles: ca - cb, peakRSS: rss,
	}
}

// summarize applies the load-point rule, in the same shape cmd/weft-eval's
// benchSummary does.
func summarize(rungs []rung, rates []float64, p50s []time.Duration, unloaded time.Duration) {
	// A one-rung ladder has no load point to find. SaturationRate over a single rate
	// returns that rate whenever its p50 exceeds twice the unloaded median — there is
	// nothing for it to be *first* past — and HeadlineRate returns it either way, so an
	// explicit -rate used to print a hand-chosen load point under the label of a
	// measured one.
	if len(rungs) < 2 {
		if len(rungs) == 0 {
			return
		}
		g := &rungs[0]
		fmt.Printf("\none rung measured — an explicit -rate, or a ladder cut short — so there is no "+
			"saturation point and no headline; sweep with -rate 0 to give the rule something to apply\n"+
			"rung       bleve text  rate=%.2f/s  p99=%s  p99 minus STW=%s  GC CPU %.1f%%\n",
			g.rate, fmtQ(g.all.P99, g.all.P99ok), fmtQ(g.exGC.P99, g.exGC.P99ok), 100*g.gcCPU)
		return
	}
	sat := loadgen.SaturationRate(rates, p50s, unloaded)
	head := loadgen.HeadlineRate(rates, sat)
	if sat == 0 {
		fmt.Printf("\nsaturation: not reached on this ladder; headline is the top rung %.2f/s\n", head)
	} else {
		fmt.Printf("\nsaturation: %.2f/s (first rung past 2x the unloaded p50 of %v); headline is %.2f/s\n",
			sat, unloaded.Round(time.Microsecond), head)
	}
	// By index, not by value: a rung is 192 bytes and copying one per iteration to
	// compare a float is what gocritic's rangeValCopy names. cmd/weft-eval's
	// benchSummary already reads its reports this way.
	for i := range rungs {
		g := &rungs[i]
		if g.rate == head {
			fmt.Printf("HEADLINE   bleve text  rate=%.2f/s  p99=%s  p99 minus STW=%s  GC CPU %.1f%%\n",
				g.rate, fmtQ(g.all.P99, g.all.P99ok), fmtQ(g.exGC.P99, g.exGC.P99ok), 100*g.gcCPU)
		}
	}
}

// rung is one arrival rate's result.
type rung struct {
	rate      float64
	all, exGC loadgen.Quantiles
	shed      int
	gcCPU     float64
	pause     time.Duration
	elapsed   time.Duration
	faults    loadgen.FaultCounts
	cycles    uint64
	// peakRSS is captured when the rung ends rather than read at print time, which is
	// what this did before. ru_maxrss is a high-water mark, so reading it after
	// Summarize allocated and sorted two 10,000-element slices reported a peak that
	// included the reporting machinery cmd/weft-eval's side excludes — and the comment
	// below asserts the two labels mean the same thing.
	peakRSS int64
}

// print matches cmd/weft-eval's layout column for column. Two reports that do not
// line up are two reports a reader has to translate between, and a translation is
// where an order-of-magnitude claim loses a factor of ten.
func (g rung) print() {
	fmt.Printf("\nrate=%.2f/s  n=%d  shed=%d  elapsed=%v\n",
		g.rate, g.all.N, g.shed, g.elapsed.Round(time.Millisecond))
	fmt.Printf("  latency   p50 %s  p95 %s  p99 %s  p99.9 %s  max %v\n",
		fmtQ(g.all.P50, g.all.P50ok), fmtQ(g.all.P95, g.all.P95ok), fmtQ(g.all.P99, g.all.P99ok),
		fmtQ(g.all.P999, g.all.P999ok), g.all.Max.Round(time.Microsecond))
	fmt.Printf("  minus STW p50 %s  p95 %s  p99 %s  p99.9 %s\n",
		fmtQ(g.exGC.P50, g.exGC.P50ok), fmtQ(g.exGC.P95, g.exGC.P95ok),
		fmtQ(g.exGC.P99, g.exGC.P99ok), fmtQ(g.exGC.P999, g.exGC.P999ok))
	fmt.Printf("  gc        cycles %d  STW %v (%.3f%% of elapsed)  GC CPU share %.1f%%\n",
		g.cycles, g.pause.Round(time.Microsecond),
		100*float64(g.pause)/float64(max(g.elapsed, 1)), 100*g.gcCPU)
	// peakrss, not maxrss: ru_maxrss is a high-water mark the kernel never lowers,
	// so this is the peak the process has reached by now — index open and cold pass
	// included — and not a figure for this rung. Same label cmd/weft-eval prints.
	//
	// Omitted entirely where getrusage does not exist, for the reason
	// loadgen/rusage_other.go gives: four zeroes read as measurements next to the
	// Linux figures in docs/PERF.md, and absent is the honest answer.
	if g.faults == (loadgen.FaultCounts{}) && g.peakRSS == 0 {
		return
	}
	fmt.Printf("  rusage    minflt %d  majflt %d  nvcsw %d  nivcsw %d  peakrss %.1f MiB (process)\n",
		g.faults.Minor, g.faults.Major, g.faults.Nvcsw, g.faults.Nivcsw,
		float64(g.peakRSS)/(1<<20))
}

func fmtQ(d time.Duration, ok bool) string {
	if !ok {
		return "  --  "
	}
	return d.Round(time.Microsecond).String()
}

// dirSize is WalkDir rather than Walk: Walk lstats every entry to build the FileInfo
// it hands the callback, and on a multi-gigabyte scorch index that is one syscall per
// file for a number that appears in one log line.
func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
