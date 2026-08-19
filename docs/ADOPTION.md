# Adoption — how milestone 6's numbers are produced

Milestone 6's outcome is a claim, not a feature:

> An external Go developer can add their own signal from the documentation and
> examples alone.

Nothing is built to make that true. It is measured, and this file fixes how,
before the measurement exists. [EVAL.md](EVAL.md) does the same job for milestone
4's nDCG and [PERF.md](PERF.md) for milestone 5's latency; the reason is the same
one both of those give, that a judgment rule written after the result is not a
rule.

## 1. What is being measured, and what would falsify it

Every scorer weft ships — text, vector, graph, recency — reads a field
`engine.Document` already has. None of them has ever had to answer where an
outsider's own data lives, and both carrier types are closed: `Document` has five
fields and `Query` has three, with no map, no `any`, and no extension point.
`pkg/engine/doc.go` tells a fifth scorer to "add a field here", which is an
instruction available only to whoever owns the repository.

So milestone 6 registered two predictions before measuring anything.

**Prediction A — the documented extension path is a fork for an outsider.**
A signal needing per-document data weft does not carry cannot be added the way
`doc.go` describes.

**Prediction B — the only implementation an outsider can copy is the shape
milestone 5 measured as the wall.** `scorer/recency` is the 99-line proof that a
fourth signal is cheap, and it sweeps every `DocID` calling `Index.Doc`, which
decodes a whole record each time. [FINDINGS milestone 5 §3.2](FINDINGS.md) found
that same decode to be where throughput collapses under concurrency. An outsider
copying the exemplar inherits it, and no document says so.

### 1.1 Prediction A is already downgraded, and this is why the trial cannot read this file

Before the trial, `pkg/engine/adoption_test.go` asked the question directly from a
package that can reach nothing weft does not export. **The public API is
sufficient.** `Index.Resolve` and `Index.Doc` are a real join: a caller keeps its
own store keyed by `Key`, resolves each key to a `DocID`, and returns candidates
like any other scorer. The five-scorer `Search` call is the four-scorer call with
one more slice element, and `pkg/fusion` needs no change. A side store also
survives a restart — after `Commit` and `Open`, every key still names the document
it named before, so the rebuilt store scores the right documents.

Prediction A therefore moves from **"an outsider must fork"** to **"an outsider
must invent a pattern nothing documents"**, together with a cost nothing documents
either: `Commit` carries documents and knows of no other data, so the side store
is rebuilt on every `Open`.

That verdict is the answer to trial task A. It is recorded here, and **this file
is consequently outside the boundary in §2.1** — a real adopter would not be
reading the protocol for the trial they are the subject of, and a trial that can
read its own answer measures nothing. The tests are plain `Test` functions rather
than `Example`s for the same reason: `go doc` renders Examples.

### 1.2 What would falsify the milestone's claim

A blocker that no document could have closed. If a trial subject cannot produce a
working signal without a new exported name, a new `Document` field or a new
`Query` field, the claim is false for that class of signal and the milestone says
so. Anything a sentence or an example would have prevented is a documentation
defect, which is what this milestone exists to find.

## 2. The instrument

A subject with no prior knowledge of the tree implements a fifth signal using only
what a real adopter would have, and records every point at which it was blocked.

### 2.1 The boundary of "documentation"

| In | Out |
| --- | --- |
| every `.md` in the repository except the two on the right | this file, and `docs/testing/*.tdd.md` |
| `go doc` output for any package, including rendered Examples | every `.go` under `pkg/`, `internal/`, `cmd/`, `bench/`, and `pkg/engine/testdata/*` |
| everything under `examples/` | the git history, the PRD, and `.claude/plans/` |

The two excluded documents are excluded for one reason: both record the answer.
This file states the verdict on prediction A in §1.1, and the TDD evidence states
it again with the tests that produced it. Neither is something an adopter reads to
add a scorer — one is the protocol for the trial and the other is the maintainer's
proof — so removing them costs the subject nothing a real user would have had.

