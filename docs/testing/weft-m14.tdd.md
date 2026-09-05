# Milestone 14 — TDD record: the probe that decides whether the ladder runs

**Source plan**: `.claude/plans/weft-m14.plan.md` (not in the repository; `.claude/` is
gitignored). Its tasks are reproduced below where they are referenced.

This file is the index into what the evidence proves, and the raw log for the two runs that
were made. It is not a substitute for them: every claim here names the command that produced
it.

## What is different about this milestone, and where TDD degenerates

**There is no production code to put under a RED/GREEN cycle**, and that is the round's
mechanical definition rather than an accident: the plan's D6 fixes `pkg/` at 0 lines, because
a round that judges four already-published numbers is not a round that changes what produces
them. The whole deliverable is one Makefile target, one registered procedure, and a verdict.

So the cycle applies to exactly one artefact and is stated honestly for the rest:

| Task | Artefact | What stands in for RED/GREEN |
| --- | --- | --- |
| 1 | `docs/PERF.md` §5.7 | **Ordering**, checked with `git log`: registration is committed before anything runs, so a pass line cannot be chosen after seeing the numbers |
| 2 | `Makefile` `bench-preflight` | **A real RED/GREEN.** RED is `No rule to make target`; GREEN is the target printing the two lines it exists to print |
| 3 | the two probes | **The pass line registered in Task 1**, applied to output produced after it was committed |
| 4–5 | the ladder, the write arms | **Not executed.** See Task 4 below — this is the finding, not a gap in the record |
| 6 | `FINDINGS`, `DECISIONS`, this file | `make all` |

