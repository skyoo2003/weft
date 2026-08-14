# Milestone 1 — Scorer-agnostic fusion

**Verdict: the architecture hypothesis holds. 3/3 assertions pass.** Evidence: `pkg/engine/architecture_test.go`.

| Package | Implementation | Tests |
| --- | --- | --- |
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

**Assertion 2 — a new scorer is cheap.** `pkg/scorer/recency` is 99 implementation lines against a 100-line budget, and `fusion/` needed no change at all.

The engine side is not zero, and an earlier version of this document claimed it was. `Document.Time` exists only for the recency scorer and was written before that scorer existed, so the figure was flattered by pre-provisioning the field. Stated generally: a scorer needing new input data has to read it from `engine.Document`, because scorers may not keep their own store (§2.2). **The engine cost of a new input type is one field on `Document`.** A scorer reusing existing fields costs nothing there.

Two checks measure different things, and neither substitutes for the other:

- `engine/` and `fusion/` import no `scorer/*` package, checked with `go/parser`. This proves package-level ignorance and holds for every future scorer without needing a baseline commit, and it does not trip on the words "text" or "vector" in comments. It cannot detect a new `Document` field.
- `engine`'s exported API is recorded in `pkg/engine/testdata/engine_api.txt`, signatures and member types included, each declaration's members in the order they are written. That covers all three ways a scorer can widen the shared contract — a field on `Document`, a method on the `Scorer` interface, a parameter on `Search` or `Fuser` — so the engine cost of a new scorer is a visible edit rather than a silent one. Refresh with `WEFT_UPDATE_GOLDEN=1 go test ./pkg/engine/`.

  This assertion has been corrected three times, and what it now records is the result of separating two questions that look like one. **Does this change break a caller?** goes in the file. **Is this change visible in the source?** does not.

  | Correction | Was recorded | Why it was wrong |
  | --- | --- | --- |
  | Signatures and member types | Names only | An interface method is the most expensive change a scorer can force — every existing scorer stops compiling — and names alone could not see one. |
  | Order per declaration | Every line sorted together | Field order is what unkeyed composite literals resolve against. Swapping the same-typed `Document.Key` and `Document.Text` reverses their meaning in existing callers, compiles cleanly, and left the golden byte-identical. |
  | Types without parameter names | `ctx context.Context` | Go has no named arguments, so renaming a parameter breaks nothing. Recording the name made the assertion fail on a pure refactor and tell the author to write down an engine cost that does not exist. |
  | One `has unexported fields` marker per struct | Exported fields only | Go forbids an unkeyed composite literal from another package once a struct holds any unexported field, so `Document`'s first one breaks every external `engine.Document{k, t, v, l, ts}` with no name and no exported type to show for it. One marker, not a line per field: the second unexported field takes away nothing the first had not, and recording each would fail the assertion on every internal `Index` field — the mistake the row above names. |

  The parameter-name row trims coverage, which is the opposite of the others, and it has a price: swapping two adjacent parameters of the same type is now invisible here even though it changes meaning at every call site. No declaration in `engine` has such a pair today.

The line budget counts implementation files only — counting tests would reward untested scorers.

**Assertion 3 — fusion cannot see scorers.**

