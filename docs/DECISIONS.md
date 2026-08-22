# Decisions

Only choices that are expensive to reverse. Anything readable from the code is not recorded here.

---

## D-001 — Defer the cursor interface; make the postings format block-structured now

**Status:** accepted, 2026-08-11
**Context:** [FINDINGS §3.1](FINDINGS.md), [§4.1](FINDINGS.md)

### Question

Milestone 2 was blocked on a circular dependency: skip lists only pay off if a cursor interface exists, and the disk format cannot be designed without knowing whether skip lists are in it.

### The circularity dissolves

The real question is not whether to add skip lists now but whether to keep them addable later, and that is independent of the consuming interface. Writing postings in blocks with three values per block is sufficient:

| Field | Purpose |
| --- | --- |
| `maxDocID` | last document id in the block — decides whether the block can be skipped |
| `maxTF` | highest term frequency in the block |
| `minDocLen` | shortest document length in the block |

That is everything block-max WAND (Ding & Suel, 2011) requires. Nothing reads these fields yet; milestone 5 starts reading them.

### Why `maxTF` + `minDocLen` rather than `maxScore`

The easiest part of this decision to get wrong. A BM25 term contribution is

```text
IDF(q) × f·(k1+1) / (f + k1·(1 - b + b·|D|/avgdl))
```

`IDF` depends on `N` and `n(q)`; the normalization term depends on `avgdl`. All three are collection-wide and change on every document added, so a finished `maxScore` written into a block goes stale on the next commit with nothing to signal it.

`maxTF` and `minDocLen` are segment-local and immutable. The bracketed term increases in `f` and decreases in `|D|`, so the pair yields the block's true ceiling, computed at query time against the current `N` and `avgdl`. Accurate, never stale, and no floats in the file.

### Decision

1. **No cursor interface in milestone 2.** It is a performance interface with no performance measurement behind it — that is milestone 5's work. Designing it now means designing against a guess.
2. **Postings format is block-structured from the start**, carrying `maxDocID`, `maxTF` and `minDocLen` per block.

### Rationale — the costs are asymmetric

| Deferred | Cost of deferring |
| --- | --- |
| Cursor interface | **Low.** An extension interface, so existing `Scorer` implementations are untouched ([FINDINGS §3.1](FINDINGS.md)). |
| Block structure and metadata | **High.** Format rewrite plus migration of existing indexes. |

Do the expensive-to-reverse half now, defer the cheap half — the same reasoning that kept milestone 1 in memory. Overhead is three varints per block, roughly 6–10 bytes, under 1% at 128 postings per block. Writing fields nobody reads yet is the intended cost.

### Follow-through for milestone 2

- **Keep the block size a constant** with a `ponytail:` comment stating that 128 is convention, not measurement.
- **Unread fields rot silently.** Milestone 2 tests must verify that recorded `maxDocID`, `maxTF` and `minDocLen` match each block's actual contents. Finding them wrong at milestone 5 means they are already on disk.
- **Design alongside [FINDINGS §4.3](FINDINGS.md).** Block skipping depends on postings being ordered by ascending `DocID`; deletion or merge breaking that invariant breaks the block metadata with it.

### What would show this decision was wrong

Milestone 5 adds the cursor interface and the block metadata proves insufficient, forcing a format rewrite. Record here what was missing.

---

## D-002 — Deliberate shortcuts are repaid on evidence, not on schedule

**Status:** accepted, 2026-08-12
**Context:** the six `ponytail:` markers in the tree (`grep -rn 'ponytail:' .`)

### Question

Six shortcuts are marked in the code, each naming a ceiling and an upgrade trigger. Every trigger is an observation ("once write throughput is a problem", "once candidate sets far exceed k"), and observations need instruments that do not exist yet. In what order do they get repaid, and what has to exist first?

### Principle

Group the debt by the instrument that authorizes payment, not by milestone number. Paying before the instrument exists means optimizing against a guess — the same error each shortcut was taken to avoid.

| Instrument | Arrives with | Authorizes |
| --- | --- | --- |
| Corpus larger than memory | Milestone 3 | `scorer/vector` full scan, `engine.TopK` sort |
| nDCG@10 harness ([DATASETS](DATASETS.md)) | Milestone 4 | over-fetch factor, BM25 `K1`/`B` |
| Load test with GC traces | Milestone 5 | index `RWMutex`, sequential scorer execution |

### Current interest: zero

No marker costs anything today. All six sit in a small in-memory corpus where the named ceiling is not reached. **The correct action for every item right now is none.**

### Scale-gated — milestone 3

`scorer/vector` brute force pays first: `O(n·d)` per query dominates `O(n log n)` selection, so an ANN index is the larger win.

`engine.TopK` sort has an ordering constraint its marker does not currently state. **It must not be repaid before the cursor interface question is settled** ([FINDINGS §3.1](FINDINGS.md)). With a cursor, early termination replaces bounded selection — a threshold is maintained rather than a k-sized heap — so building the heap first means writing selection logic twice. An ANN index also returns top-k directly, which may take `TopK` off the vector path entirely.

Repay the vector scan in milestone 3; hold `TopK` until after the milestone 5 cursor decision.

### Quality-gated — milestone 4, in two phases

Over-fetching and BM25 parameter tuning both change retrieval depth and scores, and milestone 4's primary job is measuring the graph scorer's contribution. Changing them during that measurement confounds it.

1. **Freeze.** Run the three-arm graph A/B ([DATASETS §3](DATASETS.md)) with RRF `k`, `K1`, `B` and the over-fetch factor at current values. This produces the number the falsification condition for graph proximity depends on.
2. **Then sweep.** RRF `k`, over-fetch factor, `K1`/`B` as a second phase against the same query set.

Freeze first, tune second. Otherwise it is unknowable what moved nDCG.

### Evidence-gated — milestone 5, possibly never

The index `RWMutex` and sequential scorer execution are the two items most likely never to be repaid. weft has a single writer by design, so if the milestone 2 commit model stays single-writer, sharding is never justified. Sequential execution only pays off if one scorer dominates latency — and after the vector scan is replaced, the obvious candidate for that stops being slow.

Do not schedule either. Add the measurement to milestone 5's load test so the evidence appears or does not.

### Action now

None outstanding. The one comment edit this decision called for — `pkg/engine/topk.go` recording the cursor-interface dependency alongside the size trigger, which made it the marker most likely to be repaid at the wrong time — has landed. Everything else stands as written.

Two markers were added afterwards, both scale-gated and both blocking the sequential-execution item above: `scorer/text` and `scorer/graph` take the index-wide read lock once per posting and once per link, so fanning scorers out with goroutines as things stand loses throughput rather than gaining it. Batch the reads first.

---

