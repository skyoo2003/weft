# Milestone 9 — TDD evidence

**Source plan**: [`.claude/plans/weft-m9.plan.md`](../../.claude/plans/weft-m9.plan.md)
**Branch**: `milestone-9-write-lock-ceiling`
**Date**: 2026-08-24

Two clauses, one place. **A read arriving while a commit is in flight waits no more than
1 second**, against the 12.539 s [FINDINGS](../FINDINGS.md) milestone 5 §5 published. And
**an operator can call a commit off**, which `Commit` could not be asked to do — it took no
context, and the README's Limitations table said so.

Both are properties of the same eleven seconds, so this file records two RED/GREEN cycles
and the measurement that judges the first of them.

## Plan safety review

The plan was read as data. Nothing in it asks for a destructive filesystem operation, a
credential to be printed or copied, or a remote script to be fetched and executed, and it
contains no instruction to disregard governing rules or to bypass validation. Three kinds of
command in it are executable, and each was translated into this repository's own gates rather
than run as written:

| In the plan | What was run | Why it is allowed |
| --- | --- | --- |
| `caffeinate -dimsu make bench BENCHFLAGS=...` | the same, with `-C` and `EVAL_DATA` for the `before` worktree | `caffeinate` prevents sleep for the duration of one command, and sleep during a 45-minute latency measurement is what [FINDINGS](../FINDINGS.md) milestone 7 §4.1 cost |
| `WEFT_UPDATE_GOLDEN=1` | the same, and only after the spend was recorded | repository-local golden refresh, and [D-015](../DECISIONS.md) fixes the order |
| `make all` / `make arch` / `make deps` / `make eval` / `make lint-docs` | the same | the project's own gates |

One deviation from the plan is recorded rather than silently taken. The plan's *Files to
Change* asks for a changie entry of kind **Changed** for the signature. It is filed as
**Exported API**, because `.changie.yaml` reserves that kind for the three things a caller
"can be forced to act on" and a changed method signature is exactly one of them. Filing it as
`Changed` would put a compile-breaking change in the section a reader skims.

## User journeys

From the plan's *Summary* and *Acceptance*, unchanged:

1. **As an operator serving queries from an index that is still being written**, I want a
   commit not to stall my reads, so that a batch ingest is not a service outage.
2. **As an operator who started a commit I no longer want**, I want to cancel it, so that a
   batch I mis-triggered does not hold the writer for eleven seconds with no way to stop it.
3. **As an operator whose cancellation landed somewhere nobody chose**, I want the directory
   to hold either the previous commit or this one, so that calling a commit off cannot reach
   a state a crash cannot.

## Task report

### Task 1 — reads must make progress while a commit encodes (RED)

`TestReadsMakeProgressWhileACommitEncodes` states journey 1 as a property rather than as a
stopwatch. It counts read batches completed inside the `Commit` call instead of timing the
longest wait, so the assertion means the same thing on a loaded runner as on an idle one: a
wall-clock bound is a claim about the machine, and a fast machine passes one while blocking
reads for a whole encode.

The corpus is `ivfMinDocs` (16,384) documents carrying dim-8 vectors — the smallest one whose
commit trains an IVF partition, because `buildIVF` is where 11.014 of milestone 5's 11.063
seconds went, and a commit that skips training is not the commit under test.

**Validation**: `go test -race -v -run TestReadsMakeProgressWhileACommitEncodes ./pkg/engine/`

```text
persist_test.go:942: 0 read batches of 4096 completed while a commit encoded 16384 documents
persist_test.go:944: 0 read batches completed while a commit encoded 16384 documents, want at
    least 20: the commit is holding the exclusive lock across its encode
--- FAIL: TestReadsMakeProgressWhileACommitEncodes (0.87s)
```

Zero, not "few". No engine code in commit `f0fcedc`.

**One thing the first version of this test got wrong, and it is why the unit is a batch.** A
test cannot see the lock, so it samples the counter around the whole call — and the `MkdirAll`
and `OpenRoot` that run *before* the lock is taken are inside that sample. A single `Lookup` on
a pending index is a map read behind an uncontended `RLock`, so a reader finishes thousands of
them inside those two syscalls, and the first version **passed on an unfixed tree**. That
observation is what named the flaw: the unit is now 4,096 reads, which costs more than the
syscalls do, so a unit landing inside the window is a unit the lock admitted.