```text
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
| --- | --- | --- |
| Seeds included | tfidf, rrf, bm25, hnsw, ivf | top 2 identical to text's top 2, same order |
| Seeds excluded | bm25, hnsw, ivf | none |

`tfidf` shows it most clearly: rank 1 in text and rank 1 in graph, so two votes for one piece of evidence.

```text
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
| --- | --- |
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
| --- | --- |
| Is `RRF k = 60` right for this domain? | Cited default, never measured here (§3.2). |
| Are `SeedN = 5` and "top n from text" good seeds? | Double counting is fixed (§2.3); seed quality is separate and unmeasured. |
| PageRank instead of BFS distance? | BFS was the simplest real proximity. A replacement candidate if quality falls short. |
| Is harmonic decay the right shape for recency? | `1/(1 + age/HalfLife)` replaced `2^(-age/HalfLife)`, which underflowed to zero past ~88 years and let insertion order stand in for recency. Both orderings are identical wherever the exponential is representable, so the swap is rank-neutral and the fused demo output did not move — which also means nothing here measures which tail is better. Age is computed from the timestamps rather than with `Sub` for the same reason: a `time.Duration` saturates at ±292 years, which is the same tie one era further out. Each operand is then widened to `float64` before the subtraction, since Unix seconds span more than `int64` and a wrapped difference reads as a future date, which scores the oldest possible document 1.0. |
| Does CJK tokenization matter? | `engine.Tokenize` collapses CJK runs into one token — a known wrong answer milestone 1 did not need to be right about. Same pressure point as §2.2. |
| Is multi-month solo development sustainable? | Milestone 1 finished well under estimate because of the in-memory and standard-library-only constraints. That says milestone 1 was easy and nothing more; persistence and segment merge are the real test. |

---

<!-- Two top-level headings on purpose. This file is an append-only log of
     milestone reports, each its own document with its own verdict; demoting
     them under a single title would imply one report with sections, and a
     later milestone would then be filed under a conclusion it did not reach. -->
<!-- markdownlint-disable-next-line MD025 -->
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
| --- | --- |
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

---
---

# Milestone 4 — Quality

**Verdict: the graph signal does not improve ranking quality.** The PRD's second
falsification condition is met and answered *no*. Under equal-weight fusion it costs
0.1202 nDCG@10, and no fusion weight makes it worth anything: the best delta available
is exactly +0.0000. Measurement design and full numbers: [EVAL.md](EVAL.md).

**The larger finding is about fusion, not about graphs.** That −0.1202 was RRF's equal
vote, not the graph's information — halving the graph stream's weight erases all but
0.0019 of the regression (§7). Unweighted rank fusion makes a substantive ranking
decision silently on every query, and its cost here was two orders of magnitude larger
than anything the graph signal was ever worth.

| Arm | nDCG@10 |
|---|---|
| `text` | 0.5826 |
| `text+vector` | **0.6233** ← best |
| `text+graph` | 0.3987 |
| `text+vector+graph` | 0.5031 |
| `text+vector+graph-including-seeds` | 0.5464 |

| Comparison | Delta | 95% CI |
|---|---|---|
| `text+vector+graph` − `text+vector` | **−0.1202** | [−0.1521, −0.0886] |
| `text+vector` − `text` | +0.0407 | [+0.0010, +0.0798] |

TREC-COVID, 50 queries, 171,332 documents, 579,720 in-corpus citation edges, 148,232
SPECTER2 vectors. Paired bootstrap, 10,000 resamples, seed 20260814. 28-configuration
sweep over `RRFk` and over-fetch: **0 sign flips, negative throughout.**

---

## 1. Result

**Assertion 1 — the metric agreed with the outside world before any arm was run.**
nDCG matches `pytrec_eval`'s `ndcg_cut_10` on 12 fixtures built to discriminate; BM25
matches `rank_bm25` to 4.44e-16 once the IDF form is explicitly aligned. The second
closes the PRD Success Metrics row "correctness floor", which no milestone had claimed
until now.

That check paid for itself immediately: **the plan's nDCG definition was wrong.** It
specified exponential gain `2^rel − 1` on the stated grounds that this matched what
BEIR reports. `trec_eval` uses linear gain. On qrels `{a:2, b:1}` ranked `[b, a]` the
two give 0.8597 and 0.7967, and every ranking that is already ideal scores 1.0 under
both — which is why it needed a fixture designed to separate them rather than a happy
path. Publishing on a scale nobody else uses would have made every number here
incomparable to the literature it was meant to be read against.

**Assertion 2 — the harness does not know what a graph scorer is.** An `eval.Arm` is a
name, a `[]engine.Scorer`, an `engine.Fuser` and a depth. `Evaluate` branches on none
of them; five arms differ only in the contents of a slice. The milestone 1 claim holds
one level up from the engine, which is where it would have been cheapest to quietly
break.

