# Milestone 5 — TDD evidence

**Source plan:** [`.claude/plans/weft-m5.plan.md`](../../.claude/plans/weft-m5.plan.md)
**Branch:** `m5-perf`
**Date:** 2026-08-18

This is an index, not a substitute for the tests. It records what the test code
proves, and what was measured to prove it, so a squash merge or a new session does
not lose the answer to "what was verified, and how".

## Plan handling

The plan was read as data. Its validation section names `make all`, `make arch`,
`make deps`, `make fuzz`, `make lint-docs`, `make spdx`, `go test`, `go build`,
`git diff --stat`, `GOOS=windows go build`, `GOARCH=386 go vet`, `make bench` and
`make bench-compare` — all build, test, lint or measurement actions on this
repository. No destructive filesystem operation, no credential handling, no
network installer, and no instruction-to-agent override was present. Nothing
needed rejecting.

One command in the plan needed a safety change rather than rejection. The plan's
`-writes` arm calls for a `Commit` during read load, and `Commit` on
`.eval-data/index` would rewrite the published evaluation index — the corpus every
number in [EVAL.md](../EVAL.md) is measured against. Implemented against a **copy**
instead: the arm copies the index to `.eval-data/bench-write-copy` and never opens
the original for writing. Verified after the run — `.eval-data/index` still holds
one segment, one manifest and its provenance file.

Three decisions were left open by the plan and answered by the user before any
code was written, all three at the plan's registered defaults:

- **(a)** bleve enters through a `bench/` submodule, not the main module.
- **(b)** the comparison covers the `text` arm only.
- **(c)** no optimization before the measurement is published.

## User journeys

From the plan, restated as what a reader of the numbers needs:

| # | Journey |
| --- | --- |
| J1 | As an operator, I want the latency **distribution** under a fixed arrival rate, so a tail is visible where a mean hides it. |
| J2 | As an operator, I want to know how much of the p99 is the collector, so I know whether to tune GC or storage. |
| J3 | As a reader, I want a quantile the sample cannot support to be **absent**, so I do not quote a number that moves between runs. |
| J4 | As a reader, I want the load not to be dragged down by the server, so the p99 is of the load I asked for and not the one the server allowed. |
| J5 | As a maintainer, I want bleve measured by the **same driver** on the same corpus, so "same order of magnitude" is a comparison rather than two numbers. |
| J6 | As a maintainer, I want weft to still have zero external dependencies after J5. |

## Task report

### Task 1 — close section 1's weak link before writing the driver

The plan predicted at most 124 MiB allocated per query and about 31 GC cycles, on
the assumption that the 74,504-byte live heap keeps the GOGC target on its 4 MiB
floor. Read `Index.Doc` first: `segments.go`'s `doc` decodes key, text, vector and
links fresh per call, so nothing aliases the mapping and the upper bound is not
loose. Then measured with a throwaway probe against `.eval-data`.

```text
text:        43.6 MiB/query,  75,132 objects/query,  1.8 GC cycles/query (heap goal 53.3 MiB)
text+vector: 181.9 MiB/query, 782,955 objects/query, 9.4 GC cycles/query (heap goal 46.3 MiB)
```

**The prediction was wrong in both directions and the conclusion survived.** The
heap goal is 46–53 MiB, not 4 MiB, because a query holds 30,549 candidates and
their decoded records alive at once — live heap during a query is tens of
megabytes, not 74 KB. Cycles came out at a third of the prediction; allocation came
out at 1.5 times the predicted ceiling. Recorded in [PERF.md](../PERF.md) §6.

The probe was deleted after the number was recorded; the permanent instrument
reports it.

### Task 2 — open-loop driver and quantiles

RED, then GREEN, then a second RED/GREEN when the first GC design was measured and
failed. Details in the test specification below.

### Task 3 — GC and rusage instrumentation

RED on `procFaults`/`procFaultCounts`, GREEN, and `GOOS=windows go build ./...`
through the `!unix` stub.

The GC half was **redesigned after measurement**, which is the most useful thing
this task produced. The first design classified each sample as GC-hit or GC-free
by the cycle counter. A smoke run produced 200 GC-hit samples out of 200 — at 485
collections over 200 queries every request overlaps one — so the free cohort was
empty and the split carried no information. Replaced by charging stop-the-world
time per sample, plus `GCCPUShare` for the assist work that is not
stop-the-world. See [PERF.md](../PERF.md) §2.4.