## D-003 — A commit is a full snapshot; incremental segments wait for milestone 3

**Status:** accepted, 2026-08-13
**Context:** [FORMAT.md](FORMAT.md), [FINDINGS milestone 2 §3.1](FINDINGS.md)

### Question

Milestone 2's outcome is "the index survives restart, one commit makes all
scorers' data visible atomically". Does that require incremental segments —
each commit writing only new documents, queries reading across many segments —
or does one rewritten segment per commit satisfy it?

### Decision

One segment per commit, the whole corpus rewritten, the previous generation
deleted. The MANIFEST nonetheless carries a segment **list** with a generation
number, and version 1 constrains the list to exactly one entry.

### Rationale — the same asymmetry as D-001

Incremental segments drag three problems in with them: multi-segment readers,
per-segment BM25 statistics that must be merged at query time, and DocIDs that
need a namespace the moment two segments are live (FINDINGS §3.4). All three
are milestone 3's problems, and solving them now means solving them against a
guess about scale.

The costs are asymmetric in the familiar direction. Deferring incremental
segments costs O(corpus) per commit — real, but irrelevant at in-memory scale,
and marked with a `ponytail:` comment. Deferring the *manifest layout* would
cost a format migration. So the layout (count + list) lands now, and the
policy (exactly one) is a version-1 writer contract the reader enforces.

A second choice folded in: **Commit refuses a corrupt manifest** instead of
superseding it. Writing a fresh generation over a directory in an unknown
state could orphan a commit the caller believes exists; refusal costs the
caller one explicit decision (repair or start a new directory) and never
costs them data they thought was safe.

### What would show this decision was wrong

Milestone 3 finds the v1 section formats unusable for multi-segment reading —
statistics that cannot merge, offsets that cannot relocate — forcing a version
bump that rewrites more than the manifest. Record here what v1 got wrong.

---

## D-004 — The graph verdict needs two conditions, and they are fixed before the numbers exist

**Status:** accepted, 2026-08-14
**Context:** [EVAL.md](EVAL.md) sections 3 and 4, [DATASETS.md](DATASETS.md)
section 3, D-002 above

### Question

Milestone 4 decides whether the graph scorer survives. Two existing documents
disagree about how to measure it, and neither says what result counts as a pass.

D-002 says freeze every constant, measure, *then* sweep — changing parameters during
the measurement confounds it. DATASETS section 3 requirement 4 says sweep the RRF
rank constant *alongside*, because measuring at a fixed `k` risks measuring `k = 60`
rather than the graph signal. Both are right about the risk they name.

### Decision

**Both, with the pass condition written down in advance.** The verdict on the PRD's
second falsification condition is **yes** only if both hold:

1. **Frozen.** At `RRFk=60`, `K1=1.2`, `B=0.75`, `SeedN=5`, `MaxDepth=3`,
   over-fetch=1, the paired 95% bootstrap interval for `+graph` minus baseline
   excludes zero and is positive.
2. **Stable.** Across the sweep, the sign of that delta does not flip.

Condition 1 holding and 2 failing is **undetermined**, not an improvement. Condition
1 failing is **no**, and `pkg/scorer/graph` is deleted while the `Scorer` interface,
`Query.Seeds` and `recency` stay.

The headline is the frozen configuration alone. The sweep is a separate artifact
reporting how much the verdict depends on a constant nobody tuned.

### Rationale — the rule has to precede the number

Everything before this milestone was measured against a property of our own code:
fusion is invariant to scorer count, a reopened index ranks identically. Those
cannot be argued with. This milestone measures a claim about the world, and the
failure mode is not a bug — it is choosing the interpretation that flatters the
signal after seeing the data. Fixing the rule first is the only defence, and it
costs nothing to write down now.

Two supporting choices are recorded with it:

**No number is published before the instrument is checked against an outside
implementation.** This has already paid. The nDCG gain function was specified as
exponential on the stated grounds that it matched BEIR; `pytrec_eval` shows
`trec_eval` uses linear gain — 0.8597 against 0.7967 on the discriminating fixture.
Publishing on a scale nobody else uses would have made every arm comparison
incomparable. BM25 agrees with `rank_bm25` to 4.44e-16 once the IDF form is
explicitly aligned, which closes the PRD's long-unclaimed "correctness floor" row.

**A limitation that biases toward the signal is stated at the same volume as the
result.** There are no query vectors, so the baseline is text alone rather than
text+vector, and a weaker baseline is easier for the graph arm to beat. A positive
result is therefore an upper bound, and necessary but not sufficient; a negative
result is conclusive. Recorded in EVAL.md section 5.5, not in a footnote.

### Consequence for D-002's marker table: one item retires unpaid

D-002 scheduled over-fetching as quality-gated, to be repaid in milestone 4 phase 2
by giving `engine.Search` a depth parameter. **It needs no parameter.**
`fusion.Fuse` scores a document from its ranks alone and passes `k` only to `TopK`,
so `Fuse(streams, k*m)[:k]` equals `Fuse(streams, k)`, and over-fetching is
`Search(ctx, q, k*m, ...)` truncated by the caller. The equality is asserted across
k ∈ [1,5] and m ∈ {2,3,10}.

The marker at `pkg/engine/search.go:112` is therefore **withdrawn rather than
repaid** — the ceiling it named was reachable from outside all along. The
precondition that makes it true (a fuser must be k-independent in scoring) is now
documented on `eval.Arm.Fuse`, because a fuser that normalised by `k` would break it
silently.

### What would show this decision was wrong

The sweep shows the frozen configuration was unrepresentative — the sign holds only
near `k = 60`, making the frozen headline the outlier rather than the centre. That
would mean freezing first bought a number reading as more solid than it is, and the
honest fix is to report the sweep as the headline with the frozen point marked on
it. Record the outcome here either way.

**Outcome, 2026-08-14:** neither. The sweep found 0 sign flips across 28
configurations, so the frozen point was representative — and the stable sign is
negative. The rule worked as intended and returned "no". One clause did fail, and not
the one this decision hedged against: the pre-registered baseline was briefly moved on
a number that turned out to be a coverage artifact, then moved back. Written up in
[EVAL.md](EVAL.md) section 4.1, because the failure mode — a narrow confidence interval
on incomplete data — is not one this decision anticipated.

---

## D-005 — Keep the graph scorer, weight it down, and spend the milestone's finding on fusion

**Status:** accepted, 2026-08-14
**Context:** [FINDINGS milestone 4](FINDINGS.md), [EVAL.md](EVAL.md) section 6, D-004
above, PRD falsification condition 2

### Question

Milestone 4 answered the PRD's second falsification condition *no*: graph proximity
costs 0.1227 nDCG@10, with the sign stable across 28 configurations. The PRD is
unambiguous about the consequence — keep the interface, discard the graph — and D-004
restated it before the numbers existed. So `pkg/scorer/graph` should be deleted.

