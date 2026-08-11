# weft

> The weft thread. The warp threads never touch each other; one weft crosses and binds them all.

A search engine where ranking signals are interchangeable. Go, from scratch, standard library only.

```go
// Every scorer implements this. Fusion knows only this.
type Scorer interface {
    Name() string
    Candidates(ctx context.Context, q Query, k int) ([]Candidate, error)
}

// Knows neither how many scorers there are nor what any of them compute.
func Fuse(streams [][]Candidate, k int) []Candidate
```

## Why

Hybrid search engines started with one signal and bolted the rest on, so fusion ends up a special case: a dedicated code path joins two signals, and a third means rewriting it. That is why graph proximity is not a first-class ranking signal in any engine. weft inverts the order — fusion is the default operation and scorers plug into it, so the fourth scorer costs what the first did.

If you need text + vector hybrid search today, [bleve](https://github.com/blevesearch/bleve) already has BM25, ANN and RRF. weft rests on an architectural hypothesis, not a market gap; [`docs/FINDINGS.md`](docs/FINDINGS.md) records how far it is verified.

## Status

Milestone 1 passed. **Not usable in production:** the index is in memory only and is lost on restart.

| # | Milestone | State |
|---|---|---|
| 1 | Scorer-agnostic fusion | ✅ 3/3 assertions pass |
| 2 | Persistence | Not started |
| 3 | Scale — segment merge, ANN | Not started |
| 4 | Quality — graph contribution to nDCG | Not started |
| 5 | Performance — p99 including GC pauses | Not started |
| 6 | External contribution readiness | Not started |

## Quick start

```bash
go run ./cmd/weft
```

```
query> ranking fusion
  1. rrf        0.03226  text:2  vector:-  graph:-  recency:2
  2. hnsw       0.03200  text:-  vector:-  graph:2  recency:3
  3. bm25       0.03178  text:-  vector:-  graph:1  recency:5
  4. ivf        0.03150  text:-  vector:-  graph:3  recency:4
  5. tfidf      0.01639  text:1  vector:-  graph:-  recency:-
```

Trailing columns are each scorer's rank *before* fusion.

- `tfidf` leads text but lands fifth: no other scorer agreed. One scorer's confidence does not beat consensus.
- `hnsw`, `bm25` and `ivf` are invisible to text — graph traversal found them.
- `-` means no opinion, which costs nothing. `vector:-` is everywhere because the query had no vector; append `@ 0,1,0` and the vector scorer joins. `graph:-` on `rrf` and `tfidf` marks them as traversal seeds, which are excluded ([FINDINGS §2.3](docs/FINDINGS.md)).

Minimal embedding: [`examples/basic`](examples/basic/main.go). Godoc example: `Example` in `pkg/engine`.

## Adding a scorer

Implement `engine.Scorer`; nothing in `engine/` or `fusion/` changes.

```go
func (s *Scorer) Name() string { return "popularity" }

func (s *Scorer) Candidates(ctx context.Context, q engine.Query, k int) ([]engine.Candidate, error) {
    cands := make([]engine.Candidate, 0, s.ix.Len())
    // Score however you like. Any scale — fusion reads rank, not score.
    return engine.TopK(cands, k), nil
}
```

Then add one element at the call site:

```go
scorers := []engine.Scorer{txt, vec, gr, rec, popularity.New(ix)}
results, err := engine.Search(ctx, q, 10, fusion.Fuse, scorers...)
```

`make arch` verifies this mechanically:

- **Fusion is invariant to scorer count** — three and four scorers use the same call expression; compiling is the proof.
- **A new scorer is cheap** — `scorer/recency` is 93 implementation lines against a 100-line budget, and `fusion/` needs no change. Whatever a scorer costs `engine` shows up in `pkg/engine/testdata/engine_api.txt`, which records signatures and member types, not just names ([FINDINGS §1](docs/FINDINGS.md)).
- **Fusion cannot see scorers** — `go list -deps ./pkg/fusion` names no `scorer/*` package.

The third assertion carries the weight. `Fuse` never reads `Candidate.Score`, only rank: BM25 is unbounded, cosine is `[-1,1]`, graph proximity is `(0,1]`, so comparing scores across scorers would need per-scorer normalization — and knowing how to normalize means knowing which scorer produced the score.

## Layout

```
cmd/weft/          interactive demo binary
examples/basic/    minimal library embedding
pkg/
  engine/          shared types, Scorer interface, in-memory index, Search
  fusion/          RRF — imports engine and nothing else
  scorer/
    text/          BM25, ln(1+…) IDF form
    vector/        brute-force cosine
    graph/         seed BFS, 1/(1+hops)
    recency/       1/(1+age/HalfLife)
docs/
  FINDINGS.md      milestone 1 results, known costs, open questions
  DECISIONS.md     decisions expensive to reverse
  DATASETS.md      evaluation dataset survey for milestone 4
```

Dependencies point inward. `engine` imports no weft package; `fusion` imports only `engine`. `engine.Search` takes a `Fuser` function rather than importing `fusion`, so `engine` is ignorant of the fusion strategy as well as of scorers.

## Development

```bash
make            # build + vet + test
make arch       # the three assertions above
make deps       # zero dependencies, and fusion sees no scorer
make run        # interactive demo
make example    # minimal example
```

964 implementation lines, 2,079 test lines, **zero external dependencies**. Go 1.26+.

## Limitations

| Limitation | Detail |
|---|---|
| No persistence | In memory only. Milestone 2. |
| No early termination | The top-k candidate interface forecloses WAND-style skipping. Cost and extension path: [FINDINGS §3.1](docs/FINDINGS.md). |
| Graph seeds unverified | Double counting is fixed; whether `SeedN = 5` and "top n from text" are good choices needs measurement. Milestone 4. |
| Scorers must share one index | `DocID` is index-relative, so scorers built against different indexes fuse unrelated documents. A precondition on `Search`, not a check: [FINDINGS §3.4](docs/FINDINGS.md). |
| No CJK tokenization | Whitespace and punctuation splitting only, so CJK runs collapse into one token. |
| No embedding generation | Vectors are supplied by the caller. |
| No query language | Queries are built through the Go API. |

## License

[Apache License 2.0](LICENSE). Third-party notices: [NOTICE](NOTICE) — there are none.
