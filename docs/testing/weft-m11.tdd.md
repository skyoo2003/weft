# Milestone 11 — TDD record: deletion and update

**Source plan**: `.claude/plans/weft-m11.plan.md` (not in the repository; `.claude/`
is gitignored). Its tasks are reproduced below where they are referenced.

This file is the index into what the tests prove. It is not a substitute for
them: every claim here names the test that carries it and the command that ran
it.

## 1. User journeys

Taken from the plan rather than invented here.

1. As an adopter, I want to delete an indexed document, so that a corpus which
   changes does not mean a full re-index.
2. As an adopter, I want to update a document under the same `Key`, so that the
   latest version is what gets searched.
3. As a scorer author, I want deleted documents kept out of my candidates
   **without my code knowing deletion exists**.
4. As an operator, I want a version 3 index to open unconverted, so that my data
   is not trapped by an upgrade.

Journey 3 is the milestone's experiment. The other three are the feature.

## 2. Cycle record

Checkpoint commits on `m11-delete-update`, in order. Each RED was verified before
the production code it names was written.

| # | Commit | Stage | Evidence |
| --- | --- | --- | --- |
| 1 | `6114e2d` | RED | `pkg/engine/dead_test.go:99:10: ix.Delete undefined` — compile-time, caused by the missing implementation |
| 2 | `65c0346` | GREEN | `go test -run 'TestNoScorerEverSees\|TestStatsExclude' ./pkg/engine/` → `ok 0.581s` |
| 3 | `81a9627` | GREEN | `go test ./pkg/engine/` → `ok 31.939s`; `go test -race ./...` all packages ok |
| 4 | `59c8db9` | RED | `ix.Update undefined`, `undefined: engine.ErrNoSuchKey` — compile-time |
| 5 | `698bc6f` | GREEN | `go test ./pkg/engine/` → `ok 33.272s`; `go test -race ./...` all ok |
| 6 | `4205c88` | verification | version-3 compatibility; went green with no production change, see §5 |
| 7 | `d64a6cf` | verification | tombstone-section corruption matrix; `parseDead` 73.1% → 92.3% |
| 8 | `e0cdf02` | docs | format v4, D-018, D-019, and `docs/PERF.md` §5.5 registered **before** any judging run |

Commits 2 and 3 both close parts of the RED raised in 1: the in-memory half first,
then the durable half. Between them the three persistence tests were **knowingly
red**, which the commit message for 2 states rather than leaves to be discovered.

**Deviation from the plan's task order.** The plan ran Update (task 3) before the
format work (tasks 4–6). It was done the other way because task 3's on-disk
consequence — one `Key` carried by two records — is task 6's problem, so
implementing Update first would have left it untestable through `Scrub`. Tasks 4
and 5 merged for the same reason: a delete-only commit is what forced the
manifest rule change, and neither is separately testable.

## 3. What the tests guarantee

| # | Guarantee | Test | Type | Result |
| --- | --- | --- | --- | --- |
| 1 | No deleted document reaches any of the four scorers, the fused result, or any of the seven read methods — pending, committed and merged | `dead_test.go:TestNoScorerEverSeesADeletedDocument`, `TestDeletionSurvivesACommit`, `TestDeletionAfterACommitSurvivesAReopen`, `TestDeletionSurvivesAMerge` | property, 8 seeds | PASS |
| 2 | Deletions survive `Close` and `Open`, in both orders (delete-then-commit, commit-then-delete) | `dead_test.go:TestDeletionSurvivesACommit`, `TestDeletionAfterACommitSurvivesAReopen` | integration | PASS |
| 3 | A merge neither resurrects a deleted document nor collects a live one | `dead_test.go:TestDeletionSurvivesAMerge` | integration | PASS |
| 4 | `Stats` drops tombstones from both the count and the token total; `Len` does not | `dead_test.go:TestStatsExcludeDeletedDocumentsAndLenDoesNot` | unit | PASS |
| 5 | Updating a pending document keeps its `DocID` and spends none | `delete_test.go:TestUpdateOfAPendingDocumentKeepsItsID` | unit | PASS |
| 6 | Updating a committed document spends an id and retires the old record | `delete_test.go:TestUpdateOfACommittedDocumentSpendsAnID` | integration | PASS |
| 7 | `Update` refuses an unknown or deleted key rather than inserting | `delete_test.go:TestUpdateOfAnUnknownKeyIsRefused` | unit | PASS |
| 8 | A key is reusable once its holder is a tombstone, within one uncommitted batch, and the resulting segment scrubs and resolves to the live record | `delete_test.go:TestAKeyIsReusableOnceItsDocumentIsDeleted` | integration | PASS |
| 9 | An update reads back across a commit boundary and a merge — new text, new term present, retired term absent | `delete_test.go:TestAnUpdateReadsBackAcrossCommitsAndMerges` | integration | PASS |
| 10 | A version 3 directory opens, answers identically to the v4 index it was made from, scrubs, and still takes writes | `ivf_test.go:TestAV3GenerationOpensAndAnswers` | compatibility | PASS |
| 11 | `MANIFEST` is stamped version 4, and a frame one version ahead is `ErrBadVersion` | `ivf_test.go:TestAVersionFourDirectoryIsRefusedByAnOlderReader` | compatibility | PASS |
| 12 | A version 2 directory still opens and ranks identically | `ivf_test.go:TestAV2SegmentOpensAndAnswersExactly`, `TestTheTwoReadersRankTheSame` | compatibility | PASS |
| 13 | The tombstone section refuses six malformed payloads, each named for the wrong answer it would otherwise produce | `deadfile_test.go:TestTombstoneListRefusesWhatNoWriterProduces` | unit, table | PASS |
| 14 | A lost tombstone file is `ErrCorrupt` from both `Open` and `Scrub`, not an empty set | `deadfile_test.go:TestALostTombstoneFileIsCorruptionRatherThanAnEmptySet` | integration | PASS |
| 15 | Every single-byte flip in a tombstone file is caught | `deadfile_test.go:TestAFlippedTombstoneByteIsCaught` | sweep | PASS |
| 16 | `pkg/fusion`, `public_api.txt` and every scorer implementation are unchanged; `engine_api.txt` grew by exactly 3 lines | `git diff --stat`, `architecture_test.go:TestEngineAPISurfaceIsUnchanged` | architecture | PASS |

