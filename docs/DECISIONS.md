# Decisions

Only choices that are expensive to reverse. Anything readable from the code is not recorded here.

Every entry has the same four parts: the **question**, the **decision**, **why**, and **what would show it was wrong**. The last one is the point — a decision with no falsification condition is a preference.

| | Decision | Milestone |
| --- | --- | --- |
| D-001 | Defer the cursor interface; block-structure the postings now | 2 |
| D-002 | Shortcuts are repaid on evidence, not on schedule | 2 |
| D-003 | A commit is a full snapshot | 2 |
| D-004 | The graph verdict needs two conditions, fixed in advance | 4 |
| D-005 | Keep the graph scorer, weight it down | 4 |
| D-006 | Map the segments, so the read API does not grow an error | 3a |
| D-007 | Format v2 refuses version 1, once | 3a |
| D-008 | The engine knows the geometry, the scorer keeps the metric | 3b |
| D-009 | The load is open-loop, and bleve lives in a submodule | 5 |
| D-010 | Adoption is decided by a trial | 6 |
| D-011 | A repetition is a rung, not a ladder | 7 |
| D-012 | Mark the falsified rule; do not replace it from inside | 7 |
| D-013 | A repetition is the ladder, named rather than derived | 8 |
| D-014 | The memory pass line reads the process's mark | 8 |
| D-015 | One line of exported surface, rather than a changed lifetime | 8 |
| D-016 | A buffer, not an iterator | 8 |
| D-017 | A second mutex, and `Commit` takes a context | 9 |
| D-018 | Tombstones leave the statistics immediately | 11 |
| D-019 | DocIDs are never renumbered, so deletion reclaims nothing | 11 |
| D-020 | `k` stays one number | 12 |
| D-021 | A value attaches to a position | 12 |
| D-022 | The tokenizer is a seam on the constructor | 13 |
| D-023 | The mismatch guard recomputes one document | 13 |
| D-024 | The gate on a performance run is a loaded probe | 14 |
| D-025 | The server is a `cmd/`, and the library is still the product | 24 |
| D-026 | The handshake claims OpenSearch 2.19.0, and that is the only lie | 24 |
| D-027 | The mapping is the server's, and it has five types | 25 |
| D-028 | A required clause is one stream | 25 |
| D-029 | `bool.filter` is emptied after it narrows | 25 |
| D-030 | A weight is a position on the wire too | 25 |
| D-031 | The document's time is a mapping flag | 26 |
| D-032 | Every exported symbol has a command or a recorded reason | 28 |
| D-033 | What OpenSearch has no name for gets a name OpenSearch does not use | 29 |
| D-034 | The native surface is a second spelling, not a second engine | 30 |
| D-035 | gRPC is a second module | 31 |

---

## D-001 — Defer the cursor interface, block-structure the postings now

Milestone 2 · accepted 2026-08-11 · [FINDINGS §3.1, §4.1](FINDINGS.md)

### Question

Milestone 2 was blocked on a circular dependency: skip lists only pay off if a cursor interface exists, and the disk format cannot be designed without knowing whether skip lists are in it.

### Decision

1. **No cursor interface in milestone 2.** It is a performance interface with no performance measurement behind it — that is milestone 5's work.
2. **The postings format is block-structured from the start**, carrying three values per block.

| Field | Purpose |
| --- | --- |
| `maxDocID` | last document id in the block — decides whether the block can be skipped |
| `maxTF` | highest term frequency in the block |
| `minDocLen` | shortest document length in the block |

That is everything block-max WAND (Ding & Suel, 2011) requires. Nothing reads these fields yet; milestone 5 starts reading them.

### Why the circularity dissolves

The real question is not whether to add skip lists now but whether to keep them *addable* later, and that is independent of the consuming interface.

**The costs are asymmetric**, which is the whole argument:

| Deferred | Cost of deferring |
| --- | --- |
| Cursor interface | **Low.** An extension interface, so existing `Scorer` implementations are untouched |
| Block structure and metadata | **High.** Format rewrite plus migration of existing indexes |

Do the expensive-to-reverse half now, defer the cheap half — the same reasoning that kept milestone 1 in memory. Overhead is three varints per block, roughly 6–10 bytes, under 1% at 128 postings per block.

### Why `maxTF` + `minDocLen` rather than `maxScore`

The easiest part of this decision to get wrong. A BM25 term contribution is:

```text
IDF(q) × f·(k1+1) / (f + k1·(1 - b + b·|D|/avgdl))
```

`IDF` depends on `N` and `n(q)`; the normalization term depends on `avgdl`. All three are collection-wide and change on every document added, so a finished `maxScore` written into a block goes stale on the next commit with nothing to signal it.

`maxTF` and `minDocLen` are segment-local and immutable. The bracketed term increases in `f` and decreases in `|D|`, so the pair yields the block's true ceiling, computed at query time against the current `N` and `avgdl`. Accurate, never stale, and no floats in the file.

### Follow-through

- **Keep the block size a constant** with a `ponytail:` comment stating that 128 is convention, not measurement.
- **Unread fields rot silently.** Milestone 2 tests must verify that recorded `maxDocID`, `maxTF` and `minDocLen` match each block's actual contents. Finding them wrong at milestone 5 means they are already on disk.
- **Block skipping depends on postings being ordered by ascending `DocID`.** Deletion or merge breaking that invariant breaks the block metadata with it.

### What would show this was wrong

Milestone 5 adds the cursor interface and the block metadata proves insufficient, forcing a format rewrite. Record here what was missing.

---

## D-002 — Shortcuts are repaid on evidence, not on schedule

Milestone 2 · accepted 2026-08-12 · the `ponytail:` markers in the tree (`grep -rn 'ponytail:' .`)

### Question

Six shortcuts are marked in the code, each naming a ceiling and an upgrade trigger. Every trigger is an observation ("once write throughput is a problem", "once candidate sets far exceed k"), and observations need instruments that do not exist yet. In what order do they get repaid, and what has to exist first?

### Decision

**Group the debt by the instrument that authorizes payment, not by milestone number.** Paying before the instrument exists means optimizing against a guess — the same error each shortcut was taken to avoid.

| Instrument | Arrives with | Authorizes |
| --- | --- | --- |
| Corpus larger than memory | Milestone 3 | `scorer/vector` full scan, `engine.TopK` sort |
| nDCG@10 harness ([DATASETS](DATASETS.md)) | Milestone 4 | over-fetch factor, BM25 `K1`/`B` |
| Load test with GC traces | Milestone 5 | index `RWMutex`, sequential scorer execution |

**Current interest: zero.** All six sit in a small in-memory corpus where the named ceiling is not reached. The correct action for every item right now is none.

### Scale-gated — milestone 3

`scorer/vector` brute force pays first: `O(n·d)` per query dominates `O(n log n)` selection, so an ANN index is the larger win.

`engine.TopK`'s sort has an ordering constraint its marker did not state. **It must not be repaid before the cursor interface question is settled.** With a cursor, early termination replaces bounded selection — a threshold is maintained rather than a k-sized heap — so building the heap first means writing selection logic twice. An ANN index also returns top-k directly, which may take `TopK` off the vector path entirely.

Repay the vector scan in milestone 3; hold `TopK` until after the milestone 5 cursor decision.

### Quality-gated — milestone 4, in two phases

Over-fetching and BM25 parameter tuning both change retrieval depth and scores, and milestone 4's primary job is measuring the graph scorer's contribution. Changing them during that measurement confounds it.

1. **Freeze.** Run the three-arm graph A/B with RRF `k`, `K1`, `B` and the over-fetch factor at current values.
2. **Then sweep**, as a second phase against the same query set.

Otherwise it is unknowable what moved nDCG.

### Evidence-gated — milestone 5, possibly never

The index `RWMutex` and sequential scorer execution are the two items most likely never to be repaid. weft has a single writer by design, so if the milestone 2 commit model stays single-writer, sharding is never justified. Sequential execution only pays off if one scorer dominates latency — and after the vector scan is replaced, the obvious candidate stops being slow.

Do not schedule either. Add the measurement to milestone 5's load test so the evidence appears or does not.

### Action now

None outstanding. Two markers were added afterwards, both scale-gated and both blocking the sequential-execution item: `scorer/text` and `scorer/graph` take the index-wide read lock once per posting and once per link, so fanning scorers out with goroutines as things stand loses throughput rather than gaining it. Batch the reads first.

---

## D-003 — A commit is a full snapshot

Milestone 2 · accepted 2026-08-13 · [FORMAT.md](FORMAT.md), [FINDINGS milestone 2 §3.1](FINDINGS.md)

### Question

Milestone 2's outcome is "the index survives restart, one commit makes all scorers' data visible atomically". Does that require incremental segments — each commit writing only new documents, queries reading across many — or does one rewritten segment per commit satisfy it?

### Decision

**One segment per commit, the whole corpus rewritten, the previous generation deleted.** The MANIFEST nonetheless carries a segment **list** with a generation number, and version 1 constrains the list to exactly one entry.

### Why — the same asymmetry as D-001

Incremental segments drag three problems in with them: multi-segment readers, per-segment BM25 statistics that must be merged at query time, and DocIDs that need a namespace the moment two segments are live. All three are milestone 3's problems, and solving them now means solving them against a guess about scale.

Deferring incremental segments costs O(corpus) per commit — real, irrelevant at in-memory scale, and marked with a `ponytail:` comment. Deferring the *manifest layout* would cost a format migration. So the layout (count + list) lands now, and the policy (exactly one) is a version-1 writer contract the reader enforces.

A second choice folded in: **`Commit` refuses a corrupt manifest** instead of superseding it. Writing a fresh generation over a directory in an unknown state could orphan a commit the caller believes exists. Refusal costs the caller one explicit decision and never costs them data they thought was safe.

### What would show this was wrong

Milestone 3 finds the v1 section formats unusable for multi-segment reading — statistics that cannot merge, offsets that cannot relocate — forcing a version bump that rewrites more than the manifest.

**Outcome:** that is what happened, and not for the reason expected. See D-007.

---

## D-004 — The graph verdict needs two conditions, fixed in advance

Milestone 4 · accepted 2026-08-14 · [EVAL.md](EVAL.md) §3–4, [DATASETS.md](DATASETS.md) §3, D-002

### Question

Milestone 4 decides whether the graph scorer survives, and two existing documents disagree about how to measure it.

D-002 says freeze every constant, measure, *then* sweep. DATASETS §3 requirement 4 says sweep the RRF rank constant *alongside*, because measuring at a fixed `k` risks measuring `k = 60` rather than the graph signal. Both are right about the risk they name, and neither says what result counts as a pass.

### Decision

**Both, with the pass condition written down in advance.** The verdict is **yes** only if both hold:

1. **Frozen.** At `RRFk=60`, `K1=1.2`, `B=0.75`, `SeedN=5`, `MaxDepth=3`, over-fetch=1, the paired 95% bootstrap interval for `+graph` minus baseline excludes zero and is positive.
2. **Stable.** Across the sweep, the sign of that delta does not flip.

Condition 1 holding and 2 failing is **undetermined**, not an improvement. Condition 1 failing is **no**, and `pkg/scorer/graph` is deleted while the `Scorer` interface, `Query.Seeds` and `recency` stay.

The headline is the frozen configuration alone. The sweep is a separate artifact reporting how much the verdict depends on a constant nobody tuned.

### Why the rule has to precede the number

Everything before this milestone was measured against a property of our own code: fusion is invariant to scorer count, a reopened index ranks identically. Those cannot be argued with.

This milestone measures a claim about the world, and the failure mode is not a bug — it is choosing the interpretation that flatters the signal after seeing the data. Fixing the rule first is the only defence, and it costs nothing to write down now.

Two supporting choices are recorded with it:

**No number is published before the instrument is checked against an outside implementation.** This has already paid. The nDCG gain function was specified as exponential on the stated grounds that it matched BEIR; `pytrec_eval` shows `trec_eval` uses linear gain — 0.8597 against 0.7967 on the discriminating fixture. Publishing on a scale nobody else uses would have made every arm comparison incomparable. BM25 agrees with `rank_bm25` to 4.44e-16 once the IDF form is explicitly aligned.

**A limitation that biases toward the signal is stated at the same volume as the result.** There are no query vectors, so the baseline is text alone rather than text+vector, and a weaker baseline is easier for the graph arm to beat. A positive result is therefore an upper bound; a negative result is conclusive.

### One D-002 item retires unpaid

