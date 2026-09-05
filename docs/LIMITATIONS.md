# Limitations — what weft does not do

Everything on this page is known and documented rather than broken. Something here is not worth an issue unless you can say what it should do instead ([SUPPORT.md](../SUPPORT.md)).

The rows are grouped by what they cost you: whether weft stays up, whether your data survives, whether the answer is good, and what you cannot ask. A row appears in exactly one group.

## Throughput and latency

| Limitation | Detail |
| --- | --- |
| Sustained throughput collapses rather than degrades — **measured before the fix** | At 27 queries/s — its own sequential rate — p50 goes 39 ms to 1.27 s, 14% of queries are shed and RSS goes 126 to 853 MiB. The wall is live heap under concurrency, and milestone 22 measured that heap against bleve: **2.82 MB a query against 18 KB**. Milestone 23 then took it to **32 KB**, 1.8× bleve, by scoring document-at-a-time. **Whether the collapse moved is unmeasured**: the load ladder that would say has come back void four rounds running, and every figure since is a sequential microbenchmark: [FINDINGS milestone 23](FINDINGS.md). |
| A commit is slow, and since milestone 9 it is only slow | Writing a 20,000-document batch still holds the writer for 11.3 s and nothing bounds that. What it no longer does is stop reads: the worst read due inside that window waits **61 ms**, from 13.072 s on the same measurement before the lock was split. `Commit` takes a `context.Context` and can be called off. What still blocks for a whole commit is `Add`, and `Merge` is still an uncancellable stop longer than a commit: [FINDINGS milestone 9](FINDINGS.md), [D-017](DECISIONS.md). |
| No early termination | The top-k candidate interface forecloses WAND-style skipping. Cost and extension path: [FINDINGS §3.1](FINDINGS.md). |

## Storage and durability

| Limitation | Detail |
| --- | --- |
| Deletion reclaims nothing | `Delete` and `Update` exist and a deleted document is invisible to every scorer, but its record, key and postings stay on disk and every `Merge` copies them forward. Emptying the slot would mean renumbering, and `DocID` is what `TopK` breaks ties on, what keeps posting lists ascending, and what lets a merge be a concatenation. A full re-index is the only compaction: [D-019](DECISIONS.md). |
| An update spends a DocID | Updating a *committed* document tombstones the old record and appends a new one, so `Len` grows while `Stats` does not. The ceiling `Add` enforces is 2³²−1 ids, not documents, and a corpus updated hot enough reaches it first. Unmeasured. |
| Durability stops at fsync | Atomic against process death; best-effort against power loss, with no platform write barrier: [FORMAT.md §6](FORMAT.md). |
| Caller-held scorer data is not persisted | A signal whose data is not an `engine.Document` field lives in your program, so `Commit` does not write it and `Open` does not restore it. Rebuild it keyed by `Document.Key` after every open. |
| A tokenizer mismatch is refused when you open, not when you query | A directory indexed with one tokenizer and queried with another answers zero hits on everything, with nothing to report. `Open` recomputes one live document's token count against the number already on disk and returns `ErrTokenizerMismatch` instead — so pass the same `WithTokenizer` the commit was made with. It compares a **count**, so a tokenizer that preserves token count (a stemmer does) still slips through, and the tokenizer's identity is deliberately not stored because a label can lie: [D-023](DECISIONS.md), [FORMAT §8](FORMAT.md). |

## Ranking quality

| Limitation | Detail |
| --- | --- |
| **Graph proximity measured worthless** | +0.0000 nDCG@10 at its best fusion weight — no weight in the tested grid beats the baseline, and at 0.1 and below the arm is the baseline exactly — and −0.1227 if fused at equal weight. Kept for the milestone 1 assertions and marked in its package doc: [D-005](DECISIONS.md). Do not enable `scorer/graph` expecting quality, and weight it down if you enable it at all. |
| The graph walk that replaces it is **unmeasured** | `graph.NewPPR` removes the mechanism milestone 4 blamed — the tie group — and costs 5.8× less per query than the BFS. Whether that recovers any nDCG is **not known**: the arms are registered in `weft-eval run` and have not been run. It also carries PageRank's degree bias, which on a citation corpus ranks the paper everything cites above the paper the query is about; the lever is degree normalization and it is not measured either: [FINDINGS milestone 15](FINDINGS.md). |
| Fusion weights have no source | `FuseWeighted` exists, but nothing decides what the weights should be. Hand-tuning per corpus reintroduces the per-deployment burden this design avoids; learning them from judgments is unbuilt. Use `Fuse` unless you have measured your own. The one exception this project publishes is the 0.1 graph discount, which milestone 4 did measure and which `cmd/weft` and `examples/basic` therefore use ([FINDINGS milestone 4 §7](FINDINGS.md)). |
| Vector recall falls as documents are deleted | `Index.Nearest` promises at least k candidates when the index holds k vectors. Tombstones are filtered after a segment has widened its own probe, so fewer than k can survive — the widening loop does not know about them: [D-019](DECISIONS.md). |
| A field match is not normalized by length | `Document.Fields` and `text.NewField` give field-scoped search, but a document's stored token count is `Text` plus every field with no record of which was which — so there is nothing to normalize a field match against. `NewField` sets BM25's `B` to 0 rather than dividing a five-token title by a five-thousand-token body and ranking by brevity. What that gives up is separating two equally good field matches by field length. A per-field count is a format v6: [FORMAT §8](FORMAT.md). |
| No embedding generation | Vectors are supplied by the caller. |

