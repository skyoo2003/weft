// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/query"
	"github.com/skyoo2003/weft/pkg/scorer/graph"
	"github.com/skyoo2003/weft/pkg/scorer/recency"
	"github.com/skyoo2003/weft/pkg/scorer/text"
	"github.com/skyoo2003/weft/pkg/scorer/vector"
)

// scorerForms is what -scorers accepts, written once because two things have to
// agree on it: the switch that builds a scorer, and the error that lists the
// choices when that switch falls through.
const scorerForms = "text, text:FIELD, vector, graph, graph:seeds, graph:ppr, recency"

// searchArgs is one invocation's flags after they have been checked.
//
// A struct rather than a dozen locals because the checking is worth reading on
// its own: more than half of this command is refusals, and they belong next to
// each other rather than interleaved with opening an index and printing a table.
type searchArgs struct {
	data, tokName, qs, qtext, vec, seeds, list, weights, nowStr string
	k                                                           int
	breakdown                                                   bool
	restart, precision                                          float64

	// set is which flags were written on the command line, as opposed to left at
	// their default. Three refusals need that difference: a default -restart is
	// not a request for one.
	set map[string]bool

	tok engine.Tokenizer
	now time.Time
	q   engine.Query
}

// searchCmd ranks an index.
func searchCmd(args []string, stdout, stderr io.Writer) error {
	a, err := parseSearchArgs(args, stderr)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ix, err := engine.Open(a.data, engine.WithTokenizer(a.tok))
	if err != nil {
		return err
	}
	defer ix.Close() //nolint:errcheck // nothing was written, so a failed close changes no result

	scorers, fuse, err := a.streams(ctx, ix)
	if err != nil {
		return err
	}

	// Wrapping the fuser hands back the very streams Search fused rather than a
	// reconstruction from a second round of Candidates calls — and the two can
	// disagree the moment a scorer is not deterministic, which is the one thing
	// this display exists to rule out.
	var streams [][]engine.Candidate
	if a.breakdown {
		inner := fuse
		fuse = func(s [][]engine.Candidate, k int) []engine.Candidate {
			streams = s
			return inner(s, k)
		}
	}

	results, err := engine.Search(ctx, a.q, a.k, fuse, scorers...)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		outln(stdout, "no results")
		return nil
	}
	printRanking(stdout, ix, results, scorers, streams)
	return nil
}

// parseSearchArgs reads the flags and refuses every value that cannot mean what
// it says. What it does not check is the combinations, which need the scorer
// list built first; streams does those.
func parseSearchArgs(args []string, stderr io.Writer) (*searchArgs, error) {
	var a searchArgs

	fs, err := parseFlags(cmdSearch, args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&a.data, "data", "", "directory holding the index")
		fs.StringVar(&a.tokName, "tokenizer", defaultTokenizer,
			"tokenizer the index was committed with: "+strings.Join(tokenizerNames(), ", "))
		fs.StringVar(&a.qs, "q", "", "a weft query string; pkg/query.Parse documents the whole syntax")
		fs.StringVar(&a.qtext, "text", "", "Query.Text, which the text scorer reads")
		fs.StringVar(&a.vec, "vector", "", "Query.Vector, comma-separated numbers")
		fs.StringVar(&a.seeds, "seeds", "", "Query.Seeds: comma-separated document keys for the graph scorer")
		fs.StringVar(&a.list, "scorers", "", "comma-separated streams to fuse: "+scorerForms)
		fs.StringVar(&a.weights, "weights", "",
			"one weight per stream, by position, comma-separated; without it every stream votes equally")
		fs.StringVar(&a.nowStr, "now", "",
			"the clock the recency scorer reads, RFC 3339; without it, the real one")
		fs.IntVar(&a.k, "k", 10, "how many results to return")
		fs.BoolVar(&a.breakdown, "breakdown", false, "print each scorer's own rank beside the fused one")
		fs.Float64Var(&a.restart, "restart", graph.DefaultRestart, "graph:ppr restart probability")
		fs.Float64Var(&a.precision, "precision", graph.DefaultPrecision, "graph:ppr residual threshold")
	})
	if err != nil {
		return nil, err
	}
	a.set = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { a.set[f.Name] = true })

	if a.data == "" {
		return nil, badUsage("-data names the directory holding the index, and it is required")
	}
	var ok bool
	if a.tok, ok = tokenizers[a.tokName]; !ok {
		return nil, badUsage("unknown tokenizer %q; the choices are %s",
			a.tokName, strings.Join(tokenizerNames(), ", "))
	}
	// Search returns an empty result for k <= 0 rather than an error, so without
	// this every query answers "no results" and exits 0.
	if a.k <= 0 {
		return nil, badUsage("-k must be positive, got %d", a.k)
	}
	if a.set["now"] {
		if a.now, err = time.Parse(time.RFC3339, a.nowStr); err != nil {
			return nil, badUsage("-now %q is not an RFC 3339 time: %v", a.nowStr, err)
		}
	}
	a.q = engine.Query{Text: a.qtext, Seeds: splitList(a.seeds)}
	if a.vec != "" {
		if a.q.Vector, err = floats32(a.vec); err != nil {
			return nil, badUsage("-vector: %v", err)
		}
	}
	return &a, nil
}

