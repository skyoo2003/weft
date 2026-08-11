// Command weft is an interactive demo of scorer-agnostic fusion.
//
// It indexes a small built-in corpus and answers queries with four scorers at
// once, printing each scorer's own rank beside the fused result so you can see
// what fusion actually did. A document that only one scorer can see still
// surfaces; a scorer with no opinion contributes nothing and costs nothing.
//
// Vectors in the corpus are hand-assigned. Generating embeddings is out of
// scope for weft, so a demo cannot compute them.
//
//	go run ./cmd/weft
//	query> ranking fusion
//	query> nearest neighbour @ 0,1,0
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/scorer/graph"
	"github.com/skyoo2003/weft/pkg/scorer/recency"
	"github.com/skyoo2003/weft/pkg/scorer/text"
	"github.com/skyoo2003/weft/pkg/scorer/vector"
)

// demoNow pins the clock. With time.Now the recency ranks would drift between
// runs and the demo would stop being reproducible.
var demoNow = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func day(n int) time.Time { return demoNow.AddDate(0, 0, -n) }

// The three vector dimensions stand in for topics: [ranking, vectors, graphs].
var corpus = []engine.Document{
	{Key: "bm25", Text: "bm25 ranks documents by term frequency and document length", Vector: []float32{1, 0, 0}, Links: []string{"tfidf"}, Time: day(400)},
	{Key: "tfidf", Text: "tf idf term weighting for ranking", Vector: []float32{0.9, 0, 0.1}, Links: []string{"bm25"}, Time: day(800)},
	{Key: "rrf", Text: "reciprocal rank fusion combines rankings from several retrievers", Vector: []float32{0.8, 0.3, 0}, Links: []string{"bm25", "hnsw"}, Time: day(30)},
	{Key: "hnsw", Text: "hierarchical navigable small world graphs for approximate nearest neighbour search", Vector: []float32{0, 0.7, 0.7}, Links: []string{"ivf"}, Time: day(120)},
	{Key: "ivf", Text: "inverted file index partitions vectors into clusters", Vector: []float32{0, 1, 0}, Links: []string{"hnsw"}, Time: day(200)},
	{Key: "pagerank", Text: "pagerank scores nodes by random walk probability over a link graph", Vector: []float32{0.3, 0, 0.9}, Links: []string{"bfs"}, Time: day(600)},
	{Key: "bfs", Text: "breadth first search finds shortest hop distance in a graph", Vector: []float32{0, 0, 1}, Links: []string{"pagerank"}, Time: day(900)},
	// No vector, no links: only text and recency can see this one.
	{Key: "changelog", Text: "release notes for the current version", Time: day(1)},
}

func main() {
	k := flag.Int("k", 5, "how many results to show")
	flag.Parse()

	ix := engine.New()
	for _, d := range corpus {
		if _, err := ix.Add(d); err != nil {
			fmt.Fprintf(os.Stderr, "index %q: %v\n", d.Key, err)
			os.Exit(1)
		}
	}

	txt := text.New(ix)
	scorers := []engine.Scorer{
		txt,
		vector.New(ix),
		graph.New(ix, txt), // seeded by any scorer, here the text one
		recency.NewAt(ix, demoNow),
	}

	fmt.Printf("weft — %d documents, %d scorers. Query syntax: TEXT [@ v1,v2,v3]. Ctrl-D to quit.\n\n", ix.Len(), len(scorers))

	in := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("query> ")
		if !in.Scan() {
			fmt.Println()
			break
		}
		q, err := parseQuery(in.Text())
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %v\n\n", err)
			continue
		}
		if q.Text == "" && len(q.Vector) == 0 {
			continue
		}
		if err := run(ix, scorers, q, *k); err != nil {
			fmt.Fprintf(os.Stderr, "  %v\n\n", err)
		}
	}
	if err := in.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
}

// run searches and prints the fused ranking with a per-scorer breakdown.
func run(ix *engine.Index, scorers []engine.Scorer, q engine.Query, k int) error {
	ctx := context.Background()

	// The fused ranking. Note that this call names no scorer and no count.
	results, err := engine.Search(ctx, q, k, fusion.Fuse, scorers...)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Print("  no results\n\n")
		return nil
	}

	// Each scorer is asked again on its own, purely so the breakdown can show
	// where each document placed before fusion.
	breakdown := make([]map[engine.DocID]int, len(scorers))
	for i, s := range scorers {
		if breakdown[i], err = ranksOf(ctx, s, q, k); err != nil {
			return err
		}
	}

	for rank, c := range results {
		d, _ := ix.Doc(c.Doc)
		fmt.Printf("  %d. %-10s %.5f  ", rank+1, d.Key, c.Score)
		for i, s := range scorers {
			fmt.Printf("%s:%s  ", s.Name(), place(breakdown[i][c.Doc]))
		}
		fmt.Println()
	}
	fmt.Println()
	return nil
}

// ranksOf maps each document a scorer returned to its 1-based position.
func ranksOf(ctx context.Context, s engine.Scorer, q engine.Query, k int) (map[engine.DocID]int, error) {
	cands, err := s.Candidates(ctx, q, k)
	if err != nil {
		return nil, fmt.Errorf("scorer %s: %w", s.Name(), err)
	}
	ranks := make(map[engine.DocID]int, len(cands))
	for i, c := range cands {
		ranks[c.Doc] = i + 1
	}
	return ranks, nil
}

// place renders a rank, or a dash for a scorer that did not return the document.
func place(rank int) string {
	if rank == 0 {
		return "-"
	}
	return strconv.Itoa(rank)
}

// parseQuery splits "some text @ 1,0,0" into text and an optional vector. The
// vector is typed by hand because weft does not generate embeddings.
func parseQuery(line string) (engine.Query, error) {
	text, vec, hasVec := strings.Cut(line, "@")
	q := engine.Query{Text: strings.TrimSpace(text)}
	if !hasVec {
		return q, nil
	}
	for _, f := range strings.Split(vec, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(f), 32)
		if err != nil {
			return engine.Query{}, fmt.Errorf("bad vector component %q: %w", strings.TrimSpace(f), err)
		}
		q.Vector = append(q.Vector, float32(v))
	}
	return q, nil
}
