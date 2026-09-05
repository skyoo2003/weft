// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

func ppr(t *testing.T, ix *engine.Index, seed engine.Scorer, opts ...Option) *PPR {
	t.Helper()
	p, err := NewPPR(adjacency(t, ix), seed, opts...)
	if err != nil {
		t.Fatalf("NewPPR: %v", err)
	}
	return p
}

func candidateScores(t *testing.T, s engine.Scorer, q engine.Query, k int) map[engine.DocID]float64 {
	t.Helper()
	cands, err := s.Candidates(context.Background(), q, k)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	out := make(map[engine.DocID]float64, len(cands))
	for _, c := range cands {
		out[c.Doc] = c.Score
	}
	return out
}

func distinct(scores map[engine.DocID]float64) int {
	seen := map[float64]bool{}
	for _, s := range scores {
		seen[s] = true
	}
	return len(seen)
}

// chain is n documents in a line, each linking to the next.
func chain(t *testing.T, n int) *engine.Index {
	t.Helper()
	nodes := make([]node, n)
	for i := range nodes {
		nodes[i] = node{key: fmt.Sprintf("n%03d", i)}
		if i+1 < n {
			nodes[i].links = []string{fmt.Sprintf("n%03d", i+1)}
		}
	}
	return index(t, nodes...)
}

// TestPPRBreaksTheTiesHopDistanceCannot is the reason this scorer was written,
// stated as a test rather than as a paragraph.
//
// Milestone 4's diagnosis was not that citation structure carries no signal. It
// was that hop distance has too few values to rank with: with MaxDepth 3 a
// candidate holds one of a handful of scores, and engine.TopK then settles the
// tie group on DocID, which is corpus insertion order. This builds exactly that
// shape — a seed with many one-hop neighbours differing only in what lies
// beyond them — and asserts the two scorers disagree about it the way the
// diagnosis predicts.
func TestPPRBreaksTheTiesHopDistanceCannot(t *testing.T) {
	// s links to eight documents. Each is at hop 1 and therefore ties under
	// 1/(1+hops); they differ in how well connected they are, which is what a
	// walk can see and a hop count cannot.
	nodes := []node{{key: "s", links: []string{
		"h0", "h1", "h2", "h3", "h4", "h5", "h6", "h7",
	}}}
	for i := range 8 {
		// h(i) links onward to i tail documents, so degrees run 1 through 8.
		links := make([]string, 0, i)
		for j := range i {
			links = append(links, fmt.Sprintf("t%d_%d", i, j))
		}
		nodes = append(nodes, node{key: fmt.Sprintf("h%d", i), links: links})
	}
	for i := range 8 {
		for j := range i {
			nodes = append(nodes, node{key: fmt.Sprintf("t%d_%d", i, j)})
		}
	}
	ix := index(t, nodes...)
	q := engine.Query{Seeds: []string{"s"}}

	bfs := candidateScores(t, New(ix, nil), q, 8)
	walk := candidateScores(t, ppr(t, ix, nil), q, 8)

	if hops := distinct(bfs); hops > 2 {
		t.Fatalf("the BFS arm produced %d distinct scores over the hop-1 frontier; "+
			"this fixture is meant to reproduce the tie group milestone 4 diagnosed", hops)
	}
	if mass := distinct(walk); mass < 8 {
		t.Errorf("PPR produced %d distinct scores over 8 candidates, want 8 — "+
			"a walk that ties is the failure this scorer exists to remove", mass)
	}

	// And the ordering it produces is the one connectivity implies rather than
	// the one insertion order implies. Which direction that runs is the whole of
	// what a walk adds over a hop count, and it is worth pinning because the
	// answer is the opposite of the obvious guess: mass a node spreads to its
	// neighbours comes back along the same edges, so the *better*-connected
	// neighbour keeps more. That is PageRank's degree bias, present here by
	// construction and not by accident.
	//
	// It is also the property most likely to be wrong for this task — on a
	// citation corpus the well-connected paper is the one everything cites and
	// nothing is specifically about. PPR's documentation names the lever that
	// removes it; this test's job is to make sure a change of direction cannot
	// happen silently.
	if walk[8] <= walk[1] {
		t.Errorf("h7 (degree 9) scored %v and h0 (degree 2) scored %v; "+
			"plain PPR returns mass along the edges it spread it down, so the "+
			"better-connected neighbour has to score higher", walk[8], walk[1])
	}
	for i := 1; i < 8; i++ {
		if walk[engine.DocID(i)] >= walk[engine.DocID(i+1)] {
			t.Errorf("h%d scored %v and h%d scored %v; the walk should be monotone in degree",
				i-1, walk[engine.DocID(i)], i, walk[engine.DocID(i+1)])
		}
	}
}