**What the passing test guarantees**: at least 20 batches of 4,096 reads complete while a
commit encodes 16,384 documents. It does not guarantee a latency bound — that is the bench run
below.

### Task 2 — split the lock (GREEN)

`Commit` now holds `Index.wmu` across its whole body, `ix.mu` in **read** mode for the encode,
`ix.mu` exclusively for the `adopt`, and no lock at all for the sweep.
[D-017](../DECISIONS.md) carries the argument; the part worth repeating here is why `wmu` is
not decoration. `sync.RWMutex` prefers writers, so one `Add` entering `mu.Lock` while the
commit holds `mu.RLock` makes every later `RLock` queue behind that waiter — lowering the
encode to a read lock without `wmu` changes the trigger of the 12.5 s stall and not its
existence.

**Validation**: the same command, plus the whole package under `-race`.

```text
persist_test.go:942: 578 read batches of 4096 completed while a commit encoded 16384 documents
--- PASS: TestReadsMakeProgressWhileACommitEncodes (0.85s)
```

578 batches — 2.4 million reads — against 0 on the parent commit.

**Bytes are asserted unchanged, not argued.** A lock mode that altered an encoding would be a
bug and not a speedup, so the determinism and concurrency tests the plan named were run
explicitly:

```text
--- PASS: TestIVFTrainingIsDeterministic (3.71s)
--- PASS: TestOpenSurvivesAConcurrentMerge (21.89s)
--- PASS: TestScrubSurvivesAConcurrentMerge (6.93s)
--- PASS: TestSegmentRoundTrip (0.29s)
--- PASS: TestCommitDuringReads (0.05s)
ok  github.com/skyoo2003/weft/pkg/engine  89.167s   (go test -race ./...)
```

### Tasks 3 and 4 — cancellation (RED and GREEN, one commit)

The RED here is **compile-time**, which is why the plan puts these two tasks in one commit and
this section says so. The three tests name `Commit(ctx, dir)`, a signature that does not exist
on the parent commit, and the compile failure is the intended signal: the tests newly exercise
the missing capability rather than failing on unrelated breakage.

**RED**: `go vet ./pkg/engine/`

```text
pkg/engine/persist_test.go:1021:33: too many arguments in call to ix.Commit
    have (context.Context, string)
    want (string)
[7 more, all the same]
```

**Implementation**: `Index.Commit(ctx context.Context, dir string) error`, with `ctx` flowing
privately into `writeSegment` → `buildIVF` → `ivfRefine` / `ivfAssign`. The contract is a
corollary of the rename being the commit point, not a new rule: before the rename `ctx.Err()`
is reported and nothing is published; after it, `ctx.Err()` is ignored for the rest of the
call, because stopping between the rename and the `adopt` would leave the directory publishing
a generation the live index has no mapping for.

**GREEN**:

```text
--- PASS: TestCommitRefusesACancelledContext (0.91s)
--- PASS: TestACancelledCommitPublishesNothing (0.17s)
    the deadline fired before the rename; asserting nothing was published
--- PASS: TestScrubAfterACancelledCommit (1.04s)
    cancelled=true, seg-000002 standing unnamed=true
```

**Why one of these three accepts two answers.** Which side of the rename a 5 ms deadline lands
on is a race, so asserting a side would be a flake.
`TestACancelledCommitPublishesNothing` allows both and pins each — a commit reporting success
published a whole generation and scrubs clean; a commit reporting the deadline published none
and left the pending documents pending. What it refuses is the third thing, a generation that
is there and incomplete, which is the invariant [D-017](../DECISIONS.md) defines cancellation by.

### The API spend, and the order it was spent in

`make arch` failed on exactly one line, which is what was expected:

```diff
-method Index.Commit(string) error
+method Index.Commit(context.Context, string) error
```

