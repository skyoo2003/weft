// SPDX-License-Identifier: Apache-2.0

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

	// ErrDimMismatch rejects a vector whose width differs from the width the
	// corpus already established. Mixed widths mean mixed embedding models, and
	// the write path is the only place that can be reported to the party able to
	// fix it: caught here it is one rejected Add, caught at query time it is
	// every vector query failing forever — and because Search aborts on the
	// first scorer error, taking every other scorer's results down with it.
	//
	// A document with no vector is not a mismatch; it just has no opinion for
	// the vector scorer to read.
	ErrDimMismatch = errors.New("engine: vector width differs from the corpus")
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

	// The pending segment: everything added since the last Commit, held the way
	// milestone 2 held the whole index. Its DocIDs start at base.
	docs  []Document       // indexed by DocID-base
	byKey map[string]DocID // Document.Key -> DocID
	base  DocID            // first DocID the pending segment owns

	postings map[string][]Posting // term -> postings, ascending DocID
	docLen   []int                // token count per DocID-base
	totalLen int                  // pending only; segments carry their own

	vecDim int // width of the first non-empty vector added; 0 until one is

	// dir is the directory the committed segments live in, set by Open and by a
	// successful Commit. Merge needs it: it publishes a new manifest, and an
	// index that was never written anywhere has nothing to merge.
	dir string

	// segs are the committed segments: mapped, immutable, sorted by base, and
	// covering [0, base) between them. Reads route here when the answer is not
	// pending, and nothing about that routing shows up in the six read methods'
	// signatures — which is the milestone 3 claim.
	segs []*segment
}

// The four methods below are the read path with the locking taken out. The
// exported versions wrap them; writeSegment calls them directly, because it
// already holds the read lock for the whole encode and taking it again per
// document would deadlock the moment a writer queued behind it.
//
// Requires ix.mu.

func (ix *Index) docAt(id DocID) (Document, bool) {
	if s := ix.segFor(id); s != nil {
		return s.doc(id)
	}
	if uint64(id) < uint64(ix.base) || uint64(id) >= uint64(ix.base)+uint64(len(ix.docs)) {
		return Document{}, false
	}
	return ix.docs[uint64(id)-uint64(ix.base)], true
}

func (ix *Index) docLenAt(id DocID) int {
	if s := ix.segFor(id); s != nil {
		return s.docLen(id)
	}
	if uint64(id) < uint64(ix.base) || uint64(id)-uint64(ix.base) >= uint64(len(ix.docLen)) {
		return 0
	}
	return ix.docLen[uint64(id)-uint64(ix.base)]
}

func (ix *Index) lookupAt(term string) []Posting {
	pending := ix.postings[term]
	if len(ix.segs) == 0 {
		return pending
	}
	var out []Posting
	for _, s := range ix.segs {
		out = append(out, s.lookup(term)...)
	}
	if out == nil {
		return pending
	}
	return append(out, pending...)
}

// segFor returns the committed segment holding id, or nil. Requires ix.mu.
//
// A linear walk, not a binary search. The segment count is bounded by the merge
// policy — eight — so a search over it would be slower than the scan and would
// need its own test for an invariant (segments sorted by base) that nothing
// else depends on. Milestone 5's load test is what would say otherwise.
func (ix *Index) segFor(id DocID) *segment {
	for _, s := range ix.segs {
		if s.holds(id) {
			return s
		}
	}
	return nil
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
// dependency story. Morphological analysis is out of scope, and CJK runs
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

	// Store copies of the slice-backed fields. Appending d would copy only the
	// slice headers, leaving the index pointing at the caller's arrays: reusing
	// a scratch buffer across Add calls would then silently rewrite documents
	// already indexed, and mutating one concurrently would race past ix.mu.
	//
	// The clone comes before the checks below, not after, and the checks read
	// the clone. Validating the caller's array and then copying it is two reads
	// of memory this package does not own, so the bytes that were checked are
	// not the bytes that get stored: a caller mutating its buffer in between
	// lands a NaN in the index, which is exactly what ErrNonFiniteVector
	// promises cannot happen — and scorer/vector builds on that promise by not
	// re-checking document vectors.
	d.Vector = slices.Clone(d.Vector)
	d.Links = slices.Clone(d.Links)

	for i, c := range d.Vector {
		if f := float64(c); math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, fmt.Errorf("add %q: vector component %d is %v: %w", d.Key, i, c, ErrNonFiniteVector)
		}
	}

	// Tokenizing outside the lock. It is the dominant cost of Add — a 1 MB
	// document spends ~92% of its Add here — and it reads only d.Text, a local
	// copy of the caller's value, so holding the exclusive lock across it just
	// stalls every reader for the duration. A duplicate key wastes the work,
	// which is the error path.
	toks := Tokenize(d.Text)
	freq := make(map[string]int, len(toks))
	for _, t := range toks {
		freq[t]++
	}

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

	// Committed segments are searched too. A key already on disk is as much a
	// duplicate as one added a moment ago, and after an Open the pending map is
	// empty — so without this an index reopened from disk would accept every
	// key it already holds and produce two documents claiming one Key, which
	// Resolve would then answer at random.
	if _, dup := ix.byKey[d.Key]; dup {
		return 0, fmt.Errorf("add %q: %w", d.Key, ErrDuplicateKey)
	}
	for _, s := range ix.segs {
		if _, ok := s.resolve(d.Key); ok {
			return 0, fmt.Errorf("add %q: %w", d.Key, ErrDuplicateKey)
		}
	}

	// DocID is uint32, so this conversion truncates silently past 2^32
	// documents: the id wraps to 0 and every posting, key and link written
	// afterwards addresses document 0 instead. Doc and DocLen already compare
	// their bounds in uint64 to avoid the mirror image of this; refusing the Add
	// is the same choice on the write side, and Add already returns an error.
	//
	// Widened to uint64 before comparing, not after. len returns int, so an
	// untyped MaxUint32 on the other side of the operator becomes an int, which
	// does not fit on a 32-bit target: the package stops compiling entirely
	// under GOARCH=386. Widened, the comparison is simply never true there,
	// which is the right answer — an int that narrow cannot reach the limit.
	//
	// The comparison is >=, not >, so the ceiling is 2^32-1 documents rather
	// than 2^32: one lower than DocID alone would allow, and the number
	// FORMAT.md publishes. The doc count on disk is a uvarint the reader ranges
	// against MaxUint32, so accepting one more here would build an index that
	// commits and then cannot be reopened.
	//
	// Checked before the vector width below, not after: that branch is the one
	// place Add mutates the index before it can still fail, and a rejected Add
	// leaving ix.vecDim set to the width of a document that was never stored
	// would make Commit write a meta the docs file cannot back — a segment that
	// commits and then refuses to reopen.
	if uint64(ix.base)+uint64(len(ix.docs)) >= math.MaxUint32 {
		return 0, fmt.Errorf("add %q: index is full at %d documents", d.Key, uint64(math.MaxUint32))
	}

	if len(d.Vector) > 0 {
		if ix.vecDim == 0 {
			ix.vecDim = len(d.Vector)
		} else if len(d.Vector) != ix.vecDim {
			return 0, fmt.Errorf("add %q: vector has %d dims, corpus has %d: %w",
				d.Key, len(d.Vector), ix.vecDim, ErrDimMismatch)
		}
	}

	id := DocID(uint64(ix.base) + uint64(len(ix.docs)))
	ix.docs = append(ix.docs, d)
	ix.byKey[d.Key] = id

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
	return int(ix.base) + len(ix.docs)
}

