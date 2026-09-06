# Adoption — how milestone 6's numbers are produced

Milestone 6's outcome is a claim, not a feature:

> An external Go developer can add their own signal from the documentation and examples alone.

Nothing is built to make that true. It is measured, and this file fixes how, before the measurement exists.

[EVAL.md](EVAL.md) does the same job for milestone 4's nDCG and [PERF.md](PERF.md) for milestone 5's latency, for the same reason: a judgment rule written after the result is not a rule.

## 1. What is being measured, and what would falsify it

Every scorer weft ships — text, vector, graph, recency — reads a field `engine.Document` already has. None has ever had to answer where an outsider's own data lives.

Both carrier types are closed. `Document` has five fields, `Query` has three, and neither has a map, an `any`, or an extension point. `pkg/engine/doc.go` told a fifth scorer to "add a field here" — an instruction available only to whoever owns the repository. That is the state the trial ran against; §6 is what it found, and the sentence has since been replaced.

Two predictions were registered before anything was measured.

**Prediction A — the documented extension path is a fork for an outsider.** A signal needing per-document data weft does not carry cannot be added the way `doc.go` describes.

**Prediction B — the only implementation an outsider can copy is the shape milestone 5 measured as the wall.** `scorer/recency` is the 99-line proof that a fourth signal is cheap, and it sweeps every `DocID` calling `Index.Doc`, which decodes a whole record each time. [FINDINGS milestone 5 §3.2](FINDINGS.md) found that decode to be where throughput collapses under concurrency. An outsider copying the exemplar inherits it, and no document says so.

### 1.1 Prediction A was downgraded before the trial

`pkg/engine/adoption_test.go` asked the question directly, from a package that can reach nothing weft does not export. **The public API is sufficient.**

`Index.Resolve` and `Index.Doc` are a real join: a caller keeps its own store keyed by `Key`, resolves each key to a `DocID`, and returns candidates like any other scorer. The five-scorer `Search` call is the four-scorer call with one more slice element, and `pkg/fusion` needs no change. A side store also survives a restart — after `Commit` and `Open`, every key still names the document it named before.

So prediction A moves from **"an outsider must fork"** to **"an outsider must invent a pattern nothing documents"**, plus an undocumented cost: `Commit` carries documents and knows of no other data, so the side store is rebuilt on every `Open`.

That verdict is the answer to trial task A. **This file is consequently outside the boundary in §2.1** — a trial that can read its own answer measures nothing. The tests are plain `Test` functions rather than `Example`s for the same reason.

### 1.2 What would falsify the milestone's claim

A blocker that no document could have closed.

If a subject cannot produce a working signal without a new exported name, a new `Document` field or a new `Query` field, the claim is false for that class of signal and the milestone says so. Anything a sentence or an example would have prevented is a documentation defect, which is what this milestone exists to find.

## 2. The instrument

A subject with no prior knowledge of the tree implements a fifth signal using only what a real adopter would have, and records every point at which it was blocked.

### 2.1 The boundary of "documentation"

| In | Out |
| --- | --- |
| every `.md` in the repository except the two on the right | this file, and `docs/testing/*.tdd.md` |
| `go doc` output for any package, and what pkg.go.dev renders from it | every `.go` under `pkg/`, `internal/`, `cmd/`, `bench/`, and `pkg/engine/testdata/*` |
| everything under `examples/` | the git history, the PRD, and `.claude/plans/` |

`examples/` is in because its name says it is documentation. `go doc` is in because pkg.go.dev renders exactly that, and it is what a `go get` user reads.

The two excluded documents are excluded for one reason: both record the answer. This file states the verdict on prediction A in §1.1; the TDD evidence states it again with the tests that produced it. Neither is something an adopter reads to add a scorer.

**Corrected 2026-08-26.** The `go doc` row said "including rendered Examples" from milestone 6 until the milestone 12 trials, and that half was false. `go doc` renders no Examples at all — `go doc -all ./pkg/engine` prints `ExampleScorer` only where a doc comment mentions it, and `go doc ./pkg/engine ExampleScorer` answers `no symbol ExampleScorer in package`. Only pkg.go.dev renders them. Task C's subject established this by trying three invocations. §8 carries what it costs, which is more than a wording fix: milestone 6 repaid its worst defect with an Example.

**Opening anything in the right-hand column is not forbidden — it is recorded.** Each one is a blocker, counted with the file it was in and what was being looked for. An outsider *can* read the source; what matters is whether the documentation made it unnecessary.