D-002 scheduled over-fetching as quality-gated, to be repaid by giving `engine.Search` a depth parameter. **It needs no parameter.** `fusion.Fuse` scores a document from its ranks alone and passes `k` only to `TopK`, so `Fuse(streams, k*m)[:k]` equals `Fuse(streams, k)`, and over-fetching is `Search(ctx, q, k*m, ...)` truncated by the caller. Asserted across k ∈ [1,5] and m ∈ {2,3,10}.

The marker is **withdrawn rather than repaid** — the ceiling it named was reachable from outside all along. The precondition that makes it true (a fuser must be k-independent in scoring) is now documented on `eval.Arm.Fuse`, because a fuser that normalised by `k` would break it silently.

### What would show this was wrong

The sweep shows the frozen configuration was unrepresentative — the sign holds only near `k = 60`. That would mean freezing first bought a number reading as more solid than it is, and the honest fix is to report the sweep as the headline with the frozen point marked on it.

**Outcome, 2026-08-14: neither.** The sweep found 0 sign flips across 28 configurations, so the frozen point was representative — and the stable sign is negative. The rule worked as intended and returned "no".

One clause did fail, and not the one this decision hedged against: the pre-registered baseline was briefly moved on a number that turned out to be a coverage artifact, then moved back ([EVAL.md §4.1](EVAL.md)). A narrow confidence interval on incomplete data is not a failure mode this decision anticipated.

---

## D-005 — Keep the graph scorer, weight it down

Milestone 4 · accepted 2026-08-14 · [FINDINGS milestone 4](FINDINGS.md), [EVAL.md §6](EVAL.md), D-004

### Question

Milestone 4 answered the PRD's second falsification condition *no*: graph proximity costs 0.1227 nDCG@10, sign stable across 28 configurations. The PRD is unambiguous — keep the interface, discard the graph. So `pkg/scorer/graph` should be deleted.

Executing that turned up a cost neither document accounted for.

### The complication

`graph` is not a leaf. It is one of the **three** signals the milestone 1 assertions are built on, and those assertions are the project's central evidence:

| Site | What breaks |
| --- | --- |
| `architecture_test.go` | "three scorers then four" becomes "two then three" |
| `TestFourthScorerIsUnderOneHundredLines` | measures `recency`, which becomes the *third* signal |
| `Query.Seeds` | kept per the PRD, but its only consumer is the graph scorer |
| `restore_test.go` | restore equivalence is asserted across four scorers, including graph traversal over persisted `Links` |
| `Document.Links` | kept (it is in the on-disk format), but nothing would read it |
| `cmd/weft`, `examples/basic` | both demonstrate graph proximity |

So the deletion is not "remove a package". It is "reduce the architecture's demonstration from four signals to three, and leave two `Document`/`Query` fields with no reader". The interface survives, which is what the PRD cared about — but the *evidence* for the interface gets thinner, and that evidence is the project's main asset.

### What the weight sweep changed

The options were framed while the graph scorer looked actively harmful at −0.1227 nDCG@10. Testing fusion weights moved that number's owner. [EVAL.md §5.11](EVAL.md): halving the graph stream's weight erases all but 0.0019 of the regression, and no weight makes the signal worth having — the best delta available is exactly +0.0000.

So the accurate description is **not** "a scorer that damages rankings" but "a scorer that contributes nothing, fused by an operator that was amplifying it". Deleting the scorer would have removed the smaller of the two problems and the more useful of the two artifacts.

### Decision

**Keep `pkg/scorer/graph`, mark it, and promote weighted fusion into the library.**

1. **Keep the package.** Its doc comment opens with the measurement, the instruction to weight it down, and a pointer here. A reader cannot enable it believing it helps.
2. **`fusion.FuseWeighted` ships**, with per-stream weights indexed by position. `Fuse` is unchanged and its unweighted path is bit-identical, so every ranking pinned by the milestone 1 and 2 tests is untouched.
3. **The falsification condition is honoured in substance.** The PRD's clause exists so a negative result cannot be quietly ignored; here it is published in FINDINGS, EVAL, README, the PRD milestone table and the package's own documentation.

The two alternatives, priced: *delete as written*, which honours the condition literally and costs the fourth signal; or *delete and promote a replacement fourth signal*, where nothing is queued and inventing a signal to keep a test honest is the kind of move this project's documents exist to prevent.

**The uncomfortable part is kept in view.** This is still the option that leaves code alive after a falsification condition fired, and "we found something more interesting" is exactly the argument a project tells itself when it does not want to delete. Two things distinguish it: the replacement work is done rather than promised (`FuseWeighted` is in `pkg/fusion` with tests), and the scorer is marked at the point of use rather than only in a document nobody reads.

### What would show this was wrong

`FuseWeighted` acquires no caller outside `internal/eval`, and `scorer/graph` is still present and still unweighted at milestone 6. That would mean the finding was used as a reason not to delete rather than as a direction. The check is mechanical: grep for `FuseWeighted` outside `pkg/fusion`.

---

## D-006 — Map the segments, so the read API does not grow an error

Milestone 3a · accepted 2026-08-15 · [FINDINGS milestone 2 §3.2](FINDINGS.md), `pkg/engine/mmap_unix.go`

### Question

Milestone 3 has to read a corpus larger than memory. Two things follow and they pull in opposite directions: reads must not load what they do not use, and the six read methods the scorers call must keep their signatures — the milestone 1 hypothesis is exactly the claim that scale costs `pkg/scorer` nothing.

Reading lazily means reading from a file. A file read fails.

### Decision

**`mmap`, and DocIDs stay one dense `uint32` space.**

The decoders already work over a `[]byte`, so a mapped region reaches them with the parsing, the bounds checks and the verification unchanged. More to the point, **an access to mapped memory cannot fail**, so `Doc(id) (Document, bool)` keeps its shape.

`ReadAt` was the alternative and it is the one that costs: every one of the six read methods would return an error, all four scorers would handle it, and `engine_api.txt` would record the widening — which is precisely what that golden file is for. The measured cost of the choice actually made is three new names: `Scrub`, `Close`, `Merge`.

The DocID half is the same shape of decision. Two live segments could have been `(segment, local id)`; instead a segment owns `[base, base+count)` and the manifest says where. `DocID`'s width is in the golden file, ids stay dense, and `TopK`'s tiebreak still means what it meant.

### What it does not buy, at the same volume

**mmap moves a corpus out of the Go heap and into the page cache. It does not shrink a working set.** The heap assertion is flat — 74,504 bytes at 250 documents and 74,504 at 2,000, 7.9× apart on disk — and flat says the corpus left the heap, not that it stopped needing to be resident.

On the milestone 4 corpus roughly 69% of the 656 MB docs file is vectors, and `scorer/vector` scans every one of them per query. Only an approximate vector index removes that.

### The cost this hides

`Doc` returns `(Document, bool)`, so a record failing its checksum reports the id as **absent**. Corruption and "no such document" are one answer at the read API.

The alternative is the error return this decision exists to avoid, so the trade is taken deliberately and pinned by a test: never a wrong document, never a panic, neighbouring documents untouched, and `Scrub` names the damage.

### What would show this was wrong

A scorer needs to tell absence from damage. The honest fix is then the error return — and the diff it forces across the scorers is the number this decision claimed to be avoiding, so record it here rather than adding a second, quieter channel.

---

## D-007 — Format v2 refuses version 1, once

Milestone 3a · accepted 2026-08-15 · [FORMAT.md](FORMAT.md), D-003

### Question

Version 1 wrote documents as a bare run of variable-length records and rebuilt `byKey` by reading all of them. Neither a `DocID` nor a `Key` could reach its document without decoding every document in front of it — no arrangement of a lazy reader fixes that, only different bytes do. So the format changes. Does the reader migrate v1 or refuse it?

### Decision

**Refuse, with `ErrBadVersion`.** One reason, and it is not a technical one: **weft has no users, and no version 1 index exists anywhere that cannot be rebuilt.** The evaluation directory was the only one, and `weft-eval build` regenerated it from sources still on disk.

FORMAT.md says a migration reads the old version and writes the new one. This milestone is exempt, and **the exemption is not available again**: it rests on a user count, and a user count only goes up.

### What v2 adds, and why each is a format change rather than a code change

| Section | What it makes possible |
| --- | --- |
| `docoff` | `DocID` → the record's offset, with the token count beside it. Fixed width, so entry *i* is arithmetic. BM25 asks for a length once per posting, and a length reachable only by decoding the record would make every posting cost a key, a text and a vector |
| `keys` | sorted `Key` → `DocID`, binary-searchable, so `Resolve` is not a map rebuilt by reading the corpus |
| per-unit checksums | one record, block or entry at a time. The frame checksum covers a whole file, so computing it costs a full read — the cost being removed |

The manifest's contents changed and its *shape* did not: D-003 put a segment list there for this.

**The checksums are seeded, and the seed is the part worth recording.** A document record never carried its own DocID — position named it — so a lazy reader following a damaged offset table would decode a healthy record under someone else's id and return a plausible wrong answer. Binding the id in makes the record prove which document it is.

### D-003's retrospective: what v1 got wrong

D-003 asked what would show it wrong: *"milestone 3 finds the v1 section formats unusable for multi-segment reading, forcing a version bump that rewrites more than the manifest."*

**That is what happened, and not for the reason it expected.** Multi-segment reading was fine — the manifest was a list, as designed. What v1 could not do was let a *single* segment be read lazily: the `docs` section was positional and `byKey` was derived, so both had to be reconstructed in full.

D-003 was watching the seam between segments. The problem was inside one.

### What would show this was wrong

Somebody turns up holding a v1 index they cannot rebuild. Then refusing was the wrong call, and the fix is a converter shipped separately rather than a reader carrying two formats.

---

## D-008 — The engine knows the geometry, the scorer keeps the metric

Milestone 3b · accepted 2026-08-17 · [FORMAT.md §4, §7](FORMAT.md), [FINDINGS milestone 3b](FINDINGS.md), D-006, D-007

### Question

Milestone 3a mapped the index and left half its outcome sentence false: `scorer/vector` scanned every document on every query, so 434 MiB of vectors moved from the Go heap into the page cache without getting smaller. Removing the scan means an approximate index, and that raises two questions at once.

**Where does the approximate index live**, given that milestone 1's hypothesis is that a scorer needs no private store? And **what does it return** — candidates, or scores?

Separately: FORMAT.md obliged a version 3 to bring either a converter or a reader that understands both versions, because D-007's argument was spent on v1. Which?

### Decision

**The partition lives in `pkg/engine`, and `Index.Nearest` returns `[]DocID` and no score.** The engine knows *which documents are close enough to be worth looking at*; the scorer knows *how close each one is*.

**Format v3 brings the reader.** v2 segments open, report no partition, and answer with every id they hold.

### Why the partition is engine's

The hypothesis milestone 1 registered is that a scorer does not need its own copy of the corpus. The partition is not a copy of the corpus and it is not a scorer's private structure — it is **a section of the segment format**: written by the writer, mapped by the reader, framed and checksummed like every other section, walked by `Scrub`.

Putting it in `scorer/vector` would have given that scorer the private store the hypothesis forbids, and would have meant a second writer for a directory with one.

### Why it returns no score

Because the alternative moves half a scorer into the engine to save it a loop. `scorer/vector` holds rules that are about cosine and about nothing else: a zero-norm document has no direction, a non-finite query is an error rather than an empty result, a document of the wrong width is `ErrDimMismatch` and not a skip, and the scan polls its context every 1024 components. Returning `[]Candidate` would have moved all four across the boundary, and then the engine would define what "similar" means for every caller.

**The measurement that says the line is in the right place is the diff on the far side of it: seven lines**, four removed and three added, all in the loop header. Every one of `scorer/vector`'s twelve contract tests passed unmodified.

Two consequences are accepted rather than hidden. `Nearest` returns a **superset** — documents with no vector, with a zero vector, or in a segment with no partition are all in it — so the scorer's skips still do work. And a query of the wrong width gets **every** id rather than none, because narrowing there would turn "you mixed embedding models" into a thin result instead of an error.

### Why the reader rather than a converter

Because it was nearly free, and a converter was not. A segment without a partition has to be readable whatever happens: the pending segment has none, a segment below 16,384 documents has none, and a partition that fails its checksum is treated as none (D-006). So "a v2 segment is a segment with no partition" reuses a branch that already had to exist, where a converter would have been a command to write, a rebuild to run, and a directory in two states while it ran.