Commands, as run:

```console
$ go test -race ./...
ok  github.com/skyoo2003/weft/cmd/weft-eval      2.241s
ok  github.com/skyoo2003/weft/internal/eval      6.308s
ok  github.com/skyoo2003/weft/pkg/engine        98.103s
ok  github.com/skyoo2003/weft/pkg/fusion         3.910s
ok  github.com/skyoo2003/weft/pkg/scorer/graph   2.686s
ok  github.com/skyoo2003/weft/pkg/scorer/recency 4.384s
ok  github.com/skyoo2003/weft/pkg/scorer/text    3.716s
ok  github.com/skyoo2003/weft/pkg/scorer/vector 12.077s

$ git diff --stat -- pkg/fusion/ pkg/engine/testdata/public_api.txt
(empty)
$ git diff --stat -- 'pkg/scorer/*/*.go' ':(exclude)pkg/scorer/*/*_test.go'
(empty)
```

## 4. Coverage

```console
$ go test -cover ./pkg/...
ok  github.com/skyoo2003/weft/pkg/engine         coverage: 88.5% of statements
ok  github.com/skyoo2003/weft/pkg/fusion         coverage: 100.0% of statements
ok  github.com/skyoo2003/weft/pkg/scorer/graph   coverage: 96.2% of statements
ok  github.com/skyoo2003/weft/pkg/scorer/recency coverage: 100.0% of statements
ok  github.com/skyoo2003/weft/pkg/scorer/text    coverage: 95.0% of statements
ok  github.com/skyoo2003/weft/pkg/scorer/vector  coverage: 90.9% of statements
```

`pkg/engine` was 88.0% before the corruption matrix and 88.5% after. Per new
function: `Delete` 83.3%, `Update` 91.7%, `replacePending` 92.3%, `setPosting`
100%, `dropPosting` 88.9%, `appendPending` 100%, `parseDead` 92.3%, `encodeDead`
100%, `deadSet.all` 100%.

## 5. Known gaps, stated rather than closed

1. **Two tests are verification, not drivers.** `TestAV3GenerationOpensAndAnswers`
   and the corruption matrix both went green with no production change — v3
   compatibility falls out of `minFormatVersion` and the version branch in
   `decodeMeta`, and `parseDead`'s refusals were written with the decoder. They
   are here because they are milestone metrics and because "by construction" is a
   claim, not because a failing test demanded them.
2. **No performance or quality run has happened.** `docs/PERF.md` §5.5 registers
   the procedure, the four readings and the cut order; **no number exists yet.**
   Until run A and run C are executed, the milestone's performance-invariant and
   quality-invariant clauses are *unjudged*, and nothing in this branch should be
   read as claiming otherwise.
3. **Run B needs a `-deletefrac` flag that does not exist.** The cost of a
   tombstone at query time — [D-019](../DECISIONS.md)'s open question — has no
   instrument.
4. **The read-path filter is maintained by hand.** Every read method filters
   today; nothing structural stops a future one from forgetting. A scorer outside
   this module reaching for such a method would see deleted documents.
   [FINDINGS milestone 11 §3](../FINDINGS.md) states this as the weakness of the
   architecture result.
5. **`Delete`'s 83.3%** is the double-delete branch and the damaged-segment
   branch of `resolveLive`; neither is exercised.
6. **`make changelog-check` was already failing on `main`** before this branch —
   `CHANGELOG.md` is stale relative to `changes/unreleased/` for the milestone 8
   and 9 entries. Not touched here.

## 6. Merge evidence

If the checkpoint commits are squashed, this file is the RED/GREEN record. The
two REDs that matter are `6114e2d` (`ix.Delete undefined`) and `59c8db9`
(`ix.Update undefined`, `undefined: engine.ErrNoSuchKey`), each a compile failure
caused by the intended missing implementation and each followed by a GREEN run of
the same target.
