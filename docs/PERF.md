# Performance — how milestone 5's numbers are produced

This file is to milestone 5 what [EVAL.md](EVAL.md) is to milestone 4: the
measurement design and the judgment rule, written down so that a reader can decide
whether to believe the numbers before reading them, and so that the numbers cannot
be chosen after the fact.

The verdict itself lives in [FINDINGS.md](FINDINGS.md), milestone 5.

## 1. What is being measured, and what would falsify it

The PRD's outcome sentence has two clauses and they are graded separately:

> GC pause를 포함한 p99가 공개되고, 기성 엔진과 같은 자릿수임을 보인다

1. **A p99 including GC pause is published.** Falsified by publishing a mean, by
   publishing a p99 from a sample that cannot support one, or by publishing a
   latency figure with no accounting of what the collector contributed to it.
2. **It is within an order of magnitude of an established engine.** Falsified by
   `p99(weft) > 10 × p99(bleve)` measured on the same machine, corpus, query set,
   arm and load generator.

The plan also fixed a prediction before any of this was built, and it is graded
too — see §6.

**What clause 2 does not claim, registered before the numbers existed.** If weft
turns out faster, that is not a claim of superiority. bleve carries an analysis
pipeline, stored fields, a facetable index and a segment merge policy weft does
not have, and it is being asked to do a job smaller than the one it was built for.
The only supportable reading of a favourable number is "the same order of
magnitude", which is the same reading as an unfavourable one inside 10×.

## 2. The instrument

`weft-eval bench` and `bench/` (the bleve side) are two commands over one driver,
`internal/loadgen`. Sharing the driver is not tidiness: clause 2 compares two
numbers, and a bias in one implementation of an open loop that is absent from the
other would move the ratio without moving either engine.

### 2.1 Open loop, and why a closed one would invalidate the whole file

A closed-loop driver sends the next request when the previous one returns. Under
that design a server that stalls **receives less load**, so the stall shows up as
one slow request and every request that would have arrived during it was never
sent. The reported p99 is then a p99 of a load the server itself chose. This is
coordinated omission, and a milestone whose deliverable is a p99 cannot be
measured by the instrument that hides it.

Here request *i* is due at `start + i/rate` whatever request *i-1* is doing, and
its latency is measured from that due time rather than from when it began running.
A stall lands on every request it delayed.

`internal/loadgen.TestOpenLoopDoesNotLetTheServerSlowTheLoad` is the assertion: a
synthetic server serialising on a mutex stalls once, and the test requires many
samples above the stall rather than one. A closed loop produces exactly one.

### 2.2 The one place the loop stops sending

In-flight requests are capped at four per core, and exceeding the cap is **counted
and reported as `shed`, never waited on**. Waiting would restore the closed-loop
bias at precisely the load where it matters most. The cap exists because a query's
working set is 210 MiB on this corpus ([FINDINGS](FINDINGS.md) milestone 3b §2),
so an unbounded loop at twice saturation is an out-of-memory rather than a
measurement.

A rung reporting a large `shed` is reporting that the load was not applied, and
its distribution should be read as "of the requests that were sent".

### 2.3 Quantiles: nearest rank, and absent when thin

Nearest rank on the sorted sample, `ceil(q·n)` clamped into range. The
interpolating definitions return a value between two observations — a latency no
request experienced — and a published tail figure should be a number the server
actually produced.

A quantile needs **100 samples beyond it** or it is not printed at all:

| quantile | samples required |
| --- | --- |
| p50 | 200 |
| p95 | 2,000 |
| p99 | **10,000** |
| p99.9 | 100,000 |

Below that a figure is an order statistic of a handful of observations and moves
by milliseconds between runs. It is left out rather than printed with a caveat,
because a caveat beside a number is not what a reader carries away. This is why a
rung is 200 rotations of the 50-query set, and why the `text+vector` arm — four
times slower per query — is reported without a p99 unless it was given the same
10,000 samples. Rule 4 is how that arm is given them without relaxing this table.

### 2.4 GC accounting, and the design that was measured and discarded

The first design classified: a sample was *GC-hit* when the completed-cycle
counter moved during it, and the report compared the hit and free cohorts. A smoke
run made **485 collections over 200 queries**, so all 200 samples were GC-hit and
the free cohort was empty. The classification is not wrong, it is degenerate at
any allocation rate weft produces — §6 has the allocation figures.

