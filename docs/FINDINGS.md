# Milestone 1 — Scorer-agnostic fusion

**Verdict: the architecture hypothesis holds. 3/3 assertions pass.** Evidence: `pkg/engine/architecture_test.go`.

| Package | Implementation | Tests |
|---|---|---|
| `pkg/engine` | 317 | 557 |
| `pkg/fusion` | 54 | 138 |
| `pkg/scorer/text` | 95 | 167 |
| `pkg/scorer/vector` | 87 | 146 |
| `pkg/scorer/graph` | 153 | 312 |
| `pkg/scorer/recency` | **71** | 106 |

777 implementation lines, 1,426 test lines, zero external dependencies.

---

## 1. Result

**Assertion 1 — fusion is invariant to scorer count.** Three and four scorers use the same call expression; compiling is the proof.

```go
engine.Search(ctx, q, 5, fusion.Fuse, three...)  // text, vector, graph
engine.Search(ctx, q, 5, fusion.Fuse, four...)   // + recency
```

Compiling alone proved insufficient — a scorer returning nothing passes it too. The corpus therefore holds a document (`lonely`) that matches no query term, carries no vector and is linked from nowhere, so only recency sees it. Three scorers must not surface it; four must.

**Assertion 2 — a new scorer is cheap.** `pkg/scorer/recency` is 71 implementation lines against a 100-line budget, zero lines changed in `engine/` or `fusion/`.

"Zero lines changed" is asserted through the import graph, not a diff: no non-test file in `engine/` or `fusion/` imports `scorer/*`, checked with `go/parser`. A diff needs a baseline commit and stops being reproducible; the import check needs nothing, holds for every future scorer, and does not trip on the words "text" or "vector" in comments. The budget counts implementation files only — counting tests would reward untested scorers.

**Assertion 3 — fusion cannot see scorers.**

```
go list -deps ./pkg/fusion   → engine, and no other weft package
go list -deps ./pkg/engine   → no weft package at all
go list -m all               → this module only
```

`fusion.Fuse` never reads `Candidate.Score`, only rank. `TestScoresAreNeverRead` pins this by putting `0.0001` on the rank-1 document and `999999` on rank-2 and asserting the order survives.

---

## 2. Constraints the architecture imposed

Not design choices. Each was discovered by trying to violate the hypothesis.

### 2.1 Dependency direction is fixed, which makes `engine` ignorant of fusion too

`fusion` imports `engine` for `Candidate`, so the reverse is a compile-time cycle. `engine.Search` therefore takes a `Fuser` function parameter. Consequence beyond the fix: `engine` knows neither the scorers nor the fusion strategy, so replacing RRF with a weighted score sum changes nothing in `engine/`.

### 2.2 The tokenizer must live in `engine`

`Index.Add` tokenizes at index time to build postings. A tokenizer in `scorer/text` would make `engine` import `scorer/text`, breaking assertion 3 immediately.

Generally: "one write entry point" plus "no scorer keeps its own store" together determine where the tokenizer lives. Multilingual and morphological tokenizers will press on this, since they cannot all live in `engine`. The likely answer is injecting the tokenizer into `Add`, the same shape as `Fuser`.

### 2.3 Seeds must be excluded from graph results

Scoring `1/(1+hops)` puts seeds at 1.0, i.e. top. With seeds drawn from the text scorer, the graph stream's head becomes a copy of the text stream's head, and RRF counts one piece of evidence as two independent votes.

Measured on the `cmd/weft` corpus, query `ranking fusion`:

| | Graph stream | Overlap with text stream |
|---|---|---|
| Seeds included | tfidf, rrf, bm25, hnsw, ivf | top 2 identical to text's top 2, same order |
| Seeds excluded | bm25, hnsw, ivf | none |

`tfidf` shows it most clearly: rank 1 in text and rank 1 in graph, so two votes for one piece of evidence.

```
included   tfidf  0.03279 (2nd)   ← 1/61 + 1/61
excluded   tfidf  0.01639 (5th)   ← 1/61, exactly halved
```

`bm25`, `hnsw` and `ivf` are documents text never found, so after exclusion the graph scorer contributes only new information.

`graph.New` excludes seeds. `graph.NewIncludingSeeds` keeps the literal behaviour, because attributing an improvement requires running both variants over one query set.

