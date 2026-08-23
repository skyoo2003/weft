# Milestone 8 — TDD evidence

**Source**: no plan file. Milestone 8's pass line — *shed 0 at 27.28 q/s* — is under
re-specification in [the PRD](../../.claude/prds/weft-hardening.prd.md), because
[FINDINGS](../FINDINGS.md) milestone 7 §4.5 showed one rate both passing and failing
it. This cycle answers the question that blocks the plan rather than doing the
milestone's engineering work. The journey below is taken from
[FINDINGS](../FINDINGS.md) milestone 7 §6 carried-forward 1.
**Branch**: `m7-baseline`
**Date**: 2026-08-21

Three things are recorded: one RED/GREEN cycle for the instrument the first question
needs, the campaign that instrument was built for — which has no such cycle and says what
stands in for one instead — and a second RED/GREEN cycle for the defect the campaign's
last run found in the report itself.

## Why a milestone about throughput starts with a flag

Milestone 7 measured 25.67 q/s three times and got 37.9 ms, 1.539 s and 416 ms. The
one structural difference that survived every check in
[FINDINGS](../FINDINGS.md) milestone 7 §2 and §3: the flat observation was the fourth
rung of a ladder, ninety-one minutes into the process; the two collapses were that
rate alone, out of a 200-request warm-up.

Deciding between the two readings — a GC pacer arriving with a heap goal already grown,
or `inflight` 40 admitting a burst at rung start — needs **the same arrival rate behind
two different prefixes.** `-rate 0` cannot express that: the sweep derives its five
rates from whatever that run's sequential throughput happens to be, so the with-prefix
run and the without-prefix run land on different rates and the prefix is confounded
with the load. Hence a flag that names the rates.

The flag arrives with its own hazard, and that hazard is what earned the cycle. A
four-rung list someone typed satisfies **every shape check** `loadgen.RuleApplies`
makes — two or more rungs, all of them intended — so it would be quoted as though
[PERF.md](../PERF.md) §3 rule 1 had selected the load point from a sweep. Rule 1 exists
against exactly that act, and choosing the rungs then letting the rule pick among them
is the same act at one remove.

## User journey

> As the maintainer, I want to run the same arrival rate behind two different ladder
> prefixes, so that I can tell whether a rung's collapse is a property of the load or
> of the process history — **without a ladder I typed being publishable as a
> rule-selected load point.**

The second clause is the whole design, the same shape as milestone 7's progress line:
the useful thing and the thing that corrupts the record are one edit apart.

## Task report

### The named ladder, and the label it may not wear

**Summary.** `-rates 3.21,6.42,12.84,25.67` runs those rates in order. `benchRates`
now answers two questions instead of one — which rates run, and whether the run may
say a rule chose among them — and returns them together, because deriving provenance
at the bottom of `benchSummary` would be a second spelling of the same condition in
the one place where there is nothing left to read it off. Three sources, one of which
earns a headline: `loadgen.Ladder` scaled by this run's sequential throughput is the
sweep rule 1 was written about; a single `-rate` and a named `-rates` list are both the
operator's choice. Each entry in the list is held to the bounds a lone `-rate` gets,
and held to them at flag time rather than at dispatch.

**RED** — re-verified rather than quoted from the checkpoint message. A worktree at
`4fd7663`, the test-only commit, then `go test ./cmd/weft-eval/`:

```text
# github.com/skyoo2003/weft/cmd/weft-eval [github.com/skyoo2003/weft/cmd/weft-eval.test]
cmd/weft-eval/bench_test.go:67:11:  o.rates undefined (type benchOpts has no field or method rates)
cmd/weft-eval/bench_test.go:112:21: assignment mismatch: 2 variables but benchRates returns 1 value
cmd/weft-eval/bench_test.go:112:59: too many arguments in call to benchRates
    have (number, []float64, "time".Duration)
    want (float64, "time".Duration)
cmd/weft-eval/bench_test.go:128:20: too many errors
FAIL    github.com/skyoo2003/weft/cmd/weft-eval [build failed]
```

Compile-time RED in three shapes: a field that does not exist, a return arity that does
not match, and — past the `too many errors` cut — `benchSummary` without the
`ruleLadder` parameter the last assertion passes it. The failures are the missing
production code, not a broken fixture. Checkpoint `4fd7663`.

**GREEN** — `go test -race -run 'TestBenchFlags|TestBenchRates|TestBenchSummary' ./cmd/weft-eval/`:

```text
--- PASS: TestBenchFlagsParsesAnExplicitLadder (0.00s)
--- PASS: TestBenchFlagsRefusesAnExplicitLadderBesideAnExplicitRate (0.00s)
--- PASS: TestBenchFlagsRefusesARateInsideALadderItWouldRejectAlone (0.00s)   [9 subtests]
--- PASS: TestBenchRatesPrefersTheExplicitLadderAndDisownsTheRule (0.00s)
--- PASS: TestBenchSummaryPublishesNoHeadlineForAnOperatorChosenLadder (0.00s)
--- PASS: TestBenchSummaryQuotesARuleSelectedHeadlineOnAFullLadder (0.00s)
--- PASS: TestBenchSummaryOnALadderThatNeverSaturatesQuotesItsTopRung (0.00s)
--- PASS: TestBenchSummaryPublishesNoHeadlineForALadderCutShort (0.00s)
--- PASS: TestBenchSummaryPublishesNoHeadlineWhenARungWasSuspended (0.00s)
--- PASS: TestBenchSummaryIsUnmovedByAGapInsideTheTolerance (0.00s)
--- PASS: TestBenchSummaryPublishesNoHeadlineForAnExplicitRate (0.00s)
--- PASS: TestBenchSummaryPrintsNothingWhenNothingWasMeasured (0.00s)
ok      github.com/skyoo2003/weft/cmd/weft-eval    1.486s
```

Twelve, of which five are new: the seven `benchSummary` cases milestone 7 wrote are in
the same run because `benchSummary` grew a parameter, and a guard added to that function
is only worth something if the claims it already makes are unmoved. Checkpoint
`07bcee7`.