The design that ships **charges** instead. A stop-the-world pause stops every
goroutine, so pause time that elapsed inside a request's window is time that
request provably spent stopped. Two distributions are printed:

- `latency` — as measured, which is the "including GC pause" the PRD asks for
- `minus STW` — the same samples with their charged pause subtracted

and the gap between the two p99s is the collector's contribution to the tail.

**Two limits, stated beside the number rather than after it.**

Stop-the-world is not all the collector costs. Mark assist charges collection work
to the goroutine that allocated — which is the query — and none of it is
stop-the-world or visible in either distribution above. The smoke run that prompted it showed
exactly this: STW was 0.053% of the wall clock while the median query ran 66.9 ms
against an unloaded 35.7 ms. `GC CPU share`, from
`/cpu/classes/gc/total:cpu-seconds`, is reported for that reason.

And the counters are read through `runtime/metrics`, not `runtime.ReadMemStats`,
which stops the world to answer. An instrument that pauses the program once per
request would be measuring pauses it caused.

### 2.5 Page faults, which say what the tail was waiting on

`getrusage` around each rung, reported as a difference:

| counter | what it means |
| --- | --- |
| `minflt` | a page the kernel already had — page cache hit, hundreds of nanoseconds |
| `majflt` | a page the kernel had to fetch — storage, four to five orders of magnitude more |
| `nvcsw` | the process yielded; it blocked on something |
| `nivcsw` | the scheduler took the core away — over-subscription, from inside |

`peakrss` on the same line is the exception and is labelled `(process)` for it.
`ru_maxrss` is a high-water mark the kernel never lowers, so there is no "during
this rung" reading of it: what it reports is the peak the process has reached by
the end of that rung, index mapping and cold pass included. It is monotonically
non-decreasing down the table by construction, and a rung's own footprint is not
recoverable from it.

`GC CPU share` **is** a per-rung figure, and getting there needs the ratio of two
differences rather than the difference of two ratios.
`/cpu/classes/gc/total:cpu-seconds` and `/cpu/classes/total:cpu-seconds` are both
cumulative since process start, so a running share stops moving once the process
has any history — by the fifth rung a rung that gave a third of its CPU to the
collector and one that gave none read the same. `loadgen.GCCPUShareBetween` takes
both totals at each end of a rung and divides the differences.

A tail dominated by `majflt` is a storage problem, one dominated by `nivcsw` is a
concurrency problem, and one with neither is the collector's or the code's. None
of that is visible in latency alone, and §6's prediction is precisely a claim
about which of them binds.

### 2.6 Warm and cold are separate measurements

Fifty queries replayed two hundred times finds the page cache fully populated from
the second rotation on. Every ladder number is therefore **warm**, and the 210 MiB
of distinct pages a query touches is largely a page-cache figure rather than a
storage one.

That is a property of any steady-state server and is reported as the headline. The
first pass over the query set is reported separately as `cold` — 50 samples, so no
p99, but a maximum and a fault count, which is what the cold path can honestly
support. There is no portable way to drop the page cache, so "cold" here means
"first touch in this process", and a second run of the command against an already
warm host cache is warmer than the first. The cold line is read for its `majflt`,
not for its latency.

### 2.7 The memory figure is the process's, and only its increase belongs to a rung

`peakrss` is `ru_maxrss`: a high-water mark the kernel never lowers and offers no way to
reset. There is no during-this-rung reading of it, so on a ladder the figure a rung
prints is **the peak the process has reached by the end of that rung**, cold pass and
every earlier rung included.

The line has always said `(process)`, and that turned out not to be enough. Milestone 8's
own pass line was written as *RSS ≤ 250 MiB at 27.28 q/s*, and the ladder that judged it
printed 345.2 MiB there — the mark set two rungs earlier at half the rate
([FINDINGS milestone 8 §7](FINDINGS.md)). So each rung now also prints **how much it
raised the mark**, a difference between two readings and therefore the one per-rung
memory statement `getrusage` can support:

```
peakrss 116.1 MiB (process, raised 0.2 MiB by this rung)
peakrss 345.2 MiB (process, unchanged by this rung — the mark is an earlier one's, and
                   this rung's own peak is only bounded by it)
```

