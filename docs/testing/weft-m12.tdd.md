# Milestone 12 — TDD record: query expressiveness

**Source plan**: `.claude/plans/weft-m12.plan.md` (not in the repository;
`.claude/` is gitignored). Its tasks are reproduced below where they are
referenced.

This file is the index into what the tests prove. It is not a substitute for
them: every claim here names the test that carries it and the command that ran
it.

**What is different about this milestone.** The RED state was not chosen by
whoever wrote the tests. Two blind trial subjects ran first
([ADOPTION §8](../ADOPTION.md)), and the arrangements that failed for *them* are
the arrangements the tests reproduce. Every RED below is a defect a subject met
before any test existed.

## 1. User journeys

Taken from the plan and from the trial tasks, rather than invented here.

1. As an external scorer author, I want an input that changes per query, so that
   my signal can personalize without weft knowing what it is.
2. As an external scorer author, I want **two** of my scorers to share one
   per-query value, without either of them disturbing a scorer I did not write.
3. As an adopter, I want the corpus-sized half of my scorer's data built once,
   not rebuilt on every search.
4. As an adopter, I want to express a phrase constraint, so that "machine
   learning" does not match "learning about machine tools".
5. As an adopter, I want a constraint to **exclude**, not merely to prefer, when
   it is used alongside the scorer I want ranking from.

Journeys 2 and 5 are the milestone's experiment. Both are what the trials
converted from a guess into a failing arrangement.

## 2. Cycle record

### RED — commit `9102f3e`

`pkg/engine/expressive_test.go`, two tests, both failing, each for the reason a
trial subject found.

```console
$ go test -race -run 'TestOneQueryTimeValueReachesTwoExternalScorers|TestAConstraintExcludesThroughFusion' ./pkg/engine/
--- FAIL: TestOneQueryTimeValueReachesTwoExternalScorers (0.00s)
    expressive_test.go:184: routing an external scorer's input through Query.Seeds moved the graph stream:
        got [{Doc:1 Score:0.5} {Doc:2 Score:0.3333333333333333}], want [{Doc:2 Score:0.8333333333333333}]
--- FAIL: TestAConstraintExcludesThroughFusion (0.00s)
    expressive_test.go:289: a document the constraint refused is in the fused result:
        [{Doc:0 Score:0.03278688524590164} {Doc:1 Score:0.016129032258064516}]
FAIL
```

Both failures are business-logic failures, not compile or setup failures:

- **First**: `Doc:1` is `tools` and `Doc:2` is `ranking`. Routing an external
  scorer's per-query value — a pivot document key — through `Query.Seeds` made the
  graph scorer traverse from somewhere else. This is the collision `engine.Query`'s
  doc comment warns about, and nothing in the tree had tested it.
- **Second**: `Doc:1` is `tools`, which holds both query terms without holding
  the phrase. The constraint scorer refused it and it is in the fused result
  anyway.

**One correction was made before this commit, and it is worth recording.** The
first test initially passed, because the per-query value used was `"coffee"` —
not any document's `Key`, so it could never collide with `Seeds`. A test that
passes because its fixture cannot express the failure is worth nothing; the value
was changed to a real document key and the corpus given a second link so both
graph streams are non-empty. The test now asserts the baseline is non-empty for
that reason:

```go
if len(wantGraph) == 0 {
    t.Fatal("the graph baseline is empty, so this test could not detect it moving")
}
```

### GREEN — commit `6f0310f`

```console
$ go test -race -run 'TestOneQueryTimeValueReachesTwoExternalScorers|TestAConstraintExcludesThroughFusion' -v ./pkg/engine/
=== RUN   TestOneQueryTimeValueReachesTwoExternalScorers
--- PASS: TestOneQueryTimeValueReachesTwoExternalScorers (0.00s)
=== RUN   TestAConstraintExcludesThroughFusion
--- PASS: TestAConstraintExcludesThroughFusion (0.00s)
PASS
ok      github.com/skyoo2003/weft/pkg/engine    1.518s
```

**No production code changed to get here**, which is the milestone's result
rather than an omission. The fixes are arrangements available to any caller:
binding the per-query value at scorer construction, and passing a `Fuser` that
reads the last stream as a restriction.

