# Evaluation — how milestone 4's numbers are produced

**Status: in progress.** The instrument is built and checked against reference
implementations. No arm numbers exist yet — the corpus join (plan Task 1) has not
run. Sections 4 and 5 are the shape the results will take, not results.

This is the provenance document for every number milestone 4 publishes. The
milestone's deliverable is a figure, and a figure without a reproducible origin
has not been published. If a reader disagrees with a number, this file plus
`internal/eval/testdata/` should be enough to re-derive or refute it.

Verdict and interpretation live in [FINDINGS.md](FINDINGS.md); the dataset choice
and its independence argument live in [DATASETS.md](DATASETS.md).

---

## 1. What is being measured, and what would falsify it

The PRD's second falsification condition: *if the graph signal does not improve
nDCG@10, the signal is worthless — keep the interface, discard the graph.*

Three arms, per [DATASETS.md](DATASETS.md) section 3 requirement 2:

| Arm | Scorers | Question it answers |
|---|---|---|
| 1 | `text + vector` | the baseline everything is measured against |
| 2 | `+ graph.New` | does graph proximity contribute? |
| 3 | `+ graph.NewIncludingSeeds` | how much of arm 2 would have been double counting? |

Arm 3 is not optional. `graph.New` deliberately withholds seeds from its results
([FINDINGS](FINDINGS.md) section 2.3) because returning them would hand the seed
scorer a second RRF vote. Without arm 3 there is no way to say whether an
improvement came from the traversal or from text getting double weight.

## 2. The harness

`internal/eval`, standard library only. Under `internal/` on purpose: an
evaluation harness is not part of weft's library contract, and keeping it out of
`pkg/` leaves `engine`'s exported API — and the golden file guarding it —
untouched by the measurement.

| File | Contents |
|---|---|
| `ndcg.go` | `NDCG(ranked, qrels, k)` |
| `eval.go` | `Arm`, `Query`, `Run`, `Evaluate` |
| `bootstrap.go` | `BootstrapCI` — paired percentile bootstrap |

**The harness does not know what a graph scorer is.** An `Arm` is a name, a
`[]engine.Scorer`, an `engine.Fuser` and an over-fetch depth; `Evaluate` branches
on none of them. That is the milestone 1 claim restated one level up — a harness
that needed `if arm == "graph"` would mean the claim was true of the engine and
false of everything built on it. `TestEvaluateIsBlindToScorerCountAndKind` runs
one, two, three and four scorers, and a reordered list, through one call shape.

### 2.1 Two things the design bought without touching `pkg/`

**Over-fetch needed no new engine API.** `pkg/engine/search.go` line 112 carries a
`ponytail:` marker naming milestone 4 as the moment over-fetching would earn a
parameter on `Search`. It does not need one. `fusion.Fuse` scores a document from
its ranks alone and passes `k` only to `TopK`, so

    Fuse(streams, k*m)[:k]  ==  Fuse(streams, k)

and over-fetching is `Search(ctx, q, k*m, ...)` truncated to `k`.
`TestOverfetchIsTruncationNotADifferentRanking` asserts the equality across
k ∈ [1,5] and m ∈ {2,3,10}. The marker can be closed rather than repaid. This is
recorded as a property, not a coincidence: a fuser that normalised by `k` would
break it, so it is stated as a precondition on `Arm.Fuse`.

**Sweeping the RRF rank constant needs no edit to `pkg/fusion`.** `Fuse` is
injected as an `engine.Fuser`, so a sweep defines its own variant locally. This is
the first place milestone 1's dependency direction pays a measurable dividend.

### 2.2 One check the engine cannot make, and the harness can

`engine.Search` documents an unchecked precondition: every scorer must read the
same index, because `DocID` is index-relative ([FINDINGS](FINDINGS.md) section
3.4). Checking it there would require asking a scorer which index it reads — the
one method that would break every `Scorer` implementation. The harness has to
resolve every fused `DocID` to a key anyway, so it gets the check for free and
returns `ErrForeignDocID`. Missed, that failure mode produces an nDCG computed
over keys belonging to other documents; caught, it is one error.

## 3. Reference comparisons — done before any arm number