What this **cannot** decide is a per-rung threshold for a rung that raised nothing: that
rung's own peak is bounded above by the mark and unmeasured below it. A threshold of that
form is decidable only against the **ladder's** peak, which is a stricter and different
claim. Which of the two a pass line means is a property of the pass line, and
[FINDINGS milestone 8 §8](FINDINGS.md) is where one of them ran out of instrument.

Milestone 8's memory clause reads the **ladder's** peak, decided in
[D-014](DECISIONS.md) on the grounds that the figure exists for an adopter's memory budget
and an adopter runs a process rather than a rung. Any pass line quoting a memory figure
says which of the two it means, or it is not a pass line.

Giving each rung its own process would give each a clean mark and destroy the ladder
prefix that rule 3, repaired, has just established as the thing a repetition must hold.
The prefix is worth more than the attribution.

## 3. Judgment rules — fixed before the numbers exist

Rules 1 and 2 were registered in `.claude/plans/weft-m5.plan.md` before the
instrument produced a publishable figure, the same discipline
[D-004](DECISIONS.md) applied in milestone 4. Rules 3 to 6 were registered in
`.claude/plans/weft-m7.plan.md` and committed here **before milestone 7's campaign
measured anything** — the order is checkable with
`git log --oneline -- docs/PERF.md`, and it is checkable on purpose.

**Rule 1 — which load point the headline is quoted at.**

A p99 has to be quoted at some load, and choosing that load after seeing the p99s
is how a performance claim is made to say whatever its author wants. So:

- The ladder is five rungs at 12.5%, 25%, 50%, 100% and 200% of the throughput a
  warm sequential replay achieved.
- **Saturation** is the lowest rung whose p50 is strictly greater than twice the
  unloaded p50. Strictly, so a rung sitting exactly at twice is still quotable.
- **The headline** is the rung nearest half the saturation rate. A ladder that
  never saturates has no such midpoint and the headline is its top rung, with the
  report saying saturation was not reached.

Every rung is published regardless. The rule selects which one the one-line
summary quotes.

*The rule is not asked unless there is a ladder to ask it about.*
`loadgen.RuleApplies` is the guard, and both commands ask it rather than each
spelling a check for itself — rule 2 produces a ratio, so a claim admitted on one
side alone moves the published number. A ladder is at least two rungs **and every
rung intended**. Two shapes fail it:

- **An explicit `-rate`** is one rung an operator chose. `SaturationRate` returns
  it whenever its p50 passed twice the unloaded median, because there is nothing
  for it to be *first* past, and `HeadlineRate` returns it either way — so the
  summary would print a hand-picked load point wearing a measured one's label.
- **A ladder cut short.** A run interrupted during rung three of five has three
  medians for five intended rates. Until milestone 7 both commands trimmed their
  rates to match before asking, which made the two indistinguishable: the summary
  said "saturation: not reached on this ladder" about a ladder whose top rungs
  never ran, and quoted a headline off the rung the interrupt truncated. They now
  pass the ladder they *intended*.

**A ladder the operator named** is the third case, and the only one the shape check
cannot see. `-rates 3.21,6.42,12.84,25.67` is four rungs with every one of them
intended, so it satisfies `RuleApplies` completely — a hand-typed sweep would
otherwise be quoted as though rule 1 had selected from it. What disqualifies it is
not its shape but where it came from: choosing the rungs and then letting the rule
pick among them is the same act as choosing the load point, at one remove. So
provenance travels out of `benchRates` beside the rates rather than being re-derived
at the bottom of the summary, where by then there is nothing left to read it off.
Each entry in the list is held to the bounds a lone `-rate` gets, and held to them at
flag time: a four-rung sweep whose third entry was a typo is ninety minutes spent
before anything says so.

In all three cases the summary says how far the run got, suppresses the claim, and
still prints every rung's own figures. Suppressing a claim is not suppressing a
measurement — rule 3's second and third repetitions are read off exactly that
path. No published milestone 5 figure moves: that ladder completed.

**Rule 2 — what counts as the same order of magnitude.**

`p99(weft, text, headline rung) ≤ 10 × p99(bleve, text, headline rung)`, both
measured on the same machine in the same session, on the same 171,332 documents
and the same 50 judged queries at k=10, through the same driver. A miss is
recorded as a miss, with the profile that explains it — which is what milestone 4
did to the graph scorer and what this repository does with a result it did not
want.

