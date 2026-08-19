// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"context"
	"fmt"
	"math"
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

// nearby ranks documents by distance from an origin, and neither half of its
// input fits in weft's types — which is the ordinary case for a scorer written
// outside this module.
//
// The positions are the caller's own table, joined to the corpus on
// Document.Key through Index.Resolve, because engine.Document has no field for
// them and does not need one. The origin changes per search, and engine.Query
// has no field for that either, so it is bound at construction and a scorer is
// built per query — the same shape recency.NewAt uses to pin a clock.
type nearby struct {
	ix     *engine.Index
	at     map[string]float64 // Document.Key -> position
	origin float64
}

func nearbyFrom(ix *engine.Index, at map[string]float64, origin float64) *nearby {
	return &nearby{ix: ix, at: at, origin: origin}
}

func (n *nearby) Name() string { return "nearby" }

// Candidates walks the caller's table rather than the corpus: its keys are the
// only documents this scorer has an opinion about, and a key the index never saw
// is skipped rather than rejected. The score scale is arbitrary on purpose —
// fusion reads rank, never Candidate.Score.
func (n *nearby) Candidates(ctx context.Context, _ engine.Query, k int) ([]engine.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cands := make([]engine.Candidate, 0, len(n.at))
	for key, pos := range n.at {
		id, ok := n.ix.Resolve(key)
		if !ok {
			continue
		}
		cands = append(cands, engine.Candidate{Doc: id, Score: 1 / (1 + math.Abs(pos-n.origin))})
	}
	// TopK breaks ties on DocID, so map iteration order cannot reach the ranking.
	return engine.TopK(cands, k), nil
}

// ExampleScorer adds a fifth signal from outside the module: data weft does not
// store, and an input that changes per query.
//
// Two searches, the same corpus and the same query text, differing only in the
// origin the scorer was built with — and the winner changes.
func ExampleScorer() {
	ix := engine.New()
	for _, d := range []engine.Document{
		{Key: "palace", Text: "cafe by the palace"},
		{Key: "harbour", Text: "cafe on the harbour"},
		{Key: "hillside", Text: "cafe up the hillside"},
	} {
		if _, err := ix.Add(d); err != nil {
			fmt.Println("add:", err)
			return
		}
	}

	// Held here, not by weft. Commit would not carry it, so it is rebuilt after
	// every Open — Key still names the same document, which is what makes that
	// safe.
	at := map[string]float64{"palace": 0, "harbour": 10, "hillside": 14}

	txt := text.New(ix)
	q := engine.Query{Text: "cafe"}

	for _, origin := range []float64{0, 11} {
		// Fused at 3 and displayed at 1. A signal orthogonal to the others
		// surfaces documents they rank below their own cut, so at a shared k it
		// would be outvoted before it could say anything.
		results, err := engine.Search(context.Background(), q, 3, fusion.Fuse,
			txt, nearbyFrom(ix, at, origin))
		if err != nil {
			fmt.Println("search:", err)
			return
		}
		d, _ := ix.Doc(results[0].Doc)
		fmt.Printf("from %.0f: %s\n", origin, d.Key)
	}

	// Output:
	// from 0: palace
	// from 11: harbour
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
