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

### 2.2 Half a check the engine cannot make, for free

`engine.Search` documents an unchecked precondition: every scorer must read the
same index, because `DocID` is index-relative ([FINDINGS](FINDINGS.md) section
3.4). Checking it there would require asking a scorer which index it reads — the
one method that would break every `Scorer` implementation. The harness has to
resolve every fused `DocID` to a key anyway, so a bound check costs nothing, and
it returns `ErrForeignDocID`.

**It is a bound check, not the precondition.** IDs are dense from zero, so a scorer
built against a different index of similar size returns IDs that are *in* range,
resolve to unrelated documents here, and produce an nDCG over the wrong keys with no
error — the silent wrong answer, still unclosed. What the check catches is the subset
that falls outside the bound: a smaller foreign index, or a stale scorer against a
corpus that has shrunk. Worth having because it is free; not worth calling enforcement.

The qrels side of the same pairing problem *is* closed, by the same free lookup: every
judged document with a positive grade is resolved against the index before any arm runs
(`ErrForeignQrelDoc`). That one cannot surface as a wrong lookup at all — an absent
judged document never appears in a ranking, it just stays in the ideal one, raising the
denominator for every arm equally — so nothing but this check would have reported it.

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
+0.0798]). The vector scorer helps, modestly. It had appeared to hurt because with
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

**And the check is in the code, not only in the prose.** Reporting coverage relies on
someone reading the number; the failure above happened with the coverage sitting in
the log. `weft-eval build` now refuses to write an index at all unless the Semantic
Scholar cache holds a record for every corpus document — `prepare` writes a tombstone
even for the 8,495 documents it can find nothing for, so a finished preparation covers
the corpus exactly and a gap can only mean the job did not finish. Building a slice on
purpose is still available behind `-partial`, which prints that the arms it produces
must not be published. Two adjacent holes closed with it: `query-vectors.jsonl` is now
checked against `queries.jsonl` by *text* rather than paired on ids alone, since ids
are stable across regenerations of the query set and a file from an older snapshot
covers every query while embedding different questions; and an all-zero query vector
is rejected rather than counted as coverage, because the vector scorer reads a zero
norm as no opinion and `text+vector` would silently be `text`.

## 5. The data

### 5.1 Snapshot and provenance

| Source | Snapshot | Size | Auth |
|---|---|---|---|
| BEIR trec-covid | `public.ukp.informatik.tu-darmstadt.de/thakur/BEIR/datasets/trec-covid.zip`, files dated 2021-02-11 | 70 MB zip; 171,332 documents, 50 queries, 66,336 judgments | none |
| CORD-19 metadata | `ai2-semanticscholar-cord-19.s3-us-west-2.amazonaws.com/2022-06-02/metadata.csv` | 1.65 GB | none |
| Citation graph | Semantic Scholar `/graph/v1/paper/batch`, `references.externalIds`, fetched 2026-08-14 | — | **none needed** |
| Vectors | the same endpoint, `embedding.specter_v2`, 768 dimensions | — | **none needed** |

**The downloads are pinned, not merely named.** A release named in prose is not a
release checked, and both ways that matters end in a complete-looking run rather than an
error: a `metadata.csv` truncated by an interrupted 1.6 GB download joins whatever prefix
arrived and lets `prepare` record the documents it never mentioned as permanently
unjoinable, and a `qrels/test.tsv` truncated at a row boundary reads as a clean prefix
and lifts every arm's nDCG by an unreported amount. So the four downloaded files are
verified by size and content hash — size first, because that catches the real accident
from a `stat` instead of hashing gigabytes — before `prepare` writes anything and before
`run`, `sweep`, `weights` or `diagnose` prints anything.

| File | Bytes | sha256 |
|---|---|---|
| `trec-covid/corpus.jsonl` | 221,370,065 | `aded6989…04ed00d7` |
| `trec-covid/queries.jsonl` | 16,552 | `78f4b76b…783b208c` |
| `trec-covid/qrels/test.tsv` | 980,831 | `10669ab7…2798982b` |
| `metadata.csv` | 1,648,942,196 | `ec2e3c55…2bf31ae5` |