**Rule 3 — what a repetition is, and what the headline is the median of.**
**FALSIFIED 2026-08-21 by the campaign it governed, then REPAIRED 2026-08-22 by the
experiment §5.2 registered against it. The falsified form is left standing below as the
record of an attempt; the repaired form is at the end of this rule —
[D-012](DECISIONS.md), [D-013](DECISIONS.md).**

> The rule directs a median over three observations of one rung. Three observations
> at 25.67 q/s gave 37.9 ms, 1.539 s and 416 ms, and two of them shed enough to fall
> under the p99 sample floor. The flat one was the fourth rung of a ladder; the two
> collapses were single rungs out of a warm-up. **A rung measured alone is not the
> same rung.** So there is no median, and the arithmetic below — still correct about
> why three sweeps cannot be compared — was answering the wrong question. What a
> repetition must hold constant was an open question against milestone 8, and
> [FINDINGS milestone 8 §2](FINDINGS.md) answers it: the ladder prefix, at depth. Read
> the rest of this paragraph and the table under it as the record of an attempt rather
> than as a procedure to run. The procedure is *rule 3, repaired*, below.

§5 has said "the median of three repetitions with the spread reported beside it"
since milestone 5, and milestone 5 published one run
([FINDINGS](FINDINGS.md) milestone 5 §4.5). The rule was never wrong; it was never
made operable. This is the operable form, and the choice behind it — with what
would show it was the wrong one — is [D-011](DECISIONS.md).

A repetition is **not another sweep of the ladder.** Every rung's rate is derived
from `benchUnloaded` — 200 sequential requests, taken fresh each run — so three
sweeps produce three different sets of five rates, and "the same rung" in two of
them is two different loads. There is nothing to take a median of.

So:

| | command | what it produces |
| --- | --- | --- |
| repetition 1 | `-rate 0` | the full ladder. Rule 1 selects the headline rate **R** |
| repetition 2 | `-rate R` | one rung at R, same `n` |
| repetition 3 | `-rate R` | one rung at R, same `n` |

The published p99 is the **median of the three**, with the spread reported as the
minimum and maximum beside it. Repetitions 2 and 3 print no headline label, and
that is rule 1's guard working rather than a defect: R was selected by the rule in
repetition 1 and is being *reused*, not selected again.

Two things are recorded alongside, because they are the only evidence about what
the spread is made of:

- **Each repetition's own unloaded p50.** If the three drift, the machine drifted,
  and part of the spread is that rather than the engine.
- **That R is quoted to two decimals.** The summary prints `%.2f`, and repetitions
  2 and 3 are run at the rounded figure — about 0.04% off repetition 1's actual
  rung. Stated rather than discovered later.

**Rule 3, repaired — a repetition is the ladder, with its rates named.**

The arithmetic above is right that rates cannot be *derived* twice. What does not follow
is that the ladder cannot be the unit: name the rates and the difficulty is gone. `-rates`
is that instrument, and the reproduction that licensed this is
[FINDINGS milestone 8 §2](FINDINGS.md) — the same ladder rung for rung, its headline rung
0.07% from the observation milestone 7 could not repeat.

| | command | what it produces |
| --- | --- | --- |
| repetition 1 | `-rate 0` | the full ladder. Rule 1 selects the headline rate **R** |
| repetition 2 | `-rates <repetition 1's rungs, through R>` | the same ladder and the same prefix, rates named rather than derived |
| repetition 3 | `-rates <repetition 1's rungs, through R>` | again |

Everything above still holds unchanged: the median of the three at R with the spread
beside it, each repetition's own unloaded p50 recorded, R quoted to two decimals, and no
headline label on repetitions 2 and 3 — R was selected once, by the sweep, and is being
reused.

What it costs: a repetition is now the whole ladder up to R, roughly **97 minutes** for
the `text` arm rather than 6.5, so three of them is 4.9 hours. `text+vector` gets one
named ladder rather than three, which is rule 6's first cut applied to a budget that
grew. [D-013](DECISIONS.md) carries the argument and what would show it wrong.

**Rule 4 — how the `text+vector` arm gets a p99.**

