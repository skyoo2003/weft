// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"time"
	"unicode"

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
//
// The three guards around the loop are the shape every scorer in this tree
// keeps, and scorer/recency states why each one earns its line: k <= 0 declines
// the work before allocating, the poll inside the loop stops a cancelled query
// walking a table that may be corpus-sized, and the poll after it stops a
// cancellation arriving on the last key from still paying for TopK's O(n log n)
// sort and then reporting success past the deadline.
func (n *nearby) Candidates(ctx context.Context, _ engine.Query, k int) ([]engine.Candidate, error) {
	if k <= 0 {
		return nil, nil
	}
	cands := make([]engine.Candidate, 0, len(n.at))
	for key, pos := range n.at {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id, ok := n.ix.Resolve(key)
		if !ok {
			continue
		}
		cands = append(cands, engine.Candidate{Doc: id, Score: 1 / (1 + math.Abs(pos-n.origin))})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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
		// The three texts are deliberately different lengths. BM25 normalizes by
		// document length, so this is what makes the text stream an ordering
		// rather than a three-way tie — and a tie here would be settled by TopK on
		// DocID, which is insertion order, so the ranking below would say more
		// about the order these three were added than about either signal.
		{Key: "palace", Text: "cafe by the palace"},
		{Key: "harbour", Text: "cafe on the harbour by the water"},
		{Key: "hillside", Text: "cafe up the hillside above the old town wall"},
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
		// Search returns no results and no error when every stream is empty, so
		// the copyable shape checks before indexing.
		if len(results) == 0 {
			fmt.Println("no results")
			return
		}
		d, _ := ix.Doc(results[0].Doc)
		fmt.Printf("from %.0f: %s\n", origin, d.Key)
	}

	// Output:
	// from 0: palace
	// from 11: harbour
}

// ExampleFuser expresses a constraint weft has no field for — match only
// documents whose text holds two words adjacent and in order — and shows why a
// scorer alone does not express it.
//
// The first search is the arrangement that looks right: a constraint scorer
// fused with the text scorer. "tools" holds both query words without holding the
// phrase, the constraint refuses it, and it is in the result anyway — rank fusion
// sums over the streams a document appears in, so absence from one costs it
// nothing in the other. The second search swaps the fuser for one that reads the
// last stream as a restriction, and the same query answers correctly.
func ExampleFuser() {
	ix := engine.New()
	for _, d := range []engine.Document{
		{Key: "survey", Text: "machine learning models rank search results"},
		{Key: "tools", Text: "learning about machine tools and workshop practice"},
	} {
		if _, err := ix.Add(d); err != nil {
			fmt.Println("add:", err)
			return
		}
	}

	txt := text.New(ix)
	// The constraint wraps the text stream rather than sweeping the corpus:
	// engine.Posting carries no positions, so deciding a phrase means decoding a
	// document, and wrapping bounds that to the candidates the inner scorer
	// already found.
	ph := &phrase{inner: txt, ix: ix, depth: 8}
	q := engine.Query{Text: "machine learning"}

	for _, f := range []struct {
		name string
		fuse engine.Fuser
	}{{"Fuse", fusion.Fuse}, {"restricting", restrictFuse}} {
		results, err := engine.Search(context.Background(), q, 4, f.fuse, txt, ph)
		if err != nil {
			fmt.Println("search:", err)
			return
		}
		fmt.Printf("%-12s", f.name)
		for _, c := range results {
			d, _ := ix.Doc(c.Doc)
			fmt.Printf(" %s", d.Key)
		}
		fmt.Println()
	}

	// Output:
	// Fuse         survey tools
	// restricting  survey
}

// ExampleWithTokenizer replaces the tokenizer, which is what a caller whose
// language the default splits wrongly has to do.
//
// engine.Tokenize cuts only where a rune is neither a letter nor a digit, so
// Korean is split at spaces and nowhere else: the document holds "검색엔진을"
// and the query is "검색엔진", and those are two different terms — the query
// finds nothing, with nothing to report. The replacement below indexes character
// bigrams over Hangul runs. It is ten lines and it is not morphological
// analysis; weft ships no second tokenizer, because a seam with a menu in it is
// not a seam.
//
// One function value serves both sides. Index.Add and scorer/text each reach it
// through Index.Tokenize, so there is no way to configure index time and query
// time apart — and a directory committed with one tokenizer and opened with
// another is refused rather than answering nothing (ErrTokenizerMismatch).
func ExampleWithTokenizer() {
	// Character bigrams for a token that starts with a Hangul syllable, and the
	// default's own answer for everything else.
	bigrams := func(s string) []string {
		var out []string
		for _, tok := range engine.Tokenize(s) {
			r := []rune(tok)
			if len(r) < 2 || !unicode.Is(unicode.Hangul, r[0]) {
				out = append(out, tok)
				continue
			}
			for i := 0; i+1 < len(r); i++ {
				out = append(out, string(r[i:i+2]))
			}
		}
		return out
	}

	// The decoy shares no bigram with the target, so a hit on the target is the
	// query being answered rather than the tokenizer matching anything Korean.
	docs := []engine.Document{
		{Key: "target", Text: "검색엔진을 만들었다"},
		{Key: "decoy", Text: "고양이가 창밖을 바라본다"},
	}
	q := engine.Query{Text: "검색엔진"}

	for _, tc := range []struct {
		name string
		opts []engine.Option
	}{
		{"default", nil},
		{"replaced", []engine.Option{engine.WithTokenizer(bigrams)}},
	} {
		ix := engine.New(tc.opts...)
		for _, d := range docs {
			if _, err := ix.Add(d); err != nil {
				fmt.Println("add:", err)
				return
			}
		}
		results, err := engine.Search(context.Background(), q, 2, fusion.Fuse, text.New(ix))
		if err != nil {
			fmt.Println("search:", err)
			return
		}
		if len(results) == 0 {
			fmt.Printf("%s: no results\n", tc.name)
			continue
		}
		d, _ := ix.Doc(results[0].Doc)
		fmt.Printf("%s: %s\n", tc.name, d.Key)
	}

	// Output:
	// default: no results
	// replaced: target
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
	if err := ix.Commit(context.Background(), dir); err != nil {
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
