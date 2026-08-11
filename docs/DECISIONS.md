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
|---|---|
| `maxDocID` | last document id in the block — decides whether the block can be skipped |
| `maxTF` | highest term frequency in the block |
| `minDocLen` | shortest document length in the block |

That is everything block-max WAND (Ding & Suel, 2011) requires. Nothing reads these fields yet; milestone 5 starts reading them.

### Why `maxTF` + `minDocLen` rather than `maxScore`

The easiest part of this decision to get wrong. A BM25 term contribution is

```
IDF(q) × f·(k1+1) / (f + k1·(1 - b + b·|D|/avgdl))
```

`IDF` depends on `N` and `n(q)`; the normalization term depends on `avgdl`. All three are collection-wide and change on every document added, so a finished `maxScore` written into a block goes stale on the next commit with nothing to signal it.

`maxTF` and `minDocLen` are segment-local and immutable. The bracketed term increases in `f` and decreases in `|D|`, so the pair yields the block's true ceiling, computed at query time against the current `N` and `avgdl`. Accurate, never stale, and no floats in the file.

### Decision

1. **No cursor interface in milestone 2.** It is a performance interface with no performance measurement behind it — that is milestone 5's work. Designing it now means designing against a guess.
2. **Postings format is block-structured from the start**, carrying `maxDocID`, `maxTF` and `minDocLen` per block.

### Rationale — the costs are asymmetric

| Deferred | Cost of deferring |
|---|---|
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
|---|---|---|
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

One comment edit: `pkg/engine/topk.go` records the size trigger but not the cursor-interface dependency, which makes it the one marker that could be repaid at the wrong time. Everything else stands as written.
