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
go run ./examples/breakdown
```

```text
query "ranking fusion" @ [1 0 0], 4 scorers

  1. rrf        0.04813  text:2  vector:3  graph:-  recency:2
  2. tfidf      0.04791  text:1  vector:2  graph:-  recency:5
  3. bm25       0.03366  text:-  vector:1  graph:1  recency:4
  4. hnsw       0.03311  text:-  vector:4  graph:2  recency:3
  5. changelog  0.01639  text:-  vector:-  graph:-  recency:1
```

Trailing columns are each scorer's rank *before* fusion. `tfidf` leads text but lands second: no other scorer put it first, and one scorer's confidence does not beat consensus. A `-` means the document is absent from that stream — no opinion, deliberately withheld as a traversal seed, or simply below the cut. `changelog` has neither a vector nor a link and surfaces anyway, which is what rank fusion buys.

The example fuses with `FuseWeighted(1, 1, 0.1, 1)`, discounting the graph stream to a tenth of a vote, because that is the one weight this project has measured.

Three more, one case each: [`examples/weights`](examples/weights/main.go) runs one query under `Fuse` and `FuseWeighted` side by side, [`examples/sparse`](examples/sparse/main.go) is about the documents a scorer cannot see, and [`examples/basic`](examples/basic/main.go) is the smallest embedding there is. Godoc example: `Example` in `pkg/engine`.

### Or from a shell, without writing Go

```bash
go run ./cmd/weft index -data ./ix < corpus.jsonl
go run ./cmd/weft search -data ./ix -q '+fusion' -scorers vector,recency -breakdown
go run ./cmd/weft inspect -data ./ix -term fusion
```

`weft` is a command with subcommands: `index`, `search`, `inspect`, `check` and `encode`. Between them they reach every callable symbol the library exports but three, and those three are named with their reason in `cmd/weft/coverage_test.go` — which reads the golden API files as data, so an export no command can call fails the build.

### Or over HTTP, without writing Go

```bash
go run ./cmd/weftd                                  # 127.0.0.1:9200
curl -XPUT localhost:9200/papers
curl -XPUT 'localhost:9200/papers/_doc/1?refresh=true' \
  -H 'Content-Type: application/json' -d '{"text":"reciprocal rank fusion"}'
curl -XPOST localhost:9200/papers/_search \
  -H 'Content-Type: application/json' -d '{"query":{"match":{"text":"fusion"}}}'
```

`weftd` speaks a subset of the OpenSearch REST API, and `opensearch-py` drives it unmodified — `make compat` is that check. `GET /` reports OpenSearch 2.19.0, which is untrue and is the **only** untrue thing it says: past the handshake, a query weft cannot express returns 400 or 501 with a reason rather than 200 with an empty hit list. [DECISIONS](docs/DECISIONS.md) D-025 and D-026 argue both halves.

The subset is `match`, `match_phrase`, `term`, `terms`, `prefix`, `wildcard`, `fuzzy`, `range`, `exists`, one level of `bool`, `knn`, `hybrid`, `function_score` decay, `_bulk`, and `from`/`size` paging. **Eight of twenty-five documented rows refuse — 32%, counted by a test rather than by eye**, and [LIMITATIONS](docs/LIMITATIONS.md) lists what each refusal is protecting you from.

The hybrid is the point of the server, not a feature of it:

```bash
curl -XPOST localhost:9200/papers/_search -H 'Content-Type: application/json' -d '{
  "query": {"hybrid": {"queries": [
    {"match": {"text": "rank fusion"}},
    {"knn": {"vec": {"vector": [0.1, 0.9, 0.2], "k": 10}}},
    {"function_score": {"gauss": {"published": {}}}}],
    "weights": [1, 1, 0.5]}}}'
```

Three signals, one request, no search pipeline and no normalization processor. The weights attach to **positions**, so `fusion.FuseWeighted` still cannot name a single scorer — which is the whole architecture claim, restated on the far side of a socket.

The library is still the product. The server is a `cmd/`, and the DSL, the mappings, `_bulk` and the hybrid together changed **zero lines under `pkg/`** — which is the assertion rather than the aspiration. Adding recency as a *fourth* signal to the HTTP surface changed zero lines of fusion code and 57 lines in its query clause; what it did cost is an index-time binding, because a JSON body has no field for "this date is the document's time" ([FINDINGS](docs/FINDINGS.md) milestone 26). The production warning above applies to it unchanged, and it binds to loopback because there is no authentication and no TLS.

### Or in weft's own terms, where the DSL runs out

`hybrid` is a wrapper around a list of streams. On weft's own route the list *is* the request, and two things the OpenSearch response has nowhere to put come back with it:

```bash
curl -XPOST localhost:9200/papers/_weft/search -H 'Content-Type: application/json' -d '{
  "streams": [{"match": {"text": "rank fusion"}},
              {"knn": {"vec": {"vector": [0.1, 0.9, 0.2], "k": 10}}}],
  "weights": [1, 0.5], "depth": 100, "breakdown": true}'
```

`breakdown` is each stream's own rank *before* fusion — the `-` column the first example prints, per hit, with `null` for a stream that had no opinion. `depth` is the fusion depth, which on `_search` is tangled with `knn`'s `k` and cannot be set at all for a text-only query. A stream is a leaf clause in the spelling `_search` already accepts, compiled by the same function, so every clause and every refusal is shared rather than reimplemented. [API](docs/API.md) is the reference.

### Or over gRPC

```bash
cd grpc && go run ./cmd/weftg -data ../.weftd-data     # 127.0.0.1:9201
```

The same request in a second encoding, and **the root module still has no dependencies**: `grpc/` is a nested module with a `replace ../`, the shape [`bench/`](bench/README.md) already uses to keep bleve out, so `go list -m all` here prints one line. The service decides nothing — it converts protobuf to the same struct the HTTP handler builds and calls the same function — and a field on one side without a counterpart on the other fails the build. [grpc/README.md](grpc/README.md) is why it is a module, and `make compat-grpc` is a stock `grpcio` client driving it with stubs it generated from `weft.proto` itself.

## Documentation

Each document answers one question, and only that one.

| Question | Document |
| --- | --- |
| How far along is it, and what do the numbers say? | [STATUS](docs/STATUS.md) |
| What does it *not* do? | [LIMITATIONS](docs/LIMITATIONS.md) |
| How is the module shaped? | [ARCHITECTURE](docs/ARCHITECTURE.md) |
| How do I plug my own signal in? | [SCORERS](docs/SCORERS.md) |
| How do I ask it something OpenSearch cannot express? | [API](docs/API.md) |
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
