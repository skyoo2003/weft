# Milestone 3b — TDD evidence

**Source plan:** [`.claude/plans/weft-m3b.plan.md`](../../.claude/plans/weft-m3b.plan.md)
**Branch:** `m3b-ivf`
**Date:** 2026-08-17

This is an index, not a substitute for the tests. It records what the test code
proves, and what was measured to prove it, so a squash merge or a new session does
not lose the answer to "what was verified, and how".

## Plan handling

The plan was read as data. Its validation section names `make all`, `make arch`,
`make deps`, `make fuzz`, `make lint-docs`, `go test`, `git diff --stat`,
`GOOS=windows go build`, `GOARCH=386 go vet`, `weft-eval build`, `make eval` and
`make recall` — all build, test or lint actions on this repository. No destructive
filesystem operation, no credential handling, no network installer, and no
instruction-to-agent override was present. Nothing needed rejecting.

Two decisions were left open by the plan and answered before any code was written:

- **(a) Cosine stays in the scorer.** `Index.Nearest` returns `[]DocID`, not
  `[]Candidate`. Recorded as [D-008](../DECISIONS.md).
- **(b) Two-version reader.** Format v3 reads v2 rather than shipping a converter.
  Recorded as [D-008](../DECISIONS.md) and [FORMAT.md §7.7](../FORMAT.md).

**One ambiguity in the plan is recorded rather than silently resolved.** It states
its quality bar two ways: "0.6233 대비 −0.005 이내" (Summary, Task 5) implies a pass
line of 0.6183, while the Validation block and the Acceptance checklist give the
literal figure `≥ 0.6228`, which is −0.0005. The measured 0.6211 falls between them,
so the two readings give opposite verdicts. The user was asked and chose −0.005
(0.6183); the reasoning recorded with that choice is that the prose states the rule
twice, and that a 0.0005 tolerance sits far inside the bootstrap interval's ±0.04
half-width and so cannot function as a statistical bar. **The plan document should be
corrected to say 0.6183.**

## User journeys

Taken from the plan; none invented.

1. As an operator with a corpus larger than memory, I want a vector query to touch a
   fraction of the corpus, so that "works past memory" is true of the vector path and
   not only the text path.
2. As an operator, I want ranking quality to survive the approximation within a
   tolerance fixed before the measurement, so that a speedup is not bought with an
   unreported quality loss.
3. As an operator holding a format v2 index, I want it to keep opening and ranking
   identically, so that upgrading weft does not require rebuilding my index.
4. As a maintainer, I want the same corpus to produce the same bytes twice, so that
   "this index changed" means the corpus changed.
5. As an operator, I want damage in the new section to cost speed rather than
   availability or correctness, so that a bad page is not a plausible wrong answer.
6. As a maintainer, I want the architecture claim to hold: `pkg/fusion` untouched and
   `pkg/scorer` changed only by the repayment.

## Task report

Each task is one RED commit and one GREEN commit on `m3b-ivf`, both reachable from
`HEAD`. The RED evidence is quoted from the run that preceded the fix.

### Task 1 — the partition, with no disk under it

`ivf.go`: spherical k-means with no RNG, sampled and seeded on a fixed stride.

- RED (`1b63507`): `go test -run TestIVF ./pkg/engine/` → `undefined: ivfMinDocs,
  buildIVF, ivfOrder, ivfNProbe, ivfNList`, `FAIL [build failed]`. Compile-time RED:
  the tests reference the code path that has to exist.
- GREEN (`fe638de`): same command, 6 tests pass. `recall@10 over 40 queries with
  nprobe=8 of nlist=91: 1.000`.
- Also measured, because the plan flagged build cost as a High risk: N = 148,232,
  d = 768 built in **1m1s**, inside the predicted one to two minutes. The throwaway
  harness was deleted after the measurement.

### Task 2 — the `ivf` section and the two-version reader

- RED (`8344177`): `undefined: ivfFile, segment.ivf, segment.nearest`, build failed.
- GREEN (`e8bb03a`): whole suite green, including every milestone 2 and 3a format
  test unmodified — the byte-flip sweep, the truncation sweep, the swapped-section
  check and `TestVersionOneIsRefused` all still pass with a seventh section present.

### Task 3 — `Index.Nearest`, commit and merge

