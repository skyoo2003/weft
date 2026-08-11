package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

type node struct {
	key   string
	links []string
}

func index(t *testing.T, nodes ...node) *engine.Index {
	t.Helper()
	ix := engine.New()
	for _, n := range nodes {
		if _, err := ix.Add(engine.Document{Key: n.key, Links: n.links}); err != nil {
			t.Fatalf("Add(%q): %v", n.key, err)
		}
	}
	return ix
}

// stub is any scorer at all. That this file can seed a BFS without importing
// scorer/text anywhere is the point: the graph scorer composes with an
// interface, not with a particular scorer.
type stub struct {
	cands []engine.Candidate
	err   error
}

func (s stub) Name() string { return "stub" }

func (s stub) Candidates(context.Context, engine.Query, int) ([]engine.Candidate, error) {
	return s.cands, s.err
}

func scores(t *testing.T, s *Scorer, q engine.Query, k int) map[engine.DocID]float64 {
	t.Helper()
	cands, err := s.Candidates(t.Context(), q, k)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	m := make(map[engine.DocID]float64, len(cands))
	for _, c := range cands {
		m[c.Doc] = c.Score
	}
	return m
}

// ---------------------------------------------------------------------------
// Seed handling — docs/FINDINGS.md section 2.3
// ---------------------------------------------------------------------------

func TestSeedsAreExcludedFromResults(t *testing.T) {
	// If seeds came back at 1.0 they would top this scorer's ranking, making it
	// a copy of the seed scorer's ranking. RRF would then count the same
	// evidence twice and quietly give the seed scorer double weight.
	ix := index(t, node{"a", []string{"b"}}, node{"b", nil})
	seed := stub{cands: []engine.Candidate{{Doc: 0, Score: 9}}}

	got := scores(t, New(ix, seed), engine.Query{Text: "anything"}, 10)
	if _, echoed := got[0]; echoed {
		t.Fatalf("got %v — the seed was echoed back, double-counting the seed scorer", got)
	}
	if len(got) != 1 || got[1] != 0.5 {
		t.Fatalf("got %v, want only doc 1 at 0.5", got)
	}
}

func TestNewIncludingSeedsKeepsThem(t *testing.T) {
	// The literal-proximity variant, kept so milestone 4 can A/B the two over
	// one query set instead of guessing which effect it measured.
	ix := index(t, node{"a", []string{"b"}}, node{"b", nil})
	seed := stub{cands: []engine.Candidate{{Doc: 0, Score: 9}}}

	got := scores(t, NewIncludingSeeds(ix, seed), engine.Query{Text: "anything"}, 10)
	if got[0] != 1 {
		t.Fatalf("got %v, want the seed at 1.0", got)
	}
	if got[1] != 0.5 {
		t.Fatalf("got %v, want doc 1 at 0.5", got)
	}
}

func TestExplicitSeedsAreExcludedToo(t *testing.T) {
	// Query.Seeds names documents the caller already knows about, so handing
	// them back is no more useful than echoing a seed scorer.
	ix := index(t, node{"a", []string{"b"}}, node{"b", nil})
	got := scores(t, New(ix, nil), engine.Query{Seeds: []string{"a"}}, 10)
	if _, echoed := got[0]; echoed {
		t.Fatalf("got %v — an explicitly named seed came back", got)
	}
}

// ---------------------------------------------------------------------------
// Traversal
// ---------------------------------------------------------------------------

func TestProximityDecaysWithHops(t *testing.T) {
	ix := index(t,
		node{"a", []string{"b"}},
		node{"b", []string{"c"}},
		node{"c", nil},
	)
	got := scores(t, New(ix, nil), engine.Query{Seeds: []string{"a"}}, 10)
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 documents (the seed is excluded)", got)
	}
	for doc, want := range map[engine.DocID]float64{1: 0.5, 2: 1.0 / 3} {
		if got[doc] != want {
			t.Errorf("doc %d scored %v, want %v", doc, got[doc], want)
		}
	}

	// Same graph, literal variant: the seed reappears at 1.0 and nothing else
	// changes.
	lit := scores(t, NewIncludingSeeds(ix, nil), engine.Query{Seeds: []string{"a"}}, 10)
	if lit[0] != 1 || lit[1] != 0.5 || lit[2] != 1.0/3 {
		t.Errorf("including seeds: got %v, want {0:1 1:0.5 2:0.333}", lit)
	}
}

func TestCyclesTerminate(t *testing.T) {
	// Without dist doubling as the visited set, this test would hang rather than
	// fail.
	ix := index(t,
		node{"a", []string{"b"}},
		node{"b", []string{"c"}},
		node{"c", []string{"a", "b"}},
	)
	got := scores(t, New(ix, nil), engine.Query{Seeds: []string{"a"}}, 10)
	if len(got) != 2 {
		t.Fatalf("got %v, want docs 1 and 2", got)
	}
	// The cycle leads back to the seed; it must not reappear with a nonzero hop
	// count.
	if _, back := got[0]; back {
		t.Errorf("got %v — the cycle re-admitted the seed", got)
	}
}

func TestUnreachableDocumentsAreExcluded(t *testing.T) {
	ix := index(t,
		node{"a", []string{"b"}},
		node{"b", nil},
		node{"island", nil},
	)
	got := scores(t, New(ix, nil), engine.Query{Seeds: []string{"a"}}, 10)
	if _, ok := got[2]; ok {
		t.Fatalf("unreachable document was scored: %v", got)
	}
	if len(got) != 1 {
		t.Fatalf("got %v, want only doc 1", got)
	}
}