`Merge` then does the conversion as a side effect. The rule this generalizes to is in FORMAT.md: **append a section rather than changing one, and the old version stays readable by construction.**

### The cost, measured

`nprobe` had to be raised from the plan's proposed 8 to **64** to hold the quality bar fixed before any of this was built: `text+vector` 0.6211 against a 0.6233 baseline, inside the registered 0.005. recall@10 against a brute-force scan is 0.992, a query is 4.6× faster, and it touches 210 MiB of a 626 MiB `docs` section where the plan predicted 12 MiB.

### What would show this was wrong

1. **A scorer needs the candidates in rank order, or needs the centroid distances.** Then `Nearest` widens to `[]Candidate`, half a scorer is inside the engine, and the size of that diff is the honest price of this record.
2. **A second metric arrives** — dot product on unnormalized vectors, say — and finds the partition unusable because it was trained on L2-normalized vectors. Spherical k-means is a commitment to cosine, made here rather than in the scorer that uses it.
3. **Somebody has to read a v2 index this build cannot open.** The reader is only cheap while the two versions differ by an appended section; the moment a v4 changes a field, this record stops being precedent.

---

## D-009 — The load is open-loop, and bleve lives in a submodule

Milestone 5 · accepted 2026-08-18

### Question

Milestone 5's outcome sentence asks for two things that each have a trap in them.

*"GC pause를 포함한 p99가 공개되고"* needs a load generator, and the obvious load generator is wrong. *"기성 엔진과 같은 자릿수임을 보인다"* needs bleve, and the PRD's own success metrics forbid bleve: *운영 — 의존성: 표준 라이브러리만. 외부 의존성 0개 유지.*

### Decision

**The driver sends on a clock, not on a completion.** Request *i* is due at `start + i/rate` whatever request *i-1* is doing, and its latency is measured from that due time. When the in-flight cap is reached the request is **shed and counted**, never waited on.

**bleve lives in `bench/`, a separate Go module**, and both harnesses import one shared driver from `internal/loadgen` through a `replace` directive.

### Why

**On the loop.** A closed-loop driver lets a stalled server receive less load. The stall then appears as one slow request, because every request that would have arrived during it was never sent, and the p99 that comes out is a p99 of a load the server chose for itself. That is coordinated omission, and a milestone whose entire deliverable is a p99 cannot be measured by the instrument that hides it.

Shedding rather than blocking is the same argument one level down: a driver that waited for a free slot would be waiting on the server again, at exactly the load where the bias matters most. `TestOpenLoopDoesNotLetTheServerSlowTheLoad` distinguishes the two designs by the *count* of slow samples — one for a closed loop, many for an open one.

**On the submodule.** The alternative was to quote a figure bleve's own documentation publishes, and that is not a comparison: different machine, corpus, query set and rank cut. Any of those alone can move a latency by more than the order of magnitude the rule is testing.

A nested module gets both properties at once, because the Go tool's module graph and the working tree are different things: `GOWORK=off go list -m all` at the root still prints one line and `go build ./...` never descends, while `gofmt -l .` and `git ls-files '*.go'` still do. Measured after adding bleve v2.6.0 and its roughly twenty transitive modules: `make deps` prints one line, `make arch` is green, `make spdx` is green.

**On sharing the driver.** The rule being tested is a ratio. A bias present in one implementation of an open loop and absent from the other moves that ratio without moving either engine. That is also why `internal/` and not `pkg/`: the driver is a measurement tool, and putting it in `pkg/` would add it to the public API golden and to the CHANGELOG's promises.

### What it costs

- **The hybrid arm is not compared.** bleve's kNN is behind a `vectors` build tag and needs cgo and a faiss shared library. Taking that on changes the subject, so the comparison covers `text` only — and the arm a user would deploy is `text+vector`.
- **The analyzers do not match.** bleve's `standard` analyzer stems and drops stop words; `engine.Tokenize` does neither. The effects run in opposite directions and neither is plausibly worth 10×, which is all rule 2 asks.
- **A submodule is a second place to keep green.** CI builds it; CI does not run it.
- **Neither `bench` target is in `all` or in CI.** A shared runner's tail latency is a function of whatever else is on the machine, so gating a merge on a p99 measured there makes the gate a coin flip.

### What would show this was wrong

1. **`bench/` acquires a reason to be imported by the main module.** Then the quarantine is load-bearing in the wrong direction. `make deps` is what would catch the drift.
2. **The open loop turns out to be measuring the driver.** If the ladder's upper rungs report `shed` counts large enough that the distribution is mostly of requests never sent, the cap is the instrument's limit rather than the server's, and the fix is a driver process separate from the server process — a rewrite, not a tweak.
3. **Somebody needs the hybrid comparison.** Then faiss enters `bench/`, the "Go engine against Go engine" framing goes with it, and this record stops being precedent.

---

## D-010 — Adoption is decided by a trial

Milestone 6 · accepted 2026-08-19

### Question

Milestone 6's outcome is a claim about readers — *an external Go developer can add their own signal from the documentation and examples alone* — and claims about readers have a failure mode the other milestones did not. There is no metric to compute. The tempting substitute is to look at the API, decide it seems adequate, and ship a paragraph.

Two facts made that substitute unsafe. `engine.Document` and `engine.Query` are both closed structs, so a signal carrying data weft does not model has no field to live in — which reads, from inside the repository, like a missing feature. And `pkg/engine/doc.go` said "adding a fifth scorer means adding a field here", which is the maintainer's own procedure written as if it were everyone's.

So: do we design an extension point — a `Document.Meta` map, a `Query` payload — or first measure whether one is needed?

### Decision

**Measure first, and forbid production changes inside the milestone.**

1. The instrument is a trial: a subject with no prior sight of the tree implements a fifth signal with only `.md` files, `examples/` and `go doc` output, and every point at which it is blocked is recorded. The rules, the boundary and the five pass lines are [ADOPTION.md](ADOPTION.md), committed before the trial ran.
2. A blocker is **docs-closable** or **code-required**, decided by attempting the API arrangement rather than by how hard it felt. Code-required blockers are named and costed, **not fixed here**.
3. `pkg/` changes default to zero, and any diff is the milestone's price tag.

### Why

An extension point touches the on-disk format and `Commit`'s atomicity at once, so designing one is milestone-sized work. Doing it speculatively inside an adoption milestone would have spent that budget on a problem nobody had demonstrated — and, as it turned out, on a problem that does not exist. Both subjects found the caller-held-table pattern unaided. **What was missing was three sentences.**

This is the same rule D-002 applies to performance, moved to documentation: fix what a measurement pointed at, and let the diff be the receipt.

The cost is real and worth naming. A trial run by an agent is a lower bound, not a user study, and this decision accepts a weaker instrument in exchange for one that exists. The alternative on offer was not a better measurement; it was no measurement and a designed feature.

### What would show this was wrong

An external user files an issue that a signal cannot be expressed at all — not "undocumented", but genuinely unrepresentable through `Resolve` and a caller-held table. That would mean the trial's two tasks were chosen for what was easy to measure. The check is not mechanical; it arrives as a bug report.

A weaker signal, and mechanical: `ExampleScorer` and the three paragraphs added to `doc.go` and `search.go` never change again while the same three questions keep being asked.

---

## D-011 — A repetition is a rung, not a ladder

Milestone 7 · accepted 2026-08-20, registered before the campaign measured anything

**Superseded on its central claim by D-013; kept on its arithmetic.**

### Question

[PERF.md](PERF.md) §5 has said "the headline is the median of three repetitions with the spread reported beside it" since milestone 5 was planned. Milestone 5 published one run. It also published no tail at all for `text+vector` — the arm a user would actually deploy — because at four times the per-query cost, ten thousand samples is over five hours.

Neither is a rule that was wrong. Both are rules that were never made operable, and a rule with no procedure is a rule right up until the first time it is inconvenient.

So: what, exactly, is a repetition — and how does an arm that cannot be laddered three times get a publishable tail without lowering the bar that makes a tail worth reading?

### Decision

**A repetition is the same rung measured again, not the ladder swept again.** Repetition 1 sweeps with `-rate 0` and rule 1 selects the headline rate R; repetitions 2 and 3 run `-rate R`. The published figure is their median, with the minimum and maximum beside it.

**And sample depth is staged for `text+vector` rather than the quantile rule relaxed.** A thin ladder (`-rotations 40`, 2,000 samples per rung) selects the load point, because a p50 needs 200 samples and rule 1 reads p50s. A deep rung (`-rotations 200`) at that rate produces the p99.

### Why

**Three sweeps have no common rung.** Every rate on the ladder is `benchUnloaded` scaled by `loadgen.Ladder`, and `benchUnloaded` is 200 sequential requests taken fresh at the start of each run. Three sweeps produce three different sets of five rates. "The 100% rung" in two of them is two different loads, and a median over them is a median over a quantity that changed between observations.

The cost is named: repetitions 2 and 3 do not re-derive R, so they cannot detect that the machine's sequential throughput moved. That is why each run's unloaded p50 is recorded beside its p99. R is also quoted to two decimals, so repetitions 2 and 3 run about 0.04% off repetition 1's actual rung.

**Relaxing `Printable` was the alternative, and it was refused.** Printing a p99 off two thousand samples would have given `text+vector` a tail immediately. It would also have made every published quantile in this repository mean something different from what [PERF.md §2.3](PERF.md) says it means, to buy one number.

**Registered before, not written after.** D-004 fixed milestone 4's verdict conditions before its numbers existed and D-010 committed `ADOPTION.md` before the trial ran. The thing that makes these rules worth anything is that they were not available to be chosen once the numbers were on screen.

### What would show this was wrong

**The three observations agree to within noise, run after run.** Then the repetition campaign is 3.1 hours buying a spread that was never in doubt, and rule 3 should collapse back to one run.

**The thin ladder selects a different rung than the deep one.** Then rule 4's staging is not a cost-saving, it is a false claim that sample depth does not move rule 1. The repair is not to widen the thin ladder but to say so in the published figure, because the same doubt then applies to every headline rule 1 has ever selected.

---

## D-012 — Mark the falsified rule; do not replace it from inside

Milestone 7 · accepted 2026-08-21 · [FINDINGS milestone 7](FINDINGS.md), D-011

### Question

D-011 decided, one day before the campaign ran, that a repetition is the same rung measured again. The campaign then measured 25.67 q/s three times:

| observation | p50 | shed | RSS |
| --- | --- | --- | --- |
| fourth rung of a ladder | 37.9 ms | 0 | 114 MiB |
| single rung | 1.539 s | 14% | 1021 MiB |
| single rung | 416 ms | 11% | 765 MiB |

Same corpus, same binary, same machine, same day, no suspension in any of them. The difference that survives every check is that the flat observation was the fourth rung of a ladder and the two collapses were single rungs out of a warm-up. **A rung measured alone is not the rung D-011 thought it was repeating.**

Rule 3 is falsified. Do we replace it now — three full sweeps, a pinned-rate ladder flag, a fixed prefix — or mark it and stop?

### Decision

**Mark it. Publish the falsification. Do not choose a replacement inside the milestone whose numbers produced it.**

1. [PERF.md](PERF.md) rule 3 stays on the page, with what falsified it named beside it. It is not edited into something that would have worked.
2. "What must a repetition hold constant" becomes an open question against milestone 8.
3. No fourth run. Rule 5 clause 4 already fixed that answer, and a 40× spread is past any reading of it.

### Why

The discipline this repository runs on is that a rule is worth something only if it was not available to be chosen once the numbers were on screen. **That constraint binds hardest exactly when the rule turns out to be wrong**, because the replacement would be picked by someone who has just seen which shapes produce which answers. Three candidate repairs are already visible, and the reason to prefer one over another is currently *which run it would have made look reproducible*.

Marking costs a milestone's headline. Milestone 7 closes with no median and no spread — a worse artifact than the one it set out to produce, and a better one than a median assembled from a rule known to be measuring two different things.

There is also a positive result to protect. The campaign produced an instrument that refuses to publish what it did not measure: a suspended ladder, a ladder cut short, an operator-chosen rate wearing a rule's label. Those are assertions now, not comments.

The cost is named: milestone 8 inherits an unanswered procedural question on top of its engineering one, and its own pass line — *shed 0 at 27.28 q/s* — is not a predicate until it is answered.

### What would show this was wrong

**The ladder prefix turns out not to be the variable.** The reading is a hypothesis with a named alternative — that `inflight` 40 admits a start-of-rung burst a process arriving from a lower rung never sees. If the prefix is ruled out, marking rather than fixing will have cost a milestone for nothing. The experiment is cheap and belongs to milestone 8.

