# TDD evidence — milestones 28 and 29

**Source plan**: none on disk. The journeys below were fixed in an inline plan
approved on 2026-09-06, with two amendments made at approval time: `weft` with no
arguments prints the help rather than running the demo, and the demo moves into
`examples/` split by case.

**Milestones**: 28 (the library reaches a shell) and 29 (the server catches up to
the library), recorded in `.claude/prds/weft-api.prd.md` section 6.

**Runner**: Go. `<test>` is `go test ./...`, `<coverage>` is `go test -cover`,
`<lint>` is `make lint`.

## User journeys

1. As someone evaluating weft, I want to index a corpus and query it from a
   shell, so that I can judge the library without writing a Go program first.
2. As an operator, I want to ask an index what it holds — is this term there,
   under what spelling, in which documents — so that a surprising ranking has an
   explanation I can reach.
3. As an operator, I want to make writes durable, rewrite segments and verify a
   directory on purpose, rather than only as a side effect of a write.
4. As a client of the HTTP surface, I want the graph signal weft has had since
   milestone 1, so that the server is not half a library.
5. As a maintainer, I want an export nothing can call to fail the build, so that
   "the CLI supports the library" stops being a sentence nobody re-derives.

## Task report

### Phase 0 — the coverage ledger, RED

`cmd/weft/coverage_test.go` reads `pkg/engine/testdata/engine_api.txt` and
`public_api.txt` as data and asks, of every callable symbol, which command
reaches it.

```text
$ go test ./cmd/weft/ -run TestEveryPublicSymbolHasACommandOrAReason
--- FAIL: TestEveryPublicSymbolHasACommandOrAReason
    engine.Index.Merge is recorded as reached by [weft weftd], and no call site ".Merge(" is there.
    ... 29 more ...
    public callable symbols: 74, reached by a command: 41 (55.4%), reached by none: 3
```

**Guaranteed**: a symbol with no row fails, a row naming a symbol that no longer
exists fails, and a row claiming a surface whose source no longer holds the call
fails. The figure is logged rather than asserted against a threshold.

The RED run found two defects in the ledger itself, both fixed before GREEN.
`fusion.Fuse` is handed over as a value, so `engine.Fuser(fusion.Fuse)` is the
call site and the bare name is a prefix of `FuseWeighted`'s. And `.Err(` is
satisfied by `bufio.Scanner`'s, which the index command calls on stdin, so the
cursor's own name had to become part of the proof.

### Phase 1 — index and search

RED was compile-time: `run` still had the demo's signature, and neither the
tokenizer table nor the subcommands existed.

```text
$ go test ./cmd/weft/
cmd/weft/main_test.go:15:9: cannot use run(...) (value of interface type error) as int value
cmd/weft/main_test.go:85:2: undefined: usage
cmd/weft/index_test.go:202:49: undefined: tokenizers
FAIL    github.com/skyoo2003/weft/cmd/weft [build failed]
```

GREEN after `main.go`, `index.go`, `search.go` and `tokenizer.go`:

```text
$ go test ./cmd/weft/ -run 'TestIndex|TestSearch|TestGraph|TestPersonalized|TestRecency|TestNoArg|TestExplicit|TestUnknown|TestUsage'
ok      github.com/skyoo2003/weft/cmd/weft    1.452s
```

**One test premise was wrong and was fixed rather than worked around.** engine
detects a tokenizer mismatch by re-tokenizing one sampled document and comparing
its token count, so `"Hello, World"` cannot trigger it — the default tokenizer
and the whitespace one both make two tokens of it. The fixture is `"weft-cli
tool"` now, where the default cuts at the hyphen and whitespace does not, and the
assertion is on `engine.ErrTokenizerMismatch` rather than on any error at all.

### Phase 2 — inspect, check, encode

RED at runtime: `weft: unknown subcommand "check"`. GREEN closed the ledger's
weft half:

