// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/skyoo2003/weft/pkg/engine"
)

// Sentinel errors from Evaluate. Every one of them reports a misconfigured arm
// or query set rather than a measurement that found nothing, because a
// measurement that quietly reports 0.0 for a broken arm is the one outcome this
// milestone cannot afford: it looks exactly like "the graph signal did not
// help".
var (
	ErrNoScorers = errors.New("eval: arm has no scorers")

	// ErrOverfetchRange rejects a negative multiplier and a k*Overfetch that
	// would overflow int. Both arrive from cmd/weft-eval flags, which is a trust
	// boundary: an overflowed fetch depth wraps negative, engine.Search reads
	// k <= 0 and returns no candidates, and the sweep records a
	// legitimate-looking 0.0 for that configuration.
	ErrOverfetchRange = errors.New("eval: arm Overfetch out of range")

	ErrNoQueries  = errors.New("eval: no queries")
	ErrQueryID    = errors.New("eval: query id is empty")
	ErrDuplicateQ = errors.New("eval: duplicate query id")

	// ErrForeignDocID reports a fused DocID this index cannot resolve. It is a bound
	// check and nothing more, which is worth saying plainly because it sits next to the
	// namespace hazard engine.Search documents and leaves unchecked (docs/FINDINGS.md
	// section 3.4) and could be read as closing it.
	//
	// It does not. DocIDs are assigned from zero in insertion order, so a scorer built
	// against a *different* index of similar size returns ids that are in range here,
	// resolve to unrelated documents, and produce an nDCG over the wrong keys with no
	// error at all. What this catches is the subset that lands outside the bound — a
	// smaller foreign index, or a stale scorer against a corpus that has shrunk.
	//
	// The rest cannot be caught from here. It would take asking a scorer which index it
	// reads, which is the one method that would break every Scorer implementation, so
	// the precondition stays the caller's: pass every scorer the same index. This check
	// is free, because the harness has to resolve each DocID to a key for qrels anyway,
	// and free is the whole argument for it. It is not a guarantee.
	ErrForeignDocID = errors.New("eval: fused DocID is not in this index")

	// ErrDuplicateRanked reports the same DocID at two ranks of one fused result.
	// Distinct from ErrDuplicateDoc, which is the corpus reader's complaint about
	// an input file; this one is about a ranking a Fuser just produced.
	//
	// The bundled fusers cannot produce it — both accumulate into a map keyed by
	// DocID — but Evaluate takes an arbitrary engine.Fuser on purpose, and the
	// interface says nothing about uniqueness. A repeat is not a cosmetic flaw in
	// the ranking: NDCG credits the document's grade at every rank it occupies
	// while idealDCG counts it once, so a duplicated relevant document scores an
	// arm above 1.0. That is a number no reader would question and no arm could
	// have earned, which is the shape of failure this harness exists to refuse.
	ErrDuplicateRanked = errors.New("eval: fused result holds the same DocID twice")

	// ErrForeignQrelDoc reports a relevant judgment naming a document the index
	// does not hold. It is the qrels half of the same pairing hazard: the index and
	// the judgments are separate inputs, produced by separate commands, and nothing
	// but this checks that they describe the same corpus.
	//
	// Unlike a foreign DocID, this one cannot surface as a wrong lookup, because the
	// absent key simply never appears in a ranking. It surfaces as arithmetic: the
	// grade stays in IDCG, where it raises the denominator by a document no arm can
	// possibly retrieve, so every arm loses part of its score. The result is the
	// failure this harness exists to prevent — a run that completes, reports a
	// plausible number, and reads as a ranking that got worse.
	ErrForeignQrelDoc = errors.New("eval: judged document is not in this index")
)

