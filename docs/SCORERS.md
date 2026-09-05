# Adding a scorer

The main extension path. Implement `engine.Scorer`; nothing in `engine/` or `fusion/` changes. What that costs and why it holds is [ARCHITECTURE.md](ARCHITECTURE.md).

```go
func (s *Scorer) Name() string { return "popularity" }

func (s *Scorer) Candidates(ctx context.Context, q engine.Query, k int) ([]engine.Candidate, error) {
    cands := make([]engine.Candidate, 0, s.ix.Len())
    // Score however you like. Any scale — fusion reads rank, not score.
    return engine.TopK(cands, k), nil
}
```

Then add one element at the call site:

```go
scorers := []engine.Scorer{txt, vec, gr, rec, pop} // pop is yours, from above
results, err := engine.Search(ctx, q, 10, fusion.Fuse, scorers...)
```

## Four things that are not obvious from the skeleton

Each cost a trial subject time in [ADOPTION.md](ADOPTION.md). `ExampleScorer` in `pkg/engine` is the first three in one compiling program and `ExampleFuser` is the fourth — read them on pkg.go.dev, which renders Examples; `go doc` does not.

**Your data does not go in `engine.Document`.** Four of its five fields are what weft's own scorers read — `Text`, `Vector`, `Links`, `Time`; the fifth, `Key`, is your identifier, not a signal — and you cannot add a sixth from outside the module. Keep your table keyed by `Document.Key` — a map, a database, whatever you have — and call `Index.Resolve` to turn a `Key` into a `DocID`. `Commit` does not carry it, so rebuild after `Open`; `Key` still names the same document, which is what makes the rebuild safe.

**An input that changes per query does not go in `engine.Query` either.** Bind it when you construct the scorer and construct one per search — `recency.NewAt(ix, now)` is that shape with a clock. A scorer value is small; this costs an allocation, not corpus work. Do not reuse `Query.Seeds`, which `scorer/graph` reads.

**Fuse deeper than you display.** `Search`'s `k` is both the per-scorer request size and the result size. A signal orthogonal to the built-in ones surfaces documents they rank below their own cut, so at a shared `k` it appears in one stream only — and RRF is built so a single vote does not win. Ask for more than you show and slice.

**A constraint is a `Fuser`, not just a `Scorer`.** Rank fusion is a **union of votes**: a document's score sums over the streams it appears in, so being absent from one costs it nothing in the others. A scorer returning only the documents that satisfy a phrase, a field match or a price ceiling therefore expresses a *preference*, and everything it refused comes back on another scorer's vote — with no error. To exclude, pass a `Fuser` that reads one stream as a restriction and drops documents missing from it before fusing. `Search` takes the fuser as a parameter precisely so this is yours to write, and `query.Must`/`query.MustNot` are the one you would have written.

## Registering it in the milestone 1 assertions

The import-graph assertion covers your scorer automatically — it reads the import graph, so it needs no baseline and no new test.

The invariance assertion does not generalize by itself. `TestAddingAFourthScorerDoesNotChangeTheCallShape` and `TestAnyNumberOfScorersFuses` name the four scorers in the tree, so nothing runs yours until you add it to those two slices in `pkg/engine/architecture_test.go`. Add it there. [CONTRIBUTING](../CONTRIBUTING.md#what-not-to-break) has the rest of what a change must not break.

## Weighting a stream you trust less

```go
// Trust the graph stream less, without fusion learning what a graph scorer is.
fuse := fusion.FuseWeighted(1, 1, 0.1)
results, err := engine.Search(ctx, q, 10, fuse, txt, vec, gr)
```

Weights index by stream *position*, not scorer kind. Where they should come from is unsolved — use `Fuse` unless you have measured your own corpus: [LIMITATIONS](LIMITATIONS.md#ranking-quality), [STATUS](STATUS.md#published-numbers).