The full values are in `cmd/weft-eval/snapshot.go`, which is the authority; the prefixes
above are for recognising them. `s2.jsonl` and `query-vectors.jsonl` are deliberately not
pinned — they are generated from API responses and a local model, and what covers them is
`prepare`'s model tally, `build`'s coverage gate and the query-vector text pairing.
`-any-snapshot` skips the check for a deliberately different corpus, and says in its own
help text that numbers measured that way are not the ones this document publishes.

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

Those 6,140 are not documents the release lists without an identifier. Scanning all
1,056,660 rows for them: **every corpus document that appears in this release has an
identifier**, so the 6,140 are simply absent from it — the BEIR corpus is dated
2021-02-11 and the metadata release 2022-06-02. That matters beyond bookkeeping, because
it is why a truncated metadata file cannot be detected from the join alone: a document the
file never mentions is the normal case, not a signal, which is what §5.1's hash pin is
for.

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
    relevant wins                   50/50  (sign test p = 8.88e-16)

The query vectors are in a usable space. Both checks are built into the script and
rerun with it.

**The bar this clears was raised after review.** The check originally passed on a
bare majority of wins, and a bare majority is the median outcome of a coin flip — so
an adapter embedding into the wrong space would have produced roughly 26 of 50 and
been accepted by the check written to refuse it, with the aggregate cosines printed
above and never compared. It now requires both a one-sided sign test at p < 0.001
(exact, via `math.comb`, so the script stays on the standard library) and a positive
aggregate margin. The run above sits sixteen orders of magnitude inside the first and
0.0778 inside the second; 34 of 50 wins would fail.

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
| In-corpus edges | 579,719 | — |
| References pointing outside the corpus (dangling) | 1,692,610 | 74.5% of all references |

The fetch itself covered 165,192 documents in 1 h 34 m, found 147,267 vectors and
2,253,428 raw references, and failed to find 2,321 papers. The remaining 6,140
documents carry no identifier the CORD-19 metadata can join on, and `prepare` records
a key-only entry for each so that a rerun neither asks about them again nor rescans
the 1.6 GB metadata file to rediscover that it cannot. The cache therefore holds one
record per corpus document, 171,332 of them.

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

> **This table is the one figure here that HEAD cannot reprint** (section 5.12). It
> buckets candidates by score, which recovers the hop only while a score *is* one
> hop — true of the single-seed formula this section diagnosed, not of the summed
> formula that replaced it. `weft-eval diagnose` was rewritten with the formula and now
> reports ties at the cut instead, which is the question that actually matters and
> survives the change; on the same pre-fix scorer it reports **45 of the 45 answering
> queries** with a tie group crossing k=10 and 2,082 slots settled by `DocID`. That
> figure is reproducible and is the one section 5.9 compares against. The hop counts
> are kept because they are what motivated the fix.

The graph stream is therefore handing fusion "the ten lowest-numbered documents cited
by the text scorer's top five", ranked by an accident of indexing. This is the failure
mode section 5 flagged in advance and Task 4 existed to catch, and it is why the
measurement below cannot yet be read as a verdict on graph proximity: a regression
here is consistent both with proximity being worthless and with us having failed to
turn proximity into a ranking.

### 5.8 Frozen five-arm measurement, before the degeneracy fix

`RRFk=60`, `K1=1.2`, `B=0.75`, `SeedN=5`, `MaxDepth=3`, over-fetch=1. 50 queries,
paired bootstrap, 10,000 resamples, seed 20260814.

The scorer this table measures is the single-seed one, which HEAD no longer contains —
section 5.9 replaced it. Re-measuring it after the determinism fix (section 5.12) meant
restoring `pkg/scorer/graph/graph.go` from commit `ed80dc2` over the current tree,
running `weft-eval run` against the same rebuilt index, and restoring the file. That is
one command and a `git show`, not a code path kept alive for a historical number.

| Arm | nDCG@10 |
|---|---|
| `text` | 0.5826 |
| `text+vector` | **0.6233** ← baseline |
| `text+graph` | 0.3527 |
| `text+vector+graph` | 0.4661 |
| `text+vector+graph-including-seeds` | 0.6071 |

| Comparison | Delta | 95% CI | Reading |
|---|---|---|---|
| `text+vector+graph` − `text+vector` **(binding)** | **−0.1572** | [−0.1947, −0.1200] | **regresses** |
| `text+graph` − `text` | −0.2299 | [−0.2766, −0.1826] | regresses |
| `text+vector+graph-including-seeds` − `text+vector` | −0.0161 | [−0.0336, +0.0017] | undetermined |
| `text+vector` − `text` | +0.0407 | [+0.0010, +0.0798] | improves |