Both goldens and their generators are committed under `internal/eval/testdata/`.
The generators are one-off, never invoked by Go, and need a throwaway virtualenv:

```bash
python3 -m venv /tmp/weft-ref-venv
/tmp/weft-ref-venv/bin/pip install pytrec_eval-terrier rank_bm25
cd internal/eval/testdata
/tmp/weft-ref-venv/bin/python gen_ndcg_reference.py > ndcg_reference.json
/tmp/weft-ref-venv/bin/python gen_bm25_reference.py > bm25_reference.json
```

weft's own dependency count is unchanged: `make deps` still prints one module.

### 3.1 nDCG — the gain function was wrong before it was checked

**Result: matches `pytrec_eval`'s `ndcg_cut_10` on 12 discriminating fixtures.**

This check changed the implementation. The plan specified the exponential gain
`(2^rel − 1)/log2(i+1)` on the stated grounds that it matched what BEIR reports.
It does not. `trec_eval`'s `ndcg_cut` uses **linear gain** `rel/log2(i+1)`, and
BEIR reports `trec_eval`'s numbers through `pytrec_eval`.

The fixture that separates them — `swapped_grades`, qrels `{a:2, b:1}` ranked
`[b, a]`:

| Gain | nDCG@10 |
|---|---|
| linear `rel` | **0.8597186998521972** ← `pytrec_eval` |
| exponential `2^rel − 1` | 0.7967 |

Every ranking that is already ideal scores 1.0 under both, which is why this
needed a fixture built to discriminate rather than a happy path.

Five more behaviours the reference settled rather than us guessing:

| Question | Answer | Fixture |
|---|---|---|
| Does an unjudged document consume a rank slot? | Yes — contributes 0, pushes everything below it down | `unjudged_at_top` |
| Is a judged grade of 0 different from unjudged? | No, identical | `judged_zero_at_top` |
| Is IDCG truncated at k? | Yes | `ideal_beyond_k` |
| What is nDCG when IDCG is 0? | 0.0, and the query is still evaluated | `no_relevant` |
| Does a `-1` grade subtract? | No, clamped to 0 | `negative_grade` |

**The first row is a bias against the graph arm, and it is structural.** A
document the graph traversal surfaces that assessors never judged actively costs
nDCG — it takes a rank slot and pays no gain. This is precisely what TREC-COVID's
judgment depth (493.5 per query, the deepest in BEIR) was chosen to mitigate, and
it does not eliminate it. Any "no improvement" verdict has to be read with this in
the frame.

`TestNDCGArithmeticIsIndependentOfTheGoldenFile` holds three hand-derived literals
with their arithmetic written out, so a fixture edit that regenerates the golden
cannot move both sides together.

### 3.2 BM25 — matches to one ULP

**Result: max absolute score error 4.44e-16 across 7 queries × 8 documents,
tolerance 1e-12.** This is the PRD Success Metrics row "correctness floor" — the
one row nothing had claimed until now.

Reference is `rank_bm25` 0.2.2 `BM25Okapi` at `k1=1.2, b=0.75`, **with weft's IDF
substituted into its table.** That substitution is an alignment, stated rather
than hidden, and it is necessary:

| | IDF |
|---|---|
| `BM25Okapi` | `ln(N − n + 0.5) − ln(n + 0.5)` — negative for `n > N/2`, then floored by an epsilon heuristic |
| weft / Lucene | `ln(1 + (N − n + 0.5)/(n + 0.5))` — always positive |

So the comparison verifies term-frequency saturation, length normalisation,
`avgdl`, and the sum over query-term *occurrences* — where a normalisation bug
would actually live — and not the IDF expression itself.

The IDF expression is checked separately and without circularity.
`TestBM25UsesTheNonNegativeIDFForm` recovers the IDF from a single-term score by
dividing out the saturation term, then compares it against both candidate forms.
The fixture puts `beta` in 5 of 8 documents, which is exactly where the classic
form goes negative (−0.4520) and Lucene's does not (+0.4925). The test fails if the
fixture ever stops exercising that difference.

Corpus design, so the numbers are not accidentally easy: one single-token
document, one three times longer than average, one document repeating a term three
times, a query with a repeated term, a query term in no document, and a document
sharing no term with any query (it must be **absent** from the candidates, not
present at score 0 — fusion consumes ranks, and a zero-scored document occupying a
rank is a vote it did not earn).

