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

// searchCmd ranks an index.
func searchCmd(args []string, stdout, stderr io.Writer) error {
	var data, tokName, qs, qtext, vec, seeds, list, weights, nowStr string
	var k int
	var breakdown bool
	var restart, precision float64

	fs, err := parseFlags(cmdSearch, args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&data, "data", "", "directory holding the index")
		fs.StringVar(&tokName, "tokenizer", defaultTokenizer,
			"tokenizer the index was committed with: "+strings.Join(tokenizerNames(), ", "))
		fs.StringVar(&qs, "q", "", "a weft query string; pkg/query.Parse documents the whole syntax")
		fs.StringVar(&qtext, "text", "", "Query.Text, which the text scorer reads")
		fs.StringVar(&vec, "vector", "", "Query.Vector, comma-separated numbers")
		fs.StringVar(&seeds, "seeds", "", "Query.Seeds: comma-separated document keys for the graph scorer")
		fs.StringVar(&list, "scorers", "", "comma-separated streams to fuse: "+scorerForms)
		fs.StringVar(&weights, "weights", "",
			"one weight per stream, by position, comma-separated; without it every stream votes equally")
		fs.StringVar(&nowStr, "now", "", "the clock the recency scorer reads, RFC 3339; without it, the real one")
		fs.IntVar(&k, "k", 10, "how many results to return")
		fs.BoolVar(&breakdown, "breakdown", false, "print each scorer's own rank beside the fused one")
		fs.Float64Var(&restart, "restart", graph.DefaultRestart, "graph:ppr restart probability")
		fs.Float64Var(&precision, "precision", graph.DefaultPrecision, "graph:ppr residual threshold")
	})
	if err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if data == "" {
		return badUsage("-data names the directory holding the index, and it is required")
	}
	tok, ok := tokenizers[tokName]
	if !ok {
		return badUsage("unknown tokenizer %q; the choices are %s", tokName, strings.Join(tokenizerNames(), ", "))
	}
	// Search returns an empty result for k <= 0 rather than an error, so without
	// this every query answers "no results" and exits 0.
	if k <= 0 {
		return badUsage("-k must be positive, got %d", k)
	}
	var now time.Time
	if set["now"] {
		if now, err = time.Parse(time.RFC3339, nowStr); err != nil {
			return badUsage("-now %q is not an RFC 3339 time: %v", nowStr, err)
		}
	}
	q := engine.Query{Text: qtext, Seeds: splitList(seeds)}
	if vec != "" {
		if q.Vector, err = floats32(vec); err != nil {
			return badUsage("-vector: %v", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ix, err := engine.Open(data, engine.WithTokenizer(tok))
	if err != nil {
		return err
	}
	defer ix.Close() //nolint:errcheck // nothing was written, so a failed close changes no result

	// The plan's scorers come first, so the stream positions -weights attaches to
	// are the ones a reader counts off the command line from left to right: the
	// query string's clauses in the order they were written, then -scorers.
	var scorers []engine.Scorer
	var plan query.Plan
	if qs != "" {
		if plan, err = query.Parse(ix, qs); err != nil {
			return err
		}
		scorers = append(scorers, plan.Scorers...)
	}

	scorers, usedPPR, err := buildScorers(ctx, ix, list, scorers, now, set, restart, precision)
	if err != nil {
		return err
	}
	if len(scorers) == 0 {
		return badUsage("nothing to rank with: give -q a query string, or -scorers one of %s, or both",
			scorerForms)
	}

	// A knob that configures a scorer nobody asked for is accepted and then
	// ignored, which leaves a caller tuning a number and reading the unchanged
	// ranking as its answer. Refused instead, by name.
	for _, name := range []string{"restart", "precision"} {
		if set[name] && !usedPPR {
			return badUsage("-%s configures graph:ppr and no graph:ppr stream was asked for, "+
				"so it would have changed nothing about this ranking", name)
		}
	}
	if set["now"] && !usesRecency(list) {
		return badUsage("-now is the clock the recency scorer reads and no recency stream was asked for, " +
			"so it would have changed nothing about this ranking")
	}

	base := engine.Fuser(fusion.Fuse)
	if weights != "" {
		w, err := floats64(weights)
		if err != nil {
			return badUsage("-weights: %v", err)
		}
		if len(w) != len(scorers) {
			return badUsage("-weights has %d numbers and there are %d streams to fuse; "+
				"a weight attaches to a position, so a list of the wrong length silently re-ranks "+
				"every stream after the one it is short of", len(w), len(scorers))
		}
		base = fusion.FuseWeighted(w...)
	}
	fuse := base
	if qs != "" {
		// A query string's + and − are constraints over stream positions rather
		// than scorers of their own, which is why Parse hands back both halves and
		// leaves the caller to assemble them.
		fuse = plan.Fuse(base)
	}

	// Wrapping the fuser hands back the very streams Search fused rather than a
	// reconstruction from a second round of Candidates calls — and the two can
	// disagree the moment a scorer is not deterministic, which is the one thing
	// this display exists to rule out.
	var streams [][]engine.Candidate
	if breakdown {
		inner := fuse
		fuse = func(s [][]engine.Candidate, k int) []engine.Candidate {
			streams = s
			return inner(s, k)
		}
	}

	results, err := engine.Search(ctx, q, k, fuse, scorers...)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Fprintln(stdout, "no results")
		return nil
	}

	ranks := make([]map[engine.DocID]int, len(streams))
	for i, stream := range streams {
		ranks[i] = ranksOf(stream)
	}
	for rank, c := range results {
		d, _ := ix.Doc(c.Doc)
		fmt.Fprintf(stdout, "  %d. %-14s %.5f", rank+1, d.Key, c.Score)
		for i, s := range scorers {
			if i < len(ranks) {
				fmt.Fprintf(stdout, "  %s:%s", s.Name(), place(ranks[i][c.Doc]))
			}
		}
		fmt.Fprintln(stdout)
	}
	return nil
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