**Test runner**: Go, and there is no npm in this repository. `<test>` is `make test`
(`go test -race ./...`); the gate is `make all` — `fmt build vet test lint lint-docs`. There is
no configured coverage threshold and none is asserted; see
[Coverage](#coverage-and-known-gaps).

## Plan safety review

The plan was read as untrusted input. Its commands are `git log`, `git diff`,
`git merge-base`, `git worktree add`, `make`, `date`, `uptime` and `caffeinate -dimsu`. No
destructive filesystem operation, no credential handling, no chained network installer, no
instruction-to-agent override phrasing. `git worktree add ../weft-m12-baseline` is the only
write outside the repository. **Approved as written**, with two corrections made during
execution and recorded rather than silently applied:

1. **The plan says "4.4 GiB 색인" (a 4.4 GiB index).** 4.4 GiB is the whole `.eval-data`
   directory; the index `make eval` loads is **677 MiB** (`du -sh .eval-data/index`). §5.7 uses
   the measured figure. The plan's argument is unaffected — it turns on there being an index
   load in the session at all, not on its size.
2. **The plan's Task 3 command cannot run on the baseline arm.** See Task 3 below.

## User journeys

From the plan's Summary and D1–D5, and from the PRD's milestone 14 row. All four are the
maintainer's, because this round has no adopter-facing surface.

1. As a maintainer about to spend 97 minutes on a ladder, I want a check that costs one minute
   and tells me the machine cannot reproduce the published figures, **so that** I do not
   publish a third void run.
2. As a maintainer reading a performance verdict, I want the pass lines committed before the
   numbers exist, **so that** "unchanged" is a finding rather than an absence.
3. As a reader of this history, I want the baseline arm to be a commit I can check out, **so
   that** the A/B is reproducible rather than an assertion.
4. As a maintainer, I want the four clauses milestone 11 opened and milestone 13 re-opened to
   move from unjudged to judged, met or missed. **This one is not met** — Task 4.

## Task report

### Task 1 — register §5.7 before the run

Wrote `docs/PERF.md` §5.7: the preflight and its pass line, the baseline substitution with the
two commands that justify it, the arm order with the page-cache bias named, four readings, and
the budget with the cut order. Added a pointer from §5.6's close.

```console
$ git log --format='%h %cd %s' --date=format:'%Y-%m-%d %H:%M:%S' -- docs/PERF.md | head -2
aee06ca 2026-08-27 23:28:12 docs: correct 5.7's baseline probe, before the baseline arm ran
d32bade 2026-08-27 23:26:34 docs: register milestone 14's readings and preflight before the run
```

`d32bade` at 23:26:34 precedes the first probe at 23:26:41 by seven seconds; `aee06ca` at
23:28:12 precedes the second at 23:28:20 by eight. **That ordering is Task 1's whole guarantee**
and it is the only one available for a round with no code: a pass line chosen after seeing the
numbers is how a performance claim is made to say whatever its author wants.

The baseline substitution was verified rather than taken from the plan:

```console
$ git merge-base --is-ancestor 1eb2a44 HEAD
(exit 1 — 1eb2a44 is not an ancestor of main)
$ git diff --stat 1eb2a44 700a178 -- pkg/ internal/ cmd/ bench/
(empty)
$ git diff --stat 9d05c16 eedc04a -- pkg/ internal/ cmd/
(empty)
```

### Task 2 — RED and GREEN, the only real cycle in this round

**RED**, before the target existed:

```console
$ make bench-preflight
make: *** No rule to make target `bench-preflight'.  Stop.
```

**Runtime RED, and the failure is the intended one**: the procedure step the round exists to
add is absent, and `make` says so rather than a test asserting it. There is no Go test for
Makefile targets in this repository and adding a harness for one six-line target would be
scaffolding the round did not need.

**GREEN**, `eedc04a`, 2026-08-27 23:26:41–23:27:09 KST, **28 seconds**:

```console
$ make -n bench-preflight
if [ ! -d .eval-data/index ]; then \
                echo "SKIP: no index at .eval-data/index — run 'make eval-data' first"; \
        else \
                go run ./cmd/weft-eval bench -data .eval-data -rates 27.28 -rotations 10; \
        fi
$ make bench-preflight
23:26:42 index: 171332 documents, avgdl 169.4, opened in 60ms
23:26:50 unloaded: p50 33.705ms over 200 requests, so 29.7 q/s sequentially
rate=27.28/s  n=500  shed=0  elapsed=18.33s
  latency   p50 35.387ms ... max 63.344ms
```

Two lines printed, 28 seconds, and the target is in `.PHONY`. `make lint-docs`: 0 issues in 30
files.

### Task 3 — the worktree, and a correction made before the baseline arm ran

```console
$ git worktree add ../weft-m12-baseline 700a178
Preparing worktree (detached HEAD 700a178)
HEAD is now at 700a178 Milestone 12: the query API needed no new name...
$ grep -c bench-preflight ../weft-m12-baseline/Makefile
0
```

**The plan's command could not have run.** `make -C ../weft-m12-baseline bench-preflight` names
a target this milestone adds, so a worktree at milestone 12's merge has no rule for it. The
probe on the baseline arm is therefore spelled as the flags the target hardcodes —
`bench -rates 27.28 -rotations 10`, identical work — and the correction went into §5.7 and was
committed (`aee06ca`) **before** the baseline arm ran, on §5.5's precedent for corrections made
before execution rather than after.

This is worth the paragraph it takes: a procedure step whose command fails on one of two arms
is the same class of defect as no step at all, and the step existing is what this round was
buying.

### Task 4 — the ladder, attempted twice, void twice

**Four probes passed the pass line registered in Task 1. Neither ladder attempt produced a
verdict.**

| probe | when | unloaded p50 | rung p50 | ratio | shed |
| --- | --- | --- | --- | --- | --- |
| HEAD | 23:26:41 | 33.705 ms | 35.387 ms | 1.05× | 0 |
| baseline `700a178` | 23:28:20 | 34.272 ms | 35.544 ms | 1.04× | 0 |
| HEAD | 00:25:51 | 35.098 ms | 35.161 ms | **1.00×** | 0 |
| HEAD, gate for attempt 2 | 05:36:00 | 32.800 ms | 34.421 ms | 1.05× | 0 |

#### Attempt 1 — SIGTERM at 46 minutes

Launched as a session-managed background task at 00:52:36. Reached 9,386 of rung 1's 10,000
samples and died at 01:38:38:

```console
make: *** [bench] Terminated: 15
01:38:38 context canceled
```

Not stopped by the operator. **Nothing published** — one partial rung of one arm is the void
production §5.7 forbids. Recorded because §5.1 requires a discard to be published. Its one
narrow contribution: p50 56.617 ms at 3.41 q/s against a published 78.576, shed 0, so the
machine was capable of the ladder's lower rungs.

#### Attempt 2 — eight rungs, both arms discarded

Relaunched fully detached, since a session-managed process group was what got killed. macOS
ships no `setsid`, so `perl` forks and calls `POSIX::setsid` before `exec`:

```bash
perl -e 'my $p = fork; if ($p) { print "detached pid $p\n"; exit }
use POSIX qw(setsid); setsid() or die;
open(STDIN, "<", "/dev/null");
open(STDOUT, ">", "/tmp/weft-m14-ladder2.log"); open(STDERR, ">&", STDOUT);
exec("bash", "/tmp/weft-m14-ladder.sh")'
```

It survived, ran 05:37:05 to about 17:00, and **completed all four rungs on both arms.** Both
then printed the instrument's refusal:

```console
DISCARD this run: the process did not run for 22m39s of the rung at 6.82/s, so the ladder
was measured across a suspension. There is no headline.
DISCARD this run: the process did not run for 4h57m8s of the rung at 3.41/s, so the ladder
was measured across a suspension. There is no headline.
```

**The cause, from `pmset -g log`:**

```console
06:46:41  Sleep  Entering DarkWake state due to 'Clamshell Sleep'  Using AC (Charge:100%)
06:46:46  Sleep  Entering Sleep state due to 'Clamshell Sleep'     Using Batt (Charge:100%)
07:10:00  Wake   Wake from Deep Idle ... due to ... lid ... HID Activity
```

**The lid was closed.** `caffeinate -dimsu` holds `PreventUserIdleSystemSleep` and cannot
prevent clamshell sleep — and the machine was on AC at full charge, so no power-state condition
was in play. `Elapsed` in `internal/loadgen/clock.go` subtracts monotonic from wall clock, and
on Darwin the monotonic clock stops during sleep, so the gap is proof the process was not
running. The guard inspects **every** rung and discards the whole ladder on any one suspension,
so no rung from either arm is quotable, including the clean ones.

**No registered reading fired.** Reading 1 needs a *failing* probe; readings 2, 3 and 4 need
readable arms. §5.7 gains readings 5 and 6, and [D-024](../DECISIONS.md) records that its own
falsification condition fired. Both detectors worked — `bench-preflight` for contention,
`SuspendTolerance` for suspension — and **the mitigation is what failed**.

[FINDINGS milestone 14 §4 and §5a](../FINDINGS.md) hold the analysis, including the top-rung
collapse that reproduces on both arms and eliminates milestone 13's code as its cause.

### Task 5 — the write arms, cut behind a discard

`-writes -writedocs 20000` on both arms, about 90 minutes, was §5.7's registered first cut. It
was never reached, because the ladder it follows was discarded. Published as an ordering
decision rather than an omission; milestone 9's read clause stays unjudged.

### Task 6 — publish

`docs/FINDINGS.md` milestone 14 (seven sections), `docs/DECISIONS.md` D-024, this file, and the
PRD's status paragraph. **`README.md` is unchanged**: reading 2 did not fire, so the unjudged
marking on the performance sentence stays. **No `changes/unreleased/` entry**: a changie item
is a change to the published surface, and `bench-preflight` is a maintainer target that touches
no `engine`, `scorer` or format behaviour.

## Test specification

What is guaranteed here is mostly *about a procedure*, so the table names the command that
carries each guarantee rather than a test function. Rows 9 to 11 are the Go gate, which this
round had to keep green without contributing to.

| # | What is guaranteed | Command or test | Type | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 1 | The pass lines and readings were committed **before** any measurement, so they cannot have been chosen to fit the numbers | `git log --oneline -- docs/PERF.md` | procedure | PASS | `d32bade` at 2026-08-27 precedes the first probe at 23:26:41 KST |
| 2 | `make bench-preflight` exists, runs in about a minute, and prints the unloaded median and the loaded rung | `make bench-preflight` | integration | PASS | 28 s; `unloaded: p50 33.705ms` and `p50 35.387ms` |
| 3 | The probe target expands to the two flags §5.7 registered and to nothing else | `make -n bench-preflight` | unit | PASS | `bench -data .eval-data -rates 27.28 -rotations 10` |
| 4 | The probe skips rather than fails when there is no index, like `bench` | `make -n bench-preflight` | unit | PASS | the `[ ! -d $(EVAL_DATA)/index ]` guard is present, mirroring `Makefile` `bench:` |
| 5 | The baseline arm is a commit reachable from `main` whose code is identical to the unreachable one §5.6 named | `git merge-base --is-ancestor`, `git diff --stat` | procedure | PASS | `1eb2a44` not an ancestor; `1eb2a44..700a178` empty across `pkg/ internal/ cmd/ bench/` |
| 6 | The measured arm is the same code §5.6 measured | `git diff --stat 9d05c16 eedc04a -- pkg/ internal/ cmd/` | procedure | PASS | empty |
| 7 | Both arms' machines were in a state to measure, by a gate the unloaded median cannot provide | the probe on each arm | integration | PASS | 1.04× and 1.05× against a 2× line, shed 0 both |
| 8 | Both arms run the same work per query | the probe on each arm | integration | PASS | `alloc 10869.0 KiB/query` on both, against a published 10,869.0 |
| 9 | `pkg/` gained nothing, and the golden API files are untouched | `git diff --stat eedc04a..HEAD -- pkg/`, `make arch` | golden | PASS | 0 lines; `make all` green |
| 10 | The docs gate is clean | `make lint-docs` | lint | PASS | 0 issues in 30 files |
| 11 | The whole Go gate is green | `make all` | gate | PASS | recorded below |
| 12 | The suspension guard refuses a ladder measured across sleep, on every rung rather than the headline's | `cmd/weft-eval/bench.go` `DISCARD this run`; `internal/loadgen/clock.go` `Elapsed` | integration | **PASS** | fired on both arms, with durations 22m39s and 4h57m08s |
| 13 | **The four performance clauses are met** | the two ladder arms | integration | **VOID** | Task 4 — completed and discarded; no clause judged |
| 14 | **Milestone 9's read clause is met** | the two write arms | integration | **NOT RUN** | Task 5 — cut behind a discard |

## Coverage and known gaps

Go has no configured coverage threshold in this repository and none is asserted; `make all` is
the gate and `make arch` / `make deps` are the architecture gates. **This round contributed no
Go code, so it moved no coverage figure** — which is the intended shape of a judgment round,
not a shortfall. What replaces a percentage is the golden API file, and it is unchanged at 0
lines.

Gaps, each deliberate and each published:

1. **The four clauses are unjudged for the third round.** The largest gap in the round and the
   round's own outcome clause. Two attempts, both void: one terminated, one discarded by the
   suspension guard. [FINDINGS milestone 14 §4](../FINDINGS.md), [PERF §5.7](../PERF.md).
2. **`caffeinate -dimsu` was the wrong mitigation for nine milestones and nothing caught it.**
   No test covers "the machine stayed awake" and none can — it is a property of the operator's
   desk, not of the code. What exists is the detector, and it worked. The procedural fix is
   *the lid stays open*, in [PERF §5.7](../PERF.md).
3. **The probe has no automated comparison and no exit code.** A person reads two printed
   lines. The `ponytail:` comment on the target prices the alternative at about thirty lines in
   `cmd/weft-eval/bench.go`. Neither failure this round would have been caught by one: the probe
   passed all four times, and what ended the runs was a SIGTERM and a closed lid.
4. **Nothing tests the Makefile target.** RED was `make` refusing the target and GREEN was it
   running; there is no regression test, so deleting the recipe would fail no test. Adding a
   harness for six lines of Makefile is scaffolding this round did not need.
5. **The probe's numbers cannot substitute for the clauses**, on three independent grounds —
   500 samples against 10,000, a lone rung against the ladder's fourth, and the load-point rule
   having no ladder to apply. [FINDINGS milestone 14 §3](../FINDINGS.md) refuses the reading
   explicitly rather than leaving it available, and the discarded ladder is why: the same rate
   that reads 34–35 ms alone collapsed to 1.6–2.0 s as a fourth rung.
6. **`ru_nivcsw` has many observations and still no threshold.** 22,698–27,434 per probe,
   649,334–970,547 per clean rung, and 2,395,569–2,723,482 on the two collapsed top rungs. Every
   reading comes from a machine that was not quiet or slept, so none of them calibrates
   anything. [D-024](../DECISIONS.md).
7. **`-deletefrac` and §5.5's run B are untouched** and still owed, deliberately kept out of
   this sitting.
8. **The top-rung collapse is unexplained**, with three candidate causes surviving and one
   eliminated. [FINDINGS §5a](../FINDINGS.md). Not a coverage gap — a measurement one, and the
   next ladder is what closes it.

## Raw log

Outside the repository per [PERF §5.1](../PERF.md), and reproduced here because §5.7's
`uptime`-beside-`date` requirement is only useful if the reading is legible.
`/tmp/weft-m14-preflight-head.log`, `/tmp/weft-m14-preflight-baseline.log`.

### HEAD — `eedc04a`

```console
Thu Aug 27 23:26:41 KST 2026
23:26  up 18 days, 11:43, 5 users, load averages: 2.31 3.23 3.26
23:26:42 index: 171332 documents, avgdl 169.4, opened in 60ms
23:26:42 queries: 50 with judgments, 50 with a vector
cold  n=50  total=1.779s  worst=55.428ms  minflt=2121  majflt=719
23:26:50 unloaded: p50 33.705ms over 200 requests, so 29.7 q/s sequentially

weft  text  warm  n=500/rung  inflight=40  GOMAXPROCS=10

rate=27.28/s  n=500  shed=0  elapsed=18.33s
  latency   p50 35.387ms  p95   --    p99   --    p99.9   --    max 63.344ms
  minus STW p50 35.372ms  p95   --    p99   --    p99.9   --
  gc        cycles 263  STW 10.998ms (0.060% of elapsed)  GC CPU share 1.3%
  alloc     10869.0 KiB/query  15478 allocs/query  (5307.1 MiB this rung)
  rusage    minflt 787  majflt 5  nvcsw 749  nivcsw 24867  peakrss 105.7 MiB (process, raised 12.3 MiB by this rung)

1 of 1 rungs measured — a ladder you named, an explicit -rate, or a ladder cut short — so the
load-point rule has nothing to apply and there is no saturation point and no headline
23:27  up 18 days, 11:43, 5 users, load averages: 2.49 3.20 3.25
Thu Aug 27 23:27:09 KST 2026
```

### Baseline — `700a178`, shared index

```console
Thu Aug 27 23:28:20 KST 2026
23:28  up 18 days, 11:44, 5 users, load averages: 3.29 3.36 3.31
23:28:21 index: 171332 documents, avgdl 169.4, opened in 60ms
23:28:21 queries: 50 with judgments, 50 with a vector
cold  n=50  total=1.799s  worst=58.172ms  minflt=2309  majflt=713
23:28:30 unloaded: p50 34.272ms over 200 requests, so 29.2 q/s sequentially

weft  text  warm  n=500/rung  inflight=40  GOMAXPROCS=10

rate=27.28/s  n=500  shed=0  elapsed=18.329s
  latency   p50 35.544ms  p95   --    p99   --    p99.9   --    max 65.058ms
  minus STW p50 35.511ms  p95   --    p99   --    p99.9   --
  gc        cycles 257  STW 10.138ms (0.055% of elapsed)  GC CPU share 1.3%
  alloc     10869.0 KiB/query  15478 allocs/query  (5307.1 MiB this rung)
  rusage    minflt 257  majflt 0  nvcsw 646  nivcsw 26692  peakrss 102.3 MiB (process, raised 4.0 MiB by this rung)
23:28  up 18 days, 11:45, 5 users, load averages: 4.14 3.55 3.38
Thu Aug 27 23:28:48 KST 2026
```

Command, for the record — the baseline spelling is the corrected one from Task 3:

```bash
make -C ../weft-m12-baseline bench EVAL_DATA=$PWD/.eval-data \
  BENCHFLAGS='-rates 27.28 -rotations 10'
```

### The discarded ladder — attempt 2, both arms

`/tmp/weft-m14-ladder-attempt2-discarded.log`, and
`/tmp/weft-m14-ladder-attempt1-killed.log` for the terminated one. **No figure below is a
clause reading**; both arms are discarded and the table is here so the next attempt knows what
shape to expect. p95 and p99 are omitted for the top rungs because the run shed and the
quantiles fell under §2.3's sample floor.

| rung | rate | M8 published p50 | baseline `700a178` | HEAD `eedc04a` |
| --- | --- | --- | --- | --- |
| 12.5% | 3.41/s | 78.576 ms | 55.426 ms | 66.643 ms |
| 25% | 6.82/s | 50.825 ms | 47.661 ms | 44.952 ms |
| 50% | 13.64/s | 34.124 ms | 37.076 ms | 37.744 ms |
| **100%** | **27.28/s** | **33.470 ms** | **1.635754 s** | **2.034673 s** |
| unloaded p50 | | 32.231 ms | 34.076 ms | 39.134 ms |
| shed, top rung | | 0 | 1,811 of 10,000 | 3,323 of 10,000 |
| ladder peak RSS | | 100.7 MiB | 660.8 MiB | 675.8 MiB |
| raised *at* the top rung | | +1.8 MiB | +553.0 MiB | +574.3 MiB |
| alloc/query, rungs 1–3 | | 10,868.9–10,869.0 KiB | 10,868.9–10,869.0 KiB | 10,868.9–10,869.0 KiB |
| `nivcsw`, top rung | | — | 2,395,569 | 2,723,482 |
| suspension | | — | 22m39s at rung 2 | **4h57m08s at rung 1** |

Timings: arm 1 started 05:37:05, arm 2 at 07:31:48, the whole thing finished about 17:00 —
against a 3.1-hour schedule, the difference being the two sleeps.

## Merge evidence

Checkpoint commits on `main`, in order. If they are squashed, this is the record:

| Commit | Stage | Evidence captured |
| --- | --- | --- |
| `d32bade` | **register** | §5.7: pass line, baseline, arm order, four readings, cut order — before any run |
| `eabb2e2` | **RED → GREEN** | `bench-preflight`. RED `No rule to make target`; GREEN 28 s, 1.05×, shed 0, both figures quoted in the message |
| `aee06ca` | **correct** | The baseline probe's command respelled, before the baseline arm ran |
| `d7b2921` | **interim verdict** | Four clauses unjudged; written when the ladder had not been attempted |
| `b68e971` | **evidence** | Registration ordering quoted to the second — seven and eight seconds |
| pending | **final verdict** | Two ladder attempts, both void; the clamshell cause; §5.7 readings 5 and 6; D-024's falsification condition fired; FINDINGS §5a's hypothesis |

No refactor commit: there was nothing behaviour-preserving to clean up in six lines of
Makefile.

**The state the next attempt starts from.** The worktree `../weft-m12-baseline` at `700a178` is
left in place, so the next run skips Task 3. `git worktree remove ../weft-m12-baseline` if it is
in the way. `/tmp/weft-m14-ladder.sh` is the runnable two-arm script and needs no edit.

Three preconditions, and the third is what this round learned the hard way:

1. A **3.1-hour window** with nothing else on the machine.
2. A passing `make bench-preflight` **on each arm, immediately before that arm** — the
   certificate does not survive a wait, and attempt 2's was 78 seconds old.
3. **The lid stays open.** `caffeinate -dimsu` does not cover clamshell sleep, and that is what
   discarded both arms of attempt 2.

Launch it detached rather than as a session-managed task — attempt 1 took SIGTERM at 46 minutes
as one — using the `perl`/`POSIX::setsid` form in Task 4, which survived.

**What a clean ladder settles first**, from [FINDINGS §5a](../FINDINGS.md): whether the
top-rung collapse seen on both discarded arms is real. If it is, the three ladder clauses are
**missed** rather than unjudged, and that would be the first verdict on them since milestone 8.