### 3.3 Statistical treatment

Paired percentile bootstrap: resample query ids with replacement, take the mean
paired difference, report the 2.5th and 97.5th order statistics over 10,000
resamples. `math/rand/v2` PCG, seeded explicitly.

Paired, not two-sample: per-query nDCG varies far more across queries than between
arms, so an unpaired interval over 50 queries is wide enough to hide any effect
this milestone could plausibly find. Runs covering different query sets are
rejected (`ErrUnpaired`) rather than silently intersected.

Nearest-rank percentiles, no interpolation, no BCa. The judgment rule reads only
whether the interval crosses zero, and interpolation moves the fourth decimal of
an interval whose width is set by having 50 queries.

Reproducibility is a tested property, not an intention: ids are sorted before
resampling because Go randomises map iteration, and `TestBootstrapIsReproducible`
runs the same input ten times and requires an identical interval.

## 4. Judgment rule — fixed before the numbers exist

Written down now so it cannot be chosen after seeing the result.

The verdict on the PRD's second falsification condition is **yes** only if both
hold:

1. **Frozen values.** At `RRFk=60`, `K1=1.2`, `B=0.75`, `SeedN=5`, `MaxDepth=3`,
   over-fetch=1, the paired 95% interval for arm 2 − arm 1 **excludes zero and is
   positive.**
2. **Sign stability.** Across the phase-2 sweep, the sign of arm 2 − arm 1 **does
   not flip.**

If 1 holds and 2 fails, the verdict is **undetermined** — not an improvement. If 1
fails, the verdict is **no**, and `pkg/scorer/graph` is deleted; the `Scorer`
interface, `Query.Seeds` and `recency` stay.

Freeze first, sweep second, per [D-002](DECISIONS.md). The headline is one frozen
configuration; the sweep is a separate artifact measuring how much the verdict
depends on a constant nobody tuned.

### 4.1 An amendment that was made on incomplete data, and withdrawn

**Withdrawn. The pre-registered baseline stands: `text + vector`.** Kept here rather
than deleted, because the mistake is more instructive than the correction.

At 27% vector coverage, `text + vector` measured 0.3200 against `text` alone at
0.5826 — the vector scorer apparently destroying 45% of the baseline. On that basis
the binding comparison was moved to `text+graph` minus `text`, arguing that a signal
should have to beat the strongest system available rather than a handicapped one. The
reasoning was sound and the direction was conservative: it raised the bar the graph
had to clear.

**The number it rested on was an artifact.** At 86.5% coverage `text + vector` scores
**0.6233**, which is *better* than `text` at 0.5826 (+0.0407, 95% CI [+0.0010,
+0.0799]). The vector scorer helps, modestly. It had appeared to hurt because with
8,647 of 171,332 documents carrying a vector, its top-10 was drawn from an arbitrary
5% slice of the corpus — so it was injecting near-random documents into fusion. That
is a property of an unfinished download, not of rank fusion.

So `text + vector` is the strongest non-graph system after all, the amendment is
moot, and the binding comparison reverts to the one section 4 registered before any
data existed.

**What it cost, and the lesson it carries:** the wrong finding came with a 95%
interval of [−0.3058, −0.2178]. Nowhere near zero, narrow, and completely wrong. The
bootstrap was working correctly — it quantifies sampling noise across queries, which
is all it claims to do. It has nothing to say about whether the *corpus* is complete,
and no amount of statistical care substitutes for checking coverage before reading a
delta. Section 3 built two reference comparisons to stop us trusting a metric we had
not verified; this was the same class of error one level out, and it landed anyway.
Every arm number in this document is now reported with the coverage it was measured
at.

## 5. The data

### 5.1 Snapshot and provenance

