# Community research — one round, before milestone 2

**Date:** 2026-08-13. **Method:** desk research over public sources — bleve source code (tag v2.6.0), GitHub issues across seven engines, vendor docs. **What this is not:** user interviews. The PRD's evidence gap ("사용자 인터뷰 0건") is narrowed, not closed.

Two questions were asked, matching the two unverified claims in the PRD's Evidence section.

---

## 1. Is bleve's fusion actually closed to new signal kinds?

The PRD assumed it; milestone 1 was told to verify it by reading code. **Verified, with one correction.**

**Closed, structurally.** bleve's RRF/RSF path (v2.5.4+) fuses exactly `1 FTS stream + len(req.KNN)` vector streams. The stream count is open — but only across kNN sub-queries; the stream _kind_ is fixed at text-plus-vectors:

- `rescorer.go:95` — `rescore(ftsHits, knnHits)`: fusion inputs are exactly two collections.
- `fusion/rrf.go:30` — "applies Reciprocal Rank Fusion across the primary FTS results and each KNN sub-query." Weights are indexed `weights[0]` = FTS, `weights[i+1]` = kNN query i.
- `rescorer.go:137` — `mergeDocs` overwrites `hit.ScoreBreakdown` on FTS hits, so a user smuggling extra signals in through their own disjunction breakdown gets clobbered. There is no back door into the built-in fusion.
- No scorer registry, no fusion-stream interface; custom Searchers are unexported machinery.

So a graph, recency or popularity signal cannot join bleve's rank fusion as a first-class stream — which is the exact failure mode weft's `Scorer`/`Fuser` design exists to remove.

**The correction: "requires forking bleve" is overstated.** Two escape hatches exist without touching internals:

1. v2.6.0 shipped `CustomScoreQuery` (PR #2289) — a per-hit Go callback with doc-values access. It can fold a third signal into the FTS stream's scores _before_ fusion. The signal reshapes stream 0's ranks; it does not get its own stream, weight, or rank list.
2. The `fusion` package is exported and operates on public `DocumentMatch` fields. An application can run its own retrievals, populate `ScoreBreakdown` with arbitrary signals, and call N-way RRF itself — outside the engine pipeline, so pagination, facets and collection are then the app's problem. That is fusion as a _formula_, not fusion as an _architecture_; it is also roughly the position weft's `Fuse` starts from, minus the shared index and scorer contract.

**Demand evidence inside bleve's own tracker:** #77 "Custom scoring" (2014, open), #396 "custom scoring function" (2016, open ~9 years), **#620 "Boosting by freshness" (2017, open — exactly weft's recency scorer)**, #1330 "how to change hit score" (2020, open, redirected to #396). The maintainers shipping `custom_score` in 2025 is supply responding to that demand — and also bleve actively closing its own gap.

## 2. Does anyone want pluggable ranking signals?

**For.** Explicit asks with the right shape exist across engines: OpenSearch k-NN #1271 (combine hybrid query with function_score — the exact three-signal composition), tantivy #815 (BM25 + vector weighted together; author hand-merges in middleware today), typesense #376 (ES-style function/decay scoring incl. recency), meilisearch discussion #548 (relevance vs favourite_count), ArcadeDB #4066 (fusion designed as "N ranked sub-pipelines, intentionally not tied to dense+sparse"). Vendors keep shipping partial answers (bleve #2289, OpenSearch neural-search #1152 RRF weights, Elastic linear retriever), which is the strongest signal the pressure is real.

**Against — and this shapes positioning more than the "for" column.**

- Most demand saturates at _two signals plus weights_. OpenSearch #1152 closed at per-retriever weights; nobody in that thread asked for arbitrary signals.
- Workaround culture is entrenched and mostly tolerated: sort-by-signal-then-score (bleve's documented pattern), `tweak_score` closures (tantivy), `ORDER BY bm25(...) * decay` (SQLite), ranking rules (meilisearch). The tantivy #815 author called native support merely "interesting to discuss".
- RRF's own pitch is anti-tuning ("stop worrying about boosting"); part of the market actively wants _fewer_ ranking knobs, not more.
- Teams that genuinely need many signals tend to jump past hand fusion to LTR/rerankers (OpenSearch LTR, Metarank, cross-encoders).
- bleve #396 accumulated modest engagement over nine years — persistent, not burning.

## 3. Prior art — where an open N-signal ranking already exists

| System                                                               | N-signal fusion open?                                                                        | Embeddable library?         | Language |
| -------------------------------------------------------------------- | -------------------------------------------------------------------------------------------- | --------------------------- | -------- |
| **Lucene** (FunctionScoreQuery + Expressions; `TopDocs.rrf` in 10.x) | **Yes** — expression over `_score` + any doc-values fields; N-way RRF over arbitrary TopDocs | **Yes**                     | Java     |
| Vespa rank profiles                                                  | Yes — arbitrary expressions, phased ranking                                                  | No — server platform        | Java/C++ |
| Elasticsearch retrievers / OpenSearch hybrid+LTR                     | Partially — N child retrievers with weights, but retrievers must be engine query types       | No — servers                | Java     |
| bleve                                                                | No — fixed FTS+kNN streams (see §1)                                                          | Yes                         | Go       |
| tantivy                                                              | DIY — custom collectors over fast fields; no fusion primitive                                | Yes                         | Rust     |
| Qdrant / Weaviate                                                    | Sub-query fusion, engine-typed sub-queries only                                              | No — servers                | Rust/Go  |
| ParadeDB / SQLite FTS5                                               | Yes at the SQL layer — signals as columns                                                    | Not for Go apps / C via cgo | SQL/C    |

**Lucene is the finding that matters.** Adding a ranking signal in embeddable Lucene = index a doc-values field + edit an expression — no engine code touched. The capability weft is building is not novel; it has existed for years, in Java. What does not exist is the Go equivalent: bleve is closed (§1), bluge is unmaintained (custom-score example exists, repo dormant since ~2022), riot archived, zinc/blast/phalanx are servers.

## 4. What this changes for weft

1. **The architecture bet survives, sharpened.** Honest positioning: _"Lucene-Expressions-class open ranking, in Go, with fusion-native signals"_ — a language-and-design gap, not a capability-first invention. The PRD's Problem statement ("fusion is always the special case") is now backed by bleve's actual source, not assumption.
2. **New competitive risk, worth tracking:** bleve is moving (v2.5.4 fusion, v2.6.0 custom_score, within ~a year). If bleve ever exposes fusion streams as an interface, weft's differentiator thins to from-scratch purity. Watch bleve releases at each weft milestone.
3. **Demand realism:** the buyer for "N pluggable signals" is narrower than the buyer for "hybrid search" — it is exactly the PRD's stated primary user (infrastructure contributors extending ranking in-process), and that population's revealed alternative today is middleware-side merging. Milestone 6 docs should speak to the person currently hand-rolling RRF in app code.
4. **Interviews remain undone.** This round was desk research. The PRD risk row stays open with its mitigation updated; the next research round should be conversations, not archaeology.

## Caveats

- Reddit/HN grassroots chatter is under-sampled (poorly indexed); GitHub reaction counts were not collected — "demand" here means explicit issue text, not measured popularity.
- bleve inspection was line-level at v2.6.0 only; v2.5.4–2.5.7 fusion internals were spot-checked, not audited.
- The app-side `fusion.*` route in bleve is inferred from exported signatures; no proof-of-concept was built.
- Some issue dates are approximate (search-result metadata, not individually fetched).