// Arm is one ranking configuration to measure.
//
// Note what it does not have: a field naming a scorer, a count, a flag saying
// "this is the graph arm". It is a list of scorers, a fuser and a depth, and
// Evaluate branches on none of them. That is the milestone 1 claim applied one
// level up — if measuring the graph signal required the harness to know which
// scorer was the graph one, the claim would be true of the engine and false of
// everything built on it. A switch on Name appearing in this file is that
// failure.
//
// The practical dividend: adding an arm is one composite literal, so the
// text-only baseline can run before the embeddings exist.
type Arm struct {
	// Name is for reporting only. Nothing in Evaluate reads it except to
	// annotate errors and results.
	Name string

	// Scorers are handed to engine.Search unchanged. Any number, any kind.
	Scorers []engine.Scorer

	// Fuse is injected rather than imported, which is what lets a sweep vary the
	// RRF rank constant without editing pkg/fusion.
	//
	// Precondition: Fuse must score documents independently of k, using k only
	// to truncate. RRF does — it sums 1/(RRFk + rank) and passes k to TopK — and
	// Overfetch below relies on it. A fuser that normalised by k would make
	// over-fetching change the ranking rather than deepen it.
	Fuse engine.Fuser

	// Overfetch asks each scorer for k*Overfetch candidates and keeps the top k
	// of the fused result. 0 and 1 both mean no over-fetch.
	//
	// This needed no new engine API, which is worth recording:
	// pkg/engine/search.go carries a ponytail marker naming milestone 4 as the
	// moment over-fetch would earn a parameter on Search. It does not need one.
	// Because Fuse is k-independent in scoring, Search(ctx, q, k*m, ...)
	// truncated to k is identical to fusing k*m-deep streams down to k, so the
	// sweep runs against an unmodified engine and the marker can be closed
	// rather than repaid.
	Overfetch int
}

// Query is one evaluation query: what to ask, and what the assessors said.
type Query struct {
	// ID is the qrels query identifier. It keys PerQuery, so it must be unique
	// and non-empty.
	ID string

	// Query is handed to the scorers unchanged. Text, Vector and Seeds are all
	// just fields here; the harness does not know which scorer reads which.
	Query engine.Query

	// Qrels maps a judged document Key to its relevance grade. Absent keys are
	// unjudged, which NDCG treats as grade 0 — see the bias note there.
	Qrels map[string]int
}

// Run is one arm's result over one query set.
type Run struct {
	Arm string

	// K is the rank cut the numbers below were computed at, carried alongside
	// them so a reported figure cannot be mistaken for a different cut.
	K int

	// NDCG is the mean of PerQuery, macro-averaged over queries the way TREC and
	// BEIR report it: every query counts once regardless of how many documents
	// it has judged.
	NDCG float64

	// PerQuery is the diagnostic that makes a delta interpretable. A mean that
	// moved because one query swung wildly is a different finding from a mean
	// that moved because thirty queries each gained a little, and only this
	// tells them apart. BootstrapCI consumes it directly.
	PerQuery map[string]float64
}

// Evaluate runs a over every query in qs and returns its nDCG@k.
//
// Nil scorers and a nil Fuse are not checked here on purpose: engine.Search
// already rejects both, including the typed nil that conditional arm assembly
// actually produces (`var vs *vector.Scorer` left unassigned when the embeddings
// are not ready). Re-checking would be a second copy of a guard that already
// exists, and its errors already name the offending position.
func Evaluate(ctx context.Context, ix *engine.Index, qs []Query, a Arm, k int) (Run, error) {
	if k <= 0 {
		return Run{}, fmt.Errorf("arm %s: k is %d, want > 0", a.Name, k)
	}
	if len(a.Scorers) == 0 {
		// Left to fall through, this fuses zero streams to an empty ranking and
		// reports nDCG 0.0 for every query — indistinguishable from an arm whose
		// scorers all had no opinion.
		return Run{}, fmt.Errorf("arm %s: %w", a.Name, ErrNoScorers)
	}
	if len(qs) == 0 {
		return Run{}, fmt.Errorf("arm %s: %w", a.Name, ErrNoQueries)
	}

	// Checked once for the whole query set, before the first search, because it is a
	// property of the inputs rather than of this arm: a stale pairing costs a message
	// instead of a full sweep over 171K documents. Every arm gets the same answer, so
	// paying for it per arm is the cost of not having a place to cache it.
	if err := checkQrels(ix, qs); err != nil {
		return Run{}, fmt.Errorf("arm %s: %w", a.Name, err)
	}

	fetch, err := fetchDepth(k, a.Overfetch)
	if err != nil {
		return Run{}, fmt.Errorf("arm %s: %w", a.Name, err)
	}

	per := make(map[string]float64, len(qs))
	var sum float64
	for i, q := range qs {
		if q.ID == "" {
			return Run{}, fmt.Errorf("arm %s: query %d: %w", a.Name, i, ErrQueryID)
		}
		// A repeated id would overwrite its own entry in per, so the mean would
		// divide by len(qs) while the map held fewer results — a quiet
		// underestimate rather than a failure.
		if _, dup := per[q.ID]; dup {
			return Run{}, fmt.Errorf("arm %s: query %d: %q: %w", a.Name, i, q.ID, ErrDuplicateQ)
		}

		keys, err := rankedKeys(ctx, ix, q.Query, fetch, k, a)
		if err != nil {
			return Run{}, fmt.Errorf("arm %s: query %q: %w", a.Name, q.ID, err)
		}

		score := NDCG(keys, q.Qrels, k)
		per[q.ID] = score
		sum += score
	}

	return Run{
		Arm:      a.Name,
		K:        k,
		NDCG:     sum / float64(len(qs)),
		PerQuery: per,
	}, nil
}