| Source | Snapshot | Size | Auth |
|---|---|---|---|
| BEIR trec-covid | `public.ukp.informatik.tu-darmstadt.de/thakur/BEIR/datasets/trec-covid.zip`, files dated 2021-02-11 | 70 MB zip; 171,332 documents, 50 queries, 66,336 judgments | none |
| CORD-19 metadata | `ai2-semanticscholar-cord-19.s3-us-west-2.amazonaws.com/2022-06-02/metadata.csv` | 1.65 GB | none |
| Citation graph | Semantic Scholar `/graph/v1/paper/batch`, `references.externalIds`, fetched 2026-08-14 | — | **none needed** |
| Vectors | the same endpoint, `embedding.specter_v2`, 768 dimensions | — | **none needed** |

Two things here differ from what the plan assumed, both in the cheaper direction.

**No API key is required.** The plan listed a Semantic Scholar key as a blocker.
The unauthenticated endpoint serves the same fields under a lower shared rate
limit; a key only makes it faster. `-api-key` and `S2_API_KEY` are honoured when
one is available.

**The embeddings did not need a local model.** The plan budgeted an offline
sentence-transformers run. The same batch request that returns citations returns
SPECTER2 vectors, so the vectors are externally injected exactly as the PRD
requires ("embedding generation and model inference" out of scope) with no new
dependency and no second pass. `make deps` still prints one module.

### 5.2 The join

`s2_id` is column 19 of the 2022-06-02 release and holds the Semantic Scholar
CorpusId directly, so the join needs no DOI resolution step. The plan required
checking this rather than assuming it, because the columns differ between releases;
`ReadCORD19IDs` fails with `ErrNoJoinColumn` on a release that has none of them.

| Join key | Documents |
|---|---|
| `s2_id` → `CorpusId:` | 139,419 |
| `doi` → `DOI:` | 25,543 |
| `pmcid` → `PMCID:` | 230 |
| **resolved** | **165,192 of 171,332 (96.4%)** |
| no usable identifier | 6,140 — indexed, but with no edges and no vector |

### 5.3 The index

171,332 documents, average length 169.4 tokens, vectors 768-dimensional. Committed
in 786 ms and reopened in 479 ms with matching document count and average length —
**the first time milestone 2's restore equivalence has run on a real corpus**
rather than on fixtures.

### 5.4 Query vectors, and why they had to be checked twice

Document vectors are free from the batch API. Query vectors are not: SPECTER embeds
papers, and no API embeds an arbitrary question. Without them the vector scorer has
no opinion at all, so the baseline would silently be text alone.

`internal/eval/testdata/gen_query_vectors.py` encodes the 50 queries locally. Nothing
enters the Go module and the engine still receives float32 vectors it did not
compute, so this is the same category as the document vectors — externally produced,
injected — and `make deps` still prints one module.

Which model it is could not be assumed, because the two sides must share an
embedding space or cosine between them is meaningless. Measured against Semantic
Scholar's own vectors for documents we already had:

| Local configuration | cosine vs S2's `specter_v2` (min / median) |
|---|---|
| `specter2_base`, no adapter | 0.9279 / 0.9594 |
| **`specter2_base` + `allenai/specter2` (proximity)** | **0.9989 / 1.000000** |
| `specter2_base` + `specter2_adhoc_query` | 0.8423 / 0.8815 |

So S2 serves the proximity adapter. Queries use SPECTER2's separate ad-hoc query
adapter, which AllenAI documents as the matched pair for that — and a documented
pairing is still an assumption, so it gets its own check: for each query, mean cosine
to its judged-relevant documents against mean cosine to random ones.

    mean cosine to judged-relevant  0.7620
    mean cosine to random           0.6842
    relevant wins                   50/50

The query vectors are in a usable space. Both checks are built into the script and
rerun with it.

### 5.5 The text baseline, against an external number

**`text` alone scores nDCG@10 = 0.5826** over the 50 queries.

BEIR publishes BM25 on trec-covid at about 0.656 (Anserini, `k1=0.9`, `b=0.4`, with
stemming and a stopword list). weft is at 0.5826 with `k1=1.2`, `b=0.75`, no stemmer,
no stopwords and whitespace tokenization — the differences all push the same way, and
landing within eight points of a mature implementation is the outcome that says the
text path is sound rather than broken. A sanity check, not a like-for-like
comparison; section 3.2 is the like-for-like one.

### 5.6 Coverage at which the numbers below were measured