## What you cannot ask

| Limitation | Detail |
| --- | --- |
| The query syntax has no boolean algebra | `query.Parse` speaks a flat clause list with `+` and `-`, which is what rank fusion can express: a union of votes with an intersection and a difference applied to it. There are **no parentheses, no OR and no nesting**, so `a OR (b AND c)` cannot be written. Expressing it would mean a query tree and an evaluator over it — a different engine, not a parser. What the flat list buys is that every query is a set of streams you can see and weight. |
| A constraint needs a `Fuser`, and `pkg/query` is the one you would have written | Rank fusion is a union of votes, so a scorer that returns only matching documents ranks rather than filters, and what it refused returns on another stream's vote with no error. `query.Must` and `query.MustNot` are that `Fuser`, composable and indexed by stream position. What is still yours: any constraint whose scorer you write, and the rule that **a restricting scorer must return every match rather than its top k** — one truncated to `k` excludes everything below its own cut: [ADOPTION §8](ADOPTION.md), `ExampleFuser`. |
| Phrase search re-reads the document | `Posting` carries a term's document and frequency, **not its positions**, so no exact phrase can be decided from the index. `query.Phrase` wraps another scorer and checks its candidates against `Document.Text`, at one record decode each — so the cost is bounded by what the inner scorer nominated rather than by the corpus, and surviving candidates keep its scores. What that does not buy is highlighting or a proximity window, both of which want the positions. A position index is a format change: [FORMAT §8](FORMAT.md). |
| A range needs the caller to encode | `Range` compares terms as bytes, so a number is only rangeable if it was indexed in an order-preserving encoding. `query.EncodeInt` and `EncodeTime` are that, and a value encoded one way and queried another matches nothing with nothing to report. There is no numeric index: the cost scales with how many **distinct values** the field holds, not how many documents hold them. |
| `Fuzzy` counts bytes, not runes | A substitution inside a multi-byte character is several edits, so edit-distance matching is accurate for Latin text and poor for Korean, Japanese and Chinese — where one typo is a syllable. A rune-aware form is a different function, not a flag on this one. |
| A term pattern scans the vocabulary | `query.Glob` narrows by the pattern's literal prefix and then examines every term that survives, because a segment's terms are a map in memory even though the `terms` section is sorted on disk. The vocabulary is bounded by the language and not by the corpus — 2.7 MB against 626 MiB of documents on the evaluation index — which is what makes this affordable rather than fast. `query.MaxTerms` bounds the expansion at 4096. |
| No CJK tokenization, but the tokenizer is replaceable | The default splits on whitespace and punctuation only, so a CJK run collapses into one token — for Korean that means `"검색엔진을"` and `"검색엔진"` are different terms and a query for the second finds nothing. `engine.WithTokenizer` replaces it at index time and query time together, and `ExampleWithTokenizer` shows a ten-line Hangul bigram tokenizer doing it. weft ships **no** second tokenizer and no morphological analysis: that would collide with the zero-dependency constraint, and a seam with a menu in it is not a seam ([D-022](DECISIONS.md)). |

## Rules the caller has to keep

Not limitations of what weft computes, but preconditions it does not check for you.

| Limitation | Detail |
| --- | --- |
| Scorers must share one index | `DocID` is index-relative, so scorers built against different indexes fuse unrelated documents. A precondition on `Search`, not a check: [FINDINGS §3.4](FINDINGS.md). |
| A graph `Adjacency` is a snapshot | It resolves every edge once, at construction, and nothing invalidates it. A document added, updated or deleted afterwards is not in it, and a stale graph answers plausibly rather than erroring. Rebuild after ingest, which is the rule every caller-held side store already follows. |
| A field's terms and `Text`'s do not mix | A term in `Text` and the same term in a field are two different terms. That is what makes a scoped query mean anything; a caller wanting a word findable both ways puts it in both places, which counts its tokens twice toward the document's length. |
