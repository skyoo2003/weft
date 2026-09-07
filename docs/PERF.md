# Performance — how milestone 5's numbers are produced

This file is to milestone 5 what [EVAL.md](EVAL.md) is to milestone 4: the measurement design and the judgment rule, written down so a reader can decide whether to believe the numbers before reading them, and so the numbers cannot be chosen after the fact.

The verdict itself lives in [FINDINGS.md](FINDINGS.md), milestone 5.

## 1. What is being measured, and what would falsify it

The PRD's outcome sentence has two clauses, graded separately:

> GC pause를 포함한 p99가 공개되고, 기성 엔진과 같은 자릿수임을 보인다

1. **A p99 including GC pause is published.** Falsified by publishing a mean, by publishing a p99 from a sample that cannot support one, or by publishing a latency figure with no accounting of what the collector contributed.
2. **It is within an order of magnitude of an established engine.** Falsified by `p99(weft) > 10 × p99(bleve)` measured on the same machine, corpus, query set, arm and load generator.

The plan also fixed a prediction before any of this was built, and it is graded too — see §6.

**What clause 2 does not claim, registered before the numbers existed.** If weft turns out faster, that is not a claim of superiority. bleve carries an analysis pipeline, stored fields, a facetable index and a segment merge policy weft does not have, and it is being asked to do a job smaller than the one it was built for. The only supportable reading of a favourable number is "the same order of magnitude" — the same reading as an unfavourable one inside 10×.

## 2. The instrument

`weft-eval bench` and `bench/` (the bleve side) are two commands over one driver, `internal/loadgen`. Sharing the driver is not tidiness: clause 2 compares two numbers, and a bias in one implementation of an open loop that is absent from the other would move the ratio without moving either engine.

### 2.1 Open loop, and why a closed one would invalidate the whole file

A closed-loop driver sends the next request when the previous one returns. Under that design a server that stalls **receives less load**, so the stall shows up as one slow request and every request that would have arrived during it was never sent. The reported p99 is then a p99 of a load the server itself chose.

This is coordinated omission, and a milestone whose deliverable is a p99 cannot be measured by the instrument that hides it.

Here request *i* is due at `start + i/rate` whatever request *i-1* is doing, and its latency is measured from that due time rather than from when it began running. A stall lands on every request it delayed.

`internal/loadgen.TestOpenLoopDoesNotLetTheServerSlowTheLoad` is the assertion: a synthetic server serialising on a mutex stalls once, and the test requires many samples above the stall rather than one. A closed loop produces exactly one.

### 2.2 The one place the loop stops sending

In-flight requests are capped at four per core, and exceeding the cap is **counted and reported as `shed`, never waited on**. Waiting would restore the closed-loop bias at precisely the load where it matters most.

The cap exists because a query's working set is 210 MiB on this corpus ([FINDINGS milestone 3b §2](FINDINGS.md)), so an unbounded loop at twice saturation is an out-of-memory rather than a measurement.

A rung reporting a large `shed` is reporting that the load was not applied, and its distribution should be read as "of the requests that were sent".

### 2.3 Quantiles: nearest rank, and absent when thin

Nearest rank on the sorted sample, `ceil(q·n)` clamped into range. The interpolating definitions return a value between two observations — a latency no request experienced — and a published tail figure should be a number the server actually produced.

A quantile needs **100 samples beyond it** or it is not printed at all:

| quantile | samples required |
| --- | --- |
| p50 | 200 |
| p95 | 2,000 |
| p99 | **10,000** |
| p99.9 | 100,000 |

Below that a figure is an order statistic of a handful of observations and moves by milliseconds between runs. It is left out rather than printed with a caveat, because a caveat beside a number is not what a reader carries away.

This is why a rung is 200 rotations of the 50-query set, and why the `text+vector` arm — four times slower per query — is reported without a p99 unless it was given the same 10,000 samples. Rule 4 is how that arm is given them without relaxing this table.

### 2.4 GC accounting, and the design that was measured and discarded

The first design **classified**: a sample was *GC-hit* when the completed-cycle counter moved during it, and the report compared the hit and free cohorts. A smoke run made **485 collections over 200 queries**, so all 200 samples were GC-hit and the free cohort was empty. The classification is not wrong; it is degenerate at any allocation rate weft produces.

The design that ships **charges** instead. A stop-the-world pause stops every goroutine, so pause time that elapsed inside a request's window is time that request provably spent stopped. Two distributions are printed:

- `latency` — as measured, which is the "including GC pause" the PRD asks for
- `minus STW` — the same samples with their charged pause subtracted

The gap between the two p99s is the collector's contribution to the tail.

**Two limits, stated beside the number rather than after it.**

*Stop-the-world is not all the collector costs.* Mark assist charges collection work to the goroutine that allocated — which is the query — and none of it is stop-the-world or visible in either distribution. The smoke run showed exactly this: STW was 0.053% of the wall clock while the median query ran 66.9 ms against an unloaded 35.7 ms. `GC CPU share`, from `/cpu/classes/gc/total:cpu-seconds`, is reported for that reason.

*The counters are read through `runtime/metrics`*, not `runtime.ReadMemStats`, which stops the world to answer. An instrument that pauses the program once per request would be measuring pauses it caused.

### 2.5 Page faults, which say what the tail was waiting on

`getrusage` around each rung, reported as a difference:

| counter | what it means |
| --- | --- |
| `minflt` | a page the kernel already had — page cache hit, hundreds of nanoseconds |
| `majflt` | a page the kernel had to fetch — storage, four to five orders of magnitude more |
| `nvcsw` | the process yielded; it blocked on something |
| `nivcsw` | the scheduler took the core away — over-subscription, from inside |

A tail dominated by `majflt` is a storage problem, one dominated by `nivcsw` is a concurrency problem, and one with neither is the collector's or the code's. None of that is visible in latency alone, and §6's prediction is precisely a claim about which of them binds.

`peakrss` on the same line is the exception and is labelled `(process)` for it — see §2.7.

**`GC CPU share` is a per-rung figure, and getting there needs the ratio of two differences rather than the difference of two ratios.** `/cpu/classes/gc/total:cpu-seconds` and `/cpu/classes/total:cpu-seconds` are both cumulative since process start, so a running share stops moving once the process has any history: by the fifth rung, a rung that gave a third of its CPU to the collector and one that gave none read the same. `loadgen.GCCPUShareBetween` takes both totals at each end of a rung and divides the differences.

### 2.6 Warm and cold are separate measurements

Fifty queries replayed two hundred times finds the page cache fully populated from the second rotation on. **Every ladder number is warm**, and the 210 MiB of distinct pages a query touches is largely a page-cache figure rather than a storage one.

That is a property of any steady-state server and is reported as the headline. The first pass over the query set is reported separately as `cold` — 50 samples, so no p99, but a maximum and a fault count, which is what the cold path can honestly support.

There is no portable way to drop the page cache, so "cold" here means "first touch in this process", and a second run against an already warm host cache is warmer than the first. The cold line is read for its `majflt`, not for its latency.

### 2.7 The memory figure is the process's, and only its increase belongs to a rung

`peakrss` is `ru_maxrss`: a high-water mark the kernel never lowers and offers no way to reset. There is no during-this-rung reading of it, so on a ladder the figure a rung prints is **the peak the process has reached by the end of that rung**, cold pass and every earlier rung included.

The line has always said `(process)`, and that turned out not to be enough. Milestone 8's own pass line was written as *RSS ≤ 250 MiB at 27.28 q/s*, and the ladder that judged it printed 345.2 MiB there — the mark set two rungs earlier at half the rate ([FINDINGS milestone 8 §7](FINDINGS.md)).

So each rung now also prints **how much it raised the mark** — a difference between two readings, and therefore the one per-rung memory statement `getrusage` can support:

```text
peakrss 116.1 MiB (process, raised 0.2 MiB by this rung)
peakrss 345.2 MiB (process, unchanged by this rung — the mark is an earlier one's, and
                   this rung's own peak is only bounded by it)
```

**What this cannot decide** is a per-rung threshold for a rung that raised nothing: that rung's own peak is bounded above by the mark and unmeasured below it. A threshold of that form is decidable only against the **ladder's** peak, which is a stricter and different claim.