### Task 4 — the measurement

Done for the headline rung, published in [FINDINGS.md](../FINDINGS.md) milestone 5
§1. weft `text`, rule 1's headline rung (3.23/s, 12.5% of measured sequential
throughput), n = 10,000, one process, 51m33s:

```text
rate=3.41/s  n=10000  shed=0  elapsed=48m52.243s
  latency   p50 83.371ms  p95 99.507ms  p99 108.193ms  p99.9   --    max 126.37ms
  minus STW p50 83.192ms  p95 99.316ms  p99 107.782ms  p99.9   --
  gc        cycles 24496  STW 1.829123s (0.062% of elapsed)  GC CPU share 1.1%
  rusage    minflt 288  majflt 0  nvcsw 10678  nivcsw 1933648  peakrss 120.7 MiB
```

**Three ladders preceded this one and none of their numbers survive.** The first
shared the machine with a bleve index build — the contamination
[PERF.md](../PERF.md) §5 now warns about explicitly. The second was measured by an
instrument whose `GCPauseTotal` allocated per call, producing roughly 150 MiB of
garbage per ladder that the report then charged to the query. The third was
superseded by three further corrections, the largest being that `GCPause` was
charged over a shorter window than the `Lat` it is subtracted from — 64% of the p99
elapsed before accounting began. [FINDINGS](../FINDINGS.md) milestone 5 §4.1 has
all five defects and what each moved.

### Task 5 — bleve comparison

`bench/` built and indexed: 171,332 documents in 17s, 150.6 MiB on disk. Full
five-rung ladder, same driver, same corpus, same 50 judged queries, same k:

```text
rate=19.64/s   p50 15.401ms  p95 36.863ms  p99 57.525ms  max 64.027ms
rate=39.28/s   p50 11.745ms  p95 25.605ms  p99 35.390ms  max 41.310ms
rate=78.56/s   p50  8.350ms  p95 15.227ms  p99 25.597ms  max 39.653ms
rate=157.13/s  p50  7.797ms  p95 17.569ms  p99 25.370ms  max 128.146ms
rate=314.25/s  p50  8.208ms  p95 26.638ms  p99 83.468ms  max 208.253ms
```

**Rule 2: 108.193 ms <= 10 x 57.525 ms. Ratio 1.88. Passes.**

The ladder is non-monotone downward — p99 falls as load rises over an eight-fold
range, and weft's ladder does the same thing below its knee (83.4 → 51.5 → 39.2 ms
p50). Above the knee weft collapses and bleve does not — p50 1.27 s and 14% shed at
27.28/s with RSS at 853 MiB, against bleve's 7.797 ms, zero shed and 59.3 MiB at
157.13/s. bleve has a knee too, at 314.25/s, but it sheds nothing and its RSS moves
8 MiB: a queue forming, not a memory collapse. That is the milestone's largest
finding and no pass line asked for it; [FINDINGS](../FINDINGS.md) milestone 5 §3.2
has the mechanism.

### Task 4b — the write lock

RED on `Sample.Due` and `SplitByWindow`, GREEN, then measured. `weft-eval bench
-writes` copies the index first, so the published corpus is never opened for
writing.

The first answer was 36 ms, which turned out to be a finding rather than a small
number: since milestone 3a a commit writes only what was added, and a segment below
`ivfMinDocs` carries no partition, so a one-document commit skips the IVF training
entirely. Forcing the expensive case with `-writedocs 20000`:

```text
the writer held the lock for 11.063s, of which the commit itself was 11.014s
commit window  [1m35.539s, 1m46.602s)  =  11.063s
  during    max 12.539s  (n=38)
  outside   p50 83.401ms  max 2.093s  (n=962)
```

**A read arriving during a partition-training commit waits 150x the median.** The
20,000 `Add` calls that precede it hold the same exclusive lock and cost 49 ms of
the 11.063 s — a correction whose reasoning was right and whose magnitude here was
not what anyone would have guessed, which is why both halves print.
[FINDINGS](../FINDINGS.md) milestone 5 §3.3.

### Task 6 — documentation