// TestPPRTravelsBothDirections is the capability the BFS scorer does not have.
// Document.Links says what a paper cites; "what cites this paper" is the reverse
// of every edge in the corpus, and on a citation graph it is most of the
// relatedness there is.
func TestPPRTravelsBothDirections(t *testing.T) {
	// citer -> seed. Nothing links out of seed at all.
	ix := index(t,
		node{"seed", nil},
		node{"citer", []string{"seed"}},
		node{"unrelated", nil},
	)
	q := engine.Query{Seeds: []string{"seed"}}

	if got := candidateScores(t, New(ix, nil), q, 10); len(got) != 0 {
		t.Fatalf("the BFS arm found %v; it follows Links only, so a seed with no "+
			"out-edges has to reach nothing", got)
	}
	walk := candidateScores(t, ppr(t, ix, nil), q, 10)
	if walk[1] <= 0 {
		t.Errorf("PPR scored the citing document %v, want positive", walk[1])
	}
	if _, found := walk[2]; found {
		t.Errorf("PPR reached an unconnected document: %v", walk)
	}
}

// TestPPRMassIsConserved is the arithmetic check on the push loop. Every push
// moves alpha of a residual into the rank and spreads the rest; nothing creates
// mass and nothing may lose it except into residuals still below their
// threshold. So the rank sums to at most one, and to nearly one once the walk
// has converged.
//
// A sum above one means the spread wrote more than it took — the classic bug
// being a divide by the out-degree while pushing to both directions.
func TestPPRMassIsConserved(t *testing.T) {
	ix := chain(t, 64)
	p := ppr(t, ix, nil, WithPrecision(1e-9))

	cands, err := p.Candidates(context.Background(), engine.Query{Seeds: []string{"n000"}}, 64)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	total := 0.0
	for _, c := range cands {
		if c.Score <= 0 {
			t.Errorf("candidate %d scored %v, want positive", c.Doc, c.Score)
		}
		total += c.Score
	}
	// The seed keeps a share of the mass and is dropped from the result, so the
	// visible total is below one by at least that. It must never exceed one.
	if total > 1 {
		t.Errorf("the non-seed mass sums to %v, which is more than the walk had", total)
	}
	if total < 0.5 {
		t.Errorf("the non-seed mass sums to %v; a 64-document chain should carry "+
			"most of the walk away from the seed", total)
	}
}

// TestPPRTerminatesOnACycle is what the work bound buys, and a cycle is where an
// unbounded traversal does not come back.
func TestPPRTerminatesOnACycle(t *testing.T) {
	ix := index(t,
		node{"a", []string{"b"}},
		node{"b", []string{"c"}},
		node{"c", []string{"a"}},
	)
	got := candidateScores(t, ppr(t, ix, nil), engine.Query{Seeds: []string{"a"}}, 10)
	if len(got) != 2 {
		t.Fatalf("scores = %v, want the two non-seed documents", got)
	}
	for id, s := range got {
		if math.IsNaN(s) || math.IsInf(s, 0) || s <= 0 {
			t.Errorf("document %d scored %v", id, s)
		}
	}
}

// TestPPRIsIndependentOfSeedOrder is the property Candidates buys with one sort,
// and it is worth a test because the failure is invisible: two mathematically
// equal scores differing in their last bit make engine.TopK's DocID tiebreak
// unreachable, so permuting Query.Seeds silently permutes the ranking.
func TestPPRIsIndependentOfSeedOrder(t *testing.T) {
	ix := index(t,
		node{"s1", []string{"m"}},
		node{"s2", []string{"m"}},
		node{"s3", []string{"m"}},
		node{"m", []string{"x", "y"}},
		node{"x", nil},
		node{"y", nil},
	)
	p := ppr(t, ix, nil)

	first := candidateScores(t, p, engine.Query{Seeds: []string{"s1", "s2", "s3"}}, 10)
	for _, order := range [][]string{
		{"s3", "s2", "s1"},
		{"s2", "s3", "s1"},
		{"s2", "s1", "s3"},
	} {
		got := candidateScores(t, p, engine.Query{Seeds: order}, 10)
		if len(got) != len(first) {
			t.Fatalf("seeds %v gave %d candidates, want %d", order, len(got), len(first))
		}
		for id, want := range first {
			// Exactly equal, not close. A tolerance here would pass the very
			// float difference the sort exists to remove.
			if got[id] != want {
				t.Errorf("seeds %v: document %d scored %v, want exactly %v", order, id, got[id], want)
			}
		}
	}
}