Executing that turned up a cost neither document accounted for.

### The complication

`graph` is not a leaf. It is one of the **three** signals the milestone 1 assertions
are built on, and those assertions are the project's central evidence:

| Site | What breaks |
| --- | --- |
| `architecture_test.go` | "three scorers then four" becomes "two then three"; `TestAddingAFourthScorerDoesNotChangeTheCallShape` loses its fourth scorer |
| `TestFourthScorerIsUnderOneHundredLines` | measures `recency`, which becomes the *third* signal |
| `Query.Seeds` | kept per the PRD, but its only consumer is the graph scorer — an interface field nothing reads |
| `restore_test.go` | restore equivalence is asserted across four scorers, including graph traversal over persisted `Links` |
| `Document.Links` | kept (it is in the on-disk format, [FORMAT.md](FORMAT.md)), but nothing would read it |
| `cmd/weft`, `examples/basic` | both demonstrate graph proximity |

So the deletion is not "remove a package". It is "reduce the architecture's
demonstration from four signals to three, and leave two `Document`/`Query` fields with
no reader". The interface survives, which is what the PRD cared about — but the
*evidence* for the interface gets thinner, and that evidence is the project's main
asset.

### Why this is not decided here

Three defensible options, and choosing between them is a scope call rather than a
measurement:

1. **Delete as written.** Honours the falsification condition literally. Costs the
   fourth signal in the milestone 1 assertions.
2. **Delete `graph`, promote a replacement fourth signal** so the assertions keep four
   subjects. Nothing is queued, and inventing a signal to keep a test honest is the
   kind of move this project's documents exist to prevent.
3. **Keep `graph` marked as measured-negative**, with the verdict in its package
   comment, and delete it when a fourth signal exists. Keeps the evidence intact and
   keeps a scorer the project has published as harmful — the option most likely to look
   like ordinary reluctance to delete, which is exactly why it needs stating rather
   than defaulting.

Option 3 is the one the PRD's own risk register warns about: "the verdict is 'no' and
the code does not get deleted" is listed as a Medium risk, mitigated by "put the
deletion in Task 7 explicitly". That mitigation was followed; the deletion is in Task 7
and is being reported as owed rather than done.

### What the weight sweep changed

The three options above were framed while the graph scorer looked actively harmful:
−0.1227 nDCG@10. Testing fusion weights moved that number's owner.
[EVAL.md](EVAL.md) section 5.11: halving the graph stream's weight erases all but
0.0019 of the regression, and no weight makes the signal worth having — the best delta
available is exactly +0.0000, the arm having converged onto the baseline.

So the accurate description is **not** "a scorer that damages rankings" but "a scorer
that contributes nothing, fused by an operator that was amplifying it". Deleting the
scorer would have removed the smaller of the two problems and the more useful of the
two artifacts.

### Decision

**Option 3, on stronger grounds than it was first argued: keep `pkg/scorer/graph`,
mark it, and promote weighted fusion into the library.**

1. **Keep the package.** Its doc comment now opens with the measurement, the
   instruction to weight it down, and a pointer here. A reader cannot enable it
   believing it helps.
2. **`fusion.FuseWeighted` ships**, with per-stream weights indexed by position.
   `Fuse` is unchanged and its unweighted path is bit-identical, so every ranking
   pinned by the milestone 1 and 2 tests is untouched.
3. **The falsification condition is honoured in substance.** The PRD's clause exists
   so a negative result cannot be quietly ignored; here it is published in FINDINGS,
   EVAL, README, the PRD milestone table and the package's own documentation. What
   changed is that the measurement identified a better target than the one the clause
   named.

The uncomfortable part is kept in view: this is still the option that leaves code alive
after a falsification condition fired, and "we found something more interesting" is
exactly the argument a project tells itself when it does not want to delete. Two things
distinguish it from that failure mode — the replacement work is done rather than
promised (`FuseWeighted` is in `pkg/fusion` with tests, not on a roadmap), and the
scorer is marked at the point of use rather than only in a document nobody reads.

### What would show this decision was wrong

`FuseWeighted` acquires no caller outside `internal/eval`, and `scorer/graph` is still
present and still unweighted at milestone 6. That would mean the finding was used as a
reason not to delete rather than as a direction, which is the failure this entry claims
to have avoided. The check is mechanical: grep for `FuseWeighted` outside `pkg/fusion`.

---

## D-006 — Map the segments, so the read API does not grow an error

**Status:** accepted, 2026-08-15
**Context:** [FINDINGS milestone 2 §3.2](FINDINGS.md), [milestone 1 §3.4](FINDINGS.md),
`pkg/engine/mmap_unix.go`

### Question

Milestone 3 has to read a corpus larger than memory. Two things follow and they pull in
opposite directions: reads must not load what they do not use, and the six read methods
the scorers call must keep their signatures — the milestone 1 hypothesis is exactly the
claim that scale costs `pkg/scorer` nothing.

Reading lazily means reading from a file. A file read fails.

### Decision

**`mmap`, and DocIDs stay one dense `uint32` space.**

The decoders already work over a `[]byte`, so a mapped region reaches them with the
parsing, the bounds checks and the verification unchanged. More to the point, an access
to mapped memory cannot fail, so `Doc(id) (Document, bool)` keeps its shape.

`ReadAt` was the alternative and it is the one that costs. Every one of the six read
methods would return an error, all four scorers would handle it, and `engine_api.txt`
would record the widening — which is precisely what that golden file is for. The
measured cost of the choice actually made is three new names: `Scrub`, `Close`, `Merge`.

The DocID half is the same shape of decision. Two live segments could have been
`(segment, local id)`; instead a segment owns `[base, base+count)` and the manifest says
where. `DocID`'s width is in the golden file, ids stay dense, and `TopK`'s tiebreak still
means what it meant.

### What it does not buy, at the same volume

**mmap moves a corpus out of the Go heap and into the page cache. It does not shrink a
working set.** The heap assertion is flat — 74,504 bytes at 250 documents and 74,504 at
2,000, 7.9× apart on disk — and flat says the corpus left the heap, not that it stopped
needing to be resident.

On the milestone 4 corpus roughly 69% of the 656 MB docs file is vectors, and
`scorer/vector` scans every one of them per query. That scan's working set is unchanged
by anything in this milestone. Only an approximate vector index removes it.

### The cost this hides, and where it is visible

`Doc` returns `(Document, bool)`, so a record failing its checksum reports the id as
**absent**. Corruption and "no such document" are one answer at the read API. The
alternative is the error return this decision exists to avoid, so the trade is taken
deliberately and pinned by a test: never a wrong document, never a panic, neighbouring
documents untouched, and `Scrub` names the damage.