Which of the two a pass line means is a property of the pass line, and [FINDINGS milestone 8 §8](FINDINGS.md) is where one of them ran out of instrument. Milestone 8's memory clause reads the **ladder's** peak, decided in [D-014](DECISIONS.md) on the grounds that the figure exists for an adopter's memory budget and an adopter runs a process rather than a rung.

**Any pass line quoting a memory figure says which of the two it means, or it is not a pass line.**

Giving each rung its own process would give each a clean mark and destroy the ladder prefix that rule 3, repaired, establishes as the thing a repetition must hold. The prefix is worth more than the attribution.

### 2.8 What a query allocated, which the peak mark cannot say

§2.7 ends at what `getrusage` cannot decide. This is the figure that decides the other half: each rung prints `TotalAlloc` and `Mallocs` differences taken around it, divided by the samples that did the work.

```text
alloc  10869.0 KiB/query  15478 allocs/query  (2122.8 MiB this rung)
```

Per query and not per rung, because a rung total is a function of how long the rung ran while a per-query figure is a property of the work — and a claim about a fix has to be held against the second. Shed requests are not in the denominator: the driver sheds by never dispatching, so a shed request allocated nothing.

Both reads sit outside the measured window, and the second is **last** in the after-snapshot block: `ReadMemStats` is the only read there that stops the world, and a counter read after it would be charged with this instrument's own stop. `TotalAlloc` and `Mallocs` are cumulative and monotonic, so a collection landing inside the rung moves neither difference.

**What it cannot say is where the bytes went.** `-memprofile` writes the `allocs` profile for that, by call site, cumulative since process start — which is also why it is cheap to ask: an allocation that happens once per query is attributed by a twenty-second run and needs no ladder. The cost of cumulative is that the profile carries the index mapping and the cold pass beside the rungs; those are separate call sites, so it is a matter of reading the right subtree.

**Two figures, two purposes, and they move independently.** Bytes per query is a peak-memory statement; allocations per query is a GC-pacing one. The first change measured with this instrument moved bytes by 30.7% and the count by 0.06% ([FINDINGS milestone 8 §10](FINDINGS.md)), which is what "independently" means in practice.

## 3. Judgment rules — fixed before the numbers exist

Rules 1 and 2 were registered in `.claude/plans/weft-m5.plan.md` before the instrument produced a publishable figure, the same discipline [D-004](DECISIONS.md) applied in milestone 4. Rules 3 to 6 were registered in `.claude/plans/weft-m7.plan.md` and committed here **before milestone 7's campaign measured anything** — checkable with `git log --oneline -- docs/PERF.md`, and checkable on purpose.

### Rule 1 — which load point the headline is quoted at

A p99 has to be quoted at some load, and choosing that load after seeing the p99s is how a performance claim is made to say whatever its author wants. So:

- The ladder is five rungs at 12.5%, 25%, 50%, 100% and 200% of the throughput a warm sequential replay achieved.
- **Saturation** is the lowest rung whose p50 is strictly greater than twice the unloaded p50. Strictly, so a rung sitting exactly at twice is still quotable.
- **The headline** is the rung nearest half the saturation rate. A ladder that never saturates has no such midpoint, and the headline is its top rung, with the report saying saturation was not reached.

Every rung is published regardless. The rule selects which one the one-line summary quotes.

**The rule is not asked unless there is a ladder to ask it about.** `loadgen.RuleApplies` is the guard, and both commands ask it rather than each spelling a check for itself — rule 2 produces a ratio, so a claim admitted on one side alone moves the published number.

A ladder is at least two rungs **and every rung intended**. Three shapes fail that:

| Shape | Why it fails |
| --- | --- |
| An explicit `-rate` | one rung an operator chose. `SaturationRate` returns it whenever its p50 passed twice the unloaded median, because there is nothing for it to be *first* past, and `HeadlineRate` returns it either way — so the summary would print a hand-picked load point wearing a measured one's label |
| A ladder cut short | a run interrupted during rung three of five has three medians for five intended rates. Until milestone 7 both commands trimmed their rates to match before asking, so the summary said "saturation: not reached" about a ladder whose top rungs never ran. They now pass the ladder they *intended* |
| A ladder the operator named | `-rates 3.21,6.42,12.84,25.67` is four rungs with every one intended, so it satisfies `RuleApplies` completely |

The third is the only one the shape check cannot see. What disqualifies it is not its shape but where it came from: choosing the rungs and then letting the rule pick among them is the same act as choosing the load point, at one remove.

So provenance travels out of `benchRates` beside the rates rather than being re-derived at the bottom of the summary, where by then there is nothing left to read it off. Each entry in the list is held to the bounds a lone `-rate` gets, and held to them **at flag time**: a four-rung sweep whose third entry was a typo is ninety minutes spent before anything says so.

In all three cases the summary says how far the run got, suppresses the claim, and still prints every rung's own figures. Suppressing a claim is not suppressing a measurement — rule 3's second and third repetitions are read off exactly that path. No published milestone 5 figure moves: that ladder completed.

### Rule 2 — what counts as the same order of magnitude

`p99(weft, text, headline rung) ≤ 10 × p99(bleve, text, headline rung)`, both measured on the same machine in the same session, on the same 171,332 documents and the same 50 judged queries at k=10, through the same driver.

A miss is recorded as a miss, with the profile that explains it — which is what milestone 4 did to the graph scorer, and what this repository does with a result it did not want.

### Rule 3 — what a repetition is

**FALSIFIED 2026-08-21 by the campaign it governed, then REPAIRED 2026-08-22** by the experiment §5.2 registered against it. [D-012](DECISIONS.md), [D-013](DECISIONS.md).

> **The falsified form, left standing as the record of an attempt.**
>
> The rule directed a median over three observations of one rung. Three observations at 25.67 q/s gave 37.9 ms, 1.539 s and 416 ms, and two of them shed enough to fall under the p99 sample floor. The flat one was the fourth rung of a ladder; the two collapses were single rungs out of a warm-up. **A rung measured alone is not the same rung.**
>
> So there is no median, and the arithmetic below — still correct about why three sweeps cannot be compared — was answering the wrong question. [FINDINGS milestone 8 §2](FINDINGS.md) answers what a repetition must hold constant: the ladder prefix, at depth.

§5 has said "the median of three repetitions with the spread reported beside it" since milestone 5, and milestone 5 published one run ([FINDINGS milestone 5 §4.5](FINDINGS.md)). The rule was never wrong; it was never made operable. [D-011](DECISIONS.md) carries the choice behind the operable form and what would show it was the wrong one.

**A repetition is not another sweep of the ladder.** Every rung's rate is derived from `benchUnloaded` — 200 sequential requests, taken fresh each run — so three sweeps produce three different sets of five rates, and "the same rung" in two of them is two different loads. There is nothing to take a median of.

| | command | what it produces |
| --- | --- | --- |
| repetition 1 | `-rate 0` | the full ladder. Rule 1 selects the headline rate **R** |
| repetition 2 | `-rate R` | one rung at R, same `n` |
| repetition 3 | `-rate R` | one rung at R, same `n` |

The published p99 is the **median of the three**, with the spread reported as the minimum and maximum beside it. Repetitions 2 and 3 print no headline label, and that is rule 1's guard working rather than a defect: R was selected by the rule in repetition 1 and is being *reused*, not selected again.

Two things are recorded alongside, because they are the only evidence about what the spread is made of:

- **Each repetition's own unloaded p50.** If the three drift, the machine drifted, and part of the spread is that rather than the engine.
- **That R is quoted to two decimals.** The summary prints `%.2f`, and repetitions 2 and 3 run at the rounded figure — about 0.04% off repetition 1's actual rung.

#### Rule 3, repaired — a repetition is the ladder, with its rates named

The arithmetic above is right that rates cannot be *derived* twice. What does not follow is that the ladder cannot be the unit: name the rates and the difficulty is gone.

`-rates` is that instrument, and the reproduction that licensed it is [FINDINGS milestone 8 §2](FINDINGS.md) — the same ladder rung for rung, its headline rung 0.07% from the observation milestone 7 could not repeat.

| | command | what it produces |
| --- | --- | --- |
| repetition 1 | `-rate 0` | the full ladder. Rule 1 selects the headline rate **R** |
| repetition 2 | `-rates <repetition 1's rungs, through R>` | the same ladder and the same prefix, rates named rather than derived |
| repetition 3 | `-rates <repetition 1's rungs, through R>` | again |