// Close releases the committed segments' mappings.
//
// Reading through an unmapped region is a segmentation fault rather than a Go
// panic, and this package promises never to panic — so Close drops the segments
// under the write lock before unmapping them. A read that arrives afterwards
// gets what it would get for an id that was never assigned: the index becomes
// empty rather than dangerous. Calling it twice is a no-op.
//
// An index that was never opened from disk holds no mappings and Close is free.
// Callers who only ever New and Add need not call it, which is why nothing in
// the read path checks whether it has happened.
func (ix *Index) Close() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var first error
	for _, s := range ix.segs {
		if err := s.close(); err != nil && first == nil {
			first = err
		}
	}
	ix.segs = nil
	// The pending segment goes too. Leaving it would make Close mean "forget
	// what was committed and keep what was not", which is a stranger contract
	// than "this index is finished" and would have Len report a corpus whose
	// front half is gone.
	ix.docs, ix.byKey, ix.postings, ix.docLen = nil, nil, nil, nil
	ix.base, ix.totalLen, ix.dir = 0, 0, ""
	// vecDim goes with them. It is the width the corpus established, and keeping
	// it past a Close that dropped that corpus would have a closed index refuse
	// an Add for mismatching a vector it no longer holds.
	ix.vecDim = 0
	return first
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
// A document that lives in a committed segment is decoded on the way out, so
// its Vector and Links are fresh allocations rather than aliases and the "must
// not be modified" rule costs the caller nothing there. A pending document
// still aliases. Callers cannot tell the two apart and must obey the stricter
// rule, which is the one written above.
func (ix *Index) Doc(id DocID) (Document, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.docAt(id)
}

// Resolve maps a caller-supplied Key to its DocID. The bool is false for a Key
// that was never added — which is exactly how dangling Links are detected.
func (ix *Index) Resolve(key string) (DocID, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if id, ok := ix.byKey[key]; ok {
		return id, ok
	}
	// Keys are unique across the whole index — Add refuses a duplicate against
	// the segments too — so the first segment that answers is the only one that
	// can.
	for _, s := range ix.segs {
		if id, ok := s.resolve(key); ok {
			return id, true
		}
	}
	return 0, false
}

// Lookup returns the postings for term, ascending by DocID, or nil if no
// document contains it. The result aliases index state and must not be
// modified.
// Postings from committed segments come first and pending last, which keeps the
// whole list ascending for free: segments are ordered by base and the pending
// segment's base is past all of them.
//
// The result aliases index state only when it came from the pending segment; a
// segment's postings are decoded per call. Callers cannot tell which, so the
// stricter rule stands.
func (ix *Index) Lookup(term string) []Posting {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.lookupAt(term)
}

// DocLen is the token count of a document, or 0 for an unknown id. The bound is
// compared in uint64 for the reason given on Doc.
func (ix *Index) DocLen(id DocID) int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.docLenAt(id)
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
	return int(ix.base) + len(ix.docs), ix.avgDocLen()
}

// avgDocLen requires ix.mu to be held. Collection-wide, so the segments'
// statistics are summed with the pending segment's — BM25 normalizes against
// the whole corpus, and an average over only the documents added since the last
// commit would rescale every score the moment a commit happened.
func (ix *Index) avgDocLen() float64 {
	n, total := len(ix.docs), ix.totalLen
	for _, s := range ix.segs {
		n += s.count
		total += s.totalLen
	}
	if n == 0 {
		return 0
	}
	return float64(total) / float64(n)
}
