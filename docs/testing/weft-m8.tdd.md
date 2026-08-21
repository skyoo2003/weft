# Milestone 8 — TDD evidence

**Source**: no plan file. Milestone 8's pass line — *shed 0 at 27.28 q/s* — is under
re-specification in [the PRD](../../.claude/prds/weft-hardening.prd.md), because
[FINDINGS](../FINDINGS.md) milestone 7 §4.5 showed one rate both passing and failing
it. This cycle answers the question that blocks the plan rather than doing the
milestone's engineering work. The journey below is taken from
[FINDINGS](../FINDINGS.md) milestone 7 §6 carried-forward 1.
**Branch**: `m7-baseline`
**Date**: 2026-08-21

Two things are recorded: one RED/GREEN cycle for the instrument the first question
needs, and the campaign that instrument was built for, which has no such cycle and says
what stands in for one instead.

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

```
# github.com/skyoo2003/weft/cmd/weft-eval [github.com/skyoo2003/weft/cmd/weft-eval.test]
cmd/weft-eval/bench_test.go:67:11:  o.rates undefined (type benchOpts has no field or method rates)
cmd/weft-eval/bench_test.go:112:21: assignment mismatch: 2 variables but benchRates returns 1 value
cmd/weft-eval/bench_test.go:112:59: too many arguments in call to benchRates
	have (number, []float64, "time".Duration)
	want (float64, "time".Duration)
cmd/weft-eval/bench_test.go:128:20: too many errors
FAIL	github.com/skyoo2003/weft/cmd/weft-eval [build failed]
```

Compile-time RED in three shapes: a field that does not exist, a return arity that does
not match, and — past the `too many errors` cut — `benchSummary` without the
`ruleLadder` parameter the last assertion passes it. The failures are the missing
production code, not a broken fixture. Checkpoint `4fd7663`.

**GREEN** — `go test -race -run 'TestBenchFlags|TestBenchRates|TestBenchSummary' ./cmd/weft-eval/`:

```
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
ok  	github.com/skyoo2003/weft/cmd/weft-eval	1.486s
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

```
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

## Test specification

| # | What is guaranteed | Test | Type | Result | Evidence |
|---|---|---|---|---|---|
| 1 | A comma-separated list parses in the order given, whitespace and all — rates get copied out of a report and a report has spaces in it | `cmd/weft-eval/bench_test.go:TestBenchFlagsParsesAnExplicitLadder` | unit | PASS | `go test -race -run 'TestBenchFlags\|TestBenchRates\|TestBenchSummary' ./cmd/weft-eval/` |
| 2 | `-rate` beside `-rates` is refused, rather than one of them winning silently | `…:TestBenchFlagsRefusesAnExplicitLadderBesideAnExplicitRate` | unit | PASS | same |
| 3 | Every entry gets the bounds a lone `-rate` gets, at flag time: non-numeric, negative, zero, empty, `NaN`, `Inf` and past the 1e9 ceiling, alone or inside a list | `…:TestBenchFlagsRefusesARateInsideALadderItWouldRejectAlone` (9 cases) | unit | PASS | same |
| 4 | The named ladder is what runs, and the run reports that no rule selected among its rungs | `…:TestBenchRatesPrefersTheExplicitLadderAndDisownsTheRule` | unit | PASS | same |
| 5 | A four-rung ladder an operator typed publishes no saturation point and no headline, though it satisfies every shape check `RuleApplies` makes, and its rungs' own figures still print | `…:TestBenchSummaryPublishesNoHeadlineForAnOperatorChosenLadder` | unit | PASS | same |
| 6 | Milestone 7's `benchSummary` guarantees are unmoved by the new parameter — rule-selected headline, never-saturates, cut short, suspended, gap inside tolerance, explicit `-rate`, nothing measured | `…:TestBenchSummary*` (7 tests) | regression | PASS | same |
| 7 | The engine is untouched | `git diff --stat pkg/` | gate | PASS (empty) | `make all`, `make deps`, `make bench-build` |

## Coverage and known gaps

```
go test -coverprofile … ./cmd/weft-eval/   →  44.3% of statements (package)
go tool cover -func …                      →  benchRates   100.0%
                                              benchSummary 100.0%
                                              benchFlags    84.6%
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
   machine (`command -v` returns nothing). CI runs it. The changelog entry was
   validated with `changie batch patch --dry-run`, which rendered it.

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