**Both traps stay asserted rather than being deleted with the failure.** The
`Seeds` arrangement is still exercised and still required to collide;
`fusion.Fuse` is still required to let the refused document through. If either
stops being true, a doc comment written this milestone has gone stale and a test
says so:

```go
if sameStream(seededGraph, wantGraph) {
    t.Fatal("Query.Seeds no longer collides with the graph scorer — the warning in
             engine.Query's doc comment, and the reason to bind at construction,
             would both be stale")
}
```

### Documentation repayment, and `ExampleFuser`

The four defects in [ADOPTION §8.1](../ADOPTION.md) are repaid in prose plus one
Example. `ExampleFuser` is the compiling form of defect 4 and runs under the same
suite, with a deterministic output:

```text
Fuse         survey tools
restricting  survey
```

## 3. Test specification

| # | What is guaranteed | Test | Type | Result |
| --- | --- | --- | --- | --- |
| 1 | One per-query value reaches two external scorers and both change the ranking | `expressive_test.go:TestOneQueryTimeValueReachesTwoExternalScorers` | integration | PASS |
| 2 | Binding that value at construction leaves the graph scorer's stream bit-identical | same test | integration | PASS |
| 3 | Routing it through `Query.Seeds` instead **does** move the graph stream — the trap is real | same test | integration | PASS |
| 4 | A corpus-sized side store is built once, across three searches and two scorers | same test (`store.built != 1`) | integration | PASS |
| 5 | A phrase constraint wrapping the text stream discriminates within its own stream | `expressive_test.go:TestAConstraintExcludesThroughFusion` | integration | PASS |
| 6 | `fusion.Fuse` does **not** exclude what the constraint refused — union, not intersection | same test | integration | PASS |
| 7 | A `Fuser` reading the last stream as a restriction does exclude it | same test | integration | PASS |
| 8 | The whole arrangement compiles from outside the module | `package engine_test` on the file | compile | PASS |
| 9 | The exported API did not move | `TestEngineAPISurface`, `TestPublicAPISurface` via `make arch` | golden | PASS |
| 10 | Ranking quality did not move | `make eval` — 0.5826 / 0.6211 | measurement | PASS |

Guarantee 8 is mechanical rather than asserted: `engine_test` is a separate
package, so a test that reached for something unexported would not build. That is
the same device `adoption_test.go` uses and the reason both files live there.

## 4. Coverage and known gaps

No coverage threshold is claimed for this milestone, and the reason is that
**nothing was implemented**. The `pkg/engine` diff is 38 added comment lines and
one removed; there is no new branch, statement or function under `pkg/` for a
coverage number to describe. `make all` runs the full suite with `-race` and
passes.

Gaps, stated rather than left to be inferred:

1. **The wrapping shape is untested for cost.** Guarantee 5 pins that wrapping
   works, not that it is cheaper than sweeping the corpus. The claim in
   `Posting`'s new doc comment that wrapping bounds the decode is an argument from
   the shape of the loop, not a measurement.
2. **`restrictFuse` is a test-local helper, not shipped API.** It is deliberately
   not exported — [D-021](../DECISIONS.md) is the decision that a caller writes
   their own — so nothing guards an adopter who gets the stream order wrong.
3. **Neither test uses a committed index.** Both build in memory.
   `adoption_test.go` carries the restart case for a side store, and nothing here
   re-asks it.
4. **The trials are one session each**, agents rather than people, and the reading
   boundary was self-reported. [ADOPTION §4 and §8.4](../ADOPTION.md) carry that in
   full; it is inherited here unchanged.

## 5. Merge evidence

If the checkpoint commits are squashed, this is the record:

- **RED** `9102f3e` — two reproducers added, both failing, each reproducing a
  blocker a blind trial subject met first.
- **GREEN** `6f0310f` — both passing, with no production change; the traps remain
  asserted.
- **Repayment** — four documentation defects closed in `doc.go`, `index.go`,
  `search.go`, `README.md`, `FORMAT.md` and `ADOPTION.md` §2.1, plus
  `ExampleFuser`.
- **Invariants** — golden API 0 lines, `pkg/fusion` 0 lines, `pkg/scorer/*` 0
  lines, nDCG@10 unchanged at 0.5826 / 0.6211.
