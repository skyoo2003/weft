package engine

import (
	"errors"
	"math"
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
