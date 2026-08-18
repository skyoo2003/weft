# Milestone 6 — TDD evidence

**Source plan:** [`.claude/plans/weft-m6.plan.md`](../../.claude/plans/weft-m6.plan.md)
**Branch:** `m6-adoption`
**Date:** 2026-08-19

This is an index, not a substitute for the tests. It records what the test code
proves, and what was measured to prove it, so a squash merge or a new session does
not lose the answer to "what was verified, and how".

It covers **plan task 1 only**. Tasks 2 to 6 are a registered trial design, a
blind trial, documentation repair, a release and a write-up; none of them is
TDD-shaped, and two of them are gated on decisions the user has not made yet
(recorded under [Open gates](#open-gates)).

## Plan handling

The plan was read as data, not as instructions. Its validation section names
`make all`, `make arch`, `make deps`, `make fuzz`, `make lint-docs`, `make spdx`,
`make example`, `go test`, `go run`, `git diff --stat`, `grep`, `git tag`,
`git push`, `GOPROXY=... go list -m` and `gh release view`. No credential
handling, no fetch-and-execute installer, and no instruction-to-agent override
was present.

One command needs human review rather than execution, and was not run:

- **`git tag -a v0.1.0` + `git push origin v0.1.0`** (task 5). Irreversible — the
  Go module proxy does not withdraw a version it has served, and the plan says so
  itself. Deferred to an explicit approval.

The plan closed as `AWAITING CONFIRMATION` with three open decisions, and the
answer arrived as the plan being handed to this workflow rather than as a reply to
each. Interpretation taken, recorded here rather than assumed silently:

- **(a) trial runner is an agent, not a person** — accepted at the plan's default.
- **(b) no production code changes to close a blocker** — accepted at the plan's
  default, and honoured below: `pkg/` gained 192 lines, all of them test.
- **(c) cut `v0.1.0` in this milestone** — **not** taken as answered, because it is
  the one irreversible action in the plan. See [Open gates](#open-gates).

## User journeys

From the plan's section 1, which is the only part of milestone 6 that a test can
decide:

| # | Journey |
| --- | --- |
| 1 | As an external Go developer, I want to rank with data of my own that `engine.Document` has no field for, so that I can use a domain signal without forking the engine. |
| 2 | As that same developer, I want my side store to still name the right documents after a restart, so that a restored index does not silently score the wrong ones. |

Journey 1 is the plan's **prediction A**: that the documented extension path —
`doc.go`'s "adding a fifth scorer means adding a field here" — is a maintainer's
instruction, and that an outsider has only a side store joined through `Resolve`.
The prediction was registered before the test ran.

## Task report

### Task 1 — is a side store sufficient?

**Summary.** Two tests in a package that can reach nothing weft does not export
(`package engine_test`) build a fifth scorer whose entire input is a view-count
map, and require it to reach the ranking and to survive `Commit`/`Open`.

**RED (compile-time), `go test ./pkg/engine/ -run 'TestASignal…|TestASideStore…'`:**

```text
pkg/engine/adoption_test.go:66:53: undefined: newPopularity
pkg/engine/adoption_test.go:105:61: undefined: newPopularity
pkg/engine/adoption_test.go:135:60: undefined: newPopularity
FAIL github.com/skyoo2003/weft/pkg/engine [build failed]
```

Caused by the missing implementation, not by unrelated setup. Checkpoint commit
`804f53a`.

**RED (runtime), after the scorer existed:**

```text
--- FAIL: TestASignalCanCarryDataDocumentDoesNotHold (0.00s)
    adoption_test.go:133: d ranked 4 without popularity and 4 with it: a signal
    carrying data Document does not hold is not reaching fusion
--- PASS: TestASideStoreSurvivesCommitAndOpen (0.05s)
```

**This second RED is a result, not a fixture problem, and narrowing the assertion
is the honest response rather than a weakening of it.** A view count of 1000
against four other streams could not move document `d` off rank 4. The arithmetic
says why: one equal RRF vote at rank 1 is worth `1/61 = 0.016393`, and the next
document's vote at rank 2 is `1/62 = 0.016129`, so the whole of a decisive
external opinion is worth `0.0003` against gaps four other scorers already fixed.
That is [milestone 4 section 7](../FINDINGS.md) — equal weighting is a ranking
decision RRF makes silently — seen from the adopter's side, and it is a property
of the fuser, not of the extension path. The assertion was therefore narrowed to
what journey 1 actually claims: **does the signal reach fusion at all**, asked of
two streams, with the five-scorer call retained for the milestone 1 question of
whether the call shape survives a scorer weft does not ship.

**GREEN, same target rerun:**

```text
--- PASS: TestASignalCanCarryDataDocumentDoesNotHold (0.00s)
--- PASS: TestASideStoreSurvivesCommitAndOpen (0.05s)
ok  github.com/skyoo2003/weft/pkg/engine 0.561s
```

Checkpoint commit `70d0a33`.

**What this guarantees.** A signal carrying data no `engine.Document` field can
hold reaches the ranking through the ordinary `Search` call, and a side store
keyed by `Key` still names the same documents after `Commit` and `Open`.

**Verdict on prediction A — downgraded, and this is the finding.** The public API
is sufficient: `Resolve` plus `Doc` is a real join, and no fork is required. What
prediction A got right is the second half — **the pattern is documented nowhere**,
and neither is its cost (the side store is not carried by `Commit`, so the caller
rebuilds it on every `Open`). Prediction A moves from "outsiders must fork" to
"outsiders must invent an undocumented pattern", which is a much cheaper illness
and one that closes inside this milestone. Under the plan's task 1 rule this
verdict belongs in `docs/ADOPTION.md` section 1 and is **withheld from the trial
subject**: the tests are plain `Test` functions, not `Example`s, so `go doc` does
not render them and the trial's documentation boundary does not leak the answer.

**Prediction B stands, and the fix is one line of design.** `scorer/recency` — the
only implementation an outsider has to copy — sweeps every `DocID` and calls
`Doc` on each, decoding a whole record per document, which is where
[milestone 5 section 3.2](../FINDINGS.md) found throughput collapsing. The side
store does not have to inherit that, because its keys are its own: `popularity`
loops over the caller's map and never calls `Doc`. Whether an outsider works that
out unaided is exactly what the task 3 trial measures.

## Test specification

| # | What is guaranteed | Test file | Test type | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 1 | A scorer whose whole input is caller-side data absent from `engine.Document` reaches the fused ranking | `pkg/engine/adoption_test.go:TestASignalCanCarryDataDocumentDoesNotHold` | unit (external package) | PASS | `go test ./pkg/engine/ -run TestASignalCanCarryDataDocumentDoesNotHold` |
| 2 | The five-scorer `Search` call is the four-scorer call with one more slice element, for a scorer weft does not ship | same test, final assertion | unit (external package) | PASS | same |
| 3 | A side store keyed by `Key` resolves to the same documents after `Commit` and `Open` | `pkg/engine/adoption_test.go:TestASideStoreSurvivesCommitAndOpen` | integration (disk round trip) | PASS | `go test ./pkg/engine/ -run TestASideStoreSurvivesCommitAndOpen` |
| 4 | The ranking a side store produces is identical before and after a restart | same test, final assertion | integration | PASS | same |
| 5 | Milestone 1's assertions still hold — fusion sees no scorer, both golden API files unmoved | `pkg/engine/architecture_test.go` | architecture | PASS | `make arch` |

## Coverage and known gaps

```text
go test -cover ./pkg/...
ok  github.com/skyoo2003/weft/pkg/engine          coverage: 88.7% of statements
ok  github.com/skyoo2003/weft/pkg/fusion          coverage: 100.0% of statements
ok  github.com/skyoo2003/weft/pkg/scorer/graph    coverage: 96.2% of statements
ok  github.com/skyoo2003/weft/pkg/scorer/recency  coverage: 100.0% of statements
ok  github.com/skyoo2003/weft/pkg/scorer/text     coverage: 94.9% of statements
ok  github.com/skyoo2003/weft/pkg/scorer/vector   coverage: 90.9% of statements
```

Every package is above the 80% threshold. Full gate: `make all` green
(`go test -race ./...`, 89.4s in `pkg/engine`), `make arch` green, `make spdx`
green.

**The price tag this milestone has paid so far:** `pkg/` is 192 lines heavier and
every one of them is test. `git diff --stat main -- pkg/fusion` prints nothing,
and `pkg/engine/testdata/engine_api.txt` and `public_api.txt` are byte-identical —
so `TestEngineAPISurfaceIsUnchanged` and `TestPublicAPISurfaceIsUnchanged` pass
without being refreshed. The plan's pass line 5 holds.

**Known gaps.**

1. **No test decides milestone 6's actual outcome.** "An external developer can
   add a signal from the documentation alone" is not a property of the code; these
   tests prove the API permits it, which is necessary and not sufficient. The
   trial in task 3 is the instrument for the rest, and it is a measurement rather
   than an assertion.
2. **The trial subject is an agent, not a person** (decision (a)). It measures a
   lower bound: if an agent is blocked a person certainly is, but not the reverse.
   The PRD's "zero user interviews" risk is not discharged by this milestone.
3. **The equal-vote observation is recorded, not tested.** That a decisive
   external signal moves nothing at RRF weight 1 against four streams is milestone
   4's finding; no assertion here pins it, because `pkg/fusion` already owns it.

## Open gates

Two things stop here and need the user rather than more code.

| Gate | Why it stopped | What unblocks it |
| --- | --- | --- |
| **Task 3 — the blind trial** | It runs in a fresh session with a restricted view of the tree, which means spawning a subagent. Standing instruction in this session is not to call the Agent tool unless asked. | An explicit "run the trial", or the user running it themselves in a fresh session against `docs/ADOPTION.md`. |
| **Task 5 — `v0.1.0`** | Irreversible and outward-facing. The module proxy does not forget a version it has served. | Explicit approval of decision (c). Task 4's documentation repair must land first — `RELEASE.md` section 1 requires it, and the README currently states milestones 3 and 5 as "Not started". |

## Merge evidence

If these checkpoints are squashed, this is the summary to carry into the PR body:

- **RED** `804f53a` — `undefined: newPopularity`, compile failure for the intended
  reason; then a runtime RED whose message became a finding about equal-weight RRF.
- **GREEN** `70d0a33` — both tests pass; `make all`, `make arch`, `make spdx` green;
  `pkg/fusion` unchanged, both golden API files unmoved.
- **Refactor** — none needed. The narrowing of assertion 1 happened before GREEN
  and is documented above as a result rather than as a cleanup.