Everything above still holds unchanged: the median of the three at R with the spread beside it, each repetition's own unloaded p50 recorded, R quoted to two decimals, and no headline label on repetitions 2 and 3.

**What it costs:** a repetition is now the whole ladder up to R, roughly **97 minutes** for the `text` arm rather than 6.5, so three of them is 4.9 hours. `text+vector` gets one named ladder rather than three, which is rule 6's first cut applied to a budget that grew. [D-013](DECISIONS.md) carries the argument.

### Rule 4 — how the `text+vector` arm gets a p99

§2.3 refuses a p99 below 10,000 samples, and that rule is **not relaxed here.** The arm is four times slower per query, which is why milestone 5 published no tail for it — the arm a user would actually deploy. Lowering the bar to print something would trade the one property that makes a published tail worth reading.

The sample depth is staged instead:

1. **A thin ladder**, `-arm text+vector -rotations 40` — 2,000 samples per rung. A p50 needs 200, so rule 1 has everything it needs to select a load point; p95 prints, p99 does not, and its absence there is correct.
2. **A deep rung** at the rate that ladder selected, `-rotations 200` — 10,000 samples. This is where the arm's p99 comes from.

Selection and measurement therefore run at different sample depths. Registered here so that it is a design rather than a later excuse — and if the thin ladder picks a different rung than a deep one would, that is a finding about how sample depth moves rule 1, and it gets published as one.

### Rule 5 — what happens if the re-measurement disagrees with milestone 5

1. **Milestone 7's median becomes the published figure.** Milestone 5's 108.193 ms moves to a footnote and is labelled what it is: a single observation.
2. **Rule 2 is re-judged on the new medians**, weft's against bleve's.
3. **If the worst of the three observations exceeds 10× bleve's median**, the verdict is published as *"clears the bar at the median, and the spread reaches it"*. Pass or fail is not decided by the median alone when the spread is the thing this milestone exists to measure.
4. **If the three observations spread by more than 20%, that is the result.** No fourth run is added. Measuring until the answer settles is the failure this whole section is built to prevent.

### Rule 6 — the budget, and what gets cut first

The campaign is roughly 14.6 hours of machine time, derived from milestone 5's measured rates:

| | estimate |
| --- | --- |
| `text` ladder (repetition 1) | 1.5 h |
| `text` headline rung × 2 | 1.6 h |
| bleve ladder + headline × 2 | 0.5 h |
| `text+vector` thin ladder | 1.2 h |
| `text+vector` deep rung × 3 | 9.8 h |

If it overruns, the order of cuts is fixed **now**:

1. `text+vector`'s deep rung drops from three repetitions to **one**. That arm's goal is that a p99 exists at all; the spread comes from the `text` arm.
2. The thin ladder drops to `-rotations 20`. A p50 still prints at 1,000 samples.
3. **The `text` arm's three repetitions are not cut.** They are the milestone.

Anything cut is published as cut, beside the figure it affects.

## 4. What is not matched, and which way each biases

| | weft | bleve | direction |
| --- | --- | --- | --- |
| analyzer | lowercase, split on non-alphanumeric | `standard`: also stop words + Porter stemming | bleve does more work indexing, queries a smaller postings space |
| arm | `text`, and `text+vector` separately | `text` only | the hybrid comparison is not made at all |
| stored fields | none | disabled | matched |
| rank cut | k=10 | k=10 | matched |
| corpus text | `title + " " + text` | `title + " " + text` | matched |

The analyzer row is the one to watch: stop-word removal makes bleve's common-term postings much shorter, which is a real advantage on queries like "what is the origin of COVID-19", and weft has no stop-word list.

It is a difference in what the two engines do, not a flaw in the measurement, and it is not plausibly worth an order of magnitude — which is the only thing rule 2 asks.

## 5. Reproducing

```bash
# One-time, from the repository root. Hours; see EVAL.md §3.
make eval-data

# weft. Roughly 90 minutes for the text ladder on the machine below.
make bench                                   # text arm, full ladder
make bench BENCHFLAGS='-arm text+vector'     # the deployable arm, fewer samples

# bleve. Build once (about 20s), then the same ladder.
cd bench && go run . -build
make bench-compare
```

**Run them one at a time.** Both saturate the machine at their upper rungs, and a run sharing a host with anything else is measuring the neighbour. The first attempt at these numbers overlapped a bleve index build with a weft ladder and was discarded for exactly that reason.

A rung prints a progress line every 30 seconds — `... 3000/10000 done, 15m0s elapsed`. It is written from a goroutine that serves no request, for the reason [loadgen.Progress](../internal/loadgen/progress.go) documents: a line printed from inside the request lands in the latency that request records, and ten such samples reach a p99 taken over ten thousand.

### 5.1 The campaign, in order

**Rule 3 was falsified by the campaign this sequence describes** — see the marking on rule 3 and [D-012](DECISIONS.md). Read the second block as the record of an attempt.

```bash
# ---- text arm, repaired: one sweep, then the same ladder named twice
make bench                                                   # repetition 1. Read R and its rungs off the report.
make bench BENCHFLAGS='-rates <rungs through R>'             # repetition 2
make bench BENCHFLAGS='-rates <rungs through R>'             # repetition 3
```

Roughly 4.9 hours for the `text` arm alone. bleve gets the same treatment; `text+vector` gets one named ladder, under rule 6's first cut.

```bash
# ---- the falsified form, kept as the record of an attempt
make bench                                   # repetition 1. Read R off the HEADLINE line.
make bench BENCHFLAGS='-rate R'              # repetition 2
make bench BENCHFLAGS='-rate R'              # repetition 3

# ---- bleve: the same treatment, so rule 2 compares like with like
cd bench && go run . -build                  # once, ~20s
make bench-compare                           # ladder. Read R_bleve off its HEADLINE line.
make bench-compare BENCHFLAGS='-rate R_bleve'
make bench-compare BENCHFLAGS='-rate R_bleve'

# ---- text+vector: thin to select, deep to measure (rule 4)
make bench BENCHFLAGS='-arm text+vector -rotations 40'   # selects R_v
make bench BENCHFLAGS='-arm text+vector -rate R_v'       # 10,000 samples, the p99
```

**Record from each run:** the rate, `n`, `inflight`, `GOMAXPROCS`, the unloaded p50, and whether `shed` was zero.

Repetitions whose unloaded p50 has drifted from the others are **discarded and re-run, and the discard is published** — a run thrown away silently is indistinguishable from one that was never made.

Logs go outside the repository. What gets committed is the figures in [FINDINGS](FINDINGS.md), not the transcripts.

The reason any of this is a procedure rather than a habit: a single run on a shared machine is a number nobody can reproduce, which is the lesson [FINDINGS milestone 4 §4.2](FINDINGS.md) paid for once already, and milestone 5 §4.5 paid for again.

### 5.2 What a repetition must hold constant — registered before it is measured

Milestone 7 measured 25.67 q/s three times and got 37.9 ms, 1.539 s and 416 ms ([FINDINGS milestone 7 §1](FINDINGS.md)). The one structural difference that survived every check: the flat observation was the **fourth rung of a ladder**, ninety-one minutes into the process; the two collapses were that rate **alone**, out of a 200-request warm-up.

Two readings remain and neither has been varied deliberately — a GC pacer that arrived at the load with a heap goal already grown to meet it, or `inflight` 40 admitting a burst at rung start that a process climbing from a lower rung never sees.

`-rate 0` cannot separate them. The sweep derives its five rates from whatever that run's sequential throughput happens to be, so a with-prefix run and a without-prefix run land on different rates, and prefix is confounded with rate. `-rates` names them, and a named ladder gets no headline — correct here, because nothing in this experiment is a load point the rule selected.

Four runs, two variables, all at `-rotations 200` so the rung under test carries the 10,000 samples §2.3 requires:

```bash
# The prefix is reproduced, not shortened: 91 minutes of climbing is the variable.
caffeinate -dimsu make bench BENCHFLAGS='-rates 3.21,6.42,12.84,25.67'
caffeinate -dimsu make bench BENCHFLAGS='-rates 3.21,6.42,12.84,25.67 -inflight 10'

# The same rate alone, which is what repetitions 2 and 3 were.
caffeinate -dimsu make bench BENCHFLAGS='-rates 25.67'
caffeinate -dimsu make bench BENCHFLAGS='-rates 25.67 -inflight 10'
```

