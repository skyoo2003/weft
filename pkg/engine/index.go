package engine

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"unicode"
)

// Sentinel errors from Add. Library code here never panics.
var (
	ErrEmptyKey     = errors.New("engine: document key is empty")
	ErrDuplicateKey = errors.New("engine: duplicate document key")

	// ErrNonFiniteVector rejects NaN and infinite vector components at the
	// point they enter the index. A non-finite component makes a cosine score
	// NaN, and NaN breaks the ordering TopK relies on: a NaN-scored document
	// can sort above a document scoring 0.9, and fusion then turns that into a
	// plausible-looking result. Failing here means it cannot happen at all.
	ErrNonFiniteVector = errors.New("engine: vector has a non-finite component")
)

// Posting is one document's occurrence count for one term.
type Posting struct {
	Doc  DocID
	Freq int
}

// Index is the single shared store. Add is the only writer; scorers only ever
// read, and none of them keeps a private copy of the corpus. That is the
// property milestone 1 is testing — a scorer that needed its own store would
// mean the index is not actually scorer-neutral.
//
// The zero value is an empty index ready to use; New is the same thing with a
// pointer already in hand. Do not copy an Index after first use — it holds a
// mutex.
type Index struct {
	// ponytail: one index-wide RWMutex. Shard or copy-on-write only if write
	// throughput ever shows up as a problem; milestone 1 has one writer.
	mu sync.RWMutex

	docs  []Document       // indexed by DocID
	byKey map[string]DocID // Document.Key -> DocID

	postings map[string][]Posting // term -> postings, ascending DocID
	docLen   []int                // token count per DocID, for BM25 normalization
	totalLen int
}

// New returns an empty index.
func New() *Index {
	return &Index{
		byKey:    make(map[string]DocID),
		postings: make(map[string][]Posting),
	}
}

// Tokenize lowercases and splits on everything that is not a letter or digit.
//
// It lives in engine rather than in scorer/text because Add has to tokenize to
// build the postings, and engine importing scorer/text would wreck the whole
// dependency story. Morphological analysis is out of scope (PRD), and CJK runs
// stay glued into one token here — a known wrong answer that milestone 1 does
// not need to be right about.
func Tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// Add stores d and returns its assigned DocID.
//
// Links are not resolved here: a document may reference a Key that has not been
// added yet, or never will be. Resolution happens at traversal time.
func (ix *Index) Add(d Document) (DocID, error) {
	if d.Key == "" {
		return 0, ErrEmptyKey
	}
	for i, c := range d.Vector {
		if f := float64(c); math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, fmt.Errorf("add %q: vector component %d is %v: %w", d.Key, i, c, ErrNonFiniteVector)
		}
	}

	// Store copies of the slice-backed fields. Appending d would copy only the
	// slice headers, leaving the index pointing at the caller's arrays: reusing
	// a scratch buffer across Add calls would then silently rewrite documents
	// already indexed, and mutating one concurrently would race past ix.mu.
	d.Vector = slices.Clone(d.Vector)
	d.Links = slices.Clone(d.Links)

	ix.mu.Lock()
	defer ix.mu.Unlock()

	// A zero-value Index is usable, the same way a bytes.Buffer is. Every read
	// path already works on nil maps and slices, so Add is the only place that
	// needs them to exist; without this, `var ix engine.Index` would pass the
	// duplicate lookup below and then panic assigning into a nil map, which
	// contradicts what this package promises above about never panicking.
	if ix.byKey == nil {
		ix.byKey = make(map[string]DocID)
		ix.postings = make(map[string][]Posting)
	}

	if _, dup := ix.byKey[d.Key]; dup {
		return 0, fmt.Errorf("add %q: %w", d.Key, ErrDuplicateKey)
	}

	id := DocID(len(ix.docs))
	ix.docs = append(ix.docs, d)
	ix.byKey[d.Key] = id

	toks := Tokenize(d.Text)
	freq := make(map[string]int, len(toks))
	for _, t := range toks {
		freq[t]++
	}
	// IDs only ever increase, so appending keeps every posting list sorted by
	// DocID for free.
	for t, f := range freq {
		ix.postings[t] = append(ix.postings[t], Posting{Doc: id, Freq: f})
	}

	ix.docLen = append(ix.docLen, len(toks))
	ix.totalLen += len(toks)

	return id, nil
}

// Len is the number of documents, which is also one past the highest DocID.
func (ix *Index) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.docs)
}

// Doc returns the document with the given id. The bool is false for an id that
// was never assigned.
//
// The returned Vector and Links alias index state and must not be modified,
// the same contract as Lookup. Copying them here would allocate once per
// document per query in the scorers that scan the whole corpus.
//
// The bound is compared in uint64, not int. DocID is uint32, so on a 32-bit
// build int(id) wraps negative for an id at or above 1<<31: the guard would pass
// and the slice access would panic, exactly where this is documented to return
// false instead.
func (ix *Index) Doc(id DocID) (Document, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if uint64(id) >= uint64(len(ix.docs)) {
		return Document{}, false
	}
	return ix.docs[id], true
}

// Resolve maps a caller-supplied Key to its DocID. The bool is false for a Key
// that was never added — which is exactly how dangling Links are detected.
func (ix *Index) Resolve(key string) (DocID, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	id, ok := ix.byKey[key]
	return id, ok
}

// Lookup returns the postings for term, ascending by DocID, or nil if no
// document contains it. The result aliases index state and must not be
// modified.
func (ix *Index) Lookup(term string) []Posting {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.postings[term]
}

// DocLen is the token count of a document, or 0 for an unknown id. The bound is
// compared in uint64 for the reason given on Doc.
func (ix *Index) DocLen(id DocID) int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if uint64(id) >= uint64(len(ix.docLen)) {
		return 0
	}
	return ix.docLen[id]
}

// AvgDocLen is the mean token count across the corpus, and 0 for an empty
// corpus or a corpus of empty documents. Callers doing BM25 length
// normalization must treat 0 as "no normalization" rather than dividing by it.
func (ix *Index) AvgDocLen() float64 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.avgDocLen()
}

// Stats returns the corpus size and mean token count as one value, read under a
// single lock so the two cannot come from different moments.
//
// BM25 needs both, and reading them through separate calls lets a concurrent
// Add land in between. Postings fetched afterwards can still be newer than the
// returned count, so a scorer must also tolerate seeing more postings for a
// term than there are documents — see the clamp in scorer/text. Making the
// whole read a true snapshot is milestone 2 work (docs/FINDINGS.md section 4.4).
func (ix *Index) Stats() (docs int, avgDocLen float64) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.docs), ix.avgDocLen()
}

// avgDocLen requires ix.mu to be held.
func (ix *Index) avgDocLen() float64 {
	if len(ix.docs) == 0 {
		return 0
	}
	return float64(ix.totalLen) / float64(len(ix.docs))
}