[FINDINGS](../FINDINGS.md) milestone 9 §2 records the spend **before**
`WEFT_UPDATE_GOLDEN=1` refreshed the file, which is [D-015](../DECISIONS.md)'s order and is
checkable with `git log --oneline -- docs/FINDINGS.md`. `pkg/fusion` and `public_api.txt` do
not move, `go list -m all` is one line, and the 82 call sites are entirely tests, `cmd` and
`examples`.

The call-site churn was verified to be churn: `git diff` over every touched test file, with
`Commit(`, `writeSegment(` and `buildIVF(` lines filtered out, is empty apart from the new
tests and one helper.

### Task 6 — the procedure, registered before the run

[PERF.md](../PERF.md) §5.4 registers the `-writes` command line, why `-writedocs 20000` is the
value (it exceeds `ivfMinDocs`, so the commit under measurement trains), the four quantities
compared, the four outcomes and what each licenses, and the repetition budget with the cut
order fixed in advance. It also records that **the arm had never been registered** —
milestone 5's 11.063 s and 12.539 s are the product of a procedure nobody wrote down.

That commit precedes the judgment run, which `git log --oneline -- docs/PERF.md` shows.

### Tasks 5, 7 and 8 — the measurement, and the accuracy invariant

No RED/GREEN here, and this section says what stands in for one: the procedure was registered
in [PERF.md](../PERF.md) §5.4 before either run, and the reading of every possible outcome was
fixed in advance. That is what makes a bench run an assertion rather than a number.

**`before`, on `f0fcedc` in a detached worktree** so the main tree was untouched:

```bash
{ date; caffeinate -dimsu make -C /tmp/weft-m9-before bench \
    EVAL_DATA=$PWD/.eval-data BENCHFLAGS='-writes -writedocs 20000'; date; }
```

```text
22:41:00 the writer held the lock for 11.467s, of which the commit itself was 11.413s
commit window  [14m48.591s, 15m0.058s)  =  11.467s
  during    p50   --    p95   --    max 13.072s  (n=40)
  outside   p50 72.004ms  p95 90.068ms  max 1.676s  (n=9956)
  shed 4
```

**`after`, on `cacc258`:**

```text
23:27:50 the writer held the lock for 11.284s, of which the commit itself was 11.233s
commit window  [14m37.758s, 14m49.042s)  =  11.284s
  during    p50   --    p95   --    max 61ms  (n=43)
  outside   p50 70.27ms  p95 90.279ms  max 191ms  (n=9957)
  shed 0
```

**Outcome 1 of the four registered fires**: `during` max 61 ms against a 1-second clause, and
below the 191 ms worst read *outside* the window. The blocking outcome 4 — a cancelled commit
publishing a generation — did not occur.

`make eval`, for the accuracy invariant:

```text
  text                                   0.5826
  text+vector                            0.6211
```

Identical to four decimals to the published figures, against a −0.005 tolerance. A lock
restructuring cannot change an answer; that is an argument, and this is its check.

Two comparisons the numbers invite are **refused** rather than published, and
[FINDINGS](../FINDINGS.md) milestone 9 §4.2 and §4.3 are where: `during` max sits below
`outside` p50 on 43 samples, and `outside` max fell 8.8× in a cohort the change should not have
touched. Both need a split the arm does not report — `outside` mixes reads against a one-segment
index with reads against a two-segment one, because the commit fires a third of the way in.

**Cuts taken**: [PERF.md](../PERF.md) §5.4 registered three `after` repetitions and cut order.
Cuts 1 and 2 were both taken, so each side is a single observation and says so beside the
figures. Cut 3 — dropping the `before` run — was **not** taken, because it is the only thing
that makes the fall attributable to this round rather than to [D-016](../DECISIONS.md)'s
read-path change.

## Test specification

