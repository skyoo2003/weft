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
10,000 samples.

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

## 3. Judgment rule — fixed before the numbers exist

Both rules below were registered in `.claude/plans/weft-m5.plan.md` before the
instrument produced a publishable figure, the same discipline
[D-004](DECISIONS.md) applied in milestone 4.

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

**Rule 2 — what counts as the same order of magnitude.**

`p99(weft, text, headline rung) ≤ 10 × p99(bleve, text, headline rung)`, both
measured on the same machine in the same session, on the same 171,332 documents
and the same 50 judged queries at k=10, through the same driver. A miss is
recorded as a miss, with the profile that explains it — which is what milestone 4
did to the graph scorer and what this repository does with a result it did not
want.

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

The headline is the median of three repetitions with the spread reported beside
it. A single run on a shared machine is a number nobody can reproduce, which is
the lesson [FINDINGS](FINDINGS.md) milestone 4 §4.2 paid for once already.

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
| repetitions | **1 of the corrected instrument** — the median-of-three rule above is not yet satisfied. Two earlier ladders were discarded and a third was superseded by instrument fixes; see [FINDINGS](FINDINGS.md) milestone 5 §4.1 and §4.5 |
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
