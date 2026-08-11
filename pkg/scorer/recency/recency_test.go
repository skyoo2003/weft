package recency

import (
	"context"
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

func TestCenturiesOldDocumentsStayOrdered(t *testing.T) {
	// The literal 2^(-age/HalfLife) underflows to exactly zero past about 88
	// years. Every document beyond that scores 0, TopK breaks the tie on DocID,
	// and insertion order silently replaces recency. These three are indexed
	// oldest first, so a tie hands them back in exactly the wrong order.
	const year = 365.25 * 24 * time.Hour
	ix := index(t, 200*year, 100*year, 90*year)
	cands, err := NewAt(ix, refNow).Candidates(t.Context(), engine.Query{}, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	want := []engine.DocID{2, 1, 0} // newest first, the reverse of insertion order
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
	ix := index(t, -365*24*time.Hour, 0)
	cands, err := NewAt(ix, refNow).Candidates(t.Context(), engine.Query{}, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	for _, c := range cands {
		if c.Score > 1 {
			t.Fatalf("doc %d scored %v, want <= 1", c.Doc, c.Score)
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
	// New must differ from NewAt only in where the clock comes from.
	if got := New(index(t, 0)).Name(); got != "recency" {
		t.Fatalf("Name() = %q", got)
	}
}

var _ engine.Scorer = (*Scorer)(nil)