### 2.4 The seed source is an interface, so scorers compose without naming each other

`graph.New(ix, seed engine.Scorer)`. The graph scorer does not know its seed is the text scorer, so `scorer/graph` does not import `scorer/text` and its tests seed traversal from a stub. Scorer-agnosticism holds between scorers, not only at the fusion layer.

---

## 3. Known costs

### 3.1 The top-k interface forecloses early termination

A scorer must evaluate all its candidates before returning k: the text scorer walks every matching posting, the vector scorer scans the whole corpus.

This is a direction-of-information problem, not a missing optimization. WAND-style skipping needs fusion to look up a per-term score ceiling *and* fusion's current threshold to reach the scorer so it can skip blocks below it. No path exists for a threshold to flow inward — the scorer computes everything internally, then hands results to fusion.

Fix by extension, not replacement:

```go
type Streamer interface {
    engine.Scorer
    Stream(ctx context.Context, q Query) (Cursor, error)
}

type Cursor interface {
    Advance(minDoc DocID) (Candidate, bool) // ordered doc id walk, skippable
    MaxScore() float64                      // remaining ceiling
}
```

Fusion would branch on `if s, ok := sc.(Streamer); ok`. That does not break the hypothesis: fusion learns a *capability* (streamable), not a *type* (text/vector/graph). The failure condition remains `switch scorer.Name()`.

Size of the cost is unmeasurable today — full scans are free on a small in-memory corpus. It first hurts at milestone 3, on corpora larger than memory.

### 3.2 RRF damping is stronger than expected

The contribution gap between rank 1 and rank 2 is `1/61 - 1/62 ≈ 0.00026`, so one scorer out of four must outweigh the other three agreeing to reverse an order. Adding recency changed scores but not order, which is why assertion 1 uses the `lonely` document instead of "the order changes". `k = 60` is a cited default, unverified here.

### 3.3 `engine.TopK` sorts rather than using a bounded heap

A heap is `O(n log k)` against `O(n log n)` and pays off only when candidate sets far exceed `k`, which nothing here measures. One shared deterministic selection path beats four hand-rolled ones; the upgrade path is marked in a `ponytail:` comment.

---

## 4. Carried into milestone 2

1. **Postings format** — settled in [D-001](DECISIONS.md): the cursor interface waits, the format goes block-structured immediately.
2. **Keep `Document.Links` keyed by document key.** Lazy resolution handles forward references and dangling edges for free (`TestForwardLinksResolve`, `TestDanglingLinksAreIgnored`). A `DocID` adjacency list introduces an indexing-order dependency, and [the recommended evaluation path](DATASETS.md) depends on joining an external citation graph by key, where many targets fall outside the corpus.
3. **Two places depend on `DocID` increasing densely** — the tiebreak in `engine.TopK`, and postings staying sorted because appends are monotonic. Deletion and segment merge break that invariant; design tombstones and generations first.
4. **Make BM25 collection statistics atomic per commit.** `N`, `avgdl` and `docLen` are collection-wide, so "one commit makes all scorers' data visible atomically" must include the statistics snapshot, or a query landing mid-commit produces inconsistent scores. Easy to atomize document visibility and forget the statistics.
5. **Evaluation dataset** — settled in [DATASETS.md](DATASETS.md): milestone 4 is viable and milestone 2's scope is unaffected.
6. **Run one round of community research.** Zero user interviews so far.

---

## 5. Open questions

| Question | Why it is open |
|---|---|
| Is `RRF k = 60` right for this domain? | Cited default, never measured here (§3.2). |
| Are `SeedN = 5` and "top n from text" good seeds? | Double counting is fixed (§2.3); seed quality is separate and unmeasured. |
| PageRank instead of BFS distance? | BFS was the simplest real proximity. A replacement candidate if quality falls short. |
| Does CJK tokenization matter? | `engine.Tokenize` collapses CJK runs into one token — a known wrong answer milestone 1 did not need to be right about. Same pressure point as §2.2. |
| Is multi-month solo development sustainable? | Milestone 1 finished well under estimate because of the in-memory and standard-library-only constraints. That says milestone 1 was easy and nothing more; persistence and segment merge are the real test. |