### 2.2 The enforcement is self-reported, and that is a known weakness

Nothing mechanically prevents the subject from reading `pkg/engine/index.go`. There is no sandbox, only an instruction and a requirement to declare.

A subject that reads source and does not say so produces a result that looks better than it is, and this file cannot detect it. Registered rather than papered over.

### 2.3 The two tasks

Two, because the closed types are two and each demands a different detour.

| Task | Signal | Tests |
| --- | --- | --- |
| **A** | popularity — an external view count per document key | whether the `Document` side has a path for data it cannot hold |
| **B** | per-query geo — input at query time | whether the `Query` side has one. `Query` is passed by value with three fixed fields |

Both are small on purpose: the target is under the 100-line figure `TestFourthScorerIsUnderOneHundredLines` publishes for `scorer/recency`, so size stays comparable to the milestone 1 measurement.

### 2.4 What is recorded

```text
weft adoption trial   task=…   subject=…   date=…
  blockers    docs-closable N   code-required N   source-opened N (files: …)
  size        impl … lines (budget 100)   call-site … lines   pkg/ diff … lines
  time        … to first correct ranking
  verdict     possible from documentation alone / not — and what decided it
```

Every blocker records: what was being attempted, which document was read before getting stuck, how it was resolved, and its class.

## 3. Judgment rules — fixed before the numbers exist

**Blocker classification.** A blocker is **docs-closable** when the public API already permits what the subject wanted and a sentence or an example would have told them so. It is **code-required** when no arrangement of the current exported API produces the behaviour. The class is decided by attempting the API arrangement, not by how hard it felt.

**Five pass lines, all registered before the trial ran.**

1. The trial runs for both tasks and every blocker is published. The output is the blocker list, not a pass or a fail.
2. Blockers are classified, and **code-required ones are not fixed in this milestone**. They are named, costed, and carried forward.
3. The README does not lie: its status table matches the PRD milestone table row for row, and the extension snippet points at a file that compiles.
4. `v0.1.0` is tagged, and `GOPROXY=proxy.golang.org go list -m github.com/skyoo2003/weft@v0.1.0` answers.
5. `pkg/fusion` diff is 0 lines. Any `pkg/` diff at all is the milestone's price tag and its line count goes in FINDINGS.

**Zero blockers is a result, not a pass.** If a task produces none, the record says so and a harder task is run in the same session — a trial that measures nothing has not measured that the documentation is good.

## 4. What this instrument cannot see

Registered here rather than discovered later in a favourable reading.

- **The subject is an agent, not a person.** It reads documentation faster and more completely than a human will. If it is blocked, a person certainly is; the reverse does not follow. **This is a lower bound**, and the PRD's "zero user interviews" risk is not discharged by it.
- **Discovery and motivation are invisible.** Whether an external developer would find weft, and having found it would depend on it, is on no axis measured here.
- **One session per task.** No variance is measured — the debt milestone 5 §4.5 named for itself, inherited.
- **The subject knows it is being measured**, which no arrangement here removes.

## 5. Reproducing

```bash
# The workspace: documentation is read from a checkout, code is written outside it.
mkdir -p /tmp/weft-trial-a && cd /tmp/weft-trial-a
cat > go.mod <<'EOF'
module trial

go 1.26

require github.com/skyoo2003/weft v0.0.0
replace github.com/skyoo2003/weft => /path/to/weft
EOF

# What the subject is allowed to read, and the one command that shows it.
go doc -all github.com/skyoo2003/weft/pkg/engine
go doc -all github.com/skyoo2003/weft/pkg/fusion

# The verification the subject has to reach on its own.
go run .
```

## 6. Results

Two trials, 2026-08-19, one session each, subject an agent with no prior sight of the tree.

```text
weft adoption trial   task=A (popularity)   subject=agent   2026-08-19
  blockers    docs-closable 1   code-required 0   source-opened 0
  size        impl 31 lines (budget 100)   call-site 8 lines   pkg/ diff 0 lines
  time        ~4.5 min to first correct ranking
  verdict     possible from documentation alone

weft adoption trial   task=B (per-query geo)   subject=agent   2026-08-19
  blockers    docs-closable 2   code-required 0   source-opened 0
  size        impl 76 lines (budget 100)   call-site 1 line    pkg/ diff 0 lines
  time        ~4 min to first correct ranking
  verdict     possible from documentation alone
```