### What would show this decision was wrong

A scorer needs to tell absence from damage. If that happens the honest fix is the error
return — and the diff it forces across the scorers is the number this decision claimed
to be avoiding, so record it here rather than adding a second, quieter channel.

---

## D-007 — Format v2 refuses version 1, and this argument works exactly once

**Status:** accepted, 2026-08-15
**Context:** [FORMAT.md](FORMAT.md), D-003 above, `pkg/engine/segment.go`

### Question

Version 1 wrote documents as a bare run of variable-length records and rebuilt `byKey` by
reading all of them. Neither a `DocID` nor a `Key` could reach its document without
decoding every document in front of it — no arrangement of a lazy reader fixes that, only
different bytes do. So the format changes. Does the reader migrate v1 or refuse it?

### Decision

**Refuse, with `ErrBadVersion`.** One reason, and it is not a technical one: **weft has no
users, and no version 1 index exists anywhere that cannot be rebuilt.** The evaluation
directory was the only one, and `weft-eval build` regenerated it from sources still on
disk.

FORMAT.md §7.5 says a migration reads the old version and writes the new one. This
milestone is exempt, and **the exemption is not available again**: it rests on a user
count, and a user count only goes up.

### What v2 adds, and why each is a format change rather than a code change

| Section | What it makes possible |
| --- | --- |
| `docoff` | `DocID` → the record's offset, with the token count beside it. Fixed width, so entry *i* is arithmetic. BM25 asks for a length once per posting, and a length reachable only by decoding the record would make every posting cost a key, a text and a vector. |
| `keys` | sorted `Key` → `DocID`, binary-searchable, so `Resolve` is not a map rebuilt by reading the corpus. |
| per-unit checksums | one record, block or entry at a time. The frame checksum covers a whole file, so computing it costs a full read — the cost being removed. |

The manifest's contents changed and its *shape* did not: D-003 put a segment list there
for this. Entries now carry `(name, base, count)`.

The checksums are **seeded**, and the seed is the part worth recording. A document record
never carried its own DocID — position named it — so a lazy reader following a damaged
offset table would decode a healthy record under someone else's id and return a plausible
wrong answer. Binding the id in makes the record prove which document it is.

### D-003's retrospective: what v1 got wrong

D-003 asked what would show it wrong: "milestone 3 finds the v1 section formats unusable
for multi-segment reading, forcing a version bump that rewrites more than the manifest."

**That is what happened, and not for the reason it expected.** Multi-segment reading was
fine — the manifest was a list, as designed, and incremental commit extends its contents
rather than its shape. What v1 could not do was let a *single* segment be read lazily: the
`docs` section was positional and `byKey` was derived, so both had to be reconstructed in
full. D-003 was watching the seam between segments. The problem was inside one.

### What would show this decision was wrong

Somebody turns up holding a v1 index they cannot rebuild. Then refusing was the wrong
call, and the fix is a converter shipped separately rather than a reader carrying two
formats. Record it here if it happens.

---

## D-008 — The engine knows the geometry, the scorer keeps the metric; v3 pays its debt with a reader

**Status:** accepted, 2026-08-17
**Context:** [FORMAT.md](FORMAT.md) §4 and §7.7, [FINDINGS milestone 3b](FINDINGS.md),
D-006 and D-007 above, `pkg/engine/ivf.go`, `pkg/scorer/vector/vector.go`

### Question

Milestone 3a mapped the index and left half its outcome sentence false: `scorer/vector`
scanned every document on every query, so 434 MiB of vectors moved from the Go heap into
the page cache without getting smaller. Removing the scan means an approximate index, and
that raises two questions at once.

**Where does the approximate index live**, given that milestone 1's hypothesis is that a
scorer needs no private store? And **what does it return** — candidates, or scores?

Separately: FORMAT.md §7.6 obliged a version 3 to bring either a converter or a reader
that understands both versions, because D-007's "refuse rather than migrate" argument was
spent on v1. Which?

### Decision

**The partition lives in `pkg/engine`, and `Index.Nearest` returns `[]DocID` and no
score.** The engine knows *which documents are close enough to be worth looking at*; the
scorer knows *how close each one is*.

**Format v3 brings the reader.** v2 segments open, report no partition, and answer with
every id they hold.

### Why the partition is engine's

The hypothesis milestone 1 registered is that a scorer does not need its own copy of the
corpus. The partition is not a copy of the corpus and it is not a scorer's private
structure — it is **a section of the segment format**: written by the writer, mapped by
the reader, framed and checksummed like every other section, walked by `Scrub`. Putting it
in `scorer/vector` would have given that scorer the private store the hypothesis forbids,
and would have meant a second writer for a directory with one.

### Why it returns no score

Because the alternative moves half a scorer into the engine to save it a loop.
`scorer/vector` holds rules that are about cosine and about nothing else: a zero-norm
document has no direction, a non-finite query is an error rather than an empty result, a
document of the wrong width is `ErrDimMismatch` and not a skip, and the scan polls its
context every 1024 components. Returning `[]Candidate` would have moved all four across
the boundary, and then the engine would define what "similar" means for every caller.

**The measurement that says the line is in the right place is the diff on the far side of
it: seven lines**, four removed and three added, all in the loop header. Every one of
`scorer/vector`'s twelve contract tests passed unmodified. Had the line been drawn wrong,
those rules would have had to move and the diff would have said so.

Two consequences are accepted rather than hidden. `Nearest` returns a **superset** —
documents with no vector, with a zero vector, or in a segment with no partition are all in
it — so the scorer's skips still do work. And a query of the wrong width gets **every** id
rather than none, because narrowing there would turn "you mixed embedding models" into a
thin result instead of an error.

### Why the reader rather than a converter

Because it was nearly free, and a converter was not. A segment without a partition has to
be readable whatever happens: the pending segment has none, a segment below 16,384
documents has none, and a partition that fails its checksum is treated as none (D-006). So
"a v2 segment is a segment with no partition" reuses a branch that already had to exist,
where a converter would have been a command to write, a rebuild to run, and a directory in
two states while it ran.

`Merge` then does the conversion as a side effect: it rewrites the run it collapses with
the current writer, so a v2 generation becomes v3 through maintenance an index already
performs. The rule this generalizes to is in FORMAT.md §7.7 — **append a section rather
than changing one, and the old version stays readable by construction**. A version that
retypes or reorders an existing field gets no such discount and still owes the converter.

### The cost, measured

`nprobe` had to be raised from the plan's proposed 8 to **64** to hold the quality bar that
was fixed before any of this was built: `text+vector` 0.6211 against a 0.6233 baseline,
inside the registered 0.005. recall@10 against a brute-force scan is 0.992, a query is
4.6× faster, and it touches 210 MiB of a 626 MiB `docs` section where the plan predicted
12 MiB. [FINDINGS milestone 3b §3](FINDINGS.md) has why the prediction missed and what
would actually fix it.