```text
$ go test ./cmd/weft/ -run TestEveryPublicSymbolHasACommandOrAReason -v
public callable symbols: 74, reached by a command: 71 (95.9%), reached by none: 3
  nothing reaches engine.NewCollector: Collector is the top-k buffer a Scorer
  implementation keeps while it walks postings. A command runs scorers; it does
  not write one.
--- PASS
```

### Phase 3a — the graph signal over HTTP

RED at runtime, both halves absent:

```text
$ go test ./internal/opensearch/ -run 'TestGraph|TestLinks'
--- FAIL: TestLinksIsAMappingBinding
    the mapping does not report the binding:
    {"papers":{"mappings":{"properties":{"cites":{"type":"keyword"},...
--- FAIL: TestGraphWalksFromItsSeeds
    status 400 (body {"error":{"reason":"no query registered for \"weft_graph\"",...
```

GREEN after `graph.go`, one property flag in `mapping.go` and one branch in
`documentFrom`:

```text
$ go test ./internal/opensearch/ -run 'TestGraph|TestLinks'
ok      github.com/skyoo2003/weft/internal/opensearch    1.135s
```

### Phase 3b — the half of engine no search touches

RED at runtime: 404 on all nine routes. GREEN after `admin.go`, two registry
methods and the route block. One test assertion was wrong — the reason travels as
a JSON string, so `"q"` arrives escaped — and was corrected to match what the
server actually sends.

### Phase 4 — lint, docs and ledgers

`make lint` went from 56 issues to 0. Two were cyclomatic complexity, on
`searchCmd` (30) and `inspectCmd` (25); neither got an exception, because unlike
`cmd/weft-eval`'s — where splitting would move a guard away from the step it
guards — these really were three things each. All 23 command tests stayed green
across the split.

## Test specification

| # | What is guaranteed | Test | Type | Result |
| --- | --- | --- | --- | --- |
| 1 | Every callable export has a command or a recorded reason, and the count is published | `cmd/weft/coverage_test.go:TestEveryPublicSymbolHasACommandOrAReason` | structural | PASS (71/74) |
| 2 | `weft` with no arguments prints the help and exits 2; `weft help` prints it to stdout and exits 0 | `cmd/weft/main_test.go` | unit | PASS |
| 3 | Documents on stdin round-trip to an index a second process can open; a key already present is updated, not duplicated | `cmd/weft/index_test.go:TestIndexCommitsWhatItRead`, `TestIndexUpdatesAnExistingKey` | integration | PASS |
| 4 | A malformed input line names its line number; an empty key carries `engine.ErrEmptyKey` through unchanged | `cmd/weft/index_test.go:TestIndexNamesTheBadLine`, `TestIndexRefusesAnEmptyKey` | unit | PASS |
| 5 | An index committed with one tokenizer will not open under another | `cmd/weft/index_test.go:TestIndexTokenizerIsRecorded` | integration | PASS |
| 6 | A `-weights` list of the wrong length is refused with both counts rather than silently re-ranking | `cmd/weft/search_test.go:TestSearchWeightsMustMatchTheStreams` | unit | PASS |
| 7 | `-restart`, `-precision` and `-now` are refused when no scorer would read them | `cmd/weft/search_test.go:TestGraphOptionsWithoutTheScorerAreRefused` and neighbours | unit | PASS |
| 8 | weft's own query string, field-scoped BM25, PPR and both its options are reachable from a shell | `cmd/weft/search_test.go:TestSearchByQueryString`, `TestSearchScopesToAField`, `TestPersonalizedPageRankIsReachable` | integration | PASS |
| 9 | `inspect` reports Len beside Stats, and a key that is not in the index is an error rather than an empty report | `cmd/weft/inspect_test.go:TestInspectStats`, `TestInspectUnknownDocumentIsNotAnEmptyReport` | integration | PASS |
| 10 | A damaged segment fails `weft check` | `cmd/weft/check_test.go:TestCheckReportsDamage` | integration | PASS |
| 11 | `encode -int` and `-time` preserve order under byte comparison | `cmd/weft/check_test.go:TestEncodeInt`, `TestEncodeTime` | unit | PASS |
| 12 | `links` is a keyword-only mapping binding, and the walk reaches what a seed cites without returning the seed | `internal/opensearch/graph_test.go:TestLinksBelongsToAKeywordField`, `TestGraphWalksFromItsSeeds` | integration | PASS |
| 13 | A graph query with no seeds, an unresolvable seed, no links mapping, or PPR options without PPR is refused rather than answered empty | `internal/opensearch/graph_test.go` (four tests) | integration | PASS |
| 14 | A graph stream fuses and weights by position like any other | `internal/opensearch/graph_test.go:TestGraphFusesLikeEverythingElse`, `TestGraphIsWeightedByPosition` | integration | PASS |
| 15 | A force merge keeps the corpus, and `max_num_segments` is refused rather than ignored | `internal/opensearch/admin_test.go:TestForceMergeKeepsTheCorpus`, `TestForceMergeTakesNoTarget` | integration | PASS |
| 16 | `_count` with a query, and `_analyze` with a named analyzer, are refused with the reason | `internal/opensearch/admin_test.go` | integration | PASS |
| 17 | `/_weft/query` runs weft's query string and reports a syntax error rather than an empty ranking | `internal/opensearch/admin_test.go:TestWeftQuery`, `TestWeftQueryReportsASyntaxError` | integration | PASS |
| 18 | `opensearch-py` 3.2.0 still drives weftd unmodified after ten new routes and a new clause — 50 checks, including every refusal | `make compat PYTHON=<venv>/bin/python` | compatibility | PASS |
| 19 | `pkg/` is unchanged, fusion imports no scorer, and the module has no dependencies | `make arch`, `make deps`, `git diff --stat main -- pkg/` | structural | PASS (0 lines) |