**The claim holds.** Both subjects produced a working fifth signal without reading a single `.go` file, without modifying weft, and inside the 100-line figure. Neither needed a new exported name, a new `Document` field or a new `Query` field, so under §3 there are **zero code-required blockers** and nothing is carried forward as an API gap.

**Three documentation defects were found, and one was named identically by both subjects, who could not see each other's work.**

| # | Defect | Found by |
| --- | --- | --- |
| 1 | `engine.Document`'s "adding a fifth scorer means adding a field here" sends an outsider to a door they cannot open. The real answer — keep your own table keyed by `Key`, join with `Index.Resolve` — is nowhere stated, though every part of it is documented separately | **both, independently** |
| 2 | `engine.Query`'s "carries every scorer's input in one value" is true of the four in-tree scorers and false of the fifth the README invites you to write. Nothing says how a per-query input reaches an external scorer | B |
| 3 | `Search`'s `k` is both the per-scorer request size and the result-set size, and nothing says so | A |

All three are documentation defects.

### 6.1 Defect 1 is the one that matters

Two subjects, two different tasks, no shared context, and both singled out the same sentence. A called it "actively points the wrong way" and nominated it as *the single sentence I would change*. B said it "points an external adopter at a door they cannot open".

Why a sentence that is *true* is the worst defect here: `doc.go` is correct — for weft's own scorers, a fifth signal does mean a new `Document` field. It is written from inside the repository, and it is the first thing an outsider reads when they ask where their data goes.

**A document that is accurate for the maintainer and misleading for the reader is not a small error**, and no test catches it. `TestEngineAPISurfaceIsUnchanged` records declarations, not the prose above them.

### 6.2 What each subject had to establish by experiment

Both are things one clause would have prevented.

- **B wrote a throwaway probe** to find out whether `Search` passes its context to `Candidates` unmodified. It does. Nothing documents it, and B noted the silence read as deliberate in a tree that documents NaN ordering in `TopK`.
- **A had to reshape its demo** to discover that fusing at the display depth structurally outvotes a new orthogonal signal. `viral`, at 1.2M views, sat at rank 5 until the fusion depth was raised above the display cut, whereupon it reached rank 3.

### 6.3 What the trials confirmed rather than found

- **The architecture claim held literally.** Both subjects added the fifth scorer as one more element in a slice, with no change to `fusion`, `engine`, or the `Search` call. A: *"the architectural claim is not just documented, it is pre-answered"*.
- **Rank-only fusion paid off where it was designed to.** A returned raw view counts — 1,200,000 — alongside cosine similarities and normalized nothing, because `Fuse` never reads `Candidate.Score`. That is the milestone 1 design claim being used by someone who did not know it was a claim.
- **Equal-weight RRF buried the new signal in both trials**, exactly as [FINDINGS milestone 4 §7](FINDINGS.md) says it does. B declined to reach for `FuseWeighted` because the README is emphatic that unmeasured weights are unearned — the documentation successfully talked a user out of a footgun.
- **The two subjects resolved `Key` to `DocID` at different times** — A once at construction, B on every query — and neither documented path told them which. Both work; the choice is a real one nobody has written down.

### 6.4 What this result is not

Every limit in §4 stands, and the first binds hardest: **the subjects were agents.** Four minutes to a working scorer is a lower bound produced by a reader that consumes `go doc -all` in one pass and never gets bored. A human meeting defect 1 does not necessarily recover by reading `Index.Resolve`'s godoc and inferring a join. The PRD's "zero user interviews" risk is untouched.

Both subjects also reported honestly under a self-reported boundary (§2.2) — the outcome this design hoped for and cannot verify.

## 7. Milestone 12 — the same instrument, a harder question

Milestone 12's outcome is also a claim:

> An external scorer receives its own query-time input without modifying weft, and a text-side constraint — a phrase, a field restriction — can be expressed.

The PRD adds a third sentence: *milestone 6's defects 2 and 3 move from documentation repayment to code repayment.* That is a **prediction, not an outcome**, and this section registers the opposite prediction before either trial runs.

### 7.1 The prediction registered before the trials

**Prediction C/D — both tasks complete with zero code-required blockers.** Three grounds, all already in the repository:

1. **The precedent has already passed.** Task B in §6 was exactly a query-time-input task: 76 implementation lines, a one-line call site, a `pkg/` diff of 0.
2. **The prose repayment is already in three places.** `engine.Query`'s doc comment names constructor binding and prices the context alternative, `Search`'s names the `k` duality, and the README repeats both under *Adding a scorer*. Defect 2 was "nothing says how", so the defect as stated is gone.
3. **Scorer-wrapping composition already exists.** `graph.New(ix, txt)` is a scorer that takes a scorer. A constraint can wrap the text stream, and the wrapper picks its own inner depth, so it does not hit the `k` duality either.

