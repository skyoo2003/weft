# Milestone 13 — TDD record: the tokenizing seam

**Source plan**: `.claude/plans/weft-m13.plan.md` (not in the repository;
`.claude/` is gitignored). Its tasks are reproduced below where they are
referenced.

This file is the index into what the tests prove. It is not a substitute for
them: every claim here names the test that carries it and the command that ran
it.

**What is different about this milestone.** Milestone 12's answer was zero
implementation lines, and that is not available here — `engine.Tokenize` is a
function, and a function is not made replaceable by a sentence. So the honesty
this round could buy was of one specific kind: **the line count was written down
and committed before the first line of implementation**, and the spend is
published against it. The RED state is therefore a compile failure, deliberately:
the tests name an API that does not exist, and the package not building is the
evidence that the seam is absent.

## User journeys

From the plan's D1, D4 and D5, and from the PRD's milestone 13 row. The first
three are outcome clauses; the last two are the open questions the round was
asked to close.

1. As an adopter whose language the default tokenizer splits wrongly, I want to
   supply my own tokenizer, **so that** a query my corpus contains can be found
   at all.
2. As an adopter, I want index-time and query-time tokenizing to pass through one
   replacement point, **so that** I cannot configure the two sides apart and get
   silence.
3. As a maintainer, I want the published nDCG figures not to move, **so that** the
   seam is known to have cost nothing to ranking.
4. As an adopter who changes tokenizers, I want a directory indexed with the old
   one to be **refused** rather than to answer zero hits forever (PRD open
   question 5).
5. As a reader of the milestone's result, I want a pass line stronger than ">0
   hits" without a labelled Korean dataset (PRD open question 6).

## Task report

### Task 1 — register the budget before the code

Wrote `docs/FINDINGS.md` milestone 13 §1: the golden budget item by item, the
invariant table, the site where the falsification condition would be read, and
the two open-question answers as designs rather than as results.

```console
$ git log --oneline --reverse main..HEAD | head -2
f499db4 docs: register milestone 13's golden budget and invariants before the code
e682709 test: add the reproducers for milestone 13's tokenizing seam
```

The registration commit precedes every code commit on the branch, which is what
makes §2's comparison a prediction rather than a description.

### Task 2 — RED

`pkg/engine/tokenizer_test.go`, eight tests, `package engine_test` so it may
import `pkg/scorer/text` without a cycle.

```console
$ go test -run 'TestTokenize|TestKorean|TestZeroValue|TestDefaultTokenizer|TestGuard' ./pkg/engine/
# github.com/skyoo2003/weft/pkg/engine_test
pkg/engine/tokenizer_test.go:135:19: too many arguments in call to engine.New
    have (unknown type)
    want ()
pkg/engine/tokenizer_test.go:135:26: undefined: engine.WithTokenizer
pkg/engine/tokenizer_test.go:186:15: ix.Tokenize undefined (type *engine.Index has no field or method Tokenize)
pkg/engine/tokenizer_test.go:285:28: undefined: engine.ErrTokenizerMismatch
FAIL    github.com/skyoo2003/weft/pkg/engine [build failed]
```

**Compile-time RED, and the failure is the finding.** Four names do not exist:
`New` takes no options, there is no `WithTokenizer`, no `Index.Tokenize`, no
`ErrTokenizerMismatch`. The plan's Task 2 asked for exactly this — *the reason for
failure must be "does not compile" and not "assertion wrong", because that is the
evidence there is no seam.*

### Task 3 — GREEN, the seam

Rung 1 of the four D-022 priced: `New(opts ...Option)`,
`Open(dir string, opts ...Option)`, `WithTokenizer`, `Tokenizer`, `Index.tok`,
`Index.Tokenize`. The three index-time call sites route through the method.

```console
$ go test -race -run 'TestTokenize|TestKorean|TestZeroValue|TestDefaultTokenizer|TestGuard' ./pkg/engine/
--- FAIL: TestTokenizerMismatchIsRefusedAtOpen (0.05s)
    tokenizer_test.go:283: Open with the default tokenizer accepted a directory indexed with bigrams
FAIL
```

Seven of eight green; the eighth now fails **on its assertion** rather than on
compilation, because `ErrTokenizerMismatch` was declared and the guard that
returns it was not yet written. That transition — compile failure to assertion
failure — is what says the seam itself landed.

### Task 4 — GREEN, the mismatch guard

`checkTokenizer` on the `Open` path, after the tombstones are read so that
"live" means something.

