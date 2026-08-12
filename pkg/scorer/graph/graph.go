// Package graph ranks documents by link proximity to a seed set.
//
// The seed set is the interesting part. It comes either from Query.Seeds, or
// from another engine.Scorer handed to New — any scorer, not specifically the
// text one. Graph proximity therefore composes with whatever is available
// without ever naming it, which is the same claim fusion makes, one level down.
package graph

import (
	"context"
	"fmt"

	"github.com/skyoo2003/weft/pkg/engine"
)

const (
	// MaxDepth bounds the BFS. Beyond a few hops "related" stops meaning
	// anything, and the frontier grows to the whole connected component.
	MaxDepth = 3

	// SeedN is how many candidates to take from the seed scorer.
	SeedN = 5
)

// Scorer ranks documents by 1/(1+hops) from the seed set.
type Scorer struct {
	ix           *engine.Index
	seed         engine.Scorer
	includeSeeds bool
}

// New returns a graph scorer reading ix, using seed to derive seed documents
// when Query.Seeds is empty. seed may be nil, in which case the scorer has an
// opinion only on queries that name their own seeds.
//
// Seeds are not returned in the results. A seed is not a discovery: whoever
// produced it — the seed scorer, or the caller via Query.Seeds — already ranked
// it, and handing it back would make this scorer's top results a copy of theirs.
// Under RRF that copy counts as a second independent vote, so the seed scorer
// silently gets double weight. Returning only what the traversal *found* keeps
// this scorer's contribution to fusion genuinely new information.
func New(ix *engine.Index, seed engine.Scorer) *Scorer {
	return &Scorer{ix: ix, seed: seed}
}

// NewIncludingSeeds is New with seeds kept in the results at score 1.0, which is
// the proximity formula taken literally.
//
// This is the double-counting variant described above, and it exists for one
// reason: milestone 4 has to measure whether graph proximity improves nDCG, and
// that measurement is only trustworthy if both variants can be run over the same
// query set. Prefer New everywhere else.
//
// Read the measurement carefully: every seed scores exactly 1.0, so TopK's tie
// break puts them in DocID order, not the order the seed scorer ranked them.
// This arm therefore measures the seed set getting a second vote, not the seed
// scorer's ranking getting one. At k <= SeedN it is only seeds — hop-1 at 0.5
// never outsorts a seed at 1.0 — so the traversal contributes nothing at the
// demo's default k of 5.
func NewIncludingSeeds(ix *engine.Index, seed engine.Scorer) *Scorer {
	return &Scorer{ix: ix, seed: seed, includeSeeds: true}
}

// Name implements engine.Scorer.
func (s *Scorer) Name() string { return "graph" }

// Candidates implements engine.Scorer.
//
// Score is 1/(1+hops): one hop out scores 0.5, two hops 1/3. Seeds sit at zero
// hops and would score 1.0, but they are dropped unless the scorer was built
// with NewIncludingSeeds — see New for why, and docs/FINDINGS.md section 2.3 for
// how the two variants differ in practice.
func (s *Scorer) Candidates(ctx context.Context, q engine.Query, k int) ([]engine.Candidate, error) {
	if k <= 0 {
		return nil, nil
	}
	seeds, err := s.seedDocs(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(seeds) == 0 {
		return nil, nil
	}

	// dist doubles as the visited set, which is what makes cycles terminate: a
	// document already in dist is never enqueued again.
	dist := make(map[engine.DocID]int, len(seeds))
	queue := make([]engine.DocID, 0, len(seeds))
	for _, id := range seeds {
		if _, seen := dist[id]; seen {
			continue
		}
		dist[id] = 0
		queue = append(queue, id)
	}

	for head := 0; head < len(queue); head++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cur := queue[head]
		hops := dist[cur]
		if hops >= MaxDepth {
			continue
		}
		doc, ok := s.ix.Doc(cur)
		if !ok {
			continue
		}
		for i, key := range doc.Links {
			// The per-node check above runs once per dequeue, which is not enough
			// for a high-degree document: Links is caller-supplied and unbounded,
			// and if the links are dangling nothing is enqueued, so the queue ends
			// and the outer check never runs again. Poll every 1024 links.
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			// ponytail: Resolve takes the index-wide RLock once per link, so a
			// high-degree traversal loses throughput as cores are added rather
			// than gaining it. Snapshot byKey once per query before fanning
			// scorers out with goroutines — the batching is a prerequisite for
			// that parallelism, not an independent optimization.
			next, ok := s.ix.Resolve(key)
			if !ok {
				continue // Dangling link: a Key that was never added.
			}
			if _, seen := dist[next]; seen {
				continue
			}
			dist[next] = hops + 1
			queue = append(queue, next)
		}
	}

	// TopK sorts, so a cancellation arriving after the last poll would otherwise
	// still pay for an O(n log n) sort of results nobody will read.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Unreachable documents are simply absent from dist, so they never appear.
	cands := make([]engine.Candidate, 0, len(dist))
	for id, hops := range dist {
		if hops == 0 && !s.includeSeeds {
			continue // A seed is not a discovery.
		}
		cands = append(cands, engine.Candidate{Doc: id, Score: 1 / float64(1+hops)})
	}
	return engine.TopK(cands, k), nil
}

// seedDocs resolves the starting set. Explicit Query.Seeds win over the seed
// scorer, so a caller can always override.
func (s *Scorer) seedDocs(ctx context.Context, q engine.Query) ([]engine.DocID, error) {
	if len(q.Seeds) > 0 {
		ids := make([]engine.DocID, 0, len(q.Seeds))
		for i, key := range q.Seeds {
			// Seeds is caller-supplied and unbounded, and this loop runs before
			// the traversal that would otherwise do the checking: if every key is
			// unknown, ids comes back empty and Candidates reports a successful
			// empty result, indistinguishable from an honest miss. Every 1024
			// keys, as in the link and posting scans.
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if id, ok := s.ix.Resolve(key); ok {
				ids = append(ids, id)
			}
			// An unknown seed key is skipped, not an error: asking about a
			// document that is not indexed yet is normal.
		}
		return ids, nil
	}
	if s.seed == nil {
		// The nil-seed branch still has to answer for the context. Without this
		// it is the one path out of Candidates that reports a cancelled query as
		// a successful "no opinion": len(seeds) == 0 short-circuits above the
		// traversal, so the pre-TopK check never runs either.
		//
		// Note this catches a nil interface, not a non-nil interface holding a
		// nil pointer — `var s *text.Scorer` passed to New still panics here.
		// Same trade-off as engine.ErrNilScorer; build the seed into the call
		// conditionally rather than pre-declaring a typed nil.
		return nil, ctx.Err()
	}
	cands, err := s.seed.Candidates(ctx, q, SeedN)
	if err != nil {
		return nil, fmt.Errorf("seed scorer %s: %w", s.seed.Name(), err)
	}
	// The seed scorer's DocIDs are checked against this index before they are
	// trusted. A DocID means nothing outside the index that assigned it, and a
	// seed scorer reading a different index hands back ids from another
	// namespace. The traversal's own s.ix.Doc(cur) miss only skips expansion —
	// the id stays in dist at hop 0, so NewIncludingSeeds would emit a document
	// that does not exist at score 1.0, the top rank, and every consumer that
	// ignores Doc's bool prints it as a blank row.
	ids := make([]engine.DocID, 0, len(cands))
	for _, c := range cands {
		if _, ok := s.ix.Doc(c.Doc); ok {
			ids = append(ids, c.Doc)
		}
	}
	return ids, nil
}
