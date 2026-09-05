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

Milestones 1 through 7 and 15 through 23 are done or measured. **Not usable in production:** a commit holds a write lock for as long as it takes — 11 seconds for a 20,000-document batch, with reads queueing behind it — and sustained query load collapses at 27 queries per second on the machine measured rather than degrading.

One result is worth knowing before you read anything else: **graph proximity does not improve ranking.** At its best fusion weight it is worth +0.0000 nDCG@10, and at equal weight it costs 0.1227. The signal is not harmful, it is not information — which is this project's own second falsification condition coming back negative, in public.

Per-milestone state, the published nDCG table and the two debts milestone 5 has not paid: **[docs/STATUS.md](docs/STATUS.md)**.

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

Trailing columns are each scorer's rank *before* fusion. `tfidf` leads text but lands fifth: no other scorer agreed, and one scorer's confidence does not beat consensus. A `-` means the document is absent from that stream — no opinion, deliberately withheld as a traversal seed, or simply below the cut.

The demo fuses with `FuseWeighted(1, 1, 0.1, 1)`, discounting the graph stream to a tenth of a vote, because that is the one weight this project has measured.

Minimal embedding: [`examples/basic`](examples/basic/main.go). Godoc example: `Example` in `pkg/engine`.

## Documentation

Each document answers one question, and only that one.

| Question | Document |
| --- | --- |
| How far along is it, and what do the numbers say? | [STATUS](docs/STATUS.md) |
| What does it *not* do? | [LIMITATIONS](docs/LIMITATIONS.md) |
| How is the module shaped? | [ARCHITECTURE](docs/ARCHITECTURE.md) |
| How do I plug my own signal in? | [SCORERS](docs/SCORERS.md) |
| What was actually measured, milestone by milestone? | [FINDINGS](docs/FINDINGS.md) |
| What was decided, and why is it expensive to reverse? | [DECISIONS](docs/DECISIONS.md) |
| What is on disk? | [FORMAT](docs/FORMAT.md) |
| How were the quality numbers produced? | [EVAL](docs/EVAL.md), [DATASETS](docs/DATASETS.md) |
| How were the latency numbers produced? | [PERF](docs/PERF.md) |
| Is the documentation enough to contribute from? | [ADOPTION](docs/ADOPTION.md) |
| What else is out there? | [RESEARCH](docs/RESEARCH.md) |

Governance and process:

| | |
| --- | --- |
| How to build, check and submit a change | [CONTRIBUTING.md](CONTRIBUTING.md) |
| Where to take a bug, a proposal or a question | [SUPPORT.md](SUPPORT.md) |
| A vulnerability — **not** the issue tracker | [SECURITY.md](SECURITY.md) |
| Behavior in this repository | [Code of Conduct](CODE_OF_CONDUCT.md) |
| Who decides what, and what is decided by a test instead | [GOVERNANCE.md](GOVERNANCE.md) |
| How a tag gets cut | [RELEASE.md](RELEASE.md) |
| What changed between versions | [CHANGELOG.md](CHANGELOG.md) |

## Contributing

`make all` is the gate — fmt, build, vet, `test -race` — and CI runs that same target, so nothing installed beyond the Go toolchain is needed to run what judges you. [CONTRIBUTING.md](CONTRIBUTING.md) has the rest: every `make` target, which assertions a test decides for you, and what a pull request should say.

## License

[Apache License 2.0](LICENSE). Third-party notices: [NOTICE](NOTICE) — there are none.
