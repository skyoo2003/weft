#!/usr/bin/env python3
"""Encode the 50 TREC-COVID queries with SPECTER2, for the vector arm.

Unlike the other two scripts in this directory, this one's output is NOT a
committed golden: it writes .eval-data/query-vectors.jsonl, which is gitignored
data. It lives here because it is the same kind of thing — a one-off Python step,
run outside the Go build, whose result weft consumes as externally injected input.

## Why this script has to exist

Document vectors come from Semantic Scholar's `embedding.specter_v2` field, so they
cost nothing and add no dependency. Query vectors cannot: SPECTER embeds papers, and
no API will embed an arbitrary question. Without them the vector scorer has no
opinion at all and the baseline arm degenerates to text alone — a weaker baseline,
which biases the whole measurement in favour of the graph arm (docs/EVAL.md section
5.5).

The PRD puts "embedding generation and model inference" out of scope *for weft*, and
that still holds: nothing here enters the Go module, `make deps` still prints one
module, and the engine receives float32 vectors it did not compute. Same category as
the document vectors — externally produced, injected.

## Which model, and why it must be checked rather than assumed

The two sides have to come from the same embedding space or cosine similarity
between them is meaningless. Semantic Scholar's `specter_v2` is SPECTER2 with the
proximity adapter. SPECTER2 ships a *separate* adapter for short ad-hoc search
queries, which is exactly this case:

    documents  allenai/specter2_base + allenai/specter2              (proximity)
    queries    allenai/specter2_base + allenai/specter2_adhoc_query  (ad-hoc query)

`--verify` is not optional decoration. It re-encodes documents whose S2 vector we
already have, using the proximity adapter, and reports cosine similarity against
S2's own numbers. If that is not close to 1.0 then the local model is not the model
S2 served, the query vectors would sit in a different space, and the vector arm must
be dropped rather than reported.

## Usage

    /tmp/weft-ref-venv/bin/pip install torch transformers adapters
    /tmp/weft-ref-venv/bin/python gen_query_vectors.py --verify
    /tmp/weft-ref-venv/bin/python gen_query_vectors.py
"""

from __future__ import annotations

import argparse
import json
import math
import os
import sys

DATA = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", ".eval-data")
QUERIES = os.path.join(DATA, "trec-covid", "queries.jsonl")
CORPUS = os.path.join(DATA, "trec-covid", "corpus.jsonl")
S2 = os.path.join(DATA, "s2.jsonl")

BASE = "allenai/specter2_base"
DOC_ADAPTER = "allenai/specter2"
QUERY_ADAPTER = "allenai/specter2_adhoc_query"


def load_model(adapter: str):
    from adapters import AutoAdapterModel
    from transformers import AutoTokenizer

    tok = AutoTokenizer.from_pretrained(BASE)
    model = AutoAdapterModel.from_pretrained(BASE)
    model.load_adapter(adapter, source="hf", load_as="active", set_active=True)
    model.eval()
    return tok, model


def encode(tok, model, texts: list[str], batch: int = 8) -> list[list[float]]:
    import torch

    out: list[list[float]] = []
    for i in range(0, len(texts), batch):
        chunk = texts[i : i + batch]
        enc = tok(chunk, padding=True, truncation=True, return_tensors="pt", max_length=512)
        with torch.no_grad():
            hidden = model(**enc).last_hidden_state
        # SPECTER2 uses the CLS token, not mean pooling. Mean pooling produces a
        # plausible-looking vector in the wrong space, which is precisely the kind of
        # error --verify exists to catch.
        out.extend(hidden[:, 0, :].tolist())
    return out


def cosine(a: list[float], b: list[float]) -> float:
    dot = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(y * y for y in b))
    if na == 0 or nb == 0:
        return 0.0
    return dot / (na * nb)


def read_jsonl(path: str):
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line:
                yield json.loads(line)


