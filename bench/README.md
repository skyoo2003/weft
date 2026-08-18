# bench — the comparison, quarantined

This directory is a **separate Go module** and that is the whole point of it.

## Why it is not part of weft

`.claude/prds/weft.prd.md` lists an operational success metric:

> **운영 — 의존성**: 표준 라이브러리만. 외부 의존성 0개 유지 —
> `go list -m all` 이 자기 모듈만 출력

Milestone 5's outcome sentence asks for something that appears to contradict it:

> GC pause를 포함한 p99가 공개되고, **기성 엔진과 같은 자릿수임을 보인다**

Both are real requirements. Quoting a number bleve's own README publishes would
satisfy neither, because it was measured on another machine, another corpus and
another query set — that is not a comparison, it is two unrelated numbers printed
next to each other.

A submodule resolves it exactly. `go.mod` here is not `go.mod` there, so:

- `GOWORK=off go list -m all` in the repository root still prints one line, and
  `pkg/engine.TestNoExternalDependencies` still passes.
- `go build ./...` and `go test ./...` at the root do not descend into this
  directory. The Go tool does not walk into nested modules.
- `make fmt` (`gofmt -l .`) and `make spdx` (`git ls-files '*.go'`) **do** cover
  it, because those walk the working tree rather than the module graph.

So the dependency is quarantined and the discipline is not.

## Do not move this up

If a future change makes `bench` a package under `pkg/` or `cmd/`, weft acquires
around twenty external modules — bleve pulls in zapx, vellum, bbolt, protobuf and
the rest — and a stated success metric becomes false. `make deps` is what catches
it.

## What it measures, and what it deliberately does not

**Measured: the `text` arm only.** BM25 over an inverted index, top 10, the same
171,332 CORD-19 documents and the same 50 TREC-COVID queries weft is measured on,
driven by the same open-loop load generator — `internal/loadgen`, imported through
a `replace` directive so both sides of the comparison share one implementation of
the schedule, the quantiles and the GC accounting.

**Not measured: the hybrid arm.** bleve's vector search is behind a `vectors`
build tag and needs cgo and a faiss shared library. Pulling that in would change
what is being compared — "a Go search engine" becomes "a Go wrapper around faiss"
— and the number would no longer say what the milestone wants it to say. The cost
of that omission is stated rather than hidden: the arm a user would actually
deploy is `text+vector`, and this comparison cannot speak to it.

**Not matched: the analyzer.** weft's `engine.Tokenize` lowercases and splits on
anything that is not a letter or a digit. bleve's default `standard` analyzer also
removes English stop words and applies Porter stemming. bleve is therefore doing
strictly more work per token at index time and querying a smaller postings space
at query time, and the two effects push in opposite directions. Neither is
plausibly worth an order of magnitude, which is the only claim being made — but
the comparison is not analyzer-matched and no reading of it should assume so.

## Running it

```bash
cd bench
go run . -build          # index the corpus into bleve; slow, once
go run . -rotations 200  # replay the queries under the open-loop ladder
```

The index it writes is gitignored. `make bench-compare` from the repository root
runs the query phase and skips with a message when the index is not there, the
same shape as `make recall`.
