// SPDX-License-Identifier: Apache-2.0

// Command weights runs one query twice and prints both rankings, so the effect
// of discounting a stream is visible rather than asserted.
//
// The left column is fusion.Fuse — every stream at a full vote. The right is
// fusion.FuseWeighted(1, 1, 0.1, 1), the same call with the third stream cut to
// a tenth. The third stream is the graph scorer, which milestone 4 measured at
// +0.0000 nDCG@10 at its best weight and −0.1227 at a full one.
//
//	go run ./examples/weights
//
// What to notice is what the weights attach to. FuseWeighted is handed a number
// for slot three, not the knowledge that slot three holds a graph scorer.
// Reorder the scorers slice without reordering the weights and the ranking moves
// silently — which is why the two are written at the same call site here.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/scorer/graph"
	"github.com/skyoo2003/weft/pkg/scorer/recency"
	"github.com/skyoo2003/weft/pkg/scorer/text"
	"github.com/skyoo2003/weft/pkg/scorer/vector"
)

var now = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func day(n int) time.Time { return now.AddDate(0, 0, -n) }

// Densely linked on purpose: the graph scorer has an opinion about every one of
// these, so weighting it down has somewhere to show.
var corpus = []engine.Document{
	{Key: "bm25", Text: "bm25 ranks documents by term frequency and document length", Vector: []float32{1, 0, 0}, Links: []string{"tfidf", "rrf"}, Time: day(400)},
	{Key: "tfidf", Text: "tf idf term weighting for ranking", Vector: []float32{0.9, 0, 0.1}, Links: []string{"bm25"}, Time: day(800)},
	{Key: "rrf", Text: "reciprocal rank fusion combines rankings from several retrievers", Vector: []float32{0.8, 0.3, 0}, Links: []string{"bm25", "hnsw"}, Time: day(30)},
	{Key: "hnsw", Text: "hierarchical navigable small world graphs for nearest neighbour search", Vector: []float32{0, 0.7, 0.7}, Links: []string{"ivf", "bm25"}, Time: day(120)},
	{Key: "ivf", Text: "inverted file index partitions vectors into clusters", Vector: []float32{0, 1, 0}, Links: []string{"hnsw"}, Time: day(200)},
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
		graph.New(ix, txt),
		recency.NewAt(ix, now),
	}

	q := engine.Query{Text: "ranking fusion", Vector: []float32{1, 0, 0}}
	const k = 5

	// A conversion rather than :=, so both halves of the comparison have the one
	// type engine.Search takes. fusion.Fuse is handed over as a value and never
	// called here.
	flat := engine.Fuser(fusion.Fuse)
	discounted := fusion.FuseWeighted(1, 1, 0.1, 1)

	left, err := rank(ix, q, k, flat, scorers)
	if err != nil {
		log.Fatal("fuse: ", err)
	}
	right, err := rank(ix, q, k, discounted, scorers)
	if err != nil {
		log.Fatal("fuse weighted: ", err)
	}

	fmt.Printf("query %q @ %v\n\n", q.Text, q.Vector)
	fmt.Printf("  %-4s %-24s %s\n", "", "Fuse (1,1,1,1)", "FuseWeighted (1,1,0.1,1)")
	for i := range max(len(left), len(right)) {
		fmt.Printf("  %-4d %-24s %s\n", i+1, at(left, i), at(right, i))
	}
	fmt.Println("\n  the third weight is the graph stream. Nothing here told the fuser that.")
}

// rank runs one search and returns each result as "key score", in fused order.
func rank(ix *engine.Index, q engine.Query, k int, fuse engine.Fuser, scorers []engine.Scorer) ([]string, error) {
	results, err := engine.Search(context.Background(), q, k, fuse, scorers...)
	if err != nil {
		return nil, err
	}
	rows := make([]string, 0, len(results))
	for _, c := range results {
		d, _ := ix.Doc(c.Doc)
		rows = append(rows, fmt.Sprintf("%s %.5f", d.Key, c.Score))
	}
	return rows, nil
}

// at is the shorter column padded with a dash, since the two rankings need not
// be the same length.
func at(rows []string, i int) string {
	if i >= len(rows) {
		return "-"
	}
	return rows[i]
}
