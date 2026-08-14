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
	"sort"
	"time"

	"github.com/skyoo2003/weft/internal/eval"
	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/scorer/graph"
	"github.com/skyoo2003/weft/pkg/scorer/text"
	"github.com/skyoo2003/weft/pkg/scorer/vector"
)

// The frozen configuration. docs/EVAL.md section 4 fixes these before any number
// exists, so the headline cannot be chosen after seeing results.
const (
	frozenK         = 10
	frozenOverfetch = 1
	bootstrapIters  = 10000
	bootstrapSeed   = 20260814 // Recorded so `make eval` reprints the same interval.

	// Arm names. Five, not the three DATASETS.md section 3 asked for. The two extra
	// ones exist because at partial vector coverage text+vector measured *worse*
	// than text alone, and the binding comparison was briefly moved onto text to
	// keep the bar high. At full coverage that reversed — text+vector is the
	// stronger baseline — so the move was withdrawn and the pre-registered pair
	// binds again (docs/EVAL.md section 4.1).
	//
	// The extra arms stay because that episode is worth being able to re-derive, and
	// because text alone is the cleanest read on what the vector scorer contributes.
	armTextBase      = "text"
	armVecBase       = "text+vector"
	armTextGraph     = "text+graph"
	armVecGraph      = "text+vector+graph"
	armVecGraphSeeds = "text+vector+graph-including-seeds"
)

// comparison is one delta the report has to show, named rather than positional so
// adding an arm cannot silently re-point an existing comparison.
type comparison struct {
	base, arm string
	why       string
}

// Binding first, because the first row is the one a reader takes away. It is the
// pre-registered pair from DATASETS.md section 3 requirement 2 — see the note on the
// arm names for the amendment that briefly replaced it and was withdrawn.
var comparisons = []comparison{
	{armVecBase, armVecGraph, "BINDING: pre-registered, DATASETS section 3 requirement 2"},
	{armTextBase, armTextGraph, "same signal against the text-only baseline, for comparison"},
	{armVecBase, armVecGraphSeeds, "double-counting control (FINDINGS milestone 1 section 2.3)"},
	{armTextBase, armVecBase, "what the vector scorer contributes, reported because it decides the baseline"},
}

// rrf is fusion.Fuse with the rank constant lifted into a parameter.
//
// Deliberately a local reimplementation rather than a change to pkg/fusion:
// fusion.RRFk is a constant and the sweep has to vary it. Because Search takes the
// fuser as an argument, a variant can be injected from out here without pkg/fusion
// growing a knob that exists only for a sweep — milestone 1's dependency direction
// paying a measurable dividend.
//
// The cost, recorded because the sweep's numbers depend on it: this is a copy, and a
// copy can drift from the library it mirrors. It is the reason to prefer a parameter
// on pkg/fusion the moment a second caller wants a rank constant of its own — the
// milestone was already willing to touch that package for FuseWeighted, so the
// argument for the copy is only that no caller outside this sweep has asked.
//
// It mirrors Fuse exactly, including the rank-major accumulation order. Sweeping
// streams first would make a document's float64 total depend on scorer order, so a
// variant that got that wrong would produce sweep numbers differing from the
// frozen run for a reason unrelated to k.
func rrf(rankConst float64) engine.Fuser {
	return func(streams [][]engine.Candidate, k int) []engine.Candidate {
		total, depth := 0, 0
		for _, s := range streams {
			total += len(s)
			depth = max(depth, len(s))
		}
		fused := make(map[engine.DocID]float64, total)
		for i := 0; i < depth; i++ {
			w := 1 / (rankConst + float64(i+1))
			for _, stream := range streams {
				if i < len(stream) {
					fused[stream[i].Doc] += w
				}
			}
		}
		cands := make([]engine.Candidate, 0, len(fused))
		for doc, score := range fused {
			cands = append(cands, engine.Candidate{Doc: doc, Score: score})
		}
		return engine.TopK(cands, k)
	}
}