`examples/` is in because its name says it is documentation. `go doc` is in
because pkg.go.dev renders exactly that and it is what a `go get` user reads.

**Opening anything in the right-hand column is not forbidden — it is recorded.**
Each one is a blocker, counted with the file it was in and what was being looked
for. That is the honest instrument: an outsider *can* read the source, and what
matters is whether the documentation made it unnecessary.

### 2.2 The enforcement is self-reported, and that is a known weakness

Nothing mechanically prevents the subject from reading `pkg/engine/index.go`.
There is no sandbox here, only an instruction and a requirement to declare. A
subject that reads source and does not say so produces a result that looks better
than it is, and this file cannot detect it. Registered rather than papered over.

### 2.3 The two tasks

Two, because the closed types are two and each demands a different detour.

**Task A — a signal carrying data `engine.Document` cannot hold.** Popularity: an
external view count per document key. Tests whether the `Document` side has a
path.

**Task B — a signal needing input at query time that `engine.Query` cannot
carry.** Tests whether the `Query` side has one. `Query` is passed by value with
three fixed fields, so anything else has to arrive some other way.

Both are small on purpose: the target is under the 100-line figure
`TestFourthScorerIsUnderOneHundredLines` publishes for `scorer/recency`, so that
size stays comparable to the milestone 1 measurement.

### 2.4 What is recorded

```text
weft adoption trial   task=…   subject=…   date=…
  blockers    docs-closable N   code-required N   source-opened N (files: …)
  size        impl … lines (budget 100)   call-site … lines   pkg/ diff … lines
  time        … to first correct ranking
  verdict     possible from documentation alone / not — and what decided it
```

Every blocker is recorded as: what was being attempted, which document was read
before getting stuck, how it was resolved, and its class.

## 3. Judgment rules — fixed before the numbers exist

**Blocker classification.** A blocker is **docs-closable** when the public API
already permits what the subject wanted and a sentence or an example would have
told them so. It is **code-required** when no arrangement of the current exported
API produces the behaviour, so a new exported name, `Document` field or `Query`
field would be needed. The class is decided by attempting the API arrangement, not
by how hard it felt.

**Pass lines, all five registered before the trial ran.**

1. The trial runs for both tasks and every blocker is published — the output is
   the blocker list, not a pass or a fail.
2. Blockers are classified, and **code-required ones are not fixed in this
   milestone**. They are named, costed, and carried forward.
3. The README does not lie: its status table matches the PRD milestone table row
   for row, and the extension snippet points at a file that compiles.
4. `v0.1.0` is tagged, and `GOPROXY=proxy.golang.org go list -m
   github.com/skyoo2003/weft@v0.1.0` answers.
5. `pkg/fusion` diff is 0 lines. Any `pkg/` diff at all is the milestone's price
   tag and its line count goes in FINDINGS.

**Zero blockers is a result, not a pass.** If a task produces none, the record
says so and a harder task is run in the same session, because a trial that
measures nothing has not measured that the documentation is good.

## 4. What this instrument cannot see

Registered here rather than discovered later in a favourable reading.

- **The subject is an agent, not a person.** It reads documentation faster and
  more completely than a human will. If it is blocked, a person certainly is; the
  reverse does not follow. **This is a lower bound.** The PRD's "zero user
  interviews" risk is not discharged by this milestone.
- **Discovery and motivation are invisible.** Whether an external developer would
  find weft, and having found it would choose to depend on it, is not on any axis
  measured here.
- **One session per task.** No variance is measured, which is the debt milestone 5
  §4.5 already named for itself and this file inherits.
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

Two trials, 2026-08-19, one session each, subject an agent with no prior sight of
the tree.

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