// streams builds what will be fused and the fuser that will do it.
//
// The two are returned together because the second is only checkable against the
// first: -weights attaches to positions, so its length is wrong or right only
// once the streams exist.
func (a *searchArgs) streams(ctx context.Context, ix *engine.Index) ([]engine.Scorer, engine.Fuser, error) {
	// The query string's scorers come first, so the positions -weights attaches
	// to are the ones a reader counts off the command line from left to right:
	// the clauses in the order they were written, then -scorers.
	var scorers []engine.Scorer
	var plan query.Plan
	var err error
	if a.qs != "" {
		if plan, err = query.Parse(ix, a.qs); err != nil {
			return nil, nil, err
		}
		scorers = append(scorers, plan.Scorers...)
	}

	scorers, usedPPR, err := buildScorers(ctx, ix, a.list, scorers, a.now, a.set, a.restart, a.precision)
	if err != nil {
		return nil, nil, err
	}
	if len(scorers) == 0 {
		return nil, nil, badUsage("nothing to rank with: give -q a query string, or -scorers one of %s, "+
			"or both", scorerForms)
	}
	if err := a.refuseIgnoredKnobs(usedPPR); err != nil {
		return nil, nil, err
	}

	base := engine.Fuser(fusion.Fuse)
	if a.weights != "" {
		w, err := floats64(a.weights)
		if err != nil {
			return nil, nil, badUsage("-weights: %v", err)
		}
		if len(w) != len(scorers) {
			return nil, nil, badUsage("-weights has %d numbers and there are %d streams to fuse; "+
				"a weight attaches to a position, so a list of the wrong length silently re-ranks "+
				"every stream after the one it is short of", len(w), len(scorers))
		}
		base = fusion.FuseWeighted(w...)
	}
	if a.qs == "" {
		return scorers, base, nil
	}
	// A query string's + and − are constraints over stream positions rather than
	// scorers of their own, which is why Parse hands back both halves and leaves
	// the caller to assemble them.
	return scorers, plan.Fuse(base), nil
}

// refuseIgnoredKnobs is the check that needs the streams: a knob configuring a
// scorer nobody asked for would be accepted and then ignored, which leaves a
// caller tuning a number and reading the unchanged ranking as its answer.
func (a *searchArgs) refuseIgnoredKnobs(usedPPR bool) error {
	for _, name := range []string{"restart", "precision"} {
		if a.set[name] && !usedPPR {
			return badUsage("-%s configures graph:ppr and no graph:ppr stream was asked for, "+
				"so it would have changed nothing about this ranking", name)
		}
	}
	if a.set["now"] && !usesRecency(a.list) {
		return badUsage("-now is the clock the recency scorer reads and no recency stream was asked " +
			"for, so it would have changed nothing about this ranking")
	}
	return nil
}

// printRanking writes the fused order, with each scorer's own rank beside it
// when the streams were captured.
func printRanking(w io.Writer, ix *engine.Index, results []engine.Candidate,
	scorers []engine.Scorer, streams [][]engine.Candidate,
) {
	ranks := make([]map[engine.DocID]int, len(streams))
	for i, stream := range streams {
		ranks[i] = ranksOf(stream)
	}
	for rank, c := range results {
		d, _ := ix.Doc(c.Doc)
		outf(w, "  %d. %-14s %.5f", rank+1, d.Key, c.Score)
		for i, s := range scorers {
			if i < len(ranks) {
				outf(w, "  %s:%s", s.Name(), place(ranks[i][c.Doc]))
			}
		}
		outln(w)
	}
}

