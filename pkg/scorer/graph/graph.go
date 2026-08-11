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
		for _, key := range q.Seeds {
			if id, ok := s.ix.Resolve(key); ok {
				ids = append(ids, id)
			}
			// An unknown seed key is skipped, not an error: asking about a
			// document that is not indexed yet is normal.
		}
		return ids, nil
	}
	if s.seed == nil {
		return nil, nil
	}
	cands, err := s.seed.Candidates(ctx, q, SeedN)
	if err != nil {
		return nil, fmt.Errorf("seed scorer %s: %w", s.seed.Name(), err)
	}
	ids := make([]engine.DocID, len(cands))
	for i, c := range cands {
		ids[i] = c.Doc
	}
	return ids, nil
}
