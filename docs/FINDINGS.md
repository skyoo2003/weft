# Milestone 1 — Scorer-agnostic fusion

**Verdict: the architecture hypothesis holds. 3/3 assertions pass.** Evidence: `pkg/engine/architecture_test.go`.

| Package | Implementation | Tests |
|---|---|---|
| `pkg/engine` | 527 | 1076 |
| `pkg/fusion` | 76 | 174 |
| `pkg/scorer/text` | 141 | 296 |
| `pkg/scorer/vector` | 134 | 250 |
| `pkg/scorer/graph` | 208 | 423 |
| `pkg/scorer/recency` | **99** | 238 |

1,185 implementation lines, 2,457 test lines, zero external dependencies.

---

## 1. Result

**Assertion 1 — fusion is invariant to scorer count.** Three and four scorers use the same call expression; compiling is the proof.

```go
engine.Search(ctx, q, 5, fusion.Fuse, three...)  // text, vector, graph
engine.Search(ctx, q, 5, fusion.Fuse, four...)   // + recency
```

Compiling alone proved insufficient — a scorer returning nothing passes it too. The corpus therefore holds a document (`lonely`) that matches no query term, carries no vector and is linked from nowhere, so only recency sees it. Three scorers must not surface it; four must.

**Assertion 2 — a new scorer is cheap.** `pkg/scorer/recency` is 93 implementation lines against a 100-line budget, and `fusion/` needed no change at all.

The engine side is not zero, and an earlier version of this document claimed it was. `Document.Time` exists only for the recency scorer and was written before that scorer existed, so the figure was flattered by pre-provisioning the field. Stated generally: a scorer needing new input data has to read it from `engine.Document`, because scorers may not keep their own store (§2.2). **The engine cost of a new input type is one field on `Document`.** A scorer reusing existing fields costs nothing there.

Two checks measure different things, and neither substitutes for the other:

- `engine/` and `fusion/` import no `scorer/*` package, checked with `go/parser`. This proves package-level ignorance and holds for every future scorer without needing a baseline commit, and it does not trip on the words "text" or "vector" in comments. It cannot detect a new `Document` field.
- `engine`'s exported API is recorded in `pkg/engine/testdata/engine_api.txt`, signatures and member types included, each declaration's members in the order they are written. That covers all three ways a scorer can widen the shared contract — a field on `Document`, a method on the `Scorer` interface, a parameter on `Search` or `Fuser` — so the engine cost of a new scorer is a visible edit rather than a silent one. Refresh with `WEFT_UPDATE_GOLDEN=1 go test ./pkg/engine/`.

  This assertion has been corrected three times, and what it now records is the result of separating two questions that look like one. **Does this change break a caller?** goes in the file. **Is this change visible in the source?** does not.

  | Correction | Was recorded | Why it was wrong |
  |---|---|---|
  | Signatures and member types | Names only | An interface method is the most expensive change a scorer can force — every existing scorer stops compiling — and names alone could not see one. |
  | Order per declaration | Every line sorted together | Field order is what unkeyed composite literals resolve against. Swapping the same-typed `Document.Key` and `Document.Text` reverses their meaning in existing callers, compiles cleanly, and left the golden byte-identical. |
  | Types without parameter names | `ctx context.Context` | Go has no named arguments, so renaming a parameter breaks nothing. Recording the name made the assertion fail on a pure refactor and tell the author to write down an engine cost that does not exist. |
  | One `has unexported fields` marker per struct | Exported fields only | Go forbids an unkeyed composite literal from another package once a struct holds any unexported field, so `Document`'s first one breaks every external `engine.Document{k, t, v, l, ts}` with no name and no exported type to show for it. One marker, not a line per field: the second unexported field takes away nothing the first had not, and recording each would fail the assertion on every internal `Index` field — the mistake the row above names. |

  The parameter-name row trims coverage, which is the opposite of the others, and it has a price: swapping two adjacent parameters of the same type is now invisible here even though it changes meaning at every call site. No declaration in `engine` has such a pair today.

The line budget counts implementation files only — counting tests would reward untested scorers.

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

### 3.4 A DocID is meaningful only inside the index that assigned it