// buildScorers turns -scorers into the streams to fuse, appending to whatever a
// query string already contributed.
//
// prior is not only the accumulator: a graph scorer is constructed *from*
// another scorer, and the one it walks from is the stream written immediately
// before it. That is why this is a fold and not a map.
func buildScorers(
	ctx context.Context, ix *engine.Index, list string, prior []engine.Scorer,
	now time.Time, set map[string]bool, restart, precision float64,
) (scorers []engine.Scorer, usedPPR bool, err error) {
	scorers = prior
	var adj *graph.Adjacency

	for _, name := range splitList(list) {
		kind, arg, _ := strings.Cut(name, ":")
		var s engine.Scorer

		switch kind {
		case "text":
			if arg == "" {
				s = text.New(ix)
			} else {
				s = text.NewField(ix, arg)
			}
		case "vector":
			if arg != "" {
				return nil, false, badUsage("%q takes no argument", name)
			}
			s = vector.New(ix)
		case "recency":
			if arg != "" {
				return nil, false, badUsage("%q takes no argument", name)
			}
			if set["now"] {
				s = recency.NewAt(ix, now)
			} else {
				s = recency.New(ix)
			}
		case "graph":
			seed := lastScorer(scorers)
			if seed == nil {
				return nil, false, badUsage("%q needs a seed scorer to walk from: put text, "+
					"text:FIELD or vector before it in -scorers, or give -q a clause. A graph "+
					"stream with no seed is empty for a reason the ranking cannot show", name)
			}
			switch arg {
			case "":
				s = graph.New(ix, seed)
			case "seeds":
				s = graph.NewIncludingSeeds(ix, seed)
			case "ppr":
				if adj == nil {
					if adj, err = graph.NewAdjacency(ctx, ix); err != nil {
						return nil, false, err
					}
				}
				var opts []graph.Option
				if set["restart"] {
					opts = append(opts, graph.WithRestart(restart))
				}
				if set["precision"] {
					opts = append(opts, graph.WithPrecision(precision))
				}
				p, err := graph.NewPPR(adj, seed, opts...)
				if err != nil {
					return nil, false, err
				}
				s, usedPPR = p, true
			default:
				return nil, false, badUsage("unknown graph form %q; the choices are graph, "+
					"graph:seeds and graph:ppr", name)
			}
		default:
			return nil, false, badUsage("unknown scorer %q; the choices are %s", name, scorerForms)
		}
		scorers = append(scorers, s)
	}
	return scorers, usedPPR, nil
}

// usesRecency answers whether -scorers asked for the one scorer -now configures.
func usesRecency(list string) bool {
	for _, name := range splitList(list) {
		if kind, _, _ := strings.Cut(name, ":"); kind == "recency" {
			return true
		}
	}
	return false
}

// lastScorer is the stream a graph scorer walks from, or nil when there is none.
func lastScorer(scorers []engine.Scorer) engine.Scorer {
	if len(scorers) == 0 {
		return nil
	}
	return scorers[len(scorers)-1]
}

// ranksOf maps each document in one scorer's stream to its 1-based position.
func ranksOf(stream []engine.Candidate) map[engine.DocID]int {
	ranks := make(map[engine.DocID]int, len(stream))
	for i, c := range stream {
		ranks[c.Doc] = i + 1
	}
	return ranks
}

// place renders a rank, or a dash for a document absent from that scorer's
// stream.
//
// A dash is not the same as "no opinion". Every scorer is asked for k, so a
// scorer that ranks the whole corpus — recency does — shows a dash for anything
// below its own top k. Raise -k and the dashes fill in.
func place(rank int) string {
	if rank == 0 {
		return "-"
	}
	return strconv.Itoa(rank)
}

// floats32 parses a vector.
func floats32(s string) ([]float32, error) {
	var out []float32
	for _, part := range splitList(s) {
		v, err := strconv.ParseFloat(part, 32)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", part)
		}
		out = append(out, float32(v))
	}
	return out, nil
}

// floats64 parses a weight list.
func floats64(s string) ([]float64, error) {
	var out []float64
	for _, part := range splitList(s) {
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", part)
		}
		out = append(out, v)
	}
	return out, nil
}