**Nobody returns to the question.** A rule marked as falsified and left standing is one nobody has to argue with. If milestone 8 publishes a performance figure without first answering what a repetition holds constant, this decision will have converted a wrong rule into no rule.

---

## D-013 — A repetition is the ladder, named rather than derived

Milestone 8 · accepted 2026-08-22 · [FINDINGS milestone 8](FINDINGS.md), [PERF.md §5.2](PERF.md), D-011, D-012

### Question

D-012 refused to repair rule 3 from inside the campaign that falsified it, and named the experiment that would license a repair. PERF.md §5.2 registered that experiment, with what each of four outcomes would license, before any of it ran.

**Outcome 1 fired.** 25.67 q/s reached as the fourth rung of a ladder whose earlier rungs ran 10,000 samples each gave p50 **37.827 ms** with **shed 0**, against milestone 7's 37.852 ms and shed 0 — 0.07% apart, with the collector's cycle count 0.12% apart. The same rate with no prefix collapsed at both `inflight` values. A prefix of the same shape at a fifth of the depth collapsed hardest of all.

So rule 3 needs an operable form. Is a repetition a rung, a ladder, or something else — and if it is a ladder, what happens to rule 1's refusal to label one?

### Decision

**A repetition is the same ladder, with its rates named rather than derived.**

1. Repetition 1 is `-rate 0`. The sweep derives the rates and rule 1 selects the headline rate **R**.
2. Repetitions 2 and 3 are `-rates <repetition 1's rungs, through R>` — the same prefix, the same rates, named so they are shared rather than re-derived.
3. The published figure is the median of the three at R, with the spread as minimum and maximum beside it. Each repetition's own unloaded p50 is recorded.
4. **Rule 1 does not change.** Repetitions 2 and 3 print no headline label, because R was selected once — by the sweep — and is being reused.
5. `text+vector` gets **one** named ladder rather than three. That is not a new decision: it is rule 6's first cut, applied to a budget that grew.

### Why

D-011's argument was never wrong about *derivation*: three sweeps take three fresh `benchUnloaded` readings, so their rungs are three different loads. What it did was conclude from that that the ladder cannot be the unit — when the actual consequence is only that the rates cannot be *derived* twice. Naming them removes the whole difficulty.

The reason to accept the cost rather than look for a cheaper unit is that **the cheaper unit is the one that failed**. A single rung is 6.5 minutes and gives 37.9 ms or 1.539 s depending on nothing the report records. A named ladder is 97 minutes and has now given the same number twice, rung for rung.

Choosing this repair now is legitimate for exactly the reason choosing it in milestone 7 would not have been: the outcome that licenses it was written down before the run that produced it.

### What would show this was wrong

- **A third named ladder does not reproduce the first two.** One reproduction is one. Then a repetition is not a ladder either, and the honest position reverts to milestone 7's — that this workload has no reproducible load point on this host.
- **The deep prefix collapses at `inflight` 10.** §5.2's registered outcome 1 says "at both `inflight` values" and only one was run. If the other collapses, prefix depth is necessary and not sufficient.
- **The prefix requirement does not travel.** If it is a property of this corpus on this host, a procedure defined by it produces figures that are reproducible and local.
- **Nobody pays the 4.9 hours.** Three named ladders per published headline is six times D-011's budget. Quietly reverting to single runs converts a correct rule into no rule.

---

## D-014 — The memory pass line reads the process's mark

Milestone 8 · accepted 2026-08-22 · [FINDINGS milestone 8 §7–§8](FINDINGS.md), [PERF.md §2.7](PERF.md)

### Question

Milestone 8's pass line is *shed 0, RSS ≤ 250 MiB, p50 ≤ 100 ms at 27.28 q/s*. Two of the three were met. The third could not be read: `ru_maxrss` is a high-water mark with no reset and no during-this-rung value, so the 345.2 MiB printed at the 27.28 q/s rung is the mark **13.64 q/s** set two rungs earlier, and the rung under test added nothing to it.

FINDINGS published two readings and chose neither, because the reason to prefer one at that moment was which verdict it produced.

Which reading does the memory clause mean — the rung's own peak, or the process's? And does the answer fire milestone 10?

### Decision

1. **The clause reads the process's mark over the ladder up to and including the load point.** Not the rung's own peak.
2. **Under that reading milestone 8 misses it: 345.2 MiB against 250 MiB.** Recorded as a miss.
3. **Milestone 10 does not fire.** Its trigger is a miss *after* the milestone's engineering, and milestone 8 has done none. The miss is the first target that engineering has, not the verdict on having tried.

### Why

**The metric exists for an adopter's memory budget, and an adopter runs a process, not a rung.** What an adopter meets first is not architectural openness, it is 853 MiB and 12.5 seconds. Anyone sizing a container from a steady-state figure and ignoring the ramp gets killed during the ramp.

It is also the reading the project has always used. Milestone 5 published "RSS 126→853 MiB" as a ladder progression of process marks. Choosing it now is continuity — and the reading that is *not* continuous is the one that would have made this ladder undecidable rather than a miss.

The direction it errs is worth stating: a ladder touches more load points than a steady server at any one of them, so its peak is an **upper bound**. A pass line that errs toward demanding less memory than the measurement shows is the safe direction for the person the metric is for.

**On milestone 10 not firing.** The PRD's clause reads "if milestone 8 cannot clear the absolute pass line". The reading that makes that a trigger rather than a starting gun is *cannot clear it having tried*. What this campaign produced is the opposite of exhaustion: a specific, measured, localised target — 345.2 MiB set at 13.64 q/s, against a cause the PRD already names.

**What is not being done, and why.** A per-rung RSS would make the other reading decidable and is declined: on Darwin it needs `task_info` through cgo or `golang.org/x/sys`, and the first breaks the build's shape while the second breaks `go list -m all` being one line. Linux would take `/proc/self/statm` and nothing else, so the instrument would answer on one platform and not the one the figures are measured on. Giving each rung its own process is ruled out separately: it destroys the ladder prefix D-013 established.

### What would show this was wrong

- **The peak is not candidate materialisation.** A profile showing the 345.2 MiB is mapped index pages the process cannot avoid touching would mean there is nothing for candidate-level work to cut, and milestone 10's trigger becomes live.
- **Reducing the peak costs nDCG past the registered −0.005 tolerance.** Then the throughput target and the accuracy invariant are in conflict, which is a larger finding than either.
- **The ladder-wide reading turns out to hide the load point.** If a per-rung instrument ever shows 27.28 q/s sitting comfortably under 250 MiB, the miss was a property of the ramp rather than of the load — still a real number, and no longer a statement about the rate the pass line names.
- **Nobody attempts the engineering.** A miss recorded as "the target for work not yet done" is worth exactly as much as the work.

---

## D-015 — One line of exported surface, rather than a changed lifetime

Milestone 8 · accepted 2026-08-22 · [FINDINGS milestone 8 §9](FINDINGS.md), D-014

### Question

`Index.Doc` decodes a whole record — key, text, links, vector, time — and the vector scorer reads one field of it. Measured on a synthetic corpus, scoring 64 documents allocated 4,208,024 bytes against 13,312 for the same vectors with 4 MiB less text: one full copy of the document text per candidate scored, for a field nothing reads.

The PRD registered **golden API files byte-identical** as an invariant for this round. Three routes cut the copy and each one moves something registered:

1. **An additive read accessor** — clean and safe; the golden file gains a line.
2. **A zero-copy decode** — `Document.Text` and `.Key` alias the mapping. Signature byte-identical, so the invariant passes as written.
3. **Fewer candidates** — narrow `Nearest`. No API change at all; costs recall.

### Decision

**Route 1.** `Index.Vector(DocID) ([]float32, bool)` is added, and the golden API file gains exactly one line. `pkg/fusion` stays at zero, `public_api.txt` does not move, `go list -m all` stays one line, the `Scorer` interface and every existing signature are untouched.

### Why

**The invariant's purpose is narrower than its wording.** It exists so that performance work cannot quietly cost the architecture — a fuser that learns signal types, a `Scorer` that grows a method, a dependency. An additive read accessor touches none of those, and the project has added exported methods in three prior milestones (`Scrub`, `Close`, `Merge`, `Nearest`) as ordinary changelog entries.

**Route 2 is worse for looking better.** It keeps the file byte-identical while changing what a `Document` *means*: a value held past `Close()` would point into an unmapped range, so the same signature would carry a new lifetime rule and the failure mode is a segfault in a caller's process. An invariant that a change can satisfy by making the same API more dangerous is measuring the wrong thing.

**Route 3 is not this decision's to take.** It trades recall — 0.992 in milestone 3b — for memory, against an nDCG tolerance of −0.005, and sizing it needs a measurement campaign rather than a code change. It stays available and unspent.

**The decision is the operator's, taken explicitly.** The three routes and their costs were put to them before any of the three was written, because picking the one that unblocks the work is precisely what D-012 refuses.

### What would show this was wrong

- **The saving never gets measured.** The arm it helps has no published memory figure, so the line of exported surface has been spent against a synthetic benchmark.
- **One accessor becomes four.** `recency` reads `Time`, `graph` reads `Links`, and both call `Doc` for it. If each gets its own accessor the exported surface grows by a field-shaped method per scorer, which is the closed-`Document` design turned inside out. A second accessor should have to argue harder than this one did.
- **The format drifts between the two modes.** `decodeDocFields` is one function so the layout is written down once, and `assertReadAPIsAgree` checks that `Vector` and `Doc` answer the same on both sides of a commit.

---

## D-016 — A buffer, not an iterator

Milestone 8 · accepted 2026-08-23 · [FINDINGS milestone 8 §10](FINDINGS.md), D-014, D-015

### Question

D-014 recorded milestone 8's memory clause as a miss and called the target "localised", naming 30,549 candidates decoded per query. FINDINGS had already corrected half of that: every ladder ran the `text` arm, where no scorer calls `Doc`, so the candidate decode is the vector arm's cost and the 345.2 MiB was unattributed.

Two guesses were then made about what a `text`-arm query allocates, and the instrument built to check the first is what refused it:

| accumulator hint | KiB/query |
| --- | --- |
| corpus-sized | 15,691.2 |
| first posting list | 19,501.6 |

The `allocs` profile then attributed the whole figure: **53.2% is `Index.Lookup`** materialising a term's entire posting list into a fresh slice, per term, per query; 44.3% is the accumulator map and the candidate slice.

So: remove the per-term materialisation how — a streaming iterator, which is what milestone 10's sentence describes, or a caller-owned buffer, which is not a new shape at all?

### Decision

**A buffer.** `Index.LookupInto(term string, buf []Posting) []Posting` is `Lookup` writing into the caller's array. `pkg/scorer/text` keeps one across a query's terms, so what is live is the longest posting list rather than the sum of them.

**And the first guess is reverted**, on the measurement rather than on the property test it passed.

### Why

**D-006 decides it, not performance.** A segment that claims a term and cannot decode it makes the whole lookup absent — absence is what this repository gives corruption on the read path, because a partial posting list is not a shorter answer, it is a wrong one. That verdict arrives *after* some of the term's postings are decoded. Postings sitting in a buffer can be discarded; postings already yielded to a caller cannot.

**It is also the smaller change.** `scanPostings` already streams, because `Merge` needs it to; `lookup` is the only caller that builds a slice. One exported method, no new interface, no change to what `Search` or `Fuser` see.

**The lock stays where it was.** One read lock for the whole walk, released before returning. An iterator yielding under the lock would put the scorer's `DocLen` call inside it, and a second `RLock` from a goroutine holding one deadlocks the moment a writer queues behind it.

**Why the first guess loses.** Hinting the accumulator from the first posting list is correct for a narrow query and wrong for the workload the clause is judged on: a TREC-COVID query's term union really is most of the corpus, so the map doubles its way up and every abandoned table is charged to the query. Worse for a *peak* specifically, because during a growth the old table and the new one are live together.

The trade does not vanish by taking the other side — a narrow query still pays 4.52 MiB for a map holding eight entries — and what would remove it is a posting count in the terms index, which `termSpan` does not carry. So the trade is published rather than resolved.

**The invariant.** A second line on `engine_api.txt`, after D-015's first. `public_api.txt` does not move, `pkg/fusion` is untouched, `go list -m all` is one line, and `make eval` returns nDCG@10 identical to four decimals.