**The claim holds.** Both subjects produced a working fifth signal without reading
a single `.go` file under `pkg/`, `internal/`, `cmd/` or `bench/`, without
modifying weft, and inside the 100-line figure. Neither needed a new exported
name, a new `Document` field or a new `Query` field, so under §3 there are **zero
code-required blockers** and nothing is carried forward as an API gap.

**Three documentation defects were found, and one of them was named identically by
both subjects, who could not see each other's work.**

| # | Defect | Found by | Class |
| --- | --- | --- | --- |
| 1 | `engine.Document`'s "adding a fifth scorer means adding a field here" sends an outsider to a door they cannot open. The real answer — keep your own table keyed by `Key`, join with `Index.Resolve` — is nowhere stated, though every part of it is documented separately. | **both, independently** | docs |
| 2 | `engine.Query`'s "carries every scorer's input in one value" is true of the four in-tree scorers and false of the fifth the README invites you to write. Nothing says how a per-query input reaches an external scorer. | B | docs |
| 3 | `Search`'s `k` is both the per-scorer request size and the result-set size, and nothing says so. A signal orthogonal to the built-in ones is exactly the signal whose documents every other stream truncates away, so its single vote is arithmetically incapable of winning. | A | docs |

### 6.1 Defect 1 is the one that matters

Two subjects, two different tasks, no shared context, and both singled out the same
sentence. A said it "actively points the wrong way" and nominated it as *the single
sentence I would change*; B said it "points an external adopter at a door they
cannot open".

It is worth being precise about why a sentence that is *true* is the worst defect
here. `doc.go` is correct: for weft's own scorers, a fifth signal does mean a new
`Document` field. It is written from inside the repository, and it is the first
thing an outsider reads when they ask where their data goes. **A document that is
accurate for the maintainer and misleading for the reader is not a small error**,
and no test catches it — `TestEngineAPISurfaceIsUnchanged` records declarations,
not the prose above them.

### 6.2 What each subject had to establish by experiment

Both are things one clause would have prevented.

- **B wrote a throwaway probe** to find out whether `Search` passes its context to
  `Candidates` unmodified. It does. Nothing documents it, and B noted the silence
  read as deliberate in a tree that documents NaN ordering in `TopK`.
- **A had to reshape its demo** to discover that fusing at the display depth
  structurally outvotes a new orthogonal signal — `viral`, at 1.2M views, sat at
  rank 5 until the fusion depth was raised above the display cut, whereupon it
  reached rank 3.

### 6.3 What the trials confirmed rather than found

- **The architecture claim held literally.** Both subjects added the fifth scorer as
  one more element in a slice, with no change to `fusion`, `engine`, or the `Search`
  call. A: *"the architectural claim is not just documented, it is pre-answered"*.
- **Rank-only fusion paid off in the place it was designed for.** A returned raw
  view counts — 1,200,000 — alongside cosine similarities and normalized nothing,
  because `Fuse` never reads `Candidate.Score`. That is the milestone 1 design
  claim being used by someone who did not know it was a claim.
- **Equal-weight RRF buried the new signal in both trials**, exactly as
  [FINDINGS milestone 4 §7](FINDINGS.md) says it does. B declined to reach for
  `FuseWeighted` because the README is emphatic that unmeasured weights are
  unearned — the documentation successfully talked a user out of a footgun.
- **The two subjects resolved `Key` to `DocID` at different times** — A once at
  construction, B on every query — and neither documented path told them which. Both
  work; the choice is a real one nobody has written down.

### 6.4 What this result is not

Every limit in §4 stands, and the first one binds hardest here: **the subjects were
agents.** Four minutes to a working scorer is a lower bound produced by a reader
that consumes `go doc -all` in one pass and never gets bored. A human meeting
defect 1 does not necessarily recover by reading `Index.Resolve`'s godoc and
inferring a join. The PRD's "zero user interviews" risk is untouched by this
milestone.

Both subjects also reported honestly under a boundary that was self-reported
(§2.2), which is the outcome this design hoped for and cannot verify.
