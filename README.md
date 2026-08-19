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

Milestones 1 through 5 are done. **Not usable in production:** documents cannot be deleted, a commit holds a write lock for as long as it takes — 11 seconds for a 20,000-document batch, with reads queueing behind it — and sustained query load collapses at 27 queries per second on the machine measured rather than degrading.

| # | Milestone | State |
| --- | --- | --- |
| 1 | Scorer-agnostic fusion | ✅ 3/3 assertions pass |
| 2 | Persistence | ✅ restores identically, commits atomically |
| 3 | Scale — segment merge, lazy loading, ANN | ✅ both paths hold. `Open` 979 ms → 54 ms, ANN recall@10 0.992 at 4.6× the query speed — but the vector gain is smaller than hoped |
| 4 | Quality — graph contribution to nDCG | ✅ measured, and the answer is **no** — see below |
| 5 | Performance — p99 including GC pauses | ✅ p99 **108.193 ms**, of which the collector is 411 µs (0.38%); 1.88× bleve v2.6.0 against a 10× bar |
| 6 | External contribution readiness | ✅ measured — two outside developers added a signal from the documentation alone, which found three documentation defects: [ADOPTION.md](docs/ADOPTION.md) |

No tag yet; the first will be `v0.1.0`. Until then `go get` resolves to a pseudo-version naming a commit, which is the honest state — a tag would give you a shorter name without changing anything the warning above says. [CHANGELOG](CHANGELOG.md) is where a version tells you whether you have work to do, and it records three things only: the exported API of every package under `pkg/`, the on-disk format version, and the minimum Go version. The milestone numbers in this table are not among them.

Milestone 4 ran ahead of 3 because the dataset fits in memory and the project's second falsification condition was waiting on it.

Two things milestone 5 owes and has not paid, stated here rather than left in a findings document: its headline is **one run, not the three repetitions [PERF.md](docs/PERF.md) requires**, and the arm you would actually deploy — `text + vector` — **has no published tail**, because at four times the cost per query 10,000 samples take over five hours. The published p99 is comparable to bleve's; it is not the number a `text + vector` user will see.

### Published numbers

TREC-COVID, 50 queries, 171,332 documents, 579,719 in-corpus citation edges from Semantic Scholar, 148,232 SPECTER2 vectors. nDCG@10, paired bootstrap over 10,000 resamples.

| Arm | nDCG@10 |
| --- | --- |
| `text` (BM25) | 0.5826 |
| `text + vector` | **0.6233** |
| `text + vector + graph` | 0.5005 |

**Graph proximity does not improve ranking.** Under equal-weight fusion it costs 0.1227 nDCG@10 (95% CI [−0.1550, −0.0909]), and across 28 configurations of the RRF rank constant and fusion depth the sign never flips and no interval reaches zero.

**But that −0.1227 belonged to the fusion operator, not to the graph.** Giving the graph stream half a vote instead of a full one erases all but 0.0019 of the regression. No weight in the tested grid beats the baseline, and from 0.1 downward the arm *is* the baseline, delta exactly zero — see [EVAL.md](docs/EVAL.md) section 5.11 for what that is and is not entitled to claim. So the accurate statement is the flatter one: the graph signal is not harmful information, it is not information.

| Graph stream weight | nDCG@10 | Delta vs baseline |
| --- | --- | --- |
| 1.0 | 0.5005 | −0.1227 |
| 0.5 | 0.6214 | −0.0019 |
| ≤0.1 | 0.6233 | +0.0000, converged to baseline |

**Equal weighting is a ranking decision RRF makes silently on every query**, and here it cost two orders of magnitude more than the signal being evaluated. So `fusion.FuseWeighted` shipped:

```go
// Trust the graph stream less, without fusion learning what a graph scorer is.
fuse := fusion.FuseWeighted(1, 1, 0.1)
results, err := engine.Search(ctx, q, 10, fuse, txt, vec, gr)
```

Weights index by stream *position*, not scorer kind — the caller already fixed that order — so this does not compromise scorer-agnosticism, and `go list -deps ./pkg/fusion` still names no scorer. `Fuse` is unchanged and its unweighted path is bit-identical. Where the weights should come from is the open question: hand-tuning per corpus reintroduces exactly the burden this design avoids, so `FuseWeighted`'s docs say a caller without its own measurement should use `Fuse`. [FINDINGS milestone 4 §7](docs/FINDINGS.md).

`scorer/graph` is **kept rather than deleted**, with the verdict at the top of its package documentation. The falsification condition said a worthless signal goes; the weight sweep then showed the scorer is inert rather than harmful, and that deleting it would cut the milestone 1 assertions from four signals to three. [D-005](docs/DECISIONS.md) argues both sides.

What was falsified is one construction — BFS hop distance, seeded from the text top 5 — not the idea that citation structure carries signal. The graph stream returns thousands of candidates carrying 3 to 16 distinct scores, so it has almost no internal ranking to contribute at any weight.

