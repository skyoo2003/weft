# Architecture — how the module is shaped

What lives where, which direction dependencies point, and which of those two facts a test decides rather than a reviewer. How to *add* a signal is [SCORERS.md](SCORERS.md); this file is the shape it plugs into.

## The one interface

```go
// Every scorer implements this. Fusion knows only this.
type Scorer interface {
    Name() string
    Candidates(ctx context.Context, q Query, k int) ([]Candidate, error)
}

// Knows neither how many scorers there are nor what any of them compute.
func Fuse(streams [][]Candidate, k int) []Candidate
```

## Layout

```text
cmd/weft/          interactive demo binary
examples/basic/    minimal library embedding
pkg/
  engine/          shared types, Scorer interface, in-memory index, Search,
                   segment format, Commit and Open
  fusion/          RRF — imports engine and nothing else
  query/           Must/MustNot restriction fusers; Glob, Range and Fuzzy term
                   selection; Phrase; Parse for the string syntax — imports
                   engine and nothing else
  scorer/
    text/          BM25, ln(1+…) IDF form; NewField for one Document field
    vector/        brute-force cosine
    graph/         seed BFS, 1/(1+hops); Adjacency, the link structure resolved
                   into DocID space forward and reverse; PPR, a walk over it
    recency/       1/(1+age/HalfLife)
internal/
  eval/            nDCG@10, arm runner, paired bootstrap, dataset readers
  loadgen/         open-loop load driver, quantiles, GC and rusage accounting
cmd/weft-eval/     prepare / build / diagnose / run / sweep / weights / recall / bench
bench/             separate module: the bleve comparison, so weft keeps zero deps
docs/              see the map in README
```

`internal/eval` is under `internal/` on purpose: an evaluation harness is not part of the library contract, and keeping it out of `pkg/` leaves `engine`'s exported API — and the golden file guarding it — untouched by the measurement. It uses the standard library only, so `make deps` still prints one module.

## Dependencies point inward

`engine` imports no weft package; `fusion` imports only `engine`. `engine.Search` takes a `Fuser` function rather than importing `fusion`, so `engine` is ignorant of the fusion strategy as well as of scorers.

## Three assertions, verified mechanically

`make arch` runs them by name. They are ordinary tests in `pkg/engine`, so `make all` — and therefore CI — already runs them too.

| Assertion | What decides it |
| --- | --- |
| **Fusion is invariant to scorer count** | Three and four scorers use the same call expression; compiling is the proof |
| **A new scorer is cheap** | `scorer/recency` is 99 implementation lines against a 100-line budget, and `fusion/` needs no change |
| **Fusion cannot see scorers** | `go list -deps ./pkg/fusion` names no `scorer/*` package |

The third carries the weight. `Fuse` never reads `Candidate.Score`, only rank: BM25 is unbounded, cosine is `[-1,1]`, graph proximity is `(0,1]`, so comparing scores across scorers would need per-scorer normalization — and knowing how to normalize means knowing which scorer produced the score.

Whatever a scorer costs `engine` shows up in `pkg/engine/testdata/engine_api.txt`, which records member types, parameter and result types, declaration order, the package clause, the value a constant is declared from, and whether a struct has become unkeyed-literal-hostile by gaining an unexported field — everything a caller has to satisfy, and nothing that only spelling would change ([FINDINGS §1](FINDINGS.md)).

Two of these three are also what [GOVERNANCE.md](../GOVERNANCE.md) means by a constraint that overrules an opinion: the gate rejects a contradicting change before a maintainer has to.

## Size

2,753 implementation lines under `pkg/`, 5,138 test lines, **zero external dependencies**. Go 1.26+.

The evaluation harness in `internal/eval` and `cmd/weft-eval` is another 3,753 and 2,851; it ships no API and is not part of the library.

The line-count figure is a measurement, not a budget — with one exception, `scorer/recency`'s 100-line ceiling, which [CONTRIBUTING](../CONTRIBUTING.md#what-not-to-break) explains before it bites you.