Largest per-query moves for the binding pair, `text+vector+graph` against
`text+vector`: query 24 falls from 0.9149 to 0.4787, query 40 from a perfect 1.0000 to
0.6321, query 3 from 0.8568 to 0.4948. The graph stream is not diluting mediocre
rankings, it is dismantling good ones.

**The double-counting arm is the diagnostic that makes this legible.** Keeping the
seeds scores 0.6071 while dropping them scores 0.4661 — a 0.14 gap in favour of the
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

| Tie analysis at k=10 (`weft-eval diagnose`) | Before | After |
|---|---|---|
| Queries producing a graph stream at all | 45 of 50 | 45 of 50 |
| Queries whose stream's membership is settled by DocID | 45 of 45 | 41 of 45 |
| Candidates excluded from the top k by DocID alone | 2,082 | 960 |
| Reported slots held at the cut score | not measured | 241 |
| Distinct scores per query | 3 | 3–24, and 3 is still the mode |

The reason is the graph, not the formula. Only 28.1% of documents have an in-corpus
out-edge (section 5.6), so two seeds rarely cite the same paper — the sum almost
always has exactly one non-zero term, and 0.5 remains the modal score. Granularity did
widen where seeds agree, from a flat 3 distinct scores to as many as 24 on one query,
but 3 is still the most common value across the 50 and the tie group still crosses the
cut on 41 of the 45 queries that answer at all. Multiplying granularity by `SeedN` only
helps if the seeds agree, and on a citation graph this sparse they mostly do not.

It did move the numbers, in the right direction and not nearly far enough:

| Arm | Before | After | Change |
|---|---|---|---|
| `text+graph` | 0.3527 | 0.3985 | +0.0458 |
| `text+vector+graph` | 0.4661 | 0.5005 | +0.0344 |
| `text+vector+graph-including-seeds` | 0.6071 | 0.5451 | −0.0620 |

| Comparison (after fix) | Delta | 95% CI | Reading |
|---|---|---|---|
| `text+vector+graph` − `text+vector` **(binding)** | **−0.1227** | [−0.1550, −0.0909] | **regresses** |
| `text+graph` − `text` | −0.1841 | [−0.2274, −0.1409] | regresses |
| `text+vector+graph-including-seeds` − `text+vector` | −0.0782 | [−0.1047, −0.0510] | regresses |
| `text+vector` − `text` | +0.0407 | [+0.0010, +0.0798] | improves |

`make eval` reprints this block. Largest per-query moves for the binding pair: query 24
from 0.9149 to 0.4819, query 40 from a perfect 1.0000 to 0.6321, query 3 from 0.8568 to
0.4948.

The `including-seeds` arm moved *down*, which is consistent rather than surprising: a
non-seed reached by two seeds now sums to 1.0 and can displace a seed, so that arm
admits more traversal output than it used to and inherits more of its cost. The
control is behaving like a control.

**Both measurements are kept.** The fix is attributable — it is worth about +0.04 —
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
configurations, 2,000 resamples each — **both pairs**: the binding
`text+vector+graph` against `text+vector`, and `text+graph` against `text` for
comparison.

| | Binding pair | Comparison pair |
|---|---|---|
| Configurations | 28 | 28 |
| Sign flips | **0** | **0** |
| Cells where the graph arm is ahead | **0** | **0** |
| CI upper bound, worst case | **−0.0065** (never reaches zero) | **−0.1278** |
| Widest gap | 0.4890 vs 0.6189 at `RRFk`=1, over-fetch 1 | 0.3929 vs 0.5826 |
| Narrowest gap | 0.6829 vs 0.7047 at `RRFk`=60, over-fetch 10 | 0.4039 vs 0.5826 |
| Baseline across the grid | 0.6189 → 0.7091 | 0.5826, invariant |

The comparison column reproduces what this section published before the binding pair
was added — delta range −0.1786 to −0.1896, worst-case CI upper bound −0.1278 — so the
grid was extended, not re-measured.