Two supporting results:

- **BM25 matches `rank_bm25` to 4.44e-16**, and nDCG matches `pytrec_eval`'s `ndcg_cut_10` on 12 discriminating fixtures. Both checks ran before any arm number was published, and the second found the plan's own nDCG definition to be wrong.
- **BM25 alone scores 0.5826** where BEIR reports ~0.656 for Anserini with stemming and a stopword list — the same order, from a whitespace tokenizer.

Full measurement design, coverage, sweep and reasons to doubt the numbers: [EVAL.md](docs/EVAL.md). Reproduce with `make eval`.

## Quick start

```bash
go run ./cmd/weft
```

```text
query> ranking fusion
  1. rrf        0.03226  text:2  vector:-  graph:-  recency:2
  2. hnsw       0.01749  text:-  vector:-  graph:2  recency:3
  3. ivf        0.01721  text:-  vector:-  graph:3  recency:4
  4. bm25       0.01702  text:-  vector:-  graph:1  recency:5
  5. tfidf      0.01639  text:1  vector:-  graph:-  recency:-
```

Trailing columns are each scorer's rank *before* fusion. The demo fuses with `FuseWeighted(1, 1, 0.1, 1)`, discounting the graph stream to a tenth of a vote, because that is what the [limitations](#limitations) below tell you to do and a demo that ignored its own project's advice would be worth less than no demo.

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

**Three things that are not obvious from the skeleton.** Each cost a trial subject time in [ADOPTION.md](docs/ADOPTION.md), and `ExampleScorer` in `pkg/engine` is all three in one compiling program.

*Your data does not go in `engine.Document`.* Those five fields are what weft's own scorers read, and you cannot add a sixth from outside the module. Keep your table keyed by `Document.Key` — a map, a database, whatever you have — and call `Index.Resolve` to turn a `Key` into a `DocID`. `Commit` does not carry it, so rebuild after `Open`; `Key` still names the same document, which is what makes the rebuild safe.

*An input that changes per query does not go in `engine.Query` either.* Bind it when you construct the scorer and construct one per search — `recency.NewAt(ix, now)` is that shape with a clock. A scorer value is small; this costs an allocation, not corpus work. Do not reuse `Query.Seeds`, which `scorer/graph` reads.

*Fuse deeper than you display.* `Search`'s `k` is both the per-scorer request size and the result size. A signal orthogonal to the built-in ones surfaces documents they rank below their own cut, so at a shared `k` it appears in one stream only — and RRF is built so a single vote does not win. Ask for more than you show and slice.

`make arch` verifies this mechanically:

- **Fusion is invariant to scorer count** — three and four scorers use the same call expression; compiling is the proof.
- **A new scorer is cheap** — `scorer/recency` is 99 implementation lines against a 100-line budget, and `fusion/` needs no change. Whatever a scorer costs `engine` shows up in `pkg/engine/testdata/engine_api.txt`, which records member types, parameter and result types, declaration order, the package clause, the value a constant is declared from, and whether a struct has become unkeyed-literal-hostile by gaining an unexported field — everything a caller has to satisfy, and nothing that only spelling would change ([FINDINGS §1](docs/FINDINGS.md)).
- **Fusion cannot see scorers** — `go list -deps ./pkg/fusion` names no `scorer/*` package.

The third assertion carries the weight. `Fuse` never reads `Candidate.Score`, only rank: BM25 is unbounded, cosine is `[-1,1]`, graph proximity is `(0,1]`, so comparing scores across scorers would need per-scorer normalization — and knowing how to normalize means knowing which scorer produced the score.

## Layout

```text
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
internal/
  eval/            nDCG@10, arm runner, paired bootstrap, dataset readers
  loadgen/         open-loop load driver, quantiles, GC and rusage accounting
cmd/weft-eval/     prepare / build / diagnose / run / sweep / weights / recall / bench
bench/             separate module: the bleve comparison, so weft keeps zero deps
docs/
  FINDINGS.md      every milestone's result, known costs, open questions
  FORMAT.md        the on-disk format, versions 1 to 3
  DECISIONS.md     decisions expensive to reverse
  DATASETS.md      evaluation dataset survey for milestone 4
  EVAL.md          how milestone 4's numbers were produced, and why to doubt them
  PERF.md          how milestone 5's latency numbers are produced, and the load-point rule
  ADOPTION.md      how milestone 6 tested whether the documentation is enough
  RESEARCH.md      one round of community and competitive research
  testing/         TDD evidence per milestone
```

`internal/eval` is under `internal/` on purpose: an evaluation harness is not part of the library contract, and keeping it out of `pkg/` leaves `engine`'s exported API — and the golden file guarding it — untouched by the measurement. It uses the standard library only, so `make deps` still prints one module.