- RED (`01a698b`): `Index.Nearest undefined`, build failed.
- GREEN (`9491bc5`): `go test ./...` green; `make arch` green. Exactly one golden
  edit: `+method Index.Nearest([]float32, int) []DocID`.

### Task 4 — the repayment, and its price tag

- RED (`1695f07`): `go test -run TestTheScanIsNarrowedByTheIndex ./pkg/scorer/vector/`
  → `3669728 B allocated per query, against 2097152 B of corpus vectors (175.0%)`,
  `FAIL`. Runtime RED: the scan was a full scan and the new test could see it.
- GREEN (`5d839f8`): same command passes at `338620 B (16.1%)`, and all twelve
  pre-existing contract tests pass **unmodified** — which is the evidence the metric
  never moved.
- Price tag, non-comment lines: `pkg/scorer/vector/vector.go` **4 removed, 3 added**,
  all in the loop header. `git diff --stat main -- pkg/fusion` → no output.

### Task 5 — the acceptance gates

`nprobe = 8` missed the quality bar at 0.6003. The plan's registered response was to
raise it and re-measure, which produced the curve in [FINDINGS §1](../FINDINGS.md)
and [EVAL §5.14](../EVAL.md). `nprobe = 64` was adopted.

Raising it invalidated both narrowing tests at their original corpus sizes, and the
reason is real rather than incidental: a constant `nprobe` cannot narrow a segment
that has fewer lists than `nprobe`, and at 8,192 documents `nlist` is 91. Both tests
moved to 65,536-document corpora, where `nlist` is 256 — and the scorer test's
baseline changed from a figure derived from the corpus's shape to a **measured full
scan of the same index**, which is a stronger comparison than the one it replaced.

### Task 6 — documentation

[FORMAT.md](../FORMAT.md) v3 with the `ivf` spec, the §5 rejection rows and §7.7's
rule for version 4; [FINDINGS milestone 3b](../FINDINGS.md); [D-008](../DECISIONS.md);
[EVAL §5.3 and §5.14](../EVAL.md); four changie entries; the PRD's M3 row.

Three rows were **removed** from FORMAT §8 rather than added: "Segments per
generation 1", "Commit cost O(corpus)" and "Load cost O(corpus)" were made false by
milestone 3a and had been left standing.

## Test specification

