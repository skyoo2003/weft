package engine

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"lowercases", "Go BM25", []string{"go", "bm25"}},
		{"splits on punctuation", "fusion,scorer;graph", []string{"fusion", "scorer", "graph"}},
		{"collapses runs of separators", "  a   --  b  ", []string{"a", "b"}},
		{"separators only", "--- ,,, ...", nil},
		{"digits are tokens", "top 10 results", []string{"top", "10", "results"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Tokenize(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("Tokenize(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Tokenize(%q) = %q, want %q", tc.in, got, tc.want)
				}
			}
		})
	}
}

func TestAddAssignsDenseIDsInOrder(t *testing.T) {
	ix := New()
	for i, key := range []string{"a", "b", "c"} {
		id, err := ix.Add(Document{Key: key, Text: "x"})
		if err != nil {
			t.Fatalf("Add(%q): %v", key, err)
		}
		if id != DocID(i) {
			t.Fatalf("Add(%q) = %d, want %d", key, id, i)
		}
	}
	if got := ix.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3", got)
	}
}

func TestAddRejectsEmptyAndDuplicateKeys(t *testing.T) {
	ix := New()
	if _, err := ix.Add(Document{Key: "", Text: "x"}); !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("Add(empty key) err = %v, want ErrEmptyKey", err)
	}
	if _, err := ix.Add(Document{Key: "dup"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if _, err := ix.Add(Document{Key: "dup"}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("Add(duplicate) err = %v, want ErrDuplicateKey", err)
	}
	// The rejected duplicate must not have been stored.
	if got := ix.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1 — rejected Add mutated the index", got)
	}
}

func TestPostingsCountFrequencyAndStaySorted(t *testing.T) {
	ix := New()
	mustAdd(t, ix, Document{Key: "a", Text: "go go go"})
	mustAdd(t, ix, Document{Key: "b", Text: "rust"})
	mustAdd(t, ix, Document{Key: "c", Text: "go rust"})

	posts := ix.Lookup("go")
	if len(posts) != 2 {
		t.Fatalf("Lookup(go) returned %d postings, want 2", len(posts))
	}
	if posts[0].Doc != 0 || posts[0].Freq != 3 {
		t.Fatalf("posts[0] = %+v, want {Doc:0 Freq:3}", posts[0])
	}
	if posts[1].Doc != 2 || posts[1].Freq != 1 {
		t.Fatalf("posts[1] = %+v, want {Doc:2 Freq:1}", posts[1])
	}
	if got := ix.Lookup("absent"); got != nil {
		t.Fatalf("Lookup(absent) = %v, want nil", got)
	}
}

func TestDocLenAndAvgDocLen(t *testing.T) {
	ix := New()
	if got := ix.AvgDocLen(); got != 0 {
		t.Fatalf("AvgDocLen() on empty index = %v, want 0", got)
	}

	mustAdd(t, ix, Document{Key: "a", Text: "one two three four"}) // 4 tokens
	mustAdd(t, ix, Document{Key: "b", Text: "one two"})            // 2 tokens
	if got := ix.DocLen(0); got != 4 {
		t.Fatalf("DocLen(0) = %d, want 4", got)
	}
	if got := ix.AvgDocLen(); got != 3 {
		t.Fatalf("AvgDocLen() = %v, want 3", got)
	}
	// Unknown ids report 0 rather than panicking.
	if got := ix.DocLen(99); got != 0 {
		t.Fatalf("DocLen(99) = %d, want 0", got)
	}
}

func TestAvgDocLenOfEmptyDocumentsIsZero(t *testing.T) {
	// The avgdl == 0 boundary that BM25 length normalization must not divide by.
	ix := New()
	mustAdd(t, ix, Document{Key: "a", Text: ""})
	mustAdd(t, ix, Document{Key: "b", Text: "!!! ???"})
	if got := ix.AvgDocLen(); got != 0 {
		t.Fatalf("AvgDocLen() = %v, want 0", got)
	}
	if got := ix.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2 — empty text is still a document", got)
	}
}