Dependencies point inward. `engine` imports no weft package; `fusion` imports only `engine`. `engine.Search` takes a `Fuser` function rather than importing `fusion`, so `engine` is ignorant of the fusion strategy as well as of scorers.

## Development

```bash
make            # fmt + build + vet + test -race — needs only the Go toolchain
make arch       # the three assertions above
make deps       # zero dependencies, and fusion sees no scorer
make run        # interactive demo
make example    # minimal example
make eval       # milestone 4's published nDCG table (needs a prepared corpus)
make eval-full  # adds the graph tie analysis and the 28-configuration sweep
make bench      # milestone 5's latency ladder (needs a prepared corpus, ~90 min)
```

CI runs `make all` plus five targets kept out of it because each costs a tool to install or a minute of wall clock: `make spdx`, `make bench-build`, `make lint`, `make lint-docs`, and `make fuzz` — the last being 30 seconds each against the two segment-decoder fuzz targets, which is where a hostile file would land. [CONTRIBUTING](CONTRIBUTING.md#the-gate) has the detail.

`make eval` needs data that is not in the repository. [EVAL.md §7](docs/EVAL.md) lists the downloads and the one-time `weft-eval prepare` step.

2,753 implementation lines under `pkg/`, 5,138 test lines, **zero external dependencies**. Go 1.26+. The evaluation harness in `internal/eval` and `cmd/weft-eval` is another 3,753 and 2,851; it ships no API and is not part of the library.

## Limitations

| Limitation | Detail |
| --- | --- |
| Sustained throughput collapses rather than degrades | At 27 queries/s — its own sequential rate — p50 goes 39 ms to 1.27 s, 14% of queries are shed and RSS goes 126 to 853 MiB. The wall is live heap under concurrency: every candidate decodes a whole record. Usable throughput is somewhere in 13.6–27.3/s: [FINDINGS milestone 5 §3.2](docs/FINDINGS.md). |
| A commit blocks reads for as long as it takes | 11.063 s for a 20,000-document batch, and a read arriving inside that window waited 12.539 s — 150× the p50 beside it. Nothing bounds the window and `Commit` takes no context: [FINDINGS milestone 5 §5](docs/FINDINGS.md). |
| No deletion | Documents can be added, never removed. Tombstones and DocID namespacing are one design problem and neither is built: [FINDINGS, milestone 2 §4](docs/FINDINGS.md). |
| Caller-held scorer data is not persisted | A signal whose data is not an `engine.Document` field lives in your program, so `Commit` does not write it and `Open` does not restore it. Rebuild it keyed by `Document.Key` after every open. |
| Durability stops at fsync | Atomic against process death; best-effort against power loss, with no platform write barrier: [FORMAT.md §6](docs/FORMAT.md). |
| No early termination | The top-k candidate interface forecloses WAND-style skipping. Cost and extension path: [FINDINGS §3.1](docs/FINDINGS.md). |
| **Graph proximity measured worthless** | +0.0000 nDCG@10 at its best fusion weight — no weight in the tested grid beats the baseline, and at 0.1 and below the arm is the baseline exactly — and −0.1227 if fused at equal weight. Kept for the milestone 1 assertions and marked in its package doc: [D-005](docs/DECISIONS.md). Do not enable `scorer/graph` expecting quality, and weight it down if you enable it at all. |
| Fusion weights have no source | `FuseWeighted` exists, but nothing decides what the weights should be. Hand-tuning per corpus reintroduces the per-deployment burden this design avoids; learning them from judgments is unbuilt. Use `Fuse` unless you have measured your own ([FINDINGS milestone 4 §7](docs/FINDINGS.md)). |
| Scorers must share one index | `DocID` is index-relative, so scorers built against different indexes fuse unrelated documents. A precondition on `Search`, not a check: [FINDINGS §3.4](docs/FINDINGS.md). |
| No CJK tokenization | Whitespace and punctuation splitting only, so CJK runs collapse into one token. |
| No embedding generation | Vectors are supplied by the caller. |
| No query language | Queries are built through the Go API. |

## Contributing

`make all` is the gate, and CI runs that same target. What the three assertions mean is [Adding a scorer](#adding-a-scorer), above. How to contribute — which of them a test decides for you, what the 100-line figure does and does not enforce, what a pull request should say, why a decision is recorded — is in [CONTRIBUTING.md](CONTRIBUTING.md).

| | |
| --- | --- |
| Where to take a bug, a proposal or a question | [SUPPORT.md](SUPPORT.md) |
| A vulnerability — **not** the issue tracker | [SECURITY.md](SECURITY.md) |
| Behavior in this repository | [Code of Conduct](CODE_OF_CONDUCT.md) |
| Who decides what, and what is decided by a test instead | [GOVERNANCE.md](GOVERNANCE.md) |
| How a tag gets cut | [RELEASE.md](RELEASE.md) |

## License

[Apache License 2.0](LICENSE). Third-party notices: [NOTICE](NOTICE) — there are none.
