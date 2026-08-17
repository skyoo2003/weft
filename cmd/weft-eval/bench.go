// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
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
		inflight = loadgen.DefaultInflight()
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
		for _, f := range loadgen.Ladder {
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
		p50s = append(p50s, rep.all.P50)
		rep.print()
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
	for _, rep := range reports {
		if rep.rate == head {
			fmt.Printf("HEADLINE   %s  rate=%.2f/s  p99=%s  p99 minus STW=%s  GC CPU %.1f%%\n",
				arm, rep.rate, fmtQ(rep.all.P99, rep.all.P99ok),
				fmtQ(rep.exGC.P99, rep.exGC.P99ok), 100*rep.gcCPUEnd)
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
	all, exGC  loadgen.Quantiles
	shed       int
	faults     loadgen.FaultCounts
	gcCycles   uint64
	pause      time.Duration
	gcCPUStart float64
	gcCPUEnd   float64
	rss        int64
	elapsed    time.Duration
}

// benchRung applies one arrival rate and collects everything measured around it.
func benchRung(ctx context.Context, rate float64, n, inflight int, do func(int)) benchReport {
	faultsBefore, cyclesBefore := loadgen.ProcFaults(), loadgen.GCCycles()
	pausesBefore, cpuBefore := loadgen.GCPauseTotal(), loadgen.GCCPUShare()
	start := time.Now()

	samples, shed := loadgen.Drive(ctx, rate, n, inflight, do)

	elapsed := time.Since(start)
	raw, exGC := loadgen.SplitByGC(samples)
	return benchReport{
		all:        loadgen.Summarize(raw),
		exGC:       loadgen.Summarize(exGC),
		shed:       shed,
		faults:     loadgen.ProcFaults().Sub(faultsBefore),
		gcCycles:   loadgen.GCCycles() - cyclesBefore,
		pause:      loadgen.GCPauseTotal() - pausesBefore,
		gcCPUStart: cpuBefore,
		gcCPUEnd:   loadgen.GCCPUShare(),
		rss:        loadgen.MaxRSS(),
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
		100*float64(r.pause)/float64(max(r.elapsed, 1)), 100*r.gcCPUEnd)
	fmt.Printf("  rusage    minflt %d  majflt %d  nvcsw %d  nivcsw %d  maxrss %.1f MiB\n",
		r.faults.Minor, r.faults.Major, r.faults.Nvcsw, r.faults.Nivcsw,
		float64(r.rss)/(1<<20))
}
