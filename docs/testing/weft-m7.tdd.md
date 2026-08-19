# Milestone 7 — TDD evidence

**Source plan**: `.claude/plans/weft-m7.plan.md` (task 1 of six)
**Branch**: `m7-baseline`
**Date**: 2026-08-20

This file covers **task 1 only**. Tasks 2 to 6 are a measurement campaign and the
documents that fix its rules; they have no RED/GREEN cycle because they produce
numbers rather than behaviour. What they need instead is that their procedure is
committed before the first measurement, which is task 2's own pass line.

## Why this report is short, and where the rest of the milestone's proof lives

Milestone 7 writes almost no code. Its deliverable is a baseline nobody has to
qualify: three repetitions of the headline rung with the spread published, and a p99
for the `text+vector` arm, which has never had one. Neither is a thing a test can
assert — the assertion is the published number and the rule that selected it, and
those live in [PERF.md](../PERF.md) and [FINDINGS.md](../FINDINGS.md).

What *is* code is one progress line, and it earns a test for a specific reason:
the obvious implementation of it corrupts the measurement the whole milestone exists
to make trustworthy.

## User journey

Taken from the plan's D3 rather than written fresh:

> As the maintainer, I want a running rung to say how far it has got, so that I can
> tell a live forty-nine-minute rung from a hung one — **without the reporting
> becoming part of what the rung measures.**

The second clause is the whole design. `do` runs on the goroutine `Drive` spawned for
that request, so a `fmt.Printf` inside it — the obvious "print every thousandth
request" — is inside the window `Sample.Lat` measures, along with any wait on the
lock behind stdout. A p99 is the hundredth slowest sample of ten thousand, so ten
contaminated samples land in the region the milestone publishes.

This is [FINDINGS](../FINDINGS.md) milestone 5 §4.1 in a different place: `GCPauseTotal`
allocated a histogram per call, made ~150 MiB of garbage per ladder, and the report
charged the collections that garbage provoked to the query. Two ladders were thrown
away. The same sentence applies here before the fact rather than after it.

## Task report

### Task 1 — the progress line stays out of the request path

**Summary.** `internal/loadgen.Progress` splits the work by which goroutine runs it:
`Count` wraps the request and does one atomic add; `Report` runs a ticker on its own
goroutine and does all the writing. `benchRung` (`cmd/weft-eval`) and `measureRung`
(`bench/`) both wrap their `do` and stop the reporter immediately after `Drive`
returns — before the counters are read, so the rung is not charged with its own
progress lines and no line lands inside the distribution table.

**RED** — `go test -race ./internal/loadgen/`, before `progress.go` existed:

```
internal/loadgen/progress_test.go:62:8: undefined: Progress
internal/loadgen/progress_test.go:79:8: undefined: Progress
internal/loadgen/progress_test.go:97:8: undefined: Progress
internal/loadgen/progress_test.go:132:8: undefined: Progress
internal/loadgen/progress_test.go:155:8: undefined: Progress
FAIL	github.com/skyoo2003/weft/internal/loadgen [build failed]
```

Compile-time RED. The tests newly reference a type with no implementation; the
failure is the missing production code, not a broken fixture. Checkpoint `140013c`.

**GREEN** — `go test -race -run TestProgress ./internal/loadgen/`:

```
--- PASS: TestProgressCountingDoesNotAllocateInTheRequestPath (0.00s)
--- PASS: TestProgressCountingWritesNothingFromTheRequestPath (0.00s)
--- PASS: TestProgressReportsCompletionsWhileTheRungRuns (0.01s)
--- PASS: TestProgressStopWaitsSoNothingWritesAfterIt (0.07s)
--- PASS: TestProgressCountsEveryConcurrentCompletion (0.00s)
ok  	github.com/skyoo2003/weft/internal/loadgen	1.499s
```

Checkpoint `c0688fb`.

**Refactor** — the interval was a constant in each command. That is the duplication
[PERF.md](../PERF.md) §2 gives as the reason the two commands share one driver, so it
moved to `loadgen.ProgressEvery`. `Progress.Done` was dropped: an accessor with no
caller. One test was added for the switched-off case, which is a guard and not a
feature — `time.NewTicker` panics on a non-positive duration. That test is written
against behaviour that already existed and is a characterisation test, not a RED/GREEN
cycle; it is recorded here as such rather than counted as one. Checkpoint `b57f7d4`.

**End to end, on the evaluation corpus** — `make bench BENCHFLAGS='-rate 5 -rotations 4'`:

