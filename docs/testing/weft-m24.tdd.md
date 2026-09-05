# Milestone 24 — TDD evidence

**Source plan:** [`.claude/plans/weft-m24.plan.md`](../../.claude/plans/weft-m24.plan.md)
**Source PRD:** [`.claude/prds/weft-api.prd.md`](../../.claude/prds/weft-api.prd.md)
**Branch:** `m24-opensearch-api`
**Date:** 2026-09-05

This is an index, not a substitute for the tests. It records what the test code
proves, and what was measured to prove it, so a squash merge or a new session does
not lose the answer to "what was verified, and how".

## Plan handling

The plan was read as data. Its validation section names `make all`, `make arch`,
`make deps`, `make spdx`, `make compat`, `go test -race`, `go build`,
`git diff --stat pkg/`, `git log`, `go run ./cmd/weftd` and `curl` against
loopback — all build, test, lint or measurement actions on this repository. No
destructive filesystem operation, no credential handling, no network installer
and no instruction-to-agent override was present. Nothing needed rejecting.

One item in the plan needed correcting rather than rejecting. Its Files to Change
said `DELETE /{index}` calls `engine.Scrub`. **`Scrub` verifies a directory's
integrity and does not remove one**, so `Registry.Drop` is `os.RemoveAll`
instead. The plan was wrong about the API, not about the intent.

One dependency was installed, under an existing precedent. `make compat` needs
`opensearch-py`, which is not in the repository and must not be: it went into a
throwaway virtualenv at `/tmp/weft-compat-venv`, the shape [EVAL §7](../EVAL.md)
already prescribes for `pytrec_eval` and `rank_bm25`. The `compat` target takes
`PYTHON=` so nothing global is touched, and skips entirely when the client is
absent — `make all` still needs nothing but the Go toolchain.

**Task 0 of the plan was a blocking approval gate**, because this milestone
reverses an out-of-scope line in the founding PRD. It was approved by the
maintainer invoking implementation on the plan, and the approval is recorded
inside [D-025](../DECISIONS.md) rather than only here.

## User journeys

From the PRD, restated as what a person outside this repository needs:

| # | Journey |
| --- | --- |
| J1 | As someone evaluating weft, I want to try it without writing a Go program, so the cost of a look is a `curl` rather than a project. |
| J2 | As a client author, I want my existing OpenSearch client to connect unmodified, so I write no adapter. |
| J3 | As an operator, I want a query the engine cannot answer to **fail loudly**, so a missing capability is never a quietly empty result page. |
| J4 | As a maintainer, I want the library to stay the product, so a server does not become the only way in. |
| J5 | As an operator, I want the document I sent back exactly as I sent it, so `_source` is not a lossy re-rendering. |
| J6 | As an operator, I want a slow commit not to hand its cost to every request that arrives during it. |

## Task report

### Task 0–1 — the decisions, registered before the first line of Go

D-025 (the server is a `cmd/`) and D-026 (the handshake is the only lie) were
written and committed as `0f455e2`, ahead of every Go file. That order is
milestone 14's rule applied to a scope reversal, and it is checkable:

```console
$ git log --oneline --reverse -- docs/DECISIONS.md internal/opensearch/ | head -2
0f455e2 docs: D-025 and D-026 — the server is a cmd/, and the handshake is the only lie
8f65b15 test: add the _source store reproducer, RED
```

### Task 2 — the `_source` store

RED `8f65b15` → GREEN `a122e83`. `engine.Document` has no field for the JSON a
client sent and must not grow one — the raw body in `Text` or a `Field` would be
tokenized, and the vocabulary would then hold every key and brace ever posted. So
the store sits beside the segments and is published by rename, which is what
`Commit` does with a manifest.

The count check against `Index.Stats` is the load-bearing part. A directory whose
index and store have drifted apart would let search find documents the response
cannot show; `LoadSource` refuses to start from it.

### Task 3 — the registry and one writer per index

RED `9ffcc5f` → GREEN `87d403e`.

**The RED test found a real defect on its first run.** Shutdown closed the `ops`
channel, and a send on a closed channel panics *even inside a `select`*, so a
write racing shutdown took the process down instead of receiving `ErrClosed`:

```text
--- FAIL: TestAWriteAfterCloseIsRefused (0.04s)
panic: send on closed channel [recovered, repanicked]
  opensearch.(*Index).write(...) registry.go:257
```

`serve` now watches a separate `closed` channel. An op already received still
always completes, because an unbuffered send only returns once the loop holds it.

### Task 4–5 — the HTTP surface and the match mapping

RED `27d358b` → GREEN `7a3c9e2`.

`match` becomes one `query.Glob` stream per token and `fusion.Fuse` ranks them.
That is the whole mapping, and it needed no code in `pkg/`: rank fusion is
already a union of votes, which is what an OR-ed match means.

Two ceilings were taken deliberately and are marked in the source:

- `match_all` is a linear scan of the id space, because no scorer nominates every
  document and `Glob("*")` would silently stop at `query.MaxTerms` — a wrong
  answer on any corpus with more than 4096 distinct terms.
- `hits.total.relation` is `gte` rather than `eq` when the page is full, because
  `Search` returns the top k and `eq` there is a number the server cannot stand
  behind.

### Task 6 — `cmd/weftd`

`1f4c235`. Loopback by default, and the startup banner repeats STATUS.md's
production warning on every start — weftd is the first thing that makes that easy
to forget.

### Task 7 — `make compat`

`e9d0d54`. The Go tests drive every route through `httptest` and prove the
handler does what the handler was written to do. Only a real client can say the
**protocol** is right, which is what D-026's version claim was spent on.