| # | What is guaranteed | Test | Type | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 1 | Two builds of one corpus produce bit-identical centroids and lists | `pkg/engine/ivf_test.go:TestIVFTrainingIsDeterministic` | unit | PASS | `go test -run TestIVF ./pkg/engine/` |
| 2 | Every vector is in exactly one list; vectorless and zero vectors are in none; empty lists survive | `ivf_test.go:TestIVFAssignsEveryVectorToExactlyOneList` | unit | PASS | ditto |
| 3 | nprobe lists find what a full scan finds on a clustered corpus | `ivf_test.go:TestIVFRecallOnClusteredCorpus` | unit | PASS, recall 1.000 | ditto |
| 4 | No partition below 4,096 documents, with no vectors, or on an empty corpus | `ivf_test.go:TestIVFIsNotBuiltWhereItCannotPay` | unit | PASS | ditto |
| 5 | `nlist` is √count clamped to 1024, exact at perfect squares | `ivf_test.go:TestIVFListCountFollowsSqrt` | unit | PASS | ditto |
| 6 | The centroid ranking is total, so widening the probe is a longer prefix | `ivf_test.go:TestIVFOrderIsBestFirstAndTotal` | unit | PASS | ditto |
| 7 | The section round-trips centroid bits and list membership | `ivf_test.go:TestIVFSectionRoundTrips` | integration | PASS | `go test -run TestIVF ./pkg/engine/` |
| 8 | An unpartitioned segment still writes the section, at constant size, and offers every id | `ivf_test.go:TestAnUnpartitionedSegmentStillWritesTheSection` | integration | PASS | ditto |
| 9 | A v2 generation opens under the v3 reader and answers exactly | `ivf_test.go:TestAV2SegmentOpensAndAnswersExactly` | integration | PASS | `go test -run TestAV2 ./pkg/engine/` |
| 10 | The v2 and v3 paths return the same ranking | `ivf_test.go:TestTheTwoReadersRankTheSame` | integration | PASS | `go test -run TestTheTwo ./pkg/engine/` |
| 11 | A segment mixing frame versions is `ErrCorrupt` | `ivf_test.go:TestASegmentMixingFormatVersionsIsRefused` | integration | PASS, 5 sections | `go test -run TestASegmentMixing ./pkg/engine/` |
| 12 | A v3 segment missing `ivf` is `ErrCorrupt`, not an older segment | `ivf_test.go:TestAV3SegmentMissingItsIVFSectionIsRefused` | integration | PASS | `go test -run TestAV3 ./pkg/engine/` |
| 13 | A damaged list falls back to the whole segment, never empty, never a panic; `Scrub` names it | `ivf_test.go:TestADamagedIVFListFallsBackToTheWholeSegment` | integration | PASS, 200 queries | `go test -run TestADamagedIVF ./pkg/engine/` |
| 14 | Two commits of one corpus write identical `ivf` bytes | `ivf_test.go:TestIVFBytesAreIdenticalAcrossTwoCommits` | integration | PASS | ditto |
| 15 | `Nearest` returns at least min(k, corpus) for every k | `nearest_test.go:TestNearestOffersAtLeastK` | integration | PASS, 6 values of k | `go test -run TestNearest ./pkg/engine/` |
| 16 | A query narrows the scan | `nearest_test.go:TestNearestNarrowsTheScan` | integration | PASS, 27.4% of 65,536 | ditto |
| 17 | Every candidate resolves, none repeats, order ascends, across three segments and pending | `nearest_test.go:TestNearestOffersOnlyIDsThatResolve` | integration | PASS | ditto |
| 18 | Uncommitted documents are never skipped | `nearest_test.go:TestNearestIncludesPendingDocuments` | integration | PASS | ditto |
| 19 | A wrong-width query gets every id, so the scorer can raise `ErrDimMismatch` | `nearest_test.go:TestNearestOnAWrongWidthOffersEverything` | integration | PASS | ditto |
| 20 | Empty and zero-value indexes answer with nothing | `nearest_test.go:TestNearestOnAnEmptyIndexIsEmpty` | unit | PASS | ditto |
| 21 | A merge rewrites a v2 run as v3 with a partition, scrubs clean, and moves no ranking | `nearest_test.go:TestMergeUpgradesAV2SegmentToV3` | integration | PASS | `go test -run TestMergeUpgrades ./pkg/engine/` |
| 22 | Two merges of one corpus write identical `ivf` bytes | `nearest_test.go:TestMergeIsByteDeterministicWithVectors` | integration | PASS | `go test -run TestMergeIsByteDeterministicWithVectors ./pkg/engine/` |
| 23 | A merge refuses to publish a partition built from bytes that would not read | `nearest_test.go:TestMergeRefusesAPartitionItCouldNotRead` | integration | PASS | `go test -run TestMergeRefusesAPartition ./pkg/engine/` |
| 24 | The scorer's scan is narrowed against a measured full scan of the same index | `pkg/scorer/vector/narrow_test.go:TestTheScanIsNarrowedByTheIndex` | integration | PASS, 27.2% | `go test ./pkg/scorer/vector/` |
| 25 | The scorer's twelve contract tests are unchanged by the repayment | `pkg/scorer/vector/vector_test.go` | unit | PASS, unmodified | ditto |
| 26 | Arbitrary bytes never panic either `ivf` decoder | `segment_test.go:FuzzSegmentDecoding`, `FuzzParseSection` | fuzz | PASS, 6.3M execs | `make fuzz` |
| 27 | Every byte flip and every truncation of the `ivf` file is `ErrCorrupt` | `segment_test.go:TestEveryByteFlipIsCaught`, `TestEveryTruncationIsCaught` | integration | PASS | `go test -run 'TestEvery' ./pkg/engine/` |
| 28 | `engine`'s exported surface grew by exactly one name | `architecture_test.go:TestEngineAPISurfaceIsUnchanged` | golden | PASS | `make arch` |

## Real-corpus measurements

171,332 documents, 148,232 with a 768-dimensional vector, `nlist` 414, 50 queries.