**No refactor checkpoint.** The one decision that could have been left for a cleanup
pass — provenance carried out of `benchRates` against provenance re-derived inside
`benchSummary` — was taken in the GREEN commit, and the reason is in `benchRates`' own
doc comment rather than here.

**Field, on the evaluation corpus** — `make bench BENCHFLAGS='-rates 5,10 -rotations 2'`:

```text
weft  text  warm  n=100/rung  inflight=40  GOMAXPROCS=10

rate=5.00/s  n=100  shed=0  elapsed=19.862s
  latency   p50   --    p95   --    p99   --    p99.9   --    max 82.032ms
rate=10.00/s  n=100  shed=0  elapsed=9.942s
  latency   p50   --    p95   --    p99   --    p99.9   --    max 67.811ms

2 of 2 rungs measured — a ladder you named, an explicit -rate, or a ladder cut short —
so the load-point rule has nothing to apply and there is no saturation point and no
headline; sweep with -rate 0 and let it finish to give the rule a ladder
rung       text  rate=5.00/s  p99=  --    p99 minus STW=  --    GC CPU 1.4%
rung       text  rate=10.00/s  p99=  --    p99 minus STW=  --    GC CPU 2.3%
```

Both named rungs ran, in the order given, at the rates given. This is the case the
shape check cannot catch: two rungs, both intended, so `RuleApplies` is **true** and the
suppression comes entirely from provenance. Every quantile prints `--` because 100
samples is below the floor [PERF.md](../PERF.md) §2.3 sets even for a p50 — that is the
sample rule doing its job on a deliberately short smoke run, not the flag.

### The campaign — no RED/GREEN, and what stands in for one

**Summary.** Four runs at 25.67 q/s, 2026-08-21 22:10 to 2026-08-22 00:25 KST, about
two hours of exclusive machine time. The prefix reading held: reached as the fourth rung
of a ladder run to 10,000 samples per rung, the rate that gave 37.9 ms and 1.539 s in
milestone 7 gave **37.827 ms with shed 0** — 0.07% from its first observation, with the
collector's cycle count 0.12% apart. Verdict, all four runs and every caveat are
[FINDINGS milestone 8](../FINDINGS.md); the repair it licensed is
[D-013](../DECISIONS.md).

**A measurement has no RED gate**, and inventing one would be theatre. What it has
instead is a pass line of its own, and it is the same one milestone 7's task 2 was held
to: **the procedure and the interpretation of every outcome are committed before the
first run.** That commit is `86df620` — [PERF.md](../PERF.md) §5.2, with all four
outcomes written down — and the first run started after it. `git log --oneline -- docs/PERF.md`
is where that order is checkable, and it is checkable on purpose.