**Assertion 3 — the graph signal regresses, robustly.** −0.1202 against the
pre-registered baseline, CI far from zero, sign stable across 28 configurations
including over-fetch to depth 100 and rank constants from 1 to 200.

## 2. What the architecture bought, in numbers

**Two sweeps needed no library change.** Varying the RRF rank constant is a local
`engine.Fuser` passed to `Search`; `pkg/fusion` is untouched. Over-fetching turned out
to need nothing at all — `Fuse` scores a document from its ranks alone and passes `k`
only to `TopK`, so `Fuse(streams, k*m)[:k]` equals `Fuse(streams, k)`, asserted across
k ∈ [1,5] and m ∈ {2,3,10}. The `ponytail:` marker at `search.go:112` that named
milestone 4 as its repayment trigger is **withdrawn rather than repaid**: the ceiling
it described was reachable from outside all along ([D-004](DECISIONS.md)).

**One check the engine cannot make became free.** `Search` documents an unchecked
precondition — every scorer must read the same index, because `DocID` is
index-relative (milestone 1 §3.4) — and checking it there would need a method asking a
scorer which index it holds, the one change that breaks every implementation. The
harness resolves every fused `DocID` to a key anyway, so it gets the check for nothing
and returns `ErrForeignDocID`.

**Milestone 2 was exercised on a real corpus for the first time.** 171,332 documents
committed in 2.2 s, reopened in 979 ms, document count and average length matching.
Until now restore equivalence had only run on fixtures.

**And the honest counterweight: none of that made the fourth signal *good*.** Adding a
signal is cheap to wire — milestone 1 proved it and this harness re-proved it at the
evaluation layer. Wiring is not quality. The PRD's hypothesis is about the cost of
*adding* a signal and remains true as stated; this milestone is the reminder that a
cheap-to-add signal can still be worth less than nothing.

## 3. Why the graph signal failed, mechanically

`1/(1+hops)` with `MaxDepth = 3` gives a non-seed candidate three possible values. On
this corpus the hop-1 frontier averages 41 documents per query, so the whole top ten
sat at 0.5 and `engine.TopK`'s tiebreak — `DocID`, i.e. corpus insertion order — chose
which ten. **Every one of the 45 queries the graph could answer at all was ranking by
an accident of indexing**, 2,082 slots of it.

The plan predicted this as a High risk and named the minimal fix in advance: sum
per-seed distances instead of taking the nearest, so documents several seeds agree on
rise. Implemented, tested, and measured before and after as the plan required.

**It did not work.** Only 28.1% of documents have an in-corpus out-edge, so two seeds
almost never cite the same paper and the sum almost always has one non-zero term. 3
distinct scores per query is still the modal case, 41 of 45 answering queries still
have their stream's membership decided by `DocID`, and the arm moved +0.037 — an order
of magnitude short of the 0.12 it needed.

So the stream carries almost no ordering, and unweighted RRF gives its arbitrary top
ten the same vote as BM25's. Query 40 falls from a perfect 1.0000 to 0.6321 and query
24 from 0.9149 to 0.4819. This is not dilution, it is displacement.

**The double-counting control earned its place twice.** At 5% graph coverage, with a
traversal returning literally nothing, `including-seeds` showed +0.1021 with a CI
excluding zero — an "improvement" that was purely text getting a second vote. Without
that arm in the table it would have read as graph proximity working. Milestone 1 §2.3
predicted the inflation; this is what it looks like. Post-fix it regresses too, at
−0.0769, which rules out the harness being rigged against the traversal.

## 4. The methodological failures worth recording

Two, both caught after publication, both the same shape: a statistic answering the
question it was asked while the thing that actually moved the number sat outside its
scope.

### 4.1 A confident interval on an incomplete corpus

[EVAL.md](EVAL.md) section 4.1 documents a finding this milestone published to itself
and then withdrew. At 27% vector coverage, `text+vector` measured 0.3200 against `text`
at 0.5826, and that was written up as a substantive result about unweighted rank
fusion — with a 95% interval of [−0.3058, −0.2178]. Narrow, nowhere near zero, and
completely wrong. At 86.5% coverage the same comparison is **+0.0407**.