Milestone 10 is not what this is. Its sentence is "sorted traversal and early termination, so candidates are never fully materialised", and early termination is the half that could force a stream to declare its own score bound. Nothing here declares a bound or reorders a traversal.

### What would show this was wrong

- **The ladder does not move.** 30.7% off what a query allocates is a prediction about the 345.2 MiB and not a measurement of it.
- **A second caller wants the postings and cannot reuse a buffer.** Two callers with different lifetimes would want the iterator this rejected, and the rejection is about D-006 rather than about there being one caller.
- **The buffer outlives what a caller expects.** The contract is that the result is the caller's until the next call with the same buffer.
- **The trade published instead of resolved turns out to matter.** An adopter whose queries are two selective terms pays 4.52 MiB a query for an accumulator holding a few thousand entries.

---

## D-017 — A second mutex, and `Commit` takes a context

Milestone 9 · accepted 2026-08-24 · [FINDINGS milestone 9 §1](FINDINGS.md), [PERF.md §5.4](PERF.md), D-015

### Question

[FINDINGS milestone 5 §3.3](FINDINGS.md) measured an 11.063 s window in which `Commit` held `ix.mu` exclusively, and a read that arrived inside it waiting 12.539 s — 150× the p50 beside it. Of the window, 20,000 `Add` calls were 49 ms; the other 11.014 s was the commit, and nearly all of that was `buildIVF`.

`buildIVF` only reads the index. The `ponytail:` note on `Commit` had already named the upgrade — encode under the read lock and swap under the write lock — and priced it at capture counting: a partial hand-off of six fields.

Separately, `Commit` took no context, so `cmd/weft-eval`'s write-arm probe had to wait out a whole encode on Ctrl-C.

Two questions that turn out to be one:

1. Encoding under the read lock needs the pending segment to stand still. Pay for the capture counting, or find something cheaper?
2. Make `Commit` cancellable by changing its signature, or by adding `CommitContext` beside it?

### Decision

**A second mutex, `Index.wmu`, and `Commit(ctx context.Context, dir string) error`.**

`wmu` is a plain `sync.Mutex` that every site taking `ix.mu` exclusively takes first — `Add`, `Close`, `Merge`, `Commit`, which is all four. `Commit` holds `wmu` across its whole body while taking `ix.mu` in read mode for the encode and exclusively only for the `adopt`.

**The capture counting is not built.** With `Add` excluded, the pending segment cannot change and there is nothing to count.

**`CommitContext` is not added.** The signature changes, and the golden API file records one line altered rather than one added.

### Why

**`wmu` is what makes the split work at all, and this is the part that is easy to get wrong.** `sync.RWMutex` prefers writers: while a commit holds `mu.RLock` for 11 seconds, one `Add` entering `mu.Lock` makes *every* `RLock` after it queue behind that waiter. Lowering the encode to a read lock without `wmu` does not remove the 12.5 s — it only changes what triggers it, from "a commit is running" to "a commit is running and somebody added a document".

**The gap between the two sections is safe for the same reason.** `sync.RWMutex` has no upgrade operation, so the read lock is released and the write lock taken separately. Nothing can change in between, because every mutator takes `wmu` first and `wmu` is still held.

**Not building the capture counting is a smaller diff and a smaller invariant.** The set written to the segment is exactly the set that was pending when the commit was admitted. That is also what keeps milestone 2's atomicity argument intact: the commit point is still the rename.

**The price is named rather than hidden.** `Add` blocks for a whole `Commit`. It already did, so this is a ceiling and not a regression, and `Index.wmu` carries both the ceiling and the upgrade path.

**The signature changes because there is nothing to preserve.** `v0.1.0` is not cut. An `XxxContext` twin planted where no compatibility exists becomes permanent surface for a compatibility that never existed. `Search` already takes a `context.Context` first, so the twin would leave two entry points with two conventions. The 82 call sites are entirely tests, `cmd` and `examples`.

**Cancellation is defined by the rename, so it needed no new state.** Before the rename, nothing is published. After the rename, `ctx.Err()` is ignored: stopping between the rename and the `adopt` would leave the directory publishing a generation the live index has no mapping for. **A cancellation must not be able to reach a state a crash cannot.**

### What would show this was wrong

- **A caller needs to ingest while a commit runs.** That is the one thing `wmu` forecloses, and the capture counting is then owed with the invariant designed rather than deferred.
- **`during` max stays above 1 s with `during` ≈ `outside`.** The lock would be fixed and the wait would be something else — the 30 MiB segment write plus two fsyncs is the first candidate. A different cause is not a pass.
- **A fifth `mu.Lock` site appears without `wmu`.** The design rests on the enumeration being complete. `grep -n 'mu.Lock()' pkg/engine/*.go` is the check, and `TestReadsMakeProgressWhileACommitEncodes` catches it from the outside.
- **`Merge`'s stop turns out to be what callers actually hit.** It is longer than a commit and this round did not touch it beyond `wmu`.

---

## D-018 — Tombstones leave the statistics immediately

Milestone 11 · accepted 2026-08-25

### Question

`Stats` and `AvgDocLen` are what BM25 normalizes every score against. When a document is deleted, does it leave those numbers at once, at the next commit, or not at all?

The PRD asked it as a conflict: taking a tombstone out of the statistics immediately might fight the commit atomicity milestone 2 bought, and leaving it in makes **every score quietly wrong** — an IDF computed against a corpus larger than the one being searched, and a length normalization against an average that includes documents nobody can read.

### Decision

**Immediately, and exactly.** `deadSet` carries two running totals beside the bitmap — how many documents are tombstoned and how many tokens they held — and `Delete` maintains both in O(1). `Stats` and `AvgDocLen` subtract them.

**There is no conflict with atomicity.** The in-memory set is the authority and the `dead-<gen>` file is its durable copy, published by the same manifest rename that publishes a segment — exactly the arrangement `Add` already has with the pending segment. The anticipated conflict would only exist if the statistics were updated *at* commit boundaries; they are not.

**And `Len` does not follow.** It counted documents and was also one past the highest `DocID`; a tombstone makes those different numbers. `Len` keeps the id bound and `Stats` takes the population.

That split is forced, and by a scorer that never mentions deletion. `scorer/recency` walks `for i := range ix.Len()` and skips whatever `Doc` refuses. Narrowing `Len` to the live count would stop that walk short of the newest documents and return a wrong ranking rather than a slow one — so the one change that would have made `Len` "correct" is the one change that would have forced a scorer edit, which is the milestone's falsification condition.

### What it costs

Two numbers on `Index` that must move together with every mutator, and an `Open` that walks the tombstone set to rebuild the token total — bounded by the set, not the corpus. And a corpus-walking scorer keeps visiting deleted ids: `recency` is O(id space) forever, because nothing reclaims an id.

### The rejected alternative

**Leave tombstones in the statistics.** It costs nothing to implement and the error is small while the deleted fraction is small. Rejected because the error is *invisible*: nothing reports it, no test fails, and a caller comparing weft's ranking against another engine's would find a discrepancy with no name.

### What would show this was wrong

An index with a high deleted fraction measures **higher** nDCG with tombstones left in. Then leaving them in was acting as a length-normalization correction rather than as an error, and the exact answer is exactly the wrong one. Nothing has measured that; [PERF.md §5.5](PERF.md) run B is where the number would come from.

---

## D-019 — DocIDs are never renumbered, so deletion reclaims nothing

Milestone 11 · accepted 2026-08-25

### Question

A tombstone hides a document. Something has to decide when — if ever — its bytes go away, and the honest options are two: compact during `Merge`, or never.

### Decision

**Never.** A deleted document keeps its `docs` record, its `keys` entry and its postings, and every `Merge` copies all of it forward. The only compaction weft has is a full re-index.

Compaction means renumbering, and three separate things rest on `DocID` being what it is:

1. `engine.TopK` breaks ties on `DocID`. Milestone 4 measured 241 reported slots decided by that tiebreak alone, so renumbering moves rankings.
2. Posting lists are ascending by `DocID`, which the block encoder's delta chain, `Merge` and every reader take as given.
3. `Merge` is a *concatenation* of adjacent segments precisely because ids do not move. A compacting merge is a different algorithm with a different cost.

Renumbering is therefore not a local change to `Merge`; it is the `DocID` namespacing problem [FINDINGS §3.4](FINDINGS.md) has carried since milestone 2.

### What it costs, priced

- **Disk grows monotonically with deletions and updates.** An update of a committed document is a tombstone plus an append, so a workload that updates hot documents repeatedly pays for every version it ever wrote.
- **Ids are spent, not documents.** The ceiling `Add` enforces is 2³²−1 *ids*. A corpus updated hot enough exhausts that before it exhausts documents. Nothing has measured where.
- **`Nearest` weakens.** It promised at least k candidates when the index holds k vectors; a segment widens its own probe until it has them and then the tombstones are filtered out afterwards, so fewer than k may survive.

All three are published in [FORMAT.md §8](FORMAT.md) and [LIMITATIONS.md](LIMITATIONS.md) rather than left in a comment.

### The rejected alternative

**Compact during `Merge`.** It is what every mature engine does and it is the right answer eventually. Rejected for this round because it requires `DocID` namespacing first, and because milestone 11's question was whether deletion can be added *without* the scorers learning about it — a question a renumbering merge does not answer any better, at several times the cost.

### What would show this was wrong

A caller whose deleted fraction makes the disk or the `Nearest` recall a problem they can name. [PERF.md §5.5](PERF.md) run B is the first number on either.

---

## D-020 — `k` stays one number

Milestone 12 · accepted 2026-08-26

### Question

`Search`'s `k` is both the per-scorer request size and the size of the fused result. Milestone 6 recorded that as defect 3 and repaid it with prose rather than with an API change. Should milestone 12 now repay it with code?

### Decision

**No. `k` remains dual, and callers pass a `k` above their display size and slice.**

This was fixed before the milestone 12 trials ran, together with the condition that would overturn it, so that the trials could decide it rather than confirm it.

**The registered resume condition:** *if a subject repeats what milestone 6 recorded — that it had to reshape its program to discover that fusing at the display depth outvotes a new orthogonal signal — the sentence failed and the API repays it.*

**The condition did not fire.** Task C's subject fused at 50 and displayed 10, cited `Search`'s doc comment as its source, and listed the point among the things it did *not* have to establish by experiment. Task D's subject never hit it.

One trial subject met this by experiment in milestone 6; none did in milestone 12, with the sentence in place. That is the whole evidence for keeping the API as it is.

### The rejected alternatives

- **`SearchDeep(ctx, q, depth, k, fuse, scorers...)`.** One golden line, two entry points. What it buys is the `cands[:display]` the caller already writes; what it costs is that every adopter must now decide which function they are calling.
- **A `depth` parameter on `Search`.** One golden line changed and every call site in the README, `examples/`, `internal/eval`, `bench` and the tests. Permitted before `v0.1.0`, and it buys the same one line.

### What would reopen it

A subject who meets the truncation by experiment *after* reading `Search`'s doc comment. The next adoption trial is where it would appear.

---

## D-021 — A value attaches to a position

Milestone 12 · accepted 2026-08-26

### Question

`engine.Query` has three fields, no map and no `any`. An external scorer needing an input that changes per query, or a caller needing a constraint the query type cannot express, has nowhere in the type to put it. The PRD asked what shape the extension point should take: a map, an `any`, or a type parameter.

### Decision

**None of the three. Nothing is added.**

Milestone 4 answered this question once already, with `FuseWeighted`: a value attaches to a *position* rather than to a name. The positions already exist and the milestone 12 trials used both.

1. **A per-query input attaches to the scorer's constructor.** The caller builds one scorer per search, binding the value. The corpus-sized half of the input is built once and handed in, so a per-query construction is an allocation.
2. **A constraint attaches to the `Fuser`.** `Search` takes the fuser as a parameter, so a caller needing an intersection rather than a union of votes writes one that reads a chosen stream as a restriction. The convention is positional and is the caller's own; no scorer can observe it.

Two blind subjects reached both arrangements from the public API with **zero code-required blockers** ([ADOPTION §8](ADOPTION.md)), which is what makes this a decision to add nothing rather than a decision to defer.

### The rejected alternatives, all three priced