If this prediction holds, **the PRD's third sentence is wrong**, and the milestone publishes it as wrong. If it fails, the blocker that failed it is what gets repaid. Either way the result is the blocker list.

**This is why this file remains outside the boundary in §2.1.** It now records a prediction as well as an answer.

### 7.2 Zero blockers here would not be a pass, so the tasks are harder

§3's *"zero blockers is a result, not a pass"* binds hardest in a round that predicts zero. Re-running task B would measure a documentation edit, not a new question, so both tasks below are past the shape §6 already cleared.

**Task C — query-time input, harder along two axes.**

- **The scorer is expensive to construct.** Task B's geo scorer was small enough that "build one per query" cost an allocation. Task C's scorer must hold a side store the size of the corpus. The question is whether the store (once) and the query input (every time) can be separated.
- **Two external scorers share one query-time input.** `engine.Query`'s doc comment warns that two scorers sharing `Seeds` is how one of them silently stops working. This is the arrangement where that warning would fire.

**Task D — the text-side constraint, in two parts.**

- **D-i, phrase.** Exactly these two words, in this order, adjacent.
- **D-ii, field restriction.** Match in the title only.

### 7.3 Two facts fixed here that the subjects are not told

Written down before the trials so the record distinguishes "the subject discovered it" from "the subject was told".

- **`engine.Posting` carries `Doc` and `Freq` and no position.** An exact phrase cannot be decided from postings. The only route left is the document text, which means one record decode per candidate — the decode [FINDINGS milestone 5 §3.2](FINDINGS.md) named as the throughput wall.
- **The index has no concept of a field.** `engine.Document` has one `Text`, and the tokenizer puts all of it into one term space. Any field restriction has to be a convention on the adopter's side: a side store, a second index, or a term prefix.

  *Which* term space it is has been the adopter's since milestone 13 — `engine.WithTokenizer` replaces the split at index and query time together ([D-022](DECISIONS.md)). That gives the index no field concept, so the sentence above stands. Per-field term spaces are format v5 ([FORMAT §8](FORMAT.md)).

Whether a subject finds either fact is half of task D.

Both tasks keep §2.3's limits: under 100 implementation lines, zero source files opened, boundary unchanged.

### 7.4 Two judgment rules added to §3

§3's blocker classification is inherited exactly. Two rules are added:

- **Added rule 1 — code-required blockers are fixed in this milestone.** This reverses §3's pass line 2, which milestone 6 wrote and [D-010](DECISIONS.md) defended. Grounds: milestone 6 did not know whether an extension point was needed, and milestone 12 would be fixing one the trial has named. The reversal is recorded in [D-021](DECISIONS.md), not decided here.
- **Added rule 2 — a blocker that requires an on-disk format change is not fixed.** A position index and a field index are both of that kind. They are named, costed and carried forward.

### 7.5 The four readings, fixed before the numbers exist

| # | Condition | Reading |
| --- | --- | --- |
| 1 | C and D both produce zero code-required blockers | Defects 2 and 3 were documentation defects. **The PRD's "moves to code repayment" is published as wrong**, and the trial output is pinned as tests and an Example |
| 2 | C produces a code-required blocker | Repay at the rung the subject was actually blocked at. Any golden-file spend is recorded in FINDINGS **first** |
| 3 | D produces a code-required blocker that does not need a format change | Repay it |
| 4 | D needs a format change | Carry forward. **The outcome clause "a field restriction can be expressed" is judged unmet**, with what forced it |

Readings 1 and 4 can both be true. Then both are published.

### 7.6 What is recorded

§2.4's block, unchanged, with `task=C` and `task=D`. The self-reported enforcement of §2.2 is inherited without improvement, and so is every limit in §4.

## 8. Milestone 12 results

Two trials, 2026-08-26, one session each, two agents with no prior sight of the tree and no sight of each other's task.

```text
weft adoption trial   task=C (per-query viewer, two scorers, corpus-sized stores)   subject=agent   2026-08-26
  blockers    docs-closable 1   code-required 0   source-opened 0
  size        impl 84 lines (budget 100)   call-site 8 lines   pkg/ diff 0 lines
  time        one pass — first build and first run both succeeded
  verdict     possible from documentation alone

weft adoption trial   task=D (phrase and field restriction)   subject=agent   2026-08-26
  blockers    docs-closable 3   code-required 0   source-opened 0
  size        impl 86 lines (budget 100)   call-site 8 lines   pkg/ diff 0 lines
  time        one pass to first correct ranking
  verdict     D-i expressible, D-ii expressible — each needs a custom Fuser as well as a custom Scorer
```