// loadQueries reads the BEIR queries and qrels and pairs them.
//
// A query with no judgments is dropped rather than scored: nDCG for it is 0 by
// definition (IDCG is 0), so keeping it would drag every arm's mean toward zero by
// the same amount while adding nothing to the comparison — and it would make the
// reported query count disagree with the number of queries that can actually
// distinguish the arms.
func loadQueries(dir string) ([]eval.Query, error) {
	qs, err := eval.ReadQueries(filepath.Join(dir, queriesFile))
	if err != nil {
		return nil, err
	}
	qrels, err := eval.ReadQrels(filepath.Join(dir, qrelsFile))
	if err != nil {
		return nil, err
	}

	// Query vectors are optional, and their absence is reported loudly rather than
	// silently degrading the baseline. Without them the vector scorer has no opinion
	// and the baseline arm is text alone — a weaker baseline, which biases the whole
	// comparison in favour of the graph arm (docs/EVAL.md section 5.5). That is a
	// materially different measurement, so it must not be something a missing file
	// does quietly.
	vecPath := filepath.Join(dir, queryVecFile)
	vecs, err := eval.ReadQueryVectors(vecPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		log.Printf("WARNING: %s is missing, so the vector scorer has no opinion and the", vecPath)
		log.Printf("WARNING: baseline arm is TEXT ONLY. Any graph improvement measured against")
		log.Printf("WARNING: it is an upper bound. Run testdata/gen_query_vectors.py first.")
	}

	var out []eval.Query
	var unjudged, withVec int
	for _, q := range qs {
		rel := qrels[q.ID]
		if len(rel) == 0 {
			unjudged++
			continue
		}
		eq := eval.Query{
			ID:    q.ID,
			Query: engine.Query{Text: q.Text},
			Qrels: rel,
		}
		if v, ok := vecs[q.ID]; ok {
			eq.Query.Vector = v
			withVec++
		}
		out = append(out, eq)
	}
	if unjudged > 0 {
		log.Printf("queries: %d of %d dropped for having no judgments", unjudged, len(qs))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no query had judgments: %w", eval.ErrEmptyDataset)
	}
	log.Printf("queries: %d with judgments, %d with a vector", len(out), withVec)
	// A partial set is the dangerous case: some queries get two signals and others
	// one, so the arms are not measured under the same conditions and the mean mixes
	// two populations.
	if withVec > 0 && withVec != len(out) {
		return nil, fmt.Errorf("%d of %d queries have a vector; all or none, "+
			"otherwise the arms are not comparable across queries", withVec, len(out))
	}
	return out, nil
}

// armsFor builds the arms. Note there is no branch on which scorer is which: the
// arms differ only in the contents of a slice, which is the milestone 1 claim
// holding one level up from the engine.
func armsFor(ix *engine.Index, fuse engine.Fuser, overfetch int) map[string]eval.Arm {
	ts := text.New(ix)
	vs := vector.New(ix)
	sets := map[string][]engine.Scorer{
		armTextBase:      {ts},
		armVecBase:       {ts, vs},
		armTextGraph:     {ts, graph.New(ix, ts)},
		armVecGraph:      {ts, vs, graph.New(ix, ts)},
		armVecGraphSeeds: {ts, vs, graph.NewIncludingSeeds(ix, ts)},
	}
	out := make(map[string]eval.Arm, len(sets))
	for name, scorers := range sets {
		out[name] = eval.Arm{Name: name, Scorers: scorers, Fuse: fuse, Overfetch: overfetch}
	}
	return out
}

// armOrder is the reporting order. A map has none, and a table whose rows move
// between runs is a table nobody can diff.
var armOrder = []string{armTextBase, armVecBase, armTextGraph, armVecGraph, armVecGraphSeeds}

func openIndex(dir string) (*engine.Index, error) {
	start := time.Now()
	ix, err := engine.Open(filepath.Join(dir, indexDir))
	if err != nil {
		return nil, fmt.Errorf("open index: %w (run `weft-eval build` first)", err)
	}
	docs, avgdl := ix.Stats()
	log.Printf("index: %d documents, avgdl %.1f, opened in %s",
		docs, avgdl, time.Since(start).Round(time.Millisecond))
	return ix, nil
}

// ---------------------------------------------------------------- run

