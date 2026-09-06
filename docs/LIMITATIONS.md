# Limitations — what weft does not do

Everything here is known and documented rather than broken. A row is not worth an issue unless you can say what it should do instead ([SUPPORT.md](../SUPPORT.md)).

Rows are grouped by what they cost you: whether weft stays up, whether your data survives, whether the answer is good, and what you cannot ask. Each row appears in exactly one group.

## Throughput and latency

| Limitation | Detail |
| --- | --- |
| Sustained throughput collapses rather than degrades | At 27 queries/s — its own sequential rate — p50 goes 39 ms to 1.27 s, 14% of queries are shed, RSS goes 126 to 853 MiB. **Measured before the fix**, and whether the fix moved it is unknown: the load ladder that would say has come back void four rounds running. [FINDINGS milestone 23](FINDINGS.md) |
| A commit is slow, and since milestone 9 it is only slow | A 20,000-document batch holds the writer for 11.3 s and nothing bounds that. It no longer stops reads: the worst read due inside that window waits **61 ms**, from 13.072 s before the lock was split. [FINDINGS milestone 9](FINDINGS.md), [D-017](DECISIONS.md) |
| `Add` and `Merge` are worse than `Commit` | `Commit` takes a `context.Context` and can be called off. `Add` blocks for a whole commit; `Merge` is an uncancellable stop longer than one |
| No early termination | The top-k candidate interface forecloses WAND-style skipping. Cost and extension path: [FINDINGS §3.1](FINDINGS.md) |

The wall under the collapse is live heap under concurrency. Milestone 22 measured that heap against bleve — **2.82 MB a query against 18 KB** — and milestone 23 took it to **32 KB**, 1.8× bleve, by scoring document-at-a-time. Every figure since is a sequential microbenchmark.

## Storage and durability

| Limitation | Detail |
| --- | --- |
| Deletion reclaims nothing | A deleted document is invisible to every scorer, but its record, key and postings stay on disk and every `Merge` copies them forward. A full re-index is the only compaction: [D-019](DECISIONS.md) |
| An update spends a DocID | Updating a *committed* document tombstones the old record and appends a new one, so `Len` grows while `Stats` does not. The ceiling `Add` enforces is 2³²−1 ids, not documents. Unmeasured |
| Durability stops at fsync | Atomic against process death, best-effort against power loss, no platform write barrier: [FORMAT.md §6](FORMAT.md) |
| Caller-held scorer data is not persisted | A signal whose data is not an `engine.Document` field lives in your program. `Commit` does not write it and `Open` does not restore it — rebuild it keyed by `Document.Key` after every open |
| A tokenizer mismatch is refused when you open, not when you query | `Open` recomputes one live document's token count against the number on disk and returns `ErrTokenizerMismatch`. Pass the same `WithTokenizer` the commit was made with: [D-023](DECISIONS.md), [FORMAT §8](FORMAT.md) |

Why deletion cannot reclaim: emptying the slot would mean renumbering, and `DocID` is what `TopK` breaks ties on, what keeps posting lists ascending, and what lets a merge be a concatenation.

Why the tokenizer guard is weak: it compares a **count**, so a tokenizer that preserves token count — a stemmer does — still slips through. The tokenizer's identity is deliberately not stored, because a label can lie.

## Ranking quality

