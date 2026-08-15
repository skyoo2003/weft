// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"errors"
	"fmt"
	"math"
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
// Multi-seed accumulation — the fix milestone 4's degeneracy measurement forced
// (docs/EVAL.md sections 5.7 and 5.9)
// ---------------------------------------------------------------------------

// TestSeedsAgreeingRaisesScore is the whole reason the score became a sum. Under the
// previous nearest-seed formula every one of these documents scored 0.5 and TopK's
// DocID tiebreak decided the order, which on a real citation graph meant the stream
// ranked by corpus insertion order.
func TestSeedsAgreeingRaisesScore(t *testing.T) {
	// s0, s1, s2 are seeds. shared is cited by all three; pair by two; lonely by one.
	ix := index(t,
		node{"s0", []string{"shared", "pair", "lonely"}},
		node{"s1", []string{"shared", "pair"}},
		node{"s2", []string{"shared"}},
		node{"shared", nil},
		node{"pair", nil},
		node{"lonely", nil},
	)
	q := engine.Query{Seeds: []string{"s0", "s1", "s2"}}
	got := scores(t, New(ix, nil), q, 10)

	// One hop from n seeds sums to n * 0.5.
	for key, want := range map[string]float64{"shared": 1.5, "pair": 1.0, "lonely": 0.5} {
		id, ok := ix.Resolve(key)
		if !ok {
			t.Fatalf("%s missing from the fixture", key)
		}
		if got[id] != want {
			t.Errorf("%s scored %v, want %v", key, got[id], want)
		}
	}

	// And the ordering that follows, which is the point: agreement outranks
	// proximity to any single seed.
	cands, err := New(ix, nil).Candidates(t.Context(), q, 3)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	var order []string
	for _, c := range cands {
		doc, _ := ix.Doc(c.Doc)
		order = append(order, doc.Key)
	}
	if len(order) != 3 || order[0] != "shared" || order[1] != "pair" || order[2] != "lonely" {
		t.Errorf("order = %v, want [shared pair lonely]", order)
	}
}

// TestRepeatedSeedVotesOnce covers what the sum broke about seed handling. Query.Seeds
// is caller-supplied, so two of its entries can name the same document; the old merged
// BFS absorbed that because a repeated id was already in dist, and a per-seed sum does
// not. Left unfixed, listing a seed twice doubles every score it reaches — a ranking
// change with no cause in the graph.
func TestRepeatedSeedVotesOnce(t *testing.T) {
	ix := index(t,
		node{"s0", []string{"x"}},
		node{"other", []string{"y"}},
		node{"x", nil},
		node{"y", nil},
	)
	once := scores(t, New(ix, nil), engine.Query{Seeds: []string{"s0", "other"}}, 10)
	twice := scores(t, New(ix, nil), engine.Query{Seeds: []string{"s0", "s0", "other"}}, 10)

	for _, key := range []string{"x", "y"} {
		id, ok := ix.Resolve(key)
		if !ok {
			t.Fatalf("%s missing from the fixture", key)
		}
		if once[id] != twice[id] {
			t.Errorf("%s scored %v with the seed listed once and %v with it listed twice",
				key, once[id], twice[id])
		}
	}
}