Over-fetching to depth 100 does not rescue it, and neither does collapsing the rank
constant to 1 or raising it to 200. Section 4 rule 2 is satisfied in the sense that
matters least: the sign is perfectly stable, and it is stably negative.

Two things this rules out. The result is not an artifact of `RRFk = 60`, which
[DATASETS.md](DATASETS.md) section 3 requirement 4 warned about: at the frozen depth the
binding delta is identical for every rank constant from 10 to 200, and −0.1299 at 1.

And it is not an artifact of shallow streams, though this is where the binding grid
answers differently from the comparison grid, so it is worth stating precisely. Depth is
the one knob that moves the binding delta materially — the gap narrows from 0.1273 to
0.0218 as over-fetch goes from 1 to 10 — and it narrows because *both* arms gain, not
because the graph stream starts contributing: the baseline gains as much. On the
comparison pair the same column changes only the fourth decimal, because a single-stream
baseline cannot deepen. Either way no cell brings the interval to zero.

**The limitation this section used to state is now measured away.** The grid swept only
`text+graph` against `text`, because that was the binding pair when it was written (§4.1's
amendment, later withdrawn) and because re-running it against `text+vector` looked like an
hour's work: `text` is a single stream and provably invariant across the grid, so it is
measured once instead of 28 times, and `text+vector` has no such shortcut. Meanwhile the
command printed "rule 2 holds" from a pair rule 2 does not name — the kind of claim this
document exists to catch, and it took a review to catch it. The estimate was also wrong:
tripling the evaluations costs 14 minutes, not an hour. Rule 2 is now satisfied by the
comparison it actually names.

**What the binding grid shows that the text-only grid could not.** Over-fetching lifts
the baseline itself — `text+vector` runs from 0.6233 at the frozen depth to 0.7091 at
`RRFk`=200, over-fetch 10 — because both of its streams get deeper, where the text-only
baseline was flat by construction. The frozen configuration is therefore not the best
one for the baseline arm, and the published 0.6233 is conservative in the direction that
matters least: the graph arm improves too, and never catches up. The gap narrows with
depth, from 0.1273 at the shallowest cell to 0.0218 at the closest, and the interval
still excludes zero in all 28 cells.

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
| 1.0 (as measured everywhere above) | 0.5005 | **−0.1227** | [−0.1550, −0.0909] | regresses |
| 0.5 | 0.6214 | −0.0019 | [−0.0057, +0.0000] | undetermined |
| 0.25 | 0.6214 | −0.0019 | [−0.0057, +0.0000] | undetermined |
| **0.1** | 0.6233 | **+0.0000** | [+0.0000, +0.0000] | converged to baseline |
| 0.05 and below | 0.6233 | +0.0000 | [+0.0000, +0.0000] | converged to baseline |

The bottom rows are the implementation's own check: as the weight approaches zero the
graph stream can only break ties among documents the other two already rank equally,
so the arm must converge exactly onto the baseline. It does, to the last bit, and it
gets there by weight 0.1 rather than 0.02.

**Two conclusions, pointing in opposite directions.**

**The entire −0.1227 belonged to fusion, not to the graph.** Halving the weight erases
all but 0.0019 of it. What section 5.8 measured was not a signal destroying a ranking;
it was RRF handing ten slots to a stream that had not earned them, exactly as
[FINDINGS](FINDINGS.md) milestone 4 section 5.2 suspected. Equal weighting is a
substantive ranking decision that unweighted RRF makes silently on every query, and
its cost here was 0.12 nDCG — an order of magnitude larger than anything the graph
signal itself was ever worth.

**And the graph contributes nothing at all.** Not "nothing detectable": **no weight in
this sweep beats the baseline.** Between 1.0 and 0.25 the arm is worse than
`text+vector`; from 0.1 down it is `text+vector`, delta exactly zero, interval a point
at zero — the graph stream is present, is being fused, and changes no ranking any
query is scored on. Section 4's rule asks for a positive delta whose interval excludes
zero; the best available is a delta of zero. Down-weighting a near-noise stream stops
it doing harm. It does not turn it into information.