Both arms go through `-rates` rather than one through `-rate`, so provenance is identical across all four and the only things that differ are the two variables. `inflight` 40 is this host's default — 4 per core at `GOMAXPROCS` 10 — and 10 is one per core, the other principled point on the same rule rather than a tuned value.

**Compared, at the 25.67 q/s rung only:** p50, shed count, peak RSS, GC cycles. Those are the four figures whose milestone 7 values are already on the page. No headline is quoted from any of these runs.

| | machine time |
| --- | --- |
| with prefix, `inflight` 40 | 1.6 h |
| with prefix, `inflight` 10 | 1.6 h |
| no prefix, `inflight` 40 | 7 min |
| no prefix, `inflight` 10 | 7 min |

Roughly 3.5 hours, and almost all of it is the prefix. That asymmetry is the finding's price: the cheap arm is the one already measured three times.

**What each outcome licenses, fixed now:**

1. **The collapse follows the prefix at both `inflight` values** — a repetition must hold the ladder prefix constant. The cost is that a repetition becomes a ladder again, which [D-011](DECISIONS.md) showed cannot be compared across sweeps whose rates are derived, so the rungs must be *named*.
2. **The collapse follows `inflight` at both prefixes** — the burst reading. `inflight` becomes a constant the procedure states rather than a default that happens to be 4 per core on this host.
3. **Both move it** — both are pinned, and every published figure quotes both.
4. **Neither reproduces the collapse** — then the variable is something not yet named, the published run count stays at **one**, and no further arm is added looking for a shape that reproduces. Rule 5 clause 4's reason applies unchanged.

**This is an experiment, not a replacement for rule 3.** [D-012](DECISIONS.md) decided that the repair may not be chosen inside the milestone whose numbers falsified the rule, because three candidate repairs were already visible and the reason to prefer one was which run it would have made look reproducible.

#### Outcome, 2026-08-22: clause 1 fired

The verdict and every figure are [FINDINGS milestone 8](FINDINGS.md); the repair it licensed is *rule 3, repaired*, and [D-013](DECISIONS.md).

What ran differed from what is registered above in three ways, recorded because a procedure that gets edited to match what happened is not a procedure:

1. **Reordered** — the two no-prefix arms ran first. Outcome 4 would have made the expensive arms pointless, and they cost seven minutes each to rule out.
2. **A cut probe inserted** — the with-prefix arm was first run at `-rotations 40` (~20 min) on the argument that p50, shed, RSS and GC cycles all print at 2,000 samples. It did not reproduce, which left the depth confound the cut created, so the deep arm ran anyway. The 20 minutes bought a finding rather than the answer they were spent on ([FINDINGS milestone 8 §3](FINDINGS.md)).
3. **One arm dropped** — deep prefix at `inflight` 10, 1.6 h. Runs A and B had already shown `inflight` does not gate the collapse. So clause 1's registered wording, "at both `inflight` values", is **half-tested**, and it is published that way.

### 5.3 Milestone 8's memory clause, judged — registered before it is measured

[D-014](DECISIONS.md) recorded the clause as a miss on the ladder's peak, 345.2 MiB against 250, and kept milestone 10 shut on the grounds that the miss was a target rather than a verdict.

[FINDINGS milestone 8 §10](FINDINGS.md) is that target being attacked: the first attempt was measured out; the second — `Index.LookupInto` — cut what a query allocates by **30.7%**, from 15,691.3 to 10,869.0 KiB, and `make eval` puts nDCG@10 at 0.5826 and 0.6211, identical to four decimals.

None of that is a ladder figure. One run decides it, and it is the same ladder the clause was judged on so that nothing but the engine differs:

```bash
date; caffeinate -dimsu make bench BENCHFLAGS='-rates 3.41,6.82,13.64,27.28'; date
```

97 minutes, `-rotations 200`, `inflight` 40, `text` arm. `date` either side, because a machine that slept is what [milestone 7 §4.1](FINDINGS.md) cost.

**Compared against [FINDINGS milestone 8 §7](FINDINGS.md), rung for rung:** ladder peak RSS, each rung's `raised`, bytes and allocations per query, the 13.64 q/s rung's p99, and shed and p50 at 27.28 q/s. The last two are the clauses that already passed, re-measured rather than assumed — a memory fix that broke them would otherwise be published as a win.

**What each outcome licenses, fixed now:**

1. **Ladder peak ≤ 250 MiB** — the clause is met, the milestone's three targets are all met, and **milestone 10 does not fire.**
2. **> 250 MiB, and the profile puts the remainder in the Go heap** — one more engineering round inside milestone 8, and **that one is the last**. A second miss after it fires milestone 10.
3. **> 250 MiB, and the profile puts the remainder in mapped pages or runtime fixed costs** — [D-014](DECISIONS.md)'s revival condition, and **milestone 10 fires**, on the profile rather than on the number.
4. **The middle-rung excursion** — 13.64 q/s gave p99 849.853 ms and raised the mark by 206.6 MiB while shedding nothing. If the tail and the memory move together, they had one cause and it is named. If not, that is a second finding for the PRD's open questions.

**Repetitions** follow *rule 3, repaired* — the same named ladder, three times, 4.9 hours. Cut order, fixed now: repetition 3 first, and then the two observations are published as two observations and not as a median; then repetition 2, published as single. **The judgment run is not cut.**

#### Outcome, 2026-08-24: clause 1 fired

Ladder peak **100.7 MiB** against 250, shed **0** at 27.28 q/s, p50 **33.470 ms**, and the 13.64 q/s excursion went with the memory — p99 849.853 ms to 53.868 ms, the mark raised by 206.6 MiB to 0. Every figure and the caveats are [FINDINGS milestone 8 §11](FINDINGS.md). **Milestone 10 does not fire.**

What differed from what is registered above:

1. **The repetitions were not run.** Cut 2 of the order above, taken deliberately: the judgment run is published as a single observation and says so beside every figure.
2. **An earlier attempt at the same command was interrupted** after 42 minutes, 8,635 of 10,000 samples into rung 1. It produced one datum worth keeping — 10,867.3 KiB/query, 0.02% from the smoke run's figure at ladder depth — and no verdict, because the peak is decided at a rung it never reached. Nothing about it was published as a measurement.

### 5.4 Milestone 9's read clause, judged — registered before it is measured

**The `-writes` arm has never been registered.** [FINDINGS milestone 5 §3.3](FINDINGS.md) publishes 11.063 s of held lock and a 12.539 s read wait from it, and no section of this file said what command produced them or what any result would license. That gap is closed here before the arm is run again.

```bash
date; caffeinate -dimsu make bench BENCHFLAGS='-writes -writedocs 20000'; date
```

About 45 minutes: a 626 MiB copy of the index, a warm-up rotation, then 10,000 samples at about 3.9/s, `inflight` 40, `text` arm.

**`-writedocs 20000` is the load-bearing flag.** 20,000 exceeds `ivfMinDocs` (16,384), so the commit under measurement trains an IVF partition — which is where 11.014 of the 11.063 seconds went. A smaller batch skips the training and prices a different event.

**The `text` arm, not `text+vector`.** The lock is a property of the writer and not of the query mix. Measuring under `text` keeps the comparison with §3.3, and this line is the record that it was a choice.

**A `before` run on the unmodified tree, for attribution rather than for the pass line.** The clause is an absolute — 1 second — so the verdict needs only the `after` run. But the 12.539 s was measured on a pre-milestone-8 tree, and `Index.LookupInto` has since changed the read path ([D-016](DECISIONS.md)); without a `before` on the same lineage, a fall cannot be attributed to this round rather than to that one.

**The four quantities compared:** `commit window`, `during` max, `outside` max, `shed`.

**What each outcome licenses, fixed now:**