- **`Query.Extra any`.** One slot. Task C required *two* external scorers sharing one input, so two consumers contend for one field, and a failed type assertion is not an error but an abstention — the exact failure mode `Seeds` already has, which `TestOneQueryTimeValueReachesTwoExternalScorers` now pins.
- **`Query.Extra map[string]any`, keyed by import path.** Namespacing solves the contention. It pays for it with compile-time checking, and `Query`'s own doc comment already argues the other way: a missing map entry is a runtime surprise where a missing constructor argument does not compile. **This is what the rung above would have been** had a trial demanded one.
- **A type parameter, `Query[T]`.** `Scorer` becomes `Scorer[T]` and all four in-tree scorers become generic. That widens the `Scorer` interface, which is the PRD's first-stage falsification condition, so buying type safety this way would cost the hypothesis the round exists to test.

### What this reverses, and what it did not have to

[ADOPTION §7.4](ADOPTION.md) reversed milestone 6's pass line 2 — *code-required blockers are not fixed in this milestone* — on the grounds that milestone 12 would be fixing one a trial had named. **The reversal was authorized and never fired**, because both trials produced zero code-required blockers. It stands for the next round on the same terms: a named blocker, not an anticipated one.

D-010's rule survives a second application intact.

### What would show this was wrong

A signal that needs its per-query input *inside* a scorer the caller does not construct — a weft-supplied scorer, or one buried in a third-party wrapper — where there is no constructor to bind to and the `Fuser` is too late. Nothing has produced one. If one appears, the map above is the shape, and its cost is already priced here.

---

## D-022 — The tokenizer is a seam on the constructor

Milestone 13 · accepted 2026-08-26

### Question

`engine.Tokenize` is a package-level function called from four places — three inside `Index` and one in `scorer/text`. A caller whose language the default splits wrongly has no way to replace it. Where does the replacement point attach, and what does it cost?

### Decision

**A variadic `Option` on the two constructors that already exist, and the index owns it.** `New(opts ...Option)`, `Open(dir string, opts ...Option)`, `WithTokenizer(Tokenizer) Option`, and `Index.Tokenize` as the one way in.

`scorer/text` calls `s.ix.Tokenize(q.Text)`. It **asks** the index — the same shape as the `s.ix.Stats` and `s.ix.LookupInto` calls beside it — and does not **receive** a tokenizer through `Query` or through the `Scorer` interface. That distinction is the milestone's falsification condition.

This is D-021 applied a second time and from the other side. A value attaches to a *position*; milestone 12 found the position for a per-query value was the scorer's constructor, and the position for an index-wide one is the index's.

### The ladder, priced before anything was written

| rung | shape | golden | call sites | verdict |
| --- | --- | --- | --- | --- |
| 0 | `var Tokenize = func(...)`, a package variable | 1 changed | 0 | **rejected** |
| 1 | `New(opts ...Option)` / `Open(dir, opts ...Option)` + `WithTokenizer` | **5 to 7** | **0** | **accepted** |
| 2 | `NewWithTokenizer` / `OpenWithTokenizer` | 4 to 5 | 0 | **rejected** |
| 3 | `New(tok Tokenizer)`, required | 1 changed | **77** | **rejected** |

- **Rung 0** is the cheapest and is unusable. Two indexes in one process cannot have different tokenizers, and writing it while an `Add` runs is a data race — one that a batch job setting it at startup and reading it forever would not even trip `-race` on, so the failure ships silently.
- **Rung 2** costs one to two golden lines *less* than rung 1. What those lines buy is the entry-point count: two rather than four. D-020 rejected `SearchDeep` on the ground that an adopter would have to decide which of two entry points to call before knowing whether they cared. Variadic options also compose: a second option later is zero new entry points, where rung 2 would double them again.
- **Rung 3** buys exactly what rung 1 buys and breaks all 77 `New`/`Open` call sites to do it.

### Bound at construction, and no `SetTokenizer`

1. **No lock on the query path.** Nothing writes the field after the constructor returns, so `Index.Tokenize` takes no `ix.mu`. A read lock there would be one index-wide acquisition per `Add` and per query, protecting a value that cannot change.
2. **A `SetTokenizer` would be a time coupling whose violation is not an error.** Called after the first `Add`, it leaves an index half of whose documents live in a different term space: every query against the other half answers zero hits, forever, with nothing to report. That is the class of silent failure `ErrDimMismatch` exists to refuse, refused the same way.
3. **The open-time guard becomes a statement about the whole lifetime** of the index rather than about one instant of it — see D-023.

**`engine.Tokenize` stays.** It is the default, `internal/eval/bm25_test.go` uses it as the reference implementation the published nDCG figures were measured under, and `Index.Tokenize` falls back to it when no option was given — which keeps a zero-value `Index` usable.

### What weft does not ship

**A second tokenizer.** The Hangul bigram tokenizer that demonstrates the seam lives in `pkg/engine/tokenizer_test.go` and in `ExampleWithTokenizer`, not in `pkg/`. Shipping it would turn a seam into a menu, and morphological analysis collides head-on with the zero-external-dependency constraint.

### What would show this was wrong

A signal that needs a *different* tokenizer for one field or one query while the corpus keeps its own — per-field term spaces, which is a format question and not an options question. Or an adopter who has to reach the tokenizer from inside a scorer they did not construct, which is the same gap D-021 names for per-query values.

---

## D-023 — The mismatch guard recomputes one document

Milestone 13 · accepted 2026-08-26

### Question

A directory indexed with one tokenizer and queried with another does not fail. It answers **zero hits**, on every query, forever — the query's terms and the corpus's terms are different strings, so no posting list is ever consulted and there is nothing in the result to say which of the two happened. Can that be caught mechanically?

### Decision

**Yes, from bytes that were already on disk, and the format does not change.**

`Open` picks the first live document whose stored token count is non-zero, recomputes its tokens with the configured tokenizer, and compares the count against the number `docoff` already holds. Disagreement is `ErrTokenizerMismatch`. A bigram index opened with the default reads 7 tokens on disk against 2 recomputed.

[FORMAT.md §4](FORMAT.md) forbids recomputing a token count from a document's text, and the reason it gives is this round: *"recomputing would let a future tokenizer replacement disagree silently with postings that already exist."* The ban is on recomputing in order to **use** the answer. This recomputes once in order to **compare** it, which is that predicted disagreement being caught rather than committed.

It is the same trade `ErrDimMismatch` makes one layer down: caught at open time it is one refused directory; not caught, it is every query failing for the life of the index. And it is a genuinely new failure mode — a directory that opened yesterday can be refused today — traded against a query that used to answer nothing.

### The rejected alternative: store the tokenizer's name

- **A label can lie and bytes cannot.** A Go function value has no stable name, so what would be stored is a string the caller supplied. A caller who swaps tokenizers without editing the string makes the guard confidently wrong, which is worse than no guard: it certifies the mismatch it exists to catch.
- **It buys a format version.** A version bump has a cost and there is no need to pay it here.

### The third check, written and taken back out

The registered design had a third step: every recomputed term against the segment's terms index, which would have caught a tokenizer producing the right *number* of different terms. **It shipped nothing.**

It failed `TestALyingTermOffsetIsNeverFollowed`, `TestAnImpossibleFrequencyIsRefused` and `TestALyingBlockMinimumIsRefused`, each of which doctors a segment's whole `terms` section and then asserts `Open` **succeeds** with the damage surfacing as absence.

Those tests are right. A terms section that does not claim a live document's terms is what a replaced tokenizer looks like *and* what a damaged one looks like, and the two are the same bytes — so the check reported corruption as a tokenizer mismatch, sending a caller to look for a tokenizer they never changed. It also put verification back into `Open` for one section, which milestone 3 removed and D-006 settled the direction of.

### The ceilings, published rather than discovered

1. **A tokenizer that preserves token count passes**, whatever it does to the terms. A stemmer is exactly that shape, so the guard catches the large failure and not the small one.
2. **The sample is one document.** Re-tokenizing the corpus would make `Open` cost the size of the index, which is the whole of what mapping it bought.
3. **A corpus with no text to judge passes.** It answers nothing under every tokenizer.
4. **A non-deterministic tokenizer disagrees with itself.** `Tokenizer`'s doc comment makes determinism the contract.

All four are in the `ponytail:` comment on `checkTokenizer` and in [FORMAT.md §8](FORMAT.md).

### What would show this was wrong

A real corpus refused by the guard while its tokenizer is in fact the one it was written with — which for a deterministic tokenizer cannot happen, so an occurrence would mean the determinism contract is being broken in the field. Or a caller who wants the count check off because a one-document sample is too weak a signal, which would argue for widening the sample rather than removing it.

---

## D-024 — The gate on a performance run is a loaded probe

Milestone 14 · accepted 2026-08-27

### Question

Two performance rounds in a row produced nothing. Milestone 11's run A died during the index load; milestone 13's spent 97 minutes and came back **void** — every clause missed by one to two orders of magnitude, and the commit *before* the milestone missing them the same way, so the instrument had been measuring the machine.

[FINDINGS milestone 13 §8 item 2](FINDINGS.md) named the gap: *a quiet-machine requirement is now a load-bearing part of the procedure and nothing enforces it.*

### Decision

**A short loaded probe at the top rate, run as its own process, before the ladder.**

`make bench-preflight` is `bench -rates 27.28 -rotations 10` — 500 requests, under 30 seconds. Pass is the probe rung's p50 ≤ **twice the same process's unloaded p50**, and shed 0.

Twice is `loadgen.SaturationRate`'s constant rather than a number chosen for this gate: the first rung past twice the unloaded median *is* saturation, and 27.28 q/s was **not** saturation on the published ladder. A machine that saturates at the probe cannot reproduce that ladder.

### Why not the unloaded median, which is the obvious gate

**Because it was normal on the machine that voided the run.** `benchWarmup` already computes this figure and prints it, so a gate built on it is nearly free — and it passes the exact failure it would exist to catch.

| reading | void run | published | ratio |
| --- | --- | --- | --- |
| unloaded p50 | 34.072 ms | 32.231 ms | 1.06× — **passes** |
| p50 under load, 27.28 q/s | 2.616899 s | 33.470 ms | **78×** |
| ladder peak RSS | 654.1 MiB | 100.7 MiB | 6.5× |

The readings that *did* separate the two machines all require load. The cheapest is the top rate: **1.04× published, 78× void**. Nothing needs tuning between those.

### Why a separate process

`peakrss` is `ru_maxrss`, a high-water mark the kernel never lowers, and D-014 fixed the memory clause as the **ladder's** peak. A probe folded into the ladder would raise the mark to near its top-rung value before rung 1 reported. Its own process has its own mark.

### A documented step, not an enforced one

No Go code, no new flag: `-rates` and `-rotations` both already existed, so the target is six lines of Makefile and `pkg/` stays at zero. What was missing was never the arithmetic — **nobody ran a probe at all** before a 97-minute ladder.

The ceiling is on the target in a `ponytail:` comment: if a fourth ladder still comes back void, the probe becomes a `-preflight` flag with an exit code.

### The rejected alternatives

- **`ru_nivcsw` per rung.** `getrusage` is already called, so one more field is a few lines, and involuntary context switches measure contention **directly**. Rejected for having no threshold: this repository held **zero** observations of the figure. **Revived by** a run where the probe passes and the ladder is void anyway.
- **`uptime` load average around each run.** Zero lines, and a proxy. **Partly adopted**: logged beside `date`, and excluded from the verdict.

### The falsification condition fired on first use — 2026-08-28

**Accepted, and amended by its own test.** Four probes passed — 1.00× to 1.05×, shed 0 — and **two ladder attempts produced no verdict**: the first took SIGTERM at 46 minutes, the second completed eight rungs and both arms printed `DISCARD this run` after the laptop's lid was closed mid-ladder, twice.

The naive conclusion is that the probe is worthless. That is wrong:

| failure mode | detector | worked? |
| --- | --- | --- |
| a **contended** machine | `bench-preflight`, this decision | untested — no attempt failed this way |
| a **suspended** machine | `SuspendTolerance`, present since milestone 7 | **yes**, in-band, both arms, with durations |
| a machine that **stays awake** | `caffeinate -dimsu`, prescribed since milestone 5 | **no** |

**Both detectors behaved correctly; the mitigation is what failed.** So the amendment is not to the probe's pass line, which is untouched, but to two places around it:

1. **The lid stays open.** `caffeinate -dimsu` holds `PreventUserIdleSystemSleep` and has no power over clamshell sleep; the machine slept on AC at 100% charge. Nine milestones prescribed a remedy that does not cover the failure mode `clock.go`'s own comment names. `sudo pmset disablesleep 1` rejected — root, and a machine that never sleeps if the operator forgets to unset it.
2. **The probe is necessary and not sufficient, for two reasons rather than one.** It certifies the machine at the instant it runs and the ladder needs 3.1 hours; and it measures one failure mode of at least three. [PERF §5.7](PERF.md) gains readings 5 and 6.