func TestResolveAndDoc(t *testing.T) {
	ix := New()
	mustAdd(t, ix, Document{Key: "found", Text: "x"})

	id, ok := ix.Resolve("found")
	if !ok || id != 0 {
		t.Fatalf("Resolve(found) = %d, %v; want 0, true", id, ok)
	}
	// A key that was never added is how dangling Links get detected.
	if _, ok := ix.Resolve("dangling"); ok {
		t.Fatal("Resolve(dangling) reported ok — dangling links would resolve")
	}
	if d, ok := ix.Doc(0); !ok || d.Key != "found" {
		t.Fatalf("Doc(0) = %+v, %v", d, ok)
	}
	if _, ok := ix.Doc(99); ok {
		t.Fatal("Doc(99) reported ok for an unassigned id")
	}
}

func TestHugeDocIDsAreOutOfRangeNotAPanic(t *testing.T) {
	// DocID is uint32 and the bounds are compared in uint64. Under the earlier
	// int(id) comparison these ids wrapped negative on a 32-bit build, passed the
	// guard, and panicked on the slice access.
	//
	// This cannot fail on a 64-bit build, where int is wide enough that the old
	// comparison was already correct: it guards a 32-bit target rather than
	// reproducing the fault here.
	ix := New()
	mustAdd(t, ix, Document{Key: "only", Text: "x"})

	for _, id := range []DocID{1 << 31, 1<<31 + 1, math.MaxUint32} {
		if _, ok := ix.Doc(id); ok {
			t.Errorf("Doc(%d) reported ok for an unassigned id", id)
		}
		if got := ix.DocLen(id); got != 0 {
			t.Errorf("DocLen(%d) = %d, want 0", id, got)
		}
	}
}

func TestTopK(t *testing.T) {
	tests := []struct {
		name string
		in   []Candidate
		k    int
		want []DocID
	}{
		{"orders by score descending", []Candidate{{1, 0.1}, {2, 0.9}, {3, 0.5}}, 3, []DocID{2, 3, 1}},
		{"truncates to k", []Candidate{{1, 0.1}, {2, 0.9}, {3, 0.5}}, 2, []DocID{2, 3}},
		{"ties break on DocID", []Candidate{{7, 0.5}, {3, 0.5}, {5, 0.5}}, 3, []DocID{3, 5, 7}},
		{"k larger than input", []Candidate{{1, 0.1}}, 10, []DocID{1}},
		{"k zero returns nothing", []Candidate{{1, 0.1}}, 0, nil},
		{"negative k returns nothing", []Candidate{{1, 0.1}}, -1, nil},
		{"empty input", nil, 5, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TopK(tc.in, tc.k)
			if len(got) != len(tc.want) {
				t.Fatalf("TopK = %+v, want docs %v", got, tc.want)
			}
			for i := range got {
				if got[i].Doc != tc.want[i] {
					t.Fatalf("TopK = %+v, want docs %v", got, tc.want)
				}
			}
		})
	}
}

func TestSearchRejectsNilFuser(t *testing.T) {
	if _, err := Search(t.Context(), Query{}, 5, nil); !errors.Is(err, ErrNoFuser) {
		t.Fatalf("Search with nil Fuser err = %v, want ErrNoFuser", err)
	}
}

func TestSearchRejectsNilScorer(t *testing.T) {
	// A nil interface in the scorer list used to panic on the Candidates call.
	// Search already rejects a nil Fuser, and a scorer list assembled at runtime
	// is where an unconfigured optional scorer turns up.
	fuse := func(streams [][]Candidate, k int) []Candidate { return nil }
	ran := false
	ok := scorerFunc(func() { ran = true })

	_, err := Search(t.Context(), Query{}, 5, fuse, ok, nil)
	if !errors.Is(err, ErrNilScorer) {
		t.Fatalf("Search with a nil Scorer err = %v, want ErrNilScorer", err)
	}
	if !strings.Contains(err.Error(), "scorer 1") {
		t.Errorf("err = %q, want it to name the offending position", err)
	}
	if ran {
		t.Error("a scorer ran before the nil entry was reported; validate before scanning")
	}
}

// typedNilScorer dereferences its receiver, so a nil *typedNilScorer panics on
// any method call — the shape of every pointer-backed scorer in this tree.
type typedNilScorer struct{ name string }

func (s *typedNilScorer) Name() string { return s.name }

func (s *typedNilScorer) Candidates(context.Context, Query, int) ([]Candidate, error) {
	return nil, nil
}