| Quantity | Count | Share of corpus |
|---|---|---|
| Documents indexed | 171,332 | 100% |
| Documents with a vector | 148,232 | 86.5% |
| Documents with at least one in-corpus citation edge | 48,194 | 28.1% |
| In-corpus edges | 579,720 | — |
| References pointing outside the corpus (dangling) | 1,692,610 | 74.5% of all references |

The fetch itself covered 165,192 documents in 1 h 34 m, found 147,267 vectors and
2,253,428 raw references, and failed to find 2,321 papers.

**Two ceilings to read every graph number against.** Only 28.1% of documents have any
usable out-edge, and three quarters of all references point outside the corpus. The
graph the scorer traverses is a sparse fragment of the real citation graph — that is
inherent to restricting a citation network to one topical corpus, not a defect in the
join, but it bounds what any graph result here can claim.

### 5.7 Graph degeneracy: the risk bit

Measured through the public `Scorer` interface by asking for a very large k and
bucketing candidates by score, which recovers each one's hop exactly.

| Hop | Candidates across 50 queries | Mean per query |
|---|---|---|
| 1 | 2,076 | 41.5 |
| 2 | 21,004 | 420 |
| 3 | 108,287 | 2,166 |

**38 of 50 queries have more than k=10 hop-1 candidates.** For those queries, every
candidate the graph stream returns scores exactly 0.5, so `engine.TopK`'s tiebreak
decides the order — and that tiebreak is `DocID`, which is corpus insertion order.

The graph stream is therefore handing fusion "the ten lowest-numbered documents cited
by the text scorer's top five", ranked by an accident of indexing. This is the failure
mode section 5 flagged in advance and Task 4 existed to catch, and it is why the
measurement below cannot yet be read as a verdict on graph proximity: a regression
here is consistent both with proximity being worthless and with us having failed to
turn proximity into a ranking.

### 5.8 Frozen five-arm measurement, before the degeneracy fix

`RRFk=60`, `K1=1.2`, `B=0.75`, `SeedN=5`, `MaxDepth=3`, over-fetch=1. 50 queries,
paired bootstrap, 10,000 resamples, seed 20260814.

| Arm | nDCG@10 |
|---|---|
| `text` | 0.5826 |
| `text+vector` | **0.6233** ← baseline |
| `text+graph` | 0.3565 |
| `text+vector+graph` | 0.4639 |
| `text+vector+graph-including-seeds` | 0.6087 |

| Comparison | Delta | 95% CI | Reading |
|---|---|---|---|
| `text+vector+graph` − `text+vector` **(binding)** | **−0.1594** | [−0.1979, −0.1210] | **regresses** |
| `text+graph` − `text` | −0.2261 | [−0.2723, −0.1788] | regresses |
| `text+vector+graph-including-seeds` − `text+vector` | −0.0146 | [−0.0327, +0.0042] | undetermined |
| `text+vector` − `text` | +0.0407 | [+0.0010, +0.0799] | improves |

Largest per-query moves for the binding pair, `text+vector+graph` against
`text+vector`: query 24 falls from 0.9149 to 0.4787, query 40 from a perfect 1.0000 to
0.6321, query 3 from 0.8568 to 0.4948. The graph stream is not diluting mediocre
rankings, it is dismantling good ones. (`make eval` reprints these; the figures are
larger still against the text-only baseline, where query 30 and query 40 both fall
from 1.0000 to 0.4451.)

**The double-counting arm is the diagnostic that makes this legible.** Keeping the
seeds scores 0.6087 while dropping them scores 0.4639 — a 0.14 gap in favour of the
variant whose top five are *the text scorer's own results*. The traversal's
contribution is worse than re-voting text. Combined with section 5.7, the picture is
consistent: the ten documents the traversal contributes are close to arbitrary, and
they displace good ones.

**No verdict is recorded from this table.** Section 4's rule needs a graph signal that
has been given a fair chance to rank, and section 5.7 shows this one was not. The plan
anticipated exactly this and required the fix be measured against the same query set,
before and after, so the two effects stay separable. That is section 5.9.

### 5.9 The degeneracy fix, and why it was not enough