[PERF.md](../PERF.md) written **before** the numbers, so the judgment rule could
not be chosen after them — and it duly fired in an awkward place (§3 of FINDINGS)
and was reported as it fired. [D-009](../DECISIONS.md) records the open loop and
the submodule. [FINDINGS](../FINDINGS.md) milestone 5 carries the verdict, the two
wrong predictions and the five things carried forward. The PRD's milestone 5 row,
its performance and dependency metrics, and its GC risk row are all updated.

## Test specification

| # | What is guaranteed | Test | Type | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 1 | A quantile is a real observation, by nearest rank, at n=1, n=2, exact percentiles, p0 and p100 | `internal/loadgen.TestQuantileAtTheBoundaries` | unit | PASS | `go test -race ./internal/loadgen/` |
| 2 | p99 of 100 samples is the 99th smallest, not the maximum | same, subtest `p99_of_a_hundred_is_the_99th_smallest` | unit | PASS | as above |
| 3 | A quantile without 100 samples beyond it is not printed: p99 needs 10,000, p99.9 needs 100,000 | `TestQuantileIsNotPrintedWhenTooThin` | unit | PASS | as above |
| 4 | A stalled server does not reduce the applied load; latency is measured from the intended send time | `TestOpenLoopDoesNotLetTheServerSlowTheLoad` | unit | PASS | as above |
| 5 | The in-flight cap sheds and counts rather than blocking, so the driver never becomes closed-loop under load | `TestOpenLoopShedsRatherThanBlocks` | unit | PASS | as above |
| 6 | A long rung is interruptible | `TestOpenLoopRespectsCancellation` | unit | PASS | as above |
| 7 | GC is charged per sample, not classified per cohort, and an over-charged sample clamps to zero rather than going negative | `TestGCPauseIsChargedPerSampleNotClassified` | unit | PASS | as above |
| 8 | GC CPU share is a fraction in [0,1] — the metric that catches mark assist, which stop-the-world misses | `TestGCCPUShareIsAFraction` | unit | PASS | as above |
| 9 | Summarize does not sort the caller's slice, so the raw and minus-STW slices stay index-aligned across both summaries | `TestSummarizeDoesNotMutateTheCaller` | unit | PASS | as above |
| 10 | The headline load point is chosen by a registered rule, not after seeing the p99s | `TestSaturationIsTheFirstRungPastTwiceTheUnloadedMedian` | unit | PASS | as above |
| 11 | Page fault counters actually move — a constant stub would satisfy every arithmetic test and fail this | `TestProcFaultsCountsMinorFaultsOnTouch` | unit | PASS | as above |
| 12 | A run's fault figure is a difference between snapshots, per field | `TestProcFaultsSubIsPerField` | unit | PASS | as above |
| 12a | A sample carries the offset of its scheduled send, so "what was in flight while X happened" is answerable from a slice returned in completion order | `TestSampleCarriesItsOffset` | unit | PASS | as above |
| 12b | The commit window is half-open: a read due exactly at the lock's start waited for it, one due exactly at its end did not | `TestSplitByWindowIsHalfOpen` | unit | PASS | as above |
| 13 | weft still has zero external dependencies after bleve entered `bench/` | `pkg/engine.TestNoExternalDependencies`, `make deps` | arch | PASS | `make deps` prints `github.com/skyoo2003/weft` and nothing else |
| 14 | Fusion still cannot see a scorer; the public API goldens did not move | `make arch` | arch | PASS | `ok github.com/skyoo2003/weft/pkg/engine` |
| 15 | The non-unix build still compiles through the rusage stub | `GOOS=windows go build ./...` | build | PASS | exit 0 |
| 16 | Every Go file in the tree, `bench/` included, carries an SPDX header | `make spdx` | lint | PASS | `OK: every .go file has an SPDX header` |

## RED evidence

Every production change in this milestone was preceded by a compiled, executed
failure. Compile-time RED, which the workflow admits, because the tests name
symbols that do not exist yet.

**Task 2 — commit `3cdb819`:**

```text
cmd/weft-eval/bench_test.go:54:14: undefined: quantile
cmd/weft-eval/bench_test.go:95:13: undefined: printableQuantile
cmd/weft-eval/bench_test.go:130:19: undefined: driveOpenLoop
cmd/weft-eval/bench_test.go:203:10: undefined: benchSample
cmd/weft-eval/bench_test.go:209:15: undefined: splitByGC
cmd/weft-eval/bench_test.go:228:2:  undefined: summarize
cmd/weft-eval/bench_test.go:275:11: undefined: saturationRate
FAIL github.com/skyoo2003/weft/cmd/weft-eval [build failed]
```