| # | What is guaranteed | Test | Type | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 1 | At least 20 batches of 4,096 reads complete while a commit encodes 16,384 documents | `pkg/engine/persist_test.go:TestReadsMakeProgressWhileACommitEncodes` | concurrency | PASS (was FAIL at 0) | `go test -race -run TestReadsMakeProgress ./pkg/engine/` |
| 2 | A commit given an already-cancelled context reports `context.Canceled`, publishes nothing, keeps the pending documents, and commits normally afterwards | `pkg/engine/persist_test.go:TestCommitRefusesACancelledContext` | integration | PASS | `go test -race -run TestCommitRefusesACancelledContext ./pkg/engine/` |
| 3 | The same over a live generation leaves that generation's number unchanged and its bytes scrubbable | same test, second half | integration | PASS | same |
| 4 | A commit cancelled mid-encode publishes generation 1 whole or publishes nothing — never a partial generation | `pkg/engine/persist_test.go:TestACancelledCommitPublishesNothing` | integration | PASS | `go test -race -run TestACancelledCommitPublishesNothing ./pkg/engine/` |
| 5 | What a cancelled commit leaves is scrubbable, openable, and swept by the next commit | `pkg/engine/persist_test.go:TestScrubAfterACancelledCommit` | integration | PASS | `go test -race -run TestScrubAfterACancelledCommit ./pkg/engine/` |
| 6 | Two builds of one corpus produce bit-identical centroids and lists, with the context polls in place | `pkg/engine/ivf_test.go:TestIVFTrainingIsDeterministic` | unit | PASS | `go test -race -run TestIVFTrainingIsDeterministic ./pkg/engine/` |
| 7 | Commit under concurrent reads is race-clean | `pkg/engine/persist_test.go:TestCommitDuringReads` | race | PASS | `go test -race ./pkg/engine/` |
| 8 | `Open` and `Scrub` survive a concurrent `Merge` under the new lock order | `pkg/engine/persist_test.go:TestOpenSurvivesAConcurrentMerge`, `TestScrubSurvivesAConcurrentMerge` | race | PASS | `go test -race ./pkg/engine/` |
| 9 | The exported surface changed by exactly one line | `pkg/engine/architecture_test.go:TestEngineAPISurfaceIsUnchanged` | golden | PASS after refresh | `make arch` |

## Coverage and known gaps

- **No `go test -cover` figure is quoted for this cycle.** The change is a lock restructuring
  and a threaded parameter, not new logic, and every branch it added is a `ctx.Err()` check on
  a path an existing test already walks. The cancellation branches a test can *reach* — entry,
  and immediately before the rename — are covered by tests 2–5.
- **Three `ctx.Err()` polls have no test that lands on them specifically**: the two section
  boundaries inside `writeSegment` and the Lloyd-pass poll in `ivfRefine`. They are reached
  non-deterministically by `TestACancelledCommitPublishesNothing` and
  `TestScrubAfterACancelledCommit`, which is why those two *log* which branch ran instead of
  asserting it. Pinning a poll would need an injectable cancellation point inside the writer,
  which is production structure bought for a test.
- **`Merge` has no cancellation and no lock split.** Carried forward deliberately: no arm
  measures a merge, and [FINDINGS](../FINDINGS.md) milestone 9 §2 says so rather than claiming
  an improvement nobody timed.
- **`Add` blocks for a whole commit.** Not a regression — it already did — and recorded as a
  ceiling on `Index.wmu` with the upgrade path (the capture counting) named.
- **The read clause is judged by a bench run, not by a test.** Test 1 proves reads *progress*;
  whether the worst of them waits under 1 second is [PERF.md](../PERF.md) §5.4's question, and
  the answer is 61 ms — [FINDINGS](../FINDINGS.md) milestone 9 §3, with §4 for what that
  measurement cannot say.

## Merge evidence

The checkpoint commits on `milestone-9-write-lock-ceiling`, in order, in case they are squashed:

| commit | stage | evidence |
| --- | --- | --- |
| `f0fcedc` | RED | `TestReadsMakeProgressWhileACommitEncodes` fails at **0 batches**, want 20, no engine change in the commit |
| `e14a212` | GREEN | the same test passes at **578 batches**; `go test -race ./...` clean including the three determinism and concurrency tests |
| `b919c9e` | RED + GREEN | compile-time RED (`too many arguments in call to ix.Commit`) then three cancellation tests passing; `make all`, `make arch`, `make deps` clean; golden refreshed only after FINDINGS recorded the spend |
| `cacc258` | registration | [PERF.md](../PERF.md) §5.4, committed **before** either bench run |
| this commit | publication | `during` max 13.072 s → **61 ms**, nDCG 0.5826 / 0.6211 unchanged |