func run(ctx context.Context, args []string) error {
	var k, overfetch, iters int
	var rankConst float64
	data := dataFlags("run", args, func(fs *flag.FlagSet) {
		fs.IntVar(&k, "k", frozenK, "rank cut")
		fs.IntVar(&overfetch, "overfetch", frozenOverfetch, "ask each scorer for k*this")
		fs.IntVar(&iters, "iters", bootstrapIters, "bootstrap resamples")
		fs.Float64Var(&rankConst, "rrfk", fusion.RRFk, "RRF rank constant")
	})

	ix, err := openIndex(*data)
	if err != nil {
		return err
	}
	qs, err := loadQueries(*data)
	if err != nil {
		return err
	}

	fuse := engine.Fuser(fusion.Fuse)
	if rankConst != fusion.RRFk {
		fuse = rrf(rankConst)
	}

	arms := armsFor(ix, fuse, overfetch)
	runs, err := evaluateArms(ctx, ix, qs, arms, armOrder, k)
	if err != nil {
		return err
	}

	fmt.Printf("\nnDCG@%d over %d queries — RRFk=%v overfetch=%d\n\n", k, len(qs), rankConst, overfetch)
	fmt.Printf("  %-36s %8s\n", "arm", "nDCG")
	for _, name := range armOrder {
		fmt.Printf("  %-36s %8.4f\n", name, runs[name].NDCG)
	}

	fmt.Printf("\npaired bootstrap, %d resamples, seed %d\n\n", iters, bootstrapSeed)
	for _, c := range comparisons {
		iv, err := eval.BootstrapCI(runs[c.base].PerQuery, runs[c.arm].PerQuery, iters, bootstrapSeed)
		if err != nil {
			return err
		}
		verdict := "improves"
		switch {
		case iv.ContainsZero():
			verdict = "UNDETERMINED (CI contains zero)"
		case iv.Hi < 0:
			verdict = "REGRESSES"
		}
		fmt.Printf("  %s\n    %-42s delta %+8.4f  95%% CI [%+.4f, %+.4f]  %s\n",
			c.why, c.arm+" - "+c.base, iv.Delta, iv.Lo, iv.Hi, verdict)
	}

	// The queries that moved most, for the binding comparison above. A mean that shifted
	// because one query swung is a different finding from thirty queries each gaining
	// a little, and only this tells them apart.
	fmt.Printf("\nlargest per-query moves, %s vs %s\n\n", armVecGraph, armVecBase)
	for _, m := range topMoves(runs[armVecBase].PerQuery, runs[armVecGraph].PerQuery, 5) {
		fmt.Printf("  query %-6s %.4f -> %.4f  (%+.4f)\n", m.id, m.from, m.to, m.to-m.from)
	}
	fmt.Println()
	return nil
}

func evaluateArms(ctx context.Context, ix *engine.Index, qs []eval.Query, arms map[string]eval.Arm, order []string, k int) (map[string]eval.Run, error) {
	out := make(map[string]eval.Run, len(order))
	for _, name := range order {
		a, ok := arms[name]
		if !ok {
			return nil, fmt.Errorf("arm %q is not defined", name)
		}
		start := time.Now()
		r, err := eval.Evaluate(ctx, ix, qs, a, k)
		if err != nil {
			return nil, err
		}
		log.Printf("arm %-36s nDCG@%d %.4f  (%s)", name, k, r.NDCG, time.Since(start).Round(time.Millisecond))
		out[name] = r
	}
	return out, nil
}

type move struct {
	id       string
	from, to float64
}

func topMoves(base, arm map[string]float64, n int) []move {
	moves := make([]move, 0, len(base))
	for id, b := range base {
		moves = append(moves, move{id, b, arm[id]})
	}
	sort.Slice(moves, func(i, j int) bool {
		di, dj := math.Abs(moves[i].to-moves[i].from), math.Abs(moves[j].to-moves[j].from)
		if di != dj {
			return di > dj
		}
		return moves[i].id < moves[j].id // Deterministic on ties.
	})
	return moves[:min(n, len(moves))]
}

// ---------------------------------------------------------------- sweep

