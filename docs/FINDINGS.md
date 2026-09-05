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

The engine side is not zero, and an earlier version of this document claimed it was. `Document.Time` exists only for the recency scorer and was written before that scorer existed, so the figure was flattered by pre-provisioning the field. Stated generally: a scorer *in this module* needing new input data has to read it from `engine.Document`, because scorers here may not keep their own store (§2.2). **The engine cost of a new input type is one field on `Document`.** A scorer reusing existing fields costs nothing there. This rule is about scorers inside `pkg/`; a scorer written outside the module cannot add a field and does not have to — milestone 6 §3 records the caller-held table joined through `Index.Resolve` as the supported path, at the cost that `Commit` does not carry it.

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

Measured on the `cmd/weft` corpus, query `ranking fusion`, fusing every stream at
an equal vote — which is `fusion.Fuse`, and is what the demo used at the time. The
demo now discounts the graph stream to 0.1 (milestone 6), so re-running it does not
reproduce the "included" figure below; `fusion.Fuse` and
`graph.NewIncludingSeeds` do.

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

<!-- More than one top-level heading on purpose. This file is an append-only log
     of milestone reports, each its own document with its own verdict; demoting
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

<!-- markdownlint-disable-next-line MD025 -->
# Milestone 4 — Quality

**Verdict: the graph signal does not improve ranking quality.** The PRD's second
falsification condition is met and answered *no*. Under equal-weight fusion it costs
0.1227 nDCG@10, and no fusion weight in the tested grid makes it worth anything: the best delta available
is exactly +0.0000. Measurement design and full numbers: [EVAL.md](EVAL.md).

**The larger finding is about fusion, not about graphs.** That −0.1227 was RRF's equal
vote, not the graph's information — halving the graph stream's weight erases all but
0.0019 of the regression (§7). Unweighted rank fusion makes a substantive ranking
decision silently on every query, and its cost here was two orders of magnitude larger
than anything the graph signal was ever worth.

| Arm | nDCG@10 |
| --- | --- |
| `text` | 0.5826 |
| `text+vector` | **0.6233** ← best |
| `text+graph` | 0.3985 |
| `text+vector+graph` | 0.5005 |
| `text+vector+graph-including-seeds` | 0.5451 |

| Comparison | Delta | 95% CI |
| --- | --- | --- |
| `text+vector+graph` − `text+vector` | **−0.1227** | [−0.1550, −0.0909] |
| `text+vector` − `text` | +0.0407 | [+0.0010, +0.0798] |

TREC-COVID, 50 queries, 171,332 documents, 579,719 in-corpus citation edges, 148,232
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

**Assertion 3 — the graph signal regresses, robustly.** −0.1227 against the
pre-registered baseline, CI far from zero, sign stable across 28 configurations of that
same pair — over-fetch to depth 100, rank constants from 1 to 200 — with the interval
excluding zero in every one of them.

## 2. What the architecture bought, in numbers

**Two sweeps needed no library change.** Varying the RRF rank constant is a local
`engine.Fuser` passed to `Search`; `pkg/fusion` is untouched. Over-fetching turned out
to need nothing at all — `Fuse` scores a document from its ranks alone and passes `k`
only to `TopK`, so `Fuse(streams, k*m)[:k]` equals `Fuse(streams, k)`, asserted across
k ∈ [1,5] and m ∈ {2,3,10}. The `ponytail:` marker at `search.go:112` that named
milestone 4 as its repayment trigger is **withdrawn rather than repaid**: the ceiling
it described was reachable from outside all along ([D-004](DECISIONS.md)).

**Half of one check the engine cannot make became free.** `Search` documents an
unchecked precondition — every scorer must read the same index, because `DocID` is
index-relative (milestone 1 §3.4) — and checking it there would need a method asking a
scorer which index it holds, the one change that breaks every implementation. The
harness resolves every fused `DocID` to a key anyway, so it gets a bound check for
nothing and returns `ErrForeignDocID`. A bound check only: IDs are dense from zero, so a
foreign index of similar size returns IDs that resolve here to unrelated documents and
produce a plausible nDCG over the wrong keys. §3.4 stays open; this narrows it.

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

**A later round of the same review moved it again, by one edge.** A reference naming
the same `CorpusId` twice, or resolving back to the citing document, was written as two
links and counted as two edges; the traversal walked neither. Deduplicating them
removes exactly **one** edge from this snapshot — 579,720 becomes 579,719 — and the
binding delta moves from −0.1202 to **−0.1227**. That a single adjacency is worth
0.0025 nDCG is §5's degeneracy seen from the other side: 241 of the reported slots are
held at a cut score 960 further candidates are excluded from by `DocID` alone, so one
changed edge re-decides a whole tie group. The verdict is robust and
the third decimal of a graph arm is not; [EVAL.md](EVAL.md) section 5.13 carries the
full re-measurement.

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
4. **`engine.Search`'s over-fetch marker is withdrawn**, not repaid (§2).

## 7. Weighted fusion — the thing this milestone actually found

Sections 3 and 5.10 of [EVAL.md](EVAL.md) rule out the rank constant and fusion depth
as explanations for the graph arm's regression — 28 configurations of each, on the
binding pair, no sign flip and no interval reaching zero. Both change how ranks are
damped, not how much each stream counts; depth narrows the gap to 0.0218 at its closest
and does it by lifting the baseline as much as the graph arm. So the equal vote itself was tested: a `Fuser` variant
multiplying each stream by a weight, text and vector held at 1.0, only the graph
stream moving.

| Graph stream weight | nDCG@10 | Delta vs `text+vector` | 95% CI |
| --- | --- | --- | --- |
| 1.0 | 0.5005 | **−0.1227** | [−0.1550, −0.0909] |
| 0.5 | 0.6214 | −0.0019 | [−0.0057, +0.0000] |
| 0.25 | 0.6214 | −0.0019 | [−0.0057, +0.0000] |
| ≤ 0.1 | 0.6233 | +0.0000 | [+0.0000, +0.0000] — converged to baseline |

**Halving one weight erased 0.118 of a 0.120 regression.** The graph stream was never
destroying rankings; RRF was giving it ten slots it had not earned. Equal weighting is
not a neutral default — it is a ranking decision made silently on every query, and on
this corpus it was worth two orders of magnitude more than the signal being evaluated.

**Weights do not compromise scorer-agnosticism.** They index by *position in the
stream list*, and the caller already fixed that order when it passed scorers to
`Search`. The fuser still never learns what produced a stream. The first pass was run
without touching `pkg/fusion` at all — a weighted variant local to `cmd/weft-eval`,
injected as an `engine.Fuser`, the same mechanism the RRF-constant sweep uses — which
is what established that the library needed no change to answer the question. The
table above is not from that copy. Once the result was worth publishing, the variant
moved into `pkg/fusion` as `FuseWeighted` and `weft-eval weights` now calls it, so the
reproducible command and the shipped API are the same code.

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

---

<!-- markdownlint-disable-next-line MD025 -->
# Milestone 3 — Scale

**Verdict: storage is lazy and the read API did not move; the corpus is not
resident-free.** Four of the five pass lines hold. The fifth — that a corpus
larger than memory is workable — is true of the text and graph paths and
false of the vector path, for a reason arithmetic settles rather than
engineering. Evidence: `pkg/engine/lazy_test.go`, `formatv2_test.go`,
`segment_test.go`; the format is [FORMAT.md](FORMAT.md), the decisions are
[D-006](DECISIONS.md) and [D-007](DECISIONS.md).

| Pass line | Result |
| --- | --- |
| Lazy ranks identically to eager | **holds.** All five milestone 4 arms reproduce to four decimals |
| Heap does not scale with the corpus | **holds.** 74,504 bytes at 250 documents, 74,504 at 2,000 |
| Commit cost is bounded by the addition | **holds.** One document onto a 7.2 MB corpus writes 245 bytes |
| Segment bytes are deterministic | **holds.** Commit and merge both |
| `pkg/scorer` and `pkg/fusion` unchanged | **holds.** 0 lines |

## 1. Result

**The real corpus reproduces milestone 4 exactly.** The evaluation index was
rebuilt at format v2 and `make eval` re-run against it:

| Arm | Milestone 4 | Milestone 3 |
| --- | --- | --- |
| `text` | 0.5826 | 0.5826 |
| `text+vector` | 0.6233 | 0.6233 |
| `text+graph` | 0.3985 | 0.3985 |
| `text+vector+graph` | 0.5005 | 0.5005 |
| `text+vector+graph-including-seeds` | 0.5451 | 0.5451 |

Both binding deltas carry their intervals across unchanged — `−0.1227`
`[−0.1550, −0.0909]` and `+0.0407` `[+0.0010, +0.0798]` — and so do the largest
per-query moves, query 24 at `0.9149 → 0.4819` and query 40 at `1.0000 →
0.6321`. 171,332 documents, avgdl 169.4, 579,719 in-corpus edges: the same
corpus, read a different way, ranked identically.

**Opening it costs 54 ms.** Milestone 4 measured 979 ms for the same directory.

**The six read methods kept their signatures.** `engine`'s exported API grew by
three names — `Scrub`, `Close`, `Merge` — and no existing one moved. That is the
milestone 1 hypothesis surviving contact with storage, and the golden file is
what makes it a measurement rather than a claim.

## 2. What the numbers are

| Measurement | Before | After |
| --- | --- | --- |
| `Open` allocation, 7.2 MB segment | the corpus | 154,696 B (2.1%) |
| `Commit` allocation, 7.2 MB segment | 51,453,656 B (585%) | 695,536 B (9.6%) |
| Commit after one `Add`, 7.2 MB corpus | the corpus | 245 B |
| Heap after `Open`, 250 → 2,000 documents | tracks the corpus | 74,504 → 74,504 B |

Two of those were found by writing the test rather than by reasoning about the
design. The writer's 51 MB was mostly not the buffer it was rewritten to remove
— 35 MB of it was a ten-byte varint scratch array escaping to the heap on every
posting, twice. And making the pending index satisfy the merge's source
interface put 9 MB straight back, by allocating a translated posting list per
term.

## 3. The arithmetic the milestone does not beat

`mmap` moves a corpus out of the Go heap and into the page cache. **That is a
different accounting, not a smaller working set**, and the flat heap number
above says exactly that and nothing more.

On the evaluation index, of a 656 MB `docs` file roughly 434 MiB — 69% — is
vectors: 148,232 documents at 768 dimensions. `scorer/vector` scans every one of
them on every query. So every page of that 434 MiB is touched per query, before
and after this milestone, and the heap assertion passes while the machine needs
the memory it always needed.

The text and graph paths are genuinely lazy: postings are decoded per term,
O(df) and not O(corpus), and links per document. The vector path is the
exception and it is the majority of the bytes.

**This is why the milestone's outcome sentence is only half true.** "Works on a
corpus larger than memory" holds for a corpus without vectors and does not hold
for one with them. Removing the scan is an approximate index — planned as task 7,
not built — and until it exists this is the honest statement of where the
milestone stands.

## 4. Known costs

### 4.1 Corruption and absence are one answer

`Doc` returns `(Document, bool)`. A record that fails its checksum reports the id
as absent, because the alternative is an error return on all six read methods —
the one change that reaches every scorer, and the change this milestone exists to
avoid making. What still holds is pinned: never a wrong document, never a panic,
neighbouring documents untouched, and `Scrub` names the damage. [D-006](DECISIONS.md).

`Lookup` answers the same way, and it did not at first — the sentence above was
written before the code kept it. A term's offset is the one value on the lazy
path that nothing re-derives. `decodePostings` can refuse a bad one because it
walks the postings file in step with the terms file and knows where each entry
belongs; `decodeTermIndex` cannot, because not walking is precisely what makes
`Open` lazy. Nothing replaced the check at the point of use, so an offset below
the frame header indexed a slice negatively and `Index.Lookup` panicked on a
directory whose every checksum verified. That the checksums verified is the
point: CRC32C is an integrity code, not a signature, and this package parses
files it did not write. The guard now sits beside `doc`'s, and
`TestALyingTermOffsetIsNeverFollowed` is what keeps this paragraph true.

The lesson is narrower than "check offsets". A check that lives in a sequential
decoder does not survive the decoder being made random-access, and it does not
announce its absence — the walk was providing it for free, and removing the walk
removed it silently. Every other check this milestone moved was moved
deliberately, from `Open` to `Scrub`, and written down. This one was not moved;
it was dropped, and the fuzzers did not reach it because reaching it needs a
checksum that verifies.

### 4.2 A unit nobody reads is never verified

Milestone 2 got whole-index verification free, because `Open` read every byte.
This one has to buy it, and `Scrub` is the price. Rot in a document no query
reaches sits there until somebody runs it. That is the deal lazy loading makes
and it is stated in `Scrub`'s own documentation rather than only here.

### 4.3 `Commit` holds the write lock

The streaming writer put the disk writes inside the lock, and adopting the new
segment made `Commit` a writer rather than a reader. Queries now wait on a commit
where they used to run alongside it. Incremental commit is what bounds the
window — a commit writes what was added, not the corpus — and the marker names
the upgrade: encode under the read lock, swap under the write lock, counting
captured documents. Worth doing when a load test shows the pause.

### 4.4 Merge policy is a constant

Eight segments, oldest run merged. Adjacency is not a policy choice — it is what
makes a merge a concatenation, so every document keeps its id and no ranking can
move — but the number is unmeasured, and what it trades against is write
amplification nobody counts. Milestone 5's load test is the instrument.

### 4.5 The terms index is read in full

Bounded by the vocabulary and not the corpus: 2.7 MB behind 626 MiB on the
evaluation index. A third fixed-width table would remove it and would be a format
section bought before anything measured a need. If a corpus turns up whose
vocabulary is the problem, `decodeTermIndex` is the function that says so.

## 5. What milestone 1 §3.4 got, and did not

The DocID namespace question stayed open and did not get worse, which was the
obligation. A segment owns `[base, base+count)`, ids stay dense and index-wide,
and `DocID` is still a `uint32` — no composite id, no widened `Candidate`, no
method on `Scorer`. The manifest checks that the bases tile `[0, total)`
contiguously while it reads them, because a list that did not would give two
segments overlapping ids and `segFor` would answer with whichever it walked into
first: a wrong document, not an error.

*Between* indexes the problem is exactly where milestone 1 left it.

## 6. Carried into milestone 5

1. **The vector scan is the milestone's unfinished half** (§3). An approximate
   index is the only thing that closes it.
2. **Block metadata is still written and unread.** D-001 wrote it for a skipper;
   the per-block checksums added here mean a skipped block can now be verified
   when it is finally read.
3. **`Scrub` has no schedule and no incremental form.** It reads everything.
4. **RSS was not measured**, only the Go heap. §3 is the argument for why the two
   differ; a load test is what would show by how much.

---

<!-- markdownlint-disable-next-line MD025 -->
# Milestone 3b — the vector scan

**Verdict: the scan is gone and the working set is not.** The approximate index
holds quality inside the bar that was fixed before it was built, and it costs 5.6×
less arithmetic per query. The bytes a query touches fell by 3.0×, against a
predicted 52×. Both halves are results, and the second is the more useful: the
prediction was wrong in a way that names what would actually fix it. Evidence:
`pkg/engine/ivf_test.go`, `nearest_test.go`, `pkg/scorer/vector/narrow_test.go`,
`weft-eval recall`. The format is [FORMAT.md](FORMAT.md) §4, the decision is
[D-008](DECISIONS.md).

| Pass line | Result |
| --- | --- |
| `text+vector` within 0.005 of 0.6233 | **holds.** 0.6211, at `nprobe = 64`. At the plan's proposed 8 it was 0.6003 and did not |
| Recall and working set are recorded | **holds.** recall@10 = 0.992; 210 MiB per query, against a 12 MiB prediction |
| `pkg/fusion` unchanged; `pkg/scorer` only the repayment | **holds.** 0 lines and 7 lines |
| Two builds write identical `ivf` bytes | **holds.** `sha256 bc042260…b2b6d89`, twice |
| v2 segments open and rank identically | **holds.** The evaluation index was v2 and scored 0.6233 unchanged |

## 1. Result

`nprobe` is the only screw on recall, and the plan registered in advance what to
do if the bar was missed: raise it and re-measure. It was missed, so here is the
curve, all of it, including the part that is not monotone.

| `nprobe` | 8 | 16 | 32 | **64** | 96 | 128 | 160 | 256 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `text+vector` nDCG@10 | 0.6003 | 0.6095 | 0.6174 | **0.6211** | 0.6211 | 0.6205 | 0.6233 | 0.6233 |

Two things in that table are worth more than the chosen number. **It is not
monotone** — 128 scores below 64 — because adding candidates reshuffles ties as
well as adding neighbours, so recall and nDCG are different quantities measured on
the same run. And **64 of `nlist` 414 is 15% of the lists**, where the IVF
literature expects one to ten. That is not a tuning result, it is a statement
about the data: this corpus does not cluster tightly, and §4.1 says what that
might be.

The five arms at `nprobe = 64`, against milestone 3a on the same corpus:

| Arm | Milestone 3a | Milestone 3b |
| --- | --- | --- |
| `text` | 0.5826 | 0.5826 |
| `text+vector` | 0.6233 | **0.6211** |
| `text+graph` | 0.3985 | 0.3985 |
| `text+vector+graph` | 0.5005 | 0.4983 |
| `text+vector+graph-including-seeds` | 0.5451 | 0.5448 |

The binding graph verdict is unmoved: `−0.1228` `[−0.1555, −0.0905]`, still
REGRESSES. **One interval did move and it is reported rather than buried.** What
the vector scorer contributes over text alone was `+0.0407` `[+0.0010, +0.0798]`
and is now `+0.0386` `[−0.0015, +0.0779]` — the point estimate barely moved and
the interval now contains zero. Milestone 4's weakest published claim was already
one hundredth of a point from undetermined; the approximation spent that
hundredth. Anyone quoting "the vector scorer improves over text" has to quote the
exact version, and after this milestone the honest statement is that it is not
distinguishable from zero at 50 queries.

## 2. What the numbers are

Measured by `weft-eval recall` on the evaluation index: 171,332 documents, 148,232
of them carrying a 768-dimensional vector, `nlist` 414, 50 queries.

| Measurement | Brute force | `nprobe = 8` | `nprobe = 64` |
| --- | --- | --- | --- |
| recall@10 vs exact | 1.000 | 0.850 | **0.992** |
| worst query | — | 0.100 | 0.800 |
| candidates per query | 171,332 | 4,361 (2.5%) | 30,549 (17.8%) |
| query latency | 577 ms | 18.5 ms (31.2×) | **125 ms (4.6×)** |
| record bytes reached | 626.6 MiB | 17.8 MiB | 124.1 MiB |
| distinct 4 KiB pages | 626.6 MiB | 34.0 MiB | **210.1 MiB** |
| `text+vector` nDCG@10 | 0.6233 | 0.6003 | 0.6211 |

And what it costs to write: **68 seconds** added to a commit of the whole corpus,
against the plan's predicted one to two minutes. Constant per commit and per merge,
never per query, and not paid at all below 16,384 documents. The partition itself is
1.44 MiB on disk beside a 626 MiB `docs`.

The `nprobe = 64` column was re-measured after a review found `nearest` counting
empty lists against the budget. A list `ivfRefine` left with no members still sits
at the direction it was seeded from, so it ranks high for queries near that document
and used to spend a probe returning nothing; the corrected budget counts only lists
with members. **The column did not move — 30,549.5 candidates per query, measured on
this index both before the fix and after.** That is a fact about this corpus rather
than a verdict on the guard: it says no empty list entered any query's probed set
here, which is what 148,232 vectors over 414 lists should do. A sparse segment, or an
ingest that leaves centroids unclaimed, is where the guard would bind — and this
instrument would report it as recall, never as a candidate count.

That floor moved from 4,096 while this milestone ran, and the move is a second
finding about `nprobe`. The two constants are one decision: a query scans `nlist`
centroids and then `nprobe` lists, so with `nlist = √count` it excludes nothing at
all until `√count` passes `nprobe`. 4,096 was the right floor at `nprobe = 8` and
became exactly the break-even point when the quality bar pushed `nprobe` to 64 — a
segment sitting at the old floor offered **100%** of itself as candidates while its
commit paid for a training and an assignment pass. The floor is now derived,
`4·nprobe²`, the size at which a query first touches half a segment or less:

| segment | 4,096 | 8,192 | 16,384 | 32,768 | 65,536 |
| --- | --- | --- | --- | --- | --- |
| `nlist` | 64 | 91 | 128 | 182 | 256 |
| candidates | 100% | 76% | 48% | 41% | 27% |

Nothing published above moves with it: the evaluation corpus is one segment of
171,332 documents and partitions under either floor. What changes is the tail of an
incremental ingest, which now stays exact instead of paying for a partition that
excludes nothing.

Two smaller numbers, from the tests rather than the corpus. The repayment in
`scorer/vector` is **7 lines** — four removed, three added, all in the loop header
— and every one of the twelve existing contract tests passes unmodified, which is
what says the metric never moved. `pkg/fusion` is **0 lines**.

## 3. The prediction that was wrong

The plan predicted a query would touch about 12 MiB. It touches **210 MiB**, 17.5×
that. The arithmetic behind the prediction was not wrong; the model of what a page
costs was.

The prediction assumed the working set is the vectors a query reads: `nprobe ×
(N/nlist) × d × 4`. At the operating point that is 30,549 × 3,072 B = 89.5 MiB
already, so a third of the gap is simply that 64 lists are probed rather than 8.
The rest is two multipliers the prediction had no term for:

- **A candidate costs a whole record, not a vector.** `Index.Doc` decodes the key,
  the text and the links to reach the vector. 124.1 MiB of records for 89.5 MiB of
  vectors — a 1.4× tax.
- **A record costs whole pages.** The candidates are scattered, because `docs` is
  laid out in DocID order and an inverted list is in centroid order, so each 4.3 KB
  record drags in about 1.7 pages of its own. 124.1 MiB of records becomes 210.1
  MiB of pages — another 1.7×.