### What would show this decision was wrong

Three things, and each has a number attached.

1. **A scorer needs the candidates in rank order, or needs the centroid distances.** Then
   `Nearest` widens to `[]Candidate`, half a scorer is inside the engine, and the size of
   that diff is the honest price of this record. Nothing needs it today.
2. **A second metric arrives** — dot product on unnormalized vectors, say — and finds the
   partition unusable because it was trained on L2-normalized vectors. Spherical k-means is
   a commitment to cosine, and it is made here rather than in the scorer that uses it.
3. **Somebody has to read a v2 index this build cannot open.** The reader is only cheap
   while the two versions differ by an appended section; the moment a v4 changes a field,
   this record stops being precedent.

---

## D-009 — The load is open-loop, and bleve lives in a submodule

**Date:** 2026-08-18
**Milestone:** 5 — performance
**Status:** accepted

### Context

Milestone 5's outcome sentence asks for two things that each have a trap in them.

"GC pause를 포함한 p99가 공개되고" needs a load generator, and the obvious load
generator is wrong. "기성 엔진과 같은 자릿수임을 보인다" needs bleve, and the PRD's own
success metrics forbid bleve: *운영 — 의존성: 표준 라이브러리만. 외부 의존성 0개 유지 —
`go list -m all` 이 자기 모듈만 출력*.

### Decision

**The driver sends on a clock, not on a completion.** Request *i* is due at
`start + i/rate` whatever request *i-1* is doing, and its latency is measured from that
due time. When the in-flight cap is reached the request is **shed and counted**, never
waited on.

**bleve lives in `bench/`, a separate Go module**, and both harnesses import one shared
driver from `internal/loadgen` through a `replace` directive.

### Why

**On the loop.** A closed-loop driver — send the next when the last returns — lets a
stalled server receive less load. The stall then appears as one slow request, because
every request that would have arrived during it was never sent, and the p99 that comes
out is a p99 of a load the server chose for itself. That is coordinated omission, and a
milestone whose entire deliverable is a p99 cannot be measured by the instrument that
hides it. Shedding rather than blocking is the same argument one level down: a driver
that waited for a free slot would be waiting on the server again, at exactly the load
where the bias matters most. `TestOpenLoopDoesNotLetTheServerSlowTheLoad` is the
assertion, and it distinguishes the two designs by the *count* of slow samples — one for
a closed loop, many for an open one.

**On the submodule.** The alternative was to quote a figure bleve's own documentation
publishes, and that is not a comparison: different machine, different corpus, different
query set, different rank cut. Any of those alone can move a latency by more than the
order of magnitude the rule is testing. A nested module gets both properties at once,
because the Go tool's module graph and the working tree are different things:
`GOWORK=off go list -m all` at the root still prints one line and `go build ./...` never
descends, while `gofmt -l .` and `git ls-files '*.go'` still do. Measured after adding
bleve v2.6.0 and its roughly twenty transitive modules: `make deps` prints
`github.com/skyoo2003/weft` and nothing else, `make arch` is green, `make spdx` is green.

**On sharing the driver.** The rule being tested is a ratio. A bias present in one
implementation of an open loop and absent from the other moves that ratio without
moving either engine, so the two harnesses are two `main` packages over one
`internal/loadgen`. That is also why `internal/` and not `pkg/`: the driver is a
measurement tool, not part of what weft offers an embedder, and putting it in `pkg/`
would add it to the public API golden and to the CHANGELOG's promises.

### What it costs

**The hybrid arm is not compared.** bleve's kNN is behind a `vectors` build tag and needs
cgo and a faiss shared library. Taking that on changes the subject — "a Go search engine"
becomes "a Go wrapper around faiss" — so the comparison covers `text` only. The arm a
user would actually deploy is `text+vector`, and this decision means the milestone cannot
speak to it. Stated in `bench/README.md` and [PERF.md](PERF.md) §4 rather than discovered
by a reader later.

**The analyzers do not match.** bleve's `standard` analyzer stems and drops stop words;
`engine.Tokenize` does neither. The effects run in opposite directions and neither is
plausibly worth 10×, which is all rule 2 asks — but the comparison is not
analyzer-matched and no reading of it should assume otherwise.

**A submodule is a second place to keep green.** It has its own `go.mod` and its own
lockstep with the driver's exported names. CI builds it; CI does not run it.

**Neither `bench` target is in `all` or in CI.** A shared runner's tail latency is a
function of whatever else is on the machine, so gating a merge on a p99 measured there
makes the gate a coin flip. The numbers are produced by a person on a quiet machine and
published in [PERF.md](PERF.md).

### What would show this decision was wrong

1. **`bench/` acquires a reason to be imported by the main module.** Then the quarantine
   is load-bearing in the wrong direction and the dependency metric has to be renegotiated
   rather than worked around. `make deps` is what would catch the drift.
2. **The open loop turns out to be measuring the driver.** If the ladder's upper rungs
   report `shed` counts large enough that the distribution is mostly of requests that were
   never sent, the cap is the instrument's limit rather than the server's, and the fix is
   a driver process separate from the server process — which is a rewrite, not a tweak.
3. **Somebody needs the hybrid comparison.** Then faiss enters `bench/`, the "Go engine
   against Go engine" framing goes with it, and this record stops being precedent for what
   the comparison means.

---

## D-010 — Adoption is decided by a trial, and the extension point is not designed before it

**Date:** 2026-08-19
**Milestone:** 6 — adoption
**Status:** accepted

### Context

Milestone 6's outcome is a claim about readers — *an external Go developer can add
their own signal from the documentation and examples alone* — and claims about
readers have a failure mode the other milestones did not. There is no metric to
compute. The tempting substitute is to look at the API, decide it seems adequate,
and ship a paragraph.

Two facts made that substitute unsafe. `engine.Document` and `engine.Query` are
both closed structs, so a signal carrying data weft does not model has no field to
live in — which reads, from inside the repository, like a missing feature. And
`pkg/engine/doc.go` said "adding a fifth scorer means adding a field here", which
is the maintainer's own procedure written as if it were everyone's.

### Question

Do we design an extension point — a `Document.Meta` map, a `Query` payload — or do
we first measure whether one is needed?

### Decision

**Measure first, and forbid production changes inside the milestone.**

1. The instrument is a trial: a subject with no prior sight of the tree implements
   a fifth signal with only `.md` files, `examples/` and `go doc` output, and every
   point at which it is blocked is recorded. The rules, the boundary and the five
   pass lines are [ADOPTION.md](ADOPTION.md), committed before the trial ran.