func TestSearchRejectsTypedNilScorer(t *testing.T) {
	// The form runtime configuration produces: an optional scorer declared up
	// front and never assigned. The interface carries a type descriptor, so it is
	// not == nil and the plain guard waved it through into a panic — in a package
	// whose sentinels are introduced with "library code here never panics".
	var missing *typedNilScorer
	fuse := func(streams [][]Candidate, k int) []Candidate { return nil }

	_, err := Search(t.Context(), Query{}, 5, fuse, missing)
	if !errors.Is(err, ErrNilScorer) {
		t.Fatalf("Search with a typed-nil Scorer err = %v, want ErrNilScorer", err)
	}
	if !strings.Contains(err.Error(), "scorer 0") {
		t.Errorf("err = %q, want it to name the offending position", err)
	}
	// A live scorer of the same type must still be accepted, or the guard is
	// rejecting the type rather than the nil.
	if _, err := Search(t.Context(), Query{}, 5, fuse, &typedNilScorer{name: "live"}); err != nil {
		t.Fatalf("Search with a live scorer err = %v, want nil", err)
	}
}

func TestSearchRejectsNilScorerWhateverKIs(t *testing.T) {
	// A nil entry is a configuration mistake, not a query that found nothing, so
	// it has to surface independently of k. While the k check ran first, a caller
	// computing k dynamically — a page size, a limit parameter — got a silent
	// empty result until the first time k went positive, in production.
	fuse := func(streams [][]Candidate, k int) []Candidate { return nil }
	for _, k := range []int{0, -1} {
		if _, err := Search(t.Context(), Query{}, k, fuse, nil); !errors.Is(err, ErrNilScorer) {
			t.Errorf("Search with k=%d and a nil Scorer err = %v, want ErrNilScorer", k, err)
		}
	}
}

func TestSearchObservesCancellationBeforeFusing(t *testing.T) {
	// Every scorer checks the context before its own TopK so a late cancellation
	// does not buy a sort nobody reads. Fusion does the same shape of work and
	// Fuser takes no context, so Search is the only place that check can live.
	fused := false
	fuse := func(streams [][]Candidate, k int) []Candidate {
		fused = true
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	// Cancelled after the scorer runs, so the scorers themselves see a live
	// context and only the window before fusion is left.
	s := scorerFunc(cancel)

	_, err := Search(ctx, Query{}, 5, fuse, s)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Search err = %v, want context.Canceled", err)
	}
	if fused {
		t.Error("the fuser ran after the context was cancelled")
	}
}

// scorerFunc is a Scorer with no opinion that records having been asked.
type scorerFunc func()

func (s scorerFunc) Name() string { return "stub" }

func (s scorerFunc) Candidates(context.Context, Query, int) ([]Candidate, error) {
	s()
	return nil, nil
}

func TestTopKSortsNaNLastAndDeterministically(t *testing.T) {
	// Both NaN > x and x > NaN are false, so comparing scores by hand reported a
	// NaN as neither better nor worse than anything and skipped the DocID
	// tiebreak with it: a NaN document held rank 1 above one scoring 0.9, and the
	// answer depended on the order candidates arrived in.
	nan := math.NaN()
	orderings := [][]Candidate{
		{{1, nan}, {2, 0.9}, {3, 0.5}, {4, nan}, {5, 0.1}},
		{{2, 0.9}, {1, nan}, {5, 0.1}, {3, 0.5}, {4, nan}},
		{{4, nan}, {5, 0.1}, {3, 0.5}, {2, 0.9}, {1, nan}},
	}
	want := []DocID{2, 3, 5, 1, 4} // scores descending, then the NaNs by DocID
	for i, in := range orderings {
		got := TopK(in, len(want))
		if len(got) != len(want) {
			t.Fatalf("ordering %d: TopK = %+v, want %d results", i, got, len(want))
		}
		for j := range got {
			if got[j].Doc != want[j] {
				t.Fatalf("ordering %d: TopK = %+v, want docs %v", i, got, want)
			}
		}
	}
}

func TestAddRejectsNonFiniteVectors(t *testing.T) {
	// A non-finite component makes cosine scores NaN, and NaN sorts arbitrarily
	// in TopK, so fusion would report a NaN document above a genuine match.
	for _, tc := range []struct {
		name string
		vec  []float32
	}{
		{"NaN", []float32{1, float32(math.NaN())}},
		{"positive infinity", []float32{float32(math.Inf(1)), 0}},
		{"negative infinity", []float32{0, float32(math.Inf(-1))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ix := New()
			_, err := ix.Add(Document{Key: "a", Vector: tc.vec})
			if !errors.Is(err, ErrNonFiniteVector) {
				t.Fatalf("err = %v, want ErrNonFiniteVector", err)
			}
			if ix.Len() != 0 {
				t.Fatal("rejected document was stored anyway")
			}
		})
	}
}

