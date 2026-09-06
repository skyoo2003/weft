# Status — what is built, and what it cost

Where the project actually is. [FINDINGS.md](FINDINGS.md) is the per-milestone record behind every row here; this file is the one screen that says which milestones exist, which passed, and which came back void. When the two disagree, FINDINGS is right and this file is stale.

**Not usable in production.** A commit holds a write lock for as long as it takes — 11 seconds for a 20,000-document batch, with reads queueing behind it — and sustained query load collapses at 27 queries per second on the machine measured rather than degrading. [LIMITATIONS.md](LIMITATIONS.md) is the full list.

## Milestones

| # | Milestone | State |
| --- | --- | --- |
| 1 | Scorer-agnostic fusion | ✅ 3/3 assertions pass |
| 2 | Persistence | ✅ restores identically, commits atomically |
| 3 | Scale — segment merge, lazy loading, ANN | ✅ both paths hold. `Open` 979 ms → 54 ms, ANN recall@10 0.992 at 4.6× the query speed — but the vector gain is smaller than hoped |
| 4 | Quality — graph contribution to nDCG | ✅ measured, and the answer is **no** — see [Published numbers](#published-numbers) |
| 5 | Performance — p99 including GC pauses | ✅ p99 **108.193 ms**, of which the collector is 411 µs (0.38%); 1.88× bleve v2.6.0 against a 10× bar. That ratio predates milestones 17 and 21, which changed the query path by 2.78×; milestone 22 re-measured against bleve directly and weft is now **faster on every query shape** |
| 6 | External contribution readiness | ✅ measured — two subjects with no prior sight of the tree added a signal from the documentation alone, which found three documentation defects. Both subjects were **agents, not people**, so this is a lower bound and not a user study: [ADOPTION.md](ADOPTION.md) |
| 7 | A baseline nobody has to qualify | ⚠️ **there is no baseline.** Three runs at one arrival rate gave 37.9 ms, 1.539 s and 416 ms; the load point is not reproducible and what decides the outcome is not the load: [FINDINGS milestone 7](FINDINGS.md) |
| 15 | Graph indexing and search | ⚠️ **built and measured for cost, not for quality.** The citation graph is an index — resolved into `DocID` space, forward and reverse — and a personalized-PageRank scorer walks it: 5.8× the speed of the BFS arm and **211× fewer allocations** (48,522 → 230 a query). The tie group milestone 4 blamed is gone, 2 distinct scores becoming 8 on the fixture that reproduces it. **No nDCG number exists**: the arms are registered and the corpus has not been run: [FINDINGS milestone 15](FINDINGS.md) |
| 16 | Query expressiveness — boolean, pattern, phrase | ✅ `pkg/query` ships the `Fuser` every adopter had to write for themselves — `Must`, `MustNot`, composable and indexed by stream position — plus `Glob` for prefix/suffix/wildcard terms and `Phrase` for exact runs. No format change, no new dependency, and `go list -deps ./pkg/query` names no scorer |
| 17 | The allocation floor under every query | ✅ `scorer/text` sized its BM25 accumulator to the **corpus**, so a query matching nothing allocated 1.18 MB and 78 µs on a 50,000-document index — 4.73 MB on the evaluation corpus, and 128 MB/s of garbage at the arrival rate where load collapses. Now sized to the widest posting list the query walks: **521× faster and 73,890× less memory** on a query matching nothing, 103× and 165× on a rare term, unchanged on a term held by every document. No score changes. Whether it moves the collapse is a ladder run and is **not** claimed: [FINDINGS milestone 17](FINDINGS.md) |
| 18 | Field search | ✅ `Document.Fields` and `text.NewField(ix, "title")`. A field is a **term space, not a section** — every field's tokens are indexed under `engine.FieldTerm` as `name\x00token`, so nothing in the index learned what a field means and searching two fields is two scorers plus fusion. Format **v5**; v2 through v5 all read. The field scorer does **not** normalize by length, and [FINDINGS milestone 18](FINDINGS.md) says why |
| 19 | Ranges, fuzzy, field-scoped patterns | ✅ `query.Range` over a field, with `EncodeInt`/`EncodeTime` making a string sort like the number it encodes — so numeric and date ranges cost **no numeric index and no format change**, and scale with a field's distinct values rather than with the corpus. `query.Fuzzy` is Levenshtein within 2, bytes not runes. `Glob`, `Range` and `Fuzzy` all scope to a field |
| 20 | A query string syntax | ✅ `query.Parse` speaks `+title:covid -draft "airborne transmission" price:[lo TO hi] covd~2`, returning a `Plan` the caller fuses with whatever policy it likes. **No boolean algebra** — no parentheses, no OR — because a clause list with `+` and `-` is exactly what rank fusion can express, and a tree would be a different engine |
| 21 | Top-k selection | ✅ `TopK` sorted 50,000 candidates to return 10. It now selects with a k-sized heap whose root is the worst of the best k so far: **23.6× faster** on that shape, zero allocations, and the answer is provably identical — the two paths are checked against each other. `scorer/text`'s expensive query drops **2.78×** end to end |
| 22 | Measured against bleve | ✅ `make bench-head` — 50,000 documents, one BM25 query, top 10. **weft is faster than bleve on every query shape**: 9.2× on a query matching nothing, 1.36× on a term held by every document. And it allocates **156× more** on that same query, which is the gap that is left and the reason the next round is document-at-a-time: [FINDINGS milestone 22](FINDINGS.md) |
| 23 | Document-at-a-time | ✅ The 156× memory gap against bleve is **1.8×**, and weft is **3.1× faster** on the same query. `scorer/text` walks one `BlockCursor` per term into a k-sized `Collector`, so a query holds no accumulator, no candidate slice and no posting list — 2.82 MB a query became **32 KB**. On three of five query shapes weft now allocates *less* than bleve. No score changes: [FINDINGS milestone 23](FINDINGS.md) |

Milestone 4 ran ahead of 3 because the dataset fits in memory and the project's second falsification condition was waiting on it.

## Versioning

`v0.1.0` is the first tag. It gives you a shorter name than the pseudo-version `go get` resolved to before it, and it changes nothing the production warning above says — a tag is a commit you can name, not a claim that the code became ready.

[CHANGELOG](../CHANGELOG.md) is where a version tells you whether you have work to do, and it records three things only: the exported API of every package under `pkg/`, the on-disk format version, and the minimum Go version. The milestone numbers in the table above are not among them.

## Two debts milestone 5 has not paid

Stated here rather than left in a findings document. Its headline is **one run, not the three repetitions [PERF.md](PERF.md) requires**, and the arm you would actually deploy — `text + vector` — **has no published tail**, because at four times the cost per query 10,000 samples take over five hours. The published p99 is comparable to bleve's; it is not the number a `text + vector` user will see.

Milestone 7 went to pay the first of those and could not. Three runs at the same arrival rate produced medians 40× apart — one rung shed nothing at 37.9 ms, two collapsed to 1.539 s and 416 ms and shed 14% and 11% of the load — with no suspension, no thermal story and no relative-load story that fits. The difference that survives is that the flat observation was the fourth rung of a ladder and the two collapses were single rungs.

**So `108.193 ms` still stands, and now stands with two further caveats**: it is one draw from a load-point rule that flips between the ladder's slowest and fastest rung on a few milliseconds of first-rung median, and it was measured before the instrument could tell you whether the machine slept through it. Neither is a reason to withdraw it; both are reasons not to compare against it silently. [FINDINGS milestone 7](FINDINGS.md), [D-012](DECISIONS.md).

## Published numbers

TREC-COVID, 50 queries, 171,332 documents, 579,719 in-corpus citation edges from Semantic Scholar, 148,232 SPECTER2 vectors. nDCG@10, paired bootstrap over 10,000 resamples.

| Arm | nDCG@10 |
| --- | --- |
| `text` (BM25) | 0.5826 |
| `text + vector` | **0.6233** |
| `text + vector + graph` | 0.5005 |

**Graph proximity does not improve ranking.** Under equal-weight fusion it costs 0.1227 nDCG@10 (95% CI [−0.1550, −0.0909]), and across 28 configurations of the RRF rank constant and fusion depth the sign never flips and no interval reaches zero.

**But that −0.1227 belonged to the fusion operator, not to the graph.** Giving the graph stream half a vote instead of a full one erases all but 0.0019 of the regression. No weight in the tested grid beats the baseline, and from 0.1 downward the arm *is* the baseline, delta exactly zero — see [EVAL.md](EVAL.md) section 5.11 for what that is and is not entitled to claim. So the accurate statement is the flatter one: the graph signal is not harmful information, it is not information.

| Graph stream weight | nDCG@10 | Delta vs baseline |
| --- | --- | --- |
| 1.0 | 0.5005 | −0.1227 |
| 0.5 | 0.6214 | −0.0019 |
| ≤0.1 | 0.6233 | +0.0000, converged to baseline |

**Equal weighting is a ranking decision RRF makes silently on every query**, and here it cost two orders of magnitude more than the signal being evaluated. So `fusion.FuseWeighted` shipped:

```go
// Trust the graph stream less, without fusion learning what a graph scorer is.
fuse := fusion.FuseWeighted(1, 1, 0.1)
results, err := engine.Search(ctx, q, 10, fuse, txt, vec, gr)
```

Weights index by stream *position*, not scorer kind — the caller already fixed that order — so this does not compromise scorer-agnosticism, and `go list -deps ./pkg/fusion` still names no scorer. `Fuse` is unchanged and its unweighted path is bit-identical. Where the weights should come from is the open question: hand-tuning per corpus reintroduces exactly the burden this design avoids, so `FuseWeighted`'s docs say a caller without its own measurement should use `Fuse`. [FINDINGS milestone 4 §7](FINDINGS.md).

`scorer/graph` is **kept rather than deleted**, with the verdict at the top of its package documentation. The falsification condition said a worthless signal goes; the weight sweep then showed the scorer is inert rather than harmful, and that deleting it would cut the milestone 1 assertions from four signals to three. [D-005](DECISIONS.md) argues both sides.

What was falsified is one construction — BFS hop distance, seeded from the text top 5 — not the idea that citation structure carries signal. The graph stream returns thousands of candidates carrying 3 to 16 distinct scores, so it has almost no internal ranking to contribute at any weight.

Two supporting results:

- **BM25 matches `rank_bm25` to 4.44e-16**, and nDCG matches `pytrec_eval`'s `ndcg_cut_10` on 12 discriminating fixtures. Both checks ran before any arm number was published, and the second found the plan's own nDCG definition to be wrong.
- **BM25 alone scores 0.5826** where BEIR reports ~0.656 for Anserini with stemming and a stopword list — the same order, from a whitespace tokenizer.

Full measurement design, coverage, sweep and reasons to doubt the numbers: [EVAL.md](EVAL.md). Reproduce with `make eval`.