[§2.3](#23-quantiles-nearest-rank-and-absent-when-thin) refuses a p99 below 10,000
samples, and that rule is **not relaxed here.** The arm is four times slower per
query, which is why milestone 5 published no tail for it — the arm a user would
actually deploy. Lowering the bar to print something would trade the one property
that makes a published tail worth reading.

The sample depth is staged instead:

1. **A thin ladder**, `-arm text+vector -rotations 40` — 2,000 samples per rung.
   A p50 needs 200, so rule 1 has everything it needs to select a load point; p95
   prints, p99 does not, and its absence there is correct.
2. **A deep rung** at the rate that ladder selected, `-rotations 200` — 10,000
   samples. This is where the arm's p99 comes from.

Selection and measurement therefore run at different sample depths. Registered
here so that it is a design rather than a later excuse — and if the thin ladder
picks a different rung than a deep one would, that is a finding about how sample
depth moves rule 1, and it gets published as one.

**Rule 5 — what happens if the re-measurement disagrees with milestone 5.**

1. **Milestone 7's median becomes the published figure.** Milestone 5's 108.193 ms
   moves to a footnote and is labelled what it is: a single observation.
2. **Rule 2 is re-judged on the new medians**, weft's against bleve's.
3. **If the worst of the three observations exceeds 10× bleve's median**, the
   verdict is published as *"clears the bar at the median, and the spread reaches
   it"*. Pass or fail is not decided by the median alone when the spread is the
   thing this milestone exists to measure.
4. **If the three observations spread by more than 20%, that is the result.** No
   fourth run is added. Measuring until the answer settles is the failure this
   whole section is built to prevent.

**Rule 6 — the budget, and what gets cut first.**

The campaign is roughly 14.6 hours of machine time, derived from milestone 5's
measured rates:

| | estimate |
| --- | --- |
| `text` ladder (repetition 1) | 1.5 h |
| `text` headline rung × 2 | 1.6 h |
| bleve ladder + headline × 2 | 0.5 h |
| `text+vector` thin ladder | 1.2 h |
| `text+vector` deep rung × 3 | 9.8 h |

If it overruns, the order of cuts is fixed **now**:

1. `text+vector`'s deep rung drops from three repetitions to **one**. That arm's
   goal is that a p99 exists at all; the spread comes from the `text` arm.
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

The analyzer row is the one to watch: stop-word removal makes bleve's common-term
postings much shorter, which is a real advantage on queries like "what is the
origin of COVID-19", and weft has no stop-word list. It is a difference in what
the two engines do, not a flaw in the measurement, and it is not plausibly worth
an order of magnitude — which is the only thing rule 2 asks.

## 5. Reproducing

```bash
# One-time, from the repository root. Hours; see EVAL.md section 3.
make eval-data

# weft. Roughly 90 minutes for the text ladder on the machine below.
make bench                                   # text arm, full ladder
make bench BENCHFLAGS='-arm text+vector'     # the deployable arm, fewer samples

# bleve. Build once (about 20s), then the same ladder.
cd bench && go run . -build
make bench-compare
```

Run them **one at a time**. Both saturate the machine at their upper rungs, and a
run sharing a host with anything else is measuring the neighbour. The first
attempt at these numbers overlapped a bleve index build with a weft ladder and was
discarded for exactly that reason.

A rung prints a progress line every 30 seconds — `... 3000/10000 done, 15m0s
elapsed`. It is written from a goroutine that serves no request, for the reason
[loadgen.Progress](../internal/loadgen/progress.go) documents: a line printed from
inside the request lands in the latency that request records, and ten such samples
reach a p99 taken over ten thousand.

### 5.1 The campaign, in order

Rules 3 and 4 in commands. Roughly 14.6 hours, so it is written down rather than
remembered.

**Rule 3 was falsified by the campaign this sequence describes** — see the marking on
[rule 3](#3-judgment-rules--fixed-before-the-numbers-exist) and
[D-012](DECISIONS.md). The three commands below still run, and repetitions 2 and 3
still produce figures; what they do not produce is three observations of one rung.
Read this as the record of an attempt. **What replaces it is the campaign below** —
`-rate 0` once, then the same ladder named twice, from *rule 3, repaired*.

```bash
# ---- text arm, repaired: one sweep, then the same ladder named twice
make bench                                                   # repetition 1. Read R and its rungs off the report.
make bench BENCHFLAGS='-rates <rungs through R>'             # repetition 2
make bench BENCHFLAGS='-rates <rungs through R>'             # repetition 3
```

Roughly 4.9 hours for the `text` arm alone. bleve gets the same treatment; `text+vector`
gets one named ladder, under rule 6's first cut.

```bash
# ---- text arm: one ladder, then the same rung twice more (rule 3)
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

Record from each run: the rate, `n`, `inflight`, `GOMAXPROCS`, the unloaded p50,
and whether `shed` was zero. Repetitions whose unloaded p50 has drifted from the
others are **discarded and re-run, and the discard is published** — a run thrown
away silently is indistinguishable from one that was never made.

Logs go outside the repository. What gets committed is the figures in
[FINDINGS](FINDINGS.md), not the transcripts.

The reason any of this is a procedure rather than a habit: a single run on a
shared machine is a number nobody can reproduce, which is the lesson
[FINDINGS](FINDINGS.md) milestone 4 §4.2 paid for once already, and milestone 5
§4.5 paid for again.

### 5.2 What a repetition must hold constant — registered before it is measured

Milestone 7 measured 25.67 q/s three times and got 37.9 ms, 1.539 s and 416 ms
([FINDINGS milestone 7 §1](FINDINGS.md)). The one structural difference that survived
every check: the flat observation was the **fourth rung of a ladder**, ninety-one
minutes into the process; the two collapses were that rate **alone**, out of a
200-request warm-up. Two readings remain and neither has been varied deliberately —
a GC pacer that arrived at the load with a heap goal already grown to meet it, or
`inflight` 40 admitting a burst at rung start that a process climbing from a lower
rung never sees (§3 there).

`-rate 0` cannot separate them. The sweep derives its five rates from whatever that
run's sequential throughput happens to be, so a with-prefix run and a without-prefix
run land on different rates and prefix is confounded with rate. `-rates` names them,
and a named ladder gets no headline — which is correct here, because nothing in this
experiment is a load point the rule selected.

Four runs, two variables, all at `-rotations 200` so the rung under test carries the
10,000 samples [§2.3](#23-quantiles-nearest-rank-and-absent-when-thin) requires:

```bash
# The prefix is reproduced, not shortened: 91 minutes of climbing is the variable.
caffeinate -dimsu make bench BENCHFLAGS='-rates 3.21,6.42,12.84,25.67'
caffeinate -dimsu make bench BENCHFLAGS='-rates 3.21,6.42,12.84,25.67 -inflight 10'

# The same rate alone, which is what repetitions 2 and 3 were.
caffeinate -dimsu make bench BENCHFLAGS='-rates 25.67'
caffeinate -dimsu make bench BENCHFLAGS='-rates 25.67 -inflight 10'
```

Both arms go through `-rates` rather than one through `-rate`, so provenance is
identical across all four and the only things that differ are the two variables.
`inflight` 40 is this host's default — 4 per core at `GOMAXPROCS` 10 — and 10 is one
per core, the other principled point on the same rule rather than a tuned value.

**Compared, at the 25.67 q/s rung only:** p50, shed count, peak RSS, GC cycles. Those
are the four figures whose milestone 7 values are already on the page. No headline is
quoted from any of these runs.

| | machine time |
| --- | --- |
| with prefix, `inflight` 40 | 1.6 h |
| with prefix, `inflight` 10 | 1.6 h |
| no prefix, `inflight` 40 | 7 min |
| no prefix, `inflight` 10 | 7 min |

Roughly 3.5 hours, and almost all of it is the prefix. That asymmetry is the finding's
price: the cheap arm is the one already measured three times.

**What each outcome licenses, fixed now:**

1. **The collapse follows the prefix at both `inflight` values** — a repetition must
   hold the ladder prefix constant. The cost is that a repetition becomes a ladder
   again, which [D-011](DECISIONS.md) showed cannot be compared across sweeps whose
   rates are derived — so the rungs must be *named*, which is what `-rates` is for.
2. **The collapse follows `inflight` at both prefixes** — the burst reading. `inflight`
   becomes a constant the procedure states rather than a default that happens to be
   4 per core on this host.
3. **Both move it** — both are pinned, and every published figure quotes both.
4. **Neither reproduces the collapse** — the lone rung at 25.67 q/s runs flat this
   time — then the variable is something not yet named, the published run count stays
   at **one**, and no further arm is added looking for a shape that reproduces. Rule 5
   clause 4's reason applies unchanged: measuring until the answer settles is the
   failure this section exists to prevent.

**This is an experiment, not a replacement for rule 3.** [D-012](DECISIONS.md) decided
that the repair may not be chosen inside the milestone whose numbers falsified the
rule, because three candidate repairs were already visible and the reason to prefer
one was which run it would have made look reproducible. Registering the experiment and
what each result licenses — before the result exists — is what makes the repair
choosable later without that objection.

**Outcome, 2026-08-22: clause 1 fired.** The verdict and every figure are
[FINDINGS milestone 8](FINDINGS.md); the repair it licensed is *rule 3, repaired*, above,
and [D-013](DECISIONS.md). What ran differed from what is registered above in three
ways, recorded here because a procedure that gets edited to match what happened is not a
procedure:

1. **Reordered** — the two no-prefix arms ran first. Outcome 4 would have made the
   expensive arms pointless and they cost seven minutes each to rule out.
2. **A cut probe inserted** — the with-prefix arm was first run at `-rotations 40`
   (~20 min) on the argument that p50, shed, RSS and GC cycles all print at 2,000
   samples. It did not reproduce, which left the depth confound the cut created, so the
   deep arm ran anyway. The 20 minutes bought a finding rather than the answer they were
   spent on ([FINDINGS milestone 8 §3](FINDINGS.md)).
3. **One arm dropped** — deep prefix at `inflight` 10, 1.6 h. Runs A and B had already
   shown `inflight` does not gate the collapse. So clause 1's registered wording, "at
   both `inflight` values", is **half-tested**, and it is published that way.

### Machine

<!-- Filled in with the published numbers. A latency table without the machine it
     was measured on is not reproducible. -->

| | |
| --- | --- |
| host | Apple M4, 16 GiB |
| OS | macOS 26.5.2 |
| Go | 1.26.1 darwin/arm64 |
| GOMAXPROCS | 10 |
| GOGC | 100 (default) |
| corpus | 171,332 documents, 50 judged TREC-COVID queries, k=10 |
| repetitions | **1 for every figure below, and since 2026-08-22 a second is definable.** Milestone 7 found the headline load point irreproducible as a lone rung — 37.9 ms, 1.539 s and 416 ms at one rate, [FINDINGS milestone 7 §1](FINDINGS.md) — and milestone 8 found what a repetition has to hold: reached as the fourth rung of a ladder run to depth, the same rate came back **0.07% from its first observation** ([FINDINGS milestone 8 §2](FINDINGS.md)), which is *rule 3, repaired*, above. **No figure below has been re-measured under it.** Each remains a single observation, known to be one draw from a rule that flips on a few milliseconds of rung-1 median (milestone 7 §4.2) and taken on a machine whose sleep state was not recorded (§4.1 there) |
| weft ladder | one process, five rungs, `-rotations 200` |

The numbers measured on it are in [FINDINGS.md](FINDINGS.md) milestone 5 §1.

## 6. The prediction being graded

The plan's section 1 made a falsifiable claim before the instrument existed, and
it disagreed with the PRD's own risk table. The PRD says:

> Go GC로 p99 예측 가능성이 낮음 — Medium / Medium

The plan predicted the opposite: that weft's tail is a **working-set** problem
rather than a GC problem, because [milestone 3a](FINDINGS.md) pinned the live heap
at 74,504 bytes while a query touches 210 MiB of distinct pages.

Its arithmetic was wrong in a way the first measurement caught, and both halves
are recorded:

| | plan §1 predicted | measured (task 1, `.eval-data`, 171,332 docs) |
| --- | --- | --- |
| allocation per query, `text` | — | **43.6 MiB**, 75,132 objects |
| allocation per query, `text+vector` | ≤ 124 MiB | **181.9 MiB**, 782,955 objects |
| heap goal | 4 MiB (the floor, since live is 74 KB) | **46–53 MiB** |
| GC cycles per query, `text+vector` | ≈ 31 | **9.4** |

The prediction assumed the live heap stays at its idle 74 KB, so the GOGC target
would sit on its 4 MiB floor forever. It does not: a query holds 30,549 candidates
and their decoded records alive at once, so live heap during a query is tens of
megabytes and the target rises with it. Cycles came out at a third of the
prediction and each one has far more to mark than predicted — the two errors point
in opposite directions.

**The headline conclusion survives the broken path to it**, and that distinction
is the finding rather than a footnote. Whether it survives at the tail, under
load, is what the ladder answers, and the answer is in
[FINDINGS.md](FINDINGS.md) milestone 5.
