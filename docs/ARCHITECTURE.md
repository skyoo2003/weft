# Architecture — how the module is shaped

What lives where, which direction dependencies point, and which of those a test decides rather than a reviewer. How to *add* a signal is [SCORERS.md](SCORERS.md); this file is the shape it plugs into.

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

The library, under `pkg/`:

```text
pkg/engine/     shared types, the Scorer interface, the in-memory index,
                Search, the segment format, Commit and Open
pkg/fusion/     RRF — imports engine and nothing else
pkg/query/      Must/MustNot restriction fusers; Glob, Range and Fuzzy term
                selection; Phrase; Parse for the string syntax.
                Imports engine and nothing else
pkg/scorer/text/      BM25, ln(1+…) IDF form; NewField for one Document field
pkg/scorer/vector/    brute-force cosine
pkg/scorer/graph/     seed BFS, 1/(1+hops); Adjacency, the link structure
                      resolved into DocID space forward and reverse;
                      PPR, a walk over it
pkg/scorer/recency/   1/(1+age/HalfLife)
```

Everything else is in front of it or beside it:

```text
cmd/weft/       the command line: index, search, inspect, check, encode
cmd/weftd/      the HTTP server
cmd/weft-eval/  prepare / build / diagnose / run / sweep / weights / recall / bench
examples/basic/       minimal library embedding
examples/breakdown/   four scorers, one query, each scorer's own rank beside it
examples/weights/     Fuse and FuseWeighted over the same query
examples/sparse/      the documents a scorer cannot see
internal/eval/        nDCG@10, arm runner, paired bootstrap, dataset readers
internal/loadgen/     open-loop load driver, quantiles, GC and rusage accounting
internal/opensearch/  the DSL subset, mappings and the _weft routes
bench/          separate module: the bleve comparison, so weft keeps zero deps
grpc/           separate module: the gRPC surface, for the same reason
docs/           see the map in README
```

`internal/eval` is under `internal/` on purpose. An evaluation harness is not part of the library contract, and keeping it out of `pkg/` leaves `engine`'s exported API — and the golden file guarding it — untouched by the measurement. It uses the standard library only, so `make deps` still prints one module.

## Dependencies point inward

`engine` imports no weft package. `fusion` imports only `engine`. `engine.Search` takes a `Fuser` function rather than importing `fusion`, so `engine` is ignorant of the fusion strategy as well as of scorers.

## Three assertions, verified mechanically

`make arch` runs them by name. They are ordinary tests in `pkg/engine`, so `make all` — and therefore CI — already runs them too.

| Assertion | What decides it |
| --- | --- |
| **Fusion is invariant to scorer count** | Three and four scorers use the same call expression; compiling is the proof |
| **A new scorer is cheap** | `scorer/recency` is 99 implementation lines against a 100-line budget, and `fusion/` needs no change |
| **Fusion cannot see scorers** | `go list -deps ./pkg/fusion` names no `scorer/*` package |

The third carries the weight. `Fuse` never reads `Candidate.Score`, only rank. BM25 is unbounded, cosine is `[-1,1]`, graph proximity is `(0,1]`, so comparing scores across scorers would need per-scorer normalization — and knowing how to normalize means knowing which scorer produced the score.

Whatever a scorer costs `engine` shows up in `pkg/engine/testdata/engine_api.txt`. That file records member types, parameter and result types, declaration order, the package clause, the value a constant is declared from, and whether a struct has become unkeyed-literal-hostile by gaining an unexported field: everything a caller has to satisfy, and nothing that only spelling would change ([FINDINGS §1](FINDINGS.md)).

Two of these three are what [GOVERNANCE.md](../GOVERNANCE.md) means by a constraint that overrules an opinion. The gate rejects a contradicting change before a maintainer has to.

## Size

**Zero external dependencies.** Go 1.26+.

| | Implementation | Tests |
| --- | --- | --- |
| `pkg/` — the library | 11,039 | 15,427 |
| `internal/eval` + `cmd/weft-eval` — the harness | 5,763 | — |

Lines are counted the way `TestFourthScorerIsUnderOneHundredLines` counts them: every line of every `.go` file, minus the SPDX header `make spdx` puts on all of them.

The harness ships no API and is not part of the library.

These figures are a measurement, not a budget. The one exception is `scorer/recency`'s 100-line ceiling, which [CONTRIBUTING](../CONTRIBUTING.md#what-not-to-break) explains before it bites you.