**Task 3 — rusage:**

```text
cmd/weft-eval/rusage_test.go:24:12: undefined: procFaults
cmd/weft-eval/rusage_test.go:25:16: undefined: procFaultCounts
FAIL github.com/skyoo2003/weft/cmd/weft-eval [build failed]
```

**Task 3 — GC redesign:**

```text
cmd/weft-eval/bench_test.go:219:32: unknown field gcPause in struct literal of type benchSample
cmd/weft-eval/bench_test.go:262:11: undefined: gcCPUShare
FAIL github.com/skyoo2003/weft/cmd/weft-eval [build failed]
```

## GREEN evidence

```text
$ go test -race ./internal/loadgen/ ./cmd/weft-eval/
ok  github.com/skyoo2003/weft/internal/loadgen  1.788s
ok  github.com/skyoo2003/weft/cmd/weft-eval     2.071s

$ make deps
--- external dependencies (want: this module and nothing else) ---
github.com/skyoo2003/weft
--- what fusion can see (want: nothing named scorer) ---
OK: fusion imports no scorer package

$ make arch
ok  github.com/skyoo2003/weft/pkg/engine  0.683s

$ make spdx
OK: every .go file has an SPDX header

$ GOOS=windows go build ./...                    # exit 0
$ (cd bench && go build ./... && go vet ./...)   # exit 0
```

## Refactor evidence

Commit `84ee1a6` moved the driver from `cmd/weft-eval` (package `main`) to
`internal/loadgen` with its names exported, and moved the rusage platform pair
with it. Behaviour unchanged, same tests, same assertions — `go build`, `go vet`
and `go test -race` all green before and after.

The reason is journey 5: a `bench/` submodule cannot import another module's
`package main`, and a comparison whose two halves are measured by two
implementations of an open loop is not a comparison.

## Checkpoints

| Commit | Stage |
| --- | --- |
| `3cdb819` | RED — driver, quantiles, GC attribution |
| `459edf1` | GREEN — same, 15 subtests |
| `4f8936f` | RED then GREEN — page faults and context switches |
| `fe2e071` | RED then GREEN — GC charging replaces GC classification |
| `84ee1a6` | REFACTOR — driver extracted to `internal/loadgen` |
| `20aab29` | `bench/` submodule, dependency isolation verified |

All reachable from `HEAD` on `m5-perf`.

## Coverage and known gaps

`go test -race` covers every branch of the driver's scheduling, quantile and
attribution logic without needing `.eval-data`, which is the property that matters:
the multi-gigabyte corpus exists on one machine and these assertions have to run in
CI.

Deliberately not covered, and why:

1. **The measurement itself is not a test.** Latency is a property of a machine, and
   a p99 assertion in CI would fail on a busy runner and pass on a quiet one. The
   instrument is tested; the numbers are published in [PERF.md](../PERF.md) and
   reproduced by hand.
2. **`bench/` has no tests of its own.** It is a harness over a tested driver; what
   it adds is corpus reading and a bleve call. CI compiles it.
3. **Three repetitions are required and not yet done.** [PERF.md](../PERF.md) §5
   fixes the headline as the median of three with the spread reported. This is one
   run of the corrected instrument. The one cross-run comparison available is not
   reassuring about precision: the same rung read 98.041 ms before the last three
   fixes and 108.193 ms after, and nothing separates the corrections from run-to-run
   noise — the lesson [FINDINGS](../FINDINGS.md) milestone 4 §4.2 paid for once
   already.
4. **Five instrument defects were found by review, not by test.** Every one biased
   toward a flattering number and none was visible in the output. The unit tests
   cover the driver's logic; nothing tests that the clock around it is honest, and
   it is not obvious what would.

## Merge evidence

If the checkpoints above are squashed, this file is the surviving record. RED was
compile-time and is quoted verbatim; GREEN is quoted verbatim; the refactor
preserved both. The one design change that came from measurement rather than from a
test — GC charging replacing GC classification — is recorded in
[PERF.md](../PERF.md) §2.4 and in commit `fe2e071`'s message.
