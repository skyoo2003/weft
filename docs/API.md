# weft's own API

`weftd` answers on two surfaces. Everything under `_search` is [the OpenSearch subset](LIMITATIONS.md) — weft answering a question somebody else's protocol knows how to ask, so that an existing client connects without being changed. This document is the other one: the things weft does that the OpenSearch DSL has no way to ask for at all.

The route is `POST /{index}/_weft/search`. It lives under `/_weft/` because what OpenSearch has no name for gets a name OpenSearch does not use ([DECISIONS](DECISIONS.md) D-033), and it cannot collide with an index of yours because an index name may not begin with an underscore.

```bash
curl -XPOST localhost:9200/papers/_weft/search -H 'Content-Type: application/json' -d '{
  "streams": [
    {"match": {"text": "rank fusion"}},
    {"knn": {"vec": {"vector": [0.1, 0.9, 0.2], "k": 10}}},
    {"function_score": {"gauss": {"published": {}}}},
    {"weft_graph": {"seeds": ["doc-3"]}}
  ],
  "weights": [1, 1, 1, 0.1],
  "size": 10,
  "depth": 100,
  "breakdown": true
}'
```

## The three things it says that `_search` cannot

### 1. The request *is* the stream list

On the compatibility surface, fusing several signals means a `hybrid` clause wrapping a list of sub-queries, and weighting them means `weights` — which is weft's extension to that wrapper, because OpenSearch puts normalization in a search pipeline instead (D-030).

Here the list is the query. There is no wrapper to open, and a weight is what a positional weight always was: **an index into a list the client wrote**. That is the whole reason `fusion.FuseWeighted` can exist without fusion learning what a scorer is, arriving as the shape of the protocol rather than as a field inside a clause.

A `streams` entry is one leaf clause, spelled exactly as `_search` spells it. All eleven leaf kinds work here — `match`, `match_phrase`, `term`, `terms`, `prefix`, `wildcard`, `fuzzy`, `range`, `exists`, `knn`, `function_score` — plus `weft_graph` and one level of `bool`. They are compiled by the same function `_search` compiles a leaf with, so a clause that works on one surface works on the other by cutting and pasting it, and every refusal in [LIMITATIONS](LIMITATIONS.md) is inherited rather than re-listed.

### 2. `breakdown` — where each stream ranked the document before fusion

```json
{"hits": {"hits": [
  {"_id": "rrf",       "_score": 0.048, "breakdown": [0, 2, 1, null]},
  {"_id": "changelog", "_score": 0.016, "breakdown": [null, null, 0, null]}
]}}
```

One column per stream, in the order the request named them, holding that stream's own rank for this document **before** fusion saw any of them. `null` is the stream having no opinion — the document is absent from it, deliberately withheld, or simply below the cut. This is the `-` column [`examples/breakdown`](../examples/breakdown/main.go) prints and `weft search -breakdown` prints, and an OpenSearch response has nowhere to put it: a client on the compatibility surface can see that a document was ranked and never why.

Two details are load-bearing.

**A column is a stream you wrote, not a scorer it became.** A two-token `match` is two `query.Glob` streams, so the list the fuser sees is longer than the list in your request. The columns follow your request. When one entry became several streams, the column reports the **best** rank among them — an average would be a score by another name, and this engine never compares scores across streams.

**`null` is not zero.** A document nothing nominated would otherwise look like one everything ranked first.

The breakdown is taken from the streams that were actually fused, not from a second round of calls to each scorer. The two can disagree the moment a scorer is not deterministic, which is exactly what a breakdown exists to rule out.

### 3. `depth` is not `size`

`size` is the page. `depth` is how many candidates the fusion produces before the page is cut.

On `_search` those are tangled: a `knn` clause's `k` raises the candidate depth of *every* stream at once, and a text-only query has no way to ask for a deeper fusion than the page it wants. They were always two ideas, so here they are two numbers.

A deeper fusion changes the ranking and not only the length of it — rank fusion reads position, and a document that only appears at depth 80 in one stream cannot be voted for by that stream at depth 10.

## Refusals

The rule the whole server follows applies here unchanged: **a query this engine cannot express returns 400 or 501 with a reason, never 200 with an empty hit list** (D-026).

| Request | Answer |
| --- | --- |
| `streams` empty or absent | 400 — the request *is* the stream list, so an empty one is a body that forgot its query |
| `weights` shorter or longer than `streams` | 400 — a weight is positional, so an unequal list weights a stream you did not mean |
| `depth` below 1, or over 10,000 | 400 — the collector allocates for the depth it is given, so it is refused before it is allocated |
| `from` + `size` over 10,000 | 400 — a page here is the whole prefix fetched and then cut |
| a stream kind nobody registered | 400, quoting the name |
| anything `_search` refuses | the same status and the same reason, because it is the same compiler |

A weight of `0` is **not** the short spelling of "do not vote": it removes the document from the fused result entirely (D-029). Discount a stream instead — `0.01` votes weakly, `0` excludes.

## The rest of the native surface

Four routes predate this one and answer questions about the index rather than over it.

| Route | What it answers |
| --- | --- |
| `GET /{index}/_weft/terms?prefix=&field=&limit=` | walks the term space, with each term's document count |
| `GET /{index}/_weft/postings?term=&field=&limit=` | one term's postings, ids resolved back to keys |
| `POST /{index}/_weft/query` | runs weft's own query string — `{"q": "+fusion -draft"}`, the syntax `pkg/query.Parse` documents |
| `POST /{index}/_weft/scrub` | verifies the committed directory on disk |

`query_string` on `_search` stays a 501 while `/_weft/query` answers, and that is not a contradiction: the refusal is about reading Lucene's language as if it were this one, and here a client has asked for weft's language by name.

## What this surface does not change

It is not a second engine and it did not become the product. `pkg/` changed **zero lines** for it, `go list -m all` still prints one module, and `opensearch-py` still drives `weftd` unmodified — `make compat` is that check.

The production warning applies here exactly as it does everywhere else: sustained load collapses at 27 queries a second rather than degrading, a commit holds the writer for 11 seconds on a 20,000-document batch, and there is no authentication and no TLS. [STATUS](STATUS.md) and [LIMITATIONS](LIMITATIONS.md) are the full account.