**What "no weight" is entitled to mean, and what it is not.** This is a grid of eight
values, and nDCG is not monotonic in the weight: a weighted RRF ranking changes at
query-specific score-crossing thresholds, so the two ends of an unsampled interval do
not bound what happens inside it. The claim that carries no sampling assumption is the
lower one — **at 0.1 and below the arm is bit-identical to the baseline**, so every
weight in that region provably changes nothing, and there is no room for an unmeasured
maximum under it. Above 0.1 the honest statement is the sampled one: of the weights
tested, none beats the baseline, and the interval between 0.25 and 0.1 is where an
untested value would have to hide. Two things make that a thin hope rather than an open
question — the arm at 0.25 is already within 0.0019 of the baseline it cannot beat, and
the mechanism section 5.9 measured says why: the graph stream's scores are three
distinct values on most queries, so re-weighting moves a tie group rather than a
ranking. Worth stating precisely all the same, because "no weight" and "no weight we
tried" are different claims, and this document exists to keep them apart.

> An earlier revision of this table reported a best case of **+0.0018 at weight 0.1,
> CI [+0.0000, +0.0053]**, and both this document and the README built a sentence on
> it — "worth +0.0018, a 0.29% relative gain". That number does not survive
> section 5.12: it came from a build whose citation graph was assembled in Go map
> order, and it was smaller than the run-to-run spread of that graph. It was noise
> being read as the graph's best case. The corrected answer is cleaner and less
> flattering to the scorer.

### 5.12 The build was not deterministic, and every number above was re-measured

Found in review of the milestone 4 pull request, after the numbers had been published.

`weft-eval build` inverts the Semantic Scholar cache into `CorpusId → cord_uid` so a
citation can be written as a `Document.Link`. That mapping is not injective: CORD-19
ships the same paper under several `cord_uid`s, and on this release **20,556 of the
162,837 records carrying a CorpusId share one with another record**. The inversion
ranged over a Go map and let the last writer win. Go randomises map iteration, so the
winner was drawn afresh on every build:

| Two builds from the identical cache | CorpusIds resolving to a different `cord_uid` |
|---|---|
| run 1 vs run 0 | 9,377 of 142,281 |
| run 2 vs run 0 | 2,571 |
| run 3 vs run 0 | 8,240 |
| run 4 vs run 0 | 4,281 |
| run 5 vs run 0 | 3,795 |

Up to 6.6% of the graph moved between builds. The edge *count* did not — every
reference still resolved to exactly one document — which is why the build log looked
stable at 579,720 edges and nothing downstream noticed. What varied was which document
received each edge, and that is precisely what the graph scorer traverses.

**Fixed** by inverting in sorted `cord_uid` order and keeping the first, with the
collision count printed rather than left implicit
(`corpusIDIndex`, `cmd/weft-eval/main.go`). Which duplicate wins matters far less than
that the same one always does; they carry near-identical text. Two consecutive builds
from the same cache now produce byte-identical segments:

    a466fd42bbf407eb  index/seg-NNNNNN/docs
    f7709b3184820730  index/seg-NNNNNN/postings
    6d24fd18e4c8032e  index/seg-NNNNNN/terms

**Every measured number in section 5 was then taken again** against the rebuilt index:
sections 5.8 through 5.11, the 28-configuration sweep, the weight sweep and the
degeneracy diagnostic. Sections 5.1–5.7 are unaffected — they describe the corpus, the
join and the text baseline, none of which depend on the inversion. The two arms with no
graph stream, `text` at 0.5826 and `text+vector` at 0.6233, are unchanged to four
decimals, as they must be.

**What changed, and what did not.** The verdict did not move: the binding delta went
from −0.1156 to **−0.1202**, interval [−0.1521, −0.0886], still excluding zero, still
negative, still 0 sign flips across the sweep. What did move is section 5.11's headline.
The graph's best case under any fusion weight was published as **+0.0018**; it is
**+0.0000**. That figure was smaller than the run-to-run spread of the graph it was
measured on, and it was the one number in this document that a reader might have taken
as a reason to keep the scorer.

**The lesson is the same shape as section 4.1's, one layer lower.** There the interval
was asked a question it could not answer — it quantifies variation across queries, and
was read as though it covered corpus coverage too. Here it was asked an even more basic
one: the bootstrap resamples queries against *one* index, so it cannot see variation
that lives in how the index was built. Nothing in a confidence interval, a sweep across
28 configurations, or a seed pinned for reproducibility detects a pipeline that is
nondeterministic upstream of all three. The check that would have caught it is the
cheapest one available and is now in the repository twice — as a unit test on
`corpusIDIndex`, and as the observation that building the same corpus twice should
produce the same bytes.