// TestSeedOrderDoesNotChangeScores is the regression for a ranking that moved when
// nothing about the graph did.
//
// The sum used to accumulate per seed, in the caller's seed order, and float addition
// is not associative. A document reached at hops {1,1,1,2,3} therefore summed to
// 2.083333333333333 or 2.0833333333333335 depending on which seed was listed first —
// so two documents with the same multiset of distances, which the formula says tie,
// came out unequal and TopK's DocID tiebreak never ran. Permuting Query.Seeds then
// reordered the result.
//
// Bit equality, not a tolerance: the whole failure is in the last bit, and any epsilon
// wide enough to be a tolerance hides it.
func TestSeedOrderDoesNotChangeScores(t *testing.T) {
	// mid and far put one document at three different distances from the five seeds:
	// three at one hop, one at two, one at three. That is the {½,½,½,⅓,¼} multiset
	// above, and it needs SeedN seeds and the full MaxDepth to reach.
	ix := index(t,
		node{"s0", []string{"target"}},
		node{"s1", []string{"target"}},
		node{"s2", []string{"target"}},
		node{"s3", []string{"mid"}},
		node{"s4", []string{"far"}},
		node{"mid", []string{"target"}},
		node{"far", []string{"near"}},
		node{"near", []string{"target"}},
		node{"target", nil},
	)
	seeds := []string{"s0", "s1", "s2", "s3", "s4"}
	target, ok := ix.Resolve("target")
	if !ok {
		t.Fatal("target missing from the fixture")
	}

	want := scores(t, New(ix, nil), engine.Query{Seeds: seeds}, 10)[target]
	// 3·½ + ⅓ + ¼, summed in hop order, so that the test still means something if
	// every ordering drifts together. Pinned as a decimal literal rather than as that
	// expression: Go folds an untyped constant expression at arbitrary precision, and
	// 25/12 rounded once is 2.0833333333333335, which is not what three float64
	// roundings of the same terms come to.
	if want != 2.083333333333333 {
		t.Errorf("target scored %.17g, want 2.083333333333333 (3/2 + 1/3 + 1/4 by hop)", want)
	}

	// Every rotation and the reversal. The old accumulation differs from the first
	// ordering on at least one of these; which one is not the point.
	for i := range seeds {
		p := append(append([]string(nil), seeds[i:]...), seeds[:i]...)
		if got := scores(t, New(ix, nil), engine.Query{Seeds: p}, 10)[target]; got != want {
			t.Errorf("seeds %v: target scored %.17g, want %.17g — the same distances in a "+
				"different seed order", p, got, want)
		}
	}
	rev := append([]string(nil), seeds...)
	for l, r := 0, len(rev)-1; l < r; l, r = l+1, r-1 {
		rev[l], rev[r] = rev[r], rev[l]
	}
	if got := scores(t, New(ix, nil), engine.Query{Seeds: rev}, 10)[target]; got != want {
		t.Errorf("seeds %v: target scored %.17g, want %.17g", rev, got, want)
	}
}

// TestEqualDistanceMultisetsTie is the half of permutation invariance a caller can see.
//
// Two documents the same distances from the same seeds have to hold the same float64,
// or TopK ranks them by a rounding artifact instead of falling through to its DocID
// tiebreak — the same thing pkg/fusion's rank-major sweep exists to prevent.
func TestEqualDistanceMultisetsTie(t *testing.T) {
	// a and b are both {1,1,1,2,3} hops from the seeds, reached through different
	// seeds at each distance so the per-seed accumulation order differs between them.
	ix := index(t,
		node{"s0", []string{"a", "b"}},
		node{"s1", []string{"a", "b"}},
		node{"s2", []string{"a", "b"}},
		node{"s3", []string{"amid", "bfar"}},
		node{"s4", []string{"afar", "bmid"}},
		node{"amid", []string{"a"}},
		node{"bmid", []string{"b"}},
		node{"afar", []string{"anear"}},
		node{"bfar", []string{"bnear"}},
		node{"anear", []string{"a"}},
		node{"bnear", []string{"b"}},
		node{"a", nil},
		node{"b", nil},
	)
	got := scores(t, New(ix, nil), engine.Query{Seeds: []string{"s0", "s1", "s2", "s3", "s4"}}, 20)
	a, _ := ix.Resolve("a")
	b, _ := ix.Resolve("b")
	if got[a] != got[b] {
		t.Errorf("a scored %.17g and b scored %.17g — the same distance multiset must be "+
			"the same float64, so TopK settles them on DocID", got[a], got[b])
	}
}

