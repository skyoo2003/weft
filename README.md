# weft

[![CI](https://github.com/skyoo2003/weft/actions/workflows/ci.yml/badge.svg)](https://github.com/skyoo2003/weft/actions/workflows/ci.yml)

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

Milestones 1 and 2 passed. **Not usable in production:** every commit rewrites the whole corpus and every open reads it all into memory, so the corpus must fit in RAM.

| # | Milestone | State |
|---|---|---|
| 1 | Scorer-agnostic fusion | ✅ 3/3 assertions pass |
| 2 | Persistence | ✅ restores identically, commits atomically |
| 3 | Scale — segment merge, ANN | Not started |
| 4 | Quality — graph contribution to nDCG | Not started |
| 5 | Performance — p99 including GC pauses | Not started |
| 6 | External contribution readiness | Not started |

No tag yet; the first will be `v0.1.0`. Until then `go get` resolves to a pseudo-version naming a commit, which is the honest state — a tag would give you a shorter name without changing anything the warning above says. [CHANGELOG](CHANGELOG.md) is where a version tells you whether you have work to do, and it records three things only: `engine`'s exported API, the on-disk format version, and the minimum Go version. The milestone numbers in this table are not among them.

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
- `-` means the document is absent from that scorer's stream, for one of three reasons. No opinion, which costs nothing: `vector:-` is everywhere because the query had no vector, so append `@ 0,1,0` and the vector scorer joins. Deliberately withheld: `graph:-` on `rrf` and `tfidf` marks them as traversal seeds, which are excluded ([FINDINGS §2.3](docs/FINDINGS.md)). Or simply below the cut — every scorer is asked for `k`, so `recency:-` on `tfidf` is truncation, not abstention. Raise `-k` and it fills in.

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
- **A new scorer is cheap** — `scorer/recency` is 99 implementation lines against a 100-line budget, and `fusion/` needs no change. Whatever a scorer costs `engine` shows up in `pkg/engine/testdata/engine_api.txt`, which records member types, parameter and result types, declaration order, and whether a struct has become unkeyed-literal-hostile by gaining an unexported field — everything a caller has to satisfy, and nothing that only spelling would change ([FINDINGS §1](docs/FINDINGS.md)).
- **Fusion cannot see scorers** — `go list -deps ./pkg/fusion` names no `scorer/*` package.

The third assertion carries the weight. `Fuse` never reads `Candidate.Score`, only rank: BM25 is unbounded, cosine is `[-1,1]`, graph proximity is `(0,1]`, so comparing scores across scorers would need per-scorer normalization — and knowing how to normalize means knowing which scorer produced the score.

## Layout

```
cmd/weft/          interactive demo binary
examples/basic/    minimal library embedding
pkg/
  engine/          shared types, Scorer interface, in-memory index, Search,
                   segment format, Commit and Open
  fusion/          RRF — imports engine and nothing else
  scorer/
    text/          BM25, ln(1+…) IDF form
    vector/        brute-force cosine
    graph/         seed BFS, 1/(1+hops)
    recency/       1/(1+age/HalfLife)
docs/
  FINDINGS.md      milestone 1 and 2 results, known costs, open questions
  FORMAT.md        the on-disk format, version 1
  DECISIONS.md     decisions expensive to reverse
  DATASETS.md      evaluation dataset survey for milestone 4
  RESEARCH.md      one round of community and competitive research
```

Dependencies point inward. `engine` imports no weft package; `fusion` imports only `engine`. `engine.Search` takes a `Fuser` function rather than importing `fusion`, so `engine` is ignorant of the fusion strategy as well as of scorers.

## Development

```bash
make            # fmt + build + vet + test -race — the same gate CI runs
make arch       # the three assertions above
make deps       # zero dependencies, and fusion sees no scorer
make run        # interactive demo
make example    # minimal example
```

2,396 implementation lines under `pkg/`, 4,044 test lines, **zero external dependencies**. Go 1.26+.

## Limitations

| Limitation | Detail |
|---|---|
| Corpus must fit in memory | `Commit` rewrites the whole corpus and `Open` loads all of it. Incremental segments and lazy loading are milestone 3: [FORMAT.md §8](docs/FORMAT.md). |
| No deletion | Documents can be added, never removed. Tombstones and DocID namespacing are one design problem, taken together in milestone 3: [FINDINGS, milestone 2 §4](docs/FINDINGS.md). |
| Durability stops at fsync | Atomic against process death; best-effort against power loss, with no platform write barrier: [FORMAT.md §6](docs/FORMAT.md). |
| No early termination | The top-k candidate interface forecloses WAND-style skipping. Cost and extension path: [FINDINGS §3.1](docs/FINDINGS.md). |
| Graph seeds unverified | Double counting is fixed; whether `SeedN = 5` and "top n from text" are good choices needs measurement. Milestone 4. |
| Scorers must share one index | `DocID` is index-relative, so scorers built against different indexes fuse unrelated documents. A precondition on `Search`, not a check: [FINDINGS §3.4](docs/FINDINGS.md). |
| No CJK tokenization | Whitespace and punctuation splitting only, so CJK runs collapse into one token. |
| No embedding generation | Vectors are supplied by the caller. |
| No query language | Queries are built through the Go API. |

## Contributing

`make all` is the gate, and CI runs that same target. What the three assertions mean is [Adding a scorer](#adding-a-scorer), above. How to contribute — which of them a test decides for you, what the 100-line figure does and does not enforce, what a pull request should say, why a decision is recorded — is in [CONTRIBUTING.md](CONTRIBUTING.md).

Vulnerabilities go to [SECURITY.md](SECURITY.md), not to the issue tracker. Behavior is covered by the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

[Apache License 2.0](LICENSE). Third-party notices: [NOTICE](NOTICE) — there are none.
