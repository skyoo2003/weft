// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"fmt"
	"slices"

	"github.com/skyoo2003/weft/pkg/engine"
)

// Defaults for PPR's two constants. Both are exported so a caller can see what
// it is getting without reading the source, and both are overridable because
// this scorer exists in order to be measured and a knob nothing can turn is a
// measurement of one point.
const (
	// DefaultRestart is the probability a walk returns to the seed set at each
	// step. 0.15 is the PageRank convention. Higher keeps the mass nearer the
	// seeds and cuts the work; lower spreads it further.
	DefaultRestart = 0.15

	// DefaultPrecision bounds the residual a node may keep per edge before the
	// push loop stops caring about it. It is what turns an algorithm over the
	// whole graph into a local one: total edge work is at most
	// 1/(restart × precision), so this pair is 66,667 edges per query however
	// large the corpus is.
	//
	// ponytail: chosen from that arithmetic against the 579,719-edge evaluation
	// graph, not from a measurement of quality. The sweep that would set it is
	// the one this scorer was written to be run through.
	DefaultPrecision = 1e-4
)

// PPR ranks documents by personalized PageRank from the seed set.
//
// # Why this exists beside Scorer
//
// Milestone 4 measured Scorer — BFS hop distance, 1/(1+hops) summed over seeds —
// on TREC-COVID joined to the Semantic Scholar citation graph and found it
// contributes nothing: +0.0000 nDCG@10 at its best fusion weight. The mechanism
// was diagnosed rather than guessed, and it was not that citation structure
// carries no signal. It was that *this construction* has almost no ranking to
// contribute: with MaxDepth 3 a candidate holds one of a handful of distinct
// scores, the hop-1 frontier ran to 41 documents a query, and engine.TopK broke
// the resulting ties on DocID — which is corpus insertion order. The stream was
// ranking by an accident of indexing.
//
// A random walk has no such ceiling. Every node reached holds a distinct share
// of the walk's mass, so the score is continuous and the tie groups the
// diagnosis blamed do not form. It also weighs a path by how easy it is to
// travel rather than by its length alone: two seeds agreeing, a short path
// through a low-degree node, and a node reachable many ways all raise a score,
// where hop distance sees only the shortest hop count.
//
// docs/FINDINGS.md milestone 4 section 5 named this as the principled version of
// what Scorer approximates. This is that, and whether it beats the baseline is a
// measurement and not a claim — read docs/EVAL.md before enabling it.
//
// # What it costs
//
// Andersen–Chung–Lang forward push, which is local: it touches the neighbourhood
// the mass actually reaches and never the whole graph. Total edge work is
// bounded by 1/(restart × precision) regardless of corpus size, which is the
// property that makes it cheaper than the BFS it replaces rather than more
// expensive — MaxDepth 3 over a real citation graph has no such bound.
//
// It walks an Adjacency, not the index, and that is a hard requirement rather
// than an optimisation. The push loop reads a node's edges tens of thousands of
// times per query; through engine.Index.Doc and Resolve, as Scorer does, each of
// those reads is a record decode and a locked binary search.
//
// # The degree bias, which is a property and not a bug
//
// Mass a node spreads down its edges comes back along the same edges, so a
// well-connected document scores higher than a poorly connected one at the same
// distance from the seeds. That is PageRank's, it is here by construction, and
// TestPPRBreaksTheTiesHopDistanceCannot pins the direction so it cannot change
// silently.
//
// It is also the property most likely to be wrong for this task. On a citation
// corpus the best-connected paper is the one everything cites and nothing is
// specifically about, so a stream that ranks it first is ranking by fame rather
// than by relevance — which is a plausible reading of what milestone 4 measured
// and did not explain.
//
// The lever is degree normalization, and this scorer does not have a mode for
// it: a seam with a menu in it is not a seam (D-022). It is reachable from
// outside instead, because Adjacency.Degree is exported and engine.Search takes
// scorers by interface. A caller wanting it wraps this one, asks for more
// candidates than it will return, divides each score by the degree, and re-runs
// engine.TopK — the same shape docs/ADOPTION.md section 8 recommends for a
// constraint. Whether it helps is a measurement nobody has made.
type PPR struct {
	adj  *Adjacency
	seed engine.Scorer

	alpha float64
	eps   float64
}

