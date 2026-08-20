# Milestone 7 — TDD evidence

**Source plan**: `.claude/plans/weft-m7.plan.md` (task 1, plus one defect found on the way to task 2)
**Branch**: `m7-baseline`
**Date**: 2026-08-20

This file covers **the code milestone 7 changes**. Tasks 3 to 6 are a measurement
campaign and the documents that publish it; they have no RED/GREEN cycle because they
produce numbers rather than behaviour. What they need instead is that their procedure
is committed before the first measurement, which is task 2's own pass line.

Two cycles are recorded:

1. **Task 1** — the progress line, planned.
2. **The load-point rule's ladder check** — not in the plan. Found while checking what
   code task 2's procedure leans on, and it turned out to lean on something untested
   that was wrong. D1 of the plan runs repetitions two and three as single-rung
   `-rate R` runs, and over fourteen hours a ladder will also get interrupted. Both
   paths went through a summary that could publish a rule's claim about a load point
   no rule selected.

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

### Defect — the load-point rule could not tell a ladder from some rungs

**Summary.** `docs/PERF.md` §3 rule 1 fixes which rung the headline is quoted at,
"because choosing that load after seeing the p99s is how a performance claim is made
to say whatever its author wants". The rule was registered before milestone 5's
numbers existed and was checked by no test. It had a hole.

`SaturationRate` and `HeadlineRate` answer for any slice handed to them. The
single-rung case — an operator's explicit `-rate` — was guarded at both call sites by
a length check each spelled for itself. **The interrupted case was not, and could not
be:** `bench` sliced its rates down to the rungs it reached (`rates[:len(p50s)]`)
before calling, so three rungs of five arrived indistinguishable from a complete
three-rung ladder. The summary then printed `saturation: not reached on this ladder`
about a ladder whose top two rungs never ran, and quoted a `HEADLINE` off the rung the
interrupt truncated mid-flight.

Milestone 7's campaign is 14.6 hours of these runs, and its D1 makes single-rung runs
routine rather than exceptional. Both are load-bearing.

**RED** — nine assertions across three targets, none of which compiled:

```
internal/loadgen/rule_test.go:88:14: undefined: RuleApplies
cmd/weft-eval/bench_test.go:55:79:  too many arguments in call to benchSummary
  have (*bytes.Buffer, string, []float64, []time.Duration, []benchReport, time.Duration)
  want (string, []float64, []time.Duration, []benchReport, time.Duration)
bench/summary_test.go:48:51:        too many arguments in call to summarize
  have (*bytes.Buffer, []rung, []float64, []time.Duration, time.Duration)
  want ([]rung, []float64, []time.Duration, time.Duration)
```

The `io.Writer` parameter is what makes any of this assertable. Printing to
`os.Stdout` is what the command wants and is also why the rule was never checked.
Checkpoint `b5476ba`.

**GREEN** — `loadgen.RuleApplies(rates, p50s)` is one predicate both summaries ask:
a ladder is at least two rungs and every rung intended. Both callers now pass the
rates they *intended* rather than the ones they reached. When the rule has no ladder,
both sides say how far the run got and suppress the claim — while still printing every
rung's own figures, because suppressing a claim is not suppressing a measurement and
repetitions two and three are read off exactly that path.

```
--- PASS: TestRuleAppliesOnlyToACompleteLadder (6 subtests)
--- PASS: TestBenchSummaryQuotesARuleSelectedHeadlineOnAFullLadder
--- PASS: TestBenchSummaryOnALadderThatNeverSaturatesQuotesItsTopRung
--- PASS: TestBenchSummaryPublishesNoHeadlineForALadderCutShort
--- PASS: TestBenchSummaryPublishesNoHeadlineForAnExplicitRate
--- PASS: TestBenchSummaryPrintsNothingWhenNothingWasMeasured
--- PASS: TestSummarizeQuotesARuleSelectedHeadlineOnAFullLadder
--- PASS: TestSummarizeOnALadderThatNeverSaturatesQuotesItsTopRung
--- PASS: TestSummarizePublishesNoHeadlineForALadderCutShort
--- PASS: TestSummarizePublishesNoHeadlineForAnExplicitRate
--- PASS: TestSummarizePrintsNothingWhenNothingWasMeasured
```

Checkpoint `5802032`.

**Observed on the corpus**, `make bench BENCHFLAGS='-rate 5 -rotations 2'`:

```
1 of 1 rungs measured — an explicit -rate, or a ladder cut short — so the load-point
rule has nothing to apply and there is no saturation point and no headline; sweep
with -rate 0 and let it finish to give the rule a ladder
rung       text  rate=5.00/s  p99=  --    p99 minus STW=  --    GC CPU 1.5%
```

**Why the fix is in `internal/loadgen` and not twice in the commands.** Rule 2 is a
ratio. A claim admitted on weft's side and suppressed on bleve's — or the reverse —
moves the published number without either engine moving. `bench/summary_test.go` is
the first test in that module and exists for that sentence.

**What this does not change.** No published milestone 5 figure moves: that ladder ran
to completion, so `RuleApplies` is true of it and the branch it takes is the one it
always took. What changes is what a *future* interrupted run is allowed to claim.

### Defect — the instrument could not see time the process did not run

**Summary.** The campaign's first ladder ran 08:11 to 22:03 by the wall clock and
reported 92 minutes of rungs. The lid had closed at 08:34:13, twenty-three minutes
into the first rung, and the machine slept and dark-woke for thirteen hours.

**Every figure in that report was plausible.** Each rung's elapsed matched its own
schedule to within a second, shed was zero below the knee, `majflt` was 0, and the
headline p99 came out 107.332 ms against milestone 5's 108.193 ms — a 0.8% agreement
that reads as reproduction. `time.Since` uses the monotonic clock, which on Darwin
does not advance during sleep. The run was caught only because a `date` happened to
be piped either side of the command.

**RED** — three targets, none compiled:

```
internal/loadgen/clock_test.go:57:14: undefined: unaccounted
internal/loadgen/clock_test.go:72:41: undefined: SuspendTolerance
cmd/weft-eval/bench_test.go:130:13:  benchReport has no field unaccounted
bench/summary_test.go:98:8:          rung has no field unaccounted
```

Checkpoint `7cc7185`.

**GREEN** — `loadgen.Elapsed` returns the monotonic span and the wall time it cannot
account for. `time.Time` carries both readings and `Sub` uses the monotonic one only
when both operands have it, so `Round(0)` strips it and the second subtraction is the
wall clock's answer to the same question. Past `SuspendTolerance` a rung prints
`SUSPENDED` as its **first** line — before the distribution, because a reader who
takes the table and stops has taken the wrong thing — and both summaries refuse the
run with what to do about it. That check runs **before** `RuleApplies`: a complete
five-rung sweep across a sleeping machine satisfies the ladder shape and would
otherwise be quoted. Every rung is inspected, not the headline's alone.

The tolerance is pinned from both sides: a 2 s NTP step must not flag, 12h20m must.
Checkpoint `84b0467`.

**Verified in the field.** The re-run under `caffeinate -dimsu` printed no `SUSPENDED`
line and closed with 100 m 48 s of wall clock against 100 m 39 s of rungs plus a 10 s
warm-up — nine seconds unaccounted, which is the report printing.

## Test specification

