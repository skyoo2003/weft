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