| Limitation | Detail |
| --- | --- |
| **Graph proximity measured worthless** | +0.0000 nDCG@10 at its best fusion weight, −0.1227 if fused at equal weight. No weight in the tested grid beats the baseline. Do not enable `scorer/graph` expecting quality: [D-005](DECISIONS.md) |
| The graph walk that replaces it is **unmeasured** | `graph.NewPPR` removes the tie group milestone 4 blamed and costs 5.8× less per query. Whether that recovers any nDCG is not known — the arms are registered and have not been run: [FINDINGS milestone 15](FINDINGS.md) |
| PPR carries PageRank's degree bias | On a citation corpus that ranks the paper everything cites above the paper the query is about. The lever is degree normalization, and it is not measured either |
| Fusion weights have no source | `FuseWeighted` exists, but nothing decides what the weights should be. Use `Fuse` unless you have measured your own corpus |
| `weft_graph` with `"ppr": true` builds an adjacency per query | The HTTP graph clause reads every document's links on each such request, and pays it again on the next one. Cache it per index generation if it is ever run at a rate. The plain walk does not do this |
| Links are indexed as tokenized text as well as edges | A field bound with `"links": true` becomes `Document.Links` **and** an indexed field, so an id the tokenizer splits — anything hyphenated, under the default — is reachable by its parts rather than whole. The edges are unaffected: they are keys, not terms ([D-022](DECISIONS.md)) |
| Vector recall falls as documents are deleted | Tombstones are filtered after a segment has widened its own probe, so fewer than k can survive `Index.Nearest`: [D-019](DECISIONS.md) |
| A field match is not normalized by length | A document's stored token count is `Text` plus every field, with no record of which was which, so there is nothing to normalize against. `NewField` sets BM25's `B` to 0 rather than ranking by brevity. A per-field count is a format v6: [FORMAT §8](FORMAT.md) |
| No embedding generation | Vectors are supplied by the caller |

Hand-tuning fusion weights per corpus reintroduces exactly the per-deployment burden this design avoids; learning them from judgments is unbuilt. The one exception this project publishes is the 0.1 graph discount, which milestone 4 measured and which `examples/basic` and `examples/breakdown` therefore use ([FINDINGS milestone 4 §7](FINDINGS.md)).

## What you cannot ask

| Limitation | Detail |
| --- | --- |
| The query syntax has no boolean algebra | `query.Parse` speaks a flat clause list with `+` and `-`. **No parentheses, no OR, no nesting** — `a OR (b AND c)` cannot be written |
| A constraint needs a `Fuser` | A scorer that returns only matching documents ranks rather than filters, and what it refused returns on another stream's vote with no error. `query.Must` and `query.MustNot` are that `Fuser`: [ADOPTION §8](ADOPTION.md), `ExampleFuser` |
| Phrase search re-reads the document | `Posting` carries a term's document and frequency, **not its positions**. `query.Phrase` wraps another scorer and checks its candidates against `Document.Text`, at one record decode each. No highlighting and no proximity window: [FORMAT §8](FORMAT.md) |
| A range needs the caller to encode | `Range` compares terms as bytes. `query.EncodeInt` and `EncodeTime` make a string sort like the number it encodes; a value encoded one way and queried another matches nothing, with nothing to report |
| `Fuzzy` counts bytes, not runes | A substitution inside a multi-byte character is several edits, so matching is accurate for Latin text and poor for Korean, Japanese and Chinese, where one typo is a syllable |
| A term pattern scans the vocabulary | `query.Glob` narrows by the pattern's literal prefix, then examines every term that survives. `query.MaxTerms` bounds the expansion at 4096 |
| No CJK tokenization | The default tokenizer splits on whitespace and punctuation only, so a CJK run collapses into one token. `engine.WithTokenizer` replaces it at index and query time together, and `ExampleWithTokenizer` shows a ten-line Hangul bigram tokenizer ([D-022](DECISIONS.md)) |

Four of these are cheaper than they look:

- **No boolean algebra is the point.** A flat clause list is what rank fusion can express: a union of votes with an intersection and a difference applied to it. Expressing a tree would mean an evaluator over it — a different engine, not a parser. What the flat list buys is that every query is a set of streams you can see and weight.
- **A restricting scorer must return every match, not its top k.** One truncated to `k` excludes everything below its own cut.
- **The vocabulary scan is affordable, not fast.** The vocabulary is bounded by the language, not the corpus: 2.7 MB against 626 MiB of documents on the evaluation index.
- **A range has no numeric index.** Its cost scales with a field's distinct values, not with how many documents hold them.