**What the probe did buy, and it is not nothing.** Twelve independent readings of `alloc 10869.0 KiB/query` across probes and rungs, agreeing with the published figure to one decimal — evidence that both arms run the same work. And the observation that 27.28 q/s as a **lone rung** returns 34–35 ms on this machine while 27.28 q/s as a **ladder's fourth rung** collapses to 1.6–2.0 s.

### What would still show this wrong

A ladder that comes back void on a **quiet, awake** machine after a passing probe — the test the two attempts did not get to run. If that happens the probe is measuring something the ladder does not care about, and `ru_nivcsw` per rung becomes the next instrument.

The opposite failure — a probe that fails on a machine which would in fact have reproduced the ladder — costs one minute and is the direction the threshold was chosen to err in.

---

## D-025 — The server is a `cmd/`, and the library is still the product

Milestone 24 · accepted 2026-09-05

### Question

The founding PRD lists **"compatibility with an existing query language"** among the things this project does not do, and [RESEARCH.md §3](RESEARCH.md) dismisses zinc, blast and phalanx in four words — *they are servers*. An OpenSearch-compatible HTTP surface contradicts the first directly and points the second at weft itself.

Both objections are real and neither survives being looked at closely, which is why this is a decision and not an oversight.

### Decision

**Build it, in `cmd/weftd` and `internal/opensearch`. Nothing under `pkg/`.** The reversal was approved on 2026-09-05 by the maintainer.

The check that makes this a boundary rather than an intention: **milestone 24 changes zero lines under `pkg/`.** `git diff --stat pkg/` is the assertion, `make arch` guards the two golden API files, and `make deps` still prints one module because `net/http` and `encoding/json` are the standard library.

### Why the founding out-of-scope line does not bind

Its sibling — *"query language and parser — queries are built through the Go API; a DSL contributes nothing to proving the architecture"* — was reversed by milestone 20, on grounds that apply here word for word. The rejection was conditional on an unproven architecture. Milestones 1, 16, 18, 19, 20 and 23 have since passed; the open question is no longer whether the design works but whether anyone will use it, and the adoption metric this project registered for itself has **not started**.

### Why `internal/` rather than `pkg/api`

[ARCHITECTURE.md](ARCHITECTURE.md) already argues this for the evaluation harness: a measurement apparatus is not part of the library contract. A server is the same kind of thing. Put it in `pkg/api` and `engine_api.txt` starts carrying HTTP types, every handler signature becomes a semver promise, and the CHANGELOG claim that a release with no entry has nothing for a caller to do stops being true.

`internal/opensearch` imports `pkg/scorer/*`, and that is not a violation. What milestone 1 forbids is **`pkg/fusion` knowing a scorer exists**; `cmd/weft/main.go` has named all four since the beginning.

### What this costs, and it is not nothing

A server is an operational surface with its own failure modes, and this one opens on a codebase [STATUS.md](STATUS.md) calls **not usable in production**. `weftd` binds to loopback and prints that warning at startup. Both are mitigations and neither is a fix; milestone 27 is where the number gets measured through HTTP, and it is blocked on a quiet window that has failed four rounds running.

### What would show this was wrong

**A line needed under `pkg/`.** If the HTTP surface cannot be expressed without widening `Scorer`, `Search` or `Fuse`, then the architecture claim does not survive a process boundary — and that is a finding worth more than the server. It gets written into FINDINGS before the line is written into the code.

The weaker signal: the library falling behind the server. If a capability lands in `internal/opensearch` that a `go get` user cannot reach, this decision has quietly made the server the product.

---

## D-026 — The handshake claims OpenSearch 2.19.0, and that is the only lie

Milestone 24 · accepted 2026-09-05

### Question

`GET /` must return `version.distribution: "opensearch"` and a version number, because every official client reads them and branches on what it finds. weft is not OpenSearch. Saying so truthfully means no client connects, and a compatibility surface nothing connects to is not a compatibility surface.

### Decision

**Claim `2.19.0`, and confine the untruth to that one response.** Everything else is honest: a query type weft cannot express returns **400 or 501**, never 200 with an empty result.

2.19.0 rather than something older because it is the first line carrying the `hybrid` query and an RRF ranker — the shapes that map onto `fusion.Fuse` without translation, and the reason milestone 26 exists. Claiming 1.x would buy a smaller lie and lose the part of the protocol weft is actually good at.

### Why silence is worse than refusal here

This is `pkg/query`'s rule reaching the wire. That package's documentation states it outright: *an unterminated quote, a malformed range, a phrase with no terms — none of them is a query that quietly returns nothing, which is the failure this package refuses everywhere else.*

A server that answers `aggs` with an empty aggregation block commits exactly that failure, at a distance, in someone else's dashboard. So the version string is a handshake token, not a claim about behaviour.

### What would show this was wrong

A client that gets **further in** because of the claimed version and then fails in a way the operator cannot attribute — a 501 arriving somewhere the client has no error path for, so it surfaces as a hang or a silent empty page. If `make compat` finds that shape, the answer is not a lower version number but a documented list of what the claim invites.

---

## D-027 — The mapping is the server's, and it has five types

Milestone 25 · accepted 2026-09-05

### Question

`query.EncodeInt` names its own contract: *the caller indexes it and the caller queries it.* A value encoded one way and queried another matches nothing with nothing to report.

weft's index holds terms and postings and does not hold the fact that `views` was a number, so over HTTP something has to remember it — the client that wrote the document and the client that writes the range query are not the same process, and may not be the same person.

### Decision

The server keeps a mapping in `<dir>/_mapping.json`, beside `_source.json` and published by the same temp-file-then-rename rule.

**Five types:** `text`, `keyword`, `date`, `integer`/`long`, `knn_vector`. Any other type is **refused**, and a field already declared cannot be re-declared to a different type.

### Why five

Each type has to earn itself twice — once deciding what term a value becomes at index time, once deciding what a bound becomes at query time. A type that changed neither answer would be `text` wearing a different name.

`date` and the integers exist so `EncodeTime` and `EncodeInt` can be applied at both moments; `knn_vector` exists so an array lands on `Document.Vector` instead of being dropped as a non-scalar; `keyword` differs from `text` at query time only.

### What was rejected

- **Accepting an unknown type and treating it as `text`** — that is the failure this whole surface is arranged against: a range over a silently-demoted field matches nothing and reports nothing.
- **Inferring the type from the first document** — the width of a `knn_vector` would then depend on arrival order, and `engine.ErrDimMismatch` refuses a mismatched width for the whole commit rather than for one document.
- **Allowing a re-map** — the documents already indexed were encoded by the old rule, so the field would hold two encodings and a range query would read half of it. OpenSearch refuses the same change for the same reason.

### The price, stated

`keyword` does not fully arrive. One index has one tokenizer (D-022 — no per-field analyser), so a keyword value is tokenized like any other field and a keyword holding two tokens is found as a **conjunction** of its tokens rather than as one indivisible term.

Right for ids, statuses and tags; broader than OpenSearch for `"New York"`. Fixing it is a per-field analyser, which is a `pkg/` change, which milestone 25's mechanical definition forbids. Priced in [LIMITATIONS.md](LIMITATIONS.md) instead.

### What would show this was wrong

A sixth type is wanted and cannot be added without changing how the five are read. Then the mapping is doing more than carrying an encoding and belongs somewhere else.

---

## D-028 — A required clause is one stream

Milestone 25 · accepted 2026-09-05

### Question

`query.Must` intersects the positions it is given, and a `match` over two tokens is two streams. Wired the obvious way, `bool.must` holding a two-word match becomes `operator: and` — narrower than the client wrote, with no error and nothing to notice it by.

### Decision

A clause landing in `must` or `filter` is collapsed to **one** stream first:

- a single-stream clause is itself;
- a conjunction (`operator: and`, a multi-token `term`) is its streams, which `Must` can require directly;
- a disjunction of several streams becomes `anyOf` — twelve lines in `internal/opensearch` that union its inner scorers' candidates.

### Why here and not in `pkg/query`

The constraint vocabulary did not need a disjunction; what was missing was a *scorer*, which is the extension point this project is built on. Adding `query.Any` would have grown the library's API for a problem a caller can solve, and [ADOPTION.md](ADOPTION.md) measured callers solving exactly this kind of problem from outside.

`anyOf` returns every nominated document and does not truncate to `k`, which is the rule `pkg/query`'s documentation states for any scorer used as a restriction: a restriction truncated to `k` excludes every document below its own cut, and that is a wrong answer rather than a narrow one.

**`must_not` does not get this treatment**, and the asymmetry is the point: excluding a document present in *any* of the streams is exactly what "this clause did not match" means for a disjunction, so `query.MustNot` over every position is already correct.

### What would show this was wrong

A third occurrence type needs a fourth collapsing rule. Then "a constraint names one stream" is not the right abstraction and the plan should carry a query tree instead.

---

## D-029 — `bool.filter` is emptied after it narrows

Milestone 25 · accepted 2026-09-05

### Question

A filter must narrow without contributing to the ranking. `fusion.FuseWeighted` takes a weight per position, and weight 0 looks like the way to say "does not vote".

### Decision

Filter streams are required by `query.Must` and then **emptied** — `blank`, ten lines — before the base fuser sees them. **Weight 0 is not used.**

### Why

A weight of 0 does not silence a stream's vote; it removes the document from the fused result entirely. A filter written that way excludes everything it was meant to keep, which is the inverse of the request.

The PRD registered this trap in the `bool.filter` row before the code existed, and it was still the first thing tried.

### What it rests on

`query.Must` and `query.MustNot` each hand on a slice of the **same length and order**, so a position stays meaningful through the whole chain `Must(blank(MustNot(Fuse)))`. That property was undocumented; it is now asserted by test.

**The one exception.** A bool holding nothing but filters is not blanked — there is no other ranking, and blanking would answer nothing to a query that named documents.

---

## D-030 — A weight is a position on the wire too

Milestone 25 · accepted 2026-09-05

### Question

OpenSearch's `hybrid` query puts the weighting in a search pipeline's normalization processor: two streams are normalized onto a common scale and then combined. That pipeline is the thing this PRD's §1 says weft exists to make unnecessary.

### Decision

`hybrid` takes an optional `weights` array — weft's, not OpenSearch's — with one weight per sub-query, expanded to one weight per **stream** and handed to `fusion.FuseWeighted`. **`search_pipeline` is refused** with a 400 that says why.

### Why it can exist at all

A weight attaches to a *position*, never to a name. So `FuseWeighted` takes a `[]float64` and still cannot identify a single scorer, and `go list -deps ./pkg/fusion` names no scorer package after this change exactly as before it.

That is D-005's repayment reaching the wire: milestone 4's −0.1202 nDCG@10 was the cost of an unweighted vote from a stream with nothing to say, and a client can now turn that vote down without the fusion learning what the stream holds.

### What the refusal says

Normalizing two streams onto a common scale is the step this server does not have, because rank fusion reads position and never score — `engine.Candidate.Score` is explicitly not comparable across streams. Answering `search_pipeline` would mean inventing a normalization and calling it OpenSearch's.

### The structural guard

Weights are positions in the original stream list and `query.MustNot` is the one wrapper that hands on a shorter one. A hybrid inside a `bool.must_not` would shift every weight after the removed stream by one, silently. It cannot happen, because a hybrid is a whole query rather than a clause of a bool — refused, and tested rather than commented.

### What would show this was wrong

A caller needs a weight that depends on what a stream holds — a boost that means something different for a vector than for a posting list. Then the positional convention is insufficient and the fusion has to learn about signals, which is the architecture hypothesis failing at the wire.

---

## D-031 — The document's time is a mapping flag

Milestone 26 · accepted 2026-09-05

### Question

`scorer/recency` reads `engine.Document.Time` and nothing else. A JSON body has no such field: `{"published": "2024-03-01"}` is a date the client happens to care about, and nothing on the wire says it is *the* date.

### Decision

A `date` property may carry `"recency": true`. **One per index.**

`documentFrom` sets `Document.Time` from that field — and indexes it as a range-able term as well, because a client that mapped a date wants both and neither is derivable from the other. `function_score` with a `gauss`, `exp` or `linear` decay on that field becomes a `recency.NewAt(ix, time.Now())` stream; a decay on any other field is refused by name.

