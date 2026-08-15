// SPDX-License-Identifier: Apache-2.0

// Command basic is the smallest useful weft program: index, search, print.
//
// For an interactive version with a per-scorer breakdown, see ./cmd/weft.
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

func main() {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	ix := engine.New()
	for _, d := range []engine.Document{
		{Key: "rrf", Text: "reciprocal rank fusion combines rankings", Vector: []float32{1, 0, 0}, Links: []string{"bm25"}, Time: now},
		{Key: "bm25", Text: "bm25 ranks documents by term frequency", Vector: []float32{0.8, 0.6, 0}, Time: now.Add(-72 * time.Hour)},
		{Key: "hnsw", Text: "approximate nearest neighbour graphs", Vector: []float32{0, 1, 0}, Time: now.Add(-24 * time.Hour)},
	} {
		if _, err := ix.Add(d); err != nil {
			log.Fatalf("index %q: %v", d.Key, err)
		}
	}

	txt := text.New(ix)

	// The whole point is this slice. Search and the fuser do not know how many
	// scorers are in it, or what any of them are. Add a fifth here and nothing
	// else in weft changes.
	scorers := []engine.Scorer{
		txt,
		vector.New(ix),
		graph.New(ix, txt),
		recency.NewAt(ix, now),
	}

	q := engine.Query{Text: "fusion ranks", Vector: []float32{1, 0, 0}}

	// One weight per stream, by position. The third entry discounts the graph
	// scorer, which milestone 4 measured as contributing nothing — see its package
	// documentation. Note what this still does not require: the fuser is handed a
	// number for slot three, not the knowledge that slot three is a graph scorer.
	// Plain fusion.Fuse is this same call with every weight at 1.
	fuse := fusion.FuseWeighted(1, 1, 0.1, 1)

	results, err := engine.Search(context.Background(), q, 3, fuse, scorers...)
	if err != nil {
		log.Fatal("search: ", err)
	}
	for i, c := range results {
		d, _ := ix.Doc(c.Doc)
		fmt.Printf("%d. %-6s %.5f\n", i+1, d.Key, c.Score)
	}
}
