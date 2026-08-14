#!/usr/bin/env python3
"""Generate bm25_reference.json: BM25 ground truth from rank_bm25.

This closes the one PRD Success Metrics row nothing had claimed yet — "BM25
implementation matches the standard formula (score error against a reference
implementation within tolerance)". It has to happen before any arm number is
published: if the text baseline is wrong, the graph delta measured on top of it
says nothing about graph proximity.

Like gen_ndcg_reference.py this is a one-off generator, never invoked by Go, and
the golden file it writes is the committed evidence.

    /tmp/weft-ref-venv/bin/pip install rank_bm25
    /tmp/weft-ref-venv/bin/python gen_bm25_reference.py > bm25_reference.json

## The alignment, stated rather than assumed

rank_bm25's BM25Okapi does NOT compute the same IDF weft does, and running it
unmodified would produce a disagreement that says nothing about weft:

    BM25Okapi:  ln(N - n + 0.5) - ln(n + 0.5)      classic; negative for n > N/2
    weft:       ln(1 + (N - n + 0.5)/(n + 0.5))    Lucene's form; always > 0

BM25Okapi patches the negative case with an epsilon floor derived from the mean
IDF, which is a third formula again. weft chose the Lucene form deliberately
(pkg/scorer/text/text.go documents why), so the honest comparison overwrites
BM25Okapi's idf table with weft's formula and compares everything else: term
frequency saturation, the length normalisation, avgdl, and the sum over query
term occurrences. Those are where a length-normalisation bug would live.

What that means for the result: this file verifies the machinery around the IDF,
not the IDF expression itself. The IDF expression is checked against Lucene's
published BM25Similarity form, and the arithmetic of the two candidate forms is
compared directly in the Go test so the substitution cannot hide a swap.

## Tokenization

weft's engine.Tokenize lowercases and splits on every rune that is neither a
letter nor a digit. For ASCII input that is exactly re.findall(r"[a-z0-9]+",
s.lower()), which is what this script uses. The corpus below is deliberately
ASCII-only: matching Go's Unicode letter classes in Python would be a second
implementation of something weft is explicitly not trying to be right about yet
(CJK runs stay glued into one token — see Tokenize).
"""

import json
import math
import re
import sys

from rank_bm25 import BM25Okapi

K1 = 1.2
B = 0.75

# Chosen for what each document does to the formula, not for realism:
#
#   "beta" appears in 5 of 8 documents — more than half, so the classic IDF
#          would go negative here and weft's does not. This is the case the IDF
#          choice exists for.
#   d3     is one token long, d5 is far longer than average: the length
#          normalisation term spans a wide range.
#   d1     repeats "alpha" three times: term frequency saturation.
#   d7     shares no term with any query: must be absent from weft's candidates
#          rather than present at score 0.
CORPUS = [
    ("d0", "alpha beta gamma"),
    ("d1", "alpha alpha alpha beta"),
    ("d2", "beta gamma delta epsilon zeta"),
    ("d3", "alpha"),
    ("d4", "beta delta"),
    ("d5", "gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi beta"),
    ("d6", "epsilon zeta eta"),
    ("d7", "omicron pi rho"),
]

QUERIES = [
    ("q_single_rare", "lambda"),
    ("q_single_common", "beta"),
    ("q_two_terms", "alpha beta"),
    # A repeated query term must count twice: the BM25 sum is over occurrences
    # in the query, not over the distinct set.
    ("q_repeated_term", "alpha alpha beta"),
    # "sigma" is in no document and must contribute nothing rather than shifting
    # the other terms.
    ("q_unknown_term", "alpha sigma"),
    ("q_long", "gamma delta epsilon zeta eta"),
    # Punctuation and case, to pin the tokenizer alignment.
    ("q_punctuated", "Alpha, BETA!  gamma?"),
]


def tokenize(s: str) -> list[str]:
    return re.findall(r"[a-z0-9]+", s.lower())


def main() -> None:
    keys = [k for k, _ in CORPUS]
    tokens = [tokenize(text) for _, text in CORPUS]
    n_docs = len(tokens)

    bm25 = BM25Okapi(tokens, k1=K1, b=B)

    # Overwrite every IDF with weft's form. nd is not exposed, so document
    # frequency is recounted here from the same token lists BM25Okapi was built
    # from.
    df: dict[str, int] = {}
    for toks in tokens:
        for term in set(toks):
            df[term] = df.get(term, 0) + 1
    for term, n in df.items():
        bm25.idf[term] = math.log(1 + (n_docs - n + 0.5) / (n + 0.5))

    queries = []
    for qid, qtext in QUERIES:
        qtoks = tokenize(qtext)
        scores = bm25.get_scores(qtoks)
        queries.append(
            {
                "id": qid,
                "text": qtext,
                "tokens": qtoks,
                "scores": {keys[i]: float(scores[i]) for i in range(n_docs)},
            }
        )

    out = {
        "generated_by": "rank_bm25 0.2.2 BM25Okapi with weft's IDF substituted",
        "k1": K1,
        "b": B,
        "idf_form": "ln(1 + (N - n + 0.5)/(n + 0.5))",
        "idf_substituted": True,
        "tokenizer": "re.findall(r'[a-z0-9]+', s.lower()) — ASCII-equivalent to engine.Tokenize",
        "avgdl": sum(len(t) for t in tokens) / n_docs,
        "note": (
            "Ground truth for pkg/scorer/text. Verifies term frequency "
            "saturation, length normalisation, avgdl and the sum over query "
            "occurrences. The IDF expression itself is substituted rather than "
            "compared — see the module docstring in gen_bm25_reference.py."
        ),
        "corpus": [{"key": k, "text": t} for k, t in CORPUS],
        "queries": queries,
    }
    json.dump(out, sys.stdout, indent=2, sort_keys=False)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
