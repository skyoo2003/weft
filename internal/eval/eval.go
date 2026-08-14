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

	// ErrForeignDocID reports a fused DocID the index does not know. This is the
	// namespace hazard engine.Search documents and deliberately leaves unchecked
	// (docs/FINDINGS.md section 3.4) — a scorer built against a different index
	// returns ids from another namespace, and they resolve against neither
	// corpus. Search cannot check it without asking a scorer which index it
	// reads, which is the one method that would break the Scorer interface. Here
	// it is checkable for free: the harness has to resolve every DocID to a key
	// anyway to look it up in qrels. Caught, it is one error; missed, it is an
	// nDCG computed over keys belonging to other documents.
	ErrForeignDocID = errors.New("eval: fused DocID is not in this index")
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
	for rank, c := range cands {
		doc, ok := ix.Doc(c.Doc)
		if !ok {
			return nil, fmt.Errorf("rank %d: doc %d: %w", rank+1, c.Doc, ErrForeignDocID)
		}
		keys = append(keys, doc.Key)
	}
	return keys, nil
}

// fetchDepth is k*overfetch, with 0 and 1 both meaning k.
func fetchDepth(k, overfetch int) (int, error) {
	if overfetch < 0 {
		return 0, fmt.Errorf("Overfetch is %d: %w", overfetch, ErrOverfetchRange)
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