// TestOneSeedIsUnchangedByTheSum pins backward compatibility: with a single seed the
// sum has one term, so every score is the nearest-seed value it was before. Most of
// the rest of this file relies on that.
func TestOneSeedIsUnchangedByTheSum(t *testing.T) {
	ix := index(t,
		node{"a", []string{"b"}},
		node{"b", []string{"c"}},
		node{"c", []string{"d"}},
		node{"d", nil},
	)
	got := scores(t, New(ix, nil), engine.Query{Seeds: []string{"a"}}, 10)
	for id, want := range map[engine.DocID]float64{1: 0.5, 2: 1.0 / 3, 3: 0.25} {
		if got[id] != want {
			t.Errorf("doc %d scored %v, want %v", id, got[id], want)
		}
	}
}

// TestSeedReachableFromAnotherSeedIsStillExcluded covers what the sum broke about the
// old exclusion rule. Previously a seed was recognisable by hops == 0; now a seed one
// hop from a sibling accumulates 1.0 + 0.5, so exclusion has to be by identity rather
// than by score.
func TestSeedReachableFromAnotherSeedIsStillExcluded(t *testing.T) {
	ix := index(t,
		node{"s0", []string{"s1"}}, // s0 cites s1, and both are seeds
		node{"s1", []string{"x"}},
		node{"x", nil},
	)
	q := engine.Query{Seeds: []string{"s0", "s1"}}

	got := scores(t, New(ix, nil), q, 10)
	s1, _ := ix.Resolve("s1")
	if _, present := got[s1]; present {
		t.Errorf("s1 is a seed but came back at %v", got[s1])
	}
	x, _ := ix.Resolve("x")
	// x is one hop from s1 and two from s0: 1/3 + 0.5.
	//
	// Compared with a tolerance, unlike the exact assertions above. 1/3 is not
	// representable, and the sum accumulates in seed order at runtime — two
	// roundings — while a Go constant expression like 0.5 + 1.0/3 is evaluated in
	// arbitrary precision and rounded once. The two differ in the last bit. That is
	// a property of the arithmetic, not of the traversal, and the traversal is what
	// this test is about; the order itself is deterministic because seeds are walked
	// in slice order.
	if want, tol := 1.0/3+0.5, 1e-12; math.Abs(got[x]-want) > tol {
		t.Errorf("x scored %v, want %v", got[x], want)
	}

	// The literal variant keeps s1, and its score carries both contributions.
	lit := scores(t, NewIncludingSeeds(ix, nil), q, 10)
	if want := 1 + 0.5; lit[s1] != want {
		t.Errorf("including seeds: s1 scored %v, want %v (1.0 from itself, 0.5 from s0)", lit[s1], want)
	}
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
	// The expectation is written out rather than derived from MaxDepth. Comparing
	// the result against the constant under test makes any value self-validating:
	// at MaxDepth = 2 this fixture returns 2 documents and a derived assertion
	// still passes, so halving the traversal's reach would go unnoticed.
	want := map[engine.DocID]float64{1: 1.0 / 2, 2: 1.0 / 3, 3: 1.0 / 4}
	got := scores(t, New(ix, nil), engine.Query{Seeds: []string{"n0"}}, 10)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v (MaxDepth=%d, seed excluded)", got, want, MaxDepth)
	}
	for id, score := range want {
		if got[id] != score {
			t.Fatalf("doc %d scored %v, want %v — full result %v", id, got[id], score, got)
		}
	}
	if _, ok := got[4]; ok {
		t.Fatalf("n4 sits four hops out and was included, MaxDepth is %d: %v", MaxDepth, got)
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

func TestSeedScorerDocIDsAreCheckedAgainstThisIndex(t *testing.T) {
	// A DocID means nothing outside the index that assigned it, so a seed scorer
	// reading a different index hands back ids from another namespace. The
	// traversal's own Doc miss only skips expansion — the id stays in dist at hop
	// zero — so without this check NewIncludingSeeds emits a document that does
	// not exist at score 1.0, the top rank, and every caller that ignores Doc's
	// bool prints it as a blank row.
	ix := index(t, node{"a", []string{"b"}}, node{"b", nil})
	seed := stub{cands: []engine.Candidate{{Doc: 0}, {Doc: 9999}}}

	for name, s := range map[string]*Scorer{"New": New(ix, seed), "NewIncludingSeeds": NewIncludingSeeds(ix, seed)} {
		for id := range scores(t, s, engine.Query{Text: "anything"}, 10) {
			if _, ok := ix.Doc(id); !ok {
				t.Errorf("%s returned doc %d, which this index never assigned", name, id)
			}
		}
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

// cancelAfter reports itself cancelled only once Err has been called n times.
// A plain cancelled context is caught by the per-node check before the link
// loop starts, so it cannot distinguish "cancellation is observed" from
// "cancellation is observed in time".
type cancelAfter struct {
	context.Context
	n int
}

func (c *cancelAfter) Err() error {
	if c.n > 0 {
		c.n--
		return nil
	}
	return context.Canceled
}

func TestCancellationIsObservedInsideTheLinkScan(t *testing.T) {
	// One seed whose links are both numerous and dangling: nothing is enqueued,
	// so the queue ends after a single node and the per-node check never runs a
	// second time. The three free calls are seedDocs' i == 0 poll, the per-node
	// check, and the link scan's i == 0 poll, which puts the cancellation on the
	// i == 1024 poll — inside the scan, the only place the old code had no check
	// at all. The count must include seedDocs: while it did not, deleting the
	// entire link-scan poll left this test green, because the cancellation fell
	// through to the guard before TopK instead.
	links := make([]string, 3000)
	for i := range links {
		links[i] = fmt.Sprintf("nowhere%d", i)
	}
	ix := index(t, node{"a", links})

	ctx := &cancelAfter{Context: t.Context(), n: 4}
	got, err := New(ix, nil).Candidates(ctx, engine.Query{Seeds: []string{"a"}}, 10)
	if err == nil {
		t.Fatalf("Candidates returned %d results and no error after mid-scan cancellation", len(got))
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Candidates error = %v, want context.Canceled", err)
	}
}

func TestCancellationIsObservedWhileResolvingExplicitSeeds(t *testing.T) {
	// Every seed key unknown, so nothing is enqueued and the traversal that polls
	// the context never starts: the old code resolved all 3000 keys and returned
	// a successful empty result, which a caller cannot tell apart from "no seed
	// matched". A plainly cancelled context suffices — the poll at i == 0 is the
	// first check that exists anywhere on this path.
	seeds := make([]string, 3000)
	for i := range seeds {
		seeds[i] = fmt.Sprintf("missing%d", i)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := New(engine.New(), nil).Candidates(ctx, engine.Query{Seeds: seeds}, 10)
	if err == nil {
		t.Fatalf("Candidates returned %+v and no error while resolving seeds after cancellation", got)
	}
}

func TestCancellationArrivingAfterTheLastNodeIsObserved(t *testing.T) {
	// An explicit seed with one link, so the calls are countable: seedDocs' i == 0
	// poll, one per-dequeue check for the seed, one i == 0 poll over its single
	// link, and one more per-dequeue check for the node that link reached.
	// Cancellation lands on the fifth, the check guarding TopK — the window
	// between the last node and the sort, which no earlier check covers. The
	// count must include seedDocs, or the cancellation lands one check early and
	// deleting the pre-TopK guard leaves this green.
	ix := engine.New()
	for _, d := range []engine.Document{{Key: "a", Links: []string{"b"}}, {Key: "b"}} {
		if _, err := ix.Add(d); err != nil {
			t.Fatalf("Add(%q): %v", d.Key, err)
		}
	}
	ctx := &cancelAfter{Context: t.Context(), n: 4}
	got, err := New(ix, nil).Candidates(ctx, engine.Query{Seeds: []string{"a"}}, 10)
	if err == nil {
		t.Fatalf("Candidates returned %+v and no error after cancellation before the sort", got)
	}
}

var (
	_ engine.Scorer = New(nil, nil)
	_ engine.Scorer = NewIncludingSeeds(nil, nil)
)
