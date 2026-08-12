package engine_test

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/scorer/graph"
	"github.com/skyoo2003/weft/pkg/scorer/recency"
	"github.com/skyoo2003/weft/pkg/scorer/text"
	"github.com/skyoo2003/weft/pkg/scorer/vector"
)

// Example indexes four documents and searches them with four scorers at once.
//
// The line worth looking at is the Search call. It names no scorer and no scorer
// count: the slice could hold one scorer or ten, and neither Search nor
// fusion.Fuse would be written differently. Adding a fifth scorer means adding a
// fifth element here.
func Example() {
	ix := engine.New()

	// A pinned clock keeps the recency scorer, and so this example's output,
	// reproducible.
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	for _, d := range []engine.Document{
		{
			Key:    "rrf",
			Text:   "reciprocal rank fusion combines rankings",
			Vector: []float32{1, 0, 0},
			Links:  []string{"bm25"},
			Time:   now,
		},
		{
			Key:    "bm25",
			Text:   "bm25 ranks documents by term frequency",
			Vector: []float32{0.8, 0.6, 0},
			Time:   now.Add(-72 * time.Hour),
		},
		{
			Key:    "hnsw",
			Text:   "approximate nearest neighbour graphs",
			Vector: []float32{0, 1, 0},
			Time:   now.Add(-24 * time.Hour),
		},
		// No vector and no links: only the text and recency scorers can see it.
		{Key: "changelog", Text: "release notes", Time: now},
	} {
		if _, err := ix.Add(d); err != nil {
			fmt.Println("add:", err)
			return
		}
	}

	txt := text.New(ix)
	scorers := []engine.Scorer{
		txt,
		vector.New(ix),
		graph.New(ix, txt), // seeded by any scorer, here the text one
		recency.NewAt(ix, now),
	}

	q := engine.Query{Text: "fusion ranks", Vector: []float32{1, 0, 0}}

	results, err := engine.Search(context.Background(), q, 3, fusion.Fuse, scorers...)
	if err != nil {
		fmt.Println("search:", err)
		return
	}
	for i, c := range results {
		d, _ := ix.Doc(c.Doc)
		fmt.Printf("%d. %s\n", i+1, d.Key)
	}

	// Output:
	// 1. rrf
	// 2. bm25
	// 3. hnsw
}

// ExampleIndex_Commit is the persistence round trip: commit, restart, search.
// Open returns an ordinary Index — scorers built on it neither know nor care
// that it came from disk.
func ExampleIndex_Commit() {
	dir, err := os.MkdirTemp("", "weft-example")
	if err != nil {
		fmt.Println("tempdir:", err)
		return
	}
	defer os.RemoveAll(dir)

	ix := engine.New()
	for _, d := range []engine.Document{
		{Key: "weft", Text: "the thread that crosses and binds the warp"},
		{Key: "warp", Text: "the threads a weft crosses"},
	} {
		if _, err := ix.Add(d); err != nil {
			fmt.Println("add:", err)
			return
		}
	}
	if err := ix.Commit(dir); err != nil {
		fmt.Println("commit:", err)
		return
	}

	// The restart: everything above is gone, only dir remains.
	restored, err := engine.Open(dir)
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	results, err := engine.Search(context.Background(),
		engine.Query{Text: "crosses binds"}, 1, fusion.Fuse, text.New(restored))
	if err != nil {
		fmt.Println("search:", err)
		return
	}
	d, _ := restored.Doc(results[0].Doc)
	fmt.Println(d.Key)

	// Output:
	// weft
}