The bootstrap was not broken. It quantifies sampling noise across queries, which is all
it claims to do, and it has nothing to say about whether the corpus is complete. Two
reference implementations were wired in specifically to stop us trusting an unverified
metric, and the same class of error landed one level out anyway — in the data rather
than the instrument. Every arm number now carries the coverage it was measured at.

### 4.2 A reproducible measurement on a non-reproducible build

Found by review of the milestone's own pull request, after the numbers were published.

`weft-eval build` inverted the Semantic Scholar cache into `CorpusId → cord_uid` by
ranging over a Go map. The mapping is not injective — CORD-19 ships the same paper
under several `cord_uid`s, and **20,556 of 162,837 records with a CorpusId collide** —
so randomised map iteration chose a different winner on every build. Two builds from
the identical cache disagreed on **2,571 to 9,377 of 142,281 CorpusIds**, up to 6.6% of
the citation graph. The edge *count* was identical every time, 579,720, which is why
the build log looked stable and nothing downstream noticed.

Everything guarding this measurement was pointed elsewhere. The bootstrap resamples
queries against one index. The seed is pinned so the *resampling* reproduces. The
28-configuration sweep varies fusion, not the corpus. `make eval` reprints the numbers
faithfully — from whichever graph the last build happened to produce. A pipeline that
is nondeterministic upstream is invisible to all four.

The verdict survived: −0.1156 became −0.1202, same sign, interval still far from zero,
still 0 sign flips. What did not survive was §7's headline. The graph's best case under
any fusion weight was published as +0.0018 and is **+0.0000** — a figure smaller than
the run-to-run spread of the graph it was measured on, and the one number in this
document a reader might have taken as a reason to keep the scorer.

The fix is four lines: iterate in sorted key order, keep the first, print the collision
count. The check that would have caught it is cheaper still — build twice, compare the
bytes — and is now in the repository as a unit test on `corpusIDIndex` and as a command
in [EVAL.md](EVAL.md) section 7. **A harness whose purpose is reproducibility had never
been asked to reproduce anything.**

## 5. Known costs

### 5.1 The verdict is about one construction, not about graphs

Falsified: BFS hop distance, seeded from the text top 5, fused by unweighted RRF, over
a citation graph where 74.5% of references dangle. Not falsified: that graph structure
carries ranking signal. A continuous score (personalised PageRank, random-walk
probability — milestone 1 §5) or a fusion operator with per-stream weights would each
attack a different part of the mechanism in §3, and neither was in scope.

This distinction is load-bearing for what happens next, and it is also the most
convenient thing this document could say — which is why the evidence for it is stated
as mechanism rather than as hope: on most queries the stream demonstrably carries 3
distinct scores across thousands of candidates.

### 5.2 Unweighted fusion has no way to discount a weak stream — measured, see §7

RRF reads ranks and nothing else, deliberately: knowing a stream's reliability means
knowing which scorer produced it, which is the coupling milestone 1 exists to prevent.
The cost is now measured, and it turned out to be the largest effect in this
milestone. §7 has the numbers.

### 5.3 Judgment bias points toward this verdict

Unjudged documents count as grade 0 *and* consume rank slots, so a signal whose purpose
is surfacing documents assessors never saw is structurally penalised. TREC-COVID's
493.5 judgments per query was chosen to mitigate this and does not eliminate it. The
direction is unfavourable to the graph and the verdict is negative, so this cannot be
used to defend the number — but a future positive result on a shallower dataset would
have to account for it.

### 5.4 `MaxDepth` and `SeedN` were never swept

The sweep covered `RRFk` and over-fetch. `SeedN=5` and `MaxDepth=3` stayed frozen, and
turning them into `New` parameters was deferred rather than done. Given the frontier is
already too wide at depth 1, widening it further is not the obvious remedy — but it is
unmeasured, and this is where that is recorded.