// checkQrels rejects a query set whose relevant judgments name documents this
// index does not hold — a qrels file and an index built from different corpus
// snapshots, which is one `-data` flag away at every call site.
//
// Every judged key is checked, whatever its grade.
//
// It used to skip the nonrelevant ones, reasoning that idealDCG drops everything at
// or below 0 before sorting, so a missing one changes no number. That is true of the
// arithmetic and false of the ranking. A judged-nonrelevant document is one the
// assessors saw because some system retrieved it, so it is retrievable here too —
// and a retrieved nonrelevant document takes a slot above the cut and pushes a
// relevant one below it. An index missing it scores differently, usually higher, and
// the run completes either way. It is also the same evidence of a mispaired qrels
// file and index as a missing relevant document is, and this is the cheapest place
// either can be seen.
//
// Measured before tightening rather than assumed safe: all 66,336 of trec-covid's
// judgments, including the 41,663 at grade 0, name documents in the BEIR corpus. So
// the stricter check refuses no run that was measuring correctly.
func checkQrels(ix *engine.Index, qs []Query) error {
	for _, q := range qs {
		missing, relevant, first := 0, 0, ""
		for key, grade := range q.Qrels {
			if _, ok := ix.Resolve(key); ok {
				continue
			}
			missing++
			if grade > 0 {
				relevant++
			}
			// Map iteration is randomised, so the smallest key is named rather than
			// whichever came up first: an error that changes between identical runs is an
			// error nobody can act on.
			if first == "" || key < first {
				first = key
			}
		}
		if missing > 0 {
			return fmt.Errorf("query %s: %d of its %d judgments name documents this index does not "+
				"hold (%q is one, %d of the %d are relevant): the relevant ones stay in the ideal "+
				"ranking where no arm can reach them, and the rest are documents that would have "+
				"taken slots in the ranking this index produces: %w",
				q.ID, missing, len(q.Qrels), first, relevant, missing, ErrForeignQrelDoc)
		}
	}
	return nil
}

// rankedKeys searches to depth fetch, truncates to k and resolves DocIDs to
// document keys, which is the form NDCG scores against qrels.
func rankedKeys(ctx context.Context, ix *engine.Index, q engine.Query, fetch, k int, a Arm) ([]string, error) {
	cands, err := engine.Search(ctx, q, fetch, a.Fuse, a.Scorers...)
	if err != nil {
		return nil, err
	}
	// Truncation happens here rather than inside fusion. Fuse scores a document
	// from its ranks alone, so the fused order of the first k is the same whether
	// k or k*m was passed down — see the precondition on Arm.Fuse.
	if len(cands) > k {
		cands = cands[:k]
	}

	keys := make([]string, 0, len(cands))
	seen := make(map[engine.DocID]int, len(cands))
	for rank, c := range cands {
		doc, ok := ix.Doc(c.Doc)
		if !ok {
			return nil, fmt.Errorf("rank %d: doc %d: %w", rank+1, c.Doc, ErrForeignDocID)
		}
		// Checked here rather than trusted from the Fuser, for the same reason the
		// bound above is: this loop already has to touch every candidate, so the
		// check is free, and NDCG scores the slice it returns without looking for
		// repeats. See ErrDuplicateRanked.
		if first, dup := seen[c.Doc]; dup {
			return nil, fmt.Errorf("rank %d: doc %d (%q) is already at rank %d: %w",
				rank+1, c.Doc, doc.Key, first, ErrDuplicateRanked)
		}
		seen[c.Doc] = rank + 1
		keys = append(keys, doc.Key)
	}
	return keys, nil
}

// fetchDepth is k*overfetch, with 0 and 1 both meaning k.
func fetchDepth(k, overfetch int) (int, error) {
	if overfetch < 0 {
		return 0, fmt.Errorf("overfetch is %d: %w", overfetch, ErrOverfetchRange)
	}
	if overfetch <= 1 {
		return k, nil
	}
	// k and overfetch are both positive here, so this is the whole overflow
	// condition.
	if k > math.MaxInt/overfetch {
		return 0, fmt.Errorf("k %d times Overfetch %d overflows int: %w", k, overfetch, ErrOverfetchRange)
	}
	return k * overfetch, nil
}
