package fusion

import (
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

func docs(cands []engine.Candidate) []engine.DocID {
	ids := make([]engine.DocID, len(cands))
	for i, c := range cands {
		ids[i] = c.Doc
	}
	return ids
}

func equal(got []engine.DocID, want ...engine.DocID) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestSingleStreamPreservesOrder(t *testing.T) {
	stream := []engine.Candidate{{Doc: 5, Score: 9}, {Doc: 3, Score: 4}, {Doc: 8, Score: 1}}
	got := docs(Fuse([][]engine.Candidate{stream}, 10))
	if !equal(got, 5, 3, 8) {
		t.Fatalf("Fuse = %v, want [5 3 8]", got)
	}
}

func TestScoresAreNeverRead(t *testing.T) {
	// Doc 1 is ranked first with a negligible score, doc 2 second with an
	// enormous one. If Fuse looked at Score at all, doc 2 would win.
	stream := []engine.Candidate{{Doc: 1, Score: 0.0001}, {Doc: 2, Score: 999999}}
	got := docs(Fuse([][]engine.Candidate{stream}, 10))
	if !equal(got, 1, 2) {
		t.Fatalf("Fuse = %v, want [1 2] — Score leaked into the ranking", got)
	}
}

func TestAgreementBeatsSingleStreamConfidence(t *testing.T) {
	// Doc 2 is second in both streams; doc 1 is first in one and absent from the
	// other. Two mid-rank appearances must outweigh one top-rank appearance.
	a := []engine.Candidate{{Doc: 1, Score: 100}, {Doc: 2, Score: 1}}
	b := []engine.Candidate{{Doc: 3, Score: 100}, {Doc: 2, Score: 1}}
	got := docs(Fuse([][]engine.Candidate{a, b}, 10))
	if len(got) != 3 || got[0] != 2 {
		t.Fatalf("Fuse = %v, want doc 2 first", got)
	}
}

func TestIncompatibleScoreScalesDoNotDistortRanking(t *testing.T) {
	// The real reason RRF was chosen: unbounded BM25-like scores in one stream,
	// [-1,1] cosine-like scores in another. A weighted sum without normalization
	// would let the first stream dictate the whole ranking.
	bm25 := []engine.Candidate{{Doc: 1, Score: 42.7}, {Doc: 2, Score: 31.9}}
	cosine := []engine.Candidate{{Doc: 2, Score: 0.81}, {Doc: 3, Score: -0.4}}
	got := docs(Fuse([][]engine.Candidate{bm25, cosine}, 10))
	// Doc 2 appears in both (ranks 2 and 1), so it must lead despite its scores
	// being tiny next to doc 1's.
	if len(got) != 3 || got[0] != 2 {
		t.Fatalf("Fuse = %v, want doc 2 first", got)
	}
}

func TestEmptyInputs(t *testing.T) {
	tests := []struct {
		name    string
		streams [][]engine.Candidate
		k       int
	}{
		{"nil streams", nil, 10},
		{"zero streams", [][]engine.Candidate{}, 10},
		{"one empty stream", [][]engine.Candidate{{}}, 10},
		{"all streams empty", [][]engine.Candidate{{}, {}, {}}, 10},
		{"k zero", [][]engine.Candidate{{{Doc: 1}}}, 0},
		{"k negative", [][]engine.Candidate{{{Doc: 1}}}, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The point of the table is that none of these panic.
			if got := Fuse(tc.streams, tc.k); len(got) != 0 {
				t.Fatalf("Fuse = %+v, want empty", got)
			}
		})
	}
}

func TestRespectsK(t *testing.T) {
	stream := []engine.Candidate{{Doc: 1}, {Doc: 2}, {Doc: 3}, {Doc: 4}}
	got := Fuse([][]engine.Candidate{stream}, 2)
	if len(got) != 2 {
		t.Fatalf("Fuse returned %d candidates, want 2", len(got))
	}
}

// TestScorerCountIsIrrelevant is the architecture assertion at the fusion layer:
// the same call shape works for any number of streams, so adding a scorer never
// changes this function's signature or body.
func TestScorerCountIsIrrelevant(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 7, 20} {
		streams := make([][]engine.Candidate, n)
		for i := range streams {
			streams[i] = []engine.Candidate{
				{Doc: engine.DocID(i), Score: float64(i)},
				{Doc: 999, Score: 0},
			}
		}
		got := Fuse(streams, 5)
		if len(got) == 0 {
			t.Fatalf("Fuse with %d streams returned nothing", n)
		}
		// Doc 999 sits at rank 2 in every stream; with two or more streams its
		// accumulated score must put it first.
		if n >= 2 && got[0].Doc != 999 {
			t.Fatalf("with %d streams, Fuse = %v, want doc 999 first", n, docs(got))
		}
	}
}

func TestDuplicateDocWithinOneStreamAccumulates(t *testing.T) {
	// Not a case any scorer produces today, but Fuse must not corrupt or panic
	// if a stream ever repeats a document.
	stream := []engine.Candidate{{Doc: 1}, {Doc: 1}, {Doc: 2}}
	got := Fuse([][]engine.Candidate{stream}, 10)
	if len(got) != 2 {
		t.Fatalf("Fuse = %+v, want 2 distinct docs", got)
	}
	if got[0].Doc != 1 {
		t.Fatalf("Fuse = %v, want doc 1 first", docs(got))
	}
}