A second review finding touched the numbers directly: `percentileIndex` truncated
`p·n` instead of taking ⌈`p·n`⌉ − 1, so both bootstrap bounds were reported one order
statistic high. Corrected, and every interval above reflects the correction; the effect
is in the third or fourth decimal and changes no reading.

### 5.13 One edge, and every number moved

Found in review of the same pull request, two rounds after 5.12.

`build` wrote a `Document.Link` for every reference that resolved inside the corpus and
counted each one. A references list can name the same `CorpusId` twice, and the
duplicate-paper merging described in 5.12 can resolve one back to the citing document
itself — so both produced entries the graph does not have. The traversal dedupes on
visit and a self-edge leads nowhere, so neither was ever walked; what they inflated was
the published edge total, which is a claim about density that no ranking can
corroborate or contradict.

**On this snapshot it is exactly one reference**, and the count goes from 579,720 to
**579,719**. `build` now prints the collapsed count on its own line, separate from the
dangling references that point outside the corpus.

**One edge in 579,720 moved the headline by 0.0025.** The binding delta went from
−0.1202 to **−0.1227**, interval [−0.1550, −0.0909]. That is not noise in the
measurement and it is not a flaw in the fix — it is section 5.9's degeneracy restated
as a sensitivity. The modal graph score is 0.5, the tie group crosses the cut on 41 of
the 45 queries that answer at all, and 241 of the reported slots are held at the cut
score with a further 960 candidates excluded from it by `DocID` alone. A single
changed adjacency re-decides a whole tie group, and the tie group
is most of what the graph stream contributes. **Read section 6 with that in mind: the
verdict is robust — 28 configurations, 0 sign flips, no interval near zero — and the
third decimal of any individual graph arm is not.**

**Every current figure was retaken** against the rebuilt index: the arm table and tie
analysis in 5.9, the 28-configuration sweep in 5.10, the weight sweep in 5.11, and the
degeneracy diagnostic. `text` at 0.5826 and `text+vector` at 0.6233 are unchanged to
four decimals, as they must be — neither fuses a graph stream. The weight sweep's
headline is unchanged: the best available delta is still exactly **+0.0000**, and the
arm still converges onto the baseline by weight 0.1. Sections 5.8 and 5.12 describe
states this repository has left behind and keep the numbers measured at the time.

**The index also stopped being anonymous.** Nothing bound a committed index to the
corpus it was built from: `build -any-snapshot` against another revision, followed by
`run` beside the pinned queries and qrels, passed every check there was, and a revision
keeping the same document keys satisfied the qrels check too. `build` now writes
`index/provenance.json` recording the sha256 of the corpus it read and whether
`-partial` was used, and `run`, `sweep`, `weights` and `diagnose` refuse an index that
does not match the pinned release, cannot say which corpus it holds, or was built over
a cache that does not cover it. `-any-snapshot` opts out, as everywhere else. The
record is removed before the segments are replaced rather than written after, so a
rebuild that does not finish leaves an index that cannot say what it holds — which is
refused — instead of one describing itself with the previous build's account.

**And both sides of the vector arm now name their embedding.** Width is not provenance:
SPECTER v1 and v2 are both 768-dimensional, so a cached document vector from the wrong
one was indexed on a width match and the query vectors were checked for id and text but
never for which model produced them. Cosine similarity across two embedding spaces is
not a similarity, and it arrives as a plausible vector baseline with the whole graph
delta measured against it. `build` now refuses document vectors carrying a foreign
model, `gen_query_vectors.py` records `allenai/specter2_base+allenai/specter2_adhoc_query`
in every record it writes, and `run` refuses a query vector that names anything else.
An unrecorded model is warned about rather than refused on both sides: that is what the
committed artifacts hold, so refusing it would refuse the published measurement rather
than a mistake.

---

## 6. Verdict

**The PRD's second falsification condition is met. The answer is NO: graph proximity
as weft implements it does not improve nDCG@10.**