1. **`during` max ≤ 1 s** — milestone 9's read clause is **met**. `outside` max is published beside it, so that "the lock is no longer the tail" is a comparison and not an assertion.
2. **> 1 s, and `during` max ≈ `outside` max** — the lock is fixed and what remains is not the lock. First candidate: the 30 MiB segment write plus two `fsync`s contending for IO with the read load. That is published, **and the clause is recorded as missed.** A different cause is not a pass.
3. **> 1 s, and `during` max ≫ `outside` max** — an exclusive stretch is still long. Remaining candidates are `adopt`'s `openSegment` and the `clear(ix.postings)` beside it. **One** engineering round is licensed and that one is the last.
4. **A cancelled commit leaves a published generation** — an atomicity regression. Not a performance result and not published as one: it **blocks**, and the change is reverted. `TestACancelledCommitPublishesNothing` is what is supposed to catch it before a bench run can.

**Repetitions** follow *rule 3, repaired* and [D-013](DECISIONS.md): the whole named procedure, three times, 2.25 hours, on top of 0.75 for the `before`. Cut order: repetition 3 first, then repetition 2, then the `before` run — which keeps the verdict and loses the attribution, and the loss is published. **The judgment run is not cut.**

`-rotations` is not cut either. The `during` cohort is decided by the arrival rate times the window length, so a smaller `n` does not enlarge it.

#### Outcome, 2026-08-24: clause 1 fired

`during` max **61 ms** against 1 second, from **13.072 s** on the unmodified tree — and below the `outside` max of 191 ms, so the commit window is no longer the worst part of the run. `shed` 4 to 0.

The commit window itself did **not** move (11.467 s to 11.284 s), which is the design: the encode is as long as it was and is no longer exclusive. Every figure and its caveats are [FINDINGS milestone 9 §3](FINDINGS.md).

What differed from what is registered above:

1. **Cuts 1 and 2 were both taken.** The three `after` repetitions were not run; each side is a single observation and says so beside every figure.
2. **The `before` run was kept**, which cut 3 would have dropped. It is what makes the fall attributable to this round rather than to [D-016](DECISIONS.md)'s read-path change, and it reproduced milestone 5 §3.3 within 4.3% on the same tree lineage.
3. **Two comparisons the run invites are refused rather than published.** `during` max sits below `outside` p50, and `outside` max fell 8.8× in a cohort this change should not have touched. Both are [FINDINGS milestone 9 §4.2 and §4.3](FINDINGS.md), and both need an instrument change this arm does not have: `outside` mixes reads against a one-segment index with reads against a two-segment one, because the commit fires a third of the way in.

### 5.5 Milestone 11's invariance clauses, judged — registered before they are measured

Milestone 11 has no performance target of its own. What it has is three **invariance** clauses whose denominators are figures earlier rounds published — which makes the most plausible failure of this round *quietly moving one of them*. Registering the readings before the runs is the only thing that makes "unchanged" a finding rather than an absence.

The claim under test, stated so the run can falsify it: **every tombstone check takes an empty-set fast path**, so an index that has never had a document deleted pays one branch per lookup rather than one per posting, and the evaluation corpus has no deletions at all.

**Correction, made before run A was executed rather than after.** This section first wrote run A as `-rates 27.28`, a single rung, while its own pass line says *ladder* peak RSS. Those are not the same reading: `peakrss` is a high-water mark (§2.7), so a lone rung starts from a lower baseline than the same rung reached at the end of a four-rung climb. [D-014](DECISIONS.md) fixed the reading as the ladder's peak, so run A is milestone 8's ladder, unchanged.

```bash
# Run A — the published operating point, no deletions. The same four rungs milestone 8
# climbed, because the memory reading is the ladder's peak and not a rung's.
date; caffeinate -dimsu make bench BENCHFLAGS='-rates 3.41,6.82,13.64,27.28'; date
date; caffeinate -dimsu make bench BENCHFLAGS='-writes -writedocs 20000'; date

# Run C — quality, no deletions.
make eval

# Run B — the price of a tombstone, which is an observation and not a pass line.
# Deletes a tenth of the corpus after the copy, then the same rung as A.
date; caffeinate -dimsu make bench BENCHFLAGS='-rates 27.28 -deletefrac 0.1'; date
```

**Run B needs a flag that does not exist yet.** `-deletefrac` is not in `cmd/weft-eval`; it is part of run B's cost and is named here so that "run B was cut" is legible as a decision rather than as an omission.

**The four readings, fixed now:**

1. **A reproduces M8 and M9, C reproduces M4's nDCG.** Shed 0 at 27.28 q/s, p50 ≤ 40 ms (M8: 33.470 ms), ladder peak RSS ≤ 120 MiB (M8: 100.7 MiB), worst read inside a commit window ≤ 1 s (M9: 61 ms), nDCG@10 within −0.005 of 0.5826 / 0.6211. → the invariance clauses pass.
2. **A misses and the miss is attributable to the tombstone checks.** The fast path is leaking. **Revert and fix.** A blocking reading, not a publishable one.
3. **A misses and the same miss reproduces on the pre-milestone tree.** Then it is drift in the machine or the toolchain. **Publish the drift and say so explicitly**, with both numbers; do not let a shared miss read as a pass.
4. **B pushes RSS past 120 MiB, or moves p50.** That is what a tombstone actually costs, and it is the first number anyone has on it. It does **not** block. Publish it as the opening figure on "when does a deleted fraction force a re-index", which [D-019](DECISIONS.md) leaves open.

**Budget and cut order, fixed now.** A is about 2.3 h, B about 0.75 h plus the flag that does not exist, C is minutes. Cut **B first** — the open question stays open and is published as still open; then the `-writes` half of A. **The ladder rung and `make eval` are not cut.**

**One observation each, and that is written beside the numbers.** [D-013](DECISIONS.md)'s truncation applies unchanged: a single run is not a median and must not be called one.

#### Outcome, 2026-08-25: run C executed, reading 1. Runs A and B not executed

`make eval` returned nDCG@10 **0.5826** (`text`) and **0.6211** (`text+vector`), identical to four decimals — the quality-invariance clause passes on one observation. The run also opened a format **version 3** index, so it doubles as the unconverted-read metric on the real 171,332-document corpus rather than on a fixture.

**Run A was attempted and produced nothing.** Started 2026-08-25 19:25:25 KST and terminated during the index load, before the first rung reported: the log holds one `date` line and no measurement. There is no partial ladder to read and none is published. Run B was not attempted; its instrument does not exist.

So **the performance-invariance clauses are unjudged — not passed, and not failed.** That is a departure from the cut order above, and it is recorded here rather than in a commit message. What the order got right is the priority: `make eval` holds this round's quality denominator and it ran.

Whoever picks this up runs the run A block unchanged. [FINDINGS milestone 11 §5](FINDINGS.md) carries it forward.

### 5.6 Milestone 13's invariance clauses — attempted, and the instrument had nothing to say

Milestone 13 has no performance target of its own either. Like §5.5 it has **invariance** clauses whose denominators are milestone 8's and milestone 9's published figures: shed 0 at 27.28 q/s, p50 ≤ 40 ms, ladder peak RSS ≤ 120 MiB, worst read inside a commit window ≤ 1 s.

The argument under test: **the default path gained exactly one branch** — the `ix.tok == nil` test in `Index.Tokenize` — and is otherwise the same code doing the same work, so run A reproduces milestone 8's ladder.

```bash
# Run A — milestone 8's ladder, unchanged, because the memory reading is the ladder's peak.
date; caffeinate -dimsu make bench BENCHFLAGS='-rates 3.41,6.82,13.64,27.28'; date
# Run B — milestone 9's write arm.
date; caffeinate -dimsu make bench BENCHFLAGS='-writes -writedocs 20000'; date
# Run C — quality.
make eval
```

#### Outcome, 2026-08-26/27: run C passed, run A is void, run B was not attempted

Run C reproduced nDCG@10 0.5826 and 0.6211 exactly. Run A missed every clause by one to two orders of magnitude, and **that result is void rather than a verdict**, because the same ladder's top rung misses them the same way on the commit *before* this milestone:

| top rung, 27.28 q/s, single rung | pre-M13 (`1eb2a44`) | M13 (`9d05c16`) |
| --- | --- | --- |
| unloaded p50 | 33.186 ms | 34.072 ms |
| p50 under load | 2.320762 s | 2.616899 s |
| served / sent | 5,109 / 8,875 | 5,233 / 10,000 |
| process peak RSS | 664.2 MiB | 671.7 MiB |
| allocation per query | 10,928.9 KiB | 10,943.5 KiB |