### Why a mapping flag rather than a convention

The alternatives were worse in the way this project cares about:

- *the only `date` field* would break the moment a second date is mapped, and would break silently;
- *a field literally named `time`* would collide with a client's own schema;
- *an index setting* would put the binding somewhere that does not already know the field's type at both index and query time.

`dimension` on `knn_vector` is the same shape of extension — the k-NN plugin's, not core OpenSearch's — so the precedent is one a client already accepts.

### What this cost, and it is milestone 26's actual finding

**33 net lines, all of them index-time plumbing** that a Go caller does not pay: filling `Document.Time` in a Go program is a struct field assignment. The query half — the `function_score` clause — is **57 lines**, inside milestone 1's budget.

**The wire's surcharge on a signal is the binding, not the fusion.**

### Decay parameters are refused

`scorer/recency` is an exponential with a fixed half-life; reading `origin`, `scale`, `offset` or `decay` and ignoring them would rank by a curve nobody asked for. The three curve *names* are accepted and approximated, because a name this server can answer approximately is better than three names it refuses — and a parameter it would silently ignore is not.

---

## D-032 — Every exported symbol has a command or a recorded reason

Milestone 28 · accepted 2026-09-06

### Question

D-025 registered the signal that would show the server had quietly become the product: *a capability lands in `internal/opensearch` that a `go get` user cannot reach.* The inverse was never registered and turned out to be the one that was true.

`pkg/scorer/graph` was the package the HTTP surface did not import at all; `engine.Scrub` had never been called by anything in this repository; `query.Parse` — weft's own query language, milestone 20 — could be reached only by writing a Go program.

None of that is visible from anywhere. The golden API files record what is exported and no test asks whether anything can call it.

### Decision

**`cmd/weft/coverage_test.go` reads both golden files as data.** Every callable symbol they list has a row: the surfaces that reach it and the call site that proves it, or the reason nothing does.

- A symbol with no row fails.
- A row naming a symbol that no longer exists fails.
- A row claiming a surface whose source no longer holds the call fails.

The figure is printed rather than asserted against a threshold:

```text
public callable symbols: 74, reached by a command: 71 (95.9%), reached by none: 3
```

The three are `NewCollector`, `Collector.Offer` and `Collector.Take`, and the recorded reason is that a Collector is the top-k buffer a `Scorer` implementation keeps while it walks postings — reaching it from a command would mean inventing a scorer for a flag to select, and `pkg/scorer` is what that would duplicate.

This is milestone 25's shape reused. `TestTheRefusalRateIsCounted` holds the DSL table as data so that a quietly implemented row and a quietly broken one both break the build; this holds the API surface the same way.

### What it proves and what it does not

The proof is a call site in the surface's own source, found **after comments are stripped** — because this repository writes what it is *not* doing beside the call it is not making, and `internal/opensearch` says "engine.Scrub is not this" in a comment three lines from where a raw text scan would have read it as a call.

It does not prove the call sits on a path a user can reach, and it cannot tell two same-named methods apart: `.Len(` is `Index.Len` and `Adjacency.Len` both. Where that mattered the row carries an explicit call string instead of the derived one, and `.Err(` is the case that caught it — `bufio.Scanner` has one too.

### What would show this was wrong

**A command invented to satisfy the ledger.** The test measures reachability, and the cheap way to raise a percentage is a subcommand nobody would run. The three unreachable rows are the control: if a later round makes them reachable without a use case arriving first, the number is being farmed rather than earned.

---

## D-033 — What OpenSearch has no name for gets a name OpenSearch does not use

Milestone 29 · accepted 2026-09-06

### Question

Half of weft has no OpenSearch spelling. The graph signal is not a query type OpenSearch has; neither is weft's own query string, a term-space walk, or `engine.Scrub`. D-026 confines the untruth to the version handshake — but it does not say what to call a thing OpenSearch never named.

Two ways to get it wrong: reuse an OpenSearch name for different behaviour, which is a second lie and a worse one because a client that already knows the name will not read the documentation; or refuse to expose the capability at all, which is D-025's weaker signal in reverse.

### Decision

**A `weft_` prefix on the clause and a `/_weft/` prefix on the route.**

`weft_graph` is the graph signal; `/_weft/terms`, `/_weft/postings`, `/_weft/scrub` and `/_weft/query` are the four routes with no OpenSearch counterpart. A client sending standard DSL cannot reach any of them by accident, and a client reading one back knows which half of the surface it is on.

`hybrid.weights` is the precedent, an extension inside a query that exists (D-030). This is the same move applied to a whole clause and a whole route.

**Where the honest name is OpenSearch's, it is used.** `_flush`, `_refresh`, `_forcemerge`, `_count` and `_analyze` all do here what those names mean elsewhere — a flush is a commit, and a refresh can only be one too, since a search already reads the live index. Prefixing those would have been the mirror error: hiding a standard capability behind a private name.

### Why `query_string` stays a 501 while `/_weft/query` answers

They are different requests. `query_string` asks this server to read *Lucene's* language, and weft's is not it — the two disagree about `+`, `OR` and parentheses, so translating silently runs a query other than the one written.

`/_weft/query` is a client asking for weft's language by name, on a route that says whose language it is. The refusal was never about the capability.

### What would show this was wrong

**A client that has to be told the prefix exists before it finds anything.** The namespace is cheap to add and cheap to ignore, and the failure mode is a surface where the interesting half is invisible to everyone who did not read this file. If adopters keep asking for a feature that has been reachable under `/_weft/` for months, the prefix is not carrying the meaning it was supposed to carry.

---

## D-034 — The native surface is a second spelling, not a second engine

Milestone 30 · accepted 2026-09-06

### Question

A surface of weft's own was going to need three things the OpenSearch DSL cannot express: a request that *is* a positional stream list, a per-hit pre-fusion breakdown, and a fusion depth separate from the page size.

The plan for it put those in a new package, `internal/weftapi`, with its own request types and its own stream parser — four stream kinds named after the four signals. Reading the code first made that plan wrong in three places at once, and the three have one cause.

### Decision

**`internal/opensearch/native.go`, mounted at `POST /{index}/_weft/search`, compiling streams with the compiler `_search` already has.**

| The plan said | It is | Because |
| --- | --- | --- |
| a new package, `internal/weftapi` | a file in `internal/opensearch` | `compiler`, `plan`, `runSearch` and `apiError` are unexported. A new package would have had to export or copy them, and `admin.go` already hosts `/_weft/*` here |
| a parser for four stream kinds | `compiler.clause`, reused | eleven leaf kinds are streams on the day they land on `_search`, and there is no second list to keep in step |
| the prefix `/_weft/v1/` | `/{index}/_weft/search` | the four routes that predate it are unversioned, there is no released tag yet, and a version segment nothing has needed is speculative |

`hybrid`'s own loop moved out to `compiler.streams` and both callers share it. That is the finding under all three rows: **`hybrid` was always a wrapper around a stream list**, so the native surface is that list with the wrapper taken off rather than a new thing beside it.

### What the shared compiler buys, and what it cost

It buys the property that makes this a spelling and not an engine: a clause that works on one surface works on the other by being cut and pasted, every refusal is inherited rather than re-listed, and a signal added to `_search` is a native stream the same day. `pkg/` changed zero lines and `opensearch-py` still drives `weftd` unmodified.

It cost one defect, and the defect is worth the record because the plan's separate parser would not have had it — it would have had a different one.

**A `streams` entry is not a scorer**: a two-token `match` is two `query.Glob` streams, so the list the fuser sees is longer than the list the client wrote. The first cut of the breakdown truncated the scorer row to the entry count, which put **the second entry's rank under the first entry's label, for every multi-token query, silently.**

That is D-028 arriving in a new place — the same finding that cost milestone 25 an `anyOf` when `bool.must` met it, and the second time this repository has paid for the gap between a clause and a stream.

`compiler.streams` now returns the grouping, and the fold takes the best rank when an entry became several streams. **Not an average**: an average is a score by another name, and this engine never compares scores across streams.

### Why the breakdown reads positions and nothing else

A breakdown that knew stream 2 was a vector would be a fifth place to edit when a sixth signal arrives — and the whole claim being re-tested here is that there is no such place.

`TestTheNativeFusionPathKnowsNoScorer` reads `native.go`'s AST and fails if `capture` or `rankOf` mentions a scorer constructor, which is the same lock `TestTheFusionCodeDoesNotKnowAboutTheFourthSignal` puts on `search.go`.

### What would show this was wrong

**A refusal that has to differ between the two surfaces.** Sharing the compiler means sharing every "no", and the bet is that a refusal is a property of the engine rather than of the protocol. If a native request needs to be allowed where the same clause is refused on `_search`, then the two surfaces do not share a semantics, only a parser, and the file should become the package the plan asked for.

The weaker signal is the one D-025 registered and this makes cheaper to trip: a capability that lands on the native route and never reaches a `go get` user. The native surface is closer to the library's own shape than the compatibility surface is, which makes it a more tempting place to put something that belongs in `pkg/`.

---

## D-035 — gRPC is a second module

Milestone 31 · accepted 2026-09-06

### Question

gRPC cannot be spoken without `google.golang.org/grpc`, which brings protobuf, `x/net` and `genproto` behind it. The founding PRD registers an operational metric — *`go list -m all` prints this module and nothing else* — and `pkg/engine`'s `TestNoExternalDependencies` fails the build if it does not.

Two ways out were real, and they differ by a factor of five in cost.

### Decision

**A nested module at `grpc/`, with a `replace ../`.** The maintainer chose it on 2026-09-06.

The precedent is exact rather than analogous: `bench/` has quarantined bleve since milestone 5, for a requirement that read the same way. `go list -m all` at the root prints one line with `grpc/` in the tree, checked.

The quarantine works because **Go's `internal/` is enforced by import path prefix and not by module**. `github.com/skyoo2003/weft/grpc` sits under `github.com/skyoo2003/weft/`, so it may import `internal/opensearch`; `bench/main.go` has relied on exactly this for `internal/eval` since it was written.

### The alternative, and why it lost

**Hand-rolled: h2c from the standard library, plus a protobuf codec.** Go 1.24 added `http.Protocols.SetUnencryptedHTTP2`, so cleartext HTTP/2 no longer needs `x/net/http2/h2c`, and gRPC's framing is a five-byte prefix and two trailers. One module, zero dependencies, and entirely in the spirit of a repository that wrote its own inverted index, its own IVF and its own DSL parser.

It lost on what it cannot buy. **The point of gRPC is the client ecosystem**, and a hand-rolled wire is only worth having if every stock client drives it — which is 400 to 600 lines of protocol code whose failure mode is silent misencoding against clients this repository cannot run. Five days against one, and the risk is permanent rather than paid once.

Registered so a later reader does not have to re-derive it: if the nested module becomes the thing nobody remembers to build, the hand-rolled version is still available and Go 1.24 is why.

### What this costs

`cd grpc && go run ./cmd/weftg` rather than `go run ./cmd/weftg`, one more `go.sum` in the CI cache key, and a second module that `go build ./...` does not reach.

That last one is the real cost and it has a name: **`bench/` rotted between the runs that used it**, which is why `bench-build` exists. `grpc-build` is the same target for the same reason, and it is in CI from the first commit rather than after the first rot.

### The one thing that is not conversion

`SearchRequest.streams` is `repeated string` — leaf clauses as JSON — and not a `oneof` over the clause kinds. That is D-034's finding applied to the second wire: a typed union would be a second list of what a stream can be, kept in step by hand with `compiler.clause`, and it drifts on the first clause added to one surface and not the other.

The *response* is fully typed, which is where a client wants types. The trade is honest and it is a trade: a gRPC client gets no compile-time help writing a query. If that turns out to be the thing adopters trip over, the answer is a generated `oneof` **derived from** the clause table rather than written beside it.

### What would show this was wrong

**A second module nobody runs.** `make grpc-build` in CI is the mitigation and not the proof; the proof would be a release where the gRPC surface was broken for weeks and no one noticed, which is the failure `bench/` already had once. The answer then is not a third module — it is folding the surface into the root and paying the dependency, with the metric formally withdrawn in a decision of its own rather than quietly.

The weaker signal: `TestTheProtoAndTheCoreDoNotDrift` starting to need exceptions. It has one today (`index`, which lives in the URL path on HTTP and has nowhere to sit in a body-decoded struct) and the reason is structural. A second and a third would mean the two surfaces are no longer one request in two encodings, and the drift test would be documenting the drift instead of preventing it.
