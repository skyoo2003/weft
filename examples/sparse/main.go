// SPDX-License-Identifier: Apache-2.0

// Command sparse is about the documents a scorer cannot see.
//
// Two things are shown at once, and they are opposite sides of one claim:
//
//   - A document with no vector and no links still surfaces. Text and recency
//     can see it, the other two cannot, and fusion does not require agreement.
//   - A scorer with nothing to say contributes nothing and costs nothing. The
//     second query below carries no vector, so the vector scorer returns an
//     empty stream — not an error, and not a row of zeros.
//
//	go run ./examples/sparse
//
// The failure this rules out is the one weft would have if fusion averaged
// scores instead of ranks: a document only one scorer knows about would be
// dragged down by every scorer that has never heard of it.
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

// changelog is the point of this corpus: no Vector, no Links. Only two of the
// four scorers can produce it at all.
var corpus = []engine.Document{
	{Key: "hnsw", Text: "hierarchical navigable small world graphs for nearest neighbour search", Vector: []float32{0, 0.7, 0.7}, Links: []string{"ivf"}, Time: day(120)},
	{Key: "ivf", Text: "inverted file index partitions vectors into clusters", Vector: []float32{0, 1, 0}, Links: []string{"hnsw"}, Time: day(200)},
	{Key: "rrf", Text: "reciprocal rank fusion combines rankings from several retrievers", Vector: []float32{0.8, 0.3, 0}, Links: []string{"hnsw"}, Time: day(30)},
	{Key: "changelog", Text: "release notes for the current version of the search index", Time: day(1)},
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

	// Two queries over one corpus and one set of scorers. The second carries no
	// vector, which is how a scorer comes to have no opinion at all.
	report(ix, scorers, "with a vector", engine.Query{Text: "search index", Vector: []float32{0, 1, 0}})
	report(ix, scorers, "no vector", engine.Query{Text: "search index"})
}

// report searches and prints how many candidates each scorer produced, then the
// fused ranking.
func report(ix *engine.Index, scorers []engine.Scorer, label string, q engine.Query) {
	const k = 4

	var streams [][]engine.Candidate
	fuse := func(s [][]engine.Candidate, k int) []engine.Candidate {
		streams = s
		return fusion.Fuse(s, k)
	}

	results, err := engine.Search(context.Background(), q, k, fuse, scorers...)
	if err != nil {
		log.Fatal("search: ", err)
	}

	fmt.Printf("%s — query %q @ %v\n", label, q.Text, q.Vector)
	for i, s := range scorers {
		fmt.Printf("  %-8s produced %d\n", s.Name(), len(streams[i]))
	}
	for rank, c := range results {
		d, _ := ix.Doc(c.Doc)
		fmt.Printf("  %d. %-10s %.5f\n", rank+1, d.Key, c.Score)
	}
	fmt.Println()
}