**The plan named the wrong repayment, and the measurement is what shows it.** The
registered trigger was to separate a `vectors` section from `docs` once the working
set passed twice the prediction. It passed 8.75× over, and separating `vectors`
would buy far less than it looks: it removes the first multiplier and leaves the
second untouched, because a 3,072-byte vector scattered across a dense array still
straddles about 1.7 pages. Optimistically 210 MiB → 150 MiB, for a format
migration.

**What the second multiplier names is the layout, not the section.** If `docs` were
ordered by centroid rather than by DocID, an inverted list would be contiguous and
30,549 candidates would read 124 MiB in a few dozen sequential runs instead of
30,549 scattered ones — page waste near zero, and readahead working for the query
instead of against it. That is the change worth costing, and it is expensive in a
specific way worth writing down now: DocID is positional in `docs`, `TopK` breaks
ties on DocID, and a merge is a concatenation precisely because adjacent segments
keep their ids. Reordering documents touches all three. It is a milestone, not a
repayment, and it belongs to whoever measures that 210 MiB is the binding
constraint on a real deployment rather than a number in this table.

Both are recorded as debts with their triggers stated, in `scorer/vector`'s
`ponytail:` marker and here. Neither is scheduled — [D-002](DECISIONS.md).

## 4. Known costs

### 4.1 The corpus does not cluster tightly, and that is about the data

15% of lists probed for 0.992 recall is far off the IVF literature's one to ten
percent. The synthetic clustered corpus in `ivf_test.go` reaches 1.000 recall at
`nprobe = 8` of `nlist = 91`, so the algorithm is not the problem. Two candidate
explanations, neither tested: SPECTER2 embeddings of scientific abstracts may
genuinely occupy a space without tight clusters, or a single Lloyd run from a
strided seed over 20,000 samples may leave centroids a k-means++ start would beat.
**The second is testable and was deliberately not tested**, because testing it
means tuning the build against 50 queries, which is how a benchmark gets
overfitted. The observable that would justify it is recall at fixed `nprobe` on a
second corpus.

### 4.2 `nprobe` is a constant, and small segments pay for it

`nlist` grows as √n, so a constant probes a shrinking share as the corpus grows —
15% at 171k documents. That asymptotic behaviour is the whole reason it is a
constant and not a fraction: a fraction of `nlist` scans a fixed share of the
corpus at every size, which is a full scan with a discount. It shrinks only as far
as `ivfMaxList` lets `nlist` grow, though: past 2²⁰ documents `nlist` is pinned at
1,024 and the share floors at 64/1024 = 6.25%, so a ten-million-document segment
decodes 625,000 records per query rather than the 200,000 an uncapped √n implies.
The cost lands at the other end too. A segment with fewer than 64 lists is scanned
nearly whole, and the answer there is simply exact — slower than it needs to be,
never wrong.

### 4.3 Training cannot be cancelled

It is the longest thing a `Commit` does — 68 seconds on the evaluation corpus — and
no context reaches it, because `Commit` takes none and the golden API file admits
exactly one new name this milestone. It is also the longest thing a `Commit` holds
the *write* lock for: `writeSegment` runs inside it, so for those 68 seconds every
`Search`, `Doc`, `Lookup` and `Nearest` waits. Before 3b a commit's exclusive
window was the time to encode and fsync one generation; now it is dominated by two
argmax passes that touch no shared state at all. Both loops are per-document
independent and would stay bit-identical partitioned by contiguous ranges, so the
repayment is parallelism rather than a context — but the ceiling to state is the
one a load test will find, and it is over a minute, not the write.

`Index.Nearest` has the same no-context gap for a much smaller window: the centroid
scan is 3.2e5 multiply-accumulates (414 × 768) and the list decode is a uvarint per
candidate, 30,549 of them, where the scan it replaced ran 1.14e8 multiply-accumulates
between polls. Smaller than what it replaced, and not zero.

### 4.4 A document missing from every list is not detected

`Scrub` verifies that every list decodes, that no document is in two of them, and
that the lists fill the section. It does not check that every vector-bearing
document is in one. A document that is not is invisible to vector queries, and the
cost of that is recall — the same currency the index spends by design, and not
separable from it by any rule available here. `weft-eval recall` is the instrument
that would see it.

### 4.5 Determinism rests on the absence of a generator, not on a seed

There is no RNG in `ivf.go`. The training sample and the initial centroids are both
taken on a fixed stride, which is stronger than a fixed seed — there is nothing to
forget to seed. Its one weakness is stated where it lives: a corpus whose ordering
is periodic with the stride is sampled from one phase of that period. Ingest order
would have to be adversarial for that to bite.

## 5. Where ANN lives, and what milestone 1 §2.2 actually asked

The partition is in `pkg/engine`, and from a distance that looked like the engine
absorbing a scorer's meaning. It is not, and the distinction is worth keeping
because the next structure will raise it again.

Milestone 1's test is whether a scorer needs a private store — whether the index is
genuinely scorer-neutral, or whether each scorer ends up with its own copy of the
corpus. The partition is a *section of the segment format*. It is written by the
writer, mapped by the reader, checksummed like every other section and verified by
`Scrub`; putting it in `scorer/vector` would have given that scorer exactly the
private store the hypothesis forbids.

What the boundary then had to answer is where the metric goes, and
[D-008](DECISIONS.md) is that answer: `Nearest` returns `[]DocID` and computes no
score. The engine knows the geometry — which documents are close enough to be worth
looking at — and the scorer knows the metric. **The evidence that the line was
drawn in the right place is the size of the diff on the other side of it: 7 lines.**
Every rule about zero norms, non-finite queries, mixed widths and cancellation
stayed in `vector.go` untouched, and all twelve of its tests passed without edit.
Had the line been wrong, those rules would have had to move.

The counterfactual is recorded too, because it is what would falsify this: if a
scorer ever needs the candidates *in rank order*, or needs the centroid distances,
`Nearest` widens to `[]Candidate` and half of a scorer is inside the engine. That
diff would be the honest price of D-008 being wrong.

## 6. Carried forward

1. **The working set is 210 MiB per query and the layout is why** (§3). Not the
   `vectors` section the plan expected — centroid-ordered `docs`, which is a
   milestone rather than a repayment.
2. **`text+vector − text` is now undetermined at 50 queries** (§1). Not caused by
   this milestone alone; exposed by it.
3. **Whether better centroids buy back `nprobe`** (§4.1) — testable, deliberately
   untested, and the test needs a second corpus.
4. **Neither the build nor `Nearest` can be cancelled** (§4.3).
5. **Milestone 3's outcome sentence is now true of both paths.** The half milestone
   3a left open — a corpus with vectors — is closed at 3.0× fewer bytes and 5.6×
   less arithmetic per query, which is smaller than hoped and is measured rather
   than claimed.

---

<!-- markdownlint-disable-next-line MD025 -->
# Milestone 5 — Performance

**Verdict: both clauses hold, and the prediction that got them there was wrong.**
weft's p99 at the registered load point is **108.193 ms**, of which the collector
accounts for **411 µs — 0.38%**. bleve on the same machine, corpus, query set, arm
and load generator is **57.525 ms**, so weft is **1.88×** it against a bar of 10×.
The plan predicted the tail would be a working-set problem rather than a GC problem;
below saturation it is neither. **Above it, the ladder found what the plan did not
look for: weft cannot sustain its own sequential throughput, and bleve can** (§3.2).
Evidence: `internal/loadgen`, `weft-eval bench`, `bench/`. The measurement design and
the judgment rules fixed before the numbers are [PERF.md](PERF.md), the decision is
[D-009](DECISIONS.md).

| Pass line | Result |
| --- | --- |
| A p99 including GC pause is published | **holds.** 108.193 ms at 3.41/s, n = 10,000, with the stop-the-world time charged per sample and subtracted alongside |
| Within an order of magnitude of an established engine | **holds.** 1.88× bleve v2.6.0, same machine and session |
| `go list -m all` still one line after bleve entered | **holds.** bleve and its ~20 transitive modules live in `bench/`, a separate module |
| `pkg/fusion` unchanged | **holds.** 0 lines. `pkg/scorer` 0 lines too — this milestone changed no engine code |
| The write lock's ceiling under read load | **holds.** A commit adding 20,000 documents held it **11.063 s**, and the worst read due inside that window waited **12.539 s** — 150× the p50 beside it. §3.3 |
| *(not a pass line, found anyway)* | weft collapses at 27.28/s — p50 39 ms to 1.27 s, 14% shed, RSS 126 to 853 MiB. §3.2 |

**Every figure here was re-measured after a review corrected three defects in the
instrument** — §4.1 has them and what each moved.

## 1. Result

Machine: Apple M4, 16 GiB, macOS 26.5.2, Go 1.26.1 darwin/arm64, GOMAXPROCS 10,
GOGC default. Corpus: the 171,332-document evaluation index, 50 judged TREC-COVID
queries, k = 10, `text` arm both sides.

| | weft | bleve v2.6.0 |
| --- | --- | --- |
| sequential p50, warm, 200 samples | 36.656 ms | 6.364 ms |
| headline rung (12.5% of own throughput) | 3.41/s | 19.64/s |
| p50 | 83.371 ms | 15.401 ms |
| p95 | 99.507 ms | 36.863 ms |
| **p99** (headline rung) | **108.193 ms** | **57.525 ms** |
| p99 minus charged STW | 107.782 ms | 57.436 ms |
| best p99 on the ladder | 64.709 ms at 13.64/s | 25.370 ms at 157.13/s |
| max | 126.370 ms | 64.027 ms |
| GC cycles over the rung | 24,496 | 1,756 |
| STW total | 1.829 s (0.062% of elapsed) | 157.5 ms (0.031%) |
| GC CPU share | 1.1% | 0.1% |
| major faults | 0 | 0 |
| involuntary context switches | 1,933,648 | 284,273 |
| peak RSS (process) | 120.7 MiB | 58.9 MiB |
| index on disk | 626 MiB (434 MiB of it vectors) | 150.6 MiB |
| build time | 68 s of IVF training alone | 17 s |

**The ratio the milestone is graded on is p99, and it is 1.88.** Every other row is
context, and two of them matter for reading it: bleve's index is a fifth the size
because it holds no vectors, and bleve's analyzer removes stop words while
`engine.Tokenize` does not — so on a query like "what is the origin of COVID-19"
bleve walks far shorter postings. [PERF.md](PERF.md) §4 has the full list of what is
not matched and which way each biases. None of it is worth a factor of ten, which is
the only claim being made.

## 2. The prediction was wrong twice, and the conclusion is right anyway

The plan's §1 disagreed with the PRD's own risk table. The PRD had *Go GC로 p99
예측 가능성이 낮음*; the plan predicted the tail would be a **working-set** problem,
reasoning from milestone 3a's 74,504-byte live heap and milestone 3b's 210 MiB of
distinct pages per query.

**Both are wrong, and in the same direction: below saturation the collector and the
storage are each a fraction of a percent.**

| | predicted | measured |
| --- | --- | --- |
| allocation per query, `text` | — | 43.6 MiB, 75,132 objects |
| allocation per query, `text+vector` | ≤ 124 MiB | 181.9 MiB, 782,955 objects |
| GC heap goal | 4 MiB (live is 74 KB, so the floor) | 46–53 MiB |
| GC cycles per query, `text+vector` | ≈ 31 | 9.4 |
| STW share of the p99 | ≈ 1.5% | **0.38%** |
| major faults per rung, warm | the binding constraint | **0** |

The arithmetic error is worth naming because it is easy to repeat. The prediction
took the *idle* live heap — 74 KB, which milestone 3a measured and published — and
assumed the GOGC target therefore sits on its 4 MiB floor forever. It does not: a
query holds 30,549 candidates and their decoded records alive simultaneously, so
live heap *during* a query is tens of megabytes and the target rises with it.
Cycles came out at a third of the prediction, each with far more to mark. **A heap
figure measured at rest does not predict the behaviour of a collector under load,
and milestone 3a's headline number was measured at rest.**

