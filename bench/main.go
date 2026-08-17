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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"

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

// corpusDoc is the BEIR record. Same three fields cmd/weft-eval reads, so the two
// indexes are built from the same bytes.
type corpusDoc struct {
	ID    string `json:"_id"`
	Title string `json:"title"`
	Text  string `json:"text"`
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

	f, err := os.Open(corpusPath)
	if err != nil {
		return fmt.Errorf("corpus: %w (run `weft-eval prepare` from the repository root first)", err)
	}
	defer f.Close() //nolint:errcheck // read-only

	ix, err := bleve.New(indexPath, indexMapping())
	if err != nil {
		return fmt.Errorf("create index: %w", err)
	}
	// Reported, not dropped. scorch persists on Close, so a Close that fails is
	// exactly the half-written index the os.Stat guard above exists to prevent
	// anyone from measuring — and it would otherwise be the one failure that leaves
	// a directory looking complete.
	defer func() {
		if cerr := ix.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close index: %w", cerr)
		}
	}()

	start := time.Now()
	sc := bufio.NewScanner(f)
	// The corpus has abstracts; the default 64 KiB token limit truncates some of
	// them, and a silently short document is a silently different index.
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)

	batch := ix.NewBatch()
	var n int
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var d corpusDoc
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			return fmt.Errorf("corpus line %d: %w", n+1, err)
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
			if n%50000 == 0 {
				log.Printf("indexed %d in %s", n, time.Since(start).Round(time.Second))
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("corpus: %w", err)
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

// evalQuery is a query the qrels actually judge.
type evalQuery struct {
	ID   string
	Text string
}

// loadQueries pairs queries.jsonl with the qrels and drops the unjudged ones.
//
// The same rule cmd/weft-eval applies, for a different reason: nDCG is not being
// computed here, but the comparison has to replay the *same* 50 queries weft is
// measured on, and "the ones with judgments" is what selects them. A run over all
// the queries in the file rather than the judged ones would be comparing different
// work.
func loadQueries(data string) ([]evalQuery, error) {
	judged, err := readQrels(filepath.Join(data, qrelsFile))
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(data, queriesFile))
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only

	var out []evalQuery
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		var q struct {
			ID   string `json:"_id"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(sc.Bytes(), &q); err != nil {
			return nil, err
		}
		if judged[q.ID] {
			out = append(out, evalQuery{ID: q.ID, Text: q.Text})
		}
	}
	return out, sc.Err()
}

func readQrels(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only

	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		fields := strings.Fields(text)
		// The BEIR qrels carry a header row, and it is the only row allowed to be
		// unparseable. Every other malformed row is an error rather than a skip:
		// internal/eval.ReadQrels refuses the same file on weft's side, and a
		// comparison where one engine errors and the other quietly replays a
		// smaller query set is measuring two different amounts of work under one
		// ratio.
		if len(fields) < 3 {
			if line == 1 {
				continue
			}
			return nil, fmt.Errorf("%s line %d: %d columns, need 3", path, line, len(fields))
		}
		if _, err := strconv.Atoi(fields[2]); err != nil {
			if line == 1 {
				continue
			}
			return nil, fmt.Errorf("%s line %d: score %q is not a number", path, line, fields[2])
		}
		out[fields[0]] = true
	}
	return out, sc.Err()
}

func measure(ctx context.Context, data, indexPath string, rate float64, rotations, inflight int) error {
	if rotations <= 0 {
		return fmt.Errorf("-rotations=%d: a run needs at least one pass", rotations)
	}
	// Same rejection weft's bench makes: a negative rate puts every due time before
	// the last, dispatches the whole rung at once, and prints latencies measured
	// from a due time minutes in the past as though they were a distribution.
	if rate < 0 || math.IsNaN(rate) {
		return fmt.Errorf("-rate=%v: an arrival rate is positive, or 0 to sweep the ladder", rate)
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

	// Cold, then the warm sequential median the ladder is scaled from. Same two
	// preliminaries weft's bench runs, in the same order, including the per-query
	// cancellation check, so the denominators are derived the same way on both
	// sides and Ctrl-C ends both at the same granularity.
	coldFaults := loadgen.ProcFaults()
	coldStart := time.Now()
	var coldWorst time.Duration
	for i := range qs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		t := time.Now()
		do(i)
		if d := time.Since(t); d > coldWorst {
			coldWorst = d
		}
	}
	cd := loadgen.ProcFaults().Sub(coldFaults)
	fmt.Printf("cold  n=%d  total=%v  worst=%v  minflt=%d  majflt=%d\n",
		len(qs), time.Since(coldStart).Round(time.Millisecond),
		coldWorst.Round(time.Microsecond), cd.Minor, cd.Major)

	warm := make([]time.Duration, 0, len(qs))
	for i := range qs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		t := time.Now()
		do(i)
		warm = append(warm, time.Since(t))
	}
	unloaded := loadgen.Summarize(warm).P50
	if unloaded <= 0 {
		return errors.New("the sequential replay measured no time at all")
	}
	// Fatal for the same reason it is on weft's side: these are not a distribution
	// the run reports, they are what every rate on the ladder is derived from, and a
	// median taken off an error path scales the whole ladder to a schedule no engine
	// was ever asked to meet.
	if f := failed.Load(); f > 0 {
		return fmt.Errorf("%d of the %d cold and sequential searches returned an error, so the "+
			"ladder would be scaled from an error path rather than from a query", f, 2*len(qs))
	}
	log.Printf("unloaded: p50 %v over %d queries, so %.1f q/s sequentially",
		unloaded.Round(time.Microsecond), len(qs), float64(time.Second)/float64(unloaded))

	rates := []float64{rate}
	if rate == 0 {
		base := float64(time.Second) / float64(unloaded)
		rates = rates[:0]
		for _, f := range loadgen.Ladder {
			rates = append(rates, base*f)
		}
	}

	n := rotations * len(qs)
	fmt.Printf("\nbleve  text  warm  n=%d/rung  inflight=%d  GOMAXPROCS=%d\n",
		n, inflight, runtime.GOMAXPROCS(0))

	p50s := make([]time.Duration, 0, len(rates))
	rungs := make([]rung, 0, len(rates))
	for _, r := range rates {
		fb, cb, pb := loadgen.ProcFaults(), loadgen.GCCycles(), loadgen.GCPauseTotal()
		// Both CPU totals rather than their ratio, for the reason cmd/weft-eval
		// states: a ratio of running totals does not subtract, so the raw figure
		// stops moving after the first rung whatever the collector then does.
		gcCPU0, totalCPU0 := loadgen.GCCPUSeconds()
		start := time.Now()
		samples, shed := loadgen.Drive(ctx, r, n, inflight, do)
		elapsed := time.Since(start)
		gcCPU1, totalCPU1 := loadgen.GCCPUSeconds()
		raw, exGC := loadgen.SplitByGC(samples)
		g := rung{
			rate: r, all: loadgen.Summarize(raw), exGC: loadgen.Summarize(exGC),
			shed:    shed,
			gcCPU:   loadgen.GCCPUShareBetween(gcCPU0, totalCPU0, gcCPU1, totalCPU1),
			pause:   loadgen.GCPauseTotal() - pb,
			elapsed: elapsed, faults: loadgen.ProcFaults().Sub(fb), cycles: loadgen.GCCycles() - cb,
		}
		rungs = append(rungs, g)
		p50s = append(p50s, g.all.P50)
		g.print()
		if ctx.Err() != nil {
			break
		}
	}

	sat := loadgen.SaturationRate(rates[:len(p50s)], p50s, unloaded)
	head := loadgen.HeadlineRate(rates[:len(p50s)], sat)
	if sat == 0 {
		fmt.Printf("\nsaturation: not reached on this ladder; headline is the top rung %.2f/s\n", head)
	} else {
		fmt.Printf("\nsaturation: %.2f/s (first rung past 2x the unloaded p50 of %v); headline is %.2f/s\n",
			sat, unloaded.Round(time.Microsecond), head)
	}
	for _, g := range rungs {
		if g.rate == head {
			fmt.Printf("HEADLINE   bleve text  rate=%.2f/s  p99=%s  p99 minus STW=%s  GC CPU %.1f%%\n",
				g.rate, fmtQ(g.all.P99, g.all.P99ok), fmtQ(g.exGC.P99, g.exGC.P99ok), 100*g.gcCPU)
		}
	}
	if f := failed.Load(); f > 0 {
		fmt.Printf("\nWARNING: %d searches returned an error and are counted in the distributions above\n", f)
	}
	return ctx.Err()
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
	fmt.Printf("  rusage    minflt %d  majflt %d  nvcsw %d  nivcsw %d  peakrss %.1f MiB (process)\n",
		g.faults.Minor, g.faults.Major, g.faults.Nvcsw, g.faults.Nivcsw,
		float64(loadgen.MaxRSS())/(1<<20))
}

func fmtQ(d time.Duration, ok bool) string {
	if !ok {
		return "  --  "
	}
	return d.Round(time.Microsecond).String()
}

func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
