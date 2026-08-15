// SPDX-License-Identifier: Apache-2.0

// Package eval measures ranking quality. It is milestone 4's instrument, not
// part of weft's library contract, which is why it lives under internal/: an
// arm, a qrels file and a bootstrap interval are things this repo does once to
// answer a question, not things a caller embedding weft should be able to
// depend on. Keeping it here also keeps engine's exported API — and the golden
// file guarding it — untouched by the measurement.
//
// It imports engine and the scorers. Nothing imports it.
package eval

import (
	"math"
	"slices"
)

// NDCG returns nDCG@k of ranked against qrels, where ranked holds document
// keys best-first and qrels maps a judged key to its relevance grade.
//
// The gain is LINEAR — grade rel contributes rel/log2(i+1) at rank i — and that
// is not the textbook definition. Most write-ups (and this milestone's plan,
// before it was checked) use the exponential gain 2^rel - 1. trec_eval's
// ndcg_cut does not, and BEIR reports trec_eval's numbers via pytrec_eval, so
// exponential gain would put every number this milestone publishes on a scale
// nobody else uses. testdata/ndcg_reference.json pins the difference: on qrels
// {a:2, b:1} ranked [b, a], linear gives 0.8597 and exponential 0.7967. That
// fixture exists precisely because the two are indistinguishable on any ranking
// that is already ideal.
//
// Everything else the reference settled, none of it guessed:
//
//   - An unjudged document occupies its rank and contributes nothing. It is not
//     skipped over, so a document the graph scorer surfaced but the assessors
//     never saw actively costs nDCG. That is the structural bias against the
//     graph arm, and it is why TREC-COVID's judgment depth was the reason to
//     pick it (docs/DATASETS.md section 3).
//   - A judged grade of 0 is indistinguishable from being unjudged.
//   - A negative grade — some TREC qrels use -1 for "explicitly not judged" —
//     is clamped to 0 rather than subtracting from DCG.
//   - IDCG is truncated at k like the run is. Without that, nDCG@10 could never
//     reach 1.0 on a pool with more than 10 relevant documents, and TREC-COVID
//     averages 493.5 judgments per query.
//   - IDCG == 0 (a query with no relevant document at all) returns 0, not NaN.
//
// Precondition: ranked contains no duplicates. A repeated key would be counted
// twice. This is unchecked because it cannot happen through Evaluate — fusion
// returns distinct DocIDs and engine rejects duplicate keys at Add — and a scan
// here would cost a map allocation per query to re-derive a guarantee the index
// already makes.
func NDCG(ranked []string, qrels map[string]int, k int) float64 {
	if k <= 0 {
		return 0
	}
	var dcg float64
	for i, key := range ranked {
		if i >= k {
			break
		}
		// A missing key reads 0 from the map, which is the same answer as a
		// judged 0 — deliberately, per the reference above.
		if rel := qrels[key]; rel > 0 {
			dcg += float64(rel) / math.Log2(float64(i+2))
		}
	}
	idcg := idealDCG(qrels, k)
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// idealDCG is the DCG of the best ranking qrels permits, truncated to k.
//
// Grades at or below 0 are dropped before sorting rather than sorted to the
// back, so they cannot occupy one of the k ideal slots and depress IDCG.
func idealDCG(qrels map[string]int, k int) float64 {
	rels := make([]int, 0, len(qrels))
	for _, rel := range qrels {
		if rel > 0 {
			rels = append(rels, rel)
		}
	}
	slices.Sort(rels)
	slices.Reverse(rels)

	var idcg float64
	for i, rel := range rels {
		if i >= k {
			break
		}
		idcg += float64(rel) / math.Log2(float64(i+2))
	}
	return idcg
}