| Assertion | Command | Result |
| --- | --- | --- |
| Quality | `make eval` | `text+vector` **0.6211**, bar 0.6183 — **passes**. All five arms in [EVAL §5.14](../EVAL.md) |
| Recall | `make recall` | **0.992** at k=10 against a brute-force scan; worst query 0.800 |
| Candidates | `make recall` | 30,549 of 171,332 per query (17.8%) |
| Latency | `make recall` | 125 ms against 577 ms — **4.6×** |
| Working set | `make recall` | 124.1 MiB of records, **210.1 MiB** of distinct 4 KiB pages, of a 626.6 MiB `docs` |
| Determinism | `weft-eval build` twice | `ivf` `sha256 bc042260…b2b6d89` both times |
| Two-version reader | `make eval` on the pre-rebuild v2 index | `text+vector` **0.6233**, identical to milestone 3a |
| Architecture | `git diff --stat main -- pkg/fusion` | no output |
| Repayment size | `git diff main -- pkg/scorer/vector/vector.go` | 4 lines removed, 3 added |

## Gates

| Gate | Result |
| --- | --- |
| `make all` (fmt, build, vet, `go test -race ./...`) | PASS |
| `make arch` | PASS |
| `make deps` | PASS — one module, fusion imports no scorer |
| `make fuzz` | PASS — 30 s each target, no panics |
| `make lint-docs` | PASS via `npx markdownlint-cli2` (not installed locally; 0 issues) |
| `make changelog-check` | PASS |
| `GOOS=windows go build ./...` | PASS |
| `GOOS=linux GOARCH=386 go build ./...` | PASS |
| `GOOS=linux GOARCH=386 go vet` on every package this milestone touched | PASS |

## Coverage and known gaps

Stated rather than glossed, in decreasing order of how much they should bother a
reviewer.

1. **`GOARCH=386 go vet ./...` cannot run in full here, and neither failure is this
   milestone's.** On darwin the toolchain reports `default PIE binary requires
   external (cgo) linking` — reproduced on `main`. Under `GOOS=linux GOARCH=386` it
   fails on `pkg/fusion/rrf_test.go:311`, where a `1 << 40` constant overflows a
   32-bit `int`; that file is pre-existing and this milestone is required to leave
   `pkg/fusion` at a zero-line diff. Every package this milestone touched vets clean
   on `linux/386`, and the new 32-bit overflow surface the plan flagged —
   `nlist × dim` — is compared in `uint64` in `parseIVF` and in `ivfNList`.
2. **No coverage percentage was produced.** The repository has no coverage target and
   `.golangci.yaml` sets no threshold; introducing one for this milestone would be a
   new gate rather than evidence about this change. The table above is per-guarantee
   instead, which is what this repository's tests are organised around.
3. **`golangci-lint` was not run** — not installed locally, and CI pins v2.12.2.
   `gofmt -l`, `go vet` and the build are clean.
4. **Whether better centroids would buy back `nprobe` is untested on purpose.**
   Testing it means tuning k-means against 50 queries. Recorded in
   [FINDINGS §4.1](../FINDINGS.md) with the observable that would justify it.
5. **`Scrub` does not check that every vector-bearing document is in some list.**
   Deliberate; the cost is recall, which `weft-eval recall` measures directly.
   Recorded in [FORMAT §5](../FORMAT.md) and [FINDINGS §4.4](../FINDINGS.md).
6. **Neither the build nor `Nearest` can be cancelled.** `Commit` takes no context and
   the milestone was allowed one new exported name. [FINDINGS §4.3](../FINDINGS.md).

## Merge evidence

Checkpoint commits on `m3b-ivf`, in order, each reachable from `HEAD`:

```text
1b63507  test: add IVF partition reproducer for milestone 3b                     RED   task 1
fe638de  feat(engine): IVF-flat partition — spherical k-means without an RNG     GREEN task 1
8344177  test: add reproducers for the ivf section and the two-version reader    RED   task 2
e8bb03a  feat(engine): format v3 — the ivf section, and a reader for v2 and v3   GREEN task 2
01a698b  test: add reproducers for Index.Nearest and the merge upgrade path      RED   task 3
9491bc5  feat(engine): Index.Nearest — geometry in the engine, metric in scorer  GREEN task 3
1695f07  test: add reproducer for the scorer/vector full scan                    RED   task 4
5d839f8  fix(scorer/vector): repay ponytail:36 — the loop reads Index.Nearest    GREEN task 4
```

If these are squashed, this file is the surviving record of what was RED, what turned
it GREEN, and what was measured rather than assumed.