```console
$ go test -race -run 'TestTokenize|TestKorean|TestZeroValue|TestDefaultTokenizer|TestGuard|TestRestore|TestFormatV2' ./pkg/engine/
ok      github.com/skyoo2003/weft/pkg/engine    1.817s
```

`TestRestore` and `TestFormatV2` are in that selector deliberately: they read
version 3 and version 4 fixtures, and their staying green is the assertion that
the guard did not break backward reading.

**One check was written, run, and removed — and it is the most informative thing
in this report.** The registered design had a third step: every recomputed term
against the segment's terms index. It failed three existing tests:

```console
$ go test -race ./... 2>&1 | grep FAIL
--- FAIL: TestALyingTermOffsetIsNeverFollowed/before_the_frame_header
    lazy_test.go:803: Open: document "delta" yields term "ranking" under this
    tokenizer and no segment claims it: engine: documents were indexed by a
    different tokenizer
--- FAIL: TestAnImpossibleFrequencyIsRefused
--- FAIL: TestALyingBlockMinimumIsRefused
```

Each of those replaces a segment's whole `terms` section with a doctored
one-entry payload and asserts `Open` **succeeds**, with damage surfacing as
absence at query time. The check could not tell a doctored terms section from a
replaced tokenizer — the bytes are the same — so it reported corruption under the
wrong sentinel, and it put verification back into `Open` where milestone 3 and
[D-006](../DECISIONS.md) had removed it. It came out, and the widened ceiling is
published in [FINDINGS §4](../FINDINGS.md), `FORMAT.md` §8 and the `ponytail:`
comment on `checkTokenizer` rather than quietly edited into the registration.

### Task 5 — the query side, one line

```console
$ git diff --numstat 1eb2a44..HEAD -- pkg/scorer/text/text.go
6    3    pkg/scorer/text/text.go
$ git diff --stat 1eb2a44..HEAD -- pkg/fusion pkg/scorer/graph pkg/scorer/recency pkg/scorer/vector
(empty)
```

Nine lines, of which one is code: `engine.Tokenize(q.Text)` became
`s.ix.Tokenize(q.Text)`. **The scorer asks the index and does not receive a
tokenizer**, which is the falsification condition read clean — see
[FINDINGS §3](../FINDINGS.md).

### Task 6 — the golden spend

```console
$ git diff --numstat -- pkg/engine/testdata/
7    2    pkg/engine/testdata/engine_api.txt
$ make arch
ok      github.com/skyoo2003/weft/pkg/engine
```

Five added lines and two changed, against a 5-to-7 budget; `public_api.txt`
unmoved. The spend was recorded in `3b2faf4` and the file refreshed in `f290e49`,
in that order, which is [D-015](../DECISIONS.md)'s rule.

### Task 7 — invariants

```console
$ make eval
  text                                   0.5826
  text+vector                            0.6211
$ make deps
--- external dependencies (want: this module and nothing else) ---
github.com/skyoo2003/weft
OK: fusion imports no scorer package
```

Quality identical to four decimals. **The performance half came back void** and is
not a pass: the ladder missed all three clauses, and the same top rung on the
pre-milestone commit misses them the same way, so the instrument was measuring the
machine. [FINDINGS §7](../FINDINGS.md) and [PERF §5.6](../PERF.md) carry the A/B
and what a valid attempt needs.

## Test specification