weft ships **no** second tokenizer and no morphological analysis. That would collide with the zero-dependency constraint, and a seam with a menu in it is not a seam.

## Rules the caller has to keep

Not limitations of what weft computes, but preconditions it does not check for you.

| Rule | Why |
| --- | --- |
| Scorers must share one index | `DocID` is index-relative, so scorers built against different indexes fuse unrelated documents. A precondition on `Search`, not a check: [FINDINGS §3.4](FINDINGS.md) |
| A graph `Adjacency` is a snapshot | It resolves every edge once, at construction, and nothing invalidates it. A stale graph answers plausibly rather than erroring — rebuild after ingest |
| A field's terms and `Text`'s do not mix | A term in `Text` and the same term in a field are two different terms. Putting a word in both places makes it findable both ways and counts its tokens twice toward the document's length |

## The HTTP surface (`cmd/weftd`)

The server is not the library. Everything above applies to it, and these are its own. [DECISIONS](DECISIONS.md) D-025 through D-031 are the arguments; `internal/opensearch`'s package documentation is the shortest version.

| Limitation | Detail |
| --- | --- |
| `keyword` is analysed like `text` | One index has one tokenizer ([D-022](DECISIONS.md)) and there is no per-field analyser. A single-token keyword behaves exactly as OpenSearch's does; a **two**-token keyword is found as a conjunction, so `{"term":{"city":"New York"}}` matches documents holding both words in any order — broader than OpenSearch |
| `terms` loses a multi-token value's conjunction | Each value's tokens become separate streams, so `{"terms":{"tag":["new york","paris"]}}` matches a document holding `new` and `paris`. Single-token values are unaffected |
| `wildcard` reads `[` as a character class | `query.Glob` borrows `path.Match`, where `[a-z]` is an alternation; OpenSearch reads `[` literally. `*` and `?` agree |
| `fuzzy` counts **bytes** | One mistyped Hangul syllable is three edits, past `MaxEditDistance` before it is one character wrong. `fuzziness: AUTO` uses OpenSearch's length thresholds over byte length for the same reason |
| Deep paging is linear, capped at 10,000 | A page is `from + size` candidates fetched and then cut. Over `index.max_result_window` it is refused rather than allocated |
| No boolean algebra | A `bool` holds leaf clauses one level deep. A nested `bool` is a 400 |
| One vector and one time per index | `engine.Document` carries one `Vector` and one `Time`. A second `knn_vector` or `"recency": true` field is refused at mapping time rather than accepted and ignored |
| Decay parameters are refused | `function_score` accepts `gauss`, `exp` and `linear` and approximates all three with `scorer/recency`'s fixed half-life. `origin`, `scale`, `offset` and `decay` are refused, because reading them and ignoring them would rank by a curve nobody asked for |
| `_source` and `_mapping` are two side stores | Both live beside the segments and are published by atomic rename. `LoadSource` refuses to start when the store and the index disagree about document count; **there is no equivalent check for the mapping** |
| Nothing is measured under load | Every judgment about the server is functional. The 27 q/s collapse and the eleven-second commit lock are the *library's*, measured through a Go harness. What they look like through HTTP is unmeasured |

### The refusal rate is 24% of the documented query surface

**Six of twenty-five rows** in the PRD's DSL table refuse, for reasons in the format rather than in the schedule:

| Row | Refused because |
| --- | --- |
| `function_score` without a decay function | there is no curve to approximate |
| nested `bool` | a query here is a flat list of streams; nesting needs a query tree and an evaluator |
| `aggs` | no aggregation machinery exists |
| `sort` other than `_score` | the top-k collector orders by fused rank |
| `highlight` | postings carry no positions ([FORMAT §8](FORMAT.md)) |
| `scroll` / PIT | no cursor or point-in-time snapshot exists |

`TestTheRefusalRateIsCounted` in `internal/opensearch/dsl_test.go` keeps that number honest. It logs the count and fails if more than half the table refuses, which is the threshold at which "compatible" becomes the wrong word.
