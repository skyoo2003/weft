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

## D-015 — One line of exported surface, rather than a Document whose lifetime quietly changed

**Date:** 2026-08-22
**Milestone:** 8 — the throughput wall
**Status:** accepted
**Context:** [FINDINGS milestone 8 §9](FINDINGS.md), [D-014](#d-014--the-memory-pass-line-reads-the-processs-mark-milestone-8-misses-it-and-milestone-10-does-not-fire-on-that)

### Context

`Index.Doc` decodes a whole record — key, text, links, vector, time — and the vector
scorer reads one field of it. Measured on a synthetic corpus, scoring 64 documents
allocated 4,208,024 bytes against 13,312 for the same vectors with 4 MiB less text: one
full copy of the document text per candidate scored, for a field nothing reads.

The PRD registered **golden API files byte-identical** as an invariant for this round,
against sacrificing the architecture for performance.

### Question

Three routes cut the copy and each one moves something registered. Which?

1. **An additive read accessor** — clean and safe; the golden file gains a line.
2. **A zero-copy decode** — `Document.Text` and `.Key` alias the mapping. Signature
   byte-identical, so the invariant passes as written.
3. **Fewer candidates** — narrow `Nearest`. No API change at all; costs recall, and the
   trade is one of the PRD's own untested open questions.

### Decision

**Route 1.** `Index.Vector(DocID) ([]float32, bool)` is added, and the golden API file
gains exactly one line. `pkg/fusion` stays at zero, `public_api.txt` does not move,
`go list -m all` stays one line, the `Scorer` interface and every existing signature are
untouched.

### Why

**The invariant's purpose is narrower than its wording.** It exists so that performance
work cannot quietly cost the architecture — a fuser that learns signal types, a `Scorer`
that grows a method, a dependency. An additive read accessor touches none of those, and
the project has added exported methods in three prior milestones (`Scrub`, `Close`,
`Merge`, `Nearest`) as ordinary changelog entries. Spending the invariant here is spending
it on the thing it was not written to protect.

**Route 2 is worse for looking better.** It keeps the file byte-identical while changing
what a `Document` *means*: a value held past `Close()` would point into an unmapped range,
so the same signature would carry a new lifetime rule and the failure mode is a segfault
in a caller's process. Nothing in the engine hands out mapped memory today. An invariant
that a change can satisfy by making the same API more dangerous is measuring the wrong
thing, and passing it that way would be the more dishonest of the two.

**Route 3 is not this decision's to take.** It trades recall — measured at 0.992 in
milestone 3b — for memory, against an nDCG tolerance of −0.005, and sizing it needs a
measurement campaign rather than a code change. It stays available and unspent.

**The decision is the operator's, taken explicitly.** The three routes and their costs
were put to them before any of the three was written, because picking the one that
unblocks the work is precisely what
[D-012](#d-012--d-011s-premise-is-false-mark-the-rule-do-not-replace-it-from-inside-the-campaign-that-broke-it)
refuses.

### What would show this decision was wrong

**The saving never gets measured.** It is a bounded property, not a number: the arm it
helps has no published memory figure ([FINDINGS milestone 8 §9](FINDINGS.md)), so the line
of exported surface has been spent against a synthetic benchmark. If the `text+vector` arm
is never laddered, this bought a guarantee and no measurement.

**One accessor becomes four.** `recency` reads `Time`, `graph` reads `Links`, and both call
`Doc` for it. If each gets its own accessor the exported surface grows by a field-shaped
method per scorer, which is the closed-`Document` design turned inside out — and that
design is why `Resolve` exists at all, as its own doc comment argues: a signal whose data
is not one of `Document`'s fields keeps that data in a table of the caller's own. A second
accessor should have to argue harder than this one did.

**The format drifts between the two modes.** `decodeDocFields` is one function so that the
layout is written down once, and `assertReadAPIsAgree` checks that `Vector` and `Doc`
answer the same on both sides of a commit. If a future field is added to one path only,
those are the two things that were supposed to catch it.

## D-016 — A buffer, not an iterator, and the second guess about the peak was measured out

**Date:** 2026-08-23
**Milestone:** 8 — the throughput wall
**Status:** accepted
**Context:** [FINDINGS milestone 8 §10](FINDINGS.md), [D-014](#d-014--the-memory-pass-line-reads-the-processs-mark-milestone-8-misses-it-and-milestone-10-does-not-fire-on-that), [D-015](#d-015--one-line-of-exported-surface-rather-than-a-document-whose-lifetime-quietly-changed)

### Context

D-014 recorded milestone 8's memory clause as a miss — 345.2 MiB against 250 — and called
the target "localised", naming the cause the PRD names: 30,549 candidates decoded per
query. [FINDINGS milestone 8 §9](FINDINGS.md) had already corrected half of that: every
ladder ran the `text` arm, where no scorer calls `Doc`, so the candidate decode is the
vector arm's cost and the 345.2 MiB was unattributed.

Two guesses were then made about what a `text`-arm query allocates, and the instrument
built to check the first is what refused it:

| accumulator hint | KiB/query |
| --- | --- |
| corpus-sized | 15,691.2 |
| first posting list | 19,501.6 |

The `allocs` profile then attributed the whole figure. 53.2% is `Index.Lookup`
materialising a term's entire posting list into a fresh slice, per term, per query; 44.3%
is the accumulator map and the candidate slice.

### Question

Remove the per-term materialisation how — a streaming iterator, which is what milestone
10's sentence describes, or a caller-owned buffer, which is not a new shape at all?

### Decision

**A buffer.** `Index.LookupInto(term string, buf []Posting) []Posting` is `Lookup` writing
into the caller's array. `pkg/scorer/text` keeps one across a query's terms, so what is
live is the longest posting list rather than the sum of them.

**And the first guess is reverted**, on the measurement rather than on the property test it
passed.

### Why

**D-006 decides it, not performance.** A segment that claims a term and cannot decode it
makes the whole lookup absent, pending postings included — absence is what this repository
gives corruption on the read path, because a partial posting list is not a shorter answer,
it is a wrong one. That verdict arrives *after* some of the term's postings are decoded.
Postings sitting in a buffer can be discarded; postings already yielded to a caller cannot.
An iterator would have to either weaken the guarantee or hand back a signal the caller must
remember to act on, and the second is the first with extra steps.

**It is also the smaller change.** `scanPostings` already streams, because `Merge` needs it
to; `lookup` is the only caller that builds a slice. So the traversal was already there and
what was missing was somewhere to put the result. One exported method, no new interface, no
change to what `Search` or `Fuser` see.

**The lock stays where it was.** One read lock for the whole walk, released before
returning, and nothing of the caller's runs inside it. An iterator yielding under the lock
would put the scorer's `DocLen` call inside it, and `index.go` already records what that
costs: a second `RLock` from a goroutine holding one deadlocks the moment a writer queues
behind it. Avoiding that would have meant handing document lengths out through the iterator,
which is a second design decision smuggled into this one.

**Why the first guess loses.** Hinting the accumulator from the first posting list is
correct for a narrow query and wrong for the workload the clause is judged on: a
TREC-COVID query's term union really is most of the corpus, so the map doubles its way up
and every abandoned table is charged to the query. Worse for a *peak* specifically, because
during a growth the old table and the new one are live together. The trade does not vanish
by taking the other side — a narrow query still pays 4.52 MiB for a map holding eight
entries — and what would remove it is a posting count in the terms index, which `termSpan`
does not carry. So the trade is published rather than resolved.

**The invariant.** A second line on `pkg/engine/testdata/engine_api.txt`, after D-015's
first. `public_api.txt` does not move, `pkg/fusion` is untouched, `go list -m all` is one
line, no existing signature changes, and `make eval` returns nDCG@10 identical to four
decimals. Two lines of exported surface is what this round has spent to keep the
architecture assertion intact while attacking the memory clause, and both are recorded
against the invariant rather than against a changelog.

**Milestone 10 is not what this is.** Its sentence is "sorted traversal and early
termination, so candidates are never fully materialised", and early termination is the half
that could force a stream to declare its own score bound — the thing the PRD's second
falsification clause is about. Nothing here declares a bound or reorders a traversal.
[PERF.md](PERF.md) §5.3 registers what the ladder result licenses, including the case where
milestone 10 fires after this.

### What would show this decision was wrong

- **The ladder does not move.** 30.7% off what a query allocates is a prediction about the
  345.2 MiB and not a measurement of it. If the peak is unchanged, the 44.3% half or
  something not in this profile is what holds it, and §5.3 outcome 2 or 3 applies.
- **A second caller wants the postings and cannot reuse a buffer.** Two callers with
  different lifetimes would want the iterator this rejected, and the rejection is about
  D-006 rather than about there being one caller — so it would need answering again, with
  the corruption signal designed rather than deferred.
- **The buffer outlives what a caller expects.** The contract is that the result is the
  caller's until the next call with the same buffer. A caller that keeps the slice across
  calls sees it overwritten, and `assertReadAPIsAgree` reusing one buffer across every term
  is the only thing standing over that today.
- **The trade published instead of resolved turns out to matter.** An adopter whose queries
  are two selective terms pays 4.52 MiB a query for an accumulator holding a few thousand
  entries. If that ever shows up in a real deployment rather than in a synthetic test, a
  posting count in the terms index is the format change that answers it.

## D-017 — A second mutex, and `Commit` takes a context rather than growing a twin

**Date:** 2026-08-24
**Milestone:** 9 — the write-lock ceiling
**Status:** accepted
**Context:** [FINDINGS milestone 9 §1](FINDINGS.md), [PERF.md §5.4](PERF.md), [D-015](#d-015--one-line-of-exported-surface-rather-than-a-document-whose-lifetime-quietly-changed)

### Context

[FINDINGS milestone 5 §3.3](FINDINGS.md) measured an 11.063 s window in which `Commit` held
`ix.mu` exclusively, and a read that arrived inside it waiting 12.539 s — 150× the p50
beside it. Of the window, 20,000 `Add` calls were 49 ms; the other 11.014 s was the commit,
and nearly all of that was `buildIVF`.

`buildIVF` only reads the index. The `ponytail:` note on `Commit` had already named the
upgrade — "encode under the read lock and swap under the write lock, counting the documents
captured so a concurrent `Add` is neither written twice nor dropped" — and priced it at that
capture counting: a partial hand-off of `docs`, `byKey`, `postings`, `docLen`, `totalLen`
and `base`.

Separately, `Commit` took no context. The README's Limitations table published that as a
known limit, and `cmd/weft-eval`'s write-arm probe had to wait out a whole encode on Ctrl-C
because of it.

### Question

Two questions that turn out to be one:

1. Encoding under the read lock needs the pending segment to stand still. Pay for the
   capture counting, or find something cheaper?
2. Make `Commit` cancellable by changing its signature, or by adding `CommitContext`
   beside it?

### Decision

**A second mutex, `Index.wmu`, and `Commit(ctx context.Context, dir string) error`.**

`wmu` is a plain `sync.Mutex` that every site taking `ix.mu` exclusively takes first —
`Add`, `Close`, `Merge`, `Commit`, which is all four. `Commit` then holds `wmu` across its
whole body while taking `ix.mu` in read mode for the encode and exclusively only for the
`adopt`.

**The capture counting is not built.** With `Add` excluded, the pending segment cannot
change and there is nothing to count.

**`CommitContext` is not added.** The signature changes, and the golden API file records one
line altered rather than one added.

### Why

**`wmu` is what makes the split work at all, and this is the part that is easy to get
wrong.** `sync.RWMutex` prefers writers: while a commit holds `mu.RLock` for 11 seconds, one
`Add` entering `mu.Lock` makes *every* `RLock` after it queue behind that waiter. Lowering
the encode to a read lock without `wmu` does not remove the 12.5 s — it only changes what
triggers it, from "a commit is running" to "a commit is running and somebody added a
document". `wmu` stops a mutator from reaching `mu.Lock` in the first place.

**The gap between the two sections is safe for the same reason.** `sync.RWMutex` has no
upgrade operation, so the read lock is released and the write lock taken separately. Nothing
can change in between, because every mutator takes `wmu` first and `wmu` is still held.

**Not building the capture counting is a smaller diff and a smaller invariant.** The counting
version has to keep two halves of the pending segment consistent across a hand-off; this
version has none, and the set written to the segment is exactly the set that was pending when
the commit was admitted. That is also what keeps milestone 2's atomicity argument intact: the
commit point is still the rename, and a document arriving during training arrives *after* the
commit rather than into an undefined state.

**The price is named rather than hidden.** `Add` blocks for a whole `Commit`. It already did,
so this is a ceiling and not a regression, and `Index.wmu` carries both the ceiling and the
upgrade path — the capture counting, owed the day a caller needs to ingest during a commit.
Milestone 9's clause is read latency.

**The signature changes because there is nothing to preserve.** `v0.1.0` is not cut. An
`XxxContext` twin planted where no compatibility exists becomes permanent surface for a
compatibility that never existed. `Search` already takes a `context.Context` first, so the
twin would also leave two entry points with two conventions. The 82 call sites are entirely
tests, `cmd` and `examples`: a one-time mechanical churn against permanent surface.

**Cancellation is defined by the rename, so it needed no new state.** Before the rename,
nothing is published and what is left is the unnamed `seg-<gen+1>` that `Open` already
documents and the next `Commit` already sweeps. After the rename, `ctx.Err()` is ignored:
stopping between the rename and the `adopt` would leave the directory publishing a generation
the live index has no mapping for, and the next `Commit` would report that directory as
corrupt. **A cancellation must not be able to reach a state a crash cannot** — which is why
that is the one thing the tests assert on both sides of the race rather than picking a side.

### What would show this decision was wrong

- **A caller needs to ingest while a commit runs.** That is the one thing `wmu` forecloses,
  and the capture counting is then owed with the invariant designed rather than deferred.
- **`during` max stays above 1 s with `during` ≈ `outside`.** The lock would be fixed and the
  wait would be something else — the 30 MiB segment write plus two fsyncs is the first
  candidate — and [PERF.md §5.4](PERF.md) registered that reading in advance. A different
  cause is not a pass.
- **A fifth `mu.Lock` site appears without `wmu`.** The whole design rests on the enumeration
  being complete, and one omission restores the full stall. `grep -n 'mu.Lock()'
  pkg/engine/*.go` is the check, and `TestReadsMakeProgressWhileACommitEncodes` is what
  catches it from the outside.
- **`Merge`'s stop turns out to be what callers actually hit.** It is longer than a commit and
  this round did not touch it beyond `wmu`, on the grounds that no arm measures it. Build the
  arm and the same restructuring applies.

---

## D-018 — Tombstones leave the statistics immediately, and `Len` stops counting documents

**Date**: 2026-08-25 · **Milestone**: 11 · **Status**: accepted

### The question

`Stats` and `AvgDocLen` are what BM25 normalizes every score against. When a
document is deleted, does it leave those numbers at once, or at the next commit,
or not at all?

The PRD asked it as a conflict: taking a tombstone out of the statistics
immediately might fight the commit atomicity milestone 2 bought, and leaving it in
makes **every score quietly wrong** — an IDF computed against a corpus larger than
the one being searched, and a length normalization against an average that
includes documents nobody can read.

### The decision

**Immediately, and exactly.** `deadSet` carries two running totals beside the
bitmap — how many documents are tombstoned and how many tokens they held — and
`Delete` maintains both in O(1) from the length the document actually had. `Stats`
and `AvgDocLen` subtract them.

There is no conflict with atomicity. The in-memory set is the authority and the
`dead-<gen>` file is its durable copy, published by the same manifest rename that
publishes a segment — which is exactly the arrangement `Add` already has with the
pending segment. The conflict the question anticipated would only exist if the
statistics were updated *at* commit boundaries; they are not.

**And `Len` does not follow.** It counted documents and was also one past the
highest `DocID`; a tombstone makes those different numbers. `Len` keeps the id
bound and `Stats` takes the population.

That split is forced, and by a scorer that never mentions deletion.
`scorer/recency` walks `for i := range ix.Len()` and skips whatever `Doc`
refuses. Narrowing `Len` to the live count would stop that walk short of the
newest documents and return a wrong ranking rather than a slow one — so the one
change that would have made `Len` "correct" is the one change that would have
forced a scorer edit, which is the milestone's falsification condition.

### What it costs, stated rather than argued away

Two numbers on `Index` that must move together with every mutator, and an `Open`
that walks the tombstone set to rebuild the token total — bounded by the set, not
the corpus. And a corpus-walking scorer keeps visiting deleted ids: `recency` is
O(id space) forever, because nothing reclaims an id.

### The rejected alternative, and the signal that it was right

**Leave tombstones in the statistics.** It costs nothing to implement and the
error is small while the deleted fraction is small. It was rejected because the
error is *invisible*: nothing reports it, no test fails, and a caller comparing
weft's ranking against another engine's would find a discrepancy with no name.

The signal that this decision was wrong: if an index with a high deleted fraction
measures **higher** nDCG with tombstones left in, then leaving them in was acting
as a length-normalization correction rather than as an error, and the exact
answer is exactly the wrong one. Nothing has measured that. `docs/PERF.md` §5.5
run B is where the number would come from.

---

## D-019 — DocIDs are never renumbered, so deletion reclaims nothing

**Date**: 2026-08-25 · **Milestone**: 11 · **Status**: accepted

### The question

A tombstone hides a document. Something has to decide when — if ever — its bytes
go away, and the honest options are two: compact during `Merge`, or never.

### The decision

**Never.** A deleted document keeps its `docs` record, its `keys` entry and its
postings, and every `Merge` copies all of it forward. The only compaction weft
has is a full re-index.

Compaction means renumbering, and three separate things rest on `DocID` being
what it is:

1. `engine.TopK` breaks ties on `DocID`. Milestone 4 measured 241 reported slots
   decided by that tiebreak alone, so renumbering moves rankings.
2. Posting lists are ascending by `DocID`, which the block encoder's delta chain,
   `Merge` and every reader take as given.
3. `Merge` is a *concatenation* of adjacent segments precisely because ids do not
   move. A compacting merge is a different algorithm with a different cost.

Renumbering is therefore not a local change to `Merge`; it is the `DocID`
namespacing problem [FINDINGS §3.4](FINDINGS.md) has carried since milestone 2,
and doing it here would have meant doing that first.

### What it costs, priced

- **Disk grows monotonically with deletions and updates.** An update of a
  committed document is a tombstone plus an append, so a workload that updates
  hot documents repeatedly pays for every version it ever wrote.
- **Ids are spent, not documents.** The ceiling `Add` enforces is 2³²−1 *ids*. A
  corpus updated hot enough exhausts that before it exhausts documents. Nothing
  has measured where.
- **`Nearest` weakens.** It promised at least k candidates when the index holds k
  vectors; a segment widens its own probe until it has them and then the
  tombstones are filtered out afterwards, so fewer than k may survive. Recall
  falls as the deleted fraction rises, and the widening loop would have to learn
  about tombstones to fix it.

All three are published in `docs/FORMAT.md` §8 and the README's limitations table
rather than left in a comment.

### The rejected alternative

**Compact during `Merge`.** It is what every mature engine does and it is the
right answer eventually. It was rejected for this round because it requires
`DocID` namespacing first, and because milestone 11's question was whether
deletion can be added *without* the scorers learning about it — a question that a
renumbering merge does not answer any better, at several times the cost.

The trigger for revisiting: a caller whose deleted fraction makes the disk or the
`Nearest` recall a problem they can name. `docs/PERF.md` §5.5 run B is the first
number on either.

---

## D-020 — `k` stays one number, because the sentence that explained it worked

**Date**: 2026-08-26 · **Milestone**: 12 · **Status**: accepted

### The question

`Search`'s `k` is both the per-scorer request size and the size of the fused
result. Milestone 6 recorded that as defect 3 and repaid it with prose rather
than with an API change. The PRD's open question asked whether milestone 12
should now repay it with code.

### The decision

**No. `k` remains dual, and callers pass a `k` above their display size and
slice.** This was fixed before the milestone 12 trials ran, together with the
condition that would overturn it, so that the trials could decide it rather than
confirm it.

The registered resume condition: *if a subject repeats what §6.2 recorded — that
it had to reshape its program to discover that fusing at the display depth
outvotes a new orthogonal signal — the sentence failed and the API repays it.*

**The condition did not fire.** Task C's subject fused at 50 and displayed 10,
cited `Search`'s doc comment as its source, and listed the point among the things
it did *not* have to establish by experiment. Task D's subject never hit it. One
trial subject met this by experiment in milestone 6; none did in milestone 12,
with the sentence in place. That is the whole evidence for keeping the API as it
is, and it is the reason the decision is recorded rather than assumed.

### The rejected alternatives

- **`SearchDeep(ctx, q, depth, k, fuse, scorers...)`.** One golden line, two entry
  points. What it buys is the `cands[:display]` the caller already writes; what it
  costs is that every adopter must now decide which function they are calling.
- **A `depth` parameter on `Search`.** One golden line changed and every call site
  in the README, `examples/`, `internal/eval`, `bench` and the tests. Permitted
  before `v0.1.0`, and it buys the same one line.

### What would reopen it

A subject who meets the truncation by experiment *after* reading `Search`'s doc
comment. That is the same signal, read the same way, and the next adoption trial
is where it would appear.

---

## D-021 — A value attaches to a position, so the extension point is the constructor and the `Fuser`

**Date**: 2026-08-26 · **Milestone**: 12 · **Status**: accepted

### The question

`engine.Query` has three fields, no map and no `any`. An external scorer needing
an input that changes per query, or a caller needing a constraint the query type
cannot express, has nowhere in the type to put it. The PRD asked what shape the
extension point should take: a map, an `any`, or a type parameter.

### The decision

**None of the three. Nothing is added.** Milestone 4 answered this question once
already, with `FuseWeighted`: a value attaches to a *position* rather than to a
name. The positions already exist and the milestone 12 trials used both.

1. **A per-query input attaches to the scorer's constructor.** The caller builds
   one scorer per search, binding the value. The corpus-sized half of the input is
   built once and handed in, so a per-query construction is an allocation.
2. **A constraint attaches to the `Fuser`.** `Search` takes the fuser as a
   parameter, so a caller needing an intersection rather than a union of votes
   writes one that reads a chosen stream as a restriction. The convention is
   positional and is the caller's own; no scorer can observe it.

Two blind subjects reached both arrangements from the public API with **zero
code-required blockers** ([ADOPTION §8](ADOPTION.md)), which is what makes this a
decision to add nothing rather than a decision to defer.

### The rejected alternatives, all three priced

- **`Query.Extra any`.** One slot. Task C required *two* external scorers sharing
  one input, so two consumers contend for one field, and a failed type assertion
  is not an error but an abstention — the exact failure mode `Seeds` already has,
  which `TestOneQueryTimeValueReachesTwoExternalScorers` now pins.
- **`Query.Extra map[string]any`, keyed by import path.** Namespacing solves the
  contention. It pays for it with compile-time checking, and `Query`'s own doc
  comment already argues the other way: a missing map entry is a runtime surprise
  where a missing constructor argument does not compile. **This is what the rung
  above would have been** had a trial demanded one, and the reason it would be a
  map rather than a bare `any` is task C's multiplicity.
- **A type parameter, `Query[T]`.** `Scorer` becomes `Scorer[T]` and all four
  in-tree scorers become generic. That widens the `Scorer` interface, which is the
  PRD's first-stage falsification condition, so buying type safety this way would
  cost the hypothesis the round exists to test. The most expensive option on the
  list.

### What this reverses, and what it did not have to

[ADOPTION §7.4](ADOPTION.md) reversed milestone 6's pass line 2 — *code-required
blockers are not fixed in this milestone* — on the grounds that milestone 6 did
not know whether an extension point was needed and milestone 12 would be fixing
one a trial had named. **The reversal was authorized and never fired**, because
both trials produced zero code-required blockers. It stands for the next round on
the same terms: a named blocker, not an anticipated one.

[D-010](#d-010--adoption-is-decided-by-a-trial-and-the-extension-point-is-not-designed-before-it)'s
rule survives a second application intact. The extension point was not designed
before the trial, the trial did not ask for one, and none was built.

### What would show this is wrong

A signal that needs its per-query input *inside* a scorer the caller does not
construct — a weft-supplied scorer, or one buried in a third-party wrapper — where
there is no constructor to bind to and the `Fuser` is too late. Nothing has
produced one. If one appears, the map above is the shape, and its cost is already
priced here.

## D-022 — The tokenizer is a seam on the constructor, and the scorer asks rather than receives

**Date**: 2026-08-26 · **Milestone**: 13 · **Status**: accepted

### The question

`engine.Tokenize` is a package-level function called from four places — three
inside `Index` and one in `scorer/text`. A caller whose language the default
splits wrongly has no way to replace it. Where does the replacement point attach,
and what does it cost?

### The decision

**A variadic `Option` on the two constructors that already exist, and the index
owns it.** `New(opts ...Option)`, `Open(dir string, opts ...Option)`,
`WithTokenizer(Tokenizer) Option`, and `Index.Tokenize` as the one way in.

`scorer/text` calls `s.ix.Tokenize(q.Text)`. It **asks** the index, the same
shape as the `s.ix.Stats` and `s.ix.LookupInto` calls beside it, and does not
**receive** a tokenizer through `Query` or through the `Scorer` interface. That
distinction is the milestone's falsification condition and
[FINDINGS milestone 13 §3](FINDINGS.md) judges it.

This is [D-021](#d-021--a-value-attaches-to-a-position-so-the-extension-point-is-the-constructor-and-the-fuser)
applied a second time and from the other side. A value attaches to a *position*;
milestone 12 found the position for a per-query value was the scorer's
constructor, and the position for an index-wide one is the index's.

### The ladder, priced before anything was written

| rung | shape | golden | call sites | verdict |
| --- | --- | --- | --- | --- |
| 0 | `var Tokenize = func(...)`, a package variable | 1 changed | 0 | **rejected** |
| 1 | `New(opts ...Option)` / `Open(dir, opts ...Option)` + `WithTokenizer` | **5 to 7** | **0** | **accepted** |
| 2 | `NewWithTokenizer` / `OpenWithTokenizer` | 4 to 5 | 0 | **rejected** |
| 3 | `New(tok Tokenizer)`, required | 1 changed | **77** | **rejected** |

- **Rung 0** is the cheapest and is unusable. Two indexes in one process cannot
  have different tokenizers, and writing it while an `Add` runs is a data race —
  one that a batch job setting it at startup and reading it forever would not even
  trip `-race` on, so the failure ships silently.
- **Rung 2** costs one to two golden lines *less* than rung 1. What those lines
  buy is the entry-point count: two rather than four.
  [D-020](#d-020--k-stays-one-number-because-the-sentence-that-explained-it-worked)
  rejected `SearchDeep` on the ground that an adopter would have to decide which
  of two entry points to call before knowing whether they cared about the
  difference, and this is the same shape. Variadic options also compose: a second
  option later is zero new entry points, where rung 2 would double them again.
- **Rung 3** buys exactly what rung 1 buys and breaks all 77 `New`/`Open` call
  sites in this module to do it. Variadic is source-compatible; every existing
  `New()` and `Open(dir)` keeps compiling and keeps meaning what it meant.

### Bound at construction, and no `SetTokenizer`

Three things come out of that one rule, and `Index.tok`'s field comment carries
all three:

1. **No lock on the query path.** Nothing writes the field after the constructor
   returns, so `Index.Tokenize` takes no `ix.mu`. A read lock there would be one
   index-wide acquisition per `Add` and per query, protecting a value that cannot
   change.
2. **A `SetTokenizer` would be a time coupling whose violation is not an error.**
   Called after the first `Add`, it leaves an index half of whose documents live
   in a different term space: every query against the other half answers zero
   hits, forever, with nothing to report. That is the class of silent failure
   `ErrDimMismatch` exists to refuse, refused the same way — at the write side,
   once.
3. **The open-time guard becomes a statement about the whole lifetime** of the
   index rather than about one instant of it. See
   [D-023](#d-023--the-mismatch-guard-recomputes-one-document-because-a-stored-name-can-lie).

**`engine.Tokenize` stays.** It is the default, `internal/eval/bm25_test.go` uses
it as the reference implementation the published nDCG figures were measured
under, and `Index.Tokenize` falls back to it when no option was given — which is
what keeps a zero-value `Index` usable, the promise that type's own doc comment
makes.

### What weft does not ship

**A second tokenizer.** The Hangul bigram tokenizer that demonstrates the seam
lives in `pkg/engine/tokenizer_test.go` and in `ExampleWithTokenizer`, not in
`pkg/`. Shipping it would turn a seam into a menu — an adopter would have to
choose between weft's two rather than plug in the one their language needs — and
morphological analysis is out of scope for the reason it has always been: it
collides head-on with the zero-external-dependency constraint that
[milestone 5](FINDINGS.md) tested and kept.

### What would show this is wrong

A signal that needs a *different* tokenizer for one field or one query while the
corpus keeps its own — per-field term spaces, which is a format v5 question
(`FORMAT.md` §8) and not an options question. Or an adopter who has to reach the
tokenizer from inside a scorer they did not construct, which is the same gap
[D-021](#d-021--a-value-attaches-to-a-position-so-the-extension-point-is-the-constructor-and-the-fuser)
names for per-query values.

## D-023 — The mismatch guard recomputes one document, because a stored name can lie

**Date**: 2026-08-26 · **Milestone**: 13 · **Status**: accepted

### The question

A directory indexed with one tokenizer and queried with another does not fail. It
answers **zero hits**, on every query, forever — the query's terms and the
corpus's terms are different strings, so no posting list is ever consulted and
there is nothing in the result to say which of the two happened. The PRD's open
question 5 asked whether that can be caught mechanically.

### The decision

**Yes, from bytes that were already on disk, and the format does not change.**
`Open` picks the first live document whose stored token count is non-zero,
recomputes its tokens with the configured tokenizer, and compares the count
against the number `docoff` already holds. Disagreement is
`ErrTokenizerMismatch`. A bigram index opened with the default reads 7 tokens on
disk against 2 recomputed.

`FORMAT.md` §4 forbids recomputing a token count from a document's text, and the
reason it gives is this round: *"recomputing would let a future tokenizer
replacement disagree silently with postings that already exist."* The ban is on
recomputing in order to **use** the answer. This recomputes once in order to
**compare** it, which is that predicted disagreement being caught rather than
committed.

It is the same trade `ErrDimMismatch` makes one layer down: caught at open time it
is one refused directory, not caught it is every query failing for the life of the
index. And it is a genuinely new failure mode — a directory that opened yesterday
can be refused today — traded against a query that used to answer nothing.

### The rejected alternative: store the tokenizer's name

A `v5` segment field naming the tokenizer, compared at open.

- **A label can lie and bytes cannot.** A Go function value has no stable name, so
  what would be stored is a string the caller supplied. A caller who swaps
  tokenizers without editing the string makes the guard confidently wrong, which
  is worse than no guard: it certifies the mismatch it exists to catch.
- **It buys a format version.** The PRD used *only deletion touches the disk
  format* as the ground for milestone ordering, and
  [milestone 12's D8](FINDINGS.md) carried position and field indexes to v5 on the
  same rule. `FORMAT.md` §7.8's lesson — a root file is cheaper than a section —
  holds, but a version bump still has a cost and there is no need to pay it here.

### The third check, written and taken back out

The registered design had a third step: every recomputed term against the
segment's terms index, which would have caught a tokenizer producing the right
*number* of different terms. **It shipped nothing.** It failed
`TestALyingTermOffsetIsNeverFollowed`, `TestAnImpossibleFrequencyIsRefused` and
`TestALyingBlockMinimumIsRefused`, each of which doctors a segment's whole `terms`
section and then asserts `Open` **succeeds** with the damage surfacing as absence.

Those tests are right. A terms section that does not claim a live document's terms
is what a replaced tokenizer looks like *and* what a damaged one looks like, and
the two are the same bytes — so the check reported corruption as a tokenizer
mismatch, sending a caller to look for a tokenizer they never changed. It also put
verification back into `Open` for one section, which milestone 3 removed and
[D-006](#d-006--map-the-segments-so-the-read-api-does-not-grow-an-error)
already settled the direction of.

### The ceilings, published rather than discovered

1. **A tokenizer that preserves token count passes**, whatever it does to the
   terms. A stemmer is exactly that shape — one token in, one token out — so the
   guard catches the large failure and not the small one. Catching the small one
   needs the tokenizer's identity, which is the thing that cannot be stored
   honestly.
2. **The sample is one document.** Re-tokenizing the corpus would make `Open` cost
   the size of the index, which is the whole of what mapping it instead of loading
   it bought.
3. **A corpus with no text to judge passes.** It answers nothing under every
   tokenizer, so there is nothing to protect.
4. **A non-deterministic tokenizer disagrees with itself.** `Tokenizer`'s doc
   comment makes determinism the contract rather than leaving it to be discovered
   here.

All four are in the `ponytail:` comment on `checkTokenizer` and in `FORMAT.md` §8.

### What would show this is wrong

A real corpus refused by the guard while its tokenizer is in fact the one it was
written with — which for a deterministic tokenizer cannot happen, so an occurrence
would mean the determinism contract is being broken in the field and the doc
comment is not enough. Or a caller who wants the count check off because a
one-document sample is too weak a signal to justify the refusal, which is the
opposite complaint and would argue for widening the sample rather than removing it.

## D-024 — The gate on a performance run is a loaded probe, not the unloaded median

**Date**: 2026-08-27 · **Milestone**: 14 · **Status**: accepted

### The question

Two performance rounds in a row produced nothing. Milestone 11's run A died during
the index load; milestone 13's spent 97 minutes and came back **void** — every
clause missed by one to two orders of magnitude, and the commit *before* the
milestone missing them the same way, so the instrument had been measuring the
machine. [FINDINGS milestone 13 §8 item 2](FINDINGS.md) named the gap: *a
quiet-machine requirement is now a load-bearing part of the procedure and nothing
enforces it — no preflight, no recorded machine state beyond `date`.*

This is a procedure decision rather than a code one, and it is recorded here because
[PERF §5](PERF.md)'s campaign order is the thing it changes.

### The decision

**A short loaded probe at the top rate, run as its own process, before the ladder.**
`make bench-preflight` is `bench -rates 27.28 -rotations 10` — 500 requests, under
30 seconds in practice. Pass is the probe rung's p50 ≤ **twice the same process's
unloaded p50**, and shed 0.

Twice is `loadgen.SaturationRate`'s constant rather than a number chosen for this
gate: the first rung past twice the unloaded median *is* saturation, and 27.28 q/s
was **not** saturation on the published ladder. A machine that saturates at the
probe cannot reproduce that ladder, and there is no version of the run worth
starting on it.

### Why not the unloaded median, which is the obvious gate

**Because it was normal on the machine that voided the run.** 34.072 ms against a
published 32.231, a 1.06× ratio, well inside the 8.8% machine-state band milestone 8
documented for itself. `benchWarmup` already computes this figure and prints it, so a
gate built on it is nearly free — and it passes the exact failure it would exist to
catch.

The readings that *did* separate the two machines all require load. The cheapest is
the top rate: **1.04× published, 78× void**. Nothing needs tuning between those.

| reading | void run | published | ratio |
| --- | --- | --- | --- |
| unloaded p50 | 34.072 ms | 32.231 ms | 1.06× — **passes** |
| p50 under load, 27.28 q/s | 2.616899 s | 33.470 ms | **78×** |
| ladder peak RSS | 654.1 MiB | 100.7 MiB | 6.5× |

### Why a separate process

`peakrss` is `ru_maxrss`, a high-water mark the kernel never lowers ([PERF
§2.7](PERF.md)), and
[D-014](#d-014--the-memory-pass-line-reads-the-processs-mark-milestone-8-misses-it-and-milestone-10-does-not-fire-on-that)
fixed the memory clause as the **ladder's** peak. A probe folded into the ladder
would raise the mark to near its top-rung value before rung 1 reported, changing what
the clause reads. Its own process has its own mark.

### A documented step, not an enforced one

No Go code, no new flag: `-rates` and `-rotations` both already existed, so the
target is six lines of Makefile and `pkg/` stays at zero. What was missing was never
the arithmetic — nobody failed to compare two printed numbers. **Nobody ran a probe
at all** before a 97-minute ladder. The fix is that the step exists in the procedure.

The ceiling is on the target in a `ponytail:` comment: if a fourth ladder still comes
back void, the probe becomes a `-preflight` flag with an exit code, about thirty lines
in `cmd/weft-eval/bench.go`.

### The rejected alternatives, and what revives each

- **`ru_nivcsw` per rung.** `internal/loadgen/rusage_unix.go` already calls
  `getrusage`, so one more field is a few lines, and involuntary context switches
  measure contention **directly** instead of by proxy. Rejected for having no
  threshold: this repository held **zero** observations of the figure, so it would
  have added a number rather than a gate. The first two observations arrived with the
  probes below and do not fix that — the machine passed the probe but was not quiet
  in the registered sense, so they bound nothing. **Revived by** a run where the probe
  passes and the ladder is void anyway, which is the case where the probe cannot see
  what the ladder feels.
- **`uptime` load average around each run.** Zero lines, and a proxy for the thing
  rather than the thing. **Partly adopted**: logged beside `date`, which is what item
  2 asked for, and excluded from the verdict.

### The falsification condition fired on first use — 2026-08-28

**Accepted, and amended by its own test.** The condition below said *a ladder that
comes back void after a probe that passed* would show this wrong. Four probes passed —
1.00× to 1.05×, shed 0 — and **two ladder attempts produced no verdict**: the first
took SIGTERM at 46 minutes, the second completed eight rungs and both arms printed
`DISCARD this run` after the laptop's lid was closed mid-ladder, twice.

The naive conclusion is that the probe is worthless. That is wrong, and the correct
reading is what makes this decision worth keeping:

| failure mode | detector | worked? |
| --- | --- | --- |
| a **contended** machine | `bench-preflight`, this decision | untested — no attempt failed this way |
| a **suspended** machine | `SuspendTolerance`, present since milestone 7 | **yes**, in-band, both arms, with durations |
| a machine that **stays awake** | `caffeinate -dimsu`, prescribed since milestone 5 | **no** |

**Both detectors behaved correctly; the mitigation is what failed.** The probe measures
contention and the failure was suspension — different things, caught by different
checks, and the second check did its job without help. So the amendment is not to the
probe's pass line, which is untouched, but to two places around it:

1. **The lid stays open.** `caffeinate -dimsu` holds `PreventUserIdleSystemSleep` and
   has no power over clamshell sleep; the machine slept on AC at 100% charge. Nine
   milestones prescribed a remedy that does not cover the failure mode `clock.go`'s own
   comment names. Now stated beside the command in [PERF §5.7](PERF.md).
   `sudo pmset disablesleep 1` rejected — root, and a machine that never sleeps if the
   operator forgets to unset it.
2. **The probe is necessary and not sufficient, for two reasons rather than one.** It
   certifies the machine at the instant it runs and the ladder needs 3.1 hours; and it
   measures one failure mode of at least three. [PERF §5.7](PERF.md) gains readings 5
   and 6 — *window known unavailable* → not executed, and *instrument discarded the
   completed run* → void with the cause in-band.

**What the probe did buy, and it is not nothing.** Twelve independent readings of
`alloc 10869.0 KiB/query` across probes and rungs, agreeing with the published figure
to one decimal, which is the evidence that both arms run the same work. And the
observation that 27.28 q/s as a **lone rung** returns 34–35 ms on this machine while
27.28 q/s as a **ladder's fourth rung** collapses to 1.6–2.0 s — a second, sharper use
for the distinction
[D-014](#d-014--the-memory-pass-line-reads-the-processs-mark-milestone-8-misses-it-and-milestone-10-does-not-fire-on-that)
drew for `peakrss`, and the reason a passing probe can never stand in for the clause.

### What would still show this wrong

A ladder that comes back void on a **quiet, awake** machine after a passing probe —
the test the two attempts above did not get to run, because neither machine stayed
awake. If that happens the probe is measuring something the ladder does not care
about, and `ru_nivcsw` per rung becomes the next instrument rather than a rejected
alternative. The opposite failure — a probe that fails on a machine which would in
fact have reproduced the ladder — costs one minute and is the direction the threshold
was chosen to err in.

---

## D-025 — The server is a `cmd/`, and the library is still the product

**Date**: 2026-09-05 · **Milestone**: 24 · **Status**: accepted

### The question

The founding PRD lists **"compatibility with an existing query language"** among the
things this project does not do, and `docs/RESEARCH.md` §3 dismisses zinc, blast and
phalanx in four words — *they are servers*. An OpenSearch-compatible HTTP surface
contradicts the first directly and points the second at weft itself.

Both objections are real and neither survives being looked at closely, which is why
this is a decision and not an oversight.

### The decision

**Build it, in `cmd/weftd` and `internal/opensearch`. Nothing under `pkg/`.**

The reversal was approved on 2026-09-05 by the maintainer, who read the argument below
and asked for the implementation.

The check that makes this a boundary rather than an intention: **milestone 24 changes
zero lines under `pkg/`.** `git diff --stat pkg/` is the assertion, `make arch` guards
the two golden API files, and `make deps` still prints one module because `net/http`
and `encoding/json` are the standard library.

### Why the founding out-of-scope line does not bind

Its sibling — *"query language and parser — queries are built through the Go API; a
DSL contributes nothing to proving the architecture"* — was reversed by milestone 20,
and reversed on grounds that apply here word for word. The rejection was conditional
on an unproven architecture. Milestones 1, 16, 18, 19, 20 and 23 have since passed;
the open question is no longer whether the design works but whether anyone will use
it, and the adoption metric this project registered for itself has **not started**.

### Why `internal/` rather than `pkg/api`

`docs/ARCHITECTURE.md` already argues this for the evaluation harness: a measurement
apparatus is not part of the library contract, and keeping it out of `pkg/` leaves the
exported surface — and the golden file guarding it — untouched by the measurement. A
server is the same kind of thing. Put it in `pkg/api` and `engine_api.txt` starts
carrying HTTP types, every handler signature becomes a semver promise, and the
`CHANGELOG` claim that a release with no entry has nothing for a caller to do stops
being true for a package no library user imports.

`internal/opensearch` imports `pkg/scorer/*`, and that is not a violation. What
milestone 1 forbids is **`pkg/fusion` knowing a scorer exists**; `cmd/weft/main.go`
has named all four since the beginning. `make deps`' second check is unchanged.

### What this costs, and it is not nothing

A server is an operational surface with its own failure modes, and this one opens on a
codebase `docs/STATUS.md` calls **not usable in production**: sustained load collapses
at 27 queries a second and a commit holds the writer for 11 seconds. `weftd` binds to
loopback and prints that warning at startup. Both are mitigations and neither is a
fix; milestone 27 is where the number gets measured through HTTP, and it is blocked on
a quiet window that has failed four rounds running.

### What would show this decision was wrong

**A line needed under `pkg/`.** If the HTTP surface cannot be expressed without
widening `Scorer`, `Search` or `Fuse`, then the architecture claim does not survive a
process boundary — and that is a finding worth more than the server. It gets written
into `docs/FINDINGS.md` before the line is written into the code.

The weaker signal: the library falling behind the server. If a capability lands in
`internal/opensearch` that a `go get` user cannot reach, this decision has quietly
made the server the product.

---

## D-026 — The handshake claims OpenSearch 2.19.0, and that is the only lie

**Date**: 2026-09-05 · **Milestone**: 24 · **Status**: accepted

### The question

`GET /` must return `version.distribution: "opensearch"` and a version number, because
every official client reads them and branches on what it finds. weft is not
OpenSearch. Saying so truthfully means no client connects, and a compatibility surface
nothing connects to is not a compatibility surface.

### The decision

**Claim `2.19.0`, and confine the untruth to that one response.** Everything else is
honest: a query type weft cannot express returns **400 or 501**, never 200 with an
empty result.

2.19.0 rather than something older because it is the first line carrying the `hybrid`
query and an RRF ranker — the shapes that map onto `fusion.Fuse` without translation,
and the reason milestone 26 exists. Claiming 1.x would buy a smaller lie and lose the
part of the protocol weft is actually good at.

### Why silence is worse than refusal here

This is `pkg/query`'s rule reaching the wire. That package's documentation states it
outright: *an unterminated quote, a malformed range, a phrase with no terms — none of
them is a query that quietly returns nothing, which is the failure this package
refuses everywhere else.* A server that answers `aggs` with an empty aggregation block
commits exactly that failure, at a distance, in someone else's dashboard.

So the version string is a handshake token, not a claim about behaviour, and the
behaviour says what it is on every request that reaches past the handshake.

### What would show this decision was wrong

A client that gets **further in** because of the claimed version and then fails in a
way the operator cannot attribute — a 501 arriving somewhere the client has no error
path for, so it surfaces as a hang or a silent empty page. If milestone 24's
`make compat` run finds that shape, the answer is not a lower version number but a
documented list of what the claim invites, kept in this decision.

## D-027 — The mapping is the server's, and it has five types because five is what changes an answer

**Context.** `query.EncodeInt` names its own contract: *the caller indexes it and the caller
queries it.* A value encoded one way and queried another matches nothing with nothing to report.
weft's index holds terms and postings and does not hold the fact that `views` was a number, so
over HTTP something has to remember it — the client that wrote the document and the client that
writes the range query are not the same process, and may not be the same person.

**Decision.** The server keeps a mapping in `<dir>/_mapping.json`, beside `_source.json` and
published by the same temp-file-then-rename rule. Five types: `text`, `keyword`, `date`,
`integer`/`long`, `knn_vector`. Any other type is **refused**, and a field already declared
cannot be re-declared to a different type.

**Why five.** Each type has to earn itself twice — once deciding what term a value becomes at
index time, once deciding what a bound becomes at query time. A type that changed neither answer
would be `text` wearing a different name. `date` and the integers exist so `EncodeTime` and
`EncodeInt` can be applied at both moments; `knn_vector` exists so an array lands on
`Document.Vector` instead of being dropped as a non-scalar; `keyword` differs from `text` at
query time only.

**What was rejected.** *Accepting an unknown type and treating it as `text`* — that is the
failure this whole surface is arranged against: a range over a silently-demoted field matches
nothing and reports nothing. *Inferring the type from the first document* — the width of a
`knn_vector` would then depend on arrival order, and `engine.ErrDimMismatch` refuses a
mismatched width for the whole commit rather than for one document. *Allowing a re-map* — the
documents already indexed were encoded by the old rule, so the field would hold two encodings
and a range query would read half of it; OpenSearch refuses the same change for the same reason.

**The price, stated.** `keyword` does not fully arrive. One index has one tokenizer ([D-022] —
no `SetTokenizer`, no per-field analyser), so a keyword value is tokenized like any other field
and a keyword holding two tokens is found as a **conjunction** of its tokens rather than as one
indivisible term. Right for ids, statuses and tags; broader than OpenSearch for `"New York"`.
Fixing it is a per-field analyser, which is a `pkg/` change, which milestone 25's mechanical
definition forbids. Priced in `docs/LIMITATIONS.md` instead.

**Falsification.** If a sixth type is wanted and cannot be added without changing how the five
are read, the mapping is doing more than carrying an encoding and belongs somewhere else.

## D-028 — A required clause is one stream, and the disjunction that makes it one is a scorer

**Context.** `query.Must` intersects the positions it is given, and a `match` over two tokens is
two streams. Wired the obvious way, `bool.must` holding a two-word match becomes `operator: and`
— narrower than the client wrote, with no error and nothing to notice it by.

**Decision.** A clause landing in `must` or `filter` is collapsed to **one** stream first. A
single-stream clause is itself; a conjunction (`operator: and`, a multi-token `term`) is its
streams, which `Must` can require directly; a disjunction of several streams becomes `anyOf` —
twelve lines in `internal/opensearch` that union its inner scorers' candidates.

**Why here and not in `pkg/query`.** The constraint vocabulary did not need a disjunction; what
was missing was a *scorer*, which is the extension point this project is built on. Adding
`query.Any` would have grown the library's API for a problem a caller can solve, and
`docs/ADOPTION.md` measured callers solving exactly this kind of problem from outside.

`anyOf` returns every nominated document and does not truncate to `k`, which is the rule
`pkg/query`'s package documentation states for any scorer used as a restriction: a restriction
truncated to `k` excludes every document below its own cut, and that is a wrong answer rather
than a narrow one.

**`must_not` does not get this treatment**, and the asymmetry is the point: excluding a document
present in *any* of the streams is exactly what "this clause did not match" means for a
disjunction, so `query.MustNot` over every position is already correct.

**Falsification.** If a third occurrence type needs a fourth collapsing rule, "a constraint
names one stream" is not the right abstraction and the plan should carry a query tree instead.

## D-029 — `bool.filter` is emptied after it narrows, and a weight of 0 is not the short spelling

**Context.** A filter must narrow without contributing to the ranking. `fusion.FuseWeighted`
takes a weight per position, and weight 0 looks like the way to say "does not vote".

**Decision.** Filter streams are required by `query.Must` and then **emptied** — `blank`, ten
lines — before the base fuser sees them. Weight 0 is not used.

**Why.** A weight of 0 does not silence a stream's vote; it removes the document from the fused
result entirely. A filter written that way excludes everything it was meant to keep, which is
the inverse of the request. The PRD registered this trap in the `bool.filter` row before the
code existed, and it was still the first thing tried.

**What it rests on.** `query.Must` and `query.MustNot` each hand on a slice of the **same length
and order**, so a position stays meaningful through the whole chain
`Must(blank(MustNot(Fuse)))`. That property was undocumented; it is now asserted by test.

**The one exception.** A bool holding nothing but filters is not blanked — there is no other
ranking, and blanking would answer nothing to a query that named documents.

## D-030 — A weight is a position on the wire too, and `search_pipeline` is refused

**Context.** OpenSearch's `hybrid` query puts the weighting in a search pipeline's normalization
processor: two streams are normalized onto a common scale and then combined. That pipeline is
the thing this PRD's section 1 says weft exists to make unnecessary.

**Decision.** `hybrid` takes an optional `weights` array — weft's, not OpenSearch's — with one
weight per sub-query, expanded to one weight per **stream** and handed to
`fusion.FuseWeighted`. `search_pipeline` is refused with a 400 that says why.

**Why it can exist at all.** A weight attaches to a *position*, never to a name. So
`FuseWeighted` takes a `[]float64` and still cannot identify a single scorer, and
`go list -deps ./pkg/fusion` names no scorer package after this change exactly as before it.
That is [D-005]'s repayment reaching the wire: milestone 4's −0.1202 nDCG@10 was the cost of an
unweighted vote from a stream with nothing to say, and a client can now turn that vote down
without the fusion learning what the stream holds.

**What the refusal says.** Normalizing two streams onto a common scale is the step this server
does not have, because rank fusion reads position and never score —
`engine.Candidate.Score` is explicitly not comparable across streams. Answering
`search_pipeline` would mean inventing a normalization and calling it OpenSearch's.

**The structural guard.** Weights are positions in the original stream list and `query.MustNot`
is the one wrapper that hands on a shorter one. A hybrid inside a `bool.must_not` would shift
every weight after the removed stream by one, silently. It cannot happen, because a hybrid is a
whole query rather than a clause of a bool — refused, and tested rather than commented.

**Falsification.** If a caller needs a weight that depends on what a stream holds — a boost that
means something different for a vector than for a posting list — the positional convention is
insufficient and the fusion has to learn about signals, which is the architecture hypothesis
failing at the wire.

## D-031 — The document's time is a mapping flag, because a JSON body has no field for it

**Context.** `scorer/recency` reads `engine.Document.Time` and nothing else. A JSON body has no
such field: `{"published": "2024-03-01"}` is a date the client happens to care about, and
nothing on the wire says it is *the* date.

**Decision.** A `date` property may carry `"recency": true`. One per index. `documentFrom` sets
`Document.Time` from that field — and indexes it as a range-able term as well, because a client
that mapped a date wants both and neither is derivable from the other. `function_score` with a
`gauss`, `exp` or `linear` decay on that field becomes a `recency.NewAt(ix, time.Now())` stream;
a decay on any other field is refused by name.

**Why a mapping flag rather than a convention.** The alternatives were worse in the way this
project cares about: *the only `date` field* would break the moment a second date is mapped and
would break silently; *a field literally named `time`* would collide with a client's own schema;
*an index setting* would put the binding somewhere that does not already know the field's type
at both index and query time. `dimension` on `knn_vector` is the same shape of extension — the
k-NN plugin's, not core OpenSearch's — so the precedent is one a client already accepts.

**What this cost, and it is milestone 26's actual finding.** 33 net lines, all of them index-
time plumbing that a Go caller does not pay: filling `Document.Time` in a Go program is a struct
field assignment. The query half — the `function_score` clause — is 57 lines, inside milestone
1's budget. **The wire's surcharge on a signal is the binding, not the fusion.**

**Decay parameters are refused.** `scorer/recency` is an exponential with a fixed half-life;
reading `origin`, `scale`, `offset` or `decay` and ignoring them would rank by a curve nobody
asked for. The three curve *names* are accepted and approximated, because a name this server can
answer approximately is better than three names it refuses — and a parameter it would silently
ignore is not.

---

## D-032 — Every exported symbol has a command or a recorded reason, and a test counts them

**Date**: 2026-09-06 · **Milestone**: 28 · **Status**: accepted

### The question

D-025 registered the signal that would show the server had quietly become the product: *a
capability lands in `internal/opensearch` that a `go get` user cannot reach.* The inverse was
never registered and turned out to be the one that was true. `pkg/scorer/graph` was the package
the HTTP surface did not import at all; `engine.Scrub` had never been called by anything in this
repository; `query.Parse` — weft's own query language, milestone 20 — could be reached only by
writing a Go program.

None of that is visible from anywhere. The golden API files record what is exported and no test
asks whether anything can call it.

### The decision

**`cmd/weft/coverage_test.go` reads both golden files as data.** Every callable symbol they list
has a row: the surfaces that reach it and the call site that proves it, or the reason nothing
does. A symbol with no row fails. A row naming a symbol that no longer exists fails. A row
claiming a surface whose source no longer holds the call fails. The figure is printed rather
than asserted against a threshold:

```text
public callable symbols: 74, reached by a command: 71 (95.9%), reached by none: 3
```

The three are `NewCollector`, `Collector.Offer` and `Collector.Take`, and the recorded reason is
that a Collector is the top-k buffer a `Scorer` implementation keeps while it walks postings —
reaching it from a command would mean inventing a scorer for a flag to select, and `pkg/scorer`
is what that would duplicate.

This is milestone 25's shape reused. `TestTheRefusalRateIsCounted` holds the DSL table as data so
that a quietly implemented row and a quietly broken one both break the build; this holds the API
surface the same way.

### What it proves and what it does not

The proof is a call site in the surface's own source, found after comments are stripped —
because this repository writes what it is *not* doing beside the call it is not making, and
`internal/opensearch` says "engine.Scrub is not this" in a comment three lines from where a raw
text scan would have read it as a call.

It does not prove the call sits on a path a user can reach, and it cannot tell two same-named
methods apart: `.Len(` is `Index.Len` and `Adjacency.Len` both. Where that mattered the row
carries an explicit call string instead of the derived one, and `.Err(` is the case that caught
it — `bufio.Scanner` has one too, and the index command calls it on stdin.

### What would show this decision was wrong

**A command invented to satisfy the ledger.** The test measures reachability, and the cheap way
to raise a percentage is a subcommand nobody would run. The three unreachable rows are the
control: if a later round makes them reachable without a use case arriving first, the number is
being farmed rather than earned, and the reason recorded against them is the argument that has
to be defeated in writing before the row changes.

---

## D-033 — What OpenSearch has no name for gets a name OpenSearch does not use

**Date**: 2026-09-06 · **Milestone**: 29 · **Status**: accepted

### The question

Half of weft has no OpenSearch spelling. The graph signal is not a query type OpenSearch has;
neither is weft's own query string, a term-space walk, or `engine.Scrub`. D-026 confines the
untruth to the version handshake and says everything past it is honest — but it does not say
what to call a thing OpenSearch never named.

Two ways to get it wrong. Reuse an OpenSearch name for different behaviour, which is a second
lie and a worse one, because a client that already knows the name will not read the
documentation. Or refuse to expose the capability at all, which is D-025's weaker signal in
reverse: the library reaching further than anything a client can call.

### The decision

**A `weft_` prefix on the clause and a `/_weft/` prefix on the route.** `weft_graph` is the graph
signal; `/_weft/terms`, `/_weft/postings`, `/_weft/scrub` and `/_weft/query` are the four routes
with no OpenSearch counterpart. A client sending standard DSL cannot reach any of them by
accident, and a client reading one back knows which half of the surface it is on.

`hybrid.weights` is the precedent, an extension inside a query that exists (D-030). This is the
same move applied to a whole clause and a whole route.

**Where the honest name is OpenSearch's, it is used.** `_flush`, `_refresh`, `_forcemerge`,
`_count` and `_analyze` all do here what those names mean elsewhere — a flush is a commit, and a
refresh can only be one too, since a search already reads the live index. Prefixing those would
have been the mirror error: hiding a standard capability behind a private name.

### Why `query_string` stays a 501 while `/_weft/query` answers

They are different requests. `query_string` asks this server to read *Lucene's* language, and
weft's is not it — the two disagree about `+`, `OR` and parentheses, so translating silently runs
a query other than the one written. `/_weft/query` is a client asking for weft's language by
name, on a route that says whose language it is. The refusal was never about the capability.

### What would show this decision was wrong

**A client that has to be told the prefix exists before it finds anything.** The namespace is
cheap to add and cheap to ignore, and the failure mode is a surface where the interesting half is
invisible to everyone who did not read this file. If adopters keep asking for a feature that has
been reachable under `/_weft/` for months, the prefix is not carrying the meaning it was supposed
to carry, and the answer is documentation on the route that would have been sent anyway — not a
second name for it.

---

## D-034 — The native surface is a second spelling, not a second engine

**Date**: 2026-09-06 · **Milestone**: 30 · **Status**: accepted

### The question

A surface of weft's own was going to need three things the OpenSearch DSL cannot express: a
request that *is* a positional stream list, a per-hit pre-fusion breakdown, and a fusion depth
separate from the page size. The plan for it put those in a new package, `internal/weftapi`, with
its own request types and its own stream parser — four stream kinds named after the four signals.

Reading the code first made that plan wrong in three places at once, and the three have one cause.

### The decision

**`internal/opensearch/native.go`, mounted at `POST /{index}/_weft/search`, compiling streams with
the compiler `_search` already has.**

| The plan said | It is | Because |
| --- | --- | --- |
| a new package, `internal/weftapi` | a file in `internal/opensearch` | `compiler`, `plan`, `runSearch` and `apiError` are unexported. A new package would have had to export them or copy them, and `admin.go` already hosts `/_weft/*` in this package |
| a parser for four stream kinds | `compiler.clause`, reused | eleven leaf kinds are streams on the day they land on `_search`, and there is no second list to keep in step |
| the prefix `/_weft/v1/` | `/{index}/_weft/search` | the four routes that predate it are unversioned, there is no released tag yet, and a version segment nothing has needed is speculative |

`hybrid`'s own loop moved out to `compiler.streams` and both callers share it. That is the finding
under all three rows: **`hybrid` was always a wrapper around a stream list**, so the native surface
is that list with the wrapper taken off rather than a new thing beside it.

### What the shared compiler buys, and what it cost

It buys the property that makes this a spelling and not an engine: a clause that works on one
surface works on the other by being cut and pasted, every refusal is inherited rather than
re-listed, and a signal added to `_search` is a native stream the same day. `pkg/` changed zero
lines and `opensearch-py` still drives `weftd` unmodified.

It cost one defect, and the defect is worth the record because the plan's separate parser would
not have had it — it would have had a different one. A `streams` entry is not a scorer: a
two-token `match` is two `query.Glob` streams, so the list the fuser sees is longer than the list
the client wrote. The first cut of the breakdown truncated the scorer row to the entry count,
which put **the second entry's rank under the first entry's label, for every multi-token query,
silently**. That is D-028 arriving in a new place — the same finding that cost milestone 25 an
`anyOf` when `bool.must` met it, and the second time this repository has paid for the gap between
a clause and a stream.

`compiler.streams` now returns the grouping, and the fold takes the best rank when an entry became
several streams. Not an average: an average is a score by another name, and this engine never
compares scores across streams.

### Why the breakdown reads positions and nothing else

A breakdown that knew stream 2 was a vector would be a fifth place to edit when a sixth signal
arrives — and the whole claim being re-tested here is that there is no such place.
`TestTheNativeFusionPathKnowsNoScorer` reads `native.go`'s AST and fails if `capture` or `rankOf`
mentions a scorer constructor, which is the same lock
`TestTheFusionCodeDoesNotKnowAboutTheFourthSignal` puts on `search.go`.

### What would show this decision was wrong

**A refusal that has to differ between the two surfaces.** Sharing the compiler means sharing
every "no", and the bet is that a refusal is a property of the engine rather than of the protocol.
If a native request needs to be allowed where the same clause is refused on `_search` — or refused
where it is allowed — then the two surfaces do not share a semantics, only a parser, and the file
should become the package the plan asked for.

The weaker signal is the one D-025 registered and this makes cheaper to trip: a capability that
lands on the native route and never reaches a `go get` user. The native surface is closer to the
library's own shape than the compatibility surface is, which makes it a more tempting place to put
something that belongs in `pkg/`.

---

## D-035 — gRPC is a second module, because the dependency metric is not negotiable

**Date**: 2026-09-06 · **Milestone**: 31 · **Status**: accepted

### The question

gRPC cannot be spoken without `google.golang.org/grpc`, which brings protobuf, `x/net` and
`genproto` behind it. The founding PRD registers an operational metric — *`go list -m all` prints
this module and nothing else* — and `pkg/engine`'s `TestNoExternalDependencies` fails the build if
it does not. A gRPC surface in the root module trades a founding property for a protocol.

Two ways out were real, and they differ by a factor of five in cost.

### The decision

**A nested module at `grpc/`, with a `replace ../`.** The maintainer chose it on 2026-09-06 over
the alternative below.

The precedent is exact rather than analogous: `bench/` has quarantined bleve since milestone 5,
for a requirement that read the same way — milestone 5 needed a comparison against a real engine
and the root module was not allowed to see one. `go list -m all` at the root prints one line with
`grpc/` in the tree, checked.

The quarantine works because **Go's `internal/` is enforced by import path prefix and not by
module**. `github.com/skyoo2003/weft/grpc` sits under `github.com/skyoo2003/weft/`, so it may
import `internal/opensearch`; `bench/main.go` has relied on exactly this for `internal/eval` since
it was written.

### The alternative, and why it lost

**Hand-rolled: h2c from the standard library, plus a protobuf codec.** Go 1.24 added
`http.Protocols.SetUnencryptedHTTP2`, so cleartext HTTP/2 no longer needs `x/net/http2/h2c`, and
gRPC's framing is a five-byte prefix and two trailers. One module, zero dependencies, and entirely
in the spirit of a repository that wrote its own inverted index, its own IVF and its own DSL
parser.

It lost on what it cannot buy. **The point of gRPC is the client ecosystem**, and a hand-rolled
wire is only worth having if every stock client drives it — which is 400 to 600 lines of protocol
code whose failure mode is silent misencoding against clients this repository cannot run. Five
days against one, and the risk is permanent rather than paid once.

Registered so a later reader does not have to re-derive it: if the nested module becomes the thing
nobody remembers to build, the hand-rolled version is still available and Go 1.24 is why.

### What this costs

`cd grpc && go run ./cmd/weftg` rather than `go run ./cmd/weftg`, one more `go.sum` in the CI
cache key, and a second module that `go build ./...` does not reach. That last one is the real
cost and it has a name: **`bench/` rotted between the runs that used it**, which is why
`bench-build` exists. `grpc-build` is the same target for the same reason, and it is in CI from
the first commit rather than after the first rot.

### The one thing that is not conversion

`SearchRequest.streams` is `repeated string` — leaf clauses as JSON — and not a `oneof` over the
clause kinds. That is D-034's finding applied to the second wire: a typed union would be a second
list of what a stream can be, kept in step by hand with `compiler.clause`, and it drifts on the
first clause added to one surface and not the other. The *response* is fully typed, which is where
a client wants types.

The trade is honest and it is a trade: a gRPC client gets no compile-time help writing a query. If
that turns out to be the thing adopters trip over, the answer is a generated `oneof` **derived
from** the clause table rather than written beside it.

### What would show this decision was wrong

**A second module nobody runs.** `make grpc-build` in CI is the mitigation and not the proof; the
proof would be a release where the gRPC surface was broken for weeks and no one noticed, which is
the failure `bench/` already had once. The answer then is not a third module — it is folding the
surface into the root and paying the dependency, with the metric formally withdrawn in a decision
of its own rather than quietly.

The weaker signal: `TestTheProtoAndTheCoreDoNotDrift` starting to need exceptions. It has one
today (`index`, which lives in the URL path on HTTP and has nowhere to sit in a body-decoded
struct) and the reason is structural. A second and a third would mean the two surfaces are no
longer one request in two encodings, and the drift test would be documenting the drift instead of
preventing it.