func TestMaxDepthBounds(t *testing.T) {
	// A chain one node longer than MaxDepth reaches: everything within MaxDepth
	// hops is in, the next node is out.
	ix := index(t,
		node{"n0", []string{"n1"}},
		node{"n1", []string{"n2"}},
		node{"n2", []string{"n3"}},
		node{"n3", []string{"n4"}},
		node{"n4", nil},
	)
	got := scores(t, New(ix, nil), engine.Query{Seeds: []string{"n0"}}, 10)
	if len(got) != MaxDepth {
		t.Fatalf("got %d docs, want %d (MaxDepth=%d, seed excluded)", len(got), MaxDepth, MaxDepth)
	}
	if _, ok := got[engine.DocID(MaxDepth+1)]; ok {
		t.Fatalf("the node at %d hops was included, MaxDepth is %d", MaxDepth+1, MaxDepth)
	}
}

func TestDanglingLinksAreIgnored(t *testing.T) {
	// A link to a key that was never added must be skipped, not panic and not
	// invent a document.
	ix := index(t,
		node{"a", []string{"never-added", "b"}},
		node{"b", nil},
	)
	got := scores(t, New(ix, nil), engine.Query{Seeds: []string{"a"}}, 10)
	if len(got) != 1 || got[1] != 0.5 {
		t.Fatalf("got %v, want only doc 1 at 0.5", got)
	}
}

func TestForwardLinksResolve(t *testing.T) {
	// "a" links to "c" before "c" is added. Links resolve at traversal time
	// precisely so that indexing order does not matter.
	ix := index(t,
		node{"a", []string{"c"}},
		node{"b", nil},
		node{"c", nil},
	)
	got := scores(t, New(ix, nil), engine.Query{Seeds: []string{"a"}}, 10)
	if got[2] != 0.5 {
		t.Fatalf("doc 2 scored %v, want 0.5 — a forward link failed to resolve", got[2])
	}
}

// ---------------------------------------------------------------------------
// Seed resolution
// ---------------------------------------------------------------------------

func TestUnknownSeedKeysAreSkipped(t *testing.T) {
	ix := index(t, node{"a", []string{"b"}}, node{"b", nil})
	got := scores(t, New(ix, nil), engine.Query{Seeds: []string{"ghost", "a"}}, 10)
	if len(got) != 1 || got[1] != 0.5 {
		t.Fatalf("got %v, want only doc 1 at 0.5", got)
	}
	// Every seed unknown means no seeds, which means no opinion.
	if len(scores(t, New(ix, nil), engine.Query{Seeds: []string{"ghost"}}, 10)) != 0 {
		t.Fatal("an entirely unknown seed set produced results")
	}
}

func TestNoSeedsAndNoSeedScorerMeansNoOpinion(t *testing.T) {
	ix := index(t, node{"a", []string{"b"}}, node{"b", nil})
	if got := scores(t, New(ix, nil), engine.Query{Text: "anything"}, 10); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}

func TestSeedScorerSuppliesSeeds(t *testing.T) {
	ix := index(t, node{"a", []string{"b"}}, node{"b", nil})
	seed := stub{cands: []engine.Candidate{{Doc: 0, Score: 7}}}
	got := scores(t, New(ix, seed), engine.Query{Text: "anything"}, 10)
	if len(got) != 1 || got[1] != 0.5 {
		t.Fatalf("got %v, want only doc 1 at 0.5", got)
	}
}

func TestQuerySeedsOverrideSeedScorer(t *testing.T) {
	// The escape hatch that keeps graph proximity independent of whatever scorer
	// is seeding it.
	ix := index(t,
		node{"a", []string{"c"}},
		node{"b", []string{"d"}},
		node{"c", nil},
		node{"d", nil},
	)
	seed := stub{cands: []engine.Candidate{{Doc: 0}}} // would traverse a -> c
	got := scores(t, New(ix, seed), engine.Query{Seeds: []string{"b"}}, 10)

	if _, fromScorer := got[2]; fromScorer {
		t.Fatalf("got %v — traversal started from the seed scorer, not Query.Seeds", got)
	}
	if len(got) != 1 || got[3] != 0.5 {
		t.Fatalf("got %v, want only doc 3 at 0.5", got)
	}
}

func TestSeedScorerErrorIsWrapped(t *testing.T) {
	boom := errors.New("boom")
	ix := index(t, node{"a", nil})
	_, err := New(ix, stub{err: boom}).Candidates(t.Context(), engine.Query{Text: "x"}, 10)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap boom", err)
	}
}

// ---------------------------------------------------------------------------
// Boundaries
// ---------------------------------------------------------------------------

func TestRespectsK(t *testing.T) {
	ix := index(t,
		node{"a", []string{"b", "c"}},
		node{"b", nil},
		node{"c", nil},
	)
	s := New(ix, nil)
	q := engine.Query{Seeds: []string{"a"}}

	cands, err := s.Candidates(t.Context(), q, 1)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	// Both neighbours tie at 0.5, so the tiebreak on DocID decides.
	if cands[0].Doc != 1 {
		t.Fatalf("got %+v, want doc 1 first", cands)
	}

	for _, k := range []int{0, -1} {
		if len(scores(t, s, q, k)) != 0 {
			t.Fatalf("k=%d returned results", k)
		}
	}
}

func TestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ix := index(t, node{"a", []string{"b"}}, node{"b", nil})
	if _, err := New(ix, nil).Candidates(ctx, engine.Query{Seeds: []string{"a"}}, 10); err == nil {
		t.Fatal("Candidates on a cancelled context returned no error")
	}
}

var (
	_ engine.Scorer = New(nil, nil)
	_ engine.Scorer = NewIncludingSeeds(nil, nil)
)