## Coverage

```text
$ go test -cover ./cmd/weft/ ./internal/opensearch/
ok      github.com/skyoo2003/weft/cmd/weft    coverage: 88.8% of statements
ok      github.com/skyoo2003/weft/internal/opensearch    coverage: 82.7% of statements
```

255 tests across the two packages, both above the 80% line. Beyond them, `make compat`
runs an official client against a live `weftd`:

```text
$ make compat PYTHON=/tmp/v/bin/python
  ok  hybrid fuses a text stream and a vector stream
  ok  three signals fuse in one query
  ok  a search pipeline is refused with 400 (want 400)
  ... 50 checks ...
PASS: opensearch-py drove weftd unmodified.
```

That is milestone 24's judgment sentence, re-run after ten routes and a clause were
added. It holding is the evidence the `/_weft/` namespace is invisible to a standard
client, which is what D-033 is spending the prefix on.

## Known gaps

- **Three symbols are reachable by nothing**, on purpose and with the reason in
  the ledger: `NewCollector`, `Collector.Offer`, `Collector.Take`. D-032 records
  what would show that judgment was wrong.
- **`text.Scorer` is still not on the HTTP surface.** weftd's `match` builds
  `query.Glob` streams, so BM25 proper — `K1`, `B`, `NewField` — is reached from
  the CLI and not from the server. That is pre-existing behaviour rather than
  something this round changed, and changing it would move rankings the DSL tests
  pin. Recorded here rather than fixed quietly.
- **`weft_graph` with `"ppr": true` builds an adjacency per query.** Marked in
  the code with a `ponytail:` comment and in `docs/LIMITATIONS.md`.
- **No load measurement through the new routes.** Milestone 27 is still blocked
  on a machine window, and nothing here changes what it will measure — the ladder
  aims at `_search`.

## Merge evidence

Checkpoint commits on `cli-full-api-surface`, in order: `2154261` (RED, ledger
and help contract), `dfe8899` (examples), `979757d` (RED, index and search),
`c5819fe` (GREEN, the command), `5792da5` (RED, inspect/check/encode), `bacc8f1`
(GREEN, ledger closed at 95.9%), `ff657a1` (GREEN, graph over HTTP), `eaa3076`
(GREEN, nine routes), `171af24` (refactor, lint clean). If they are squashed,
this file is the surviving record of which commit was RED and which was GREEN.