The plan named the minimal fix in advance: replace nearest-seed distance with a sum
of per-seed distances, `Σ_seeds 1/(1+hops)`, so documents several seeds agree on
outrank documents only one seed reaches. Implemented in `pkg/scorer/graph`, with the
formula's own tests (`TestSeedsAgreeingRaisesScore`) and every pre-existing graph test
still green — single-seed scores are unchanged by construction, so the change is
strictly an extension.

**It did not fix the degeneracy.**

| Tie analysis at k=10 | Before | After |
|---|---|---|
| Queries whose stream's membership is settled by DocID | 38 of 50 | 41 of 45 answering |
| Slots decided by DocID across all queries | — | 954 |
| Distinct scores per query, typical | 3 | 3–16 |

The reason is the graph, not the formula. Only 28.1% of documents have an in-corpus
out-edge (section 5.6), so two seeds rarely cite the same paper — the sum almost
always has exactly one non-zero term, and 0.5 remains the modal score. Multiplying
granularity by `SeedN` only helps if the seeds agree, and on a citation graph this
sparse they do not.

It did move the numbers, in the right direction and not nearly far enough:

| Arm | Before | After | Change |
|---|---|---|---|
| `text+graph` | 0.3565 | 0.4017 | +0.0452 |
| `text+vector+graph` | 0.4639 | 0.5077 | +0.0438 |
| `text+vector+graph-including-seeds` | 0.6087 | 0.5485 | −0.0602 |

| Comparison (after fix) | Delta | 95% CI | Reading |
|---|---|---|---|
| `text+vector+graph` − `text+vector` **(binding)** | **−0.1156** | [−0.1483, −0.0837] | **regresses** |
| `text+graph` − `text` | −0.1809 | [−0.2236, −0.1386] | regresses |
| `text+vector+graph-including-seeds` − `text+vector` | −0.0748 | [−0.1026, −0.0468] | regresses |
| `text+vector` − `text` | +0.0407 | [+0.0010, +0.0799] | improves |

The `including-seeds` arm moved *down*, which is consistent rather than surprising: a
non-seed reached by two seeds now sums to 1.0 and can displace a seed, so that arm
admits more traversal output than it used to and inherits more of its cost. The
control is behaving like a control.

**Both measurements are kept.** The fix is attributable — it is worth about +0.044 —
and so is the conclusion that it does not change the outcome. Reporting only the
post-fix number would hide that a predicted remedy was tried and fell short by an
order of magnitude.

**Known risk to record before it is measured:** `graph.Candidates` scores
`1/(1+hops)` with `MaxDepth = 3`, so a non-seed candidate can only hold one of
three values — 0.5, 1/3, 0.25. In a citation graph, hop-1 neighbours will number in
the hundreds or thousands per query, and `engine.TopK` breaks ties on `DocID`,
which is insertion order. At k=10 the graph stream would then be "the ten
lowest-numbered hop-1 neighbours" — a measurement of index insertion order, not of
proximity. Task 4 measures the hop distribution before changing anything; if it
bites, the minimal fix is per-seed distances summed (`Σ_seeds 1/(1+hops)`), which
multiplies the score granularity by `SeedN` and costs `SeedN` traversals. Both
before and after must be measured so the contribution stays attributable.

### 5.10 Sweep: the sign does not flip, and it is negative everywhere

`RRFk` ∈ {1, 10, 20, 40, 60, 100, 200} × over-fetch ∈ {1, 2, 5, 10}, 28
configurations, `text+graph` against `text`, 2,000 resamples each.

| | Value |
|---|---|
| Configurations | 28 |
| Sign flips | **0** |
| Delta range | −0.1806 to −0.1870 |
| CI upper bound, worst case | **−0.1307** (never reaches zero) |
| Baseline, all configurations | 0.5826 (unaffected — the graph arm is what varies) |

Over-fetching to depth 100 does not rescue it, and neither does collapsing the rank
constant to 1 or raising it to 200. Section 4 rule 2 is satisfied in the sense that
matters least: the sign is perfectly stable, and it is stably negative.

Two things this rules out. The result is not an artifact of `RRFk = 60`, which
[DATASETS.md](DATASETS.md) section 3 requirement 4 warned about. And it is not an
artifact of shallow streams — the over-fetch column exists because deeper fusion is
known to help RRF, and here it changes the fourth decimal.