### Task 8 — invariants, lint, and one more defect

`af6f08a` and the commit after it. `make all` initially failed with 11 lint
findings; all eleven were acted on rather than excluded. One was not cosmetic:
`noctx` on `net.Listen` led to moving the signal context ahead of the bind, which
closed a window where Ctrl-C between binding and installing the handler killed the
process with a bound port and open mappings.

Coverage then measured **79.9%**, with `matchText` at 18.8% and `scalarText` at
50% — both on the honest-refusal surface this milestone's claim rests on. The
tests that closed that gap found a second real defect: **`refresh=maybe` returned
400 over a document that was already indexed.** `parseRefresh` now runs before the
write. Coverage after: **87.1%**.

## Test specification

| # | What is guaranteed | Test | Type | Result |
| --- | --- | --- | --- | --- |
| 1 | An official client drives weftd with no modification — J2 | `internal/opensearch/testdata/compat.py`, 21 checks | end-to-end | PASS |
| 2 | A query weft cannot express is 400 or 501 with a reason, never 200 with an empty hit list — J3 | `server_test.go:TestAnUnsupportedQueryIsNotAnEmptyResult`, 11 subtests | integration | PASS |
| 3 | `match` is fusion over its tokens: the document both streams rank comes first | `server_test.go:TestMatchIsFusionOverItsTokens` | integration | PASS |
| 4 | `_source` round-trips byte for byte, numbers included — J5 | `server_test.go:TestAnIndexRoundTripsThroughTheDocumentAPI` | integration | PASS |
| 5 | The store and the index survive a restart together, and a mismatch is refused | `source_test.go`, 6 tests; `server_test.go:TestRefreshTrueMakesTheWriteSurviveAReopen` | unit + integration | PASS |
| 6 | 32 concurrent writers land exactly once, and a write after shutdown is refused rather than dropped — J6 | `registry_test.go:TestConcurrentWritesAllLandExactlyOnce`, `TestAWriteAfterCloseIsRefused` | unit, `-race` | PASS |
| 7 | A directory that will not open fails startup rather than becoming a 404 | `registry_test.go:TestRegistryRefusesADirectoryItCannotOpen` | unit | PASS |
| 8 | A refused `refresh` value leaves the document unwritten | `mapping_test.go:TestRefreshRefusesAValueItCannotMean` | integration | PASS |
| 9 | Every long-form `match` option is named rather than dropped, 8 shapes | `mapping_test.go:TestMatchAcceptsTheLongFormAndRefusesItsOptions` | integration | PASS |
| 10 | Scalars are indexed, structures are stored only, a field is its own term space | `mapping_test.go:TestScalarsAreIndexedAndStructuresAreOnlyStored`, 5 subtests | integration | PASS |
| 11 | A mapping is refused rather than accepted and dropped | `mapping_test.go:TestCreateIndexRefusesAMappingItCannotHonour` | integration | PASS |
| 12 | **The library stays the product: zero lines under `pkg/`** — J4 | `git diff --stat main -- pkg/`, `make arch`, `make deps` | invariant | PASS |

```console
$ go test -race ./internal/opensearch/     # 48 tests and subtests
ok  github.com/skyoo2003/weft/internal/opensearch  3.085s

$ make compat PYTHON=/tmp/weft-compat-venv/bin/python
PASS: opensearch-py drove weftd unmodified.        # opensearch-py 3.2.0, 21 checks

$ git diff --stat main -- pkg/
                                                    # no output
$ make arch
ok  github.com/skyoo2003/weft/pkg/engine  0.453s    # 8/8, both golden files unchanged
$ make deps
github.com/skyoo2003/weft
OK: fusion imports no scorer package
```

## Coverage and known gaps

`internal/opensearch` statements: **87.1%**. `cmd/weftd` has no test file; what it
holds is flag parsing, a listener and a banner, and the handler it wires is the
one the 48 tests above drive.

Deliberately not covered, each with the reason:

| Gap | Why |
| --- | --- |
| `Registry.Close` error paths | They report a failure to unmap, which needs a filesystem fault injector this repository does not have |
| `Source.Save` I/O failures | Same |
| Read latency during a commit | This is `engine`'s property, not the server's, and it is already published at **61 ms** ([FINDINGS milestone 9](../FINDINGS.md)). Re-asserting it here would test the wrong package |
| Behaviour under sustained HTTP load | **Milestone 27**, and it is blocked on a quiet 3.1-hour window that has come back void or unexecuted four rounds running ([FINDINGS milestone 14](../FINDINGS.md)). Nothing in this milestone claims the 27 q/s collapse moved, or that it did not |

## Merge evidence

Eleven checkpoint commits on `m24-opensearch-api`, in RED/GREEN pairs, oldest
first:

```text
0f455e2  docs: D-025 and D-026 — registered before the first line of Go
8f65b15  test: _source store reproducer, RED
a122e83  feat: the _source store, GREEN
9ffcc5f  test: registry and write-serialization reproducer, RED
87d403e  feat: registry and one writer per index, GREEN — fixed a panic the RED test found
27d358b  test: HTTP surface reproducer, RED
7a3c9e2  feat: HTTP surface and the match-to-fusion mapping, GREEN
1f4c235  feat: cmd/weftd
e9d0d54  test: opensearch-py drives weftd unmodified — the judgment
af6f08a  refactor: named response types, lint gate green
(head)   fix: validate refresh before the write, not after
```

If these are squashed, the two findings worth carrying into the squash body are
the ones the tests bought rather than confirmed: **a send on a closed channel
panics inside a `select`**, and **`refresh` was validated after the write**.