Both arms are single rungs and are compared **only against each other** — §5.5's correction forbids reading a lone rung's `peakrss` against milestone 8's ladder peak. The full ladder, which *is* the comparable reading, peaked at **654.1 MiB against 100.7** and shed **4,857 of 10,000** at the top rung against 0.

**What this licenses, and what it does not.**

1. **The clauses are unjudged and carried forward.** Not met, not missed — unmeasured, on an instrument that was not in a state to measure. This file's own §5 note that a busy machine makes a tail latency a function of whatever else is running applies to a developer machine mid-session too: this run followed a `make eval` over a 626 MiB index, a `go test -race ./...` and two lint passes in the same session.
2. **No regression is attributable to milestone 13, and invariance is not demonstrated either.** The residual gap — p50 +12.8%, served fraction 57.6% to 52.3% — is a single observation apiece, on a machine demonstrably off by 70×, and the baseline arm was cut short at 5 m 25 s of a 6 m 06 s schedule so its `shed` is not comparable in absolute terms. It cannot be read in either direction and is recorded rather than resolved.
3. **Run B was deliberately not attempted.** Spending 45 more minutes on an instrument whose companion arm had just come back void would have produced a second unusable number, not a second data point.

**What a valid attempt needs**, registered so the next one is not a third void run: a machine with no other work on it and no preceding index load in the same session, `date` either side, and the pre-M13 commit measured on the same ladder in the same sitting. Milestone 14 is that attempt.

### 5.7 Milestone 14's four clauses — the procedure registered before the run

Milestone 14 has no code. Its outcome is the verdict on four numbers, and all four have now been left unjudged twice: §5.5's run A died during the index load, and §5.6's came back void after 97 minutes. **Neither failure was short of code. Both were short of measuring conditions.**

| clause | denominator | source |
| --- | --- | --- |
| shed 0 at 27.28 q/s | 0 | [FINDINGS milestone 8](FINDINGS.md) |
| p50 ≤ 40 ms | 33.470 ms | same |
| ladder peak RSS ≤ 120 MiB | 100.7 MiB | same; the reading is fixed by [D-014](DECISIONS.md) |
| worst read inside a commit window ≤ 1 s | 61 ms | [FINDINGS milestone 9 §3](FINDINGS.md) |

So what this round buys is not a run. It is **a procedure that does not produce a third void one**, which [FINDINGS milestone 13 §8 item 2](FINDINGS.md) named as work: *a quiet-machine requirement is now a load-bearing part of the procedure and nothing enforces it.*

**The quality clause is not this round's**, and that is a subtraction rather than an omission. Milestone 13's run C reproduced nDCG@10 0.5826 / 0.6211 to four decimals, so quality invariance is **already judged**. Re-running `make eval` would put a 677 MiB index load into the same session as the ladder — precisely the condition §5.6 identified as what voided the last attempt. **`make eval` is therefore not in this round's run list.**

#### The preflight, and why the unloaded median cannot be the gate

The two-arm table in [FINDINGS milestone 13 §7](FINDINGS.md) narrows the choice of signal to one, because it says which readings the void machine got right:

| reading | void run (M13 arm) | published | ratio |
| --- | --- | --- | --- |
| unloaded p50 | 34.072 ms | 32.231 ms | **1.06× — normal** |
| p50 under load, 27.28 q/s | 2.616899 s | 33.470 ms | **78×** |
| served / sent | 5,233 / 10,000 | 10,000 / 10,000 | 1.9× |
| ladder peak RSS | 654.1 MiB | 100.7 MiB | 6.5× |

**The unloaded median was normal on the machine that voided the run.** Any gate built on the figure `benchWarmup` already computes therefore passes this void through. The one cheap signal that separates the two is **a short loaded probe at the top rate** — 1.04× on the good machine, 78× on the void one. There is nothing to tune in between.

The probe runs as a **separate process**. Inside the ladder it would lift `ru_maxrss` to near its top-rung value before rung 1 reported, and the peak-RSS clause reads the ladder's high-water mark (§2.7, [D-014](DECISIONS.md)). A separate process has its own mark.

```bash
# Preflight: top rate, 10 rotations (500 requests), about a minute.
# These numbers are not published. They decide whether the ladder runs.
make bench-preflight
```

**The pass line, fixed now**: the probe rung's p50 ≤ **twice the same process's unloaded p50**, and shed = 0.

Twice is not arbitrary — it is the constant `loadgen.SaturationRate` already uses: the first rung past twice the unloaded median *is* saturation, and 27.28 q/s was **not** saturation on the published ladder. A machine that saturates at the probe cannot reproduce that ladder.

**This is a documented step, not an enforced one.** `-rates` and `-rotations` both already exist; what the preflight adds over `make bench` is a name in the procedure and a fixed flag pair. The two failures did not happen because a person could not compare two printed numbers — they happened because no probe was run at all before a 97-minute ladder. A `ponytail:` comment on the target carries its ceiling: if a fourth run still comes back void, the probe becomes a `-preflight` flag with an exit code.

**Two alternatives rejected, and the signal that revives each:**

- **Record `getrusage`'s `ru_nivcsw` per rung.** One more field is a few lines, and involuntary context switches are a **direct** measure of contention rather than a proxy. Rejected because there is no threshold: this repository holds zero observations of what `nivcsw` reads on a quiet machine. **Revived by** a run where the preflight passes and the ladder is void anyway.
- **Capture `uptime`'s load average around each run.** Zero lines, and a proxy. **Partly adopted**: not a gate, but logged beside `date`. It does not enter the verdict.

#### The baseline is `700a178`, not `1eb2a44`

§5.6 named `1eb2a44` as its baseline arm. That commit **is not an ancestor of `main`** — it sits on a side branch and was the M13 development base, so nobody else can check it out from this history:

```console
$ git merge-base --is-ancestor 1eb2a44 HEAD
(exit 1 — not an ancestor)
$ git diff --stat 1eb2a44 700a178 -- pkg/ internal/ cmd/ bench/
(empty)
```

`700a178` is milestone 12's merge on `main` and **its code is byte-identical** across all four directories. Substituting it does not change what is measured; it moves the measurement somewhere a reader can reproduce.

The measured arm is **`eedc04a`** (`HEAD`), which has an empty `pkg/`/`internal/`/`cmd/` diff against the `9d05c16` §5.6 measured — the same code.

#### Both arms in one sitting, baseline first

§5.6's "same sitting" requirement is this round's load. Any other work between the arms removes what the A/B is for.