**One limitation, stated rather than buried.** This grid sweeps `text+graph` against
`text`, not the binding `text+vector+graph` against `text+vector`. It was written
while section 4.1's amendment was in force and the text-only pair was the binding one;
the amendment was withdrawn and the grid was not re-run. The sign is negative in both
framings at the frozen point — −0.1809 here, −0.1156 for the binding pair — so the
conclusion is unaffected, but section 4's rule 2 is satisfied by evidence about a
neighbouring comparison rather than the exact one it names. Re-running it against the
binding pair costs about an hour and would forfeit the shortcut that makes it
affordable: a single-stream baseline is provably invariant across this grid, so it is
measured once instead of 28 times, and `text+vector` is not single-stream.

### 5.11 Weighted fusion: the regression was fusion's, and the graph still contributes nothing

Section 5.10 establishes that no `RRFk` or over-fetch setting rescues the graph arm.
Both change how ranks are damped, not how much each stream counts. The remaining
suspect is the equal vote itself, so it was tested directly: a `Fuser` variant that
multiplies each stream's contribution by a weight, with text and vector held at 1.0
and only the graph stream moving.

**The weights attach to stream position, not scorer kind.** The caller already chose
the order when it passed the scorers to `Search`, so a fuser can discount a stream
without learning what produced it. The milestone 1 property is intact — `pkg/fusion`
is untouched, and nothing in the variant branches on a scorer.

| Graph stream weight | nDCG@10 | Delta vs `text+vector` | 95% CI | Reading |
|---|---|---|---|---|
| 1.0 (as measured everywhere above) | 0.5077 | **−0.1156** | [−0.1483, −0.0837] | regresses |
| 0.5 | 0.6240 | +0.0007 | [−0.0057, +0.0079] | undetermined |
| 0.25 | 0.6240 | +0.0007 | [−0.0057, +0.0079] | undetermined |
| **0.1** | **0.6250** | **+0.0018** | [+0.0000, +0.0053] | undetermined |
| 0.05 | 0.6246 | +0.0013 | [+0.0000, +0.0040] | undetermined |
| 0.02 and below | 0.6233 | +0.0000 | [0, 0] | converged to baseline |

The bottom rows are the implementation's own check: as the weight approaches zero the
graph stream can only break ties among documents the other two already rank equally,
so the arm must converge exactly onto the baseline. It does, to the last bit.

**Two conclusions, pointing in opposite directions.**

**The entire −0.1156 belonged to fusion, not to the graph.** Halving the weight erases
it. What section 5.8 measured was not a signal destroying a ranking; it was RRF
handing ten slots to a stream that had not earned them, exactly as
[FINDINGS](FINDINGS.md) milestone 4 section 5.2 suspected. Equal weighting is a
substantive ranking decision that unweighted RRF makes silently on every query, and
its cost here was 0.12 nDCG — an order of magnitude larger than anything the graph
signal itself was ever worth.

**And the graph still contributes nothing.** The best weight found is 0.1, worth
+0.0018 — a 0.29% relative gain whose interval touches zero. No weight produces a
positive delta with an interval excluding zero, which is what section 4's rule
requires. Down-weighting a near-noise stream stops it doing harm; it does not turn it
into information.

---

## 6. Verdict

**The PRD's second falsification condition is met. The answer is NO: graph proximity
as weft implements it does not improve nDCG@10.**

| Section 4 condition | Result |
|---|---|
| 1. Frozen: paired 95% CI for `+graph` − baseline excludes zero and is positive | **FAILS** — excludes zero and is *negative*: −0.1156, [−0.1483, −0.0837] |
| 2. Stable: the sign does not flip across the sweep | Holds — 0 flips in 28 configurations, negative throughout |
| Best case under any fusion weight (section 5.11) | +0.0018, CI [+0.0000, +0.0053] — touches zero, 0.29% relative |

Condition 1 fails, which is the "no" branch. Per the PRD and [D-004](DECISIONS.md) the
consequence is that `pkg/scorer/graph` goes while the `Scorer` interface,
`Query.Seeds` and `pkg/scorer/recency` stay.

