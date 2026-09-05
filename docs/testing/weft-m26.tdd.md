# Milestone 26 — test record

**Scope**: `knn`, `hybrid` with positional weights, and the fourth signal (recency) over HTTP.
**Mechanical definition**: `pkg/` diff 0 lines, `internal/opensearch` fusion code 0 lines. Both held.

## The test that is the milestone

`TestAFourthSignalOverHTTPIsUnderOneHundredLines` parses `search.go` with `go/parser` and
measures the clause function per signal. It is deliberately the same shape as milestone 1's
`TestFourthScorerIsUnderOneHundredLines`, because it is the same claim one process boundary out.

```text
recency  functionScore   57 lines
text     match           55 lines
vector   knn             74 lines
```

`TestTheFusionCodeDoesNotKnowAboutTheFourthSignal` is the other half and is structural rather
than numeric: the five functions that decide how streams combine — `fuser`, `blank`, `unit`,
`weigh`, `anyOf.Candidates` — may not contain `vector.New`, `recency.New`, `knn`, `gauss`,
`.Vector` or `.Time`. It also fails if it finds fewer than five of them, so the list cannot go
stale without saying so.

**One bug in the test itself, found by running it.** The first forbidden list held `"text."`,
which matches `context.Context` — a false positive that would have made the test look stricter
than it was. Replaced with constructor-qualified names.

## The measurement, and why it was staged

The judgment needs the *fourth signal's* cost, not the round's. So `knn` and `hybrid` were landed
and staged first, and recency was added on top of that boundary — `git diff --numstat` against
the index is then exactly the fourth signal. Written the other way round, the number would not
have existed.

| reading | lines | budget |
| --- | --- | --- |
| `functionScore` alone | 57 | 100 — met |
| code only, no comments or blanks | 96 | 100 — met |
| every line it cost to write (milestone 1's rule) | 127 net | 100 — **missed** |

The miss is published rather than argued around. Choosing the flattering rule after seeing the
number would make it not a budget.

## A test that was under-determined, and the fix

`TestRecencyIsAStreamOverHTTP` first asserted that a plain hybrid of text + recency puts the
recent document first. It did not, and the test was wrong rather than the code: the two documents
hold identical text, so the text stream ties them and the tie breaks on DocID — and RRF over
`[tie, recency]` produces two *equal* sums. An equal vote cannot move an equal tie.

Rewritten to assert three separate things: the recency stream alone ranks by time; the unweighted
hybrid returns both; and the **weighted** hybrid (`weights: [1, 5]`) puts the recent one first.
That is a stronger test — it is the weight doing the work, which is what D-030 claims.

## Numbers

| check | result |
| --- | --- |
| package tests | 100% pass |
| fusion functions changed | **0** |
| `pkg/` diff | 0 lines |
| `make compat` (`opensearch-py` 3.2.0) | 49 checks pass, three signals fused in one query |
| `make arch` | 8/8 |
| `go list -m all` | 1 line |