| Section 4 condition | Result |
|---|---|
| 1. Frozen: paired 95% CI for `+graph` − baseline excludes zero and is positive | **FAILS** — excludes zero and is *negative*: −0.1227, [−0.1550, −0.0909] |
| 2. Stable: the sign does not flip across the sweep | Holds — 0 flips in 28 configurations **of the pair this rule names**, negative throughout, closest interval [−0.0380, −0.0065] |
| Best case under any fusion weight (section 5.11) | **+0.0000** — no weight in the tested grid beats the baseline, and at 0.1 and below the arm *is* the baseline |

Condition 1 fails, which is the "no" branch. Per the PRD and [D-004](DECISIONS.md) the
consequence is that `pkg/scorer/graph` goes while the `Scorer` interface,
`Query.Seeds` and `pkg/scorer/recency` stay.

**Section 5.11 changes why, without changing what.** The headline −0.1227 was fusion's
doing: give the graph stream half a vote and all but 0.0019 of the regression
disappears. The honest statement of the result is therefore not "graph proximity
destroys rankings" but the flatter and less dramatic one — **at no weight between 1.0
and 0 does the graph stream beat the baseline, and from 0.1 downward the arm is the
baseline exactly.** It is not harmful information. It is, as far as this measurement
can tell, not information.

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
returns thousands of candidates carrying as few as 3 distinct scores — the modal case
across the 50 queries — so it has almost no internal ranking; RRF then gives its
arbitrary top ten the same vote as BM25's top ten, and they displace results that were
correct. Query 40 falls from a perfect 1.0000 to 0.6321. The signal is not weak, it is
close to noise, and unweighted fusion has no way to discount it.

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
  text — should have been safe. It regresses too (−0.0769).

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

`build` fails rather than producing a partial index if `prepare` has not finished —
including after a `prepare -limit` smoke test. Rerun `prepare` (it resumes from the
append-only cache), or pass `-partial` to build the slice deliberately and get the
warning that its arms are not publishable. `prepare` also reports the embedding model
behind every vector *in the whole cache*, not just the batches that run fetched, so an
interrupted preparation cannot hide two embedding spaces in one index.

Every pairing between two of these inputs is now checked rather than assumed, because
each pair is two files selected by separate flags and nothing else compares them: the
index against the cache (coverage, above), the queries against their vectors (section
5.4 — by text, since ids are stable enough to pair a stale file cleanly), and the index
against the qrels. The last one has no wrong lookup to catch it: a judged-relevant
document the index does not hold never appears in a ranking, it just stays in the ideal
one, so every arm loses the same part of its score and the run reads as a ranking that
got worse. The coverage gate itself needed one further guard. `prepare` records a key
for every document it asks about, so a metadata release that joins to nothing would
have recorded the entire corpus as asked-and-unjoinable and satisfied the gate with a
cache that never joined once; a zero-match join is now refused unless something already
in the cache proves the join can work at all.

The remaining pairings are between a file and the release it claims to be, and those are
the hashes in section 5.1. Three smaller refusals close the same shape of hole in the
inputs each command reads: a repeated key in `s2.jsonl` (concatenated caches — a later
tombstone would discard a vector and a reference list while coverage still counted the
document), a repeated query/document pair in the qrels carrying a *different* grade
(concatenated assessment rounds — the ground truth would depend on row order; an
identical repeat is still fine), and a repeated id in `query-vectors.jsonl`. `diagnose`
additionally requires `-deep` to exceed `-k`, because the tie group it measures is only
visible past the cut and a shallower frontier reports zero arbitrary slots for every
query whatever the graph did.

Bootstrap seed 20260814 and the frozen constants are compiled in, so `make eval`
reprints the intervals in this document rather than approximations of them.

Section 5.12's determinism claim is checked by building twice and comparing, which is
worth doing once on any new cache — it is the check whose absence let a nondeterministic
graph go unnoticed through a full round of published numbers:

```bash
go run ./cmd/weft-eval build && shasum -a 256 .eval-data/index/seg-*/*
go run ./cmd/weft-eval build && shasum -a 256 .eval-data/index/seg-*/*
```

Section 5.8's table needs the scorer HEAD replaced, and is the one block above that
`make eval-full` does not reprint:

```bash
git show ed80dc2:pkg/scorer/graph/graph.go > pkg/scorer/graph/graph.go
go run ./cmd/weft-eval run && go run ./cmd/weft-eval diagnose
git checkout pkg/scorer/graph/graph.go
```