**Section 5.11 changes why, without changing what.** The headline −0.1156 was fusion's
doing: give the graph stream half a vote and the entire regression disappears. The
honest statement of the result is therefore not "graph proximity destroys rankings"
but the flatter and less dramatic one — **at every weight from 1.0 down to 0, the best
the graph stream is worth is +0.0018 nDCG@10, and that interval touches zero.** It is
not harmful information. It is, as far as this measurement can tell, not information.

That reframing costs the verdict nothing and matters a great deal for what to do next,
because it moves the largest measured effect in this milestone out of the graph
scorer and into the fusion operator.

### 6.1 What exactly was falsified

Not "citation structure carries no ranking signal". What was measured and rejected is
one specific construction, and every part of it is a choice that could have been made
differently:

| Choice | What was measured |
|---|---|
| Proximity metric | BFS hop distance, `Σ_seeds 1/(1+hops)`, `MaxDepth=3` |
| Seed source | the text scorer's top 5 |
| Fusion | unweighted RRF, one equal vote per stream |
| Graph | citations restricted to one topical corpus — 28.1% of documents have any in-corpus out-edge |

The mechanism of the failure is legible and it is not subtle. The graph stream
returns thousands of candidates carrying 3 to 16 distinct scores, so it has almost no
internal ranking; RRF then gives its arbitrary top ten the same vote as BM25's top
ten, and they displace results that were correct. Queries 30 and 40 fall from a
perfect 1.0000 to about 0.51. The signal is not weak, it is close to noise, and
unweighted fusion has no way to discount it.

**A graph signal that produced a real ordering might well help.** Personalised
PageRank or a random-walk probability would give a continuous score instead of three
values ([FINDINGS](FINDINGS.md) section 5), and a fusion operator with per-stream
weights would let a weak signal contribute without displacing a strong one. Neither
was in scope here, and neither is refuted by this result. What is refuted is that hop
distance plus equal-weight RRF is enough — which is what weft actually shipped and
therefore what the PRD's hypothesis was resting on.

### 6.2 Reasons to doubt this number, in the same section as the number

- **The degeneracy was never fully removed.** 41 of 45 answering queries still have
  the stream's membership partly settled by DocID (section 5.9). Two formulas were
  tried; both regress. That is evidence, not proof, that the ceiling is the metric
  rather than the implementation.
- **50 queries.** The bootstrap quantifies that, and section 4.1 is a worked example
  of a tight interval on incomplete data being confidently wrong. The coverage behind
  every number here is in section 5.6.
- **28.1% edge coverage.** A denser graph might behave differently. Restricting a
  citation network to one topical corpus makes most references dangle, and that is
  inherent to the design DATASETS.md chose, not a defect in the join.
- **Unjudged documents count as irrelevant**, and they take rank slots (section 3.1).
  This is structurally unkind to a signal whose purpose is surfacing documents
  assessors did not see. TREC-COVID's judgment depth was chosen to mitigate it and
  does not eliminate it. The effect pushes toward this verdict.
- **Both graph variants regress, including the double-counting one.** If the harness
  were somehow rigged against the graph, `including-seeds` — which mostly re-votes
  text — should have been safe. It regresses too (−0.0748).

---

## 7. Reproducing every number above

```bash
make all                    # build + vet + test -race, includes internal/eval
make arch                   # milestone 1 assertions still green
make deps                   # external dependencies still zero
go test -v -run 'TestBM25|TestNDCG|TestBootstrap' ./internal/eval/

make eval                   # the frozen five-arm table and its intervals
make eval-full              # adds the tie analysis and the 28-configuration sweep
```

One-time data preparation, in order, assuming `.eval-data/` holds the two downloads
named in section 5.1:

```bash
go run ./cmd/weft-eval prepare          # ~1h35m, rate limited, resumable
/tmp/weft-ref-venv/bin/python internal/eval/testdata/gen_query_vectors.py --verify
/tmp/weft-ref-venv/bin/python internal/eval/testdata/gen_query_vectors.py
go run ./cmd/weft-eval build            # index + Commit + reopen equivalence check
```

Bootstrap seed 20260814 and the frozen constants are compiled in, so `make eval`
reprints the intervals in this document rather than approximations of them.