def verify(n: int) -> int:
    """Compare locally computed document vectors against Semantic Scholar's."""
    # Corpus keys first, so the sample can only contain documents whose text is actually
    # here. The cache is allowed to hold records for documents outside this corpus — it
    # can come from a larger or older one, and `weft-eval build` reports and ignores those
    # — and sampling before checking membership meant the corpus pass below found fewer
    # texts than keys, verifying less than asked without saying so, or nothing at all and
    # failing on an empty list of similarities instead of reporting anything.
    corpus_keys = {rec["_id"] for rec in read_jsonl(CORPUS)}
    have: dict[str, list[float]] = {}
    for rec in read_jsonl(S2):
        if rec.get("vec") and rec["key"] in corpus_keys:
            have[rec["key"]] = rec["vec"]
            if len(have) >= n:
                break
    if not have:
        print(
            f"no vectors in {S2} for any document in {CORPUS}; run `weft-eval prepare` first, "
            "or check that the cache and the corpus describe the same snapshot",
            file=sys.stderr,
        )
        return 1

    tok, model = load_model(DOC_ADAPTER)
    texts: list[str] = []
    keys: list[str] = []
    for rec in read_jsonl(CORPUS):
        if rec["_id"] in have:
            # SPECTER2's documented document input: title, separator, abstract.
            texts.append((rec.get("title") or "") + tok.sep_token + (rec.get("text") or ""))
            keys.append(rec["_id"])
        if len(keys) >= len(have):
            break

    local = encode(tok, model, texts)
    sims = sorted(cosine(local[i], have[keys[i]]) for i in range(len(keys)))
    print(f"verified {len(sims)} documents against Semantic Scholar's specter_v2")
    print(f"  cosine min    {sims[0]:.6f}")
    print(f"  cosine median {sims[len(sims) // 2]:.6f}")
    print(f"  cosine max    {sims[-1]:.6f}")
    if sims[0] < 0.99:
        print(
            "\nFAIL: the local model is not the model Semantic Scholar served.\n"
            "Query vectors would live in a different space from the document vectors,\n"
            "so the vector arm must be dropped rather than reported. See the module\n"
            "docstring.",
            file=sys.stderr,
        )
        return 1
    print("\nOK: same embedding space. Query vectors are comparable to document vectors.")
    return 0


def sanity(query_vecs: dict[str, list[float]]) -> bool:
    """Check the query adapter's output is usable against proximity document vectors.

    --verify establishes that our document vectors and Semantic Scholar's are the
    same. It says nothing about whether the *query* adapter lands in a space where
    cosine against those documents means anything, and the two adapters are
    different networks. AllenAI documents them as a matched pair, but a documented
    pairing is still an assumption.

    The check that does not depend on the documentation: for each query, compare
    mean cosine against its judged-relevant documents with mean cosine against
    randomly chosen ones. If the space is compatible, relevant should win clearly.
    If it does not, the vector arm is contributing noise and reporting it as a
    signal would be worse than dropping it.

    Returns False only for that conclusion. A check that could not run — no document
    vectors cached yet, no judged document among them — returns True: it has found
    nothing wrong with the query vectors, and refusing to write them before
    `weft-eval prepare` has ever run would make this script unusable in the order the
    pipeline is actually executed. The caller reports which of the two it got.
    """
    import random

    vecs: dict[str, list[float]] = {}
    for rec in read_jsonl(S2):
        if rec.get("vec"):
            vecs[rec["key"]] = rec["vec"]
    if not vecs:
        print("\nsanity check skipped: no document vectors yet", file=sys.stderr)
        return True

    rel: dict[str, list[str]] = {}
    with open(os.path.join(DATA, "trec-covid", "qrels", "test.tsv")) as f:
        head = f.readline().rstrip("\n").split("\t")
        qi, di, si = head.index("query-id"), head.index("corpus-id"), head.index("score")
        for line in f:
            cells = line.rstrip("\n").split("\t")
            if len(cells) > max(qi, di, si) and int(cells[si]) > 0 and cells[di] in vecs:
                rel.setdefault(cells[qi], []).append(cells[di])

    pool = sorted(vecs)  # Sorted so the sample is reproducible with the seed below.
    rng = random.Random(20260814)
    wins = compared = 0
    rel_sum = rnd_sum = 0.0
    for qid, docs in rel.items():
        qv = query_vecs.get(qid)
        if qv is None or not docs:
            continue
        r = sum(cosine(qv, vecs[d]) for d in docs) / len(docs)
        sample = rng.sample(pool, min(len(docs), len(pool)))
        n = sum(cosine(qv, vecs[d]) for d in sample) / len(sample)
        rel_sum += r
        rnd_sum += n
        compared += 1
        if r > n:
            wins += 1

    if compared == 0:
        print("\nsanity check skipped: no judged document has a vector yet", file=sys.stderr)
        return True

    # One-sided sign test against a coin flip. A bare majority used to pass, and a
    # bare majority is what an adapter embedding in the wrong space produces: with 50
    # queries, 26 wins is the median outcome of pure noise, so the check accepted
    # exactly the result it exists to refuse. math.comb keeps this exact and keeps the
    # script on the standard library.
    p_value = sum(math.comb(compared, w) for w in range(wins, compared + 1)) / 2**compared

    print(f"\nsanity check over {compared} queries with judged documents that have vectors")
    print(f"  mean cosine to judged-relevant  {rel_sum / compared:.4f}")
    print(f"  mean cosine to random           {rnd_sum / compared:.4f}")
    print(f"  relevant wins                   {wins}/{compared}  (sign test p = {p_value:.2e})")

    # Both halves have to hold, because they fail differently. The sign test asks
    # whether relevant beats random more often than chance; the aggregate margin asks
    # whether it beats it by anything worth fusing. An adapter can win narrowly and
    # often on near-identical cosines, and that is a stream of noise with a consistent
    # tilt. The threshold is 0.001 rather than a conventional 0.05 because this is a
    # gate on a published baseline, not a hypothesis test someone will replicate: on
    # the run recorded in docs/EVAL.md section 5.4 it was 50/50 wins, p = 8.9e-16,
    # with means 0.7620 against 0.6842, so the bar is nowhere near the real result.
    if p_value >= 0.001 or rel_sum <= rnd_sum:
        print(
            "\nFAIL: the query adapter does not separate relevant from random by more than\n"
            "chance would. The vector arm would be contributing noise. Drop it rather than\n"
            "report it.",
            file=sys.stderr,
        )
        return False
    return True