// Option configures a PPR at construction. NewPPR takes them and nothing else
// does — the same rule and the same reason as engine.Option, which is that a
// scorer whose constants can change under a running query has no reproducible
// measurement behind it.
type Option func(*PPR)

// WithRestart sets the restart probability, which must be in (0, 1].
//
// It is the knob between "near the seeds" and "across the component". At 1 the
// walk never leaves the seed set and every score is zero once seeds are dropped;
// as it approaches 0 the work bound goes to infinity and the ranking approaches
// global PageRank, which is not personalized to anything.
func WithRestart(alpha float64) Option { return func(p *PPR) { p.alpha = alpha } }

// WithPrecision sets the residual threshold, which must be positive.
//
// Smaller is a longer walk and a finer tail: it does not change which documents
// rank highest, it changes how far down the list the scorer still has an
// opinion. See DefaultPrecision for the work it buys.
func WithPrecision(eps float64) Option { return func(p *PPR) { p.eps = eps } }

// NewPPR returns a PPR scorer over adj, using seed to derive seed documents when
// Query.Seeds is empty.
//
// seed may be nil, in which case the scorer has an opinion only on queries that
// name their own seeds — the same contract New gives, and the same escape hatch
// that keeps graph proximity usable independently of the text results.
//
// It returns an error rather than clamping a bad constant. An out-of-range
// restart probability does not make the walk fail, it makes it converge to
// something the caller did not ask for and cannot see, which is the class of
// silent wrong answer this module refuses everywhere else. A constructor is
// called once; the error costs a line at a call site and buys the guarantee that
// the scorer running is the one that was configured.
func NewPPR(adj *Adjacency, seed engine.Scorer, opts ...Option) (*PPR, error) {
	if adj == nil {
		return nil, fmt.Errorf("graph: NewPPR needs an adjacency")
	}
	p := &PPR{adj: adj, seed: seed, alpha: DefaultRestart, eps: DefaultPrecision}
	for _, o := range opts {
		o(p)
	}
	// Not merely positive: a restart of 0 removes the term that bounds the work,
	// and the push loop would then run until float underflow rather than
	// terminating on the argument that licenses it.
	if !(p.alpha > 0 && p.alpha <= 1) {
		return nil, fmt.Errorf("graph: restart probability %v is not in (0, 1]", p.alpha)
	}
	if !(p.eps > 0) {
		return nil, fmt.Errorf("graph: precision %v is not positive", p.eps)
	}
	return p, nil
}

// Name implements engine.Scorer.
func (p *PPR) Name() string { return "graph-ppr" }

// Candidates implements engine.Scorer.
//
// Score is the document's share of the walk's mass. It is on no scale any other
// scorer shares, which is exactly what rank fusion is built not to care about.
//
// Seeds are dropped from the result, for the reason New gives: a seed is not a
// discovery, and handing it back makes whoever produced it vote twice.
func (p *PPR) Candidates(ctx context.Context, q engine.Query, k int) ([]engine.Candidate, error) {
	if k <= 0 {
		return nil, nil
	}
	seeds, err := seedDocs(ctx, p.adj.ix, p.seed, q)
	if err != nil {
		return nil, err
	}
	if len(seeds) == 0 {
		return nil, nil
	}

	// Deduplicated in place. Query.Seeds is caller-supplied and two of its keys
	// can resolve to one document; without this that document starts with twice
	// the mass and every node it reaches is scored as though two independent
	// seeds agreed on it.
	isSeed := make(map[engine.DocID]bool, len(seeds))
	uniq := seeds[:0]
	for _, s := range seeds {
		if !isSeed[s] {
			isSeed[s] = true
			uniq = append(uniq, s)
		}
	}
	// Sorted, and this is not tidiness. Float addition is not associative, so
	// the order residual arrives at a node decides its last bit — and that order
	// is the order the frontier was seeded in, which is the caller's. Two
	// documents whose scores are mathematically equal would then differ as
	// float64, engine.TopK's DocID tiebreak would never get to settle them, and
	// merely permuting Query.Seeds would flip which one wins.
	//
	// One sort of at most SeedN ids makes the whole push sequence a function of
	// the adjacency alone. Scorer.Candidates buys the same property a harder way,
	// by tallying per hop count; here the frontier is the only place the caller's
	// order can enter.
	slices.Sort(uniq)
	seeds = uniq

	rank, err := p.push(ctx, seeds)
	if err != nil {
		return nil, err
	}

	// TopK sorts, so a cancellation arriving after the last poll would otherwise
	// still buy an O(n log n) sort of results nobody will read. Same placement
	// and same reason as Scorer.Candidates.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cands := make([]engine.Candidate, 0, len(rank))
	for id, score := range rank {
		if isSeed[id] {
			continue
		}
		cands = append(cands, engine.Candidate{Doc: id, Score: score})
	}
	return engine.TopK(cands, k), nil
}