func sweep(ctx context.Context, args []string) error {
	var k, iters int
	data := dataFlags("sweep", args, func(fs *flag.FlagSet) {
		fs.IntVar(&k, "k", frozenK, "rank cut")
		fs.IntVar(&iters, "iters", 2000,
			"bootstrap resamples per configuration (lower than the headline: this is a sign check, not a published interval)")
	})

	ix, err := openIndex(*data)
	if err != nil {
		return err
	}
	qs, err := loadQueries(*data)
	if err != nil {
		return err
	}

	// The point is not to find the best configuration. It is to find out whether
	// the sign of (graph - baseline) survives changing constants nobody tuned.
	rankConsts := []float64{1, 10, 20, 40, fusion.RRFk, 100, 200}
	overfetches := []int{1, 2, 5, 10}

	// Swept against the text-only baseline, not the binding pre-registered pair.
	//
	// Two reasons, and the second is the honest one. The clean reason: a single-stream
	// baseline is provably invariant across this grid, which is what lets it be
	// measured once below instead of 28 times. The honest reason: this was the binding
	// comparison when the sweep was written, the amendment that made it binding was
	// later withdrawn (see the arm names above), and re-running the grid against
	// text+vector would cost an hour to re-derive a sign that is already negative in
	// both framings — −0.1809 here, −0.1156 there.
	//
	// So this establishes that the graph delta's sign is stable under the fusion
	// constants nobody tuned. It does not establish that for the exact pair section 4
	// rule 2 names. docs/EVAL.md section 5.10 states the same limitation.
	fmt.Printf("\nsign stability of %s - %s at nDCG@%d\n\n", armTextGraph, armTextBase, k)
	fmt.Printf("  %8s %10s %10s %10s %22s %s\n", "RRFk", "overfetch", "base", "graph", "delta 95% CI", "sign")

	// The baseline is measured once, not 28 times. armTextBase is a single stream, so
	// fusion assigns it scores strictly decreasing in rank whatever the rank constant
	// is, and TopK hands the same order back; over-fetching only appends candidates
	// past the cut. Its nDCG@k is therefore identical in every cell of this table —
	// re-deriving it 27 more times doubles the slowest job in the milestone to
	// reprint a constant.
	baseRun, err := eval.Evaluate(ctx, ix, qs, armsFor(ix, fusion.Fuse, 1)[armTextBase], k)
	if err != nil {
		return err
	}

	flips, total, firstSign := 0, 0, 0
	for _, rc := range rankConsts {
		for _, of := range overfetches {
			fuse := engine.Fuser(fusion.Fuse)
			if rc != fusion.RRFk {
				fuse = rrf(rc)
			}
			arms := armsFor(ix, fuse, of)
			graphRun, err := eval.Evaluate(ctx, ix, qs, arms[armTextGraph], k)
			if err != nil {
				return err
			}
			iv, err := eval.BootstrapCI(baseRun.PerQuery, graphRun.PerQuery, iters, bootstrapSeed)
			if err != nil {
				return err
			}

			sign := 0
			switch {
			case iv.ContainsZero():
				sign = 0
			case iv.Delta > 0:
				sign = 1
			default:
				sign = -1
			}
			label := map[int]string{1: "+", 0: "0 (CI spans zero)", -1: "-"}[sign]
			total++
			switch {
			case firstSign == 0 && sign != 0:
				firstSign = sign
			case sign != 0 && firstSign != 0 && sign != firstSign:
				flips++
				label += "  FLIP"
			}
			fmt.Printf("  %8v %10d %10.4f %10.4f   [%+.4f, %+.4f] %s\n",
				rc, of, baseRun.NDCG, graphRun.NDCG, iv.Lo, iv.Hi, label)
		}
	}

	fmt.Printf("\n%d configurations, %d sign flips\n", total, flips)
	switch {
	case flips > 0:
		fmt.Println("VERDICT INPUT: the sign is not stable. docs/EVAL.md section 4 rule 2 fails.")
	case firstSign == 0:
		// No configuration established a sign at all. Reporting that as stability
		// would read "rule 2 holds" off a table that established nothing — the same
		// mistake as reading a wide interval as agreement.
		fmt.Println("VERDICT INPUT: every interval spans zero, so no configuration established a")
		fmt.Println("sign. That is not stability; docs/EVAL.md section 4 rule 2 is undetermined.")
	default:
		fmt.Println("VERDICT INPUT: no sign flip across the sweep. docs/EVAL.md section 4 rule 2 holds.")
	}
	fmt.Println()
	return nil
}