```
cold  n=50  total=1.851s  worst=60.881ms  minflt=3193  majflt=713
unloaded: p50 36.21ms over 200 requests, so 27.6 q/s sequentially

weft  text  warm  n=200/rung  inflight=40  GOMAXPROCS=10
  ... 150/200 done, 30s elapsed

rate=5.00/s  n=200  shed=0  elapsed=39.848s
  latency   p50 55.724ms  p95   --    p99   --    p99.9   --    max 87.049ms
```

The line appears while the rung runs and is finished before the table starts. Two
incidental readings worth keeping: the sequential throughput came out at 27.6 q/s
against milestone 5's 27.28 q/s, so the machine is in a comparable state to the one
that produced the numbers this milestone is about to re-measure; and `p95` onward
print as `--`, which is `Printable` refusing an unsupported quantile on a 200-sample
rung — the same rule that makes task 5 of the plan necessary.

## Test specification

| # | What is guaranteed | Test | Type | Result | Evidence |
|---|---|---|---|---|---|
| 1 | Counting a completion allocates nothing, so progress reporting does not enlarge the heap the collector is measured against | `internal/loadgen/progress_test.go:TestProgressCountingDoesNotAllocateInTheRequestPath` | unit | PASS | `go test -race -run TestProgress ./internal/loadgen/` |
| 2 | The request path never writes, so no byte of reporting is inside a measured latency | `…:TestProgressCountingWritesNothingFromTheRequestPath` | unit | PASS | same |
| 3 | A line carrying completed count and total appears while the rung is still running | `…:TestProgressReportsCompletionsWhileTheRungRuns` | unit | PASS | same |
| 4 | `stop` is synchronous, so nothing writes after it returns and no line lands inside the rung's report | `…:TestProgressStopWaitsSoNothingWritesAfterIt` | unit | PASS | same |
| 5 | Every completion is counted under concurrent callers, in the shape `Drive` uses | `…:TestProgressCountsEveryConcurrentCompletion` | unit (`-race`) | PASS | same |
| 6 | Reporting can be switched off without panicking `time.NewTicker` | `…:TestProgressReportingCanBeSwitchedOffWithoutPanicking` | unit | PASS | same |
| 7 | The engine is untouched by this milestone | `git diff --stat pkg/` | gate | PASS (empty) | `make all`, `make deps`, `make bench-build` |

## Coverage and known gaps

```
go test -cover ./internal/loadgen/     →  86.7% of statements
go tool cover -func …                  →  progress.go: Count 100.0%, Report 100.0%
```

Above the 80% bar, and `progress.go` is fully covered.

**Gaps, deliberate:**

1. **The wiring in `benchRung` and `measureRung` has no unit test.** Both need a
   mapped 626 MiB index to run, which is the kind of test
   `internal/loadgen/loadgen_test.go` opens by refusing — it would run on this machine
   and nowhere else. What covers it is the end-to-end smoke run above, quoted rather
   than summarised.
2. **The bleve side is not smoke-run here.** `make bench-build` compiles it and the
   change is the same three lines; the run itself belongs to task 4, where it is
   measured rather than smoke-tested.
3. **No test asserts the reporter is stopped before the counters are read.** That
   ordering is a comment and a line position in two files, and the thing it protects
   against — a few progress lines' worth of allocation charged to a rung — is below
   what any assertion here could resolve. It is named in both files so that a later
   edit that moves it has to argue with something.

## Plan interpretation notes

- The plan put the progress code in `cmd/weft-eval/bench.go` and `bench/main.go`
  separately. It went into `internal/loadgen` instead, which both already import: one
  implementation, and the "does not allocate" idiom this package already has. The
  plan's constraint that `Drive`'s signature not change is kept — `Progress` is a new
  type beside it, not a parameter on it.
- The plan said no test was needed for task 1. That was wrong in a way worth
  recording: the property that matters is not "a line is printed" but "the line is not
  inside the measurement", and that is exactly a testable guarantee. Test 1 is the one
  that would have caught the design the plan was trying to avoid.
- The plan's task 3 to 6 shell commands are a measurement campaign, not validation
  steps, and were not run as part of this cycle. Only `make all`, `make deps`,
  `make bench-build`, `go test`, and one two-minute `make bench` smoke run were
  executed.

## Merge evidence

If these three checkpoints are squashed, the summary that must survive is:

- **RED** `140013c` — five assertions against a type that did not exist; build failed
  with `undefined: Progress` at five call sites.
- **GREEN** `c0688fb` — `loadgen.Progress` added and wired into both commands; 5/5
  PASS under `-race`; `make all` clean; `pkg/` diff empty.
- **REFACTOR** `b57f7d4` — cadence constant moved to the shared package, dead accessor
  removed, guard case covered; coverage 86.7%, `progress.go` 100%.