// push is Andersen–Chung–Lang forward push: the approximate personalized
// PageRank vector for a seed distribution, computed by moving residual mass
// outward until no node holds more than eps per edge.
//
// The loop is the whole algorithm and each part of it is a decision:
//
//   - rank takes alpha of a node's residual and the rest is spread over its
//     edges. That ratio is what makes the result a walk that restarts rather
//     than a flood.
//   - The spread is divided by the *undirected* degree and pushed to both the
//     out- and the in-neighbours. A citation edge means "these two papers are
//     about the same thing" in both directions, and a walk that only followed
//     Document.Links would travel one way through time — reaching what a paper
//     cites and never what cites it, which on a citation graph is most of the
//     relatedness there is.
//   - A node is enqueued when its residual first crosses its own threshold, and
//     the threshold is per edge. A hub therefore needs proportionally more mass
//     before it is worth expanding, which is what keeps one high-degree node
//     from pulling the walk onto the whole component.
//   - The frontier is a stack, not a queue. Both terminate on the same argument
//     and neither changes the fixed point; a stack bounds its own memory by the
//     frontier rather than by the number of pushes.
//
// It terminates because every push moves at least alpha × eps × degree of mass
// out of the residual and into the rank, and the total mass is 1. That is also
// the work bound: at most 1/(alpha × eps) edges are traversed, whatever the
// corpus holds.
func (p *PPR) push(ctx context.Context, seeds []engine.DocID) (map[engine.DocID]float64, error) {
	rank := make(map[engine.DocID]float64)
	resid := make(map[engine.DocID]float64, len(seeds))
	queued := make(map[engine.DocID]bool, len(seeds))

	// Mass split evenly, so the scorer's opinion does not grow with how many
	// seeds it happened to be given.
	share := 1 / float64(len(seeds))
	frontier := make([]engine.DocID, 0, len(seeds))
	for _, s := range seeds {
		resid[s] = share
		queued[s] = true
		frontier = append(frontier, s)
	}

	for steps := 0; len(frontier) > 0; steps++ {
		// The work is bounded but not small, and the bound is on edges rather
		// than on wall clock. Polled every 256 pops, as the traversals in this
		// package poll their own scans.
		if steps&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		u := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		queued[u] = false

		ru := resid[u]
		deg := p.adj.Degree(u)
		// A node can be popped after a later push has already drained it, or
		// before its residual ever reached the threshold it was enqueued on.
		// Re-asking here rather than trusting the enqueue is what keeps the
		// termination argument true of the loop and not merely of the algorithm.
		if ru < p.eps*float64(deg) {
			continue
		}
		delete(resid, u)

		if deg == 0 {
			// An isolated node. It can only be a seed — anything reached by an
			// edge has at least that edge — so this is the degenerate query
			// where the seed set is disconnected, and the mass it absorbs is
			// dropped with the seed on the way out.
			rank[u] += ru
			continue
		}
		rank[u] += p.alpha * ru
		spread := (1 - p.alpha) * ru / float64(deg)

		// Both directions in one pass over an array of the two rows, which keeps
		// the neighbour loop free of a closure — this is the innermost loop in
		// the scorer and a per-node allocation here is a per-edge allocation in
		// aggregate.
		for _, row := range [2][]engine.DocID{p.adj.Out(u), p.adj.In(u)} {
			for _, v := range row {
				nr := resid[v] + spread
				resid[v] = nr
				if !queued[v] && nr >= p.eps*float64(p.adj.Degree(v)) {
					queued[v] = true
					frontier = append(frontier, v)
				}
			}
		}
	}
	return rank, nil
}