2. A blocker is **docs-closable** or **code-required**, decided by attempting the
   API arrangement rather than by how hard it felt. Code-required blockers are
   named and costed, **not fixed here**.
3. `pkg/` changes default to zero, and any diff is the milestone's price tag.

### Why

An extension point touches the on-disk format and `Commit`'s atomicity at once, so
designing one is milestone-sized work. Doing it speculatively inside an adoption
milestone would have spent that budget on a problem nobody had demonstrated —
and, as it turned out, on a problem that does not exist. Both subjects found the
caller-held-table pattern unaided. **What was missing was three sentences.**

This is the same rule [D-002](#d-002--deliberate-shortcuts-are-repaid-on-evidence-not-on-schedule)
applies to performance, moved to documentation: fix what a measurement pointed at,
and let the diff be the receipt.

The cost is real and worth naming. A trial run by an agent is a lower bound, not a
user study, and this decision accepts a weaker instrument in exchange for one that
exists. The alternative on offer was not a better measurement; it was no
measurement and a designed feature.

### What would show this decision was wrong

An external user files an issue that a signal cannot be expressed at all — not
"undocumented", but genuinely unrepresentable through `Resolve` and a caller-held
table. That would mean the trial's two tasks were too narrow to find the class of
signal that needs an extension point, and that the tasks were chosen for what was
easy to measure. The check is not mechanical; it arrives as a bug report.

A weaker signal, and mechanical: `ExampleScorer` and the three paragraphs added to
`doc.go` and `search.go` never change again while the same three questions keep
being asked. That would mean the repair was aimed at the trial rather than at
readers.

---

## D-011 — A repetition is a rung, not a ladder, and the arm nobody can afford to ladder gets a staged depth

**Date:** 2026-08-20
**Milestone:** 7 — a baseline nobody has to qualify
**Status:** accepted, **registered before the campaign measured anything**

### Context

[PERF.md](PERF.md) §5 has said "the headline is the median of three repetitions with
the spread reported beside it" since milestone 5 was planned. Milestone 5 published
one run ([FINDINGS](FINDINGS.md) milestone 5 §4.5). It also published no tail at all
for `text+vector` — the arm a user would actually deploy — because at four times the
per-query cost, ten thousand samples is over five hours (§4.4).

Neither is a rule that was wrong. Both are rules that were never made operable, and a
rule with no procedure is a rule right up until the first time it is inconvenient.

Milestone 8's pass line is an absolute figure at a named load. It is measured against
this baseline. If the baseline is one observation of unknown spread, every claim built
on it inherits that.

### Question

What, exactly, is a repetition — and how does an arm that cannot be laddered three
times get a publishable tail without lowering the bar that makes a tail worth reading?

### Decision

**A repetition is the same rung measured again, not the ladder swept again.**

Repetition 1 sweeps with `-rate 0` and rule 1 selects the headline rate R.
Repetitions 2 and 3 run `-rate R`. The published figure is their median, with the
minimum and maximum beside it.

**And sample depth is staged for `text+vector` rather than the quantile rule
relaxed.** A thin ladder (`-rotations 40`, 2,000 samples per rung) selects the load
point, because a p50 needs 200 samples and rule 1 reads p50s. A deep rung
(`-rotations 200`) at that rate produces the p99.

Both are written as rules 3 and 4 in [PERF.md](PERF.md) §3, with the commands in
§5.1, and this file is committed alongside them — before any figure they govern
exists.

### Why

**Three sweeps have no common rung.** Every rate on the ladder is `benchUnloaded`
scaled by `loadgen.Ladder`, and `benchUnloaded` is 200 sequential requests taken
fresh at the start of each run. Three sweeps produce three different sets of five
rates. "The 100% rung" in two of them is two different loads, and a median over them
is a median over a quantity that changed between observations. This is not a
refinement of the median-of-three rule; it is the only reading of it that computes.

The cost is named: repetitions 2 and 3 do not re-derive R, so they cannot detect that
the machine's sequential throughput moved. That is why each run's unloaded p50 is
recorded beside its p99 — the drift is then visible as data rather than absorbed into
the spread. R is also quoted to two decimals, so repetitions 2 and 3 run about 0.04%
off repetition 1's actual rung.

**Relaxing `Printable` was the alternative, and it was refused.** Printing a p99 off
two thousand samples would have given `text+vector` a tail immediately. It would also
have made every published quantile in this repository mean something different from
what [PERF.md](PERF.md) §2.3 says it means, to buy one number. Staging the depth costs
an extra run and changes no rule. The honest cost of staging is that selection and
measurement happen at different depths, so a thin ladder could in principle select a
different rung than a deep one would — registered as a finding to publish if it
happens, not as an error to hide.

**Registered before, not written after.** [D-004](#d-004--the-graph-verdict-needs-two-conditions-and-they-are-fixed-before-the-numbers-exist)
fixed milestone 4's verdict conditions before its numbers existed and
[D-010](#d-010--adoption-is-decided-by-a-trial-and-the-extension-point-is-not-designed-before-it)
committed `ADOPTION.md` before the trial ran. A decision record written after the
campaign would be a description of what was done, and the thing that makes these
rules worth anything is that they were not available to be chosen once the numbers
were on screen. Rule 5 in particular — *the median becomes the published figure, and
if the worst observation reaches the 10× bar the verdict says so* — decides in
advance how to report a result nobody wants.

### What would show this decision was wrong

Two signals, both mechanical:

**The three observations agree to within noise, run after run, across milestones.**
Then the repetition campaign is 3.1 hours of `text` arm time buying a spread that was
never in doubt, and rule 3 should collapse back to one run with the spread quoted from
history. Milestone 7's own §4 is where that first becomes checkable — if nothing the
three runs say changes any verdict, that is the finding, and it gets published as one
rather than quietly justifying the next campaign.

**The thin ladder selects a different rung than the deep one.** Then rule 4's
staging is not a cost-saving on one arm, it is a claim that sample depth does not move
rule 1 — and that claim would be false. The repair is not to widen the thin ladder but
to say so in `text+vector`'s published figure, because the same doubt then applies to
every headline rule 1 has ever selected.

---

## D-012 — D-011's premise is false; mark the rule, do not replace it from inside the campaign that broke it

**Date:** 2026-08-21
**Milestone:** 7 — a baseline nobody has to qualify
**Status:** accepted
**Context:** [FINDINGS milestone 7](FINDINGS.md), [D-011](#d-011--a-repetition-is-a-rung-not-a-ladder-and-the-arm-nobody-can-afford-to-ladder-gets-a-staged-depth)

### Context

[D-011](#d-011--a-repetition-is-a-rung-not-a-ladder-and-the-arm-nobody-can-afford-to-ladder-gets-a-staged-depth)
decided, one day before the campaign ran, that a repetition is the same rung measured
again rather than the ladder swept again. The argument was arithmetic and still holds:
every rung's rate derives from a fresh `benchUnloaded`, so three sweeps give three
different sets of rates and there is nothing common to take a median of.

The campaign then measured 25.67 q/s three times. One observation shed nothing and
held a 37.9 ms median at 114 MiB. Two collapsed — 1.539 s and 416 ms, 14% and 11% of
the load shed, 1021 MiB and 765 MiB resident. Same corpus, same binary, same machine,
same day, no suspension in any of them.

The difference that survives every check in [FINDINGS §2](FINDINGS.md) is that the
flat observation was the fourth rung of a ladder and the two collapses were single
rungs out of a warm-up. **A rung measured alone is not the rung D-011 thought it was
repeating.**

### Question

Rule 3 is falsified. Do we replace it now — three full sweeps, a pinned-rate ladder
flag, a fixed prefix — or mark it and stop?

### Decision

**Mark it. Publish the falsification. Do not choose a replacement inside the
milestone whose numbers produced it.**

1. [PERF.md](PERF.md) §3 rule 3 stays on the page, with what falsified it named
   beside it. It is not edited into something that would have worked.
2. "What must a repetition hold constant" becomes an open question against
   milestone 8, carried in [FINDINGS §6](FINDINGS.md).
3. No fourth run. [PERF.md](PERF.md) §3 rule 5 clause 4 already fixed that answer
   for a spread this rule could not survive, and a 40× spread is past any reading of
   it.

### Why

The discipline this repository runs on is that a rule is worth something only if it
was not available to be chosen once the numbers were on screen. That constraint does
not lift when the rule turns out to be wrong — it binds hardest exactly then, because
the replacement would be picked by someone who has just seen which shapes produce
which answers. Three candidate repairs are already visible from here, and the reason
to prefer one of them over another is currently *which run it would have made look
reproducible*.

Marking costs a milestone's headline. [Milestone 7](FINDINGS.md) closes with no
median and no spread, which is a worse artifact than the one it set out to produce
and a better one than a median assembled from a rule known to be measuring two
different things.

There is also a positive result to protect. The campaign produced an instrument that
refuses to publish what it did not measure — a suspended ladder, a ladder cut short,
an operator-chosen rate wearing a rule's label. Those are assertions now, not
comments. Rewriting rule 3 in the same breath would put the milestone's one solid
output next to a rule chosen against its own evidence.

The cost is named: milestone 8 inherits an unanswered procedural question on top of
its engineering one, and its own pass line — *shed 0 at 27.28 q/s* — is not a
predicate until it is answered, since [FINDINGS §4.5](FINDINGS.md) shows one rate
both passing and failing. That is worse for milestone 8's schedule and better for
whatever it eventually claims.

### What would show this decision was wrong

**The ladder prefix turns out not to be the variable.** §3's reading is a hypothesis
with a named alternative — that `inflight` 40 admits a start-of-rung burst a process
arriving from a lower rung never sees. If the prefix is ruled out, then rule 3 was
falsified by something it could have been written to control, and marking it rather
than fixing it will have cost a milestone for nothing. The experiment is cheap and it
belongs to milestone 8: run the same rate behind two different prefixes and behind two
`inflight` caps.

**Nobody returns to the question.** A rule marked as falsified and left standing is
one nobody has to argue with. If milestone 8 publishes a performance figure without
first answering what a repetition holds constant, this decision will have converted a
wrong rule into no rule, which is the outcome it was trying to avoid.

## D-013 — A repetition is the ladder, named rather than derived, and it is published without the label

**Date:** 2026-08-22
**Milestone:** 8 — the throughput wall
**Status:** accepted
**Context:** [FINDINGS milestone 8](FINDINGS.md), [PERF.md §5.2](PERF.md),
[D-011](#d-011--a-repetition-is-a-rung-not-a-ladder-and-the-arm-nobody-can-afford-to-ladder-gets-a-staged-depth),
[D-012](#d-012--d-011s-premise-is-false-mark-the-rule-do-not-replace-it-from-inside-the-campaign-that-broke-it)

### Context

[D-012](#d-012--d-011s-premise-is-false-mark-the-rule-do-not-replace-it-from-inside-the-campaign-that-broke-it)
refused to repair rule 3 from inside the campaign that falsified it, and named the
experiment that would license a repair: the same rate behind two prefixes and two
`inflight` caps. [PERF.md](PERF.md) §5.2 registered that experiment, with what each of
four outcomes would license, before any of it ran.

Outcome 1 fired. 25.67 q/s reached as the fourth rung of a ladder whose earlier rungs
ran 10,000 samples each gave p50 **37.827 ms** with **shed 0**, against milestone 7's
37.852 ms and shed 0 — 0.07% apart, with the collector's cycle count 0.12% apart. The
same rate with no prefix collapsed at both `inflight` values. A prefix of the same
shape at a fifth of the depth collapsed hardest of all.

### Question

Rule 3 needs an operable form. Is a repetition a rung, a ladder, or something else —
and if it is a ladder, what happens to rule 1's refusal to label one?

### Decision

**A repetition is the same ladder, with its rates named rather than derived.**

1. Repetition 1 is `-rate 0`. The sweep derives the rates and rule 1 selects the
   headline rate **R** from them.
2. Repetitions 2 and 3 are `-rates <repetition 1's rungs, through R>` — the same
   prefix, the same rates, named so that they are shared rather than re-derived.
3. The published figure is the median of the three at R, with the spread reported as
   minimum and maximum beside it. Each repetition's own unloaded p50 is recorded, as
   rule 3 already required.
4. **Rule 1 does not change.** Repetitions 2 and 3 print no headline label, because R
   was selected once — by the sweep — and is being reused. This is what rule 3 already
   said about its single-rung repetitions, and it survives the change of what a
   repetition is.
5. `text+vector` gets **one** named ladder rather than three. That is not a new
   decision: it is [PERF.md](PERF.md) §3 rule 6's first cut, applied to a budget that
   grew.

[D-011](#d-011--a-repetition-is-a-rung-not-a-ladder-and-the-arm-nobody-can-afford-to-ladder-gets-a-staged-depth)
is superseded on its central claim and kept on its arithmetic. D-012's marking of rule 3
is discharged.

### Why

D-011's argument was never wrong about *derivation*: three sweeps take three fresh
`benchUnloaded` readings, so their rungs are three different loads and nothing common
survives to take a median of. What it did was conclude from that that the ladder cannot
be the unit — when the actual consequence is only that the rates cannot be *derived*
twice. Naming them removes the whole difficulty, and `-rates` is that instrument.

The reason to accept the cost rather than look for a cheaper unit is that the cheaper
unit is the one that failed. A single rung is 6.5 minutes and gives 37.9 ms or 1.539 s
depending on nothing the report records. A named ladder is 97 minutes and has now given
the same number twice, rung for rung.

Choosing this repair now is legitimate for exactly the reason choosing it in milestone 7
would not have been: the outcome that licenses it was written down before the run that
produced it, and the three candidate repairs D-012 could see were not ranked by which
run they would flatter.

### What would show this decision was wrong

**A third named ladder does not reproduce the first two.** One reproduction is one. The
procedure this decision installs is exactly the thing that would find that out, and if
it does, a repetition is not a ladder either and the honest position reverts to
milestone 7's — that this workload has no reproducible load point on this host.

**The deep prefix collapses at `inflight` 10.** §5.2's registered outcome 1 says "at
both `inflight` values" and only one was run ([FINDINGS milestone 8 §5.1](FINDINGS.md)).
If the other one collapses, prefix depth is necessary and not sufficient, and this
decision is resting on half a condition.

**The prefix requirement does not travel.** If it turns out to be a property of this
corpus on this host, a procedure defined by it produces figures that are reproducible
and local, which is a smaller claim than the one rule 3 exists to support.

**Nobody pays the 4.9 hours.** Three named ladders per published headline is six times
D-011's budget. If the project quietly reverts to single runs while this decision stands
on the page, it will have converted a correct rule into no rule — which is the failure
mode [D-012](#d-012--d-011s-premise-is-false-mark-the-rule-do-not-replace-it-from-inside-the-campaign-that-broke-it)
named for itself and did not escape by being right.

## D-014 — The memory pass line reads the process's mark, milestone 8 misses it, and milestone 10 does not fire on that

**Date:** 2026-08-22
**Milestone:** 8 — the throughput wall
**Status:** accepted
**Context:** [FINDINGS milestone 8 §7–§8](FINDINGS.md), [PERF.md §2.7](PERF.md),
[D-012](#d-012--d-011s-premise-is-false-mark-the-rule-do-not-replace-it-from-inside-the-campaign-that-broke-it)

### Context

Milestone 8's pass line is *shed 0, RSS ≤ 250 MiB, p50 ≤ 100 ms at 27.28 q/s*. Two of
the three were met on the ladder that judged it. The third could not be read: `ru_maxrss`
is a high-water mark with no reset and no during-this-rung value, so the 345.2 MiB printed
at the 27.28 q/s rung is the mark **13.64 q/s** set two rungs earlier, and the rung under
test added nothing to it.

[FINDINGS §8](FINDINGS.md) published two readings and chose neither, because the reason to
prefer one at that moment was which verdict it produced.

### Question

Which reading does the memory clause mean — the rung's own peak, or the process's? And
does the answer fire milestone 10?

### Decision

**1. The clause reads the process's mark over the ladder up to and including the load
point.** Not the rung's own peak.

**2. Under that reading milestone 8 misses it: 345.2 MiB against 250 MiB.** Recorded as a
miss, with what it is charged with — the mark was set at half the load point's rate, and
on this platform nothing can separate them (below).

**3. Milestone 10 does not fire.** Its trigger is a miss *after* the milestone's
engineering, and milestone 8 has done none. The miss is the first target that engineering
has, not the verdict on having tried.

### Why

**The metric exists for an adopter's memory budget, and an adopter runs a process, not a
rung.** The PRD put it there because what an adopter meets first is not architectural
openness, it is 853 MiB and 12.5 seconds. Anyone sizing a container from a steady-state
figure and ignoring the ramp gets killed during the ramp. The operationally meaningful
number is the high-water mark of everything the process did, which is what `ru_maxrss`
reports and what reading it this way asks for.

It is also the reading the project has always used. Milestone 5 published "RSS
126→853 MiB" as a ladder progression of process marks. Choosing it now is continuity, not
a new interpretation — and the reading that is *not* continuous is the one that would have
made this ladder undecidable rather than a miss.

The direction it errs is worth stating: a ladder touches more load points than a steady
server at any one of them, so its peak is an **upper bound** on a server held at any rate
in it. A pass line that errs toward demanding less memory than the measurement shows is
the safe direction for the person the metric is for.

**On milestone 10 not firing.** The PRD's falsification clause reads "if milestone 8
cannot clear the absolute pass line". The reading that makes that a trigger rather than a
starting gun is *cannot clear it having tried* — milestone 10 is a redesign justified by
candidate-materialisation fixes having proved insufficient, and none have been attempted.
What this campaign produced is the opposite of exhaustion: a specific, measured, localised
target — 345.2 MiB set at 13.64 q/s, against a cause the PRD already names, 30,549
candidates decoded per query. Firing a redesign against an untried target would spend the
conditional milestone on the wrong evidence.

**What is not being done, and why.** A per-rung RSS would make the other reading decidable
and is declined here: on Darwin it needs `task_info` through cgo or `golang.org/x/sys`, and
the first breaks the build's shape while the second breaks `go list -m all` being one line
— a pass line of its own since milestone 1. Linux would take `/proc/self/statm` and
nothing else, so the instrument would answer on one platform and not the one the figures
are measured on. Giving each rung its own process is ruled out separately: it destroys the
ladder prefix [D-013](#d-013--a-repetition-is-the-ladder-named-rather-than-derived-and-it-is-published-without-the-label)
established as the thing a repetition must hold.

### What would show this decision was wrong

**The peak is not candidate materialisation.** The whole reason to keep milestone 8 alive
is that its target is named and believed reachable. A profile showing the 345.2 MiB is
mapped index pages the process cannot avoid touching would mean there is nothing for
candidate-level work to cut, and milestone 10's trigger becomes live after all.

**Reducing the peak costs nDCG past the registered tolerance** (−0.005, from milestone
3b). Then the throughput target and the accuracy invariant are in conflict, which is a
larger finding than either and is not what this decision assumes.

**The ladder-wide reading turns out to hide the load point.** If a per-rung instrument
ever exists and shows 27.28 q/s sitting comfortably under 250 MiB, the miss recorded here
was a property of the ramp rather than of the load — still a real number an adopter pays,
and no longer a statement about the rate the pass line names. The verdict would stand and
its interpretation would narrow, which is a reason to keep the two apart on the page
rather than to relabel the miss.

**Nobody attempts the engineering.** A miss recorded as "the target for work not yet done"
is worth exactly as much as the work. If milestone 8 closes without an attempt at the
345.2 MiB, this decision will have functioned as a way to avoid firing milestone 10 rather
than as a reason not to — which is the failure mode
[D-012](#d-012--d-011s-premise-is-false-mark-the-rule-do-not-replace-it-from-inside-the-campaign-that-broke-it)
named for itself.