| # | What is guaranteed | Test | Type | Result | Evidence |
|---|---|---|---|---|---|
| 1 | Counting a completion allocates nothing, so progress reporting does not enlarge the heap the collector is measured against | `internal/loadgen/progress_test.go:TestProgressCountingDoesNotAllocateInTheRequestPath` | unit | PASS | `go test -race -run TestProgress ./internal/loadgen/` |
| 2 | The request path never writes, so no byte of reporting is inside a measured latency | `…:TestProgressCountingWritesNothingFromTheRequestPath` | unit | PASS | same |
| 3 | A line carrying completed count and total appears while the rung is still running | `…:TestProgressReportsCompletionsWhileTheRungRuns` | unit | PASS | same |
| 4 | `stop` is synchronous, so nothing writes after it returns and no line lands inside the rung's report | `…:TestProgressStopWaitsSoNothingWritesAfterIt` | unit | PASS | same |
| 5 | Every completion is counted under concurrent callers, in the shape `Drive` uses | `…:TestProgressCountsEveryConcurrentCompletion` | unit (`-race`) | PASS | same |
| 6 | Reporting can be switched off without panicking `time.NewTicker` | `…:TestProgressReportingCanBeSwitchedOffWithoutPanicking` | unit | PASS | same |
| 7 | A ladder is at least two rungs and every rung intended; anything else is not one | `internal/loadgen/rule_test.go:TestRuleAppliesOnlyToACompleteLadder` (6 cases) | unit | PASS | `go test -race ./internal/loadgen/` |
| 8 | A complete ladder still gets its rule-selected saturation point and headline | `cmd/weft-eval/bench_test.go:…QuotesARuleSelectedHeadlineOnAFullLadder`, `bench/summary_test.go:…` | unit | PASS | `go test ./cmd/weft-eval/`, `cd bench && go test ./...` |
| 9 | A ladder that never saturates says so and quotes the top rung actually applied | `…OnALadderThatNeverSaturatesQuotesItsTopRung` (both modules) | unit | PASS | same |
| 10 | An interrupted ladder publishes no headline and no saturation claim, and says how far it got | `…PublishesNoHeadlineForALadderCutShort` (both modules) | unit | PASS | same |
| 11 | An operator-chosen `-rate` publishes no rule label, but its rung's figures still print | `…PublishesNoHeadlineForAnExplicitRate` (both modules) | unit | PASS | same |
| 12 | A run with no completed rung prints nothing at all | `…PrintsNothingWhenNothingWasMeasured` (both modules) | unit | PASS | same |
| 13 | Wall time a span cannot account for is wall minus monotonic, clamped at zero | `internal/loadgen/clock_test.go:TestUnaccountedIsWallMinusMonotonic` (4 cases) | unit | PASS | `go test -race ./internal/loadgen/` |
| 14 | The tolerance admits a 2 s clock step and rejects 12h20m of sleep | `…:TestSuspendToleranceSeparatesClockJitterFromASleepingMachine` | unit | PASS | same |
| 15 | `Elapsed` reads two clocks rather than one twice | `…:TestElapsedReadsBothClocksOverARealSpan` | unit | PASS | same |
| 16 | A ladder that ran across a suspension publishes no headline and says how long the process was stopped | `…PublishesNoHeadlineWhenARungWasSuspended` (both modules) | unit | PASS | `go test ./cmd/weft-eval/`, `cd bench && go test ./...` |
| 17 | A gap inside the tolerance does not suppress the headline | `cmd/weft-eval/bench_test.go:…IsUnmovedByAGapInsideTheTolerance` | unit | PASS | `go test ./cmd/weft-eval/` |
| 18 | The engine is untouched by this milestone | `git diff --stat pkg/` | gate | PASS (empty) | `make all`, `make deps`, `make bench-build` |

## Coverage and known gaps

```
go test -cover ./internal/loadgen/   →  86.8% of statements
go tool cover -func …                →  Count 100.0%  Report 100.0%  RuleApplies 100.0%
                                        benchSummary 100.0%  summarize 100.0%
```

Above the 80% bar, and every function this milestone wrote or changed is fully
covered. The package figures for `cmd/weft-eval` and `bench/` are not quoted: both are
`package main` dominated by an index build and a load driver that need the 626 MiB
corpus, so a package percentage there measures how much of the command is testable
without it rather than how well the changed code is tested.

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
  `make bench-build`, `go test`, and two short `make bench` smoke runs were executed.
- **The scope widened by one defect, deliberately.** The plan's task 2 registers the
  campaign procedure; checking what code that procedure rests on turned up a rule that
  had never been asserted and was wrong for interrupted runs. Fixing it before the
  campaign is the point — after it, the same fix would be a change to the instrument
  between two measurements, which is what milestone 5 §4.1 had to throw two ladders
  away over. It is recorded here rather than folded silently into task 1.
- `make lint-docs` was not run: `markdownlint-cli2` is not installed on this machine.
  CI runs it.

## Merge evidence

If these three checkpoints are squashed, the summary that must survive is:

- **RED** `140013c` — five assertions against a type that did not exist; build failed
  with `undefined: Progress` at five call sites.
- **GREEN** `c0688fb` — `loadgen.Progress` added and wired into both commands; 5/5
  PASS under `-race`; `make all` clean; `pkg/` diff empty.
- **REFACTOR** `b57f7d4` — cadence constant moved to the shared package, dead accessor
  removed, guard case covered; coverage 86.7%, `progress.go` 100%.
- **RED** `b5476ba` — nine assertions that an interrupted ladder publishes no headline;
  three targets, none compiled (`undefined: RuleApplies`, two "too many arguments").
- **GREEN** `5802032` — `loadgen.RuleApplies` shared by both summaries, callers pass
  the intended ladder rather than the reached rungs; 11 tests PASS across three
  targets, `benchSummary` and `summarize` at 100%. No milestone 5 figure moves — that
  ladder completed, so it takes the branch it always took.