`Index.Add` hands out dense IDs from 0, so two indexes give the same `DocID` to different documents and the value carries nothing that says which index it came from. `Search` therefore requires every scorer to read one index. Given scorers built against two, RRF reads the collision as two scorers agreeing on one document, and the winning IDs resolve against neither corpus — a silent wrong answer, not an error.

This is a documented precondition rather than a check, because every way to check it costs more than it returns:

| Enforcement | What it costs |
|---|---|
| `Index()` on `Scorer` | Breaks every existing implementation, and a scorer computing purely from `Query` has no answer to give. |
| Optional `interface{ Index() *Index }` | Capability-not-type, so it fits §3.1's shape, but it only sees scorers that opt in and misses the nested case: `graph.New(ix1, seedOverIx2)` holds its seed privately, so that mix never reaches `Search`. |
| Index identity on `Candidate` | Widens the type every scorer and every `Fuser` touches, and makes fusion compare something other than rank. |

The general fix is for `DocID` to carry its namespace, which milestone 2 needs anyway: §4.3 has deletion and segment merge breaking the same density assumption from the other direction.

---

## 4. Carried into milestone 2

1. **Postings format** — settled in [D-001](DECISIONS.md): the cursor interface waits, the format goes block-structured immediately.
2. **Keep `Document.Links` keyed by document key.** Lazy resolution handles forward references and dangling edges for free (`TestForwardLinksResolve`, `TestDanglingLinksAreIgnored`). A `DocID` adjacency list introduces an indexing-order dependency, and [the recommended evaluation path](DATASETS.md) depends on joining an external citation graph by key, where many targets fall outside the corpus.
3. **Two places depend on `DocID` increasing densely** — the tiebreak in `engine.TopK`, and postings staying sorted because appends are monotonic. Deletion and segment merge break that invariant; design tombstones and generations first.
4. **Make BM25 collection statistics atomic per commit.** `N`, `avgdl` and `docLen` are collection-wide, so "one commit makes all scorers' data visible atomically" must include the statistics snapshot, or a query landing mid-commit produces inconsistent scores. Easy to atomize document visibility and forget the statistics.
5. **Evaluation dataset** — settled in [DATASETS.md](DATASETS.md): milestone 4 is viable and milestone 2's scope is unaffected.
6. **Community research: one round done, desk-only** — [RESEARCH.md](RESEARCH.md). The bleve-closedness assumption is verified at source level (fusion is kind-closed: `1 FTS + N kNN` streams). The strongest counter-finding: embeddable Lucene already has open N-signal ranking, so the gap weft fills is Go-specific, not capability-first. User interviews remain zero.

---

## 5. Open questions

| Question | Why it is open |
|---|---|
| Is `RRF k = 60` right for this domain? | Cited default, never measured here (§3.2). |
| Are `SeedN = 5` and "top n from text" good seeds? | Double counting is fixed (§2.3); seed quality is separate and unmeasured. |
| PageRank instead of BFS distance? | BFS was the simplest real proximity. A replacement candidate if quality falls short. |
| Is harmonic decay the right shape for recency? | `1/(1 + age/HalfLife)` replaced `2^(-age/HalfLife)`, which underflowed to zero past ~88 years and let insertion order stand in for recency. Both orderings are identical wherever the exponential is representable, so the swap is rank-neutral and the fused demo output did not move — which also means nothing here measures which tail is better. Age is computed from the timestamps rather than with `Sub` for the same reason: a `time.Duration` saturates at ±292 years, which is the same tie one era further out. Each operand is then widened to `float64` before the subtraction, since Unix seconds span more than `int64` and a wrapped difference reads as a future date, which scores the oldest possible document 1.0. |
| Does CJK tokenization matter? | `engine.Tokenize` collapses CJK runs into one token — a known wrong answer milestone 1 did not need to be right about. Same pressure point as §2.2. |
| Is multi-month solo development sustainable? | Milestone 1 finished well under estimate because of the in-memory and standard-library-only constraints. That says milestone 1 was easy and nothing more; persistence and segment merge are the real test. |

---

# Milestone 2 — Persistence

**Verdict: the pass lines hold.** An index restored from disk is
indistinguishable from the index that was committed, and a commit is atomic
against process death. Evidence: `pkg/engine/restore_test.go`,
`persist_test.go`, `segment_test.go`; the format spec is
[FORMAT.md](FORMAT.md).

