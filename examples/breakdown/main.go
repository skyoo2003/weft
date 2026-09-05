// SPDX-License-Identifier: Apache-2.0

// Command breakdown shows what fusion did, one scorer at a time.
//
// Four scorers answer one query, and each result is printed beside the rank it
// held in every scorer's own stream. A dash means that scorer did not have the
// document in its top k. That is the whole point of the display: the fused order
// is no single scorer's order, and nothing in engine.Search or the fuser knows
// which stream came from which scorer.
//
//	go run ./examples/breakdown
//
// For the same display over an index on disk, see `weft search -breakdown`.
package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/scorer/graph"
	"github.com/skyoo2003/weft/pkg/scorer/recency"
	"github.com/skyoo2003/weft/pkg/scorer/text"
	"github.com/skyoo2003/weft/pkg/scorer/vector"
)

// now is pinned. With time.Now the recency ranks drift between runs and the
// output stops being something a reader can check against this file.
var now = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func day(n int) time.Time { return now.AddDate(0, 0, -n) }

// The three vector dimensions stand in for topics: [ranking, vectors, graphs].
// Vectors are hand-assigned; generating embeddings is out of scope for weft, so
// an example cannot compute them.
var corpus = []engine.Document{
	{Key: "bm25", Text: "bm25 ranks documents by term frequency and document length", Vector: []float32{1, 0, 0}, Links: []string{"tfidf"}, Time: day(400)},
	{Key: "tfidf", Text: "tf idf term weighting for ranking", Vector: []float32{0.9, 0, 0.1}, Links: []string{"bm25"}, Time: day(800)},
	{Key: "rrf", Text: "reciprocal rank fusion combines rankings from several retrievers", Vector: []float32{0.8, 0.3, 0}, Links: []string{"bm25", "hnsw"}, Time: day(30)},
	{Key: "hnsw", Text: "hierarchical navigable small world graphs for approximate nearest neighbour search", Vector: []float32{0, 0.7, 0.7}, Links: []string{"ivf"}, Time: day(120)},
	{Key: "changelog", Text: "release notes for the current version", Time: day(1)},
}

func main() {
	ix := engine.New()
	for _, d := range corpus {
		if _, err := ix.Add(d); err != nil {
			log.Fatalf("index %q: %v", d.Key, err)
		}
	}

	txt := text.New(ix)
	scorers := []engine.Scorer{
		txt,
		vector.New(ix),
		graph.New(ix, txt), // seeded by any scorer, here the text one
		recency.NewAt(ix, now),
	}

	// One weight per stream, by position, written next to the slice that fixes
	// that position — a reordered slice with unedited weights re-ranks silently.
	// The graph stream takes a tenth of a vote: milestone 4 measured that scorer
	// at +0.0000 nDCG@10 at its best weight, and both the README and the scorer's
	// own package documentation tell a caller to weight it down if they enable it
	// at all. examples/weights is this corpus with that number moved.
	weighted := fusion.FuseWeighted(1, 1, 0.1, 1)

	q := engine.Query{Text: "ranking fusion", Vector: []float32{1, 0, 0}}
	const k = 5

	// The fuser is a parameter, so wrapping it hands back the very streams Search
	// fused. Asking every scorer a second time to rebuild the breakdown would
	// print a reconstruction instead of what happened, and the two can disagree
	// the moment a scorer is not deterministic — which is the one thing this
	// display exists to rule out. It also halves the work.
	var streams [][]engine.Candidate
	fuse := func(s [][]engine.Candidate, k int) []engine.Candidate {
		streams = s
		return weighted(s, k)
	}

	// Note that this call names no scorer and no count.
	results, err := engine.Search(context.Background(), q, k, fuse, scorers...)
	if err != nil {
		log.Fatal("search: ", err)
	}

	breakdown := make([]map[engine.DocID]int, len(streams))
	for i, stream := range streams {
		breakdown[i] = ranksOf(stream)
	}

	fmt.Printf("query %q @ %v, %d scorers\n\n", q.Text, q.Vector, len(scorers))
	for rank, c := range results {
		d, _ := ix.Doc(c.Doc)
		fmt.Printf("  %d. %-10s %.5f  ", rank+1, d.Key, c.Score)
		for i, s := range scorers {
			fmt.Printf("%s:%s  ", s.Name(), place(breakdown[i][c.Doc]))
		}
		fmt.Println()
	}
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
// below its own top k. Raise k and the dashes fill in. Only a scorer with
// nothing to say at all is dashed the whole way down, which is what
// examples/sparse is about.
func place(rank int) string {
	if rank == 0 {
		return "-"
	}
	return strconv.Itoa(rank)
}