## 6. Carried forward

1. **`fusion.FuseWeighted` shipped** — per-stream weights indexed by position, with
   `Fuse` unchanged and bit-identical on its unweighted path. This milestone's largest
   measured effect, repaid into the library rather than left as a note (§7).
2. **`pkg/scorer/graph` is kept, marked, and not deleted.** The verdict says the signal
   is worthless and the PRD says worthless signals go; §7 then showed the scorer is
   inert rather than harmful, and that the harm belonged to fusion. Deleting it would
   also cut the milestone 1 assertions from four signals to three, which the PRD did
   not price. Its package doc now opens with the measurement and the instruction to
   weight it down. The full argument, including the case against this choice, is
   [D-005](DECISIONS.md).
3. **`internal/eval` outlives the graph.** Any future signal inherits a harness, a
   verified metric, a judgment rule fixed in advance and committed reference goldens.
   That is the durable output.
4. **`search.go:112`'s over-fetch marker is withdrawn**, not repaid (§2).

## 7. Weighted fusion — the thing this milestone actually found

Sections 3 and 5.10 of [EVAL.md](EVAL.md) rule out the rank constant and fusion depth
as explanations for the graph arm's regression. Both change how ranks are damped, not
how much each stream counts. So the equal vote itself was tested: a `Fuser` variant
multiplying each stream by a weight, text and vector held at 1.0, only the graph
stream moving.

| Graph stream weight | nDCG@10 | Delta vs `text+vector` | 95% CI |
|---|---|---|---|
| 1.0 | 0.5031 | **−0.1202** | [−0.1521, −0.0886] |
| 0.5 | 0.6214 | −0.0019 | [−0.0057, +0.0000] |
| 0.25 | 0.6214 | −0.0019 | [−0.0057, +0.0000] |
| ≤ 0.1 | 0.6233 | +0.0000 | [+0.0000, +0.0000] — converged to baseline |

**Halving one weight erased 0.118 of a 0.120 regression.** The graph stream was never
destroying rankings; RRF was giving it ten slots it had not earned. Equal weighting is
not a neutral default — it is a ranking decision made silently on every query, and on
this corpus it was worth two orders of magnitude more than the signal being evaluated.

**Weights do not compromise scorer-agnosticism.** They index by *position in the
stream list*, and the caller already fixed that order when it passed scorers to
`Search`. The fuser still never learns what produced a stream. `pkg/fusion` was not
touched to run this; the variant lives in `cmd/weft-eval` and is injected as an
`engine.Fuser`, the same mechanism the RRF-constant sweep uses.

**It does not rescue the graph.** No weight beats the baseline. From 0.1 downward the
arm *is* the baseline — delta exactly zero, interval a point at zero — meaning the
graph stream is being fused and changes no ranking any query is scored on.
Down-weighting a near-noise stream stops it doing harm; it does not make it
informative. The verdict in §1 stands, with its reason corrected: not "graph proximity
is harmful" but "graph proximity, as constructed here, is not information."

An earlier revision of this table reported +0.0018 at weight 0.1 and called it the
graph's best case. That number came from a build whose citation graph varied between
runs (§4.2) and was smaller than that variation. It is now +0.0000.

**Shipped as `fusion.FuseWeighted`.** Weights are variadic and positional, `Fuse` is
unchanged, and the unweighted path is bit-identical — multiplying by 1.0 is exact, so
no ranking pinned by the milestone 1 or 2 tests moved. Weight 0 removes a stream
entirely rather than leaving its documents at score 0 holding ranks they did not
earn; that was a real bug, caught by the test written for it.

**The open question it leaves.** Where should weights come from? Hand-tuning per
corpus reintroduces exactly the per-deployment burden a scorer-agnostic design exists
to avoid, and that is the strongest objection to this API existing at all. Learning
them from relevance judgments is a different project. `FuseWeighted`'s documentation
says plainly that a caller with no measurement of its own should use `Fuse`, which is
the honest position until one of those is settled.