// ---------------------------------------------------------------- weights

// weights answers the question milestone 4 created rather than settled: unweighted
// RRF gives a near-noise stream the same vote as BM25's, and the measured cost of
// that was −0.1156 nDCG@10. Does discounting the graph stream recover it?
//
// Only the graph stream's weight moves. text and vector stay at 1.0, so the sweep
// isolates one variable, and w=0 is not tested because it is the baseline arm by
// definition — as w approaches 0 the graph stream can only break ties among
// documents the other two already rank equally, so `text+vector+graph` must converge
// to `text+vector`. That convergence is itself a check on the implementation: if the
// smallest weight does not land on the baseline, the weighting is wrong.
//
// Read the outcome carefully. A weight at which the graph arm merely stops losing is
// not evidence the graph signal has value — it is evidence that ignoring it is
// cheap. Only a positive delta with an interval excluding zero would reopen the
// verdict in docs/EVAL.md section 6.
// The rank constant is not a parameter here. sweep already showed it does not move
// the graph delta's sign anywhere between 1 and 200, so this measures the one knob
// that does — and it measures it through fusion.FuseWeighted rather than a local
// copy, so the library API is what the published number came from.
func weights(ctx context.Context, args []string) error {
	var k, iters int
	data := dataFlags("weights", args, func(fs *flag.FlagSet) {
		fs.IntVar(&k, "k", frozenK, "rank cut")
		fs.IntVar(&iters, "iters", bootstrapIters, "bootstrap resamples")
	})

	ix, err := openIndex(*data)
	if err != nil {
		return err
	}
	qs, err := loadQueries(*data)
	if err != nil {
		return err
	}

	// Baseline: text+vector, unweighted, exactly as section 5.8 reports it.
	base, err := eval.Evaluate(ctx, ix, qs, armsFor(ix, fusion.Fuse, frozenOverfetch)[armVecBase], k)
	if err != nil {
		return err
	}
	log.Printf("baseline %s nDCG@%d %.4f", armVecBase, k, base.NDCG)

	fmt.Printf("\ngraph stream weight sweep, %s against %s at nDCG@%d (RRFk=%v)\n",
		armVecGraph, armVecBase, k, fusion.RRFk)
	fmt.Printf("text and vector stay at weight 1.0; only the graph stream moves.\n\n")
	fmt.Printf("  %10s %10s %10s %24s %s\n", "graph w", "nDCG", "delta", "95% CI", "reading")

	best, bestW := base.NDCG, 0.0
	for _, w := range []float64{1, 0.5, 0.25, 0.1, 0.05, 0.02, 0.01, 0.001} {
		// Scorer order inside armsFor is text, vector, graph — the same order
		// FuseWeighted indexes. Positional, not by kind, which is what lets the
		// library expose this without learning what a graph scorer is.
		arm := armsFor(ix, fusion.FuseWeighted(1, 1, w), frozenOverfetch)[armVecGraph]
		run, err := eval.Evaluate(ctx, ix, qs, arm, k)
		if err != nil {
			return err
		}
		iv, err := eval.BootstrapCI(base.PerQuery, run.PerQuery, iters, bootstrapSeed)
		if err != nil {
			return err
		}
		reading := "improves"
		switch {
		case iv.ContainsZero():
			reading = "undetermined"
		case iv.Hi < 0:
			reading = "regresses"
		}
		if run.NDCG > best {
			best, bestW = run.NDCG, w
		}
		fmt.Printf("  %10v %10.4f %+10.4f   [%+.4f, %+.4f] %s\n",
			w, run.NDCG, iv.Delta, iv.Lo, iv.Hi, reading)
	}

	fmt.Printf("\nbaseline %s = %.4f\n", armVecBase, base.NDCG)
	if bestW == 0 {
		fmt.Println("VERDICT INPUT: no weight beat the baseline. Down-weighting limits the damage;")
		fmt.Println("it does not turn the graph stream into a contribution. Section 6 stands.")
	} else {
		fmt.Printf("VERDICT INPUT: weight %v scored %.4f, above the baseline. Check whether its\n", bestW, best)
		fmt.Println("interval excludes zero before reading this as the graph signal having value.")
	}
	fmt.Println()
	return nil
}

