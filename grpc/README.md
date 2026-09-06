# grpc — the second wire, quarantined

This directory is a **separate Go module**, and that is the whole point of it.

## Why it is not part of weft

`.claude/prds/weft.prd.md` lists an operational success metric:

> **운영 — 의존성**: 표준 라이브러리만. 외부 의존성 0개 유지 —
> `go list -m all` 이 자기 모듈만 출력

gRPC cannot be spoken without `google.golang.org/grpc`, and that brings protobuf, `x/net` and `genproto` with it. Putting them in the root module would trade a founding property for a protocol — and `TestNoExternalDependencies` would fail, which is the point of having written it down as a test.

So the same answer `bench/` gives about bleve: **a nested module with a `replace ../`**. `go list -m all` at the root still prints one line, and `make deps` is what says so on every run. Nothing a `go get` user pulls in has changed.

That the quarantine works at all rests on one rule: Go's `internal/` is enforced by **import path prefix, not by module**. This module is `github.com/skyoo2003/weft/grpc`, which sits under `github.com/skyoo2003/weft/`, so it may import `internal/opensearch`. `bench/` has relied on the same thing for `internal/eval` since milestone 5.

## What it serves

The same requests as `POST /{index}/_weft/search`. [docs/API.md](../docs/API.md) is the reference for both; there is no second document, because there is no second behaviour.

```bash
make serve                       # weftd, HTTP, writes and reads
make serve-grpc                  # weftg, gRPC, reads
```

Search only. Writing goes through weftd, because one writer per index is an invariant the index holds and two processes over one directory would each believe they were it.

```protobuf
rpc Search(SearchRequest) returns (SearchResponse);
```

A `SearchRequest` is a positional list of streams, each one a leaf clause as JSON in the spelling `_search` accepts, plus positional weights, a page, a fusion depth and a breakdown flag. [`weft.proto`](weft.proto) says why `streams` is JSON and not a `oneof`: a typed union would be a second list of what a stream can be, and it drifts on the first clause added to one surface and not the other.

## It decides nothing

Every method in [`server.go`](server.go) is conversion either side of one call into `opensearch.NativeSearch` — the same function the HTTP handler calls. No ranking, no refusal and no default is written twice.

Three tests hold that, and they are why this is code rather than a claim:

| Test | What fails the build |
| --- | --- |
| `TestTheTwoSurfacesAreOneEngine` | the same request ranking differently over the two wires |
| `TestTheProtoAndTheCoreDoNotDrift` | a field on `opensearch.NativeRequest` with no counterpart in `weft.proto`, or the reverse |
| `TestRefusalsArriveAsStatusCodes` | a refusal arriving as an empty result instead of a code |

The drift test is the load-bearing one. Two surfaces over one engine stays true only while both carry the same request, and a field added to one and not the other is a feature that silently does not exist — no error, no warning, just a client setting something nobody reads.

## Working on it

```bash
make grpc-build                  # go vet + go test, in this module
make compat-grpc PYTHON=...      # a stock grpcio client, stubs generated from weft.proto
```

Regenerating the Go from the proto needs `protoc` with the two Go plugins:

```bash
protoc --go_out=. --go_opt=module=github.com/skyoo2003/weft/grpc \
       --go-grpc_out=. --go-grpc_opt=module=github.com/skyoo2003/weft/grpc \
       weft.proto
```

The generated files under `weftpb/` are committed, for the reason `bench/go.sum` is: a checkout should build with the Go toolchain alone, and requiring `protoc` to compile the repository would make the tool a dependency of reading it.

`make compat-grpc` needs a Python with `grpcio` and `grpcio-tools`, and skips without one — the same shape as `make compat`, so `make all` still needs nothing but Go:

```bash
python3 -m venv /tmp/weft-grpc-venv
/tmp/weft-grpc-venv/bin/pip install grpcio grpcio-tools
make compat-grpc PYTHON=/tmp/weft-grpc-venv/bin/python
```

It generates its own client from `weft.proto` and drives `weftg` with it, which is what the Go tests cannot say: whether the descriptor this repository ships is one a foreign toolchain can compile and talk to.

## Not for production

Unchanged from everything else here. Sustained load collapses at 27 queries a second rather than degrading, there is no authentication and no TLS, and `weftg` binds loopback for that reason. [docs/STATUS.md](../docs/STATUS.md) and [docs/LIMITATIONS.md](../docs/LIMITATIONS.md) are the account.