**The §7.1 prediction holds: zero code-required blockers in both tasks.** Neither subject needed a new exported name, a new `Document` field or a new `Query` field, neither opened a `.go` file, and both came in under the 100-line budget.

Under §7.5 this is **reading 1**: the PRD's *"milestone 6's defects 2 and 3 move from documentation repayment to code repayment"* is **wrong**, and `docs/FINDINGS.md` milestone 12 publishes it as wrong.

**Reading 4 did not fire.** A field restriction is expressible without touching the on-disk format — a caller-held title table joined through `Index.Resolve` — so the outcome clause "a field restriction can be expressed" is **met**. The format-change carry-forward this section was prepared to make is not needed.

### 8.1 Four documentation defects, and the first is the one that matters

| # | Defect | Found by |
| --- | --- | --- |
| 4 | `fusion.Fuse` is a union of votes, not an intersection. A constraint expressed as a `Scorer` is a ranking preference, and every document it refused returns to the fused result on another scorer's vote — silently, with no error. Nothing said so | D |
| 5 | `Posting` has `Doc` and `Freq` and no position, so a phrase can only be decided by decoding document text. Nothing said so, and nothing said the cheap shape is to wrap another scorer rather than sweep every `DocID` | D |
| 6 | The index has no field concept, so a field restriction is a caller-side convention. Nothing said so | D |
| 7 | `go doc` renders no Examples, so the answer milestone 6 put in `ExampleScorer` is invisible to a terminal reader. §2.1 asserted the opposite | C |

**Defect 4 is worth the round.** Both trials in §6 and both tasks here produced signals that were *additive* — a view count, a distance, a topic match — and rank fusion is built for exactly that. The first constraint anyone writes is subtractive, and the architecture that makes an additive signal free makes a subtractive one silently wrong.

The subject found it only because it was asked to run the arrangement it had not run: its own demonstration had each constraint as the sole scorer, where the trap cannot appear.

**That is a limit of this instrument worth recording.** A task that says "make this work" is completed by the arrangement in which it works. Neither §6 nor this section would have found defect 4 without a follow-on asking for the composition an adopter would actually ship, and nothing in §2.3 required one.

### 8.2 What the subjects had to establish by experiment

- **D ran the fused arrangement and watched the exclusion fail.** Its report: `REAPPEARED phrase-nearmiss-order`, and the same for the other two near-misses and all three body-only documents. It then wrote a 22-line `Fuser` that reads the last stream as a restriction, and the exclusion held. It classified this docs-closable by the §3 rule — the exported API already permitted it — which is the correct call, and is why nothing was added to `Query`.
- **C tried three `go doc` invocations** looking for the Example the README names, and concluded it is not retrievable that way. It did not fall back to reading source; the prose in `Document`'s and `Query`'s doc comments was sufficient on its own.
- **D's phrase scorer swept the whole corpus** — every `DocID`, decode, tokenize, scan — because nothing told it the wrapping shape. It priced this itself: *"O(documents × doc length), no caching, redone every query."*

### 8.3 What milestone 6's repayment actually bought

[D-010](DECISIONS.md) registered the signal that would show it was wrong: *the same question keeps being asked after `ExampleScorer` and three sentences are in place.* The answer is split.

**The question did not come back.** Task C is the harder restatement of task B and it produced no blocker on the query-time input at all — the subject named the two doc comments and built from them first try. The three sentences milestone 6 wrote did their job.

**The Example did not.** `go doc` never rendered it (defect 7), so for a terminal reader it has been dead since it was written, and the prose carried the whole load alone. D-010's judgment that answers belong where `go doc` renders them was right; the belief that an `Example` is such a place was wrong, and it took a milestone to notice.

### 8.4 What this result is not

Every limit in §4 stands unchanged, and §8.1 adds one: **the instrument measures the arrangement the task names.** Both subjects were agents, both knew they were measured, and the boundary was self-reported. One session per task, so no variance is measured.

Neither subject was asked to make its scorer fast, and D's corpus sweep would not survive contact with a real corpus. That the documentation now names the wrapping shape is a repayment of defect 5, not evidence that anyone would have found it.
