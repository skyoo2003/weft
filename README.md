# weft

[![CI](https://github.com/skyoo2003/weft/actions/workflows/ci.yml/badge.svg)](https://github.com/skyoo2003/weft/actions/workflows/ci.yml)

> The weft thread. The warp threads never touch each other; one weft crosses and binds them all.

A search engine where ranking signals are interchangeable. Go, from scratch, standard library only.

```go
// Every scorer implements this. Fusion knows only this.
type Scorer interface {
    Name() string
    Candidates(ctx context.Context, q Query, k int) ([]Candidate, error)
}

// Knows neither how many scorers there are nor what any of them compute.
func Fuse(streams [][]Candidate, k int) []Candidate
```

## Why

Hybrid search engines started with one signal and bolted the rest on, so fusion ends up a special case: a dedicated code path joins two signals, and a third means rewriting it. That is why graph proximity is not a first-class ranking signal in any engine. weft inverts the order — fusion is the default operation and scorers plug into it, so the fourth scorer costs what the first did.

If you need text + vector hybrid search today, [bleve](https://github.com/blevesearch/bleve) already has BM25, ANN and RRF. weft rests on an architectural hypothesis, not a market gap; [`docs/FINDINGS.md`](docs/FINDINGS.md) records how far it is verified.

## Status

Milestones 1 through 6 are done. **Not usable in production:** a commit holds a write lock for as long as it takes — 11 seconds for a 20,000-document batch, with reads queueing behind it — and sustained query load collapses at 27 queries per second on the machine measured rather than degrading.

| # | Milestone | State |
| --- | --- | --- |
| 1 | Scorer-agnostic fusion | ✅ 3/3 assertions pass |
| 2 | Persistence | ✅ restores identically, commits atomically |
| 3 | Scale — segment merge, lazy loading, ANN | ✅ both paths hold. `Open` 979 ms → 54 ms, ANN recall@10 0.992 at 4.6× the query speed — but the vector gain is smaller than hoped |
| 4 | Quality — graph contribution to nDCG | ✅ measured, and the answer is **no** — see below |
| 5 | Performance — p99 including GC pauses | ✅ p99 **108.193 ms**, of which the collector is 411 µs (0.38%); 1.88× bleve v2.6.0 against a 10× bar |
| 6 | External contribution readiness | ✅ measured — two subjects with no prior sight of the tree added a signal from the documentation alone, which found three documentation defects. Both subjects were **agents, not people**, so this is a lower bound and not a user study: [ADOPTION.md](docs/ADOPTION.md) |
| 7 | A baseline nobody has to qualify | ⚠️ **there is no baseline.** Three runs at one arrival rate gave 37.9 ms, 1.539 s and 416 ms; the load point is not reproducible and what decides the outcome is not the load: [FINDINGS milestone 7](docs/FINDINGS.md) |
| 19 | Ranges, fuzzy, field-scoped patterns | ✅ `query.Range` over a field, with `EncodeInt`/`EncodeTime` making a string sort like the number it encodes — so numeric and date ranges cost **no numeric index and no format change**, and scale with a field's distinct values rather than with the corpus. `query.Fuzzy` is Levenshtein within 2, bytes not runes. `Glob`, `Range` and `Fuzzy` all scope to a field |
| 18 | Field search | ✅ `Document.Fields` and `text.NewField(ix, "title")`. A field is a **term space, not a section** — every field's tokens are indexed under `engine.FieldTerm` as `name\x00token`, so nothing in the index learned what a field means and searching two fields is two scorers plus fusion. Format **v5**; v2 through v5 all read. The field scorer does **not** normalize by length, and [FINDINGS milestone 18](docs/FINDINGS.md) says why |
| 17 | The allocation floor under every query | ✅ `scorer/text` sized its BM25 accumulator to the **corpus**, so a query matching nothing allocated 1.18 MB and 78 µs on a 50,000-document index — 4.73 MB on the evaluation corpus, and 128 MB/s of garbage at the arrival rate where load collapses. Now sized to the widest posting list the query walks: **521× faster and 73,890× less memory** on a query matching nothing, 103× and 165× on a rare term, unchanged on a term held by every document. No score changes. Whether it moves the collapse is a ladder run and is **not** claimed: [FINDINGS milestone 17](docs/FINDINGS.md) |
| 16 | Query expressiveness — boolean, pattern, phrase | ✅ `pkg/query` ships the `Fuser` every adopter had to write for themselves — `Must`, `MustNot`, composable and indexed by stream position — plus `Glob` for prefix/suffix/wildcard terms and `Phrase` for exact runs. No format change, no new dependency, and `go list -deps ./pkg/query` names no scorer. Still missing: a string syntax, numeric ranges, fuzzy matching — field restriction landed in milestone 18 |
| 15 | Graph indexing and search | ⚠️ **built and measured for cost, not for quality.** The citation graph is an index — resolved into `DocID` space, forward and reverse — and a personalized-PageRank scorer walks it: 5.8× the speed of the BFS arm and **211× fewer allocations** (48,522 → 230 a query). The tie group milestone 4 blamed is gone, 2 distinct scores becoming 8 on the fixture that reproduces it. **No nDCG number exists**: the arms are registered and the corpus has not been run: [FINDINGS milestone 15](docs/FINDINGS.md) |

No tag yet; the first will be `v0.1.0`. Until then `go get` resolves to a pseudo-version naming a commit, which is the honest state — a tag would give you a shorter name without changing anything the warning above says. [CHANGELOG](CHANGELOG.md) is where a version tells you whether you have work to do, and it records three things only: the exported API of every package under `pkg/`, the on-disk format version, and the minimum Go version. The milestone numbers in this table are not among them.

Milestone 4 ran ahead of 3 because the dataset fits in memory and the project's second falsification condition was waiting on it.

Two things milestone 5 owes and has not paid, stated here rather than left in a findings document: its headline is **one run, not the three repetitions [PERF.md](docs/PERF.md) requires**, and the arm you would actually deploy — `text + vector` — **has no published tail**, because at four times the cost per query 10,000 samples take over five hours. The published p99 is comparable to bleve's; it is not the number a `text + vector` user will see.

Milestone 7 went to pay the first of those and could not. Three runs at the same arrival rate produced medians 40× apart — one rung shed nothing at 37.9 ms, two collapsed to 1.539 s and 416 ms and shed 14% and 11% of the load — with no suspension, no thermal story and no relative-load story that fits. The difference that survives is that the flat observation was the fourth rung of a ladder and the two collapses were single rungs. **So `108.193 ms` still stands, and now stands with two further caveats**: it is one draw from a load-point rule that flips between the ladder's slowest and fastest rung on a few milliseconds of first-rung median, and it was measured before the instrument could tell you whether the machine slept through it. Neither is a reason to withdraw it; both are reasons not to compare against it silently. [FINDINGS milestone 7](docs/FINDINGS.md), [D-012](docs/DECISIONS.md).

### Published numbers

TREC-COVID, 50 queries, 171,332 documents, 579,719 in-corpus citation edges from Semantic Scholar, 148,232 SPECTER2 vectors. nDCG@10, paired bootstrap over 10,000 resamples.

| Arm | nDCG@10 |
| --- | --- |
| `text` (BM25) | 0.5826 |
| `text + vector` | **0.6233** |
| `text + vector + graph` | 0.5005 |

**Graph proximity does not improve ranking.** Under equal-weight fusion it costs 0.1227 nDCG@10 (95% CI [−0.1550, −0.0909]), and across 28 configurations of the RRF rank constant and fusion depth the sign never flips and no interval reaches zero.

**But that −0.1227 belonged to the fusion operator, not to the graph.** Giving the graph stream half a vote instead of a full one erases all but 0.0019 of the regression. No weight in the tested grid beats the baseline, and from 0.1 downward the arm *is* the baseline, delta exactly zero — see [EVAL.md](docs/EVAL.md) section 5.11 for what that is and is not entitled to claim. So the accurate statement is the flatter one: the graph signal is not harmful information, it is not information.

| Graph stream weight | nDCG@10 | Delta vs baseline |
| --- | --- | --- |
| 1.0 | 0.5005 | −0.1227 |
| 0.5 | 0.6214 | −0.0019 |
| ≤0.1 | 0.6233 | +0.0000, converged to baseline |

**Equal weighting is a ranking decision RRF makes silently on every query**, and here it cost two orders of magnitude more than the signal being evaluated. So `fusion.FuseWeighted` shipped:

```go
// Trust the graph stream less, without fusion learning what a graph scorer is.
fuse := fusion.FuseWeighted(1, 1, 0.1)
results, err := engine.Search(ctx, q, 10, fuse, txt, vec, gr)
```

Weights index by stream *position*, not scorer kind — the caller already fixed that order — so this does not compromise scorer-agnosticism, and `go list -deps ./pkg/fusion` still names no scorer. `Fuse` is unchanged and its unweighted path is bit-identical. Where the weights should come from is the open question: hand-tuning per corpus reintroduces exactly the burden this design avoids, so `FuseWeighted`'s docs say a caller without its own measurement should use `Fuse`. [FINDINGS milestone 4 §7](docs/FINDINGS.md).

`scorer/graph` is **kept rather than deleted**, with the verdict at the top of its package documentation. The falsification condition said a worthless signal goes; the weight sweep then showed the scorer is inert rather than harmful, and that deleting it would cut the milestone 1 assertions from four signals to three. [D-005](docs/DECISIONS.md) argues both sides.

What was falsified is one construction — BFS hop distance, seeded from the text top 5 — not the idea that citation structure carries signal. The graph stream returns thousands of candidates carrying 3 to 16 distinct scores, so it has almost no internal ranking to contribute at any weight.

Two supporting results:

- **BM25 matches `rank_bm25` to 4.44e-16**, and nDCG matches `pytrec_eval`'s `ndcg_cut_10` on 12 discriminating fixtures. Both checks ran before any arm number was published, and the second found the plan's own nDCG definition to be wrong.
- **BM25 alone scores 0.5826** where BEIR reports ~0.656 for Anserini with stemming and a stopword list — the same order, from a whitespace tokenizer.

Full measurement design, coverage, sweep and reasons to doubt the numbers: [EVAL.md](docs/EVAL.md). Reproduce with `make eval`.

## Quick start

```bash
go run ./cmd/weft
```

```text
query> ranking fusion
  1. rrf        0.03226  text:2  vector:-  graph:-  recency:2
  2. hnsw       0.01749  text:-  vector:-  graph:2  recency:3
  3. ivf        0.01721  text:-  vector:-  graph:3  recency:4
  4. bm25       0.01702  text:-  vector:-  graph:1  recency:5
  5. tfidf      0.01639  text:1  vector:-  graph:-  recency:-
```

Trailing columns are each scorer's rank *before* fusion. The demo fuses with `FuseWeighted(1, 1, 0.1, 1)`, discounting the graph stream to a tenth of a vote, because that is the one weight this project has measured — the **graph proximity** row of the [limitations](#limitations) below — and a demo that ignored its own project's advice would be worth less than no demo. Every other stream is at 1, which is `Fuse`.

- `tfidf` leads text but lands fifth: no other scorer agreed. One scorer's confidence does not beat consensus.
- `hnsw`, `ivf` and `bm25` are invisible to text — graph traversal nominated them, and at a tenth of a vote recency decides the order among them. Graph's own first pick, `bm25`, lands last of the three: that is the discount doing what milestone 4 measured it should.
- `-` means the document is absent from that scorer's stream, for one of three reasons. No opinion, which costs nothing: `vector:-` is everywhere because the query had no vector, so append `@ 0,1,0` and the vector scorer joins. Deliberately withheld: `graph:-` on `rrf` and `tfidf` marks them as traversal seeds, which are excluded ([FINDINGS §2.3](docs/FINDINGS.md)). Or simply below the cut — every scorer is asked for `k`, so `recency:-` on `tfidf` is truncation, not abstention. Raise `-k` and it fills in.

Minimal embedding: [`examples/basic`](examples/basic/main.go). Godoc example: `Example` in `pkg/engine`.

## Adding a scorer

Implement `engine.Scorer`; nothing in `engine/` or `fusion/` changes.

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

**Four things that are not obvious from the skeleton.** Each cost a trial subject time in [ADOPTION.md](docs/ADOPTION.md). `ExampleScorer` in `pkg/engine` is the first three in one compiling program and `ExampleFuser` is the fourth — read them on pkg.go.dev, which renders Examples; `go doc` does not.

*Your data does not go in `engine.Document`.* Four of its five fields are what weft's own scorers read — `Text`, `Vector`, `Links`, `Time`; the fifth, `Key`, is your identifier, not a signal — and you cannot add a sixth from outside the module. Keep your table keyed by `Document.Key` — a map, a database, whatever you have — and call `Index.Resolve` to turn a `Key` into a `DocID`. `Commit` does not carry it, so rebuild after `Open`; `Key` still names the same document, which is what makes the rebuild safe.

*An input that changes per query does not go in `engine.Query` either.* Bind it when you construct the scorer and construct one per search — `recency.NewAt(ix, now)` is that shape with a clock. A scorer value is small; this costs an allocation, not corpus work. Do not reuse `Query.Seeds`, which `scorer/graph` reads.

*Fuse deeper than you display.* `Search`'s `k` is both the per-scorer request size and the result size. A signal orthogonal to the built-in ones surfaces documents they rank below their own cut, so at a shared `k` it appears in one stream only — and RRF is built so a single vote does not win. Ask for more than you show and slice.

*A constraint is a `Fuser`, not just a `Scorer`.* Rank fusion is a **union of votes**: a document's score sums over the streams it appears in, so being absent from one costs it nothing in the others. A scorer returning only the documents that satisfy a phrase, a field match or a price ceiling therefore expresses a *preference*, and everything it refused comes back on another scorer's vote — with no error. To exclude, pass a `Fuser` that reads one stream as a restriction and drops documents missing from it before fusing. `Search` takes the fuser as a parameter precisely so this is yours to write.

`make arch` verifies this mechanically:

- **Fusion is invariant to scorer count** — three and four scorers use the same call expression; compiling is the proof.
- **A new scorer is cheap** — `scorer/recency` is 99 implementation lines against a 100-line budget, and `fusion/` needs no change. Whatever a scorer costs `engine` shows up in `pkg/engine/testdata/engine_api.txt`, which records member types, parameter and result types, declaration order, the package clause, the value a constant is declared from, and whether a struct has become unkeyed-literal-hostile by gaining an unexported field — everything a caller has to satisfy, and nothing that only spelling would change ([FINDINGS §1](docs/FINDINGS.md)).
- **Fusion cannot see scorers** — `go list -deps ./pkg/fusion` names no `scorer/*` package.

The third assertion carries the weight. `Fuse` never reads `Candidate.Score`, only rank: BM25 is unbounded, cosine is `[-1,1]`, graph proximity is `(0,1]`, so comparing scores across scorers would need per-scorer normalization — and knowing how to normalize means knowing which scorer produced the score.

## Layout

```text
cmd/weft/          interactive demo binary
examples/basic/    minimal library embedding
pkg/
  engine/          shared types, Scorer interface, in-memory index, Search,
                   segment format, Commit and Open
  fusion/          RRF — imports engine and nothing else
  query/           Must/MustNot restriction fusers; Glob, Range and Fuzzy term
                   selection; Phrase — imports engine and nothing else
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
docs/
  FINDINGS.md      every milestone's result, known costs, open questions
  FORMAT.md        the on-disk format, versions 1 to 3
  DECISIONS.md     decisions expensive to reverse
  DATASETS.md      evaluation dataset survey for milestone 4
  EVAL.md          how milestone 4's numbers were produced, and why to doubt them
  PERF.md          how milestone 5's latency numbers are produced, and the load-point rule
  ADOPTION.md      how milestone 6 tested whether the documentation is enough
  RESEARCH.md      one round of community and competitive research
  testing/         TDD evidence per milestone
```

`internal/eval` is under `internal/` on purpose: an evaluation harness is not part of the library contract, and keeping it out of `pkg/` leaves `engine`'s exported API — and the golden file guarding it — untouched by the measurement. It uses the standard library only, so `make deps` still prints one module.

Dependencies point inward. `engine` imports no weft package; `fusion` imports only `engine`. `engine.Search` takes a `Fuser` function rather than importing `fusion`, so `engine` is ignorant of the fusion strategy as well as of scorers.

## Development

```bash
make            # fmt + build + vet + test -race — needs only the Go toolchain
make arch       # the three assertions above
make deps       # zero dependencies, and fusion sees no scorer
make run        # interactive demo
make example    # minimal example
make eval       # milestone 4's published nDCG table (needs a prepared corpus)
make eval-full  # adds the graph tie analysis and the 28-configuration sweep
make bench      # milestone 5's latency ladder (needs a prepared corpus, ~90 min)
```

`make all` is fmt, build, vet, `test -race`, and the two linters when they are installed — each skipped with a line saying so when it is not, which is what keeps the target runnable on a fresh checkout. CI runs it plus three targets kept out of it because each costs a tool to install or a minute of wall clock: `make spdx`, `make bench-build`, and `make fuzz` — the last being 30 seconds each against the two segment-decoder fuzz targets, which is where a hostile file would land. [CONTRIBUTING](CONTRIBUTING.md#the-gate) has the detail.

`make eval` needs data that is not in the repository. [EVAL.md §7](docs/EVAL.md) lists the downloads and the one-time `weft-eval prepare` step.

2,753 implementation lines under `pkg/`, 5,138 test lines, **zero external dependencies**. Go 1.26+. The evaluation harness in `internal/eval` and `cmd/weft-eval` is another 3,753 and 2,851; it ships no API and is not part of the library.

## Limitations

| Limitation | Detail |
| --- | --- |
| Sustained throughput collapses rather than degrades | At 27 queries/s — its own sequential rate — p50 goes 39 ms to 1.27 s, 14% of queries are shed and RSS goes 126 to 853 MiB. The wall is live heap under concurrency: every candidate decodes a whole record. Usable throughput is somewhere in 13.6–27.3/s: [FINDINGS milestone 5 §3.2](docs/FINDINGS.md). |
| A commit is slow, and since milestone 9 it is only slow | Writing a 20,000-document batch still holds the writer for 11.3 s and nothing bounds that. What it no longer does is stop reads: the worst read due inside that window waits **61 ms**, from 13.072 s on the same measurement before the lock was split. `Commit` takes a `context.Context` and can be called off. What still blocks for a whole commit is `Add`, and `Merge` is still an uncancellable stop longer than a commit: [FINDINGS milestone 9](docs/FINDINGS.md), [D-017](docs/DECISIONS.md). |
| Deletion reclaims nothing | `Delete` and `Update` exist and a deleted document is invisible to every scorer, but its record, key and postings stay on disk and every `Merge` copies them forward. Emptying the slot would mean renumbering, and `DocID` is what `TopK` breaks ties on, what keeps posting lists ascending, and what lets a merge be a concatenation. A full re-index is the only compaction: [D-019](docs/DECISIONS.md). |
| An update spends a DocID | Updating a *committed* document tombstones the old record and appends a new one, so `Len` grows while `Stats` does not. The ceiling `Add` enforces is 2³²−1 ids, not documents, and a corpus updated hot enough reaches it first. Unmeasured. |
| Vector recall falls as documents are deleted | `Index.Nearest` promises at least k candidates when the index holds k vectors. Tombstones are filtered after a segment has widened its own probe, so fewer than k can survive — the widening loop does not know about them: [D-019](docs/DECISIONS.md). |
| Caller-held scorer data is not persisted | A signal whose data is not an `engine.Document` field lives in your program, so `Commit` does not write it and `Open` does not restore it. Rebuild it keyed by `Document.Key` after every open. |
| Durability stops at fsync | Atomic against process death; best-effort against power loss, with no platform write barrier: [FORMAT.md §6](docs/FORMAT.md). |
| No early termination | The top-k candidate interface forecloses WAND-style skipping. Cost and extension path: [FINDINGS §3.1](docs/FINDINGS.md). |
| **Graph proximity measured worthless** | +0.0000 nDCG@10 at its best fusion weight — no weight in the tested grid beats the baseline, and at 0.1 and below the arm is the baseline exactly — and −0.1227 if fused at equal weight. Kept for the milestone 1 assertions and marked in its package doc: [D-005](docs/DECISIONS.md). Do not enable `scorer/graph` expecting quality, and weight it down if you enable it at all. |
| The graph walk that replaces it is **unmeasured** | `graph.NewPPR` removes the mechanism milestone 4 blamed — the tie group — and costs 5.8× less per query than the BFS. Whether that recovers any nDCG is **not known**: the arms are registered in `weft-eval run` and have not been run. It also carries PageRank's degree bias, which on a citation corpus ranks the paper everything cites above the paper the query is about; the lever is degree normalization and it is not measured either: [FINDINGS milestone 15](docs/FINDINGS.md). |
| A graph `Adjacency` is a snapshot | It resolves every edge once, at construction, and nothing invalidates it. A document added, updated or deleted afterwards is not in it, and a stale graph answers plausibly rather than erroring. Rebuild after ingest, which is the rule every caller-held side store already follows. |
| Fusion weights have no source | `FuseWeighted` exists, but nothing decides what the weights should be. Hand-tuning per corpus reintroduces the per-deployment burden this design avoids; learning them from judgments is unbuilt. Use `Fuse` unless you have measured your own. The one exception this project publishes is the 0.1 graph discount, which milestone 4 did measure and which `cmd/weft` and `examples/basic` therefore use ([FINDINGS milestone 4 §7](docs/FINDINGS.md)). |
| Scorers must share one index | `DocID` is index-relative, so scorers built against different indexes fuse unrelated documents. A precondition on `Search`, not a check: [FINDINGS §3.4](docs/FINDINGS.md). |
| No CJK tokenization, but the tokenizer is replaceable | The default splits on whitespace and punctuation only, so a CJK run collapses into one token — for Korean that means `"검색엔진을"` and `"검색엔진"` are different terms and a query for the second finds nothing. `engine.WithTokenizer` replaces it at index time and query time together, and `ExampleWithTokenizer` shows a ten-line Hangul bigram tokenizer doing it. weft ships **no** second tokenizer and no morphological analysis: that would collide with the zero-dependency constraint, and a seam with a menu in it is not a seam ([D-022](docs/DECISIONS.md)). |
| A tokenizer mismatch is refused when you open, not when you query | A directory indexed with one tokenizer and queried with another answers zero hits on everything, with nothing to report. `Open` recomputes one live document's token count against the number already on disk and returns `ErrTokenizerMismatch` instead — so pass the same `WithTokenizer` the commit was made with. It compares a **count**, so a tokenizer that preserves token count (a stemmer does) still slips through, and the tokenizer's identity is deliberately not stored because a label can lie: [D-023](docs/DECISIONS.md), [FORMAT §8](docs/FORMAT.md). |
| No embedding generation | Vectors are supplied by the caller. |
| Phrase search re-reads the document | `Posting` carries a term's document and frequency, **not its positions**, so no exact phrase can be decided from the index. `query.Phrase` wraps another scorer and checks its candidates against `Document.Text`, at one record decode each — so the cost is bounded by what the inner scorer nominated rather than by the corpus, and surviving candidates keep its scores. What that does not buy is highlighting or a proximity window, both of which want the positions. A position index is a format change: [FORMAT §8](docs/FORMAT.md). |
| A field match is not normalized by length | `Document.Fields` and `text.NewField` give field-scoped search, but a document's stored token count is `Text` plus every field with no record of which was which — so there is nothing to normalize a field match against. `NewField` sets BM25's `B` to 0 rather than dividing a five-token title by a five-thousand-token body and ranking by brevity. What that gives up is separating two equally good field matches by field length. A per-field count is a format v6: [FORMAT §8](docs/FORMAT.md). |
| A field's terms and `Text`'s do not mix | A term in `Text` and the same term in a field are two different terms. That is what makes a scoped query mean anything; a caller wanting a word findable both ways puts it in both places, which counts its tokens twice toward the document's length. |
| A constraint needs a `Fuser`, and `pkg/query` is the one you would have written | Rank fusion is a union of votes, so a scorer that returns only matching documents ranks rather than filters, and what it refused returns on another stream's vote with no error. `query.Must` and `query.MustNot` are that `Fuser`, composable and indexed by stream position. What is still yours: any constraint whose scorer you write, and the rule that **a restricting scorer must return every match rather than its top k** — one truncated to `k` excludes everything below its own cut: [ADOPTION §8](docs/ADOPTION.md), `ExampleFuser`. |
| No query language | Queries are built through the Go API. `pkg/query` covers the boolean shell (`Must`, `MustNot`), term patterns (`Glob`), ranges (`Range`), edit distance (`Fuzzy`) and exact phrases; `text.NewField` scopes BM25 to a field. There is still **no string syntax to parse into them** — no `+title:foo -draft "exact phrase"` to hand a user. The parser is the remaining piece and is worth writing now that what it would parse into is stable. |
| A range needs the caller to encode | `Range` compares terms as bytes, so a number is only rangeable if it was indexed in an order-preserving encoding. `query.EncodeInt` and `EncodeTime` are that, and a value encoded one way and queried another matches nothing with nothing to report. There is no numeric index: the cost scales with how many **distinct values** the field holds, not how many documents hold them. |
| `Fuzzy` counts bytes, not runes | A substitution inside a multi-byte character is several edits, so edit-distance matching is accurate for Latin text and poor for Korean, Japanese and Chinese — where one typo is a syllable. A rune-aware form is a different function, not a flag on this one. |
| A term pattern scans the vocabulary | `query.Glob` narrows by the pattern's literal prefix and then examines every term that survives, because a segment's terms are a map in memory even though the `terms` section is sorted on disk. The vocabulary is bounded by the language and not by the corpus — 2.7 MB against 626 MiB of documents on the evaluation index — which is what makes this affordable rather than fast. `query.MaxTerms` bounds the expansion at 4096. |

## Contributing

`make all` is the gate, and CI runs that same target. What the three assertions mean is [Adding a scorer](#adding-a-scorer), above. How to contribute — which of them a test decides for you, what the 100-line figure does and does not enforce, what a pull request should say, why a decision is recorded — is in [CONTRIBUTING.md](CONTRIBUTING.md).

| | |
| --- | --- |
| Where to take a bug, a proposal or a question | [SUPPORT.md](SUPPORT.md) |
| A vulnerability — **not** the issue tracker | [SECURITY.md](SECURITY.md) |
| Behavior in this repository | [Code of Conduct](CODE_OF_CONDUCT.md) |
| Who decides what, and what is decided by a test instead | [GOVERNANCE.md](GOVERNANCE.md) |
| How a tag gets cut | [RELEASE.md](RELEASE.md) |

## License

[Apache License 2.0](LICENSE). Third-party notices: [NOTICE](NOTICE) — there are none.