func TestAddRejectsMixedVectorWidths(t *testing.T) {
	// A stale embedding is a write-time mistake, and the write path is the only
	// place it can be reported to whoever can fix it. Left to query time it is
	// not one bad document: scorer/vector errors on the first mismatch it scans,
	// so every vector query fails forever, and because Search aborts on the first
	// scorer error, text, graph and recency return nothing either.
	ix := New()
	mustAdd(t, ix, Document{Key: "a", Vector: []float32{1, 0, 0}})

	// No vector at all is not a mismatch, just no opinion for the vector scorer.
	mustAdd(t, ix, Document{Key: "textonly", Text: "no vector here"})

	_, err := ix.Add(Document{Key: "stale", Vector: []float32{1, 0}})
	if !errors.Is(err, ErrDimMismatch) {
		t.Fatalf("err = %v, want ErrDimMismatch", err)
	}
	if ix.Len() != 2 {
		t.Fatalf("Len() = %d, want 2 — the rejected document was stored anyway", ix.Len())
	}
	// The width survives the rejection, so the next correct Add still works.
	mustAdd(t, ix, Document{Key: "b", Vector: []float32{0, 1, 0}})
}

func TestAddCopiesSliceFields(t *testing.T) {
	// A caller reusing one scratch buffer across Add calls must not rewrite
	// documents that are already indexed.
	ix := New()
	vec := []float32{1, 0}
	links := []string{"b"}
	mustAdd(t, ix, Document{Key: "a", Vector: vec, Links: links})

	vec[0] = 9
	links[0] = "hijacked"

	d, ok := ix.Doc(0)
	if !ok {
		t.Fatal("Doc(0) missing")
	}
	if d.Vector[0] != 1 {
		t.Errorf("Vector[0] = %v, want 1 — the index aliases the caller's array", d.Vector[0])
	}
	if d.Links[0] != "b" {
		t.Errorf("Links[0] = %q, want \"b\" — the index aliases the caller's array", d.Links[0])
	}
}

func TestStatsIsConsistentWithLenAndAvgDocLen(t *testing.T) {
	ix := New()
	if docs, avg := ix.Stats(); docs != 0 || avg != 0 {
		t.Fatalf("Stats() on empty index = %d, %v; want 0, 0", docs, avg)
	}

	mustAdd(t, ix, Document{Key: "a", Text: "one two three four"})
	mustAdd(t, ix, Document{Key: "b", Text: "one two"})

	docs, avg := ix.Stats()
	if docs != 2 || avg != 3 {
		t.Fatalf("Stats() = %d, %v; want 2, 3", docs, avg)
	}
	if docs != ix.Len() || avg != ix.AvgDocLen() {
		t.Fatalf("Stats() = %d, %v disagrees with Len() = %d, AvgDocLen() = %v",
			docs, avg, ix.Len(), ix.AvgDocLen())
	}
}

func TestTheZeroValueIndexWorks(t *testing.T) {
	// Index is exported and its zero value was never documented as invalid, so
	// `var ix engine.Index` is a reasonable thing for a caller to write. Add used
	// to get past the duplicate check on a nil map and then panic assigning into
	// it, contradicting the no-panic promise on the sentinel errors above.
	var ix Index
	if got := ix.Len(); got != 0 {
		t.Fatalf("zero-value Len() = %d, want 0", got)
	}
	mustAdd(t, &ix, Document{Key: "a", Text: "one two"})

	if _, err := ix.Add(Document{Key: "a"}); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("duplicate Add err = %v, want ErrDuplicateKey", err)
	}
	if id, ok := ix.Resolve("a"); !ok || id != 0 {
		t.Fatalf("Resolve(%q) = %d, %v; want 0, true", "a", id, ok)
	}
	if got := ix.Lookup("one"); len(got) != 1 || got[0].Doc != 0 {
		t.Fatalf("Lookup(%q) = %+v, want one posting for doc 0", "one", got)
	}
}

func mustAdd(t *testing.T, ix *Index, d Document) DocID {
	t.Helper()
	id, err := ix.Add(d)
	if err != nil {
		t.Fatalf("Add(%q): %v", d.Key, err)
	}
	return id
}