The working-set half failed differently. 210 MiB of distinct pages per query is
real ([milestone 3b §2](#milestone-3b--the-vector-scan)), and it costs nothing
here, because a ladder replaying 50 queries 200 times finds every one of those
pages in the page cache: **`majflt` is 0 across every rung of both ladders.** The
cold pass shows what the other regime looks like — 713 major faults and a 94 ms
worst query against an 83 ms warm median — and that is 50 samples, which supports a
maximum and not a tail. A steady-state server is warm, so warm is the honest
headline; the number would be a different one on a host whose cache is contested,
and this measurement cannot say what.

## 3. What the tail actually is, which nothing predicted

Neither GC nor storage leaves anything like the gap between 36.656 ms sequential and
83.371 ms at 12.5% of that throughput. And nothing in the plan predicted what the
ladder found above it. Both ladders, in full:

| rate (weft) | 3.41/s | 6.82/s | 13.64/s | **27.28/s** | 54.56/s |
| --- | --- | --- | --- | --- | --- |
| p50 | 83.371 ms | 51.515 ms | 39.187 ms | **1.2688 s** | 1.8816 s |
| p95 | 99.507 ms | 73.021 ms | 57.644 ms | 2.7466 s | 3.4694 s |
| p99 | 108.193 ms | 78.605 ms | 64.709 ms | — | — |
| shed | 0 | 0 | 0 | **1,438** | 6,272 |
| peak RSS | 120.7 MiB | 123.2 MiB | 126.3 MiB | **853.0 MiB** | 921.7 MiB |

| rate (bleve) | 19.64/s | 39.28/s | 78.56/s | 157.13/s | 314.25/s |
| --- | --- | --- | --- | --- | --- |
| p50 | 15.401 ms | 11.745 ms | 8.350 ms | 7.797 ms | 8.208 ms |
| p95 | 36.863 ms | 25.605 ms | 15.227 ms | 17.569 ms | 26.638 ms |
| p99 | 57.525 ms | 35.390 ms | 25.597 ms | **25.370 ms** | 83.468 ms |
| shed | 0 | 0 | 0 | 0 | **0** |
| peak RSS | 58.9 MiB | 59.0 MiB | 59.1 MiB | 59.3 MiB | 67.6 MiB |

Three separate things are in those tables and the milestone's registered rule saw
none of them.

### 3.1 Latency falls as load rises, on both engines, until it does not

p50 drops monotonically over a four-fold range on weft (83.4 → 39.2 ms) and an
eight-fold range on bleve (15.4 → 7.8 ms), and bleve's *best* open-loop p50 —
7.797 ms at full measured throughput — is within 23% of its 6.364 ms sequential
baseline while its lowest rung is more than double it.

The explanation that fits both is that the sequential baseline measures a warm
machine and a sparse open loop does not. At 3.41/s a weft query is followed by 210
ms of idle, over which caches are evicted and the core clocks down; back to back in
a tight loop, none of that happens. So:

1. **The "unloaded" denominator is optimistic**, and every rung below saturation is
   measured against a machine state no rung reproduces.
2. **The registered saturation rule therefore fires at rung 1 for both engines** —
   p50 passed twice the unloaded p50 immediately — which puts the headline on the
   *slowest* rung of the ladder. The rule was fixed before the numbers
   ([PERF.md](PERF.md) §3) and is reported as it fired rather than adjusted
   afterwards.
3. **The comparison survives it** because both sides are quoted at the same relative
   load, 12.5% of their own sequential throughput, through the same driver. It is
   also conservative for weft: its best measured p99 is 64.709 ms, not 108.193 ms,
   and at their respective best rungs the ratio is 64.709/25.370 = **2.55×** — still
   inside the bar, and worse than the headline's 1.88×, so the rule is not flattering
   weft either.

**What would fix the rule is a different denominator** — an open-loop rung at the
lowest rate rather than a tight sequential loop. That is a change to a registered
judgment rule and belongs to whoever owns it, not to the milestone measuring under
it.

### 3.2 weft cannot sustain its own sequential throughput, and bleve can

This is the milestone's real finding and no part of the plan anticipated it.

At 27.28/s — the rate weft's own sequential replay achieved — the engine does not
slow down, it **collapses**: p50 goes from 39.2 ms to **1.27 seconds**, a factor of
32; 1,438 of 10,000 requests are shed because the in-flight cap is permanently full;
and peak RSS goes from 126 MiB to **853 MiB**. bleve at the corresponding rung of
its own ladder, 157.13/s, has p50 7.797 ms, its *best* p99 of the whole sweep, sheds
nothing, and sits at 59.3 MiB.

The mechanism is visible in the RSS column and it is not the collector's baseline
cost:

- A weft query allocates 43.6 MiB and holds much of it live at once — 30,549
  candidates and their decoded records (§2).
- The in-flight cap is 40. Forty concurrent queries is therefore of order 800 MiB of
  **live** heap, which is what the 853 MiB measures.
- GOGC targets a multiple of live heap, so the collector's work per cycle scales with
  concurrency here. More in flight makes each query slower, which puts more in
  flight.

That is a positive feedback loop with a knee, and the knee sits between 13.64/s and
27.28/s.

**bleve has a knee too, and it is a different kind.** Its p99 turns around at
314.25/s — 25.370 → 83.468 ms — but it sheds nothing, its RSS moves 59.3 → 67.6 MiB,
and its p50 barely moves at all (7.797 → 8.208 ms). That is a queue forming under
overload, which is what saturation is supposed to look like. weft's is a memory
collapse, which is not.

So the plan's §1 was wrong to acquit the collector and wrong about which resource
would bind, but the corrected story is not the one it told either. Below the knee GC
is 0.38% of the p99 and the working set is entirely page-cached. At the knee the
binding constraint is **live heap under concurrency**, which is a property of
`Index.Doc` decoding a whole record per candidate — the same decode
[milestone 3b §3](#milestone-3b--the-vector-scan) costed at a 1.4× tax on the
working set, now reappearing as the thing that ends the throughput curve.

**What this does not say.** The knee was found with `inflight = 40`; a lower cap
would trade shed requests for a lower heap and might move it. Nothing here sweeps
that, and the honest statement is that weft's usable throughput on this corpus and
this machine is **at least 13.64/s and less than 27.28/s**, against a sequential
27.3/s — bleve's is at least its sequential 157/s.

### 3.3 The write lock, and the 68 seconds that were not reproducible

[Milestone 3b §4.3](#milestone-3b--the-vector-scan) handed this milestone a
68-second IVF training inside `Commit`, held under the write lock, and said the
ceiling is "the one a load test will find". `weft-eval bench -writes` is that load
test: it copies the index — a `Commit` against `.eval-data/index` would rewrite the
corpus every published number is measured against — runs a read load against the
copy, and drops one commit into the middle of it.

The first answer was **36 milliseconds**, and it is a finding rather than a
measurement error. Since milestone 3a a commit writes only what was added since the
last one, and a new segment below `ivfMinDocs` carries no partition at all — so a
one-document commit skips the training entirely. **The 68 seconds is not a property
of `Commit`; it is a property of committing 171,332 documents at once**, which is
what a first build does and what nothing else does.

Forcing the expensive case — 20,000 documents in one commit, enough to cross the
partition floor — gives the ceiling:

| | |
| --- | --- |
| writer held the exclusive lock | **11.063 s** |
| of which the commit itself | 11.014 s |
| of which the 20,000 `Add` calls | **49 ms** |
| reads due inside that window | 38 |
| **worst read due inside** | **12.539 s** |
| worst read due outside | 2.093 s |
| p50 outside | 83.401 ms |
| shed | 0 |

**A read arriving during a partition-training commit waits 150× the median.** The
2.093 s outside the window is the queue draining afterwards, so the damage outstays
the lock. Both numbers scale with the size of the commit rather than with the size
of the index, which is the useful part: an ingest that commits in batches under
`ivfMinDocs` never pays it, and one that commits a corpus pays it in full.

The `Add`/`Commit` split is reported because it was the subject of a correction and
turned out small: `engine.Add` takes the same exclusive lock, so 20,000 of them do
block reads, but they cost 49 ms of the 11.063 s — 0.4%. The correction was right and
its magnitude on this configuration is not what anyone would have guessed, which is
the reason to print both halves rather than the total.

The documents added are synthetic — the training cost is a function of count and
vector width, not of content, and sourcing real vectors inside a latency measurement
would mean re-reading the corpus. Stated here because it is the kind of shortcut that
should not be discovered in the code.

## 4. Known costs

### 4.1 The instrument was wrong three times, and every number here is the re-measurement

A review found three defects that move published figures. All three are fixed, every
figure above was produced afterwards, and the pre-fix numbers are recorded here so
the direction of each error is visible rather than merely asserted.

| | pre-fix | corrected |
| --- | --- | --- |
| headline p99 | 98.041 ms | **108.193 ms** |
| p99 minus STW | 97.762 ms | 107.782 ms |
| collector's share of the p99 | 279 µs (0.28%) | **411 µs (0.38%)** |
| bleve headline p99 | 47.123 ms | **57.525 ms** |
| ratio | 2.08× | **1.88×** |
| write lock held | 11.641 s | 11.063 s |

**`GCPause` was charged over a shorter window than the `Lat` it is subtracted from.**
`Lat` starts at the request's due time; the pause total was read inside the request's
goroutine, so a stop-the-world landing in the queue before dispatch was in `Lat` and
not in `GCPause`. Measured on this tree at 3,000 qps: 64% of the p99 elapsed before
pause accounting began. The collector's published share was an under-estimate, which
is the direction that flatters the engine.

**`-writes` stamped its window after the `Add` loop.** `engine.Add` takes the same
exclusive lock `Commit` does, so the writer blocked reads twenty thousand times
before the window opened and `SplitByWindow` filed every one of those stalls under
`outside` — the baseline the lock's cost is compared against. §3.3 has what it turned
out to be worth: 49 ms of 11 s.

**The unloaded median was 50 samples**, which `loadgen.Printable` rejects for a p50 —
the package's own rule, applied everywhere except to the number that is the
denominator of all five arrival rates and the reference the saturation rule compares
every rung against. Now 200 on both sides, through `Summarize` so the two
denominators have one spelling.

**Two earlier defects, found in an earlier review, are why two ladders before these
were discarded.** `GCPauseTotal` allocated a fresh `[]metrics.Sample` per call —
roughly 150 MiB of garbage per ladder, produced *by* the pause counter and then
charged to the query — and `GC CPU share` was the ratio of two process-since-start
totals rather than of two differences, so by the fifth rung a rung that gave a third
of its CPU to the collector and one that gave none printed the same number. The file
already carried the argument against `runtime.ReadMemStats` — *an instrument that
pauses the program once per request would be measuring pauses it caused* — and it
applied one function down, unchecked.

**Five instrument defects across two reviews is itself the finding.** Every one of
them biased toward a flattering number, and none was visible in the output: a
latency distribution looks equally plausible whether or not the clock around it is
honest.

### 4.2 The first GC attribution design was degenerate and was measured, not reasoned, out

Classifying each sample as GC-hit or GC-free by the cycle counter produced 200 hits
out of 200 on a smoke run: at 485 collections over 200 queries every request overlaps
one. Charging pause time per sample replaced it. The limit of the replacement is
stated where the number is: **mark assist is not stop-the-world**, so a request
charged zero pause can still have spent time marking, which is why `GC CPU share` is
printed beside it. At 1.1% for weft, neither figure leaves room for the collector to
be the tail.

### 4.3 A rung is silent for forty-nine minutes

`bench` prints nothing between the header and the end of a rung. The headline rung is
10,000 requests at 3.41/s, which is 49 minutes of a process that looks hung and is
not. That cost real time in this milestone. A progress line every thousand samples
would fix it and was not written, because it is the kind of change that wants to land
after the numbers rather than between two of them.

### 4.4 The `text+vector` arm was not laddered

The deployable arm is four times slower per query, so 10,000 samples at 12.5% of its
throughput is over five hours. Its allocation figures are in §2 and
[PERF.md](PERF.md) §6; its tail is not measured. **The published p99 is for the arm
bleve can be compared against, not the arm a user would run**, and that gap is the
price of [D-009](DECISIONS.md)'s scope decision.

### 4.5 One repetition, not three

[PERF.md](PERF.md) §5 fixes the headline as the median of three runs with the spread
reported. **This is one run of the corrected instrument.** Milestone 4 §4.2 is the
standing lesson about publishing a number whose variance nobody measured; the honest
reading of 108.193 ms is that it is a single observation of a quantity whose
run-to-run spread is unknown, and the 1.88× ratio has correspondingly unknown error
bars. It clears a 10× bar by a wide enough margin that no plausible spread reaches
it, which is why the verdict stands and the caveat is still recorded.

The one cross-run comparison available is not reassuring about precision: the same
rung measured by the pre-fix and post-fix instruments gave 98.041 ms and 108.193 ms.
Most of that is the `GCPause` correction and the 200-sample denominator moving the
rate from 3.23/s to 3.41/s, but nothing here separates those from run-to-run noise,
and only three runs of one instrument would.

## 5. Carried forward

1. **Nothing bounds a commit's lock window.** §3.3 prices it — 11.063 s for 20,000
   documents, and a read caught in it waits 12.539 s — but the repayment is unbuilt.
   Two shapes are visible from here and neither is scheduled: train the partition
   outside the write lock (both argmax passes touch no shared state,
   [milestone 3b §4.3](#milestone-3b--the-vector-scan) says so), or give `Commit` a
   context so an operator can abandon one.
2. **The throughput knee is live heap under concurrency** (§3.2), and it is the first
   measurement that puts a number on the cost of `Index.Doc` decoding a whole record
   per candidate. Milestone 3b costed that decode as a 1.4× tax on the working set
   and registered a repayment trigger against page counts; the trigger that actually
   fired is a different one. Whether a lower in-flight cap moves the knee is unswept.
3. **The saturation rule's denominator is wrong** (§3.1), and fixing it changes a
   registered rule.
4. **Below the knee the tail is neither the collector nor the storage** (§2), so the
   210 MiB working set and the centroid-ordered `docs` layout milestone 3b costed
   remain unjustified by any measurement here. They would bind on a host whose page
   cache is contested; this one's is not.
5. **`text+vector` has no published tail** (§4.4).
6. **Three repetitions** (§4.5).

---

<!-- markdownlint-disable-next-line MD025 -->
# Milestone 6 — Adoption

**Verdict: the claim holds, and the milestone's value is the three defects it found
holding it.** Two subjects with no prior sight of the tree each added a fifth
ranking signal using only published documentation — no `.go` file under `pkg/`,
`internal/`, `cmd/` or `bench/` was opened, weft was not modified, and both landed
inside the 100-line figure. **Zero code-required blockers.** Three documentation
defects, one of them named independently by both subjects. Design and rules are
[ADOPTION.md](ADOPTION.md), fixed before the trial ran; the decision is
[D-010](DECISIONS.md).

| Pass line | Result |
| --- | --- |
| The trial runs for both tasks and every blocker is published | **holds.** 3 blockers, [ADOPTION §6](ADOPTION.md) |
| Blockers classified; code-required ones named, not fixed | **holds vacuously.** There were none — all three were documentation |
| README does not lie; the extension snippet points at something that compiles | **holds in part.** `ExampleScorer` is checked by `go test`, and the status table matches the PRD on milestones 1 to 5. Row 6 does not: the PRD carries this milestone as `in-progress` because pass line 4 is unmet, and the README marks it ✅. Both are deliberate and neither is silent — the ✅ is the measurement, `v0.1.0` is §4.4 — but "row for row" is not what the table does |
| `v0.1.0` tagged and resolvable | **pending.** Deliberately gated on the maintainer, §4 |
| `pkg/fusion` 0 lines, and any `pkg/` diff's line count published here | **holds.** `pkg/fusion` 0 lines, and both golden API files byte-identical. The `pkg/` price tag is **320 insertions, 4 deletions** across four files: `adoption_test.go` +192 and `example_test.go` +87 are test, `doc.go` +29/−4 and `search.go` +12 are comment only — no production statement changed |

## 1. Result

| | Task A — popularity | Task B — per-query geo |
| --- | --- | --- |
| verdict | possible from docs alone | possible from docs alone |
| blockers | 1, documentation | 2, documentation |
| source files opened | **0** | **0** |
| implementation | 31 lines | 76 lines |
| call-site wiring | 8 lines | 1 line |
| time to first correct ranking | ~4.5 min | ~4 min |

## 2. The prediction was half right, and the wrong half is the interesting one

The plan predicted that the documented extension path is **a fork** for an
outsider: `engine.Document` has five fields, `engine.Query` has three, neither is
open, and `doc.go` says a fifth scorer means adding a field.

**The public API was sufficient the whole time.** `pkg/engine/adoption_test.go`
established it before the trial: `Index.Resolve` and `Index.Doc` are a real join,
a caller-held table keyed by `Key` reaches fusion like any other stream, and it
still names the right documents after `Commit` and `Open`. Then both subjects
found the same path unaided.

So prediction A collapses from *"an outsider must fork"* to *"an outsider must
assemble a pattern nothing documents"* — and that is not a small correction, it is
the whole milestone. **Every part of the answer was documented and the assembly
was not.** `Resolve`'s godoc, `Scorer`'s silence about where data comes from, and
`Key`'s stability across a restart are each written down; the sentence that puts
them together did not exist, and the one sentence that addressed the question
pointed the other way.

Prediction B stands and was never tested by the trial. `scorer/recency` — the only
scorer an outsider can copy — sweeps every `DocID` calling `Doc`, which is
[milestone 5 §3.2](#milestone-5--performance)'s throughput wall. Both subjects
avoided it, and neither did so because a document said to: their data was a map,
so looping over the map was simply the obvious thing. **The exemplar is still
shaped like the wall, and the trial got past it by luck of task shape.**

## 3. The defect both subjects named

`engine.Document`: *"Adding a fifth scorer means adding a field here, not touching
anything else."*

A said it "actively points the wrong way" and nominated it as *the single sentence
I would change*. B said it "points an external adopter at a door they cannot
open". Neither saw the other's report.

**The sentence was true.** For weft's own scorers a fifth signal does mean a new
field. It was written from inside the repository and read from outside it, and
that is the entire failure. No test could have caught it —
`TestEngineAPISurfaceIsUnchanged` records declarations, not the prose above them,
and prose that is accurate for the author is exactly the kind that survives review.

That is the reusable lesson, and it is not about this sentence: **a project that
documents its own internals well produces documentation that reads as authoritative
to someone it was never written for.** The other two defects have the same shape —
`Query` promising to carry "every scorer's input", `Search`'s `k` quietly doing two
jobs — both true from inside, both misleading from outside.

## 4. Known costs

### 4.1 The subjects were agents, and this is a lower bound

Four minutes to a working scorer is a number produced by a reader that consumes
`go doc -all` in one pass and never loses interest. A human meeting the `Document`
sentence does not necessarily recover by reading `Resolve`'s godoc and inferring a
join; they may conclude the library requires a fork and leave, and that outcome is
invisible to this instrument. **The PRD's "zero user interviews" risk is not
discharged, not reduced, and not addressed by this milestone.**

### 4.2 The boundary was self-reported

Nothing prevented either subject from reading `pkg/engine/index.go`. Both reported
zero source reads, and this design cannot verify that. Registered in
[ADOPTION §2.2](ADOPTION.md) before the trial rather than noticed after it.

### 4.3 One run per task

No variance is measured. Milestone 5 §4.5 owed three repetitions and paid one;
this milestone inherits the same debt knowingly and at a lower cost, because the
output here is a blocker list rather than a distribution.

### 4.4 `v0.1.0` is not cut

The remaining pass line. Deliberate: a tag freezes a tree and the documentation
repair had to land first, which it now has. Gated on the maintainer rather than
scheduled — the module proxy does not withdraw a version it has served.

## 5. D-005's check, executed

D-005 said it would be shown wrong if `FuseWeighted` acquired no caller outside
`internal/eval` and `scorer/graph` were still present and still unweighted at
milestone 6. Both halves were run.

**It acquired callers** — `examples/basic` and `cmd/weft-eval`'s weight sweep.
**And the demo was still unweighted**: `cmd/weft`, the binary README's quick start
tells a newcomer to run, fused the graph stream at a full vote while README's
limitations table and the scorer's own package doc both said to weight it down.
Half the falsifying signal was live, in the most-read place in the project. Now
`FuseWeighted(1, 1, 0.1, 1)`, and the README sample output is the new ranking.

## 6. Carried forward

1. **The exemplar scorer is shaped like the throughput wall** (§2). `scorer/recency`
   is what an outsider copies and what milestone 5 measured collapsing. Nothing
   documents the difference and no test enforces it.
2. **Resolve at construction or per query is an undocumented choice.** A resolved
   once, B on every call. Both correct, the trade unwritten.
3. **`v0.1.0`, and with it the adoption metric's start date** (§4.4).
4. **No human subject** (§4.1). Every number here is a lower bound until there is
   one.

---

<!-- markdownlint-disable-next-line MD025 -->
# Milestone 7 — A baseline nobody has to qualify

**Verdict: there is no baseline. The headline load point is not reproducible, and
what decides the outcome is not the load.** Three observations at the same arrival
rate, on the same corpus, same machine, same binary, same day: one rung shed nothing
and held a 37.9 ms median, two collapsed to 1.539 s and 416 ms and shed 14% and 11%
of the load. The milestone set out to replace a single observation of unknown spread
with a median of three. What it found is that the quantity being repeated is not
well defined, and [D-011](DECISIONS.md)'s premise — that a repetition is the same
rung measured again — is false.

That is a more useful answer than three numbers would have been, and it lands on
[milestone 8](../.claude/prds/weft-hardening.prd.md) before its pass line was
written into code rather than after.

| Pass line | Result |
| --- | --- |
| The headline is the median of three repetitions with the spread published | **not met, and the reason is the finding.** Two of three observations cannot support a p99 at all: shed put them under the 10,000 samples [PERF.md](PERF.md) §2.3 requires. There is no median to take |
| `text+vector` has a published p99 | **not started.** The campaign stopped at this finding; §6 |
| Each repetition's unloaded p50 is recorded | **holds**, and it is the measurement that rules out the obvious explanation. §3 |
| `pkg/` untouched, `go list -m all` one line | **holds.** `git diff --stat pkg/` empty across every commit in this milestone |
| The procedure is committed before the first measurement | **holds.** `docs/PERF.md` rules 3 to 6 and D-011 landed in `f0dde61` and `fce4088`; the first published figure here comes from a run started afterwards |

## 1. The three observations

All at **25.67 q/s**, the rate [PERF.md](PERF.md) §3 rule 1 selected from
repetition 1's ladder. Apple M4, Go 1.26.1, 171,332 documents, 50 judged queries,
k=10, `text` arm, `inflight` 40.

| | context | n / shed | p50 | p99 | peak RSS | unloaded p50 |
| --- | --- | --- | --- | --- | --- | --- |
| rep 1 | 4th rung of a full ladder | 10,000 / **0** | **37.852 ms** | 69.136 ms | **114.2 MiB** | 38.955 ms (25.7/s) |
| rep 2 | single rung, `-rate 25.67` | 8,544 / **1,456** | **1.539 s** | — | **1020.8 MiB** | 40.357 ms (24.8/s) |
| rep 3 | single rung, `-rate 25.67` | 8,918 / **1,082** | **415.624 ms** | — | **764.7 MiB** | 36.449 ms (27.4/s) |

The medians span **40×**. Two of the three shed enough to fall under the sample
floor, so `Printable` refused their p99 — correctly, and that refusal is why the
milestone's own pass line cannot be met.

Repetition 1's full ladder:

| rung | rate | p50 | p99 | shed | peak RSS |
| --- | --- | --- | --- | --- | --- |
| 12.5% | 3.21/s | 68.963 ms | 100.136 ms | 0 | 114.2 MiB |
| 25% | 6.42/s | 52.719 ms | 82.835 ms | 0 | 114.2 MiB |
| 50% | 12.84/s | 40.438 ms | 63.654 ms | 0 | 114.2 MiB |
| **100%** | **25.67/s** | **37.852 ms** | **69.136 ms** | **0** | **114.2 MiB** |
| 200% | 51.34/s | 1.810 s | — | 5,894 | 744.3 MiB |

`saturation: 51.34/s (first rung past 2x the unloaded p50 of 38.955ms); headline is
25.67/s`.

## 2. What is not the explanation

**Not the instrument.** No rung in any of the three runs printed `SUSPENDED`. The
ladder's wall clock ran 100 m 48 s against 100 m 39 s of rungs plus a 10 s warm-up —
nine seconds unaccounted, which is the report printing. §4 is why that sentence can
be written at all.

**Not relative load.** Repetition 3's sequential throughput was **27.4 q/s**, so
25.67/s was 94% of it — and it collapsed. Repetition 1's was 25.7 q/s, so the same
rate was 100% of it — and it did not. The observation at the *lower* fraction of its
own capacity is the one that fell over.

**Not a cold machine.** Repetition 3 ran forty seconds after repetition 2 finished
and had the fastest cold pass of the three (1.82 s against 2.017 s and 3.407 s, and
`majflt` 0 against 713 and 722). It still collapsed.

**Not machine drift between sessions**, at least not on its own. The unloaded p50
moved 36.4 / 39.0 / 40.4 ms across the three — an 11% spread, worth recording and
[D-011](DECISIONS.md) is why it was — but it does not order the outcomes: the fastest
machine of the three produced a collapse and the middle one produced the flat rung.

## 3. What is left, and what points at it

The one structural difference: **repetition 1's rung was the fourth of a ladder, 91
minutes into the process, after rungs at 3.21, 6.42 and 12.84 q/s. Repetitions 2 and
3 ran that rate alone, straight out of a 200-request warm-up.**

The collector's figures say the same thing from the other side:

| | GC cycles | GC CPU share | peak RSS |
| --- | --- | --- | --- |
| rep 1, rung 4 | 23,138 | 5.5% | 114.2 MiB |
| rep 2 | 5,575 | 1.6% | 1020.8 MiB |

A quarter of the collections and nine times the memory. That is the shape of a pacer
that is behind: [milestone 5 §2](#2-the-prediction-was-wrong-twice-and-the-conclusion-is-right-anyway) measured a query allocating
43.6 MiB with a live heap that rises to tens of megabytes while 30,549 candidates are
held, and a process that has spent ninety minutes climbing a ladder reaches that load
with a heap goal already grown to meet it. One that starts there does not.

**This is a hypothesis, not a result.** Nothing here varied the ladder prefix
deliberately, and GC pacing is one of at least two readings — the other being that
`inflight` 40 admits a burst at rung start which a process arriving from a lower rung
never sees. Separating them is [milestone 8](../.claude/prds/weft-hardening.prd.md)'s
work, and §6 carries it.

## 4. Known costs

### 4.1 The first repetition was measured on a sleeping machine, and nothing said so

The campaign's first attempt ran 08:11 to 22:03 by the wall clock and reported 92
minutes of rungs. The lid had closed at 08:34:13 — twenty-three minutes into the
first rung — and the machine slept and dark-woke for thirteen hours.

**Every figure in that report was plausible.** Each rung's elapsed matched its own
schedule to within a second, shed was zero below the knee, `majflt` was 0, and the
headline p99 came out **107.332 ms against milestone 5's 108.193 ms** — a 0.8%
agreement that would have read as reproduction.

`time.Since` reads the monotonic clock, and on Darwin that clock does not advance
while the system is asleep. Measuring the time the process experienced is the right
quantity for a latency, and is exactly why it cannot see time the process did not
experience. The run was caught only because a `date` happened to be piped either side
of the command.

The repair is `loadgen.Elapsed`, which reads both clocks and publishes the gap: past
`SuspendTolerance` a rung prints `SUSPENDED` as its **first** line and both summaries
refuse the run outright. The check runs before the ladder-shape check, because a
complete five-rung sweep across a sleeping machine satisfies every other condition.
Evidence is [weft-m7.tdd.md](testing/weft-m7.tdd.md).

**Milestone 5's ladder had no such accident.** There were seven clamshell sleeps on
the two days it was measured, and whether any overlapped it is **unanswerable from
its published record**. That is not a claim that milestone 5 slept; it is the
statement that nothing published can rule it out, and that this was true of every
number in this repository until `84b0467`.

### 4.2 The load-point rule is bistable on this workload, and that was visible before the campaign

Saturation is the first rung whose p50 passes twice the unloaded median. On this
corpus rung 1 sits within a few milliseconds of that threshold, so which side it
lands on decides whether the headline is the ladder's slowest rung or its fastest:

| run | unloaded | threshold | rung 1 p50 | saturation at | headline |
| --- | --- | --- | --- | --- | --- |
| milestone 5 | 36.66 ms | 73.3 ms | above it | rung 1 | 3.41/s, p99 108.193 ms |
| discarded (slept) | 35.608 ms | 71.2 ms | 77.373 ms | rung 1 | 3.51/s, p99 107.332 ms |
| **rep 1** | 38.955 ms | 77.9 ms | **68.963 ms** | **rung 5** | **25.67/s, p99 69.136 ms** |

So `108.193 ms` and `69.136 ms` are not two measurements of one quantity. They are
p99s at load points seven times apart, selected by a rule that flipped on a few
milliseconds of rung-1 median. Rule 1 is not wrong — a headline has to be quoted
somewhere and choosing after the fact is worse — but its output on this workload is
not stable, and milestone 5 published one draw from it.

### 4.3 Rule 3 was registered a day before it was falsified

[PERF.md](PERF.md) §3 rule 3 and [D-011](DECISIONS.md) were committed before any
figure they govern, which is the discipline working. They were also wrong, and the
campaign they governed is what showed it. The rule directs a median to be taken over
three observations of a rung; §1 shows the rung is not the same rung when it is
measured alone.

The rule is **left standing and marked**, not rewritten. What replaces it is a
question this milestone opened and did not answer, and answering it after seeing
these numbers is exactly the move the section exists to prevent — see
[D-012](DECISIONS.md).

### 4.4 One arm, and the campaign stopped early

`text+vector` was never reached. Rules 4 and 6 costed it at 9.8 of the campaign's
14.6 hours, and spending that on an arm whose repetition rule is now in question
would have bought three more numbers of the same kind.

### 4.5 The collapse is not characterised

Three observations put it somewhere between "does not happen at 25.67/s" and "sheds
14% at 25.67/s". Neither the rate at which it begins nor what tips a given run into
it is measured here. Milestone 5 §3.2 placed a knee at 27.28 q/s; §1 shows a flat
rung at 25.67 and a collapsed one at the same rate, so **the knee is not a rate** —
and milestone 8's pass line is currently written as one.

## 5. What this milestone did produce

Two things, neither of them the number it went looking for:

1. **An instrument that can no longer publish a measurement it did not make.** A
   suspended rung is refused, a ladder cut short is refused, an operator-chosen rate
   no longer wears a rule's label. All three are asserted rather than commented —
   seventeen tests across `internal/loadgen`, `cmd/weft-eval` and `bench/`, the last
   of which had none before this milestone.
2. **A rung that says how far it has got**, every thirty seconds, from a goroutine
   that serves no request. Milestone 5 §4.3 named this and deferred it; a
   forty-nine-minute silence is what made the sleeping ladder cheap to miss.

## 6. Carried forward

1. **What must a repetition hold constant?** §3 says the ladder prefix is a
   variable and neither of its two readings is tested. Until it is answered there is
   no procedure that produces a spread, and [D-011](DECISIONS.md) is marked rather
   than replaced.
2. **The knee is not a rate** (§4.5). Milestone 8's pass line — shed 0 at 27.28 q/s —
   is not a predicate on the current evidence, since the same rate both passes and
   fails. It needs re-specification before it can be judged, and that is a change to
   a registered pass line, so it belongs to the PRD rather than to a plan.
3. **`text+vector` still has no published tail** (§4.4), unchanged from milestone 5.
4. **Milestone 5's headline is a single draw from a bistable rule** (§4.2), measured
   on a machine whose sleep state is unrecorded (§4.1). Neither is a reason to
   withdraw it; both are reasons not to compare against it without saying so.

---

<!-- markdownlint-disable-next-line MD025 -->
# Milestone 8 — What a repetition has to hold

**Verdict: the variable is the prefix, and holding it makes the measurement
reproducible.** The same arrival rate that gave 37.9 ms and 1.539 s in milestone 7
gives 37.827 ms again — shed 0, ten thousand samples — when it is reached as the
fourth rung of a ladder whose earlier rungs ran ten thousand samples each. Milestone
7's repetition 1 reproduces rung for rung, ninety-seven minutes of it, and the figure
the whole exercise was about lands **0.07% from its first observation**.

A repetition is therefore a ladder after all, which is what
[D-011](DECISIONS.md) denied — and the reason it denied it (three sweeps derive three
different sets of rates) is answered by naming the rates rather than deriving them.
[D-013](DECISIONS.md) is the repair, licensed in advance by
[PERF.md](PERF.md) §5.2 outcome 1.

The prefix is not a switch, though. A prefix of the same *shape* at a fifth of the
*depth* — three rungs of 2,000 samples instead of 10,000 — does not merely fail to
help; it produces the worst observation of the four.

## 1. The four runs

All at **25.67 q/s**, `text` arm, Apple M4, Go 1.26.1, `GOMAXPROCS` 10, 171,332
documents, 50 judged queries, k=10. Every run under `caffeinate -dimsu`, none printed
`SUSPENDED`, and each one's wall clock accounts for its rungs to within seconds.

| | prefix | prefix depth | `inflight` | n / shed | p50 | p99 | peak RSS | GC cycles | unloaded p50 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| A | none | — | 40 | 8,894 / **1,106** | 47.126 ms | — | 706.6 MiB | 12,922 | 36.833 ms |
| B | none | — | 10 | 7,617 / **2,383** | 369.567 ms | — | 271.2 MiB | 10,686 | 36.128 ms |
| C | 3 rungs | 2,000/rung | 40 | 1,342 / **658** | 2.088 s | — | 987.5 MiB | 611 | 35.767 ms |
| **D** | **3 rungs** | **10,000/rung** | **40** | **10,000 / 0** | **37.827 ms** | **68.179 ms** | **122.4 MiB** | **23,166** | **36.796 ms** |

The four unloaded medians span 3% — 35.767 ms to 36.833 ms — so the machine did not
drift across the two hours, and none of what follows is drift.

## 2. The reproduction

Run D is milestone 7's repetition 1, re-run with its rates named instead of derived:

| rung | milestone 7 rep 1 p50 | run D p50 | rep 1 p99 | run D p99 | shed |
| --- | --- | --- | --- | --- | --- |
| 3.21/s | 68.963 ms | 72.370 ms | 100.136 ms | 104.656 ms | 0 / 0 |
| 6.42/s | 52.719 ms | 54.277 ms | 82.835 ms | 79.082 ms | 0 / 0 |
| 12.84/s | 40.438 ms | 40.351 ms | 63.654 ms | 62.999 ms | 0 / 0 |
| **25.67/s** | **37.852 ms** | **37.827 ms** | 69.136 ms | 68.179 ms | **0 / 0** |

Whole ladder, not just the rung under test. The collector agrees to the same
precision: 23,138 cycles then, 23,166 now, a difference of 0.12% over 390 seconds.

This is the first figure in the project's history that has been measured twice and
come back. It is also, by [PERF.md](PERF.md) §3 rule 1, a figure that **may not wear
the headline label** — run D is a ladder someone named, and the rule refuses that at
one remove. That refusal is correct and it is inconvenient in a specific way §5.3
prices.

## 3. What separates the four, and what does not

**Not memory.** Run B holds 271.2 MiB — within milestone 8's own 250 MiB
neighbourhood — and sheds 2,383 requests, more than twice run A at 706.6 MiB. A run
can be small and still be drowning.

**The collector keeping up.** Normalised per second of rung, the four order
themselves exactly inversely:

| | GC cycles/s | shed/s |
| --- | --- | --- |
| D (deep prefix) | **59.5** | **0** |
| A (no prefix) | 33.1 | 2.83 |
| B (no prefix, `inflight` 10) | 27.4 | 6.11 |
| C (shallow prefix) | **7.7** | **8.28** |

Milestone 7 §3 read the same shape off two observations and called it a hypothesis
about a pacer arriving with its heap goal already grown. Four points now order
themselves by it without exception. It remains a **correlation across four runs**, not
a mechanism: nothing here instruments the pacer, and a heap-goal trace would be the
next thing that could promote it.

What run C adds is that the pacer's state is not established by *having climbed*; it
is established by having climbed **far enough**. Eighteen minutes of prefix left the
collector running at 7.7 cycles per second — an eighth of run D's rate — and produced
987.5 MiB and a third of the load shed. The two readings milestone 7 §3 left open are
therefore not symmetric: `inflight` is ruled out as the discriminator (A and B collapse
at both values), and prefix depth is what is left standing.

## 4. Where the knee is

Milestone 5 measured its own 100% rung at **27.28 q/s** and it collapsed — p50 1.27 s,
1,438 of 10,000 shed — and that rung had a deep prefix, five rungs at `-rotations 200`
([milestone 5 §3.2](#32-weft-cannot-sustain-its-own-sequential-throughput-and-bleve-can)). Run D says 25.67 q/s with a deep prefix
is flat with room to spare: its p50 is 37.827 ms against an unloaded 36.796 ms, nowhere
near rule 1's twice-unloaded bar.

So the knee sits in a **6.3% band between 25.67 and 27.28 q/s**, and a deep prefix does
not carry a rung across it. The prefix decides whether 25.67 q/s is survivable; it does
not decide 27.28 q/s.

Two cautions on the upper bound. Milestone 5's figure is a single observation, taken
before the instrument could see a suspended machine ([milestone 7 §4.1](#milestone-7--a-baseline-nobody-has-to-qualify)),
and rule 1 is bistable on this workload (§4.2 there). Nothing between the two rates has
been measured at all.

**This is milestone 8's pass line, in evidence rather than in prose.** The registered
targets are shed 0, RSS ≤ 250 MiB and p50 ≤ 100 ms at 27.28 q/s. Run D meets all three
— 0, 122.4 MiB, 37.827 ms — at 25.67 q/s, 6% below the stated rate, with no engineering
work done. Whether the milestone has any engineering left in it is one 97-minute run
away: `-rates 3.41,6.82,13.64,27.28`, the same ladder scaled to milestone 5's baseline.

## 5. Known costs

### 5.1 The registered experiment was cut in three places, and reordered

[PERF.md](PERF.md) §5.2 registered four runs at `-rotations 200`, with-prefix first.
What ran was:

1. **Reordered** — the two no-prefix arms first, because §5.2's outcome 4 would have
   made the expensive arms pointless and they cost seven minutes to rule out.
2. **A cut probe inserted** — run C, `-rotations 40`, ~20 minutes, on the argument that
   p50, shed, RSS and GC cycles all print at 2,000 samples. It did not reproduce, which
   left the depth confound the cut created, so the deep arm ran anyway. **The cut bought
   nothing but the finding in §3's last paragraph** — which is worth more than the 20
   minutes, but was not what it was spent on.
3. **One arm dropped** — deep prefix at `inflight` 10, 1.6 h. Runs A and B had already
   shown `inflight` does not gate the collapse.

So §5.2's outcome 1 is reported as fired on the strength of **one** deep prefix arm, and
its registered wording says "at both `inflight` values". Half of that condition is
untested and is carried forward.

### 5.2 A repetition now costs 97 minutes, and one arm cannot afford three

If a repetition is a named ladder, three repetitions of the `text` arm's headline is
**4.9 hours**, not the 1.6 hours [D-011](DECISIONS.md) budgeted. `text+vector` is four
times slower per query, which puts its named ladder near 6.5 hours and three of them
near 19.5 — past any reading of [PERF.md](PERF.md) §3 rule 6. Rule 4's staged depth was
built for a cheaper problem than the one that now exists, and [D-013](DECISIONS.md)
states the consequence rather than resolving it.

### 5.3 The reproducible figure is one the rule will not label

Rule 1 refuses the headline label to a ladder an operator named, and a repetition must
now be exactly that. The resolution is already in rule 3's own text — R is selected once,
by repetition 1's `-rate 0` sweep, and reused thereafter — so no rule changes. What it
costs is that the *most* trustworthy observation in this file, run D, is published as a
rung rather than a headline. Preferring that to relaxing rule 1 is
[D-013](DECISIONS.md)'s second half.

## 6. Carried forward

1. ~~**`-rates 3.41,6.82,13.64,27.28`** — the one run that decides whether milestone 8
   has engineering work in it. 97 minutes (§4).~~ **Run — §7.** shed 0 and p50 37.631 ms
   met; RSS not decidable (§8). What is left is not "the wall": it is a middle-rung
   excursion — p99 849.853 ms at 13.64 q/s under a clean top rung — and a pass line whose
   memory clause needs the PRD to choose a reading.
2. **A deep prefix at `inflight` 10** is untested, so §5.2's outcome-1 condition is
   half-satisfied (§5.1).
3. **The pacer explanation is a correlation over four runs** (§3). A heap-goal trace
   would promote or kill it, and nothing in this milestone instrumented one.
4. **Nothing between 25.67 and 27.28 q/s has been measured** (§4), and milestone 5's
   upper bound is a single caveated observation.
5. **`text+vector` still has no published tail**, and under [D-013](DECISIONS.md) it
   can no longer afford three repetitions of one either (§5.2).
6. **The middle-rung excursion is uncharacterised** (§7). Two ladders, and in one of them
   the 50% rung produced a p99 thirteen times the other's and raised the process peak by
   206.6 MiB while shedding nothing. It is invisible in p50 and it sits *below* a clean
   top rung, so nothing in the current pass line would catch it.
7. **The memory clause needs a reading chosen** (§8). Per-rung strict is undecidable with
   `getrusage`; ladder-wide is a miss at 345.2 MiB and fires milestone 10. That choice is
   a registered pass-line change and belongs to the PRD.

## 7. The pass line, judged — 27.28 q/s with the prefix held

Milestone 8's registered targets are shed 0, RSS ≤ 250 MiB and p50 ≤ 100 ms at
27.28 q/s. §4 said one run decides it. That run is
`-rates 3.41,6.82,13.64,27.28`, the same ladder scaled to milestone 5's baseline,
`-rotations 200`, `inflight` 40:

| rung | rate | p50 | p95 | p99 | shed | peak RSS | raised here | GC cycles |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 12.5% | 3.41/s | 75.480 ms | 96.165 ms | 107.807 ms | 0 | 116.6 MiB | +116.6 | 24,198 |
| 25% | 6.82/s | 48.788 ms | 77.115 ms | 139.897 ms | 0 | 138.6 MiB | +22.0 | 23,466 |
| 50% | 13.64/s | 40.974 ms | **214.244 ms** | **849.853 ms** | 0 | **345.2 MiB** | **+206.6** | 22,466 |
| **100%** | **27.28/s** | **37.631 ms** | 52.663 ms | **68.485 ms** | **0** | 345.2 MiB | **+0** | 22,902 |

**Two of three met, and the third is not decidable by the instrument that ran.**

- **shed 0 at 27.28 q/s — met.** Zero of ten thousand.
- **p50 ≤ 100 ms — met.** 37.631 ms, against an unloaded 35.332 ms.
- **RSS ≤ 250 MiB — not decidable.** The 345.2 MiB printed at that rung is the mark
  set two rungs earlier at half the rate; this rung added nothing to it. §8.

**Milestone 5's collapse at 27.28 q/s does not reproduce.** That run reported p50
1.27 s and 1,438 of 10,000 shed at this rate; this one reports 37.631 ms and shed 0.
Taken with §4, the 6.3% band closes from above as well: with the prefix held, both
25.67 and 27.28 q/s are flat, and the two figures are 0.5% apart (37.827 ms and
37.631 ms). **What milestone 5 measured at 27.28 q/s was not a property of the rate.**

**The excursion is at the middle rung, not the top.** 13.64 q/s shed nothing and still
produced a p99 of 849.853 ms and raised the process peak by 206.6 MiB. The same rung
position in the 25.67 ladder — 12.84 q/s, 6% less load — gave p99 62.999 ms and raised
the mark by 0.1 MiB. Thirteen times the tail and two thousand times the memory, and
**the p50 sees none of it**: 40.974 ms against 40.351 ms. Two ladders now place an
excursion below a clean top rung, which is not the shape "the throughput wall" names.

**Run hygiene.** 05:14:02 to 06:45:52, 91 m 50 s of wall clock against 91 m 38 s of
rungs plus a 10 s warm-up and a 1.7 s cold pass. No `SUSPENDED`. The machine did sleep
for 4.7 hours — the command was launched at 00:33 and the shell's own `date` printed
05:14:02 — but the sleep is entirely *before* the process started, so it contains no
measurement and there is nothing for the suspension check to see. A `date` either side
is what makes that statement checkable rather than assumed, and it is the habit that
caught [milestone 7 §4.1](#milestone-7--a-baseline-nobody-has-to-qualify). Unloaded p50
35.332 ms against the 25.67 ladder's 36.796 ms, 4% apart: no drift.

## 8. A rung cannot say what its own peak was

`ru_maxrss` is a high-water mark the kernel never lowers. `benchReport`'s comment has
said so since milestone 5 and the report has printed `(process)` next to the figure the
whole time. Neither stopped this milestone's own pass line from being written as a
per-rung threshold, and neither stopped this milestone from trying to judge it against a
number belonging to a different rung.

What a rung *can* say is **how much it raised the mark** — a difference between two
readings, which is what every other field on that line already is. It now says it:

```text
rusage  ... peakrss 116.1 MiB (process, raised 0.2 MiB by this rung)
rusage  ... peakrss 345.2 MiB (process, unchanged by this rung — the mark is an
            earlier one's, and this rung's own peak is only bounded by it)
```

The fix **postdates the run above** and no figure in this file moves because of it; what
changes is what a future ladder is allowed to imply. What it does not do is invent a
per-rung peak: `getrusage` has no during-this-rung reading, and giving each rung its own
process would destroy the prefix §2 has just established as load-bearing.

**So the pass line as written is not decidable on a ladder by this instrument**, and two
readings of it are available:

- **Per-rung, strict** — undecidable. The rung raised the mark by zero, so its own peak
  is somewhere at or below 345.2 MiB and was not measured.
- **Ladder-wide** — 345.2 MiB against 250 MiB is a **miss**, and milestone 10's trigger
  condition fires.

**Both are published and neither is chosen here.** Picking between them is a change to a
registered pass line, which belongs to the PRD, and the reason to prefer one of them at
this moment is which verdict it produces — the objection
[D-012](DECISIONS.md) raised about repairing rule 3 from inside the campaign that broke
it, in a different place.

**Resolved 2026-08-22, [D-014](DECISIONS.md): the ladder-wide reading, so the clause is a
miss.** The basis is what the metric is for rather than what it yields. It exists because
an adopter's first encounter with weft is a memory figure, and an adopter runs a process,
not a rung — a container sized from a steady-state number dies during the ramp. It is also
the reading milestone 5 already used when it published "RSS 126→853 MiB". So: **345.2 MiB
against 250 MiB, recorded as a miss**, charged to a ladder rather than to the rate the pass
line names, because on this platform nothing separates them without cgo or a second
module.

**Milestone 10 does not fire on it.** Its trigger is a miss after the milestone's
engineering, and milestone 8 has attempted none — the miss is the first target that
engineering has. D-014 carries that argument and what would show it wrong, including the
case where it turns out to be a way of dodging the trigger rather than a reason against it.

One comparability caveat, now that the number carries a verdict: **345.2 MiB is not
milestone 5's 853 MiB measured again.** That ladder had a fifth rung at 200% which
collapsed, and this one stops at the load point by construction, because the pass line is
about the rate it names and not about behaviour past it. The two are marks over different
ladders and the difference between them is not a measured improvement.

## 9. A correction: §7's 206.6 MiB is not the candidate decode

§7 localised the excursion to the 13.64 q/s rung and §8 recorded the memory clause as a
miss against it. [D-014](DECISIONS.md) then kept milestone 10 shut on the grounds that the
miss was a *target* — specifically the cause the PRD names, "30,549 candidates decoded per
query".

**That cause does not apply to the rung that was measured.** Every ladder in §1, §2 and §7
ran the **`text` arm**, which is `make bench`'s default. No scorer on that arm calls
`engine.Index.Doc`: `pkg/scorer/text` reads `Stats`, `Lookup` and `DocLen` and nothing
else. The candidate decode is the **vector** arm's cost, and the vector arm has never been
laddered — [milestone 7 §4.4](#milestone-7--a-baseline-nobody-has-to-qualify) and §6 item 5
here both say so.

So the attribution in D-014's *Why* was wrong: a `text`-arm measurement explained by a
`text+vector`-arm mechanism. The decision's structure survives — milestone 10's trigger
still requires a miss after engineering, and none has been attempted — but its confidence
that the target was already named does not. **The 345.2 MiB remains unattributed**, and
what the `text` arm allocates per query is now the open question: `Lookup` returns a fresh
`[]Posting` per term per query, and fusion and `TopK` build slices over the union.

### What was fixed anyway, and what it is worth

The candidate decode is real, and chasing this produced a guard for it. `Index.Doc`
decodes a whole record — key, text, links — and the vector scorer reads one field of it.
Measured on a synthetic corpus, that is **one full copy of the document text per candidate
scored**:

```text
before   4,208,024 bytes to score 64 documents, against 13,312 for the same
         vectors with 4 MiB less text
after    allocation no longer tracks text the query never reads
```

`Index.Vector` is the accessor that lets a caller say which fields it wants. It reads the
same record through the same decoder in a mode that skips the copying, keeps every check —
non-empty key, finite components, the record's own seeded checksum — and answers false for
damage, for a wrong id, and for a document carrying no vector.

**What it is worth is not measured on the corpus.** The arm it helps has no published
memory figure to improve, which is §6 item 5 and is now also the reason this saving is a
bounded property rather than a number. It is not a milestone 8 pass-line movement and is
not claimed as one.

### The invariant this spent

The PRD registered "golden API files byte-identical" for this round. The accessor adds one
line to `pkg/engine/testdata/engine_api.txt`. `pkg/fusion` is untouched, `go list -m all`
is still one line, `public_api.txt` does not move, and no existing signature changes. The
alternative — a zero-copy decode aliasing the mapping — would have kept the file identical
while silently changing what a `Document`'s lifetime means, which is a worse trade for the
same saving. [D-015](DECISIONS.md) carries the argument.

## 10. What a query allocates, measured — and the second wrong guess about the peak

§9 left the 345.2 MiB unattributed and named the open question: what the `text` arm
allocates per query. It is now measured, and the answer took two attempts, the first of
which was wrong in a way only the second instrument could see.

**The instrument first.** Each rung now prints what one query allocated — `TotalAlloc` and
`Mallocs` differences divided by the samples that did the work, both reads outside the
measured window. And `-memprofile` writes the `allocs` profile, which is cumulative since
process start and therefore attributes by call site what the rung line gives only as a
total. Cumulative also means a **twenty-second** run answers it: an allocation that happens
once per query does not need a ladder.

**The figure, and where it goes:**

```text
alloc  15691.3 KiB/query  15488 allocs/query
```

| call site | alloc_space | alloc_objects |
| --- | --- | --- |
| `engine.(*segment).lookup.func1` — the slice a `Lookup` builds | **53.2%** | — |
| `text.(*Scorer).Candidates` (flat) — accumulator map and candidate slice | **44.3%** | 4.7% |
| `decodeTermPostings` + `(*segReader).unit` | 1.4% | **59.4%** |
| everything else, index mapping included | ~1% | ~36% |

**Both halves of the byte total are whole-set materialisation, and the cause the PRD names
is neither of them.** "30,549 candidates decoded per query" is the vector arm, as §9 already
corrected; on the text arm it is `Lookup` decoding a term's *entire* posting list into a
fresh slice, per term, per query — 8.3 MiB of the 15.7 — and then `Candidates` holding an
entry per matching document twice, once in a map and once in a slice.

### The guess that was measured out

The first attempt went after the accumulator's corpus-sized size hint, which charges about
28 bytes of map per document whether the query matches every document or eight: 4.52 MiB
per query, live on every one of 40 requests in flight. Hinting from the first posting list
instead made a synthetic narrow query 236× cheaper — and made the **measured** workload
worse:

| accumulator hint | KiB/query |
| --- | --- |
| corpus-sized | 15,691.2 |
| first posting list | **19,501.6** |

A TREC-COVID query's term union really is most of the corpus, so the map doubles its way up
and every abandoned table is charged to the query: +3.7 MiB, about one extra copy of the
final map. Worse for *this* clause specifically, because during a growth the old table and
the new one are live together and the clause is a peak. It was reverted. The trade does not
vanish by taking the other side of it — a narrow query still pays 4.52 MiB for a map
holding eight entries — and what would remove it is a posting count in the terms index,
which `termSpan` does not carry.

**That is two wrong guesses about the 345.2 MiB in two milestones**, both plausible, both
named before anything measured the arm they were about. What changed is that the third
statement is a profile.

### What was fixed, and what it is worth

`Index.LookupInto` is `Lookup` writing into a caller-owned buffer. One buffer across a
query's terms holds the **longest** posting list instead of the **sum** of them, and the sum
is what a peak is made of. Measured as a property rather than a rate, on a corpus where
every term matches every document so that four extra terms are four extra lists and nothing
else:

```text
before   552,912 bytes for 5 terms against 281,232 for one — 4.1 posting lists
after    290,768 bytes for 5 terms against 281,232 for one — 0.15
```

It changes nothing about the answer: same postings, same ascending order, committed
segments before pending, and the same absence for a segment that claims a term and cannot
decode it. That last one is why this is a buffer and not an iterator — the verdict arrives
after some postings are decoded, and postings held in a buffer can be discarded where
yielded ones cannot. The read lock is held for the same span as `Lookup`'s and nothing of
the caller's runs inside it, so a scorer may still call `DocLen` per posting without meeting
the re-entrant `RLock` deadlock the unexported read path exists to avoid.

**What it is not:** a ladder figure. No campaign has run against it, so its effect on the
345.2 MiB is a prediction and not a measurement, and the 44.3% half is untouched.

### The invariant this spent

A second line on `pkg/engine/testdata/engine_api.txt`, after the one §9 spent. `pkg/fusion`
is untouched, `public_api.txt` does not move, `go list -m all` is one line, and no existing
signature changes. [D-016](DECISIONS.md) carries the argument for spending it here rather
than on an iterator, and the allocation count — 59.4% of it two small objects per posting
inside the decoder — is left alone deliberately: it is 1.4% of the bytes, so it is a
GC-pacing cost, and this milestone's clause is a peak.

## 11. The memory clause, judged — and the excursion went with it

**Verdict: all three of milestone 8's targets are met, and the ladder's peak is 100.7 MiB
against a 250 MiB clause that stood at 345.2.** The procedure and the reading of every
outcome were registered in [PERF.md](PERF.md) §5.3 before this ran, which
`git log --oneline -- docs/PERF.md` is where to check.

`-rates 3.41,6.82,13.64,27.28`, `-rotations 200`, `inflight` 40, `text` arm, Apple M4 /
Go 1.26.1, 2026-08-24 04:57:17 to 06:29:03 KST:

| rung | rate | p50 | p95 | p99 | shed | peak RSS | raised here | alloc/query | GC cycles |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 12.5% | 3.41/s | 78.576 ms | 91.988 ms | 100.857 ms | 0 | 99.0 MiB | +3.7 | 10,868.9 KiB | 5,009 |
| 25% | 6.82/s | 50.825 ms | 71.173 ms | 77.613 ms | 0 | 99.0 MiB | +0 | 10,868.9 KiB | 5,206 |
| 50% | 13.64/s | 34.124 ms | 45.105 ms | **53.868 ms** | 0 | 99.0 MiB | **+0** | 10,868.9 KiB | 5,212 |
| **100%** | **27.28/s** | **33.470 ms** | 45.407 ms | 54.825 ms | **0** | **100.7 MiB** | +1.8 | 10,869.0 KiB | 5,176 |

- **shed 0 at 27.28 q/s — met.** Zero of ten thousand, and zero on every rung below it.
- **p50 ≤ 100 ms — met.** 33.470 ms against an unloaded 32.231 ms, a ratio of 1.04 and
  nowhere near rule 1's twice-unloaded saturation bar.
- **RSS ≤ 250 MiB — met.** The **ladder's** peak, which is the reading
  [D-014](DECISIONS.md) fixed: **100.7 MiB**. §7's ladder reached 345.2.

**So [PERF.md](PERF.md) §5.3 outcome 1 fires: milestone 10 does not fire, and closes as a
conditional that was never triggered.** Its trigger was a miss after this milestone's
engineering; the engineering happened and there is no miss.

### The excursion moved with the memory, so it had one cause

§7's 50% rung is the figure §6 item 6 carried forward as uncharacterised: p99 849.853 ms and
the process peak raised by 206.6 MiB, while shedding nothing, sitting *below* a clean top
rung where no pass-line wording would catch it.

| 13.64 q/s | §7 | now |
| --- | --- | --- |
| p99 | 849.853 ms | **53.868 ms** |
| raised the mark by | 206.6 MiB | **+0 MiB** |
| p50 | 40.974 ms | 34.124 ms |

**Fifteen times less tail and none of the memory**, at the same rate on the same ladder.
§5.3's outcome 4 registered the reading in advance: if the tail and the memory move
together, they had one cause and it is named. They moved together, and the cause is the one
§10 attributed — a term's whole posting list materialised per term per query.
Carried-forward item 6 is discharged.

### What the run also says, including one thing it does not explain

- **Allocation is flat across the ladder and identical to the smoke run.** 10,868.9 to
  10,869.0 KiB/query over four rates spanning 8×, against 10,869.0 in a 20-second run. The
  figure is a property of the query set and not of the load, which is what makes it usable
  as a before-and-after at all.
- **The ladder is the same ladder** even though the machine was not in the same state: the
  unloaded p50 was 32.231 ms against §7's 35.332 ms, 8.8% apart. Because the rates are
  **named** rather than derived, that difference moves the saturation *ratio* and not the
  load points — which is the property [D-013](DECISIONS.md) bought, paying for itself here
  for the first time.
- **GC cycles fell 4.5× and this run does not explain it.** 5,009–5,212 per rung against
  §7's 22,466–24,198, while allocation fell 1.44×. Worse for the reading §3 offered: that
  section ordered four runs inversely by cycles per second and shed per second, and this is a
  fifth point with the **lowest** collection rate of any of them — about 1.7 cycles per
  second — and **shed 0 everywhere**. §3 called itself a correlation across four runs rather
  than a mechanism. It is now a correlation a fifth point contradicts, and nothing here
  instrumented the pacer either.
- **Run hygiene.** 91 m 46 s of wall clock against 91 m 38 s of rungs plus the warm-up and a
  1.6 s cold pass. No `SUSPENDED`. `p99.9` prints `--` on every rung, correctly:
  [PERF.md](PERF.md) §2.3's floor wants 100 samples beyond the quantile and 10,000 does not
  reach it.

### One observation, not three

**The repetitions registered in §5.3 were not run**, and each figure above is a **single
observation**. The cut is recorded beside the figure rather than discovered later, and what
it costs is stated exactly: milestone 7 measured one rate three times and got 37.9 ms,
1.539 s and 416 ms, so a single observation on this workload is known to be one draw. What
differs from milestone 7 is that the variable it could not hold is now held and named — the
ladder prefix, at depth — and this run reproduces §7's ladder shape rung for rung.

The three quantities the verdict rests on are also the three least likely to be a draw: shed
is 0 with no near miss, the peak is 100.7 against a bar of 250 rather than 249, and
allocation per query is deterministic to four significant figures across five runs.

---

<!-- markdownlint-disable-next-line MD025 -->
# Milestone 9 — The write-lock ceiling

Two clauses, and both point at the same place. **A read arriving while a commit is in
flight must wait no more than 1 second**, against the 12.539 s milestone 5 §5 published.
And **an operator must be able to call a commit off**, which `Commit` could not be asked to
do: it took no context, and the Limitations table said so.

## 1. What the 11 seconds were, and what had to be exclusive

Milestone 5 §3.3 measured an 11.063 s window in which the writer held `ix.mu`. Of that,
20,000 `Add` calls were 49 ms. The remaining 11.014 s was the commit itself, and nearly all
of that was `buildIVF` — 142 centroids refined over 20,000 documents by five Lloyd passes,
then every document assigned.

**That computation only reads the index.** What has to be exclusive is the manifest rename
and the `adopt` that follows it: one `openSegment` and one `clear`. So the lock was not
removed, it was split.

| section | lock | what runs there |
| --- | --- | --- |
| 1 | `wmu` + `mu.RLock` | read the manifest, encode the segment, rename — the whole 11 s |
| 2 | `wmu` + `mu.Lock` | `adopt` + `rememberDir` — one mapping and one clear |
| 3 | `wmu` only | `prune`, best-effort, touching no index state |

`Index.wmu` is the load-bearing half and not bookkeeping. `sync.RWMutex` prefers writers: a
goroutine blocked in `mu.Lock` makes every later `RLock` queue behind *it* rather than join
the readers already inside, so a single concurrent `Add` would have restored the entire
stall and merely changed its trigger. `wmu` keeps a mutator from reaching `mu.Lock` while a
commit encodes. All four exclusive sites take it first, and
`grep -n 'mu.Lock()' pkg/engine/*.go` is the whole enumeration.

It also bought a cheaper design than the `ponytail:` note on `Commit` predicted. That note
asked for the captured documents to be counted, so an `Add` arriving mid-encode is neither
written twice nor dropped — a partial hand-off of `docs`, `byKey`, `postings`, `docLen`,
`totalLen` and `base`. Excluding `Add` for the whole commit means the pending segment cannot
change, so there is nothing to count and none of that was written. The price is that `Add`
blocks for a whole commit, which is what it already did.

**This also answers an open question the PRD carried** — whether learning the partition
outside the lock breaks milestone 2's commit atomicity. It does not. The commit point is
still the manifest rename, and under `wmu` the pending set cannot change, so what is written
to the segment is exactly what was pending when the commit was admitted. A document arriving
during training is not "undefined"; it arrives *after* the commit.

## 2. Cancellation is a corollary of the rename, not a new rule

`Commit` now takes a `context.Context`, and where a cancellation lands is what it means:

- **Before the rename** — `ctx.Err()` is reported and the index is untouched. The pending
  documents are still pending and the caller may commit again. What may be left in the
  directory is an unnamed `seg-<gen+1>`, which is *already* a defined state: `Open`
  documents it as the debris of a commit that never finished, ignores it, and the next
  `Commit` sweeps it. No cleanup path was added.
- **After the rename** — the commit has succeeded and `ctx.Err()` is ignored for the rest of
  the call. Stopping there would leave the directory publishing a generation the live index
  does not know it holds, and the next `Commit` would then fail the `stored == base`
  agreement and report the directory as corrupt. **A cancellation is not allowed to reach a
  state a crash cannot.**

The poll is not per document: entry, each section boundary of the encode, each of the five
Lloyd passes, every `ivfAssignPoll` (1,024) documents of the assignment pass, and immediately
before the rename. The coarsest unpolled stretch is one walk of the training sample, bounded
by `ivfSample` positions. A poll inside `ivfDot` would put a load and a compare in a loop
that runs `count·nlist·dim` times, which is responsiveness nobody can perceive bought with a
branch in the writer's hottest loop.

`Merge` did **not** get a context, and did not get its lock split either. The same
restructuring applies to it almost verbatim and it is a *longer* stop than a commit — it
rewrites the oldest generations rather than one batch. It is carried forward rather than done
because nothing measures it: `weft-eval bench -writes` times a commit and there is no arm
that times a merge. A reordering claimed as an improvement without a number is the failure
this file exists to stop.

### The invariant this spent

**One line of `pkg/engine/testdata/engine_api.txt` changed rather than added**, which is a
first: §9 and §10 of milestone 8 each spent a new line, and this is the first existing
signature to move.

```diff
-method Index.Commit(string) error
+method Index.Commit(context.Context, string) error
```

Adding `CommitContext` and keeping `Commit` was the alternative, and it was rejected before
anything was written. `v0.1.0` is not cut, so there is no compatibility to keep, and an
`XxxContext` pair planted at a point where nothing has to be preserved becomes permanent
surface. `Search` already takes a `context.Context` first, so two entry points with different
conventions is the more expensive outcome. [D-017](DECISIONS.md) carries the argument and both
rejected alternatives.

Everything else held: `pkg/fusion` is untouched, `public_api.txt` does not move, `go list -m
all` is one line, and the 82 call sites are all tests, `cmd` and `examples` — a one-time
mechanical churn traded against permanent surface.

**Segment bytes did not change**, which is asserted rather than argued.
`TestIVFTrainingIsDeterministic`, `TestSegmentRoundTrip` and the golden-segment comparisons
all pass under `-race`: a lock mode or a context poll that changed an encoding would be a
bug, not a speedup.

## 3. The read clause, judged — 61 ms against a 1-second bar, and the window did not shrink

**Verdict: milestone 9's read clause is met.** The worst read due inside a commit waited
**61 ms**, against a clause of 1 second and against **13.072 s** measured on the same tree
lineage forty-seven minutes earlier. The procedure and the reading of every outcome were
registered in [PERF.md](PERF.md) §5.4 before either run, which
`git log --oneline -- docs/PERF.md` is where to check.

`-writes -writedocs 20000`, `-rotations 200`, `inflight` 40, `text` arm, Apple M4 / Go
1.26.1. `before` on `f0fcedc` (the RED commit, engine untouched) 2026-08-24 22:25:47 to
23:10:25 KST; `after` on `cacc258` 23:12:49 to 23:56:54 KST.

| | before | after | |
| --- | --- | --- | --- |
| unloaded p50 | 33.322 ms | 32.916 ms | 1.2% apart — the machine is the same machine |
| arrival rate | 3.75/s | 3.80/s | derived from the unloaded p50, hence the difference |
| commit window | 11.467 s | 11.284 s | **unchanged** |
| the commit itself | 11.413 s | 11.233 s | unchanged |
| **`during` max** | **13.072 s** (n=40) | **61 ms** (n=43) | **214×** |
| `outside` p50 | 72.004 ms | 70.270 ms | unchanged |
| `outside` p95 | 90.068 ms | 90.279 ms | unchanged |
| `outside` max | 1.676 s | 191 ms | 8.8× — see §4.2 |
| shed | 4 | 0 | |

**So [PERF.md](PERF.md) §5.4 outcome 1 fires.** `during` max is 61 ms against a 1-second
bar, and it is **below** `outside` max at 191 ms: the commit window is no longer the worst
part of the run. Outcome 4 — the blocking one, a cancelled commit publishing a generation —
did not occur, and `TestACancelledCommitPublishesNothing` is what stands over it.

**The window did not shrink, and that is the design rather than a disappointment.** 11.467 s
of held lock became 11.284 s of held lock; the encode is exactly as long as it was. What
changed is that the long part of it is no longer exclusive. Milestone 5 §3.3's number was
never "the commit is slow" — it was "the commit is slow *and* nothing may read during it",
and only the second clause was worth attacking. A reader who wanted the first should read
[FINDINGS milestone 3b](#milestone-3b--the-vector-scan) on what the partition costs to build.

**The before run reproduces milestone 5 §3.3 on this tree**, which is the whole reason it was
run: 11.467 s of window against 11.063 s and 13.072 s of worst read against 12.539 s — 3.7%
and 4.3% apart, four milestones and one read-path change later. Without it, the fall could not
be attributed to this round rather than to [D-016](DECISIONS.md)'s, and that misattribution is
one this file has had to correct once already (§9 of milestone 8).

## 4. Known costs

### 4.1 One observation per side

[PERF.md](PERF.md) §5.4 registered three `after` repetitions and cut order for dropping them.
**Cuts 1 and 2 were both taken**: each figure above is a **single observation**, and this
sentence rather than a later reader is where that is said. What survives the cut is the
comparison itself — `before` and `after` are one observation each, same procedure, same
machine, forty-seven minutes apart — and the margin the verdict rests on is 16× rather than a
few percent.

The debt is the same one milestone 5 §4.5 and milestone 8 §11 carry, marked the same way. It
is not equally dangerous here: milestone 7 found a *load point* irreproducible, and this arm
measures the writer's own lock rather than a knee, which is why the two lock windows agree to
1.6% across two runs and two trees.

### 4.2 `outside` fell 8.8× and this run does not prove why

`outside` max went 1.676 s to 191 ms — a cohort the fix should not have touched, since it is
by definition the reads due when no commit was running.

The candidate is drainage. `loadgen.SplitByWindow` classifies a sample by its **due** time, so
a request due a millisecond after the window closed but queued behind the backlog an
11-second stall had built is counted `outside` and carries the stall's cost. That would make
the before run's 1.676 s the same event as its 13.072 s, measured on the other side of a
boundary drawn in due-time.

**It is a candidate and not a finding.** Nothing here instrumented queue depth, and the
instrument cannot separate the two populations that `outside` actually holds — which is the
next item.

### 4.3 `during` max is below `outside` p50, and that comparison is refused

61 ms against 70.270 ms. Forty-three samples all landing under the median of nine thousand is
not something forty-three samples can establish, which is exactly the floor
[PERF.md](PERF.md) §2.3 draws — and it is why `during` p50 prints `--` rather than a number.

There is also a reason the two cohorts may not be comparable at all, and it is structural
rather than statistical. **`outside` is not one population.** The commit fires a third of the
way into the run, so a third of that cohort is reads against a one-segment index and
two-thirds are reads against a two-segment index holding 20,000 more documents — every point
query walks the segment list, and `Lookup` merges across it. The `during` cohort sits in the
transition. Comparing a maximum from one index shape against a median mixed from two is not a
comparison, and the arm reports no split that would fix it.

What the verdict rests on is therefore the **absolute** figure — 61 ms against 1,000 — and the
**before-and-after on the same cohort**, 13.072 s to 61 ms. Neither needs the cross-cohort
comparison, and this section is here so that nobody later reads one into the table.

### 4.4 Accuracy, checked rather than assumed

`make eval`: nDCG@10 **0.5826** (`text`) and **0.6211** (`text+vector`), identical to four
decimals to the published figures and inside the −0.005 tolerance by construction. A lock
restructuring cannot change an answer — that is an argument, and this run is the argument's
check.

## 5. Carried forward

1. **`Merge` has neither a context nor a split lock.** It is a *longer* stop than a commit and
   this round did not measure it. `weft-eval bench -writes` times a commit and no arm times a
   merge; building that arm is what licenses the same restructuring.
2. **`Add` blocks for a whole commit.** Recorded as a ceiling on `Index.wmu` with the way out
   named (the capture counting the `ponytail:` note described). Owed the day a caller needs to
   ingest during a commit.
3. **The `-writes` arm cannot split its own baseline.** §4.3: `outside` mixes two index shapes
   and §4.2's drainage hypothesis needs the same split to be testable. A per-third or
   pre/post-window breakdown is the instrument change, and it is cheap.
4. **The commit is still 11 seconds.** Nothing here attacked that, and
   [milestone 3b](#milestone-3b--the-vector-scan)'s `ponytail:` note on `writeSegment` still
   names the structure that would — a `vectors` section, so the assignment pass stops decoding
   four fields to read one.
5. **Three `ctx.Err()` polls have no test that lands on them deterministically** (the two
   `writeSegment` section boundaries and the Lloyd-pass poll). See
   [the TDD report](testing/weft-m9.tdd.md) for why pinning them was declined.

---

<!-- markdownlint-disable-next-line MD025 -->
# Milestone 11 — Deletion and update

> In progress. The verdict sections are written as the round produces them; what
> is here already is what has been paid for.

## 1. The exported API cost, recorded before the golden moved

`architecture_test.go` asks that a change to engine's surface be a deliberate
edit rather than a passing one, and that the author write down what it bought
before refreshing the golden. This is that entry.

| Line added to `pkg/engine/testdata/engine_api.txt` | What a caller gets |
| --- | --- |
| `method Index.Delete(string) bool` | Removes the document with a Key and reports whether there was one |
| `method Index.Update(Document) (DocID, error)` | Replaces the document holding a Key, returning the id it now has |
| `var ErrNoSuchKey` | What `Update` reports for a Key no live document holds |

**Three lines.** `Delete` is `bool` rather than `error` because the only way it
fails is that the key was not there, and a segment too damaged to answer reads as
absence everywhere else in this package ([D-006](DECISIONS.md)); it is the shape
`Resolve` already has. `Update` can fail four ways that are not that — an empty
key, a non-finite vector, a mismatched width, a key nobody holds — so it returns
an error, and the `DocID` it returns is not decoration: an update of a
*committed* document cannot rewrite the segment holding it, so it tombstones the
old record and appends a new one under a new id.

`Update` refuses an unknown key rather than inserting, which is
`ErrDuplicateKey`'s rule read from the other side: a mistyped key must not
silently become a second document, and it must not silently overwrite one either.

`public_api.txt` is unchanged, `pkg/fusion` is unchanged, and no scorer
implementation file changed — which is the milestone's actual claim and is
judged in full below once the round closes.

## 2. What `Len` stopped meaning

`Len` counted documents and was also one past the highest `DocID`. A tombstone
makes those two different numbers, and this round kept `Len` as the **id bound**
and moved the population to `Stats`.

The split is forced by a scorer that never mentions deletion. `scorer/recency`
walks `for i := range ix.Len()` and skips whatever `Doc` refuses; narrowing `Len`
to the live count would stop that walk short of the newest documents and return a
wrong ranking rather than a slow one. `Stats` is what BM25 normalizes against, so
leaving tombstones in *that* number is what would be quietly wrong. Each half now
answers the caller that needs it.

The cost is that a corpus with most of its documents deleted is still walked in
full by a scorer shaped like `recency`, because nothing here reclaims an id.

## 3. The falsification condition, judged

The PRD fixed this before the round started: *does the index's account of which
documents exist leak into the scorers' account?* If deletion had forced a wider
`Scorer`, a wider `Query` or a line of `pkg/fusion`, then "fusion does not know
what a signal is" would have been true only for a corpus that never changes.

**It did not fire.** Measured the way milestone 1 measures it:

| Clause | Result |
| --- | --- |
| `pkg/fusion` diff | **0 lines** |
| `pkg/scorer/*` implementation diff (tests excluded) | **0 lines** |
| `Scorer` interface | unchanged |
| `Query`, `Document`, `Candidate`, `Fuser` | unchanged |
| `pkg/engine/testdata/public_api.txt` | unchanged |
| `pkg/engine/testdata/engine_api.txt` | **+3 lines**, §1 |
| `go list -m all` | one line |

What made it hold is one placement decision. The four scorers reach candidates
through exactly seven methods — `text` through `LookupInto`, `vector` through
`Nearest` and `Vector`, `graph` through `Doc` and `Resolve`, `recency` through
`Len` and `Doc` — and the tombstone check went **inside** them rather than beside
their callers. No scorer in this repository contains the word.

That is a weaker result than it looks, and the weakness is worth writing down:
these are *this repository's* four scorers, and the seven methods are the ones
they happen to use. A scorer written outside the module reaching for a method
that does not filter would find one — there is none today, because every read
method filters, but "every read method" is a property maintained by hand and not
by the type system. The check that would make it structural does not exist.

### 3.1 What §3.4 was wrong about

[Milestone 2 §4](#milestone-2--persistence) carried this forward: *"Two places
depend on `DocID` increasing densely — the tiebreak in `engine.TopK`, and
postings staying sorted because appends are monotonic. Deletion and segment merge
break that invariant; design tombstones and generations first."*

Deletion is built and neither place broke, because **the property those two need
is monotonicity, not density.** `TopK` breaks ties on `DocID` and a sparse id
space orders exactly as well as a dense one. Posting lists stay ascending because
ids are still assigned in increasing order; the gaps a tombstone leaves change
nothing about the comparison.

What density is actually load-bearing for is the *docs* section, where a `DocID`
is a position — and that is why nothing is reclaimed ([D-019](DECISIONS.md)).
The note was right that ids and deletion are one problem. It named the wrong two
places.

## 4. Quality, judged — run C of the registered procedure

`docs/PERF.md` §5.5 registered three runs before any of them was executed. This
is run C. **Runs A and B have not been executed**, so the performance-invariance
clauses of this milestone are unjudged; §5 says what that leaves open.

```console
$ make eval
08:37:28 arm text                              nDCG@10 0.5826  (6.613s)
08:37:39 arm text+vector                       nDCG@10 0.6211  (10.823s)
```

| Arm | Published | This round | Delta | Tolerance |
| --- | --- | --- | --- | --- |
| `text` | 0.5826 | **0.5826** | 0.0000 | −0.005 |
| `text+vector` | 0.6211 | **0.6211** | 0.0000 | −0.005 |

Identical to four decimals, which is **reading 1** of the four §5.5 fixed in
advance. One observation, Apple M4 / go1.26.7, 2026-08-25 — not a median, per
[D-013](DECISIONS.md).

The argument this was checking is the one §5.5 named: every tombstone check takes
an empty-set fast path, and the evaluation corpus has no deletions, so the
scoring path should be the one that earned these numbers. An identical figure is
consistent with that and does not prove it — what would have disproved it is any
movement at all, since nothing else in this round touches scoring.

### 4.1 It also read a version 3 index, on the real corpus

`.eval-data/index/MANIFEST` is stamped version **3**, written before this round
existed. `make eval` opened it, ran five arms over 171,332 documents and returned
the published figures, with no conversion and no rebuild.

That is the format metric — *existing v3 segments are read unconverted* —
measured on a real corpus rather than only on the fixture
`TestAV3GenerationOpensAndAnswers` builds. It was free, in the sense that nothing
was done to obtain it: it is what running the quality suite at all now requires.

## 5. Carried forward

1. **The performance-invariance clauses are unjudged.** Shed 0 at 27.28 q/s,
   p50 ≤ 40 ms, ladder peak RSS ≤ 120 MiB, worst read inside a commit window
   ≤ 1 s — none measured, none failed, and nothing in this round should be read as
   claiming them. Run A was attempted on 2026-08-25 and died during the index load
   before the first rung reported, so there is not even a partial ladder; run B was
   not attempted. The procedure, the four readings and the cut order are registered
   at [PERF §5.5](PERF.md) and the run block is runnable as written — about 2.3
   hours of machine time.

   What that leaves resting on an argument rather than on a number: every tombstone
   check takes an empty-set fast path, so an index with no deletions should be
   running the code that earned those figures. §4 is one piece of evidence for it —
   the quality suite reproduced its numbers exactly, and nothing else in this round
   touches scoring — and a latency and memory figure is the piece that is missing.
2. **The cost of a tombstone at query time has no instrument.** Run B needs a
   `-deletefrac` flag `cmd/weft-eval` does not have. Until it exists,
   [D-019](DECISIONS.md)'s question — *what deleted fraction forces a re-index* —
   has no number on either the RSS or the recall side.
3. **Nothing reclaims.** Deletion and update grow the directory monotonically and
   spend `DocID`s that are never returned. [D-019](DECISIONS.md) prices it and
   names the trigger for revisiting; `docs/FORMAT.md` §8 publishes it.
4. **`Index.Nearest` weakened and it is unmeasured.** Tombstones are filtered
   after a segment has widened its own probe, so its "at least k candidates"
   contract no longer holds under deletion and recall falls as the deleted
   fraction rises. No measurement exists.
5. **The read-path filter is maintained by hand.** Every read method filters
   today and nothing structural keeps a future one from forgetting — §3 states
   this as the limit of the architecture result.

---

<!-- markdownlint-disable-next-line MD025 -->
# Milestone 12 — Query expressiveness

## 1. The prediction, and what happened to it

The PRD's milestone 12 row carried three clauses. Two were outcomes; the third
was a prediction:

> **Milestone 6's defects 2 and 3 move from documentation repayment to code
> repayment.**

[ADOPTION §7.1](ADOPTION.md) registered the opposite prediction before either
trial ran — **zero code-required blockers in both tasks** — on three grounds that
were already in the repository: milestone 6's task B had passed the same shape of
question at a `pkg/` diff of 0, the prose repayment was already in three places,
and `graph.New(ix, txt)` proved a scorer can take a scorer.

**The registered prediction holds.**

| | task C | task D |
| --- | --- | --- |
| docs-closable blockers | 1 | 3 |
| **code-required blockers** | **0** | **0** |
| source files opened | 0 | 0 |
| implementation lines (budget 100) | 84 | 86 |
| `pkg/` diff | 0 | 0 |

Two agents, neither able to see the other's task, both under budget, neither
needing a new exported name, a new `Document` field or a new `Query` field.
Under [ADOPTION §7.5](ADOPTION.md) this is **reading 1**, chosen explicitly and
not after the fact.

**So the PRD's third clause is wrong, and this is where that is published.** The
extension point milestone 12 was expected to build does not exist, because
nothing asked for it. What the round produced instead is four documentation
defects, one of them worth more than the API would have been.

**Reading 4 did not fire either.** A field restriction is expressible without an
on-disk format change, so the outcome clause *"a text-side constraint can be
expressed"* is **met** on both halves — phrase and field. The plan rated that
clause's failure High and prepared a carry-forward for it; it was not needed.

## 2. The blockers

| # | Task | Blocker | Class | How it was resolved |
| --- | --- | --- | --- | --- |
| 4 | D | `fusion.Fuse` is a union of votes, so a constraint scorer ranks rather than filters and every refused document returns on another stream's vote — silently | docs | A 22-line `Fuser` reading the last stream as a restriction. Exported API only |
| 5 | D | `Posting` has no positions, so a phrase can only be decided by decoding document text; nothing named the cheap shape | docs | A scorer reading `Document.Text`. The subject swept the whole corpus, at `O(documents × doc length)` per query |
| 6 | D | The index has no field concept, so a field restriction is a caller-side convention | docs | A title table keyed by `Document.Key`, joined with `Index.Resolve` |
| 7 | C | `go doc` renders no Examples, so the answer milestone 6 put in `ExampleScorer` is invisible from a terminal | docs | Not resolved by the subject — it built from the doc-comment prose instead, which was sufficient |

All four are documentation defects by the [§3 rule](ADOPTION.md): the exported
API already permitted every one, and the subjects established that by attempting
the arrangements rather than by judging difficulty.

### 2.1 Defect 4 is what the round bought

Every signal weft's adoption trials have produced until now was **additive** — a
view count, a distance, a topic match — and rank fusion is built for exactly
that. Milestone 1's claim is that fusion need not know what a scorer is, and for
an additive signal that holds without qualification.

The first constraint anybody writes is **subtractive**, and there the same
architecture is silently wrong. A scorer returning only the documents that
satisfy a phrase does not exclude anything: RRF sums over the streams a document
appears in, so absence from the constraint stream costs a document nothing in the
text stream, and what the constraint refused comes back. No error, no empty
result — a plausible ranking containing exactly the documents the caller asked to
be rid of.

**The architecture answers this without changing**, and that is the finding rather
than a rescue. `Search` already takes the `Fuser` as a parameter, so an
intersection is 22 lines the caller writes, and `pkg/fusion` does not move. The
defect was never that it was impossible; it was that nothing said it was
necessary, and the failure mode is silence.

### 2.2 How defect 4 was nearly missed, which is a limit of the instrument

Trial D's own demonstration ran each constraint as the **sole** scorer in the
`Search` call, where the trap cannot appear, and reported PASS. The defect
surfaced only when the subject was asked to run the composition an adopter would
actually ship — the constraint alongside `text.New(ix)`. It then produced
`REAPPEARED phrase-nearmiss-order` and five more like it.

Nothing in [ADOPTION §2.3](ADOPTION.md) required that composition. A task phrased
as *"make this work"* is discharged by the arrangement in which it works, and
both milestone 6 tasks and both milestone 12 tasks were phrased that way. This is
recorded as an instrument limit in [§8.1](ADOPTION.md) rather than as a lucky
catch.

## 3. Milestone 6's repayment, judged

[D-010](DECISIONS.md) registered the signal that would show it was wrong: *the
same question keeps being asked after `ExampleScorer` and three sentences are in
place.* Read here, the answer splits in two.

**The prose worked.** Task C is task B made harder along two axes — a
corpus-sized store that cannot be rebuilt per query, and two scorers contending
for one input — and it produced **no blocker at all** on the query-time input.
The subject named `Document`'s and `Query`'s doc comments as its source and built
from them in one pass, first build and first run. The three sentences milestone 6
wrote are the reason this round found nothing to repay on the `Query` side.

**The Example did not.** `go doc` renders no Examples — `go doc ./pkg/engine
ExampleScorer` answers `no symbol ExampleScorer in package` — so for anyone
reading from a terminal, the artifact D-010 chose as its repayment vehicle has
been dead since it was written. Only pkg.go.dev renders it. [ADOPTION
§2.1](ADOPTION.md) asserted the opposite for two milestones, and the trial
protocol was therefore wrong about what it was handing its own subjects.

D-010's judgment — *put the answer where `go doc` renders it* — was right. Its
belief that an `Example` is such a place was false, and only a subject that went
looking for one found out.

## 4. What this milestone cost

**Golden API: zero lines, against a budget of zero.** `engine_api.txt` and
`public_api.txt` are byte-identical. No exported name, type, field or signature
changed.

`pkg/engine`'s non-test diff is **38 lines added and 1 removed, and every one of
them is a comment**:

```console
$ git diff --stat -- 'pkg/engine/*.go' ':!*_test.go'
 pkg/engine/doc.go    | 12 +++++++++++-
 pkg/engine/index.go  | 10 ++++++++++
 pkg/engine/search.go | 17 +++++++++++++++++
 3 files changed, 38 insertions(+), 1 deletion(-)
```

`pkg/fusion` and `pkg/scorer/*` are untouched, `go list -m all` still prints one
module, and `Scorer`, `Fuser`, `Candidate` and `Document` are unchanged. The
PRD's first-stage falsification condition did not fire and was never close to
firing.

The three comments are the repayment: `Fuser` now states that fusion is a union
and that a restriction lives in a fuser, `Posting` now states that it carries no
positions and what that forecloses, and `Query` now states that two scorers
needing one value construct from it together.

## 5. Performance, argued rather than measured

No performance run was bought, and the argument is the diff above: **not one
executable line changed.** Every altered line in `pkg/engine` is a comment, and
`pkg/fusion` and every scorer are byte-identical, so the binary that produced
milestone 8's and milestone 9's figures is the binary this milestone ships. There
is no scoring-path change for a run to detect.

That argument was fixed in the plan before the round began, together with the
condition that would void it — *if the `pkg/` implementation diff is not zero, buy
the run.* It is zero in the sense the condition meant: zero statements.

**The quality suite was run anyway**, because it is minutes rather than hours and
it is the one measurement that would catch a scoring change nobody intended:

```text
text                0.5826
text+vector         0.6211
```

Both identical to the published figures, well inside the −0.005 tolerance.

## 6. Carried forward

1. **Milestone 11's performance clauses are still unjudged.** Shed 0 at
   27.28 q/s, p50 ≤ 40 ms, ladder peak RSS ≤ 120 MiB, worst read inside a commit
   window ≤ 1 s. Run A, run B and the `-deletefrac` instrument are all still owed;
   [milestone 11 §5](#milestone-11--deletion-and-update) holds the detail, and
   nothing in this round changes or discharges any of it.
2. **A position index and per-field term spaces are format v5**, and neither is
   planned. `FORMAT.md` §8 carries them with what they would cost. This is not a
   blocker — both constraints are expressible today — it is the price of the
   expressible version being a per-candidate decode.
3. **The wrapping shape is documented but unmeasured.** `Posting`'s comment now
   says to wrap a scorer rather than sweep the corpus, and nothing measures the
   difference. Trial D's sweep — every document decoded and tokenized per query —
   is what an adopter writes without that sentence, and how much the sentence
   saves has no number.
4. **A restriction expressed as a `Fuser` is a positional convention.** The
   caller decides which stream restricts, and nothing checks that they and their
   own scorer list agree. Getting the order wrong restricts by the wrong stream
   and returns a plausible ranking, which is the same silent failure class as
   defect 4 itself, one level up.
5. **Trial tasks are still phrased as "make this work".** [§2.2](#22-how-defect-4-was-nearly-missed-which-is-a-limit-of-the-instrument)
   is the cost of that, and no future task is required to include the composition
   an adopter would ship. A next round should fix the protocol, not just this
   round's finding.

---

<!-- markdownlint-disable-next-line MD025 -->
# Milestone 13 — The tokenizing seam

## 1. The budget, registered before the code

Milestone 12's answer was zero implementation lines, and that is not available
here: `engine.Tokenize` is a function, and a function is not made replaceable by
a sentence. So the one place this round can be honest is the one milestone 12 did
not need — **write the line count down first, then spend it, then publish the
two side by side.** That is [D-015](DECISIONS.md)'s order, and everything in this
section was committed before the first line of implementation.

### 1.1 The golden budget — 5 to 7 lines

The seam attaches to the constructor. Four rungs were priced and three rejected
before anything was written; [D-022](DECISIONS.md) carries the ladder. Rung 1 —
`New(opts ...Option)` / `Open(dir, opts ...Option)` plus `WithTokenizer` — is
what the budget below is for.

| Item | `engine_api.txt` | Note |
| --- | --- | --- |
| `type Tokenizer func(string) []string` | +1 | |
| `type Option` and whatever members it renders | +1 to +2 | The rendered shape is not known yet; it is recorded when it is spent |
| `func WithTokenizer(Tokenizer) Option` | +1 | |
| `method Index.Tokenize(string) []string` | +1 | The query side's way in |
| `func New(...Option) *Index` | 1 changed | |
| `func Open(string, ...Option) (*Index, error)` | 1 changed | |
| `var ErrTokenizerMismatch` | +1 | [§1.4](#14-the-guard-is-a-recomputation-not-a-stored-name) |
| **Total** | **5 to 7** | `public_api.txt` holds only what is outside `pkg/engine`, so **0 expected** |

Overspending is allowed and silence is not: the overspend and what demanded it
go here **before** `WEFT_UPDATE_GOLDEN=1` runs.

### 1.2 The invariants, fixed in advance

| Subject | Budget |
| --- | --- |
| `pkg/fusion` | **0 lines** |
| `Scorer`, `Fuser`, `Candidate`, `Document`, `Query` | **unchanged** |
| `pkg/scorer/text` | **1 line** (`engine.Tokenize` → `s.ix.Tokenize`) plus comment |
| `pkg/scorer/{graph,recency,vector}` | **0 lines** |
| `go list -m all` | one line |
| On-disk format | **v4 unchanged.** v3 and v4 segments still read with nothing converted |
| nDCG@10 | 0.5826 (`text`) / 0.6211 (`text+vector`), tolerance −0.005 |

If `pkg/fusion` moves or `Scorer` has to widen, the round stops there and the
falsification condition is judged rather than the plan repaired.

### 1.3 Where the falsification condition is read

One line: `pkg/scorer/text/text.go`, the call that today reads
`engine.Tokenize(q.Text)`.

- **`s.ix.Tokenize(q.Text)`** — the scorer *asked* the index, the same shape as
  `ix.Lookup` and `ix.Stats`, and signal-neutrality holds.
- **A tokenizer reaching the scorer through `Query` or `Scorer`** — the index's
  account leaked into the scorers', which is stage 1 of the PRD's falsification
  condition. It is then published with the hypothesis narrowed, not reverted.

### 1.4 The guard is a recomputation, not a stored name

Open Question 5 asks whether an index-time/query-time tokenizer split can be
caught mechanically instead of failing as a silent zero-hit query. The answer
registered here is **yes, from the bytes already on disk**, and it does not
touch the format. `Open` recomputes one live non-empty document's tokens and
compares them against what was stored: the count against `docoff`'s token count,
and each recomputed term against the segment's terms index. Disagreement is
`ErrTokenizerMismatch`. Storing a tokenizer *name* was rejected — a label can
lie and bytes cannot; [D-023](DECISIONS.md) carries both sides.

### 1.5 The Korean trial's decision rule

Open Question 6 says ">0 hits" is a weak predicate. The rule registered here is
stronger and costs two decoy documents:

| Tokenizer | Assertion |
| --- | --- |
| Default | **0 candidates.** `strings.FieldsFunc` cuts only at non-letters, so `"검색엔진을" ≠ "검색엔진"` |
| Replaced | **at least 1 candidate, the top one is the target, and no decoy is present** |

The replacement tokenizer does not ship. A character bigram over Hangul
syllable runs lives in the test file and in `ExampleWithTokenizer`, which is
what an adopter's own tokenizer looks like plugged in; weft shipping a second
one would make the seam a menu instead of a seam.

## 2. The spend, against the budget

**Seven diff lines against a 5-to-7 budget, and `public_api.txt` did not move.**

| Item | Budgeted | Spent | |
| --- | --- | --- | --- |
| `type Tokenizer func(string) []string` | +1 | +1 | |
| `type Option` and its members | +1 to +2 | **+1** | `type Option func(*Index)` — a func type renders on one line, so the low end |
| `func WithTokenizer(Tokenizer) Option` | +1 | +1 | |
| `method Index.Tokenize(string) []string` | +1 | +1 | |
| `var ErrTokenizerMismatch` | +1 | +1 | |
| `func New(...Option) *Index` | 1 changed | 1 changed | |
| `func Open(string, ...Option) (*Index, error)` | 1 changed | 1 changed | |
| **`engine_api.txt`** | **5 to 7** | **5 added, 2 changed = 7** | Top of the range, nothing unbudgeted |
| **`public_api.txt`** | **0** | **0** | Nothing outside `pkg/engine` gained a name |

Nothing was overspent, so there is nothing here to excuse. The one estimate that
was a range came in at its low end: `Option` is a function type, and
`architecture_test.go`'s renderer puts a function type's whole signature on the
declaration line rather than listing members under it.

The rest of the invariant table registered in [§1.2](#12-the-invariants-fixed-in-advance) held:

```text
git diff --stat pkg/fusion/                                              (empty)
git diff --stat -- pkg/scorer/graph pkg/scorer/recency pkg/scorer/vector  (empty)
```

`Scorer`, `Fuser`, `Candidate`, `Document` and `Query` are byte-identical, which
the golden file above is the mechanical proof of — every field and every method
signature of all five is recorded in it and none of those lines moved.

## 3. The falsification condition, judged

**It did not fire.** `pkg/scorer/text/text.go` is a nine-line diff of which one
line is code:

```diff
-    terms := engine.Tokenize(q.Text)
+    terms := s.ix.Tokenize(q.Text)
```

The other eight lines are the comment saying why. Read against the two readings
registered in [§1.3](#13-where-the-falsification-condition-is-read): **the
scorer asks the index and does not receive a tokenizer.** `s.ix.Tokenize` sits
beside `s.ix.Stats` and `s.ix.LookupInto` in the same function and is the same
kind of call — a question the index answers out of state it owns. Nothing was
added to `Query`, nothing was added to the `Scorer` interface, and the dependency
direction is where it was: `engine` still imports no scorer, which
`TestNeitherEngineNorFusionImportsAScorer` checks from the import graph rather
than from anybody's reading of the code.

What that buys is stated narrowly, because the claim is narrow. **There is one
tokenizer per index and no way to configure the index side and the query side
apart.** `Add`, `Update`, `Update`'s re-tokenization of a replaced document's old
text, and `scorer/text` all call `Index.Tokenize`;
`TestTokenizeSeamIsSharedByIndexAndQuery` records the strings that reached the
seam and asserts all four are there, so this is held mechanically rather than by
inspection.

## 4. The guard lost a check under test, and the ceiling widened

[§1.4](#14-the-guard-is-a-recomputation-not-a-stored-name) registered a
three-step check. **Two shipped. The third was written, run, and taken back
out**, and this is where that is published rather than quietly edited into the
registration.

The third step compared every recomputed term against the segment's terms index.
It failed three existing tests — `TestALyingTermOffsetIsNeverFollowed`,
`TestAnImpossibleFrequencyIsRefused`, `TestALyingBlockMinimumIsRefused` — each of
which replaces a segment's whole `terms` section with a doctored one-entry
payload and then asserts that `Open` **succeeds** and the damage surfaces as
absence at query time.

Those tests are right and the check was wrong, for two reasons that are the same
reason twice:

- **The bytes are ambiguous.** A terms section that does not claim a live
  document's terms is what a replaced tokenizer looks like *and* what a damaged
  or doctored terms section looks like. Reporting `ErrTokenizerMismatch` for the
  second is a wrong diagnosis handed to a caller who would then go looking for a
  tokenizer they never changed.
- **It moved work back into `Open`.** Milestone 3 stopped verifying sections
  `Open` does not need, and [D-006](DECISIONS.md) settled which way damage on a
  lazy path reports: as absence, with `Scrub` as the thing that names it. The
  third step reversed that for one section.

**What the loss costs, stated exactly.** The guard now compares a token *count*
and nothing else, so **a tokenizer that preserves token count passes whatever it
does to the terms** — and a stemmer is precisely that shape, one token in and one
token out. The registration's own ceiling list already published "the guard
catches the large failure and not a stemmer added to an otherwise identical
tokenizer", so the published ceiling is unchanged; what changed is that it is now
exactly true rather than conservative. `docs/FORMAT.md` §8 carries it, and so
does the `ponytail:` comment on `checkTokenizer`.

The two steps that shipped are enough for the failure the milestone is about: a
bigram index opened with the default reads 7 tokens on disk against 2 recomputed.

## 5. The Korean trial, judged

**Both halves pass, and the pass line is the strong one registered in
[§1.5](#15-the-korean-trials-decision-rule).**

| Tokenizer | Registered assertion | Result |
| --- | --- | --- |
| Default | 0 candidates for `"검색엔진"` | **0** |
| Hangul bigram | ≥1 candidate, top is the target, no decoy present | **1 candidate, the target, no decoy** |

The corpus is one target and two decoys. The target holds the query string with a
particle attached — `"검색엔진을 만들었다"` against a query of `"검색엔진"` — and
that is the whole of the problem: `engine.Tokenize` cuts only where a rune is
neither a letter nor a digit, a Korean particle is letters, so the corpus term and
the query term are two different strings and no posting list is consulted.

**What the decoys bought.** `">0 hits"` passes for a tokenizer that matches every
Korean document in the corpus; `"the top hit is the target and no decoy is
present"` does not. The decoys share **no character bigram** with the target, and
that property is itself asserted — `TestKoreanDecoysShareNoBigramWithTheTarget`
fails if a future edit to any of the three strings gives a decoy a bigram the
target holds, which is what keeps the pass line measuring the seam rather than the
corpus. The same test checks the query's bigrams are all reachable from the
target, so the trial cannot pass for the opposite wrong reason either.

**What it does not say.** This is an existence proof that the seam is used, not a
quality claim — and that is a statement about the milestone's scope rather than
about the strength of the predicate. A character bigram index over-matches; no
Korean relevance judgements exist here to measure that with, and building a
labelled Korean dataset is out of scope by the PRD.

**The replacement does not ship.** The bigram tokenizer is eleven lines and lives
in `pkg/engine/tokenizer_test.go` and, inline, in `ExampleWithTokenizer`. Shipping
it in `pkg/` would turn the seam into a menu; [D-022](DECISIONS.md) carries that.

**The second trial is the one that holds outcome clause 1 down.** A directory
committed with the bigram tokenizer and opened with the default is refused with
`ErrTokenizerMismatch`, and reopened with the bigram tokenizer answers the query
— so index time and query time are not merely *documented* to share the seam, a
disagreement between them is a reported error.
`TestTokenizeSeamIsSharedByIndexAndQuery` is the direct form of the same claim:
it records the strings handed to the tokenizer and asserts that `Add`, `Update`,
`Update`'s re-tokenization of the replaced text, and `Index.Tokenize` all appear.

## 6. Quality, unmoved

`make eval`, the registered procedure, on the 171,332-document evaluation corpus:

```text
text                0.5826
text+vector         0.6211
```

**Identical to four decimals to the published figures**, against a −0.005
tolerance. The paired bootstrap deltas and the graph arm's numbers are unchanged
too, which is what a scoring path nobody touched should produce.

That is the empirical half of [D3](DECISIONS.md#d-022--the-tokenizer-is-a-seam-on-the-constructor-and-the-scorer-asks-rather-than-receives)'s
argument, and the argument is worth stating because it predicted the result: the
default path gained **exactly one branch**, the `ix.tok == nil` test in
`Index.Tokenize`, and is otherwise the same code doing the same work. A figure
that had moved would have meant the branch was not the only change, which is why
[§1.2](#12-the-invariants-fixed-in-advance) registered "find what moved before
publishing" rather than a tolerance to hide inside.

## 7. Performance: the run was bought, and it came back void

[D6 of the plan](../.claude/plans/weft-m13.plan.md) bought a performance run on an explicit
argument: milestone 12 spent zero implementation lines and used that as its reason not to
measure, and this round cannot — the diff is not zero and it crosses both the index path and
the query path. **So the run was made, and this section publishes what it produced, which is
not a verdict.**

The registered clauses, and where each landed:

| clause | denominator | result |
| --- | --- | --- |
| nDCG@10 unchanged | 0.5826 / 0.6211 | **met**, identical to four decimals ([§6](#6-quality-unmoved)) |
| shed 0 at 27.28 q/s | 0 | **void** — 4,857 of 10,000 |
| p50 ≤ 40 ms | 33.470 ms | **void** — 2.675 s |
| ladder peak RSS ≤ 120 MiB | 100.7 MiB | **void** — 654.1 MiB |
| worst read in a commit window ≤ 1 s | 61 ms | **not attempted** |

**Void, not missed.** The A/B that decides it is in [PERF §5.6](PERF.md): the same top rung
on the commit *before* this milestone gives p50 2.321 s, 664.2 MiB and a served fraction of
57.6% against this build's 52.3%. The pre-M13 baseline misses the same three clauses by the
same one-to-two orders of magnitude, so the ladder is measuring the machine rather than the
change.

**What made it void is on the record, because the run was mine.** The ladder followed, in one
session and on one machine, a `make eval` over the 626 MiB corpus, a `go test -race ./...`,
two `golangci-lint` passes and an `npx markdownlint`. [PERF §2](PERF.md) already says why CI
must never gate on these figures — a tail latency on a busy machine is a function of whatever
else is on it — and that applies to a developer machine in the middle of a working session
just as squarely.

**Three things point the other way, and none of them substitutes for the run.**

1. **Allocation per query is flat and matches the published figure on both arms.** 10,928.9
   KiB on the baseline and 10,943.5 on this build, against 10,869.0 published. [Milestone 8
   §11](#11-the-memory-clause-judged--and-the-excursion-went-with-it) established that this
   number is a property of the query set rather than of the load, which is what makes it
   readable at all here.
2. **The unloaded sequential path is where it was.** 33.186 ms on the baseline, 34.072 ms on
   this build, against 32.231 ms published — inside the 8.8% machine-state band milestone 8
   §11 documented for itself.
3. **The default path gained exactly one branch.** `ix.tok == nil` in `Index.Tokenize`, and
   §6's unmoved nDCG is the empirical half of that argument: a scoring path that had changed
   would not reproduce four decimal places.

**What is deliberately not claimed.** Not "milestone 13 did not regress performance" — the
residual gap between the two arms is a single observation apiece on an instrument off by 70×,
and the baseline arm was truncated at 89% of its schedule, so it cannot be read in either
direction. And not "the invariant held" — nothing here measured that. The honest statement is
the narrow one: **the clauses are unjudged, and this round did not earn the right to say
otherwise.**

**Run B was not attempted, and that was a choice rather than an omission.** Forty-five more
minutes on an instrument whose companion arm had just come back void produces a second
unusable number. [PERF §5.6](PERF.md) registers what a valid attempt needs so the next one is
not a third void run.

## 8. Carried forward

1. **Milestone 13's own performance clauses are unjudged**, for the first time in this
   repository on a round that *bought* the run. Shed 0 at 27.28 q/s, p50 ≤ 40 ms, ladder peak
   RSS ≤ 120 MiB, and milestone 9's read clause. [§7](#7-performance-the-run-was-bought-and-it-came-back-void)
   is the attempt and [PERF §5.6](PERF.md) is what a valid one needs — a quiet machine, no
   preceding index load in the same session, and the pre-milestone commit measured on the same
   ladder in the same sitting.
2. **A quiet-machine requirement is now a load-bearing part of the procedure and nothing
   enforces it.** [PERF §2](PERF.md) said it about CI; this round is the first time it
   invalidated a run of the author's own, and there is no check — no preflight, no recorded
   machine state beyond `date` — that would have caught it before 97 minutes were spent. The
   A/B is what caught it, after the fact.
3. **Milestone 11's performance clauses are still unjudged**, unchanged by this round. Run A,
   run B and the `-deletefrac` instrument are all still owed;
   [milestone 11 §5](#milestone-11--deletion-and-update) holds the detail.
4. **The guard cannot see a count-preserving tokenizer.** A stemmer maps one token to one
   token, so it changes every term and no count and passes.
   [§4](#4-the-guard-lost-a-check-under-test-and-the-ceiling-widened) is why the check that
   would have caught it cannot be asked at `Open`, and closing it needs the tokenizer's
   identity — which [D-023](DECISIONS.md) shows cannot be stored honestly. This is a ceiling,
   not a defect.
5. **The seam's four call sites are held by one test and nothing structural.**
   `TestTokenizeSeamIsSharedByIndexAndQuery` records the strings that reached
   `Index.Tokenize`, so a fifth call site added later that called `engine.Tokenize` directly
   would not fail it — the test asserts what did arrive, not that nothing bypassed the seam.
   This is the same shape as milestone 11's carried-forward item about the read-path filter
   being maintained by hand.
6. **A position index and per-field term spaces are still format v5**, and a replaceable
   tokenizer does not change that: one tokenizer serves the whole `Text` and there is nothing
   to scope a term to. `FORMAT.md` §8 carries both with what they would cost.

---

<!-- markdownlint-disable-next-line MD025 -->
# Milestone 14 — The probe passed, the ladder ran twice, and a closed lid discarded both

**Verdict: the four clauses are still unjudged, for the third round.** The round's own outcome
clause is not met. Two ladder attempts were made in one session: the first was killed by
SIGTERM at 46 minutes, the second completed all eight rungs and **both of its arms printed
`DISCARD this run`** — the machine slept mid-ladder, twice, because the laptop's lid was closed.

What the round did deliver is the thing [milestone 13 §8 item 2](#8-carried-forward) asked for,
and the second attempt is the first time anything here has tested it end to end. The result is
more useful than a pass would have been: **the probe this round added passed before both
attempts, and the run was void anyway** — which is [D-024](DECISIONS.md)'s own registered
falsification condition firing on its first serious use.

`pkg/` diff: **0 lines**. Golden API files: **0 lines**. That was the mechanical definition of
this being a judgment round rather than a feature one, and it held.

## 1. What was registered, and that it was registered first

[PERF §5.7](PERF.md) fixes the preflight and its pass line, the baseline commit, the arm
order, four readings and the cut order. It was committed **before** anything ran, which is
checkable:

```console
$ git log --format='%h %cd %s' --date=format:'%H:%M:%S' -- docs/PERF.md | head -2
aee06ca 23:28:12 docs: correct 5.7's baseline probe, before the baseline arm ran
d32bade 23:26:34 docs: register milestone 14's readings and preflight before the run
```

`d32bade` at 23:26:34 precedes the first probe at **23:26:41** by seven seconds, and `aee06ca`
at 23:28:12 precedes the second at **23:28:20** by eight. The ordering is the point: a pass
line chosen after seeing the numbers is how a performance claim is made to say whatever its
author wants, which is [§3](PERF.md)'s standing rule and the reason §5.3 through §5.7 all carry
*registered before it is measured* in their titles.

**One correction was made mid-round and published rather than edited in.** §5.7's first draft
spelled the baseline probe `make -C ../weft-m12-baseline bench-preflight`, and **that target
does not exist on `700a178`** — this milestone adds it, so a worktree at milestone 12's merge
has no rule for it. The command was respelled as the flags the target hardcodes; nothing
measured changed. It is recorded because a procedure step whose command fails on one of two
arms is the same class of defect as no step at all, and the step existing is what this round
was buying.

## 2. The baseline moved to a commit that is reachable

§5.6 named `1eb2a44` as its baseline arm, and **that commit is not an ancestor of `main`** — it
sits on the `m12-query-expressiveness-onmain` side branch as milestone 13's development base,
so no reader of this history can check it out. `700a178`, milestone 12's merge on `main`, is
byte-identical across all four code directories:

```console
$ git merge-base --is-ancestor 1eb2a44 HEAD
(exit 1 — not an ancestor)
$ git diff --stat 1eb2a44 700a178 -- pkg/ internal/ cmd/ bench/
(empty)
$ git diff --stat 9d05c16 eedc04a -- pkg/ internal/ cmd/
(empty)
```

So the substitution moves the measurement to a reproducible place without changing what is
measured, and the measured arm `eedc04a` is the same code §5.6 measured as `9d05c16`. §5.6's
wording stands as written.

## 3. Both probes passed, and reading 1 did not fire

2026-08-27, 28 seconds each, 500 requests each, baseline arm second. Full output and `uptime`
either side are in [docs/testing/weft-m14.tdd.md](testing/weft-m14.tdd.md).

| | baseline `700a178` | HEAD `eedc04a` | HEAD, 00:25 | HEAD, 05:36 | pass line |
| --- | --- | --- | --- | --- | --- |
| unloaded p50 | 34.272 ms | 33.705 ms | 35.098 ms | 32.800 ms | — |
| p50 at 27.28 q/s | 35.544 ms | 35.387 ms | 35.161 ms | 34.421 ms | ≤ 2× the same run's unloaded p50 |
| **ratio** | **1.04×** | **1.05×** | **1.00×** | **1.05×** | ≤ 2× → **all pass** |
| shed | **0** | **0** | **0** | **0** | 0 → **all pass** |
| alloc per query | 10869.0 KiB | 10869.0 KiB | 10869.0 KiB | 10869.0 KiB | — |
| single-rung peak RSS | 102.3 MiB | 105.7 MiB | 97.5 MiB | 99.8 MiB | not a clause reading |
| load average at start | 3.29 | 2.31 | — | **6.85** | not a gate |

Four probes, four passes, against the void run's **78×**. The instrument was in a state to
measure every time, which is the one thing neither of the previous two rounds could say. The
last one is the gate certificate for the ladder attempt in [§4](#4-two-attempts-and-what-ended-each);
it was taken at 05:36:27, seventy-eight seconds before that ladder started, and it passed at
1.05× on a machine whose one-minute load average was 6.85 — higher than any other probe.
Recorded because §5.7 keeps load average out of the verdict but logs it.

**These are gate readings and they are not clause readings.** §5.7 registered them as *not
published*, and the sense in which they appear here is narrow: what is published is that the
probe passed and by how much, because that is the evidence the gate worked. They cannot be
read against the clauses, for three separate reasons and any one of them is enough:

1. **500 samples, not 10,000.** [PERF §2.3](PERF.md) leaves a quantile out rather than
   printing it when fewer than 100 samples sit beyond it, and the probe's own output prints
   `p95 -- p99 -- p99.9 --` for exactly that reason. A p50 over 500 samples is not the p50 the
   clause names.
2. **A lone rung is not the ladder's fourth rung.** §5.5's correction and
   [D-014](DECISIONS.md) forbid reading a lone rung's `peakrss` against milestone 8's ladder
   peak, and the probe's 105.7 MiB and 102.3 MiB are precisely that forbidden comparison. The
   memory clause reads the ladder's high-water mark and there is no ladder here.
3. **The rule needs a ladder to apply.** The probe's own summary says so:
   *1 of 1 rungs measured … so the load-point rule has nothing to apply and there is no
   saturation point and no headline.*

So `shed = 0` and `p50 = 35.4 ms` are **not** the shed and p50 clauses being met, and this
section declines to say they are.

## 4. Two attempts, and what ended each

### Attempt 1 — killed at 46 minutes

Started 00:52:36 after a probe that passed at 1.00×. Reached 9,386 of rung 1's 10,000 samples
and took **SIGTERM** at 01:38:38 (`make: *** [bench] Terminated: 15`), 46 minutes in, from
outside the run. It was launched as a session-managed background task, and the operator did not
stop it.

**Nothing is published from it**, because one partial rung of one arm is not a cut — it is the
void production §5.7 forbids. It is recorded because §5.1 requires it: *a run thrown away
silently is indistinguishable from one that was never made.* Its partial rung is quoted once,
in [§5 item 4](#5-what-this-licenses-and-what-it-does-not), for the single narrow thing it
establishes.

### Attempt 2 — eight rungs, both arms discarded

Relaunched detached — `perl` fork plus `POSIX::setsid`, because macOS ships no `setsid` and the
previous process group was the thing that got killed. It survived, ran 05:37:05 to roughly
17:00, and **completed all four rungs on both arms.** Both then printed the instrument's own
refusal:

```text
DISCARD this run: the process did not run for 22m39s of the rung at 6.82/s, so the ladder
was measured across a suspension. There is no headline.
DISCARD this run: the process did not run for 4h57m8s of the rung at 3.41/s, so the ladder
was measured across a suspension. There is no headline.
```

**The cause is a closed lid**, and `pmset -g log` names it exactly:

```text
06:46:41  Sleep  Entering DarkWake state due to 'Clamshell Sleep'  Using AC (Charge:100%)
06:46:46  Sleep  Entering Sleep state due to 'Clamshell Sleep'     Using Batt (Charge:100%)
07:10:00  Wake   Wake from Deep Idle ... due to ... lid ... HID Activity
```

`Elapsed` in `internal/loadgen/clock.go` subtracts the monotonic clock from the wall clock, and
on Darwin the monotonic clock does not advance while the system sleeps, so the gap is proof the
process was not running. The guard at `cmd/weft-eval/bench.go` inspects **every** rung and
discards the whole ladder when any one of them was suspended, on the stated ground that *a
machine that slept during rung one was not the same machine for rung five*. That reasoning binds
here, so no rung from either arm is quotable, including the clean ones.

### What ended them is not what this round built a check for

**The probe passed before both attempts and neither produced a verdict.** That is
[D-024](DECISIONS.md)'s registered falsification condition — *a ladder that comes back void
after a probe that passed* — and it fired. But the honest reading is narrower than *the probe
is wrong*, and the distinction matters for what the next round should do:

| failure mode | what catches it | did it work? |
| --- | --- | --- |
| a **contended** machine | `make bench-preflight`, added this round | untested — no attempt failed this way |
| a **suspended** machine | `SuspendTolerance`, already present since milestone 7 | **yes, in-band, on both arms** |
| a machine that **stays awake** | `caffeinate -dimsu`, prescribed since milestone 5 | **no** |

So the two detectors are complementary and both behaved correctly. **The thing that failed is
the mitigation.** `caffeinate -dimsu` holds `PreventUserIdleSystemSleep`, which stops *idle*
sleep; it has no power over clamshell sleep, and the machine was on AC at 100% charge when the
lid closed, so no power-state condition was in play either. This file has prescribed
`caffeinate -dimsu` as the way to keep a ladder awake since §5.1, and `clock.go`'s own comment
cites *thirteen hours of clamshell sleep* as the failure the suspension check exists to detect.
**The documented remedy does not cover the documented failure mode, and that has been true for
nine milestones.** Nothing caught it before now because until this round no ladder had been
started and then left alone for three hours.

The fix needs no code and is registered in [PERF §5.7](PERF.md): **the lid stays open**, and
`caffeinate` is kept for what it does cover. `sudo pmset disablesleep 1` would enforce it and is
rejected for the usual reason — it needs root and leaves a machine that never sleeps if the
operator forgets to unset it, which is a worse failure than the one it prevents.

## 5. What this licenses, and what it does not

1. **The four clauses are unjudged, for the third round.** Shed 0 at 27.28 q/s, p50 ≤ 40 ms,
   ladder peak RSS ≤ 120 MiB, and milestone 9's worst read inside a commit window ≤ 1 s. Not
   met, not missed. Carried forward again in [§7](#7-carried-forward).
2. **No regression is attributable to milestone 13, and this round narrowed that further than
   the last one could.** The four probes put the two arms 0.44% apart at the top rate, HEAD
   marginally ahead. That is not evidence for invariance — 500 samples, one observation, one
   rung — but it points the opposite way from §5.6's reported p50 +12.8%. What the discarded
   ladder adds is stronger and is in [§5a](#5a-a-pattern-in-the-discarded-data-registered-as-a-hypothesis-and-not-a-finding):
   the top-rung collapse §5.6 reported for milestone 13 **reproduces on pre-milestone-13
   code**. Whatever it is, it is not the `ix.tok == nil` branch.
3. **The quality clause stays judged and was not re-run.** Milestone 13's run C reproduced
   nDCG@10 0.5826 / 0.6211 to four decimals ([§6 of milestone 13](#6-quality-unmoved)).
   §5.7 removed `make eval` from the run list deliberately: a second identical verdict is not
   worth putting a 677 MiB index load into the ladder's session, which is the condition that
   voided the last attempt. **A subtraction, published as one.**
4. **Allocation per query reproduced everywhere, including across the discard.** 10869.0 KiB on
   all four probes, and 10868.9 to 10869.0 KiB on every non-saturated rung of both discarded
   arms, against a published 10,869.0.
   [Milestone 8 §11](#11-the-memory-clause-judged--and-the-excursion-went-with-it) established
   that this figure is a property of the query set rather than of the load, which is what makes
   it readable where the latency quantiles are not. Twelve independent readings agreeing to one
   decimal is the strongest evidence available that both arms run the same work — and it is not
   one of the four clauses, so it judges nothing.
5. **Attempt 1's partial rung establishes exactly one thing.** 9,386 samples at 3.41 q/s gave
   p50 56.617 ms against a published 78.576, shed 0, and the rung raised the memory mark by
   0.1 MiB. **The machine was capable of the ladder's lower rungs.** It is quoted only for
   that, because the run was terminated rather than completed and a truncated rung is not a
   rung.
6. **`ru_nivcsw` has many observations now and still no threshold**, which is the more useful
   negative result. 22,698 to 27,434 per 500-request probe; 649,334 to 970,547 per 10,000-sample
   rung on the clean rungs of both arms; and **2,395,569 and 2,723,482** on the two collapsed
   top rungs — a 3.7× step over the rung below on the same arm. [D-024](DECISIONS.md) rejected
   the metric for having no baseline, and it still has none, because every one of these readings
   comes from a machine that either was not quiet or slept mid-run. What they establish is that
   the figure **moves with the collapse**, which is what would make it a signal if a clean
   ladder ever bracketed it.

## 5a. A pattern in the discarded data, registered as a hypothesis and not a finding

The two arms are discarded and no number below is a clause reading. They are set down because
the same shape appears on both arms and matches §5.6's void run, and because the next attempt
should be looking for it rather than discovering it again.

| rung | rate | M8 published p50 | baseline `700a178` | HEAD `eedc04a` |
| --- | --- | --- | --- | --- |
| 12.5% | 3.41/s | 78.576 ms | 55.426 ms | 66.643 ms |
| 25% | 6.82/s | 50.825 ms | 47.661 ms | 44.952 ms |
| 50% | 13.64/s | 34.124 ms | 37.076 ms | 37.744 ms |
| **100%** | **27.28/s** | **33.470 ms** | **1.635754 s** | **2.034673 s** |
| shed at the top rung | | 0 | 1,811 | 3,323 |
| ladder peak RSS | | 100.7 MiB | 660.8 MiB | 675.8 MiB |
| raised *at* the top rung | | +1.8 MiB | +553.0 MiB | +574.3 MiB |

**Rungs 1 through 3 are in family with the published ladder on both arms. The top rung
collapses on both.** And §5.6's milestone 13 void run reported 2.616899 s, shed 4,857 and
654.1 MiB at that same rung — the same signature, a third time.

Set against that: **four single-rung probes at 27.28 q/s returned 34–35 ms, shed 0 and roughly
100 MiB**, on this machine, this week, including one taken 78 seconds before the ladder started.
So 27.28 q/s alone is fine and 27.28 q/s as a ladder's fourth rung is not.

That is precisely the distinction [D-014](DECISIONS.md) and §5.5's correction exist to enforce,
and it now has a second use: the lone rung and the ladder's rung are **different measurements of
different things**, and the probe cannot stand in for the clause because of it.

Four candidate explanations, and this round eliminates one:

1. **Milestone 13's code** — **eliminated.** The baseline arm is pre-milestone-13 and collapses
   the same way. Whatever this is, the `ix.tok == nil` branch is not it, and §5.6's residual
   "p50 +12.8%" between arms was noise on top of a shared failure.
2. **A ladder-prefix effect** — 85 minutes and 30,000 queries of prior load degrade something a
   lone rung never reaches. The +553 MiB step *at* the top rung looks like queue depth becoming
   resident, which is what saturation looks like from the inside.
3. **Post-suspension state** — both arms slept *before* reaching the top rung and woke on
   battery. A machine that has been asleep for five hours and comes back on battery is not the
   machine the first three rungs ran on. This one is a direct consequence of the discard and is
   the reason the discard binds.
4. **Toolchain** — the published figures are Go **1.26.1** ([§Machine](PERF.md)); every run since
   is on **1.26.7**. Both arms share it, so it cannot explain a difference *between* arms, but it
   is live for the difference against the published ladder.

**2, 3 and 4 are not separated by anything in this data**, and 3 alone is enough to refuse the
whole reading. A clean ladder — lid open — distinguishes 2 and 4 from 3 immediately, and that is
the first thing the next attempt buys.

## 6. What is not matched, and which way it biases

In [PERF §4](PERF.md)'s form. The first four were registered before the run; items 5 and 6 are
what the run added:

1. **Page cache between the arms.** The first arm to run maps the index and leaves the cache
   warm; the second inherits it. The order is fixed **baseline first**, so a HEAD regression
   is real *despite* the cache being on HEAD's side, and a HEAD improvement has a live
   competing explanation which will be written beside the figure rather than claimed as an
   improvement. Running both orders would cost 6.4 hours instead of 3.2 and randomisation buys
   nothing at n=1, so the bias is **named rather than removed**.
2. **The probes ran in the reverse order from the ladder's.** HEAD's probe was first, at
   23:26:41; the baseline's at 23:28:20 inherited whatever HEAD's had warmed. That is the
   direction *against* the observation in §5 item 2 — the baseline had the cache advantage and
   was still 0.44% slower — and it is one more reason 0.44% is not a claim.
3. **The index is milestone 11's format v4, and only HEAD has the tokenizer guard.** HEAD's
   `Open` re-tokenizes one live document; the baseline's does not. Both opened in **60 ms**,
   identically, so the guard's cost is below this instrument's resolution — which is the first
   measurement of it, against [milestone 13's gap 5](#8-carried-forward) that no test pins it.
4. **One observation each.** [D-013](DECISIONS.md) unchanged: a single run is not a median.
   Three repetitions would cost the 4.9 hours D-013 priced and this round did not buy them.
5. **The Go toolchain is not the published one.** Every figure in this file's milestone 8 ladder
   was measured on Go **1.26.1**; both arms here ran on **1.26.7**. Shared between the arms, so
   the A/B is internally valid; unmatched against the published denominators, which is where
   three of the four clauses come from. Direction unknown, and [§5a item 4](#5a-a-pattern-in-the-discarded-data-registered-as-a-hypothesis-and-not-a-finding)
   keeps it live. The [Machine table](PERF.md) is the place this has to be reconciled before any
   verdict is published against those denominators.
6. **The two suspensions were unequal, and not in the arms' favour.** The baseline arm lost
   22m39s at rung 2; HEAD lost **4h57m08s** at rung 1. HEAD's unloaded median was also the
   outlier of the session at 39.134 ms against 32.8–35.1 ms everywhere else. So the arm with the
   worse figures is also the arm that slept thirteen times longer, and the two cannot be
   separated. **This is why the discard binds rather than being a formality** — reading these
   arms against each other would attribute five hours of sleep to milestone 13.

## 7. Carried forward

1. **The four performance clauses are unjudged for the third round.** [PERF §5.7](PERF.md)
   holds the readings, the pass lines, the baseline, the arm order and the cut order, all
   committed and all still standing. What the next attempt needs is **not** more design. It is
   three things, and the third is new: a 3.1-hour window with nothing else on the machine, a
   passing `make bench-preflight` on each arm taken immediately before that arm, and **the lid
   left open**.
2. **`caffeinate -dimsu` is not sufficient and this file has said otherwise since milestone 5.**
   It cannot prevent clamshell sleep, which is what discarded both arms.
   [§4](#4-two-attempts-and-what-ended-each) has the `pmset` evidence and
   [PERF §5.7](PERF.md) now states the requirement beside the command. This is the round's most
   transferable finding: every ladder in this file's history was run under a mitigation that
   does not cover the failure mode its own suspension check was built to detect.
3. **The top-rung collapse is the next thing to explain, and it is not milestone 13's.**
   [§5a](#5a-a-pattern-in-the-discarded-data-registered-as-a-hypothesis-and-not-a-finding) has the
   shape, the three surviving candidate causes and the one it eliminated. A single clean ladder
   settles whether it is real, and if it is, the three clauses that live on the ladder are
   missed rather than unjudged — which would be the first actual verdict since milestone 8.
4. **§5.7's reading list needs two more entries, not one.** *The window is known in advance to
   be unavailable* → not executed; and *the run completed and the instrument discarded it* →
   void, distinct from reading 4's drift because the cause is recorded in-band rather than
   inferred from an A/B. This round hit the second one twice.
   [D-024](DECISIONS.md) records both amendments.
5. **Go 1.26.1 is not installed on this machine.** `~/.goenv/versions/` holds 1.26.7 alone, so
   [§5a item 4](#5a-a-pattern-in-the-discarded-data-registered-as-a-hypothesis-and-not-a-finding)'s
   toolchain candidate cannot be tested without fetching it first. Cheap, and worth doing only
   if a clean ladder still misses — otherwise it is a variable nobody needs to move.
6. **The write arm is cut, and the cut is a decision.** Milestone 9's read clause needed
   `-writes -writedocs 20000` on both arms, about 90 minutes, and §5.7 registered it as the
   first thing to cut. It was never reached because the ladder it follows was discarded, so this
   is a cut behind a discard — published so that "the write arm was cut" reads as an ordering
   decision rather than as an omission.
7. **`-deletefrac` and §5.5's run B are still open**, untouched by this round and deliberately
   so: mixing two rounds into one sitting means a void in either cannot be attributed to
   either. Milestone 11's carried-forward item 3 and half of the PRD's open question 2 both
   remain owed, and `-deletefrac` still does not exist in `cmd/weft-eval`.
8. **`ru_nivcsw` per rung stays rejected, and its revival signal has now fired.**
   [D-024](DECISIONS.md) named *a ladder that comes back void after a passing probe* as the
   condition that would revive it, and that happened twice. It stays rejected anyway, for the
   reason [§5 item 6](#5-what-this-licenses-and-what-it-does-not) gives: the readings move with
   the collapse, but every one of them comes from a machine that slept, so there is still
   nothing to calibrate against. **Reviving it is the second thing a clean ladder buys**, not
   something to add before one exists.
9. **The probe has no exit code, and this round's failures argue against adding one.** The
   comparison is a person reading two printed lines, and the `ponytail:` comment on
   `bench-preflight` prices the alternative at about thirty lines in `cmd/weft-eval/bench.go`.
   Neither failure here would have been caught by it: the probe passed both times, and what
   ended the runs was a SIGTERM and a closed lid.

---

<!-- markdownlint-disable-next-line MD025 -->
# Milestone 15 — The graph is indexed, and the walk is built but not judged

Milestone 4 measured graph proximity at **+0.0000 nDCG@10** at its best fusion weight and
recorded the verdict at the top of `scorer/graph`'s package documentation. What it also did,
and what this round is built on, is diagnose *why*. The finding was not that citation
structure carries no signal. It was that one construction of it has almost no ranking to
contribute:

> with MaxDepth 3 a candidate's score takes very few distinct values, the hop-1 frontier on a
> real citation graph runs to tens of documents per query, and `engine.TopK` breaks the
> resulting ties on `DocID` — which is corpus insertion order.

[Milestone 4 §5](#milestone-4--quality) named personalised PageRank as the principled version
of what the BFS approximates. This round writes it, and writes the structure without which it
is not affordable.

**What is claimed and what is not.** The capability is built, tested and measured for cost.
**No quality number is produced.** The corpus that would answer it — TREC-COVID joined to the
Semantic Scholar citation graph — is not on this machine, and `make eval` skips without it.
The arms are registered in `cmd/weft-eval/run.go` so that the measurement is one command
away, and §5 says what that leaves owed.

## 1. Why a traversal had to stop reading documents

`scorer/graph`'s BFS reaches a node's edges through `engine.Index.Doc`, which materialises the
key, the text and the vector in order to read `Links` — a field that is none of them. On the
evaluation corpus 69% of the `docs` section is vector bytes, so a traversal faulted in a
768-wide vector per node visited and dropped it. Each edge then cost an `Index.Resolve`, which
takes the index-wide read lock and binary-searches the keys table.

That is a per-query cost, paid again on every query, and it is why an algorithm with a real
work bound was not affordable: a push loop reads a node's edges tens of thousands of times.

Two things were built, in that order.

**`Index.Neighbors(id) ([]DocID, bool)`** — links resolved under one read lock, reached by a
decode that steps over the vector by arithmetic instead of reading it. `decodeDocFields` grew
a third mode rather than a third decoder, because two decoders for one format is how a format
drifts. `TestNeighborsStepsOverAVectorToReachTheLinks` is what holds the skip honest: mutating
the skip to `4*vn-1` fails it, and the four other `Neighbors` tests with it.

**`graph.Adjacency`** — the whole link structure resolved once into `DocID` space, forward and
reverse, CSR. After it, a hop is a slice bound.

The reverse direction is a capability and not a mirror. `Document.Links` says what a paper
cites; *what cites this paper* is the transpose of every edge in the corpus, and no per-query
traversal can reach it without scanning every document. `Adjacency.In` is that, and
`TestPPRTravelsBothDirections` shows the BFS scorer returning nothing on a query the walk
answers.

## 2. What it cost and what it bought, measured

`BenchmarkGraphArm`, 20,000 committed documents, 128-wide vectors, 120,000 edges, five seeds,
`k=10`. darwin/arm64, Apple M4. Committed and reopened, because a pending index hands `Doc`
the struct the caller passed and the decode this removes exists only on the other side of a
commit.

| Arm | ns/op | B/op | allocs/op |
| --- | --- | --- | --- |
| `bfs-over-index` | 1,378,929 | 1,031,404 | 48,522 |
| `ppr-over-adjacency` | 235,896 | 184,088 | **230** |

**5.8× the speed, and 211× fewer allocations.** The second number is the one that matters more
than the first. [Milestone 5 §3.2](#milestone-5--performance) measured this project's
throughput wall as live heap under concurrency — "every candidate decodes a whole record" —
and 48,522 allocations a query is what that sentence looks like on the graph arm.

The two are not the same algorithm, so this is not a speedup of one into the other. It is what
a caller pays for a graph stream, before and after.

**The build is not free and is not fast.** `BenchmarkAdjacencyBuild` is **93.6 ms and 4.4
million allocations** over the same corpus — about 37 allocations an edge, which is one binary
search through the keys table decoding a string at every probe. Reading the links themselves
is 7 allocations a document. It repays against the per-query saving in **81 queries**, which is
the whole of the argument for leaving it; the `ponytail:` comment on `NewAdjacency` names the
way out and the condition for buying it.

## 3. The tie group is gone, and the direction of the new ranking is the opposite of the guess

`TestPPRBreaksTheTiesHopDistanceCannot` builds the shape milestone 4 diagnosed — one seed,
eight one-hop neighbours differing only in what lies beyond them — and asserts both halves:

- the BFS arm produces **at most 2 distinct scores** over the eight, which is the tie group;
- the walk produces **8**.

So the mechanism milestone 4 blamed is removed. Whether removing it recovers any nDCG is
§5's open question and nothing here answers it.

**What the test caught is worth recording, because the prediction written into it was wrong.**
The assertion first written said the *least*-connected neighbour should keep the most mass, on
the reasoning that a node with fewer edges spreads less of what it is handed. It fails: h0
scored 0.0271 and h7 scored 0.0734. Mass a node spreads down its edges returns along the same
edges, so the better-connected neighbour keeps more. That is PageRank's degree bias, present
here by construction.

It is also the property most likely to be wrong for this task. On a citation corpus the
best-connected paper is the one everything cites and nothing is specifically about, so a
stream ranking it first is ranking by fame. **This is a plausible reading of what milestone 4
measured and did not explain**, and it survives the change of algorithm — which is the reason
to state it here rather than treat the walk as a fix.

The lever is degree normalization. It is deliberately **not** a mode on the scorer — a seam
with a menu in it is not a seam ([D-022](DECISIONS.md)) — and it is reachable from outside,
because `Adjacency.Degree` is exported and `engine.Search` takes scorers by interface. The
recipe is the wrapper shape [ADOPTION §8](ADOPTION.md) already recommends.

## 4. Two smaller things the round settled

**Seed order cannot change a ranking.** Float addition is not associative, so the order
residual arrives at a node decides its last bit, and that order is the order the frontier was
seeded in — the caller's. Two mathematically equal scores differing in their last bit make
`TopK`'s `DocID` tiebreak unreachable, so permuting `Query.Seeds` would silently permute the
result. One sort of at most `SeedN` ids makes the whole push sequence a function of the
adjacency alone. `TestPPRIsIndependentOfSeedOrder` asserts **exact** equality across three
permutations; a tolerance there would pass the very difference the sort exists to remove.
`Scorer.Candidates` buys the same property a harder way, by tallying per hop count.

**A bad constant is refused at construction rather than clamped.** A restart probability of 0
does not make the walk fail — it removes the term that bounds its work, so the loop runs until
float underflow instead of terminating on the argument that licenses it. `NewPPR` returns an
error, which is the one place it can be said to the party able to fix it.

## 5. Carried forward

1. **No quality number exists, and that is the whole debt of this round.** `text+graph-ppr`
   and `text+vector+graph-ppr` are registered arms with two registered comparisons — against
   the same baseline as milestone 4's binding pair, and against the BFS arm the walk was
   written to replace. Neither has been run. Until one is, the honest statement is that the
   mechanism milestone 4 blamed is removed and nothing is known about whether that recovers
   any nDCG.
2. **Degree normalization is untested.** §3 argues it is the first thing to try if the walk
   does not beat the baseline. It needs no new API.
3. **`WithRestart` and `WithPrecision` were chosen by arithmetic, not by measurement.**
   `DefaultPrecision = 1e-4` comes from the work bound against the 579,719-edge evaluation
   graph — 66,667 edges a query — and `DefaultRestart = 0.15` is the PageRank convention. Both
   are exported and overridable precisely so a sweep can set them; no sweep has run.
4. **The adjacency is a snapshot and nothing enforces rebuilding it.** A caller who ingests
   and forgets gets a stale graph, and a stale graph answers plausibly. `TestAdjacencyIsASnapshot`
   pins the behaviour; making it self-invalidating would mean the structure holding a lock on
   the index, which is what building it once exists to avoid.
5. **The build's 4.4 million allocations are a resolution cost, not a link-reading cost.** The
   fix is a key-to-id cache across the walk, which needs the raw keys and therefore a second
   engine accessor beside `Neighbors`. Owed when a corpus makes 93.6 ms show up as ingest
   latency rather than as a startup cost.
6. **The demo does not show it.** `cmd/weft` still fuses the four milestone-1 scorers, and the
   README's sample output is the one that documents. Adding a fifth column is a documentation
   change, not a code one, and it is not worth making before §5.1 says what the column means.

---

<!-- markdownlint-disable-next-line MD025 -->
# Milestone 17 — The floor under every query, found and removed

[Milestone 5 §3.2](#milestone-5--performance) measured this project's throughput wall and named
its cause in one sentence: **live heap under concurrency**. At 27 queries/s — weft's own
sequential rate — p50 went from 39 ms to 1.27 s, 14% of queries were shed, and RSS went from 126
to 853 MiB. The explanation attached to it was "every candidate decodes a whole record", and
[§9 of milestone 8](#milestone-8--what-a-repetition-has-to-hold) had already corrected that
attribution once without replacing it.

This round measured `scorer/text` directly and found a second, larger source that is not a
decode at all.

## 1. What a query allocated before it scored anything

`BenchmarkCandidates`, 50,000 documents committed and reopened, darwin/arm64, Apple M4. The
corpus is deliberately skewed: `everywhere` in every document, `occasional` in one in fifty,
`singular` in three, and a query term in none.

| Query | ns/op | B/op |
| --- | --- | --- |
| a term **no document holds** | 78,685 | **1,182,240** |
| a term in 3 documents | 101,285 | 1,185,247 |
| a term in 1 document in 50 | 176,454 | 1,216,432 |
| a term in every document | 5,838,555 | 2,818,997 |

**1.18 MB and 78 µs to answer a query that matches nothing.** The line responsible was one
allocation:

```go
acc := make(map[engine.DocID]float64, docs)
```

The accumulator was sized to the corpus. The comment above it argued the case honestly and the
argument was sound as far as it went — one common term produces one entry per matching document,
and an unhinted map re-buckets its way up through every doubling on exactly the queries that
cost most. What it did not price is that the insurance is bought **on every query**, including
the ones that will never write an entry into it.

That is the floor. It does not scale with what a query reaches; it scales with the corpus. On
the 171,332-document evaluation index the same benchmark shape measures **4.73 MB a query**, and
at the 27 queries/s where milestone 5 saw the collapse that is **128 MB/s of garbage produced
before a single document is scored**.

## 2. The first fix moved the cost instead of removing it

Sizing the map lazily from the first term that has postings is one line and removes the floor
completely — a query matching nothing allocates 16 bytes. It also **made the realistic case
worse**, which is the outcome the original comment predicted:

| Query, 200k in-memory corpus | corpus hint | first-term hint |
| --- | --- | --- |
| three terms, rarest first | 11,200,864 B | **15,930,392 B** |

Sizing from a three-document term and then growing to a 200,000-document one costs the doublings,
and 42% more than the hint it replaced. Real queries are multi-term, so this was not a trade
worth taking on its own.

## 3. `Index.PostingBound`, and why the bound was already on disk

The third option is to size from the widest list the query will *actually* walk, which needs
the lengths before the loop that produces them. It turns out to cost almost nothing: a term's
postings entry begins with its **block count**, and every block but the last holds exactly
`blockSize`. One varint per term per segment gives a bound tight to within one block, without
decoding a posting.

`decodeTermPostings` already computed exactly this and handed it to `size` so `Lookup` could
allocate its slice once. What was missing was a way to ask for it without also asking for the
postings.

```go
widest := 0
for _, term := range terms {
    widest = max(widest, s.ix.PostingBound(term))
}
acc := make(map[engine.DocID]float64, min(widest, docs))
```

It is a **bound and not a count**, and the distinction is load-bearing in two directions: a
bound below the truth is a map that grows anyway, which is harmless, while a caller treating it
as a count would be reading a number that does not subtract tombstones — a deleted document
keeps its posting and `Lookup` filters at read time.

## 4. The result

Same benchmark, 50,000 documents committed and reopened, before and after:

| Query | ns before | ns after | B before | B after |
| --- | --- | --- | --- | --- |
| a term no document holds | 78,685 | **151** | 1,182,240 | **16** |
| a term in 3 documents | 101,285 | **982** | 1,185,247 | **7,166** |
| a term in 1 in 50 | 176,454 | **84,655** | 1,216,432 | **70,415** |
| a term in every document | 5,838,555 | 6,085,817 | 2,818,997 | 2,818,797 |

**521× faster and 73,890× less memory** on a query matching nothing; **103×** and **165×** on a
rare term; **2.1×** and **17×** on a mid-frequency one. The expensive query is unchanged in
memory to within 200 bytes and unchanged in time to within run-to-run variation — the two
measurements of it differ by 4% in opposite directions across runs, which is the noise band at
100 iterations and not a result.

**Nothing about the ranking changed.** Map sizing does not touch a score, and
`internal/eval/bm25_test.go` — the check that matches `rank_bm25` to 4.44e-16 — is what says so
rather than an assertion here.

## 5. What this does and does not license

**It does not re-open milestone 5's numbers.** `108.193 ms` was measured under a load-point rule
[milestone 7](#milestone-7--a-baseline-nobody-has-to-qualify) then showed is not reproducible,
and this round ran a Go microbenchmark rather than the ladder. What is claimed is what was
measured: the per-query allocation floor, on one machine, at two corpus sizes.

**It is a strong reason to expect the collapse to move, and no evidence that it does.** The
wall milestone 5 named is live heap under concurrency; this removes between 1.18 and 4.73 MB per
query of it, before any candidate is scored. Whether the arrival rate at which p50 goes from
39 ms to 1.27 s moves as a result is a ladder run, and the ladder has come back void three
rounds running ([milestone 14](#milestone-14--the-probe-passed-the-ladder-ran-twice-and-a-closed-lid-discarded-both)).

## 6. Carried forward

1. **The ladder is still owed, and now has a second reason to run.** It was owed a reproducible
   load point; it is now also the only thing that can say whether §4 moves the collapse.
2. **`scorer/vector` and `scorer/graph` have not been measured this way.** The accumulator
   pattern was `scorer/text`'s; whether either of the others carries a corpus-sized allocation
   of its own is unlooked-at. `graph.PPR` allocates 230 times a query by construction, which is
   evidence about the new scorer and not about the old ones.
3. **`DocLen` still takes the index-wide read lock once per posting.** The `ponytail:` note in
   `scorer/text` has said so since milestone 3 and nothing here changed it. A term held by a
   million documents is a million lock acquisitions, and that cost gets *worse* as cores are
   added — which is the shape of a throughput wall and is exactly what this round did not
   measure.
4. **`PostingBound` is a bound and could be a count.** Making it exact means summing the last
   block's posting count, which is one more varint at a known offset per segment. Nothing needs
   the exact number yet; block-max WAND would.