The pass line held in the direction that costs something. Clause 4 said that if the lone
rung ran flat this time, the variable was not named yet and the published run count
stayed at one. It did not run flat, so clause 4 was not needed — but clause 1's registered
wording was *"at both `inflight` values"* and only one was run, so the outcome is reported
as **half-tested** rather than as fired clean ([FINDINGS milestone 8 §5.1](../FINDINGS.md),
[PERF.md](../PERF.md) §5.2's outcome note). Two further deviations — a reorder and a cut
probe — are recorded in the same two places.

**What the instrument's own tests bought here.** Three of the five assertions from the
cycle above ran for real in this campaign rather than in a test binary: every one of the
four runs printed *"a ladder you named ... no saturation point and no headline"*, so the
most reproducible figure this project has is published as a rung and not as a headline.
That is test 5 holding under the conditions it was written for, and the tension it creates
is [D-013](../DECISIONS.md)'s second half rather than a defect.

### Defect — a rung printed another rung's peak as its own

**Summary.** The campaign's last run judged milestone 8's pass line, *RSS ≤ 250 MiB at
27.28 q/s*, against **345.2 MiB** printed at that rung — which was the mark set two rungs
earlier at 13.64 q/s, half the rate. `ru_maxrss` is a high-water mark the kernel never
lowers, `benchReport`'s comment has said so since milestone 5, and the line has printed
`(process)` the whole time. None of that stopped a registered pass line from being written
as a per-rung threshold, or this milestone from trying to judge one.

`benchRung` now reads the mark before the rung as well as after, and the report prints the
difference: what **this** rung raised it by, or that it raised nothing and the figure is
therefore not its own. That is the only per-rung memory statement `getrusage` supports —
[FINDINGS milestone 8 §8](../FINDINGS.md) and [PERF.md](../PERF.md) §2.7 say what it still
cannot decide, rather than inventing a per-rung peak.

**RED** — `go test ./cmd/weft-eval/`:

```text
cmd/weft-eval/bench_test.go:327:3: unknown field rssRaised in struct literal of type benchReport
cmd/weft-eval/bench_test.go:331:10: too many arguments in call to r.print
    have (*bytes.Buffer)
    want ()
cmd/weft-eval/bench_test.go:351:3: unknown field rssRaised in struct literal of type benchReport
cmd/weft-eval/bench_test.go:355:10: too many arguments in call to r.print
FAIL    github.com/skyoo2003/weft/cmd/weft-eval [build failed]
```

Compile-time RED: a field that does not exist and a method arity that does not match. The
`io.Writer` on `print` is what makes the line assertable at all, which is the same reason
and the same move milestone 7 made on `benchSummary`. Checkpoint `5360ad1`.

**GREEN** — `go test -race -run TestBenchReport ./cmd/weft-eval/`:

```text
--- PASS: TestBenchReportSaysWhichRungRaisedThePeak (0.00s)
--- PASS: TestBenchReportRefusesToLetAnEarlierRungsPeakReadAsItsOwn (0.00s)
ok      github.com/skyoo2003/weft/cmd/weft-eval    1.516s
```

Checkpoint `dd33365`.

**Field** — `make bench BENCHFLAGS='-rates 5,10 -rotations 2'`:

```text
rusage  ... peakrss 116.1 MiB (process, raised 0.2 MiB by this rung)
rusage  ... peakrss 117.6 MiB (process, raised 1.5 MiB by this rung)
```

**The fix postdates the run it was found by**, and that ordering is deliberate: the run is
published as it was measured ([FINDINGS milestone 8 §7](../FINDINGS.md)) and no figure in
that file moves. Changing the instrument first would have put a measurement and its
instrument in the wrong order, which is what milestone 5 §4.1 discarded two ladders over.

### A query may not allocate text it never reads

**Summary.** `engine.Index.Doc` decodes a whole record per candidate — key, text, links —
and the vector scorer reads one field of it. Asserted as a property rather than a number,
because the three routes to fixing it differ in mechanism and all of them satisfy it: a
query's allocation must scale with the vectors it reads, not with the corpus text it does
not.

**RED** — `go test -run TestScoringDoesNotAllocate ./pkg/scorer/vector/`, and exact:

```text
--- FAIL: TestScoringDoesNotAllocateTheCorpusTextItNeverReads (0.14s)
    scoring allocated 4208200 bytes on a corpus whose text is 4194304 bytes larger,
    against 13312 on the small one: the scan is materialising text it never reads
    (budget was small+419430)
```

Runtime RED. The scan materialises **one full copy of the text**, to within the slack the
budget allows. The test commits a synthetic corpus and reopens it, because `engine.New()`
holds documents as values and never decodes — the cost under test exists only on the mapped
path. Checkpoint `67afd97`, and that commit deliberately contains **no** fix: all three
routes move something registered, and picking one to unblock the work is what
[D-012](../DECISIONS.md) refuses. The branch was left red until the route was chosen.

**GREEN** — route 1, an additive read accessor, chosen by the operator and argued in
[D-015](../DECISIONS.md):

```text
ok      github.com/skyoo2003/weft/pkg/scorer/vector    0.476s
```

`decodeDocFields` is the one place the record's layout is written down; the mode changes
what is copied out of it and nothing about what is read or checked. Checkpoint `895bd83`.

**The correction that matters more than the fix.** This was written while chasing §7's
206.6 MiB and does not explain it: every ladder in this milestone ran the `text` arm, where
no scorer calls `Doc` at all. [FINDINGS milestone 8 §9](../FINDINGS.md) carries it. The
property above stands on its own evidence; the 345.2 MiB is still unattributed.

**The invariant spent.** The golden API file gains one line. `pkg/fusion` diff is empty,
`public_api.txt` does not move, `go list -m all` is one line, and no existing signature
changes.

**Toolchain.** The goenv shim broke mid-session — it points `GOROOT` at a Go that is not
installed — so every command above ran under `env -u GOROOT /opt/homebrew/opt/go/bin/go`
(go1.27.0). `make all` and `make deps` fail through the shim for that reason and were run
by hand as `gofmt -l`, `go vet ./...`, `go test -race ./...` and the two `go list` checks
the `deps` target makes.

## Test specification

| # | What is guaranteed | Test | Type | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 1 | A comma-separated list parses in the order given, whitespace and all — rates get copied out of a report and a report has spaces in it | `cmd/weft-eval/bench_test.go:TestBenchFlagsParsesAnExplicitLadder` | unit | PASS | `go test -race -run 'TestBenchFlags\|TestBenchRates\|TestBenchSummary' ./cmd/weft-eval/` |
| 2 | `-rate` beside `-rates` is refused, rather than one of them winning silently | `…:TestBenchFlagsRefusesAnExplicitLadderBesideAnExplicitRate` | unit | PASS | same |
| 3 | Every entry gets the bounds a lone `-rate` gets, at flag time: non-numeric, negative, zero, empty, `NaN`, `Inf` and past the 1e9 ceiling, alone or inside a list | `…:TestBenchFlagsRefusesARateInsideALadderItWouldRejectAlone` (9 cases) | unit | PASS | same |
| 4 | The named ladder is what runs, and the run reports that no rule selected among its rungs | `…:TestBenchRatesPrefersTheExplicitLadderAndDisownsTheRule` | unit | PASS | same |
| 5 | A four-rung ladder an operator typed publishes no saturation point and no headline, though it satisfies every shape check `RuleApplies` makes, and its rungs' own figures still print | `…:TestBenchSummaryPublishesNoHeadlineForAnOperatorChosenLadder` | unit | PASS | same |
| 6 | Milestone 7's `benchSummary` guarantees are unmoved by the new parameter — rule-selected headline, never-saturates, cut short, suspended, gap inside tolerance, explicit `-rate`, nothing measured | `…:TestBenchSummary*` (7 tests) | regression | PASS | same |
| 7 | A rung that raised the process peak says by how much, so the figure can be attributed to it | `…:TestBenchReportSaysWhichRungRaisedThePeak` | unit | PASS | `go test -race -run TestBenchReport ./cmd/weft-eval/` |
| 8 | A rung that raised the peak by nothing says the mark is not its own, so it cannot be read against a per-rung threshold | `…:TestBenchReportRefusesToLetAnEarlierRungsPeakReadAsItsOwn` | unit | PASS | same |
| 9 | A query's allocation scales with the vectors it reads, not with corpus text it never touches | `pkg/scorer/vector/vector_test.go:TestScoringDoesNotAllocateTheCorpusTextItNeverReads` | unit (mapped corpus) | PASS | `go test -run TestScoringDoesNotAllocate ./pkg/scorer/vector/` |
| 10 | `Index.Vector` answers exactly what `Doc`'s vector field does — on both sides of a commit, and false on documents carrying none | `pkg/engine/lazy_test.go:assertReadAPIsAgree` (every caller of it) | unit | PASS | `go test -race ./pkg/engine/` |
| 11 | `pkg/fusion` and the module graph are untouched by the memory work | `git diff --stat pkg/fusion/`, `go list -m all`, `go list -deps ./pkg/fusion` | gate | PASS (empty / one line / no scorer) | run by hand; see **Toolchain** above |

## Coverage and known gaps

```text
go test -coverprofile … ./cmd/weft-eval/   →  45.0% of statements (package)
go tool cover -func …                      →  benchRates      100.0%
                                              benchSummary    100.0%
                                              rssAttribution  100.0%
                                              benchFlags       84.6%
                                              print            77.8%
```

The package figure is not the metric, for the reason milestone 7's report gives: this
is `package main` around an index build and a load driver that need the 626 MiB corpus,
so a package percentage measures how much of the command runs without it. The three
functions this change touched are the row that matters.

**Gaps, deliberate:**

1. **`benchFlags` at 84.6%.** The uncovered branches are the flag validations that
   predate this change — `-rotations`, `-rate`'s own bounds, `-writedocs`, `-arm`, and
   the `flag` package's own error — plus one guard this change added that no input can
   reach: `len(o.rates) == 0` after a loop over a non-empty `-rates`, which
   `strings.Split` cannot produce. The whitespace-only case in test 3 exits through
   `ParseFloat` instead. The guard is kept because the loop's exit conditions are three
   lines away and a later edit that adds a `continue` would reach it.
2. **`bench/` does not get the flag.** The bleve module is the comparison arm for rule
   2, and the prefix question is about weft's own collapse. Adding the flag there for
   symmetry would be a change to the comparison instrument between two measurements,
   which is what milestone 5 §4.1 threw two ladders away over.
3. **No test asserts the four runs of [PERF.md](../PERF.md) §5.2 differ.** That is a
   measurement, and its procedure is registered rather than asserted — the same
   division milestone 7's report draws between its tasks 1 and 2.
4. **`make lint-docs` was not run**: `markdownlint-cli2` is not installed on this
   machine (`command -v` returns nothing). CI runs it. The changelog entries were
   validated with `changie batch patch --dry-run`, which rendered them.
5. **`print` at 77.8%.** The two branches the new tests do not enter are the `SUSPENDED`
   line — asserted through `benchSummary` instead, where it changes a verdict — and the
   no-`getrusage` omission, which is a characterisation of behaviour that predates this
   cycle rather than something it introduced.
6. **No test asserts `rssRaised` is wired correctly in `benchRung`.** It is one
   subtraction between two `loadgen.MaxRSS()` reads, and asserting it needs the 626 MiB
   corpus — the same gap as gap 1's family. The field smoke run above is what covers it,
   quoted rather than summarised.

## Interpretation notes

- **There is no plan to interpret.** Milestone 8's pass line is not yet a predicate, so
  no plan exists to derive tasks from; the journey came from
  [FINDINGS](../FINDINGS.md) milestone 7 §6 and the PRD's first open question. When the
  pass line is re-specified and a plan is written, this cycle is a prerequisite it
  already has rather than a task inside it.
- **This cycle does not choose rule 3's replacement**, and [D-012](../DECISIONS.md)
  is why: the repair may not be picked inside the milestone whose numbers falsified the
  rule, when three candidate repairs are visible and the reason to prefer one is which
  run it would make look reproducible. What is registered instead is the experiment and
  what each of its four outcomes licenses — [PERF.md](../PERF.md) §5.2, committed before
  the experiment runs, which is the same ordering milestone 7's task 2 was held to.
- **The flag is not a general feature.** It exists for one question, and that question's
  arms are written into §5.2 so that a later `-rates` run has to say which arm it is.

## Merge evidence

If these checkpoints are squashed, the summary that must survive:

- **RED** `4fd7663` — five assertions against a field, a return arity and a parameter
  that did not exist; `go test ./cmd/weft-eval/` failed to build. Re-verified in a
  worktree at that commit rather than quoted.
- **GREEN** `07bcee7` — `-rates` parses and validates at flag time, `benchRates` returns
  provenance beside the rates, `benchSummary` suppresses the rule's label for a ladder
  the operator named. 12 tests PASS under `-race`, `benchRates` and `benchSummary` at
  100%. `pkg/` diff empty.
- **Registration** — [PERF.md](../PERF.md) §3 rule 1 gains the third case the shape
  check cannot see, and §5.2 registers the four-run experiment with what each outcome
  licenses, before it runs.
- **Campaign** `a662dfc` — four runs, 25.67 q/s reproduced to 0.07% behind a deep prefix,
  rule 3 repaired, D-013. Then the pass-line run: shed 0 and p50 37.631 ms met at
  27.28 q/s, RSS undecidable.
- **RED** `5360ad1` — two assertions that a rung may not print another rung's peak as its
  own; build failed on `unknown field rssRaised` and `too many arguments in call to
  r.print`.
- **GREEN** `dd33365` — `rssRaised` is the mark's increase across the rung, `print` takes
  an `io.Writer`, and a rung that raised nothing says the figure is not its own. 2/2 PASS
  under `-race`, `rssAttribution` 100%, field-verified. `pkg/` diff empty, and the fix
  postdates the run that found it.

---

<!-- markdownlint-disable-next-line MD025 -->
# Milestone 8, second cycle — the guess, and the instrument that refused it

**Source**: [`.claude/plans/weft-m8.plan.md`](../../.claude/plans/weft-m8.plan.md).
The **Interpretation notes** above say *"There is no plan to interpret"* — that was true
when written and is no longer. The pass line has since been re-specified,
[D-014](../DECISIONS.md) recorded the memory clause as a miss, and a plan exists whose
first two tasks are the cycles below.
**Branch**: `m8-memory`
**Date**: 2026-08-23

The plan's design note D2 required attribution to be measured in two places rather than
one, because [FINDINGS milestone 8 §9](../FINDINGS.md) is a memory figure explained by a
mechanism from an arm that was never measured. **The second place refused the first.**
That is what this cycle is: a fix that passed its own property test, and a rung
measurement that rejected it before any 97-minute campaign was spent on it.

## User journeys

From the plan, not invented here:

> As an adopter sizing a container for weft, I want a query's memory cost to scale with
> what it matched, not with how large the corpus is, so that peak RSS is a function of
> load rather than of corpus size.
>
> As the maintainer judging milestone 8's memory clause, I want each rung to report what
> one query allocated, so that a drop in the ladder's peak mark can be attributed rather
> than inferred.

The second journey is the one that survived. The first is now a **known trade** rather
than a defect, and the second task report is why.

## Task report

### A rung says what one query allocated

**Summary.** `benchRung` reads `runtime.MemStats` either side of the rung and the report
prints `TotalAlloc` and `Mallocs` differences divided by the samples that did the work.
Per query rather than per rung, because a rung total is a function of how long the rung
ran and a per-query figure is a property of the work — which is the one a claim about a
fix can be held against. Shed requests are not in the denominator: the driver sheds by
never dispatching, so a shed request allocated nothing and would only dilute the figure.

Both reads sit outside the measured window, the first before the rung's start timestamp
and the second after `Elapsed` has been taken, and the second is **last** in the
after-snapshot block. That placement is the same argument milestone 5 §4.1 cost two
ladders: `ReadMemStats` is the only read in that block that stops the world, so any
counter read after it would be charged with this instrument's own stop.

**RED** — `go test ./cmd/weft-eval/`:

```text
cmd/weft-eval/bench_test.go:380:3: unknown field allocBytes in struct literal of type benchReport
cmd/weft-eval/bench_test.go:381:3: unknown field allocs in struct literal of type benchReport
cmd/weft-eval/bench_test.go:408:3: unknown field allocBytes in struct literal of type benchReport
cmd/weft-eval/bench_test.go:409:3: unknown field allocs in struct literal of type benchReport
FAIL    github.com/skyoo2003/weft/cmd/weft-eval [build failed]
```

Compile-time RED, the same two shapes the `rssRaised` cycle above produced. Checkpoint
`35f03e5`.

**GREEN** — `go test -race -run TestBenchReport ./cmd/weft-eval/`:

```text
--- PASS: TestBenchReportSaysWhichRungRaisedThePeak (0.00s)
--- PASS: TestBenchReportRefusesToLetAnEarlierRungsPeakReadAsItsOwn (0.00s)
--- PASS: TestBenchReportSaysWhatAQueryAllocated (0.00s)
--- PASS: TestBenchReportOmitsTheAllocationLineWhenNothingWasMeasured (0.00s)
ok      github.com/skyoo2003/weft/cmd/weft-eval    1.502s
```

Four, of which two are new; the two from the earlier cycle are in the same run because
`print` grew a line, and a line added to that function is only worth something if the
claims it already makes are unmoved. Checkpoint `fcc5a97`.

**Field** — `make bench BENCHFLAGS='-rates 5,10 -rotations 2'`:

```text
rate=5.00/s   ... alloc 19501.6 KiB/query  15915 allocs/query  (1904.5 MiB this rung)
rate=10.00/s  ... alloc 19501.6 KiB/query  15915 allocs/query  (1904.5 MiB this rung)
```

Two rungs, identical to the decimal, and that is the evidence the figure is not a
distribution: 50 judged queries replayed twice is the same work both times, so allocation
is a deterministic count rather than a sample. It is quotable at `n=100` for exactly that
reason, where no latency quantile on the same line is — [PERF.md](../PERF.md) §2.3's floor
applies to the quantiles beside it and not to this.

### A query may not allocate a map entry per document it never matches — measured, and reverted

**Summary.** `pkg/scorer/text` hints its BM25 accumulator at the corpus size. The hint is
charged in full whether the query matches every document or eight of them: about 28 bytes
of buckets per document, **4.52 MiB per query** on the 171,332-document corpus, live on
every one of 40 requests in flight. The plan's D1 named this as the localised target for
[D-014](../DECISIONS.md)'s 345.2 MiB miss.

**RED** — `go test -run TestScoringDoesNotAllocate ./pkg/scorer/text/`, and exact:

```text
--- FAIL: TestScoringDoesNotAllocateForDocumentsTheQueryNeverMatches (0.14s)
    text_test.go:421: scoring allocated 593816 bytes over 16384 documents against 4672
    over 64, for the same eight matches: the accumulator is sized by the corpus rather
    than by what the query found (budget was small+45696)
```

Runtime RED, 36 bytes per document the query never looks at. Written as a property for the
same reason `pkg/scorer/vector`'s is: three routes to fixing it differ in mechanism and all
three satisfy it. Checkpoint `314feea`, containing no fix.

**GREEN** — hint taken from the first non-empty posting list instead:

```text
scored the same eight matches for 2520 bytes over 16384 documents and 2520 over 64
```

Identical, because the corpus no longer enters the figure — 236× less on the larger
corpus. `pkg/scorer/text`, `pkg/scorer/vector`, `pkg/engine` and `pkg/fusion` all PASS
under `-race`. Checkpoint `79f2a5d`.

**REVERTED** — checkpoint `b7e981d`, on the strength of the instrument built one section
above. Same corpus, same 50 queries, two rotations, only the scorer differing:

| accumulator hint | KiB/query | allocs/query |
| --- | --- | --- |
| corpus-sized (before) | 15,691.2 | 15,487 |
| first posting list (after) | **19,501.6** | 15,915 |

**The replaced comment was right.** A hint from the first posting list is too small for a
TREC-COVID query, whose term union really is most of the corpus, so the map doubles its
way up and every abandoned table is charged to the query: **+3.7 MiB**, about one extra
copy of the final map. Worse for this pass line specifically, because during a growth the
old table and the new one are live **together**, and the clause under judgement is a peak.

Choosing the other side of the trade does not remove it — a narrow query still pays
4.52 MiB for a map holding eight entries. What would remove it is a posting count in the
terms index, and `termSpan` carries byte offsets only, so no cheaper hint is reachable
without decoding the list first.

**What the instrument said that no guess had.** The hint was never the dominant term:
15.7 MiB per query with 4.5 MiB of it the map. **The remaining ~11 MiB and 15,487
allocations per query are unattributed.** The PRD names "30,549 candidates decoded per
query" as the cause; that is the vector arm, and every figure here is the text arm — the
same mismatch [FINDINGS milestone 8 §9](../FINDINGS.md) already corrected once. Naming
them needs a heap profile, which the plan registered as the escalation rather than the
first step.

So [FINDINGS milestone 8 §9](../FINDINGS.md)'s sentence stands unchanged: **the 345.2 MiB
remains unattributed.** This cycle is the second wrong guess about it, kept in history
rather than deleted.

**No campaign ran.** The plan's tasks 4 through 8 — registering the procedure in
[PERF.md](../PERF.md) §5.3, the 97-minute judgment ladder, `make eval`, the repetitions
and the verdict — are all downstream of a fix, and there is no longer a fix to judge.
Spending 4.9 hours of machine time to measure a reverted change is the failure
[PERF.md](../PERF.md) §3 rule 5 clause 4 exists to prevent, one step earlier than usual.

### The escalation, run — and the 15.7 MiB is now attributed

**Summary.** `-memprofile` writes the `allocs` profile, which is cumulative since process
start and therefore answers by call site what the rung line answers only in total. That
cumulativeness is also why it costs nothing to ask: a **20-second** run attributes an
allocation that happens once per query, and no ladder is needed.

**RED** — `go test ./cmd/weft-eval/`:

```text
cmd/weft-eval/bench_test.go:437:13: undefined: writeAllocProfile
cmd/weft-eval/bench_test.go:473:7:  o.memprofile undefined (type benchOpts has no field or method memprofile)
FAIL    github.com/skyoo2003/weft/cmd/weft-eval [build failed]
```

Two missing production symbols and nothing else — the first attempt also failed on
`undefined: filepath` and `undefined: os`, which is a broken fixture rather than a RED, so
the imports were added and the gate re-run before any production code was written.
Checkpoint `992cfe0`.

**GREEN** — `go test -race -run 'TestWriteAllocProfile|TestBenchFlagsParsesAMemProfilePath' ./cmd/weft-eval/` — ok.

**Field** — `make bench BENCHFLAGS='-rates 10 -rotations 4 -memprofile /tmp/allocs.pb.gz'`,
200 samples, `alloc 15691.3 KiB/query  15488 allocs/query`, then
`go tool pprof -sample_index=alloc_space -top`:

| call site | alloc_space | alloc_objects |
| --- | --- | --- |
| `engine.(*segment).lookup.func1` — the `make([]Posting, 0, n)` a `Lookup` builds | **53.2%** | — |
| `text.(*Scorer).Candidates` (flat) — the accumulator map and the candidate slice | **44.3%** | 4.7% |
| `engine.decodeTermPostings` + `(*segReader).unit` | 1.4% | **59.4%** |
| everything else, index mapping included | ~1% | ~36% |

**Both halves of the byte total are whole-set materialisation, and neither is the map hint
alone.** `Lookup` decodes a term's entire posting list into a fresh slice per term per
query — 8.3 MiB of the 15.7 — and `Candidates` then holds an entry per matching document
in a map and again in a slice. `scanPostings` beside it **already streams**, because Merge
needs it to; `lookup` is the only caller that builds the slice, and the text scorer is the
only caller of `lookup` that could iterate instead.

The allocation *count* is a different function than the byte total: 59% of it is two small
objects per posting decoded inside `decodeTermPostings` and `segReader.unit`, the latter
being a `what + " checksum"` string built on every call and read only on an error path.
That is 0.9% of the bytes, so it is a GC-pacing cost rather than a peak-RSS one, and this
milestone's clause is a peak.

**What this changes about the target.** [D-014](../DECISIONS.md) called the miss
"localised" and named the cause as 30,549 candidates decoded per query. The cause is now
measured, it is on the text arm, and it is **whole-posting-list materialisation** rather
than the candidate decode — the same *shape* of cause the PRD names, in a different place.
Removing it is a streaming read of postings, which is what milestone 10's "sorted
traversal, no full materialisation" describes.

### The target the profile named — one buffer per query, not one list per term

**Summary.** `Index.LookupInto` is `Lookup` writing into a caller-owned buffer.
`pkg/scorer/text` keeps one across a query's terms, so what is live is the **longest**
posting list rather than the **sum** of them — and the sum is what a peak is made of.

A buffer and not the iterator milestone 10's sentence describes, and
[D-016](../DECISIONS.md) carries the argument: a segment that claims a term and cannot
decode it makes the whole lookup absent (D-006), and that verdict arrives *after* some
postings are decoded. Postings in a buffer can be discarded; yielded ones cannot.

**RED** — both halves, `527701c`:

```text
pkg/engine/lazy_test.go:132:13: got.LookupInto undefined (type *Index has no field or method LookupInto)

--- FAIL: TestScoringDoesNotAllocateAPostingListPerTerm (0.09s)
    text_test.go:416: a 5-term query allocated 552912 bytes against 281232 for a one-term
    query over the same corpus: that is 4.1 posting lists of difference
```

Compile-time in `pkg/engine`, runtime in the scorer, and 4.1 is exactly the four extra
lists the profile predicted. The engine assertion lives inside `assertReadAPIsAgree`, the
same helper and the same position `Index.Vector`'s guard took in the cycle above, because
two spellings of one traversal is how a read path drifts.

**GREEN** — `8fbc09e`:

```text
--- PASS: TestScoringDoesNotAllocateAPostingListPerTerm (0.09s)
    text_test.go:408: 290768 bytes for 5 terms against 281232 bytes for one, over 4096 documents
```

0.15 posting lists of difference against 4.1. `go test -race ./pkg/engine/` ok, 86 s,
`assertReadAPIsAgree` holding `LookupInto` against `Lookup` for every term of every corpus
it builds, with one buffer reused across them.

**Field** — `make bench BENCHFLAGS='-rates 10 -rotations 4 -memprofile …'`, same flags as
the profile that named the target:

| | KiB/query | allocs/query | peakrss |
| --- | --- | --- | --- |
| before | 15,691.3 | 15,488 | 97.8 MiB |
| after | **10,869.0** | 15,478 | 92.9 MiB |

**−30.7% of what a query allocates**, and the count moved 0.06% — the two figures measure
different things and this is what that looks like. In the profile
`engine.(*segment).lookup.func1` is gone from the top and `slices.Grow` stands at 31.8%
where the sum of the lists used to be: the buffer still grows to the longest list, which is
the intended remainder rather than a leftover.

**Correctness** — `make eval`: `text` **0.5826**, `text+vector` **0.6211**, identical to four
decimals to the published figures. The registered tolerance is −0.005; identity is stronger
and is what a change that only moves where postings are written should produce.

### The judgment ladder, run — three of three met

**Summary.** `-rates 3.41,6.82,13.64,27.28`, `-rotations 200`, `inflight` 40, 2026-08-24
04:57:17 to 06:29:03 KST, 91 m 46 s. All four rungs at 10,000 samples, shed 0 on every one.
Registered in [PERF.md](../PERF.md) §5.3 **before** it ran, which
`git log --oneline -- docs/PERF.md` is where to check — the ordering milestone 7's task 2 and
[D-012](../DECISIONS.md) both hold this repository to.

| rung | rate | p50 | p99 | shed | peak RSS | raised | alloc/query |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 12.5% | 3.41/s | 78.576 ms | 100.857 ms | 0 | 99.0 MiB | +3.7 | 10,868.9 KiB |
| 25% | 6.82/s | 50.825 ms | 77.613 ms | 0 | 99.0 MiB | +0 | 10,868.9 KiB |
| 50% | 13.64/s | 34.124 ms | **53.868 ms** | 0 | 99.0 MiB | **+0** | 10,868.9 KiB |
| **100%** | **27.28/s** | **33.470 ms** | 54.825 ms | **0** | **100.7 MiB** | +1.8 | 10,869.0 KiB |

shed **0**, p50 **33.470 ms** against a 100 ms bar, ladder peak **100.7 MiB** against 250 —
where §7's ladder reached 345.2. **§5.3 outcome 1 fires and milestone 10 does not.**

And the excursion §6 item 6 carried forward went with it: 13.64 q/s from p99 849.853 ms and
+206.6 MiB to **53.868 ms and +0 MiB**. §5.3's outcome 4 registered that reading in advance —
tail and memory moving together means one cause, and it is the one §10 named.

**What the run does not explain**, recorded rather than smoothed: GC cycles fell 4.5× while
allocation fell 1.44×, and at about 1.7 cycles per second this is the lowest collection rate
of any run in this milestone *and* shed 0 everywhere — the opposite order from the four-run
correlation §3 offered. Nothing here instrumented the pacer either.

**One observation, not three.** The repetitions §5.3 registers were not run; that is cut 2 of
its order, taken deliberately and recorded beside every figure.
[FINDINGS §11](../FINDINGS.md) states what it costs and why these three quantities are the
least likely to be a draw.

**An earlier attempt was interrupted** 42 minutes in, 8,635 of 10,000 samples into rung 1. It
produced 10,867.3 KiB/query — 0.02% from the smoke figure, confirming the allocation number
holds at ladder depth — and no verdict, because the peak is decided at a rung it never
reached. Recorded in §5.3 rather than dropped.

## Test specification

| # | What is guaranteed | Test | Type | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 12 | A rung that allocated N bytes over M samples reports N/M per query, and its allocation count per query | `cmd/weft-eval/bench_test.go:TestBenchReportSaysWhatAQueryAllocated` | unit | PASS | `go test -race -run TestBenchReport ./cmd/weft-eval/` |
| 13 | A rung with no samples omits the per-query line rather than dividing by zero or printing a zero that reads as a measurement | `…:TestBenchReportOmitsTheAllocationLineWhenNothingWasMeasured` | unit | PASS | same |
| 14 | The two `print` and `rssAttribution` guarantees from the first cycle are unmoved by the new line | `…:TestBenchReport*` (2 earlier tests) | regression | PASS | same |
| 15 | Per-query allocation is deterministic on a replayed query set, so it is quotable at a sample size no quantile is | two rungs of `make bench BENCHFLAGS='-rates 5,10 -rotations 2'` | field | identical to the decimal | quoted above |
| 16 | `pkg/` is byte-identical to `main` after the revert, so no correctness invariant can have moved | `git diff main --stat -- pkg/` | gate | PASS (empty) | run by hand |
| 17 | The module graph and fusion's blindness are untouched | `make deps` | gate | PASS (one line / no scorer) | `make deps` |
| 18 | `-memprofile` writes a profile a reader can open, refuses a path it cannot write rather than discovering it after the run, and treats an empty path as "not asked for" | `…:TestWriteAllocProfile` (3 subtests) | unit | PASS | `go test -race -run TestWriteAllocProfile ./cmd/weft-eval/` |
| 19 | The flag reaches `benchOpts` | `…:TestBenchFlagsParsesAMemProfilePath` | unit | PASS | same |
| 20 | The profile is written on the `-writes` arm too, so the flag is not silently inert on one of two arms | `bench` returns `writeAllocProfile`'s error on both paths | inspection | — | no test; the arm needs the 626 MiB corpus |
| 21 | A query's allocation does not scale with the number of terms it has | `pkg/scorer/text/text_test.go:TestScoringDoesNotAllocateAPostingListPerTerm` | unit (mapped corpus) | PASS | `go test -run TestScoringDoesNotAllocate ./pkg/scorer/text/` |
| 22 | `LookupInto` answers exactly what `Lookup` does — every term, both sides of a commit, a term held by several segments, a buffer carrying another term's postings, and an absent term against a used buffer | `pkg/engine/lazy_test.go:assertReadAPIsAgree` (every caller of it) | unit | PASS | `go test -race ./pkg/engine/` |
| 23 | nDCG@10 is unmoved by the memory work | `make eval` | integration | PASS (0.5826 / 0.6211, identical to published) | `make eval` |
| 24 | The exported surface grew by exactly one line and `public_api.txt` did not move | `TestEngineAPISurfaceIsUnchanged`, `TestPublicAPISurfaceIsUnchanged` | gate | PASS after a recorded refresh | `git diff pkg/engine/testdata/` |
| 25 | At 27.28 q/s the engine sheds nothing, holds p50 under 100 ms, and the ladder's peak RSS is under 250 MiB | `make bench BENCHFLAGS='-rates 3.41,6.82,13.64,27.28'` | field (registered) | PASS (0 / 33.470 ms / 100.7 MiB) | [FINDINGS §11](../FINDINGS.md), one observation |
| 26 | The 13.64 q/s excursion is gone, and its tail and its memory moved together | same run | field (registered) | PASS (p99 53.868 ms, +0 MiB) | same |

Test 16 was why `make eval` was not run at the point the revert landed: the nDCG tolerance
of −0.005 is a guard on scorer changes, and an empty `pkg/` diff is a stronger statement
than a re-measured metric. The third cycle does change `pkg/`, so test 23 runs it, and
identity to four decimals is what a change that only moves where postings are written
should produce.

## Coverage and known gaps

```text
go test -coverprofile … ./cmd/weft-eval/   →  45.0% of statements (package)
go tool cover -func …                      →  bench.go print   81.8%
```

The package figure is not the metric, for the reason both earlier reports give: this is
`package main` around an index build and a load driver that need the 626 MiB corpus, so a
package percentage measures how much of the command runs without it. `print` is the one
function this change touched — 77.8% before, 81.8% now, because the new branch is covered
on both sides.

**Gaps, deliberate:**

1. **No test asserts `allocBytes` and `allocs` are wired correctly in `benchRung`.** They
   are two subtractions between `ReadMemStats` reads, and asserting them needs the 626 MiB
   corpus — the same gap the `rssRaised` cycle recorded as its gap 6. The field run above
   is what covers it, quoted rather than summarised.
2. ~~**The `text` arm's ~11 MiB per query is not attributed.**~~ **Attributed** — the
   escalation ran, and the table above is the answer. What is still not attributed is the
   345.2 MiB itself: per-query bytes explain the shape of the growth, and no run has yet
   tied them to the ladder's peak at 13.64 q/s.
3. **`bench/` does not get the allocation line.** Same reason as the `-rates` gap: the
   bleve module is rule 2's comparison arm, and changing the comparison instrument between
   two measurements is what milestone 5 §4.1 threw two ladders away over.
4. **The reverted property is not asserted anywhere now.** A narrow query still pays
   4.52 MiB for eight entries, and there is no test standing over it, because the only
   available fix makes the measured workload worse. It is a trade recorded here, not a
   guard.
5. **`make lint-docs` ran** this time: `markdownlint-cli2` via `npx`, 26 files, 0 issues.

## Interpretation notes

- **A fix can pass its own test and still be wrong**, and the only thing that caught it
  was an instrument the plan insisted on before the fix was attempted. That ordering is
  the whole content of this cycle.
- **The revert is not a failure of the milestone's pass line.** Nothing was judged: shed 0
  and p50 37.631 ms still stand from §7, and the memory clause is still the miss
  [D-014](../DECISIONS.md) recorded. What changed is that the target D-014 called
  "localised" is not, and the next step is a profile rather than a patch.
- **Milestone 10 still does not fire.** Its trigger is a miss after the milestone's
  engineering, and this cycle attempted engineering and withdrew it on measurement, which
  is not the same as having tried and failed to reach 250 MiB. D-014's revival condition —
  a profile pointing at mapped pages that cannot be avoided — is still unrun.

## Merge evidence

If these checkpoints are squashed, the summary that must survive:

- **RED** `35f03e5` — two assertions against `benchReport` fields that did not exist;
  build failed on `unknown field allocBytes`.
- **GREEN** `fcc5a97` — per-query allocation bytes and count, both reads outside the
  measured window, the stop-the-world read last in the block, and the line omitted when
  there is no denominator. 4/4 PASS under `-race`, field-verified on the corpus.
- **RED** `314feea` — a query allocated 593,816 bytes over 16,384 documents against 4,672
  over 64 for the same eight matches. No fix in the commit.
- **GREEN** `79f2a5d` — hint from the first posting list; 2,520 bytes on both corpora.
- **REVERT** `b7e981d` — the instrument measured the fix at 19,501.6 KiB/query against
  15,691.2 for the code it replaced, so it was withdrawn before any campaign ran. The
  corpus-sized hint stays, the trade is published, and the 345.2 MiB is still
  unattributed.
- **RED** `992cfe0` — three assertions against a function and a field that did not exist,
  with the fixture's own missing imports fixed first so the RED was only the production
  code.
- **GREEN + field** — `-memprofile` writes the `allocs` profile on both arms and reports a
  path it cannot write instead of discovering it after ninety-seven minutes. The profile
  attributes the whole 15.7 MiB per query: 53.2% is `Lookup` materialising a term's entire
  posting list, 44.3% is the accumulator map and candidate slice. Both are whole-set
  materialisation; neither is the hint the reverted commit went after.
- **RED** `527701c` — `LookupInto` undefined in `assertReadAPIsAgree`, and a 5-term query
  allocating 4.1 posting lists more than a one-term query over the same corpus.
- **GREEN** `8fbc09e` — `Index.LookupInto` writes into a caller's buffer; 4.1 lists of
  difference becomes 0.15, and on the corpus a query allocates 10,869.0 KiB against
  15,691.3, −30.7%. Same postings, same order, same absence on corruption, same lock span.
  `make eval` identical to four decimals. One line on `engine_api.txt`, recorded in
  FINDINGS §10 before the refresh; `public_api.txt` unmoved, `pkg/fusion` untouched.
- **Registration** — [PERF.md](../PERF.md) §2.8 describes the instrument and §5.3 registers
  the 97-minute judgment ladder with what each of its four outcomes licenses, committed
  before it runs. [D-016](../DECISIONS.md) is the buffer-not-iterator argument.
- **Verdict** — the ladder ran and outcome 1 fired: shed **0**, p50 **33.470 ms**, ladder
  peak **100.7 MiB** against 250 where §7 reached 345.2, and the 13.64 q/s excursion went
  from p99 849.853 ms and +206.6 MiB to 53.868 ms and +0 MiB. **Milestone 10 does not fire.**
  One observation, and the repetitions cut are recorded beside every figure. What the run
  does not explain — GC cycles down 4.5× against allocation down 1.44×, contradicting §3's
  four-run correlation — is published as unexplained.
- **`pkg/fusion` diff against `main` is empty**, `go list -m all` is one line, and `make all`
  is green including both lint gates.
