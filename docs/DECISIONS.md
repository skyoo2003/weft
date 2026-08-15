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
|---|---|
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
