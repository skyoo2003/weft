# Status — what is built, and what it cost

One screen: which milestones exist, which passed, which came back void. [FINDINGS.md](FINDINGS.md) is the per-milestone record behind every row. When the two disagree, FINDINGS is right and this file is stale.

**Not usable in production.** A commit holds the write lock for as long as it takes — 11 seconds for a 20,000-document batch, with reads queued behind it. Sustained query load collapses at 27 queries per second rather than degrading. [LIMITATIONS.md](LIMITATIONS.md) is the full list.

## Milestones

| # | Milestone | State | Headline |
| --- | --- | --- | --- |
| 1 | Scorer-agnostic fusion | ✅ | 3/3 assertions pass |
| 2 | Persistence | ✅ | restores identically, commits atomically |
| 3 | Scale — merge, lazy load, ANN | ✅ | `Open` 979 ms → 54 ms; ANN recall@10 0.992 at 4.6× query speed |
| 4 | Quality — graph contribution | ✅ | measured, and the answer is **no**: [Published numbers](#published-numbers) |
| 5 | Performance — p99 with GC | ✅ | p99 **108.193 ms**, collector 411 µs of it (0.38%) |
| 6 | External contribution readiness | ✅ | two subjects added a signal from the docs alone, and found three doc defects |
| 7 | A baseline nobody has to qualify | ⚠️ | **there is no baseline** — see [below](#milestone-7--the-load-point-is-not-reproducible) |
| 15 | Graph indexing and search | ⚠️ | built and measured for **cost, not quality** — see [below](#milestone-15--built-but-unscored) |
| 16 | Query expressiveness | ✅ | `pkg/query`: `Must`, `MustNot`, `Glob`, `Phrase`. No format change, no dependency |
| 17 | The allocation floor under a query | ✅ | **521× faster, 73,890× less memory** on a query matching nothing |
| 18 | Field search | ✅ | `Document.Fields`, `text.NewField`. Format **v5**; v2–v5 all read |
| 19 | Ranges, fuzzy, field-scoped patterns | ✅ | `query.Range`, `EncodeInt`/`EncodeTime`, `query.Fuzzy`. No numeric index, no format change |
| 20 | A query string syntax | ✅ | `query.Parse`. Flat clause list, **no boolean algebra** |
| 21 | Top-k selection | ✅ | k-sized heap: **23.6× faster**, zero allocations, provably identical answer |
| 22 | Measured against bleve | ✅ | **weft is faster on every query shape** — and allocated 156× more |
| 23 | Document-at-a-time | ✅ | that 156× gap is now **1.8×**, and weft is **3.1× faster** |

Milestone 4 ran ahead of 3 because the dataset fits in memory and the project's second falsification condition was waiting on it.

### The rows worth a second sentence

**Milestone 3.** Both paths hold, but the vector gain is smaller than hoped.

**Milestone 5.** 1.88× bleve v2.6.0 against a 10× bar. That ratio predates milestones 17 and 21, which changed the query path by 2.78×; milestone 22 re-measured against bleve directly and weft is now faster on every query shape. Two debts remain unpaid — see [below](#two-debts-milestone-5-has-not-paid).

**Milestone 6.** Both subjects were **agents, not people**, so this is a lower bound and not a user study: [ADOPTION.md](ADOPTION.md).

**Milestone 17.** `scorer/text` sized its BM25 accumulator to the *corpus*, so a query matching nothing cost 1.18 MB and 78 µs on a 50,000-document index — 4.73 MB on the evaluation corpus, and 128 MB/s of garbage at the arrival rate where load collapses. It is now sized to the widest posting list the query walks: 103× and 165× better on a rare term, unchanged on a term every document holds, no score changes. Whether it moves the collapse is a ladder run and is **not** claimed.

**Milestone 18.** A field is a **term space, not a section**: every field's tokens are indexed under `engine.FieldTerm` as `name\x00token`, so nothing in the index learned what a field means, and searching two fields is two scorers plus fusion. The field scorer does **not** normalize by length, and [FINDINGS milestone 18](FINDINGS.md) says why.

**Milestone 20.** No parentheses and no OR, because a clause list with `+` and `-` is exactly what rank fusion can express. A tree would be a different engine.

**Milestone 22.** `make bench-head` — 50,000 documents, one BM25 query, top 10. 9.2× faster on a query matching nothing, 1.36× on a term held by every document.

**Milestone 23.** `scorer/text` walks one `BlockCursor` per term into a k-sized `Collector`, so a query holds no accumulator, no candidate slice and no posting list: 2.82 MB a query became **32 KB**. On three of five query shapes weft now allocates *less* than bleve. No score changes.

### Milestone 7 — the load point is not reproducible

Three runs at one arrival rate gave medians 40× apart: 37.9 ms, 1.539 s and 416 ms. Two of the three shed 14% and 11% of the load; one shed nothing. There is no suspension story, no thermal story and no relative-load story that fits.

The one difference that survives: the flat observation was the fourth rung of a ladder, and the two collapses were single rungs. [FINDINGS milestone 7](FINDINGS.md), [D-012](DECISIONS.md).

### Milestone 15 — built but unscored

The citation graph is a real index: resolved into `DocID` space, forward and reverse. A personalized-PageRank scorer walks it at 5.8× the speed of the BFS arm and **211× fewer allocations** (48,522 → 230 a query). The tie group milestone 4 blamed is gone — 2 distinct scores became 8 on the fixture that reproduces it.

**No nDCG number exists.** The arms are registered and the corpus has not been run: [FINDINGS milestone 15](FINDINGS.md).

## Versioning

`v0.1.0` is the first tag. It gives you a shorter name than the pseudo-version `go get` resolved to before it. It changes nothing the production warning says — a tag is a commit you can name, not a claim that the code became ready.

[CHANGELOG](../CHANGELOG.md) records three things only: the exported API of every package under `pkg/`, the on-disk format version, and the minimum Go version. The milestone numbers above are not among them.

## Two debts milestone 5 has not paid

Stated here rather than left in a findings document.

1. **One run, not the three repetitions [PERF.md](PERF.md) requires.**
2. **The arm you would actually deploy — `text + vector` — has no published tail.** At four times the cost per query, 10,000 samples take over five hours. The published p99 is comparable to bleve's; it is not the number a `text + vector` user will see.

Milestone 7 went to pay the first and could not — see [above](#milestone-7--the-load-point-is-not-reproducible).

**So `108.193 ms` still stands, with two further caveats.** It is one draw from a load-point rule that flips between the ladder's slowest and fastest rung on a few milliseconds of first-rung median, and it was measured before the instrument could tell you whether the machine slept through it. Neither is a reason to withdraw it. Both are reasons not to compare against it silently.

## Published numbers

TREC-COVID: 50 queries, 171,332 documents, 579,719 in-corpus citation edges from Semantic Scholar, 148,232 SPECTER2 vectors. nDCG@10, paired bootstrap over 10,000 resamples.

| Arm | nDCG@10 |
| --- | --- |
| `text` (BM25) | 0.5826 |
| `text + vector` | **0.6233** |
| `text + vector + graph` | 0.5005 |

**Graph proximity does not improve ranking.** Under equal-weight fusion it costs 0.1227 nDCG@10 (95% CI [−0.1550, −0.0909]). Across 28 configurations of the RRF rank constant and fusion depth, the sign never flips and no interval reaches zero.

**But that −0.1227 belonged to the fusion operator, not to the graph.**

| Graph stream weight | nDCG@10 | Delta vs baseline |
| --- | --- | --- |
| 1.0 | 0.5005 | −0.1227 |
| 0.5 | 0.6214 | −0.0019 |
| ≤0.1 | 0.6233 | +0.0000, converged to baseline |

Half a vote instead of a full one erases all but 0.0019 of the regression. No weight in the tested grid beats the baseline, and from 0.1 downward the arm *is* the baseline, delta exactly zero. [EVAL.md](EVAL.md) section 5.11 says what that is and is not entitled to claim.

So the accurate statement is the flatter one: the graph signal is not harmful information. It is not information.

### What shipped because of it

**Equal weighting is a ranking decision RRF makes silently on every query**, and here it cost two orders of magnitude more than the signal being evaluated. So `fusion.FuseWeighted` shipped:

```go
// Trust the graph stream less, without fusion learning what a graph scorer is.
fuse := fusion.FuseWeighted(1, 1, 0.1)
results, err := engine.Search(ctx, q, 10, fuse, txt, vec, gr)
```

Weights index by stream *position*, not scorer kind — the caller already fixed that order — so this does not compromise scorer-agnosticism, and `go list -deps ./pkg/fusion` still names no scorer. `Fuse` is unchanged and its unweighted path is bit-identical.

Where the weights should come from is the open question. Hand-tuning per corpus reintroduces exactly the burden this design avoids, so `FuseWeighted`'s docs say a caller without its own measurement should use `Fuse`. [FINDINGS milestone 4 §7](FINDINGS.md).

### What was kept, and what was falsified

`scorer/graph` is **kept rather than deleted**, with the verdict at the top of its package documentation. The falsification condition said a worthless signal goes; the weight sweep then showed the scorer is inert rather than harmful, and deleting it would cut the milestone 1 assertions from four signals to three. [D-005](DECISIONS.md) argues both sides.

What was falsified is one construction — BFS hop distance, seeded from the text top 5 — not the idea that citation structure carries signal. The graph stream returns thousands of candidates carrying 3 to 16 distinct scores, so it has almost no internal ranking to contribute at any weight.

### Two supporting results

- **BM25 matches `rank_bm25` to 4.44e-16**, and nDCG matches `pytrec_eval`'s `ndcg_cut_10` on 12 discriminating fixtures. Both checks ran before any arm number was published, and the second found the plan's own nDCG definition to be wrong.
- **BM25 alone scores 0.5826** where BEIR reports ~0.656 for Anserini with stemming and a stopword list. The same order, from a whitespace tokenizer.

Full measurement design, coverage, sweep and reasons to doubt the numbers: [EVAL.md](EVAL.md). Reproduce with `make eval`.
