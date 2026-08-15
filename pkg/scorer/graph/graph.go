// SPDX-License-Identifier: Apache-2.0

// Package graph ranks documents by link proximity to a seed set.
//
// # Measured contribution: none. Read this before enabling it.
//
// Milestone 4 evaluated this scorer on TREC-COVID joined to the Semantic Scholar
// citation graph — 171,332 documents, 579,719 in-corpus edges, 50 queries — and it
// did not improve ranking quality at any setting tried. No fusion weight beat the
// baseline: from 0.1 downward the arm it produces *is* the baseline, delta exactly
// +0.0000. Under equal-weight RRF (fusion.Fuse) it is far worse than neutral: −0.1227.
//
// Two things follow for a caller.
//
// If you use this scorer, weight its stream down — fusion.FuseWeighted exists
// because of this measurement. Handing it an equal vote alongside BM25 measurably
// destroys rankings, and that cost belongs to the fusion policy rather than to this
// package (docs/FINDINGS.md milestone 4 section 7).
//
// And do not expect quality from it. The mechanism is understood: with MaxDepth 3 a
// candidate's score takes very few distinct values, the hop-1 frontier on a real
// citation graph runs to tens of documents per query, and engine.TopK breaks the
// resulting ties on DocID — which is corpus insertion order. The stream carries
// almost no ordering to contribute. Summing per-seed distances (see Candidates)
// improved this and did not fix it.
//
// The package is kept rather than deleted, deliberately and on the record: it is one
// of the signals the milestone 1 architecture assertions are built on, and those
// assertions are what this project is actually evidence for. docs/DECISIONS.md D-005
// has the full argument, including the case for deleting it instead.
//
// # What it does
//
// The seed set is the interesting part. It comes either from Query.Seeds, or from
// another engine.Scorer handed to New — any scorer, not specifically the text one.
// Graph proximity therefore composes with whatever is available without ever naming
// it, which is the same claim fusion makes, one level down.
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

// NewIncludingSeeds is New with seeds kept in the results, which is the proximity
// formula taken literally.
//
// This is the double-counting variant described above, and it exists for one
// reason: milestone 4 has to measure whether graph proximity improves nDCG, and
// that measurement is only trustworthy if both variants can be run over the same
// query set. Prefer New everywhere else.
//
// Read the measurement carefully: a seed contributes 1.0 to its own score, so seeds
// that do not link to each other all land on exactly 1.0 and TopK's tiebreak puts
// them in DocID order rather than the order the seed scorer ranked them. This arm
// therefore measures the seed set getting a second vote, not the seed scorer's
// ranking getting one.
//
// Since Candidates became a sum over seeds, a well-connected non-seed document can
// outrank a seed — two seeds one hop away sum to 1.0, three to 1.5. Under the
// previous nearest-seed formula hop-1 topped out at 0.5 and could never displace a
// seed, so at k <= SeedN this variant returned nothing but seeds. It no longer
// necessarily does, which makes it a slightly less clean isolation of the
// double-counting effect and a slightly more realistic arm. Milestone 4 reports both
// variants regardless (docs/EVAL.md section 5.8).
func NewIncludingSeeds(ix *engine.Index, seed engine.Scorer) *Scorer {
	return &Scorer{ix: ix, seed: seed, includeSeeds: true}
}

// Name implements engine.Scorer.
func (s *Scorer) Name() string { return "graph" }

