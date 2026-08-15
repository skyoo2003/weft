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
// copy can drift from the library it mirrors. That risk is pinned rather than argued
// away — TestRRFAtTheLibraryConstantIsFuse asserts rrf(fusion.RRFk) is bit-identical to
// fusion.Fuse, so a change to the library's accumulation that this copy does not follow
// fails the build instead of quietly re-measuring the sweep against a function the
// library no longer has. Prefer a parameter on pkg/fusion the moment a second caller
// wants a rank constant of its own — the milestone was already willing to touch that
// package for FuseWeighted, so the argument for the copy is that no caller outside this
// sweep has asked, and that docs/EVAL.md section 2.1 cites this sweep as what the
// injected Fuser bought.
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
	// Whether the file was there at all, kept separate from how many of its ids
	// matched. A file that loaded and then matched nothing — one generated against a
	// different dataset, or before the query ids were renamed — leaves withVec at 0,
	// which is indistinguishable from "no file" by count alone. The missing-file case
	// at least prints the warning below; without this flag the mismatched-file case
	// would print nothing and publish a text-only run under the text+vector label.
	haveVecFile := err == nil
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		log.Printf("WARNING: %s is missing, so the vector scorer has no opinion and the", vecPath)
		log.Printf("WARNING: baseline arm is TEXT ONLY. Any graph improvement measured against")
		log.Printf("WARNING: it is an upper bound. Run testdata/gen_query_vectors.py first.")
	}

	// Judgments for a query the query file does not hold. The loop below walks the
	// queries and looks each one's judgments up, so a qrels row for a query that is
	// missing from queries.jsonl is not skipped, reported or counted — it is never
	// reached. A truncated or mismatched query file therefore produces a mean and a
	// bootstrap over however many queries survived, printed under the usual heading
	// with a query count nobody compares to 50. Dropping a *judgment-less query* is
	// the deliberate case and is counted below; this is its mirror image, and there is
	// no reading of it that is not a broken pairing.
	//
	// Checked even under -any-snapshot, because that flag says "a different corpus",
	// not "a query set and its judgments that disagree".
	byID := make(map[string]bool, len(qs))
	for _, q := range qs {
		byID[q.ID] = true
	}
	var orphans []string
	for id := range qrels {
		if !byID[id] {
			orphans = append(orphans, id)
		}
	}
	if len(orphans) > 0 {
		// Sorted: map iteration is randomised, and an error naming a different query
		// on every run is an error nobody can act on.
		sort.Strings(orphans)
		return nil, fmt.Errorf("%s holds judgments for %d queries %s does not contain (%q is one), "+
			"so those judgments are unreachable and every arm would be averaged over the queries that "+
			"happen to remain", qrelsFile, len(orphans), queriesFile, orphans[0])
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
			// Paired by id, then checked by text. Ids are stable and short — trec-covid
			// numbers its queries "1" to "50" — so a vector file generated before the
			// query set was last edited matches every id and covers every query, and the
			// all-or-none check below passes on embeddings of different questions. The
			// text is what the generator writes for this, and it is only worth writing
			// if something reads it.
			if v.Text != q.Text {
				return nil, fmt.Errorf("query %s in %s was embedded from %q, but %s now reads %q; "+
					"the vector file is from an older query set (regenerate with "+
					"testdata/gen_query_vectors.py)", q.ID, vecPath, v.Text, queriesFile, q.Text)
			}
			// And which embedding produced it, which the id and the text cannot say. A
			// file generated from another adapter, base revision or local model
			// configuration carries the right id and the right question, so every check
			// above passes while the vector scorer computes cosine similarity across two
			// spaces — the query-side half of the hazard checkVectorModels closes on the
			// document side. Absence is tolerated for the same reason it is there: the
			// committed query-vectors.jsonl predates the field. A recorded model that
			// disagrees is not.
			if v.Model != "" && v.Model != queryVecModel {
				return nil, fmt.Errorf("query %s in %s was embedded by %q, and the document vectors "+
					"are %s: cosine similarity between two embedding spaces is not a similarity "+
					"(regenerate with testdata/gen_query_vectors.py)",
					q.ID, vecPath, v.Model, eval.S2Model)
			}
			eq.Query.Vector = v.Vec
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
	// two populations. Zero coverage from a file that did load counts as partial, not
	// as "no vectors": the run would otherwise complete with the vector scorer
	// abstaining on every query and still print, publish and label the arm
	// text+vector.
	if (haveVecFile || withVec > 0) && withVec != len(out) {
		return nil, fmt.Errorf("%d of %d queries have a vector in %s; all or none, "+
			"otherwise the arms are not comparable across queries", withVec, len(out), vecPath)
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

// openIndex opens the committed index under dir and, unless anySnapshot, checks that
// it is the one docs/EVAL.md publishes numbers from.
//
// The snapshot check the callers run covers queries.jsonl and qrels/test.tsv, which
// are the files *they* read. The index is a third input and nothing verified it: an
// index built from another corpus revision that kept the same document keys satisfies
// the qrels check too, and its different text, vectors and links then rank differently
// under the published labels. See provenance.
func openIndex(dir string, anySnapshot bool) (*engine.Index, error) {
	path := filepath.Join(dir, indexDir)
	if !anySnapshot {
		if err := verifyProvenance(path); err != nil {
			return nil, err
		}
	}
	start := time.Now()
	ix, err := engine.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open index: %w (run `weft-eval build` first)", err)
	}
	docs, avgdl := ix.Stats()
	log.Printf("index: %d documents, avgdl %.1f, opened in %s",
		docs, avgdl, time.Since(start).Round(time.Millisecond))
	return ix, nil
}

// ---------------------------------------------------------------- run

// checkIters rejects a resample count BootstrapCI would reject, at the only point
// where rejecting it is free.
//
// eval.BootstrapCI already refuses iters <= 0, but it is called after every arm has
// been evaluated: on the documented corpus that is 50 queries against 171,332
// documents, with a brute-force vector scan per query, spent to arrive at a flag
// error that was decidable before the index was opened. sweep is worse — it evaluates
// three arms per grid cell — and the failure is silent until it is total, because
// nothing prints until the table does.
// deltaSign is the sign of an observed delta, which is the whole of what docs/EVAL.md
// section 4 rule 2 reads.
//
// It used to be read off the confidence interval instead — a cell whose interval
// spanned zero was recorded as signless, and sweep's flip count skipped it — which
// folded rule 1's criterion into rule 2's and left neither stated. A grid going
// +0.02, -0.02, +0.02 under intervals wide enough to span zero then established no
// first sign, counted no flip, and printed "rule 2 holds" about deltas that had
// flipped twice. Interval uncertainty is not discarded, it goes in its own column:
// that is the separate thing rule 1 reads at the frozen point, and keeping the two
// apart is the only way either can be checked against what section 4 wrote down
// before the numbers existed.
//
// Exactly zero is not a direction: it establishes no sign and disagrees with none.
// Reachable rather than theoretical — two arms that rank identically for every query
// produce it, which is what a vanishing graph weight converges to.
func deltaSign(d float64) int {
	switch {
	case d > 0:
		return 1
	case d < 0:
		return -1
	}
	return 0
}

func checkIters(iters int) error {
	if iters <= 0 {
		return fmt.Errorf("-iters=%d: the bootstrap needs at least one resample (%w)", iters, eval.ErrNoIters)
	}
	return nil
}

func run(ctx context.Context, args []string) error {
	var k, overfetch, iters int
	var rankConst float64
	var anySnapshot bool
	data, flagErr := dataFlags(cmdRun, args, func(fs *flag.FlagSet) {
		fs.IntVar(&k, "k", frozenK, "rank cut")
		fs.IntVar(&overfetch, "overfetch", frozenOverfetch, "ask each scorer for k*this")
		fs.IntVar(&iters, "iters", bootstrapIters, "bootstrap resamples")
		fs.Float64Var(&rankConst, "rrfk", fusion.RRFk, "RRF rank constant")
		snapshotFlag(fs, &anySnapshot)
	})
	if flagErr != nil {
		return flagErr
	}

	if err := checkIters(iters); err != nil {
		return err
	}

	// The judged inputs, before the index is opened. A qrels file cut at a row boundary
	// reads as a clean prefix and lifts every arm by an unreported amount — see the
	// snapshot table.
	if !anySnapshot {
		if err := verifySnapshot(*data, queriesFile, qrelsFile); err != nil {
			return err
		}
	}

	// Checked before the index is opened, so a typo costs a message rather than a
	// minute. rrf has no error to return — it builds a Fuser — so the flag is the
	// only place this can be caught. A negative constant is not merely unusual: at
	// -1 the rank-1 weight is 1/0, every document a stream ranks first scores +Inf,
	// they compare equal, and TopK settles them on DocID. NaN and ±Inf produce the
	// same collapse from their own direction, and in all three cases the run
	// completes and prints the result under an "RRFk=" heading naming the value.
	if math.IsNaN(rankConst) || math.IsInf(rankConst, 0) || rankConst < 0 {
		return fmt.Errorf("-rrfk=%v: the RRF rank constant must be finite and non-negative", rankConst)
	}

	ix, err := openIndex(*data, anySnapshot)
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
	var anySnapshot bool
	data, flagErr := dataFlags(cmdSweep, args, func(fs *flag.FlagSet) {
		fs.IntVar(&k, "k", frozenK, "rank cut")
		fs.IntVar(&iters, "iters", 2000,
			"bootstrap resamples per configuration (lower than the headline: this is a sign check, not a published interval)")
		snapshotFlag(fs, &anySnapshot)
	})
	if flagErr != nil {
		return flagErr
	}

	if err := checkIters(iters); err != nil {
		return err
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
	qs, err := loadQueries(*data)
	if err != nil {
		return err
	}

	// The point is not to find the best configuration. It is to find out whether
	// the sign of (graph - baseline) survives changing constants nobody tuned.
	rankConsts := []float64{1, 10, 20, 40, fusion.RRFk, 100, 200}
	overfetches := []int{1, 2, 5, 10}

	// Both pairs, every cell. The binding pair is the one section 4 rule 2 names and is
	// what the verdict below is read from; the text-only pair stays because its 28 cells
	// are published in docs/EVAL.md section 5.10, and because a second framing of the
	// same signal is worth having.
	//
	// It used to be the text-only pair alone, on the grounds that the sign was already
	// negative in both framings at the frozen point and that re-running the grid would
	// cost an hour. Two things were wrong with that. The verdict line printed "rule 2
	// holds" from evidence about a neighbouring comparison, which is exactly the kind of
	// claim this harness exists to prevent. And the hour was a guess: text+vector fuses
	// two streams so it has to be measured per cell rather than once, which triples the
	// evaluations, and the whole grid still finishes well inside it.
	baseText, err := eval.Evaluate(ctx, ix, qs, armsFor(ix, fusion.Fuse, 1)[armTextBase], k)
	if err != nil {
		return err
	}

	// One cell of the grid, for one pair.
	type cell struct {
		rc        float64
		of        int
		base, arm float64
		iv        eval.Interval
		sign      int
		spansZero bool
		flipped   bool
	}
	var bindingCells, textCells []cell
	for _, rc := range rankConsts {
		for _, of := range overfetches {
			fuse := engine.Fuser(fusion.Fuse)
			if rc != fusion.RRFk {
				fuse = rrf(rc)
			}
			arms := armsFor(ix, fuse, of)

			// text+vector is not invariant across this grid the way text is: it fuses two
			// streams, so both the rank constant and the fetch depth move it. Measuring it
			// once and reusing it is the shortcut that made the old table cheap and the old
			// claim wrong.
			vecBase, err := eval.Evaluate(ctx, ix, qs, arms[armVecBase], k)
			if err != nil {
				return err
			}
			vecGraph, err := eval.Evaluate(ctx, ix, qs, arms[armVecGraph], k)
			if err != nil {
				return err
			}
			textGraph, err := eval.Evaluate(ctx, ix, qs, arms[armTextGraph], k)
			if err != nil {
				return err
			}

			ivBind, err := eval.BootstrapCI(vecBase.PerQuery, vecGraph.PerQuery, iters, bootstrapSeed)
			if err != nil {
				return err
			}
			ivText, err := eval.BootstrapCI(baseText.PerQuery, textGraph.PerQuery, iters, bootstrapSeed)
			if err != nil {
				return err
			}
			bindingCells = append(bindingCells,
				cell{rc, of, vecBase.NDCG, vecGraph.NDCG, ivBind, deltaSign(ivBind.Delta), ivBind.ContainsZero(), false})
			textCells = append(textCells,
				cell{rc, of, baseText.NDCG, textGraph.NDCG, ivText, deltaSign(ivText.Delta), ivText.ContainsZero(), false})
		}
	}

	// A flip is a sign that disagrees with the first sign the grid established, in grid
	// order. Marked before printing, and counted per pair: a flip in one framing is not
	// a flip in the other, and collapsing them would hide which one moved.
	flips := func(cells []cell) (int, int) {
		first, n := 0, 0
		for i := range cells {
			switch {
			case cells[i].sign == 0:
			case first == 0:
				first = cells[i].sign
			case cells[i].sign != first:
				cells[i].flipped = true
				n++
			}
		}
		return first, n
	}
	bindFirst, bindFlips := flips(bindingCells)
	_, textFlips := flips(textCells)

	// Cells whose interval spans zero, reported rather than folded into the sign. A
	// sweep can be perfectly sign-stable and still rest on 28 intervals that each
	// admit the opposite direction, and a reader who is told only "no flip" cannot
	// tell that from a grid of narrow intervals.
	spanning := func(cells []cell) int {
		n := 0
		for _, c := range cells {
			if c.spansZero {
				n++
			}
		}
		return n
	}
	bindSpans := spanning(bindingCells)

	printCells := func(title, base, arm string, cells []cell) {
		fmt.Printf("\n%s: sign stability of %s - %s at nDCG@%d\n\n", title, arm, base, k)
		fmt.Printf("  %8s %10s %10s %10s %9s %22s %s\n",
			"RRFk", "overfetch", "base", "arm", "delta", "delta 95% CI", "sign")
		for _, c := range cells {
			label := map[int]string{1: "+", 0: "0 (delta is exactly zero)", -1: "-"}[c.sign]
			if c.spansZero {
				label += "  CI spans zero"
			}
			if c.flipped {
				label += "  FLIP"
			}
			fmt.Printf("  %8v %10d %10.4f %10.4f %+9.4f   [%+.4f, %+.4f] %s\n",
				c.rc, c.of, c.base, c.arm, c.iv.Delta, c.iv.Lo, c.iv.Hi, label)
		}
	}
	printCells("BINDING", armVecBase, armVecGraph, bindingCells)
	printCells("COMPARISON", armTextBase, armTextGraph, textCells)

	fmt.Printf("\n%d configurations, %d sign flips on the binding pair, %d on the comparison pair\n",
		len(bindingCells), bindFlips, textFlips)
	switch {
	case bindFlips > 0:
		fmt.Println("VERDICT INPUT: the sign is not stable. docs/EVAL.md section 4 rule 2 fails.")
	case bindFirst == 0:
		// Every delta was exactly zero, so the grid established no direction to be
		// stable in. Reporting that as stability would read "rule 2 holds" off a table
		// that recorded no difference at all.
		fmt.Println("VERDICT INPUT: every delta is exactly zero, so no configuration established a")
		fmt.Println("sign. That is not stability; docs/EVAL.md section 4 rule 2 is undetermined.")
	default:
		fmt.Printf("VERDICT INPUT: no sign flip across the sweep on the pair rule 2 names (%s - %s).\n",
			armVecGraph, armVecBase)
		fmt.Println("docs/EVAL.md section 4 rule 2 holds.")
		if bindSpans > 0 {
			// Rule 2 is satisfied and says less than it appears to. Printed next to the
			// verdict rather than left in the table, because "no sign flip" over cells
			// that each admit the opposite direction is the reading this document keeps
			// having to correct.
			fmt.Printf("Note: %d of %d binding cells have an interval spanning zero, so the stable\n",
				bindSpans, len(bindingCells))
			fmt.Println("sign is not established by those cells. That is rule 1's criterion, not rule 2's.")
		}
	}
	fmt.Println()
	return nil
}

// ---------------------------------------------------------------- weights

// weights answers the question milestone 4 created rather than settled: unweighted
// RRF gives a near-noise stream the same vote as BM25's, and the measured cost of
// that was −0.1227 nDCG@10. Does discounting the graph stream recover it?
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
	var anySnapshot bool
	data, flagErr := dataFlags(cmdWeights, args, func(fs *flag.FlagSet) {
		fs.IntVar(&k, "k", frozenK, "rank cut")
		fs.IntVar(&iters, "iters", bootstrapIters, "bootstrap resamples")
		snapshotFlag(fs, &anySnapshot)
	})
	if flagErr != nil {
		return flagErr
	}

	if err := checkIters(iters); err != nil {
		return err
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
	var anySnapshot bool
	data, flagErr := dataFlags(cmdDiagnose, args, func(fs *flag.FlagSet) {
		fs.IntVar(&deep, "deep", 1_000_000, "k to request, large enough not to truncate the frontier")
		fs.IntVar(&k, "k", frozenK, "the cut whose tie group is measured")
		snapshotFlag(fs, &anySnapshot)
	})
	if flagErr != nil {
		return flagErr
	}

	if !anySnapshot {
		if err := verifySnapshot(*data, queriesFile, qrelsFile); err != nil {
			return err
		}
	}

	// Checked here rather than at the cut below, where min(k, len(cands))-1 would
	// index -1 and panic on the first query that produced any candidate. The other
	// subcommands never needed this guard because eval.Evaluate rejects k <= 0 for
	// them; diagnose reads the streams directly and so owns the check itself.
	if k <= 0 {
		return fmt.Errorf("-k is %d, want > 0", k)
	}
	if deep <= 0 {
		// Not a panic, a worse outcome: engine.Search returns nothing for a
		// non-positive k, so every query reports "no reachable neighbour" and the
		// table reads as a graph with no edges at all.
		return fmt.Errorf("-deep is %d, want > 0", deep)
	}
	// The arbitrary column counts candidates that tie the cut score and lost their slot
	// to a lower DocID, and it can only see them beyond the cut. Requesting a stream no
	// deeper than the cut truncates the tie group at exactly the point being measured,
	// so every query reports zero and the run concludes that nothing was decided by the
	// tiebreak — the reassuring answer, from a depth that could not have found the
	// alarming one. The default is 1,000,000 for that reason; this rejects a flag that
	// quietly undoes it.
	if deep <= k {
		return fmt.Errorf("-deep is %d and -k is %d: the frontier has to run past the cut for a "+
			"tie group crossing it to be visible at all, or every query reports zero slots "+
			"decided by DocID whatever the graph did", deep, k)
	}

	ix, err := openIndex(*data, anySnapshot)
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
	fmt.Printf("  %-6s %10s %10s %12s %12s %12s\n",
		"query", "cands", "distinct", "tied-at-cut", "slots", "excluded")

	var degenerate, noOpinion, slotTotal, excludedTotal int
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
		// Two different counts, kept apart because one of them used to be reported as
		// the other. `excluded` is the candidates at the cut score that did not make
		// the top k; `slots` is the positions *in the reported ranking* they lost to,
		// which is what "decided by DocID" can mean. They differ badly at the extreme:
		// 100 candidates tied for 10 positions is 90 excluded and 10 arbitrary slots,
		// and reporting 90 makes a claim about a top-10 that a top-10 cannot hold.
		// Both are printed, because excluded is what says how large the tie group was
		// and slots is what says how much of the answer it decided.
		excluded, slots := 0, 0
		for _, c := range cands[min(k, len(cands)):] {
			if c.Score == cut {
				excluded++
			}
		}
		if excluded > 0 {
			for _, c := range cands[:min(k, len(cands))] {
				if c.Score == cut {
					slots++
				}
			}
			degenerate++
			slotTotal += slots
			excludedTotal += excluded
		}
		fmt.Printf("  %-6s %10d %10d %12d %12d %12d\n",
			q.ID, len(cands), len(distinct), tied, slots, excluded)
	}

	answered := len(qs) - noOpinion
	fmt.Printf("\n%d of %d queries produced a graph stream at all", answered, len(qs))
	if noOpinion > 0 {
		fmt.Printf(" (%d had no reachable neighbour)", noOpinion)
	}
	fmt.Printf(".\n%d of those %d have a tie group crossing the cut: %d of the reported slots are held\n",
		degenerate, answered, slotTotal)
	fmt.Printf("at the cut score, with %d further candidates excluded from it by DocID alone.\n", excludedTotal)
	if degenerate > 0 {
		fmt.Println("For those queries part of the stream's membership is insertion order, not proximity.")
		fmt.Println("See docs/EVAL.md sections 5.7 and 5.9.")
	} else {
		fmt.Println("No stream's membership is decided by the tiebreak: the scores separate the cut.")
	}
	fmt.Println()
	return nil
}