// ---------------------------------------------------------------- diagnose

// diagnose measures whether the graph stream's top k is decided by proximity or by
// TopK's DocID tiebreak — which is corpus insertion order, and therefore nothing.
//
// The metric is the size of the tie group straddling the cut: how many candidates
// share the score of the k-th one, and how many of those fall outside the top k. If
// any do, membership of the returned stream was settled by DocID rather than by the
// scorer, and an arm built on it is partly measuring the order documents were added.
//
// Stated this way on purpose, rather than as a histogram of hop distances. The
// earlier version inverted score = 1/(1+hops) to recover each candidate's hop, which
// stopped being possible once the score became a sum over seeds. Counting ties at the
// cut asks the question that actually matters and works for any scoring formula, so
// it stays valid across the change it was written to evaluate. For the old formula the
// two agree: more than k hop-1 candidates meant the whole top k sat at 0.5 with the
// tie group running past the cut.
//
// Everything is read through the public Scorer interface, so no traversal logic is
// duplicated here.
func diagnose(ctx context.Context, args []string) error {
	var deep, k int
	data := dataFlags("diagnose", args, func(fs *flag.FlagSet) {
		fs.IntVar(&deep, "deep", 1_000_000, "k to request, large enough not to truncate the frontier")
		fs.IntVar(&k, "k", frozenK, "the cut whose tie group is measured")
	})

	ix, err := openIndex(*data)
	if err != nil {
		return err
	}
	qs, err := loadQueries(*data)
	if err != nil {
		return err
	}

	gs := graph.New(ix, text.New(ix))

	fmt.Printf("\ngraph stream tie analysis at k=%d, seeds from the text scorer (SeedN=%d, MaxDepth=%d)\n\n",
		k, graph.SeedN, graph.MaxDepth)
	fmt.Printf("  %-6s %10s %10s %12s %12s\n", "query", "cands", "distinct", "tied-at-cut", "arbitrary")

	var degenerate, noOpinion, arbitraryTotal int
	for _, q := range qs {
		cands, err := gs.Candidates(ctx, q.Query, deep)
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			noOpinion++
			fmt.Printf("  %-6s %10d %10s %12s %12s\n", q.ID, 0, "-", "-", "-")
			continue
		}

		distinct := make(map[float64]struct{}, len(cands))
		for _, c := range cands {
			distinct[c.Score] = struct{}{}
		}

		// TopK returned these best-first, so the cut score sits at index k-1, or at
		// the last one when the stream is shorter than k.
		cut := cands[min(k, len(cands))-1].Score
		tied := 0
		for _, c := range cands {
			if c.Score == cut {
				tied++
			}
		}
		// Candidates at the cut score that did not make the top k. Every one of them
		// lost its slot to a lower DocID rather than to a lower score.
		arbitrary := 0
		if len(cands) > k {
			for _, c := range cands[k:] {
				if c.Score == cut {
					arbitrary++
				}
			}
		}
		if arbitrary > 0 {
			degenerate++
			arbitraryTotal += arbitrary
		}
		fmt.Printf("  %-6s %10d %10d %12d %12d\n", q.ID, len(cands), len(distinct), tied, arbitrary)
	}

	answered := len(qs) - noOpinion
	fmt.Printf("\n%d of %d queries produced a graph stream at all", answered, len(qs))
	if noOpinion > 0 {
		fmt.Printf(" (%d had no reachable neighbour)", noOpinion)
	}
	fmt.Printf(".\n%d of those %d have a tie group crossing the cut, %d slots decided by DocID in total.\n",
		degenerate, answered, arbitraryTotal)
	if degenerate > 0 {
		fmt.Println("For those queries part of the stream's membership is insertion order, not proximity.")
		fmt.Println("See docs/EVAL.md sections 5.7 and 5.9.")
	} else {
		fmt.Println("No stream's membership is decided by the tiebreak: the scores separate the cut.")
	}
	fmt.Println()
	return nil
}