// TestPPRExcludesSeeds keeps the rule the package already holds: a seed is not a
// discovery, and returning it makes whoever produced it vote twice under rank
// fusion.
func TestPPRExcludesSeeds(t *testing.T) {
	ix := index(t, node{"a", []string{"b"}}, node{"b", []string{"a"}}, node{"c", nil})
	got := candidateScores(t, ppr(t, ix, nil), engine.Query{Seeds: []string{"a", "a"}}, 10)
	if _, found := got[0]; found {
		t.Errorf("the seed appears in the result: %v", got)
	}
	if got[1] <= 0 {
		t.Errorf("scores = %v, want the seed's neighbour scored", got)
	}
}

// TestPPRRefusesConstantsItCannotHonour. A restart of zero does not make the
// walk fail, it removes the term that bounds its work — so the loop would run
// until float underflow instead of terminating on the argument that licenses it.
// Refusing at construction is the only place that can be said to the party able
// to fix it.
func TestPPRRefusesConstantsItCannotHonour(t *testing.T) {
	a := adjacency(t, index(t, node{"a", nil}))
	for _, tc := range []struct {
		name string
		opt  Option
	}{
		{"restart of zero", WithRestart(0)},
		{"restart above one", WithRestart(1.5)},
		{"negative restart", WithRestart(-0.1)},
		{"precision of zero", WithPrecision(0)},
		{"negative precision", WithPrecision(-1e-4)},
		{"restart NaN", WithRestart(math.NaN())},
		{"precision NaN", WithPrecision(math.NaN())},
	} {
		if _, err := NewPPR(a, nil, tc.opt); err == nil {
			t.Errorf("NewPPR accepted a %s", tc.name)
		}
	}
	if _, err := NewPPR(nil, nil); err == nil {
		t.Error("NewPPR accepted a nil adjacency")
	}
	if _, err := NewPPR(a, nil, WithRestart(1)); err != nil {
		t.Errorf("NewPPR refused a restart of exactly 1: %v", err)
	}
}

// TestPPRHasNoOpinionWithoutSeeds mirrors the BFS scorer: a nil seed scorer and
// no Query.Seeds is not an error, it is a scorer with nothing to say. Search
// fuses an empty stream at no cost.
func TestPPRHasNoOpinionWithoutSeeds(t *testing.T) {
	ix := index(t, node{"a", []string{"b"}}, node{"b", nil})
	cands, err := ppr(t, ix, nil).Candidates(context.Background(), engine.Query{}, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("candidates = %v, want none", cands)
	}
}

// TestPPRSeedsFromAnotherScorer is the composition claim the package makes one
// level down from fusion: the seed set comes from any engine.Scorer, and this
// one never learns which.
func TestPPRSeedsFromAnotherScorer(t *testing.T) {
	ix := index(t, node{"a", []string{"b", "c"}}, node{"b", nil}, node{"c", nil})
	seed := stub{cands: []engine.Candidate{{Doc: 0, Score: 1}}}

	got := candidateScores(t, ppr(t, ix, seed), engine.Query{Text: "anything"}, 10)
	if len(got) != 2 || got[1] <= 0 || got[2] <= 0 {
		t.Errorf("scores = %v, want both of the seed's neighbours scored", got)
	}
}

// TestPPRIsCancellable. The work bound is on edges, not on wall clock, so a
// caller that gave up has to be able to stop the push loop.
func TestPPRIsCancellable(t *testing.T) {
	ix := chain(t, 4096)
	p := ppr(t, ix, nil, WithPrecision(1e-12))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Candidates(ctx, engine.Query{Seeds: []string{"n000"}}, 10); err == nil {
		t.Error("Candidates on a cancelled context returned no error")
	}
}