// Candidates implements engine.Scorer.
//
// Score is Σ over seeds of 1/(1+hops from that seed), so a document one hop from
// two seeds scores 1.0 while a document one hop from one seed scores 0.5. Seeds sit
// at zero hops from themselves and are dropped unless the scorer was built with
// NewIncludingSeeds — see New for why, and docs/FINDINGS.md section 2.3 for how the
// two variants differ in practice.
//
// # Why the sum, and not the distance to the nearest seed
//
// The obvious formula — one merged traversal, score 1/(1+min hops) — is what this
// scorer used through milestone 3, and milestone 4 measured what it does on a real
// citation graph. With MaxDepth 3 a non-seed candidate can hold exactly three
// values: 0.5, 1/3, 0.25. On TREC-COVID the hop-1 frontier averaged 41 documents per
// query, and on every one of the 45 queries the graph could answer at all the tie
// group ran past k=10 — so engine.TopK's tiebreak picked the winners, by DocID, which
// is corpus insertion order. The stream was ranking by an accident of indexing, and it
// cost 0.157 nDCG@10 against the baseline (docs/EVAL.md sections 5.7 and 5.8).
//
// Summing per-seed distances addresses the cause rather than the symptom. A document
// several seeds agree on now outranks one only a single seed reaches, which is what
// "close to what the query is about" should have meant all along, and the score's
// granularity goes from three values to a sum over SeedN of them. It is also the
// more natural reading of proximity to a *set*: nearest-neighbour throws away every
// seed but one.
//
// ponytail: SeedN separate traversals, so a query pays SeedN times the link scans a
// merged BFS would. Personalised PageRank subsumes this and is the principled
// version (docs/FINDINGS.md section 5); it is worth writing when this formula is
// what limits quality, not merely when it is inelegant.
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

	// isSeed does two jobs. It excludes seeds from the results even when a sibling
	// seed's traversal reaches them — under the old merged BFS a seed was
	// identifiable by hops == 0, but now it can legitimately be one hop from another
	// seed and its accumulated score mixes both contributions. And it deduplicates
	// the traversal set: Query.Seeds is caller-supplied, two of its keys can resolve
	// to the same document, and the old merged BFS absorbed that for free because a
	// repeated id was already in dist. Summing per seed does not, so without this a
	// seed listed twice votes twice and every document it reaches doubles its score.
	isSeed := make(map[engine.DocID]bool, len(seeds))

	// Tallied by hop count rather than accumulated as each traversal finishes. Float
	// addition is not associative, so the order a document's per-seed contributions
	// arrive in decides its last bit — and that order is the caller's seed order. Two
	// documents holding the same multiset of distances then get mathematically equal
	// scores that differ as float64, so TopK's DocID tiebreak never gets to settle
	// them, and merely permuting Query.Seeds flips which one wins. With SeedN=5 and
	// MaxDepth=3 that is reachable on a five-seed query: {0.5,0.5,0.5,⅓,0.25} sums to
	// 2.083333333333333 in one order and 2.0833333333333335 in another.
	//
	// Counting the seeds at each distance and summing one term per distance, in
	// distance order, is the same number by every seed ordering — and the n seeds at
	// one hop become a single n/(1+hops) rather than n roundings of 1/(1+hops).
	// pkg/fusion sweeps rank-major for exactly this reason, one level up.
	//
	// The array is indexed by hop count, which bfs bounds at MaxDepth.
	tally := make(map[engine.DocID][MaxDepth + 1]int32, len(seeds))
	for _, seed := range seeds {
		if isSeed[seed] {
			continue
		}
		isSeed[seed] = true
		dist, err := s.bfs(ctx, seed)
		if err != nil {
			return nil, err
		}
		for id, hops := range dist {
			t := tally[id]
			t[hops]++
			tally[id] = t
		}
	}

	// TopK sorts, so a cancellation arriving after the last poll would otherwise
	// still pay for an O(n log n) sort of results nobody will read.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Unreachable documents never entered the tally, so they never appear.
	cands := make([]engine.Candidate, 0, len(tally))
	for id, t := range tally {
		if isSeed[id] && !s.includeSeeds {
			continue // A seed is not a discovery.
		}
		// Ascending hop order, which is what makes this independent of the order the
		// seeds were traversed in. Map iteration order is random and does not matter:
		// each document sums only its own row.
		var score float64
		for hops, n := range t {
			if n > 0 {
				score += float64(n) / float64(1+hops)
			}
		}
		cands = append(cands, engine.Candidate{Doc: id, Score: score})
	}
	return engine.TopK(cands, k), nil
}

// bfs returns hop distance from one seed to every document reachable within
// MaxDepth, including the seed itself at 0.
//
// Deduplication is per seed, not global: a document two seeds both reach has to be
// counted once for each, or the sum in Candidates would quietly collapse back into
// the nearest-seed formula for every document more than one seed touches.
func (s *Scorer) bfs(ctx context.Context, seed engine.DocID) (map[engine.DocID]int, error) {
	// dist doubles as the visited set, which is what makes cycles terminate: a
	// document already in dist is never enqueued again.
	dist := map[engine.DocID]int{seed: 0}
	queue := []engine.DocID{seed}

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
	return dist, nil
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