## 1. Result

**Restore equivalence.** Four scorers plus `fusion.Fuse` produce bit-identical
rankings — same documents, same order, same `float64` scores — before a
`Commit` and after an `Open`, across a query set covering text-only, hybrid,
seeded, empty-result and out-of-range-k queries (`TestRestoredIndexRanksIdentically`).
Exact score equality is the strong form of the claim: it holds only because
postings order, document lengths and collection statistics survived the disk
exactly.

**Commit atomicity.** A segment written but never named by a MANIFEST is
indistinguishable from one that never existed, and gets swept
(`TestUnmanifestedSegmentIsInvisible`). Commit refuses to write over a corrupt
manifest rather than guess a generation. The commit point is one rename.

**The D-001 rot check became structural.** D-001 required tests to verify the
unread block metadata; the decoder now re-derives `maxDocID`, `maxTF` and
`minDocLen` from every block's contents on every `Open` and refuses the file
on disagreement — rot cannot wait for a test run. On top sit a lying-file
matrix (twenty checksum-valid files each violating one semantic rule),
exhaustive byte-flip and truncation sweeps over every file, and fuzzing over
every decoder (2M+ executions, zero panics).

**The architecture was not touched.** `pkg/scorer/*` and `pkg/fusion/*` diff
against main is zero lines; `make arch` stays green; external dependencies
stay zero. The engine's exported API grew by exactly four names — `Commit`,
`Open`, `ErrCorrupt`, `ErrBadVersion` — all recorded in the golden file as
assertion 2 intended: a visible edit, not a silent one.

## 2. What §4 asked for, and what it got

| §4 item | Disposition |
|---|---|
| 1. Postings format per D-001 | Done. Blocks of ≤128 with the metadata triple, delta-encoded, blocks independently decodable. |
| 2. `Links` keyed by document key | Done. Keys on disk, never DocIDs; dangling links survive restore unresolved. |
| 3. DocID density vs deletion/merge | **Designed, deliberately not built.** The manifest carries a generation number and a segment *list*; that is the place tombstones and multi-segment state will live. No Delete API exists, so density is never violated in v1. |
| 4. BM25 statistics atomic per commit | Done. The whole segment is encoded under one read lock, and `Open` cross-checks meta against the documents it describes. |
| 6. Community research | Done before this milestone — [RESEARCH.md](RESEARCH.md). |

**§3.4 (DocID namespace) is deferred to milestone 3, on purpose.** A v1 store
is one segment rewritten wholesale per commit, so there is no second namespace
for a DocID to collide with. Multi-segment reading is what forces the issue,
and it arrives together with deletion and merge — solving it now would mean
designing against a guess, the same error D-001 declined.

## 3. Known costs

### 3.1 A commit rewrites the whole corpus

O(corpus) per `Commit`, marked with a `ponytail:` comment on the method. The
repayment trigger is milestone 3, where corpus size is the point; the manifest
being a list means incremental segments change the format's contents, not the
format.

### 3.2 Every Open verifies everything

Decoder verification is O(index) per `Open`. Free today — `Open` is eager and
already reads every byte — but milestone 3's lazy loader cannot verify what it
does not load. The checks will have to move: per-block on first touch, or into
an explicit scrub. Leaving them out is not an option; they are what makes the
D-001 metadata trustworthy at milestone 5.

### 3.3 The 6-byte header is part of the format

The terms index records absolute file offsets, so the frame header's size is
load-bearing. Cheap while the version fits one varint byte (through 127);
version 128 would be a format change anyway.

## 4. Carried into milestone 3

1. **Multi-segment reading, DocID namespacing (§3.4) and tombstones travel
   together.** They are one design problem — a global DocID becomes
   (segment, local id) the moment two segments are live — and the manifest's
   segment list is where it starts.
2. **Re-house the decoder's verification before lazy loading.** See §3.2.
3. **The fsync boundary is declared, not proven.** FORMAT.md scopes power-loss
   durability to best-effort. If a milestone ever claims more, it owes a
   torn-write test harness, not a stronger sentence.
4. Successive commits churn the whole directory (write new generation, delete
   old). Fine at in-memory scale; incremental segments make it moot.
