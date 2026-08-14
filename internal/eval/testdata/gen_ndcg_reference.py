#!/usr/bin/env python3
"""Generate ndcg_reference.json: nDCG@10 ground truth from pytrec_eval.

This is a one-off reference generator, not part of the build. weft has zero
external dependencies (PRD Success Metrics) and this script is never invoked by
Go code; it exists so the numbers in ndcg_reference.json have a stated origin.
The golden file it writes IS committed — it is the evidence, and a reader who
does not trust internal/eval/ndcg.go can re-derive it here.

Why a reference at all: milestone 4's whole output is a number. If our nDCG
disagrees with the one BEIR reports (pytrec_eval's ndcg_cut_10), every arm
comparison is measured on a scale nobody else uses, and the falsification
condition is decided by a formula bug.

Usage:
    python3 -m venv /tmp/weft-ref-venv
    /tmp/weft-ref-venv/bin/pip install pytrec_eval-terrier
    /tmp/weft-ref-venv/bin/python gen_ndcg_reference.py > ndcg_reference.json

Fixtures are chosen to discriminate between implementations that would
otherwise look identical, not to cover happy paths:

  swapped_grades     linear gain (rel) vs exponential gain (2^rel - 1)
  unjudged_at_top    does an unjudged doc consume a rank slot, or vanish?
  judged_zero_*      is a judged rel=0 doc different from an unjudged one?
  relevant_at_11     is the cut applied to the run before scoring?
  ideal_beyond_k     is IDCG truncated at k, or computed over all judged docs?
  no_relevant        what happens when IDCG is 0 (trec_eval may skip the query)
"""

import json
import sys

import pytrec_eval

K = 10

# (name, qrels, ranked run best-first, why this case exists)
CASES = [
    (
        "perfect",
        {"a": 2, "b": 1, "c": 0},
        ["a", "b", "c"],
        "ideal order scores exactly 1.0 whatever the gain function is",
    ),
    (
        "swapped_grades",
        {"a": 2, "b": 1},
        ["b", "a"],
        "discriminates exponential gain from linear: 0.7967 vs 0.8597",
    ),
    (
        "unjudged_at_top",
        {"a": 2, "b": 1},
        ["x", "a", "b"],
        "an unjudged doc at rank 1 must push a and b down a slot, not vanish",
    ),
    (
        "judged_zero_at_top",
        {"a": 2, "b": 1, "z": 0},
        ["z", "a", "b"],
        "judged-irrelevant must behave identically to unjudged_at_top",
    ),
    (
        "all_unjudged",
        {"a": 1},
        ["x", "y", "z"],
        "retrieving nothing judged is 0.0, not an error",
    ),
    (
        "no_relevant",
        {"a": 0, "b": 0},
        ["a", "b"],
        "IDCG == 0. trec_eval's answer here is the one we cannot guess",
    ),
    (
        "short_run",
        {"a": 1, "b": 1, "c": 1},
        ["a"],
        "IDCG counts relevant docs we never retrieved: 1/2.1309",
    ),
    (
        "relevant_at_11",
        {"a": 1},
        ["x1", "x2", "x3", "x4", "x5", "x6", "x7", "x8", "x9", "x10", "a"],
        "rank 11 is past the cut and must contribute nothing",
    ),
    (
        "relevant_at_10",
        {"a": 1},
        ["x1", "x2", "x3", "x4", "x5", "x6", "x7", "x8", "x9", "a"],
        "the boundary the case above brackets: rank 10 does count",
    ),
    (
        "ideal_beyond_k",
        # 12 relevant docs, so the ideal ranking is longer than the cut.
        {f"r{i}": (2 if i % 2 == 0 else 1) for i in range(12)},
        [f"r{i}" for i in range(10)],
        "IDCG truncated at k, else nDCG can never reach 1.0 on a deep pool",
    ),
    (
        "graded_mixed",
        {"a": 2, "b": 0, "c": 1, "d": 2, "e": 0, "f": 1},
        ["b", "a", "x", "f", "e", "d"],
        "the realistic shape: grades, zeros and unjudged interleaved",
    ),
    (
        "negative_grade",
        # Some TREC qrels use -1 for "explicitly not judged". TREC-COVID is
        # 0/1/2, but the loader is a trust boundary and a negative gain would
        # subtract from DCG rather than being ignored.
        {"a": 2, "n": -1, "b": 1},
        ["n", "a", "b"],
        "a -1 grade must behave as 0, matching unjudged_at_top exactly",
    ),
]


def main() -> None:
    qrels = {name: {d: int(r) for d, r in rel.items()} for name, rel, _, _ in CASES}
    # Scores are strictly decreasing so the run order is unambiguous. trec_eval
    # breaks score ties on docno, which would make the fixture's meaning depend
    # on a detail we are not trying to measure.
    run = {
        name: {doc: float(len(ranked) - i) for i, doc in enumerate(ranked)}
        for name, _, ranked, _ in CASES
    }

    evaluator = pytrec_eval.RelevanceEvaluator(qrels, {f"ndcg_cut.{K}"})
    results = evaluator.evaluate(run)

    out = {
        "measure": f"ndcg_cut_{K}",
        "k": K,
        "generated_by": "pytrec_eval-terrier 0.5.10 (trec_eval)",
        "note": (
            "Ground truth for internal/eval.NDCG. Regenerate with "
            "gen_ndcg_reference.py; see that file for why each case exists. "
            "A case with ndcg == null was skipped by trec_eval entirely."
        ),
        "cases": [
            {
                "name": name,
                "why": why,
                "qrels": rel,
                "run": ranked,
                "ndcg": results.get(name, {}).get(f"ndcg_cut_{K}"),
            }
            for name, rel, ranked, why in CASES
        ],
    }
    json.dump(out, sys.stdout, indent=2, sort_keys=False)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