**What is not matched, and which way it biases** (§4's form): the first arm maps the index and leaves the page cache warm, and the second inherits it. **So the order is baseline first:**

- HEAD regresses anyway → it regressed *with* the cache on its side, so **the regression is real**.
- HEAD looks better → a live cache is the competing explanation, and **that sentence is written beside the figure**. No improvement is claimed.

Running both orders turns 3.2 hours into 6.4. At n=1 randomisation buys nothing, so the bias is **named rather than removed**.

**Correction, made before the baseline arm was run rather than after.** This section first wrote the baseline probe as `make -C ../weft-m12-baseline bench-preflight`, and **that target does not exist on `700a178`** — it is added by this milestone. The probe on the baseline arm is therefore spelled as the flags the target hardcodes. Nothing measured changes: `bench-preflight` *is* `bench -rates 27.28 -rotations 10`.

**The lid stays open, and `caffeinate` is not enough on its own.** Added after two arms were discarded for it.

`caffeinate -dimsu` holds `PreventUserIdleSystemSleep`, which stops *idle* sleep and has no power over **clamshell sleep** — closing the lid sleeps the machine on AC at full charge, and the monotonic clock stops with it, which is what `Elapsed` in `internal/loadgen/clock.go` reads as unaccounted time. This file has prescribed `caffeinate -dimsu` since §5.1, and `clock.go` cites *thirteen hours of clamshell sleep* as the failure its suspension check exists to detect, so the remedy has not covered the named failure mode for nine milestones.

`sudo pmset disablesleep 1` would enforce it and is rejected: it needs root, and it leaves a machine that never sleeps if the operator forgets to unset it.

```bash
# The baseline arm is a worktree. The index is shared — the same bytes read by both
# arms is the premise of the A/B.
git worktree add ../weft-m12-baseline 700a178

# Preflight both arms first. Either one failing is reading 1 below. The baseline is
# spelled out because `bench-preflight` postdates 700a178 — see the correction above.
make bench-preflight
make -C ../weft-m12-baseline bench EVAL_DATA=$PWD/.eval-data \
  BENCHFLAGS='-rates 27.28 -rotations 10'

# The ladder, baseline first. About 92 minutes each.
date; uptime; caffeinate -dimsu make -C ../weft-m12-baseline bench \
  EVAL_DATA=$PWD/.eval-data BENCHFLAGS='-rates 3.41,6.82,13.64,27.28'; uptime; date
date; uptime; caffeinate -dimsu make bench \
  BENCHFLAGS='-rates 3.41,6.82,13.64,27.28'; uptime; date

# Milestone 9's clause. First to be cut.
date; uptime; caffeinate -dimsu make bench BENCHFLAGS='-writes -writedocs 20000'; uptime; date
```

#### The four readings, fixed now

1. **The preflight misses its pass line** → **the ladder does not run.** A minute was spent and 97 were saved, and that is recorded. This is **not executed**, not void, and the two are different results.
2. **Both arms reproduce the published figures and HEAD passes all four clauses** → **the four clauses are met.** One observation, and that is written beside the numbers.
3. **Both arms reproduce the published figures and HEAD alone misses a clause** → **a regression attributable to milestone 13.** A blocking reading, on §5.5 reading 2's grounds. Find the cause and fix it.
4. **Both arms miss the published figures by the same margin** → drift that is not this round's. **Publish both numbers and say so explicitly.** But this time it arrives *after* a passing preflight, so reading 4 also publishes **"there is a condition the probe cannot see"** and revives `ru_nivcsw`.

Readings 1 and 4 are new against §5.6. Reading 4 renames §5.5's reading 3; reading 1 exists for the first time because the preflight does.

#### Budget and the cut order, fixed now

| run | command | machine time | cut |
| --- | --- | --- | --- |
| preflight ×2 | `make bench-preflight` | ~2 min | **not cut** |
| ladder — baseline | `-rates 3.41,6.82,13.64,27.28` @ `700a178` | ~92 min | **not cut** |
| ladder — HEAD | the same ladder @ `eedc04a` | ~92 min | **not cut** |
| write arm ×2 | `-writes -writedocs 20000` | ~90 min | **cut first** |

**3.2 hours** for the core, **4.8** with the write arms.

The write arms go first — what is lost is milestone 9's read clause alone, and the other three clauses live on the ladder. **Running one ladder arm is not a cut, it is void production** — that is exactly what §5.6 paid for, so the ladder **runs on both arms or on neither.**

Not run at all, and each said so rather than left out: **`make eval`** (quality is judged), and **`-deletefrac` with §5.5's run B**, which **stays open and is published as open**. Mixing two rounds into one sitting means a void in either cannot be attributed.

#### Outcome, 2026-08-27/28: four probes passed, two ladder attempts, both void

**The clauses are unjudged for the third round.**

| | what happened |
| --- | --- |
| preflight ×4 | **all passed** — 1.00× to 1.05× against a 2× line, shed 0 every time |
| ladder attempt 1 | **SIGTERM at 46 min**, in rung 1 of the baseline arm. Nothing published |
| ladder attempt 2 | **eight rungs completed, both arms printed `DISCARD this run`** |
| write arms | **not reached** — the registered first cut, behind a discard |
| `make eval` | **not run**, by design |

**Which reading fired: none of the four, and the list is the thing that was wrong.** Reading 1 needs a *failing* probe and the probe passed four times. Readings 2, 3 and 4 need arms whose figures can be read, and the instrument's own suspension check refused both. Two entries are added for the next attempt:

- **Reading 5 — the window is known in advance to be unavailable** → *not executed.* One minute spent, 97 saved. Distinct from void.
- **Reading 6 — the run completes and the instrument discards it** → *void, with the cause recorded in-band.* Distinct from reading 4, whose drift has to be inferred from an A/B; here `SuspendTolerance` names the failure and the amount.

**What voided it was not what the preflight was built to catch, and both detectors worked.** The probe measures contention; the failure was suspension, which `SuspendTolerance` caught on both arms with the duration attached — 22m39s at the baseline's second rung, 4h57m08s at HEAD's first.

The mitigation is what failed, and the lid paragraph above is the fix. [D-024](DECISIONS.md)'s own falsification condition — *a ladder that comes back void after a probe that passed* — fired, and the refinement is recorded there rather than quietly dropped.

**One thing the discarded data narrows, registered so the next attempt looks for it.** The top rung collapsed on **both** arms — 1.635754 s and 2.034673 s against a published 33.470 ms, with shed 1,811 and 3,323 and the ladder mark at 660.8 and 675.8 MiB against 100.7 — while rungs 1 through 3 stayed in family, and while four lone-rung probes at the same rate returned 34–35 ms. §5.6 reported the same signature for milestone 13.

**The baseline arm is pre-milestone-13 code and collapses identically, so the cause is not that milestone.** It is not read further here: both arms slept before reaching that rung, and that is exactly what the discard forbids reading through. [FINDINGS milestone 14 §5a](FINDINGS.md) holds the shape and the three surviving candidates.

Whoever picks this up runs the block above unchanged, **with the lid open**. The worktree at `700a178` is already in place.

### 5.8 Milestone 27's arm — the ladder through a socket, registered before the run

The measurement design of §2 carries over **unchanged**: the same open loop, the same rung structure, the same saturation rule, the same `SuspendTolerance`, the same 3.1-hour window and the same lid-open precondition D-024 registered. What changes is which process is measured.

#### What is registered before the run

**Outcome 1 — the shape of the collapse.** The in-process ladder publishes a knee at 27.28 q/s: shed 1,438 of 10,000, p50 39 ms → 1.27 s, RSS 126 → 853 MiB ([FINDINGS M5 §3.2](FINDINGS.md)). The question is whether HTTP moves that knee, and in which direction. **Met or missed, it is published.**

Two rungs of the answer are already predictable and are written down so that finding them is not mistaken for insight: an HTTP run adds a request and a response encoding per query, and it adds a second process's scheduling. Neither is a defect and both are part of what a server costs.

**Outcome 2 — where the memory goes.** The in-process collapse was diagnosed as live heap under concurrency: 30,549 decoded candidate records alive per query, times an in-flight of 40. Over HTTP the `_source` store and the response encoder are new heap on the same side of the process boundary, and the load generator's own buffers are on the other. Whether the knee moves earlier is the interesting half.

**Outcome 3 — shed means something different.** `loadgen.Drive` sheds when the in-flight cap binds, which is a property of the load. A server can also refuse, and the HTTP client is deliberately given **no timeout** so that a slow answer stays a slow answer instead of becoming a shed one. Two causes in one counter is one cause lost.

#### The rule that makes the numbers legitimate

**Every memory and collector figure comes from the server, through `GET /_nodes/stats`.** The load generator holds the in-flight requests and no index at all, so its resident set is precise, stable, reproducible and about the wrong program — which is worse than no number, because it looks like one. The counters the server reports are read from the same instruments `internal/loadgen` reads in-process, so the two arms are not reading different meters.

**A failed counter read ends the run.** There is no fallback to the local counters. A ladder that silently substituted them would produce a report whose latency column is the server's and whose memory column is the load generator's, with nothing in the output saying so. The report header prints `measuring=` for the same reason.

#### The procedure

```bash
make bench-preflight                  # the loaded probe, 28s — D-024
make bench-http BENCHFLAGS='-rates 3.41,6.82,13.64,27.28 -rotations 200'
```

`make bench-http` starts `weftd`, waits for it to listen, runs the ladder and stops it. Started by the target rather than by hand, because a run against a server somebody else started is a run whose data directory and uptime are recorded nowhere.

**There is a step missing from that block, and it is registered here rather than discovered at hour three.** `make bench-http` guards on `$(EVAL_DATA)/index`, which is the *engine's* index — what `make eval-data` produces. The load is served from `$(EVAL_DATA)/weftd` (`cmd/weft-eval/bench.go`, `-http-index papers`), and weftd wants its own layout beside the segments: `_source.json` and `_mapping.json`. **Nothing in this repository populates that directory.** Standing it up is a bulk load of all 171,332 documents over HTTP, it is not in the §5.8 budget above, and until it exists the guard passes and the ladder searches an index with nothing in it. Found twice by two readers and written down neither time; this is the writing down.

Three preconditions, and they are the ones four previous rounds failed on rather than a formality:

1. a quiet 3.1-hour window,
2. **the lid open** — `caffeinate` does not prevent clamshell sleep ([FINDINGS M14 §5a](FINDINGS.md)),
3. a preflight pass immediately before each arm, because the certificate expires.

Detach with the `perl` `fork`/`setsid`/`exec` form; a session-managed background job took a SIGTERM at 46 minutes.

#### What this arm cannot say

The two arms do not run the same query. In-process, the `text` arm scores with `scorer/text`'s BM25; over HTTP, a `match` becomes one `query.Glob` stream per token, fused.

The work has the same shape — a vocabulary lookup and a posting walk per token, then a fusion, then a top-k — and the scoring does not. **No nDCG figure may be derived from this arm**, and a latency difference between the two is a difference between two query plans as well as between two transports.

### 5.9 Milestone 32's instrument changes — registered before the run

Four ladders have come back void and none of them was the engine: a run terminated during the index load, a run on a machine that was not quiet, a SIGTERM at 46 minutes, and a lid that closed. What this section registers is four changes to the *instrument*, made before the next ladder rather than after reading it.

They are registered here because [D-012](DECISIONS.md) forbids the opposite. A rule is worth something only if it was not available to be chosen once the numbers were on screen, and "we increased the sample size because the run we did not like fell under the floor" is exactly the move that constraint exists against. Nothing below changes a pass line. The three clauses that live on the ladder keep their wording and their denominators.

#### Change 1 — `-rotations` default 200 → 240

`loadgen.Printable` requires 100 samples beyond a quantile, so a p99 needs 10,000. The default was 200 rotations over 50 judged queries, which is **exactly** 10,000. One shed request at the top rung therefore erases the figure the ladder exists to produce, and the top rung is where shedding happens. This is not hypothetical: milestone 7's repetitions 2 and 3 shed 1,456 and 1,082 and became unpublishable that way ([FINDINGS M7](FINDINGS.md)).

240 rotations is 12,000 samples — the 10,000 a p99 needs, plus 20% headroom for shed.

**What this can move, stated before it does.** The memory clause reads the *ladder's* peak `ru_maxrss` against 120 MiB, with a published denominator of 100.7 MiB ([D-014](DECISIONS.md)). Twenty percent more requests per rung is twenty percent more opportunity for the mark to rise. If clause 3 comes back missed, this change is a candidate explanation and is on the record as one **before** the run rather than offered afterwards. The other two clauses are insensitive to sample count: shed 0 is shed 0, and a p50 over 12,000 samples is the same statistic as a p50 over 10,000.

`bench/`'s own default stays at 200. The bleve comparison is not held against these clauses, and raising both would only make `make bench-compare` longer.

#### Change 2 — the report prints the send loop's own lateness

`loadgen.Drive` measures each latency from the time the request was *due*, which is the coordinated-omission correction §2.1 is about. What no run has ever printed is whether the send loop met that schedule. Absent it, every void round leaves "was it the instrument?" open to be re-litigated from outside the data.

`Drive` now returns the dispatch lateness of every request — including shed ones, because the loop falling behind and the cap binding happen in the same stretch — and the rung prints its p50 and max. Measured before the `GCPauseTotal` read and appended to a preallocated slice, so the instrument does not become what it measures.

This moves nothing. It is a column that did not exist.

#### Change 3 — the suspension check covers the warm-up

`loadgen.Elapsed` compares monotonic against wall clock and is how a run learns the machine slept. It was called from `benchRung` and nowhere else. `benchCold`, `benchUnloaded` and the span between them used plain `time.Since`.

That is the wrong span to leave uncovered. The unloaded p50 is the denominator of every rung's arrival rate and the reference `SaturationRate` compares each rung against, so a machine that slept through the sequential replay scales the entire ladder from a median of a machine that was not running. Milestone 14's slept arm is where this shows: its unloaded p50 was 39.134 ms against a 32.8–35.1 ms band across the other runs, the single outlier, and nothing in the instrument remarked on it.

The check is now one span across the whole warm-up, and exceeding `SuspendTolerance` is fatal rather than a printed caveat — for the same reason an erroring warm-up is already fatal. These are not requests the run reports; they are what every rate is derived from.

#### Change 4 — `-preflight` exits non-zero

`make bench-preflight` has been a documented step whose pass line was a person reading two printed lines. Its own comment registered the escalation: *"If a fourth ladder still comes back void, this becomes a `-preflight` flag with an exit code."* Four have.

`-preflight` applies the registered pass line — every rung's p50 at most twice this run's unloaded p50, and shed 0 — and returns an error, which the command turns into a non-zero exit. The threshold is `loadgen.SaturationRate`'s constant rather than a number chosen here, and it is strictly past, so a rung sitting exactly at twice still passes. A run that measured no rungs fails: a gate that passes without a measurement is the failure it exists to prevent.

The rate stays in the Makefile. Putting 27.28 in the binary would be a second spelling of the ladder's top rung.

#### The procedure this buys

```bash
make bench-preflight && make bench    # the gate now blocks the ladder
```

The three preconditions of §5.7 and §5.8 are unchanged and are still the ones four rounds failed on: a quiet window, **the lid open**, and a preflight pass immediately before each arm. What changes is that the third is now enforced by the exit code instead of by a person.

The window is longer. Twenty percent more requests per rung puts a two-arm ladder near 3.5 hours rather than 3.2.

## 6. The prediction being graded

The plan's §1 made a falsifiable claim before the instrument existed, and it disagreed with the PRD's own risk table. The PRD says:

> Go GC로 p99 예측 가능성이 낮음 — Medium / Medium

The plan predicted the opposite: that weft's tail is a **working-set** problem rather than a GC problem, because [milestone 3a](FINDINGS.md) pinned the live heap at 74,504 bytes while a query touches 210 MiB of distinct pages.

Its arithmetic was wrong in a way the first measurement caught, and both halves are recorded:

| | plan §1 predicted | measured (task 1, `.eval-data`, 171,332 docs) |
| --- | --- | --- |
| allocation per query, `text` | — | **43.6 MiB**, 75,132 objects |
| allocation per query, `text+vector` | ≤ 124 MiB | **181.9 MiB**, 782,955 objects |
| heap goal | 4 MiB (the floor, since live is 74 KB) | **46–53 MiB** |
| GC cycles per query, `text+vector` | ≈ 31 | **9.4** |

The prediction assumed the live heap stays at its idle 74 KB, so the GOGC target would sit on its 4 MiB floor forever. It does not: a query holds 30,549 candidates and their decoded records alive at once, so live heap during a query is tens of megabytes and the target rises with it. Cycles came out at a third of the prediction and each one has far more to mark — the two errors point in opposite directions.

**The headline conclusion survives the broken path to it**, and that distinction is the finding rather than a footnote. Whether it survives at the tail, under load, is what the ladder answers, and the answer is in [FINDINGS.md](FINDINGS.md) milestone 5.

## Machine

A latency table without the machine it was measured on is not reproducible.

| | |
| --- | --- |
| host | Apple M4, 16 GiB |
| OS | macOS 26.5.2 |
| Go | 1.26.1 darwin/arm64 |
| GOMAXPROCS | 10 |
| GOGC | 100 (default) |
| corpus | 171,332 documents, 50 judged TREC-COVID queries, k=10 |
| weft ladder | one process, five rungs, `-rotations 200` |

**Repetitions: 1 for every published figure, and since 2026-08-22 a second is definable.**

Milestone 7 found the headline load point irreproducible as a lone rung — 37.9 ms, 1.539 s and 416 ms at one rate ([FINDINGS milestone 7 §1](FINDINGS.md)) — and milestone 8 found what a repetition has to hold: reached as the fourth rung of a ladder run to depth, the same rate came back **0.07% from its first observation** ([FINDINGS milestone 8 §2](FINDINGS.md)), which is *rule 3, repaired*.

**No published figure has been re-measured under it.** Each remains a single observation, known to be one draw from a rule that flips on a few milliseconds of rung-1 median (milestone 7 §4.2), and taken on a machine whose sleep state was not recorded (§4.1 there).

The numbers measured on it are in [FINDINGS.md](FINDINGS.md) milestone 5 §1.
