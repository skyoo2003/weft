// SPDX-License-Identifier: Apache-2.0

package recency

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
)

var refNow = time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)

func index(t *testing.T, ages ...time.Duration) *engine.Index {
	t.Helper()
	ix := engine.New()
	for i, age := range ages {
		if _, err := ix.Add(engine.Document{Key: string(rune('a' + i)), Time: refNow.Add(-age)}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	return ix
}

func TestHalfLifeHalvesTheScore(t *testing.T) {
	ix := index(t, 0, HalfLife, 2*HalfLife)
	cands, err := NewAt(ix, refNow).Candidates(t.Context(), engine.Query{}, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	// One half-life halves the score. Two does not halve it again: the decay is
	// 1/(1+x), so it is 1/3 rather than 0.25.
	want := []float64{1, 0.5, 1.0 / 3.0}
	if len(cands) != len(want) {
		t.Fatalf("got %d candidates, want %d", len(cands), len(want))
	}
	// Newest first.
	for i, c := range cands {
		if math.Abs(c.Score-want[i]) > 1e-9 {
			t.Errorf("rank %d: doc %d scored %v, want %v", i, c.Doc, c.Score, want[i])
		}
	}
}

func TestVeryOldDocumentsStayOrdered(t *testing.T) {
	// Two separate ways an old document loses its ordering, in one fixture:
	//
	//   past ~88 years   2^(-age/HalfLife) underflows float64 to exactly zero
	//   past ~292 years  now.Sub saturates at the maximum time.Duration
	//
	// Either one gives a whole era the same score, and TopK then breaks the tie
	// on DocID. These are indexed oldest first, so a tie hands them back in
	// exactly the wrong order. 1000 and 400 years are past saturation, 200 and
	// 90 past underflow. Ages go through AddDate because 1000 years is not
	// expressible as a time.Duration in the first place.
	ix := engine.New()
	for i, years := range []int{1000, 400, 200, 90} {
		doc := engine.Document{Key: fmt.Sprintf("d%d", i), Time: refNow.AddDate(-years, 0, 0)}
		if _, err := ix.Add(doc); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	cands, err := NewAt(ix, refNow).Candidates(t.Context(), engine.Query{}, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	want := []engine.DocID{3, 2, 1, 0} // newest first, the reverse of insertion order
	if len(cands) != len(want) {
		t.Fatalf("got %d candidates, want %d", len(cands), len(want))
	}
	for i, c := range cands {
		if c.Doc != want[i] {
			t.Fatalf("rank %d is doc %d, want %d — got %+v", i, c.Doc, want[i], cands)
		}
		if c.Score <= 0 {
			t.Fatalf("doc %d scored %v, want > 0", c.Doc, c.Score)
		}
		if i > 0 && c.Score >= cands[i-1].Score {
			t.Fatalf("doc %d scored %v, which is not below doc %d's %v",
				c.Doc, c.Score, cands[i-1].Doc, cands[i-1].Score)
		}
	}
}

func TestFutureTimestampsCapAtOne(t *testing.T) {
	// A clock-skewed document must not outrank a genuinely new one.
	//
	// The future ages here are all inside one half-life, which is the only range
	// where dropping the clamp is visible. Past a half-life the unclamped score
	// goes negative and sorts to the bottom, so a year-in-the-future fixture
	// asserts nothing: -HalfLife lands exactly on the +Inf pole, and half of it
	// scores 2.0 and takes an unbeatable rank 1.
	for _, skew := range []time.Duration{time.Second, HalfLife / 2, HalfLife, HalfLife + time.Second} {
		ix := index(t, -skew, 0)
		cands, err := NewAt(ix, refNow).Candidates(t.Context(), engine.Query{}, 10)
		if err != nil {
			t.Fatalf("skew %v: Candidates: %v", skew, err)
		}
		for _, c := range cands {
			if !(c.Score <= 1) { // !(<=1) rather than >1, so a NaN fails too.
				t.Fatalf("skew %v: doc %d scored %v, want <= 1", skew, c.Doc, c.Score)
			}
		}
	}
}

func TestDocumentsWithoutTimestampsAreSkipped(t *testing.T) {
	ix := engine.New()
	if _, err := ix.Add(engine.Document{Key: "notime", Text: "x"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := ix.Add(engine.Document{Key: "timed", Time: refNow}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	cands, err := NewAt(ix, refNow).Candidates(t.Context(), engine.Query{}, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cands) != 1 || cands[0].Doc != 1 {
		t.Fatalf("Candidates = %+v, want only doc 1", cands)
	}
}

func TestBoundaries(t *testing.T) {
	ix := index(t, 0, time.Hour)
	s := NewAt(ix, refNow)
	for _, k := range []int{0, -1} {
		got, err := s.Candidates(t.Context(), engine.Query{}, k)
		if err != nil {
			t.Fatalf("Candidates(k=%d): %v", k, err)
		}
		if len(got) != 0 {
			t.Fatalf("k=%d returned %+v, want none", k, got)
		}
	}
	if got, err := NewAt(engine.New(), refNow).Candidates(t.Context(), engine.Query{}, 10); err != nil || len(got) != 0 {
		t.Fatalf("empty index returned %+v, %v", got, err)
	}
}

func TestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewAt(index(t, 0), refNow).Candidates(ctx, engine.Query{}, 10); err == nil {
		t.Fatal("Candidates on a cancelled context returned no error")
	}
}

func TestNewUsesTheWallClock(t *testing.T) {
	// New must differ from NewAt only in where the clock comes from, so this has
	// to score against real time rather than just check Name(). New is the only
	// constructor a caller outside a test would reach for, and asserting on the
	// name alone never invokes the clock at all: swapping time.Now for anything
	// else, a zero Time included, passed the whole suite.
	now := time.Now()
	ix := engine.New()
	for key, stamp := range map[string]time.Time{"fresh": now, "stale": now.Add(-10 * HalfLife)} {
		if _, err := ix.Add(engine.Document{Key: key, Time: stamp}); err != nil {
			t.Fatalf("Add(%q): %v", key, err)
		}
	}
	s := New(ix)
	if got := s.Name(); got != "recency" {
		t.Fatalf("Name() = %q", got)
	}
	cands, err := s.Candidates(t.Context(), engine.Query{}, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("Candidates = %+v, want two", cands)
	}
	// The spread is what pins the clock. Asserting only that the fresh document
	// scores ~1 would also pass for a clock stuck at the zero Time, which reads
	// every document as future-dated and clamps them all to 1.
	if cands[0].Score < 0.999 {
		t.Errorf("freshest document scored %v against the wall clock, want ~1", cands[0].Score)
	}
	if cands[1].Score > 0.1 {
		t.Errorf("a document ten half-lives old scored %v, want well under 1 — the clock is not moving", cands[1].Score)
	}
}

func TestTheOldestRepresentableTimestampIsNotBrandNew(t *testing.T) {
	// Unix seconds span far more than int64 can hold, so subtracting them in
	// int64 wraps. A wrapped negative age is then clamped to zero by the
	// future-timestamp guard, which scores the oldest possible document 1.0 and
	// puts it above a document from yesterday — a rank inversion, not a tie.
	ix := engine.New()
	for i, ts := range []time.Time{
		time.Unix(math.MinInt64, 0),
		refNow.Add(-24 * time.Hour),
	} {
		if _, err := ix.Add(engine.Document{Key: fmt.Sprintf("d%d", i), Time: ts}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	got, err := NewAt(ix, refNow).Candidates(t.Context(), engine.Query{}, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 2 || got[0].Doc != 1 {
		t.Fatalf("Candidates = %+v, want the day-old doc 1 ranked first", got)
	}
	if !(got[1].Score > 0) || got[1].Score >= got[0].Score {
		t.Fatalf("oldest doc scored %v against the day-old %v, want a smaller positive score",
			got[1].Score, got[0].Score)
	}
}

// cancelAfter reports itself cancelled only once Err has been called n times.
// Duplicated from the other scorers for the reason given there.
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

func TestCancellationArrivingAfterTheLastDocumentIsObserved(t *testing.T) {
	// One document, so n = 1 spends the free call on the only per-document check
	// and cancellation lands on the check guarding TopK. Without that check the
	// scorer sorted the whole corpus and reported success past the deadline.
	ctx := &cancelAfter{Context: t.Context(), n: 1}
	got, err := NewAt(index(t, 0), refNow).Candidates(ctx, engine.Query{}, 10)
	if err == nil {
		t.Fatalf("Candidates returned %+v and no error after cancellation before the sort", got)
	}
}

var _ engine.Scorer = (*Scorer)(nil)