def generate(out_path: str) -> int:
    queries = list(read_jsonl(QUERIES))
    if not queries:
        print(f"no queries in {QUERIES}", file=sys.stderr)
        return 1

    tok, model = load_model(QUERY_ADAPTER)
    # Queries go in bare. The ad-hoc query adapter is trained on the query alone;
    # wrapping one in the title[SEP]abstract shape used for documents would feed it
    # an input it never saw.
    vecs = encode(tok, model, [q["text"] for q in queries])

    # Checked before the file exists, not after. Printed next to a written file, the
    # conclusion "these vectors are noise, drop the arm" is advice nothing acts on:
    # `weft-eval run` finds query-vectors.jsonl, reports a vector arm under its usual
    # name, and that arm is the baseline the entire graph delta is measured against. An
    # unusable file that was never written cannot be picked up by mistake, which is the
    # same reason prepare refuses to record a corpus as unjoinable on no evidence.
    if not sanity({q["_id"]: v for q, v in zip(queries, vecs)}):
        print(f"not writing {out_path}", file=sys.stderr)
        return 1

    with open(out_path, "w") as f:
        for q, v in zip(queries, vecs):
            # The model goes in the file, not only in the log below. A query vector
            # from another adapter or base revision carries the right id and the right
            # text, so every pairing check the loader has passes while the arithmetic
            # is cosine similarity between two embedding spaces. The document side
            # records its model for the same reason; see checkVectorModels.
            json.dump({"id": q["_id"], "text": q["text"], "vec": v,
                       "model": f"{BASE}+{QUERY_ADAPTER}"}, f)
            f.write("\n")
    print(f"wrote {len(queries)} query vectors of dimension {len(vecs[0])} to {out_path}")
    print(f"  model {BASE} + {QUERY_ADAPTER}")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--verify", action="store_true",
                    help="check the local model reproduces Semantic Scholar's document vectors")
    ap.add_argument("--verify-n", type=int, default=16, help="documents to verify against")
    ap.add_argument("--out", default=os.path.join(DATA, "query-vectors.jsonl"))
    args = ap.parse_args()

    if args.verify:
        return verify(args.verify_n)
    return generate(args.out)


if __name__ == "__main__":
    sys.exit(main())