| # | What is guaranteed | Test | Type | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 1 | A Korean query whose term the corpus holds with a particle attached finds **nothing** under the default tokenizer | `pkg/engine/tokenizer_test.go:TestKoreanQueryFindsNothingWithTheDefaultTokenizer` | integration | PASS | `go test -race -run TestKorean ./pkg/engine/` |
| 2 | The same query under a replaced tokenizer returns the target **first**, with neither decoy present | `…:TestKoreanQueryFindsTheTargetWithAReplacedTokenizer` | integration | PASS | same |
| 3 | The decoys share no bigram with the target and the query's bigrams are all reachable from it — so #2 measures the seam, not the corpus | `…:TestKoreanDecoysShareNoBigramWithTheTarget` | unit | PASS | same |
| 4 | `Add`, `Update`, `Update`'s re-tokenization of replaced text, and the query side all pass through **one** function value | `…:TestTokenizeSeamIsSharedByIndexAndQuery` | unit | PASS | `go test -race -run TestTokenizeSeam ./pkg/engine/` |
| 5 | A directory committed with one tokenizer and opened with another is refused with `ErrTokenizerMismatch`, and reopening it with the right one answers the query | `…:TestTokenizerMismatchIsRefusedAtOpen` | integration | PASS | `go test -race -run TestTokenizerMismatch ./pkg/engine/` |
| 6 | A directory written with the default still opens with the default — the guard has no false positive on every directory weft has ever written | `…:TestDefaultTokenizerStillOpensADefaultDirectory` | integration | PASS | same |
| 7 | A corpus with no text to judge is admitted rather than refused or panicking | `…:TestGuardPassesACorpusWithNoTextToJudge` | integration | PASS | `go test -race -run TestGuard ./pkg/engine/` |
| 8 | A zero-value `Index` tokenizes with the default, on the read path and the write path both | `…:TestZeroValueIndexTokenizesWithTheDefault` | unit | PASS | `go test -race -run TestZeroValue ./pkg/engine/` |
| 9 | `engine.Tokenize` is unchanged and a default-constructed `Index` routes to it | `…:TestTokenizeIsStillThePackageDefault` | unit | PASS | same |
| 10 | Version 3 and version 4 segments still open with nothing converted | `pkg/engine/restore_test.go`, `pkg/engine/formatv2_test.go` | integration | PASS | `go test -race -run 'TestRestore\|TestFormatV2' ./pkg/engine/` |
| 11 | A doctored `terms` section still lets `Open` succeed and surfaces as absence — the guard did not take this over | `pkg/engine/lazy_test.go:TestALyingTermOffsetIsNeverFollowed` and two siblings | integration | PASS | `go test -race ./pkg/engine/` |
| 12 | `engine`'s exported surface changed by exactly the recorded 7 lines, and nothing outside `pkg/engine` gained a name | `pkg/engine/architecture_test.go:TestEngineAPISurfaceIsUnchanged`, `…:TestPublicAPISurfaceIsUnchanged` | golden | PASS | `make arch` |
| 13 | `engine` and `fusion` still name no scorer package | `…:TestNeitherEngineNorFusionImportsAScorer` | architecture | PASS | `make arch` |
| 14 | The rendered `go doc` answer compiles and produces the documented output | `pkg/engine/example_test.go:ExampleWithTokenizer` | example | PASS | `go test -run Example ./pkg/engine/` |

## Coverage and known gaps

Go has no configured coverage threshold in this repository and none is asserted;
`make all` — `fmt build vet test -race lint lint-docs` — is the gate, and
`make arch` and `make deps` are the architecture gates. What replaces a coverage
percentage here is the golden API file: a change to `engine`'s exported surface
fails a test rather than passing unnoticed.

Gaps, each deliberate and each published:

1. **The performance clauses are unjudged.** Not a coverage gap but the round's
   largest one. [FINDINGS §7](../FINDINGS.md), [FINDINGS §8 item 1](../FINDINGS.md),
   [PERF §5.6](../PERF.md).
2. **The guard cannot see a count-preserving tokenizer.** A stemmer changes every
   term and no count. Test #5 covers the large failure and nothing covers the small
   one, because nothing can at `Open` — [D-023](../DECISIONS.md).
3. **Test #4 asserts what reached the seam, not that nothing bypassed it.** A fifth
   call site added later that called `engine.Tokenize` directly would not fail it.
   Same shape as milestone 11's carried-forward note about the read-path filter
   being maintained by hand.
4. **The Korean trial is an existence proof, not a quality claim.** No Korean
   relevance judgements exist here; a bigram index over-matches and nothing
   measures that. Out of scope by the PRD.
5. **No test pins the guard's cost.** It is one `docLen` lookup, one record decode
   and one tokenization per `Open`, argued negligible against mapping the segments
   and not measured.

## Merge evidence

Seven checkpoint commits on `m13-tokenizer-seam`, in TDD order. If they are
squashed, this is the record:

| Commit | Stage | Evidence captured |
| --- | --- | --- |
| `f499db4` | register | Budget and invariants committed before any code |
| `e682709` | **RED** | Eight tests; package does not build; four names undefined |
| `d889e42` | **GREEN** | Seam lands; 7 of 8 pass; the 8th moves to an assertion failure |
| `5e64bf0` | **GREEN** | Guard lands; all 8 pass; v3/v4 fixtures and the three lazy-read tests stay green |
| `3b2faf4` | record | Actual spend, 7 lines against 5–7, written **before** the golden moved |
| `f290e49` | spend | `engine_api.txt` refreshed; `ExampleWithTokenizer` added |
| `9d05c16` | publish | D-022, D-023, `FORMAT.md` §4 and §8, README, ADOPTION, bench/README, changie, PRD |

No refactor commit. The one restructuring this round wanted — removing the guard's
third check — changed behaviour, so it is part of `5e64bf0` rather than a
behaviour-preserving cleanup, which it was not.
