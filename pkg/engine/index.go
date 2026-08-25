// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
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

	// ErrNoSuchKey reports an Update whose Key no live document holds — never
	// added, or deleted since.
	//
	// Update refuses rather than inserting for the reason ErrDuplicateKey refuses
	// rather than replacing, which is the same rule read from the other side: a
	// mistyped key must not be able to silently become a second document, and it
	// must not be able to silently overwrite one either. A caller who means
	// "store this either way" writes the fallback themselves and can see it.
	ErrNoSuchKey = errors.New("engine: no document with this key")
)

// Posting is one document's occurrence count for one term.
//
// A count, and not a position list: nothing here records *where* in the document
// the term occurred. So a constraint that depends on position — an exact phrase,
// a proximity window — cannot be decided from postings at all, and the only
// route left is the document text itself, through Doc. That costs one record
// decode per document considered, which docs/FINDINGS.md milestone 5 section 3.2
// measured as the throughput wall, so the shape that survives is a scorer
// wrapping another scorer and filtering its candidates; sweeping every DocID
// pays that decode across the whole corpus on every query. A position index is a
// format change and is not planned (docs/FORMAT.md section 8).
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

	// wmu serializes the mutators against each other, and every place that takes
	// mu.Lock takes this one first: Add, Close, Merge and Commit, which is the
	// whole list — `grep -n 'ix\.mu\.Lock()' pkg/engine/*.go` enumerates it.
	//
	// It is not a second name for the same thing. Commit spends almost all of its
	// time encoding a segment out of state it only reads, so milestone 9 moved
	// that part under mu.RLock and left mu.Lock for the swap at the end. That
	// alone does not let reads through, because sync.RWMutex prefers writers: a
	// goroutine blocked in mu.Lock makes every RLock after it queue behind that
	// goroutine rather than joining the readers already inside. One concurrent
	// Add would therefore restore the full stall and merely move its trigger, and
	// the stall is what docs/FINDINGS.md milestone 5 §3.3 measured at 12.539
	// seconds. wmu is what keeps a mutator from reaching mu.Lock while a commit is
	// encoding, so no writer is ever queued for reads to pile up behind.
	//
	// The second thing it buys is that the pending segment cannot change while a
	// commit reads it. Encoding under a read lock would otherwise have to count
	// the documents captured, so that an Add arriving mid-encode is neither
	// written twice nor dropped, and that count is a partial hand-off of docs,
	// byKey, postings, docLen, totalLen and base. Excluding Add outright means
	// there is nothing to count, and the set written to the segment is exactly
	// the set that was pending when the commit was admitted.
	//
	// ponytail: the price is that Add blocks for a whole Commit, which is what it
	// already did, so it is a ceiling rather than a regression. Lifting it is the
	// capture counting above, and it is owed only once a caller needs to ingest
	// during a commit — milestone 9's clause is read latency.
	//
	// Lock order is wmu then mu, never the reverse, and nothing takes wmu inside
	// mu.
	wmu sync.Mutex

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
	//
	// dirID is that directory's identity, taken when it was opened. A path is
	// not one: it names whatever stands there now, so an index moved aside and
	// replaced under its old name leaves the string pointing at a stranger, and
	// a check that stats the string twice compares the stranger with itself and
	// agrees. Commit would then join this index's pending documents to a history
	// that is not its own. The identity does not follow a rename, which is the
	// property being bought.
	dir   string
	dirID fs.FileInfo

	// segs are the committed segments: mapped, immutable, sorted by base, and
	// covering [0, base) between them. Reads route here when the answer is not
	// pending, and nothing about that routing shows up in the six read methods'
	// signatures — which is the milestone 3 claim.
	segs []*segment

	// dead is the tombstone set, and it spans both halves of the index: an id
	// in it may name a pending document or one in a mapped segment, because a
	// caller deleting a document does not know or care which.
	//
	// It is consulted inside the unexported read helpers below rather than by
	// their callers, which is the milestone 11 claim in one sentence — the four
	// scorers ask the questions they always asked and none of them mentions
	// deletion. A filter that had to live in a scorer would mean the index's
	// account of which documents exist had leaked into the scorers'.
	dead deadSet
}

// The four methods below are the read path with the locking taken out. The
// exported versions wrap them; writeSegment calls them directly, because its
// caller already holds ix.mu for the whole encode — in whichever mode, RLock for
// a commit and Lock for a merge — and taking it again per document would deadlock
// the moment a writer queued behind it.
//
// Requires ix.mu in either mode.
//
// The tombstone check sits at the top of each of them rather than at the top of
// their exported wrappers, and that placement is what writeSegment relies on:
// the encoders reach the pending documents and a segment's records directly,
// never through here, so a deleted document is invisible to every reader and
// still written by every writer. It has to be — its record is what the id space
// is made of, and a segment that skipped it would renumber the documents behind
// it.

func (ix *Index) docAt(id DocID) (Document, bool) {
	// Before the routing, not inside either branch: a deleted document reads
	// exactly like an id that was never assigned, whichever half of the index
	// happens to hold it.
	if ix.dead.has(id) {
		return Document{}, false
	}
	if s := ix.segFor(id); s != nil {
		return s.doc(id)
	}
	if uint64(id) < uint64(ix.base) || uint64(id) >= uint64(ix.base)+uint64(len(ix.docs)) {
		return Document{}, false
	}
	return ix.docs[uint64(id)-uint64(ix.base)], true
}

func (ix *Index) vectorAt(id DocID) ([]float32, bool) {
	if ix.dead.has(id) {
		return nil, false
	}
	if s := ix.segFor(id); s != nil {
		return s.vector(id)
	}
	if uint64(id) < uint64(ix.base) || uint64(id) >= uint64(ix.base)+uint64(len(ix.docs)) {
		return nil, false
	}
	// A pending document is held as the caller passed it, so this aliases rather
	// than copies — the same contract docAt has, and Vector repeats the rule.
	v := ix.docs[uint64(id)-uint64(ix.base)].Vector
	return v, len(v) > 0
}

func (ix *Index) docLenAt(id DocID) int {
	// Zero, which is what an unassigned id already answers and what DocLen
	// documents BM25 must read as "no normalization". Delete reads the length
	// through here before it marks, which is why the mark comes second there.
	if ix.dead.has(id) {
		return 0
	}
	if s := ix.segFor(id); s != nil {
		return s.docLen(id)
	}
	if uint64(id) < uint64(ix.base) || uint64(id)-uint64(ix.base) >= uint64(len(ix.docLen)) {
		return 0
	}
	return ix.docLen[uint64(id)-uint64(ix.base)]
}

// lookupAt is lookupAllAt with the deleted documents taken out.
//
// The filter is a second pass rather than a check inside the walk below, and the
// reason is aliasing: lookupAllAt's fast paths hand back a segment's decoded
// list or the pending slice itself, so a filter that wrote through the result
// would edit index state a concurrent reader is holding. Copying on the first
// tombstone found — and not before — means a list holding none is returned
// exactly as it was, allocation included.
//
// An index with nothing deleted does not reach any of this. That is one branch
// per lookup against one per posting, and posting-per-query is what
// docs/FINDINGS.md milestone 8 measured a query's cost in.
func (ix *Index) lookupAt(term string) []Posting {
	pl := ix.lookupAllAt(term)
	if ix.dead.empty() {
		return pl
	}
	for i, p := range pl {
		if !ix.dead.has(p.Doc) {
			continue
		}
		// The first tombstone. Everything before it is live and is copied once,
		// with room for the rest of the list so the append below cannot grow.
		out := make([]Posting, i, len(pl))
		copy(out, pl[:i])
		for _, p := range pl[i+1:] {
			if !ix.dead.has(p.Doc) {
				out = append(out, p)
			}
		}
		if len(out) == 0 {
			// A term every holder of which has been deleted is a term no document
			// contains, and Lookup says that with nil. The empty slice this filter
			// would otherwise produce is the one shape a `== nil` caller reads as
			// the opposite of what happened.
			return nil
		}
		return out
	}
	return pl
}

func (ix *Index) lookupAllAt(term string) []Posting {
	pending := ix.postings[term]
	if len(ix.segs) == 0 {
		return pending
	}
	// Collected rather than merged as they arrive, because the common shape is one
	// segment claiming the term and nothing pending, and there the answer is what
	// that segment already handed back. Appending it into a fresh slice
	// materialised the list a second time — a term held by twenty thousand
	// documents twice over, per term, per query — and `make eval-data` builds
	// exactly one segment, so that shape is behind every figure in docs/PERF.md.
	var first []Posting
	var rest [][]Posting
	for _, s := range ix.segs {
		// Each segment that claims the term has to answer, and the terms index
		// is what says which do. Appending whatever came back let a damaged
		// posting block in one segment drop its documents out of every ranking
		// while the segments beside it made the result look whole — a shorter
		// list is not a shorter answer, it is a wrong one. Merge already refuses
		// on exactly this ground; this is the same rule where a query reads.
		if _, claimed := s.terms[term]; !claimed {
			continue
		}
		pl := s.lookup(term)
		if pl == nil {
			// Nil for the whole lookup, pending included: a partial answer with
			// the newest documents grafted on is still a partial answer, and
			// absence is what D-006 gives corruption on this path.
			return nil
		}
		if first == nil {
			first = pl
			continue
		}
		rest = append(rest, pl)
	}
	if first == nil {
		return pending
	}
	if len(rest) == 0 && len(pending) == 0 {
		return first
	}
	// Sized exactly, and copied rather than appended onto `first`: that slice is
	// the segment's own and appending into whatever spare capacity it happens to
	// carry would write into the list a concurrent reader is holding.
	n := len(first) + len(pending)
	for _, r := range rest {
		n += len(r)
	}
	out := make([]Posting, 0, n)
	out = append(out, first...)
	for _, r := range rest {
		out = append(out, r...)
	}
	return append(out, pending...)
}

// lookupInto is lookupAt writing into buf instead of allocating, and it is a second
// traversal of the same structure rather than a wrapper over the first: lookupAt's
// fast paths exist to avoid materialising a list twice, and there is nothing here to
// avoid — every posting is written once, into the caller's array.
//
// The corruption rule is the reason this can be a buffer at all rather than a
// callback. A segment that claims the term and fails to decode makes the whole lookup
// absent, pending included (D-006), and that verdict arrives after some of the term's
// postings have already been decoded. Holding them in buf means they can be discarded;
// an iterator would already have yielded them, and a partial posting list is not a
// shorter answer, it is a wrong one.
//
// Requires ix.mu.
func (ix *Index) lookupInto(term string, buf []Posting) []Posting {
	out := buf[:0]
	// Hoisted out of both loops. buf is the caller's, so unlike lookupAt there is
	// nothing here to copy away from — the filter is a skipped append — and what
	// this saves is the check itself on every posting of every term of every
	// query in an index that has never deleted anything.
	filter := !ix.dead.empty()
	for _, s := range ix.segs {
		// Same rule and same reason as lookupAt: the terms index says which segments
		// have to answer, and one that claims the term and stays silent is damage.
		if _, claimed := s.terms[term]; !claimed {
			continue
		}
		// The count scanPostings returns is what the decoder produced, tombstones
		// included, and it is compared against zero rather than against what
		// landed in out. A term held only by deleted documents must read as an
		// empty list, not as the damage a zero here means.
		if s.scanPostings(term, func(n int) { out = slices.Grow(out, n) },
			func(p Posting) {
				if filter && ix.dead.has(p.Doc) {
					return
				}
				out = append(out, p)
			}) == 0 {
			return nil
		}
	}
	// Pending last, which keeps the whole list ascending for free: segments are ordered
	// by base and the pending segment's base is past all of them.
	if !filter {
		return append(out, ix.postings[term]...)
	}
	for _, p := range ix.postings[term] {
		if !ix.dead.has(p.Doc) {
			out = append(out, p)
		}
	}
	return out
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

	// wmu before mu, the order every mutator uses, and here it is what keeps the
	// milestone 9 lock split working rather than merely tidy: an Add blocked in
	// mu.Lock would make every query behind it queue too, including the ones a
	// concurrent Commit is deliberately letting through. See Index.wmu.
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	ix.mu.Lock()
	defer ix.mu.Unlock()

	// A zero-value Index is usable, the same way a bytes.Buffer is. Every read
	// path already works on nil maps and slices, so the mutators are the only
	// place that needs them to exist; without this, `var ix engine.Index` would
	// pass the duplicate lookup below and then panic assigning into a nil map,
	// which contradicts what this package promises above about never panicking.
	ix.initMaps()

	// Committed segments are searched too. A key already on disk is as much a
	// duplicate as one added a moment ago, and after an Open the pending map is
	// empty — so without this an index reopened from disk would accept every
	// key it already holds and produce two documents claiming one Key, which
	// Resolve would then answer at random.
	//
	// A key whose every holder has been deleted is not a duplicate, which is
	// what makes Delete-then-Add an update a caller can spell without a second
	// method. resolveLive is where that judgement lives, so this reads the same
	// question Resolve answers rather than a second copy of it.
	if _, dup := ix.resolveLive(d.Key); dup {
		return 0, fmt.Errorf("add %q: %w", d.Key, ErrDuplicateKey)
	}

	// The id ceiling and the vector width are admit's, which appendPending asks
	// before it appends. They used to be spelled out here; the reasoning moved
	// with the code rather than being left behind as a second copy of it.
	return ix.appendPending(d, toks, freq, "add")
}

// initMaps makes a zero-value Index usable. Requires ix.mu.
func (ix *Index) initMaps() {
	if ix.byKey == nil {
		ix.byKey = make(map[string]DocID)
		ix.postings = make(map[string][]Posting)
	}
}

// appendPending is the tail Add and Update share: the ceiling, the vector width,
// and the append itself. Requires ix.wmu and ix.mu, and that the key is free —
// Add checks that against every live document, and Update frees it by marking
// the old record.
//
// what names the operation in the errors, so an Update that runs out of ids does
// not report itself as an Add. It is the whole of what the two callers differ by
// down here, which is the point of them sharing this at all: two appends would be
// two chances for the pending segment's six parallel structures to disagree.
func (ix *Index) appendPending(d Document, toks []string, freq map[string]int, what string) (DocID, error) {
	if err := ix.admit(d, what); err != nil {
		return 0, err
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

// admit is everything appendPending can refuse d for, asked on its own so a
// caller can find out before it does something it cannot take back. Requires
// ix.mu.
//
// Update is that caller: the committed path tombstones the old record before it
// appends, and a mark is the one thing this package cannot undo — an append
// refused after it would leave the document deleted and report the refusal, which
// is data loss dressed as an error. replacePending keeps the same rule on the
// other path by validating before it writes.
//
// The ceiling stands in front of the width for the reason adoptVecDim gives.
//
// DocID is uint32, so the conversion appendPending makes below this truncates
// silently past 2^32 documents: the id wraps to 0 and every posting, key and
// link written afterwards addresses document 0 instead. Doc and DocLen already
// compare their bounds in uint64 to avoid the mirror image of this; refusing the
// write is the same choice on the write side, and both callers return an error.
//
// Widened to uint64 before comparing, not after. len returns int, so an untyped
// MaxUint32 on the other side of the operator becomes an int, which does not fit
// on a 32-bit target: the package stops compiling entirely under GOARCH=386.
// Widened, the comparison is simply never true there, which is the right answer —
// an int that narrow cannot reach the limit.
//
// The comparison is >=, not >, so the ceiling is 2^32-1 documents rather than
// 2^32: one lower than DocID alone would allow, and the number FORMAT.md
// publishes. The doc count on disk is a uvarint the reader ranges against
// MaxUint32, so accepting one more here would build an index that commits and
// then cannot be reopened.
//
// Against maxDocCount, which is the narrower of DocID's width and this
// platform's int, and the number every decoder ranges a count against. On a
// 64-bit build the two are the same. On a 32-bit one they are not, and comparing
// against DocID's width alone let an index of maxInt documents take another: Len
// and Stats overflowed, and the Commit after it published a manifest
// readManifest refuses — a writer call that succeeds and leaves an index that
// cannot be reopened.
func (ix *Index) admit(d Document, what string) error {
	if uint64(ix.base)+uint64(len(ix.docs)) >= uint64(maxDocCount) {
		return fmt.Errorf("%s %q: index is full at %d documents", what, d.Key, maxDocCount)
	}
	return ix.adoptVecDim(d, what)
}

// adoptVecDim widens the corpus to d's vector, or refuses d for disagreeing with
// the width the corpus already has. Requires ix.mu.
//
// It is the one place a mutator changes index state before it can still fail,
// which is why the document ceiling is checked in front of it: a rejected write
// leaving vecDim set to the width of a document that was never stored would make
// Commit write a meta the docs file cannot back.
func (ix *Index) adoptVecDim(d Document, what string) error {
	if len(d.Vector) == 0 {
		return nil
	}
	if ix.vecDim == 0 {
		ix.vecDim = len(d.Vector)
		return nil
	}
	if len(d.Vector) != ix.vecDim {
		return fmt.Errorf("%s %q: vector has %d dims, corpus has %d: %w",
			what, d.Key, len(d.Vector), ix.vecDim, ErrDimMismatch)
	}
	return nil
}

// Update replaces the document holding d.Key and returns the DocID it now has.
// A Key no live document holds is ErrNoSuchKey — including one whose document was
// deleted, because an update that quietly inserted would make a typo in a key
// indistinguishable from a new document.
//
// The returned id is the same one only when the document had not been committed
// yet. A committed record lives in a mapped segment and segments are immutable,
// so there the old record becomes a tombstone and a new one is appended: the
// update spends a DocID, and the bytes of the old document stay on disk for as
// long as the index does. Delete says the rest about what is not reclaimed.
//
// Whether an id was spent is observable — `Len` grows, `Stats` does not — and a
// caller holding a DocID across an update must re-Resolve rather than assume.
//
// The replacement is total. Text, Vector, Links and Time are d's, and the fields
// d leaves zero are zero afterwards; this is not a merge of two documents.
func (ix *Index) Update(d Document) (DocID, error) {
	if d.Key == "" {
		return 0, ErrEmptyKey
	}
	// The same clone-then-check order Add gives, and for the reason stated there:
	// the bytes that were validated have to be the bytes that get stored.
	d.Vector = slices.Clone(d.Vector)
	d.Links = slices.Clone(d.Links)
	for i, c := range d.Vector {
		if f := float64(c); math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, fmt.Errorf("update %q: vector component %d is %v: %w", d.Key, i, c, ErrNonFiniteVector)
		}
	}
	toks := Tokenize(d.Text)
	freq := make(map[string]int, len(toks))
	for _, t := range toks {
		freq[t]++
	}

	// wmu before mu, the order every mutator uses. See Index.wmu.
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	ix.mu.Lock()
	defer ix.mu.Unlock()

	// No initMaps, unlike Add: the only index whose maps are nil is a zero-value
	// one, Open builds its own, and a zero-value index holds no document for this
	// to resolve — the refusal below is reached before anything could write to a
	// map. Reading a nil map is legal, so resolveLive needs nothing either.
	id, ok := ix.resolveLive(d.Key)
	if !ok {
		return 0, fmt.Errorf("update %q: %w", d.Key, ErrNoSuchKey)
	}

	// Pending: the record can be replaced where it stands, so no id is spent and
	// no tombstone is made. That is worth its own path rather than being folded
	// into delete-and-append, because a caller updating the same document
	// repeatedly between commits would otherwise burn an id and leave a dead
	// record for each one.
	if uint64(id) >= uint64(ix.base) {
		if err := ix.replacePending(id, d, toks, freq); err != nil {
			return 0, err
		}
		return id, nil
	}

	// Committed: tombstone the old record and append a new one. Everything the
	// append can refuse for is asked first, because the mark is not undoable —
	// a refused update has to leave the document it could not replace exactly as
	// it was, which is the rule replacePending keeps on the path above.
	if err := ix.admit(d, "update"); err != nil {
		return 0, err
	}
	// The length comes off the index before the mark, the order Delete uses and
	// for the same reason — docLenAt answers 0 for a tombstone.
	ix.dead.mark(id, ix.docLenAt(id))
	return ix.appendPending(d, toks, freq, "update")
}

// replacePending rewrites a pending document in place, keeping its DocID.
// Requires ix.wmu and ix.mu.
//
// ponytail: the replaced document is re-tokenized here, under the exclusive
// lock, because its old terms are what say which posting lists to edit and
// nothing stores them. Add moved that cost outside the lock and this cannot,
// since which document is being replaced is only known once the key resolves.
// The upgrade is the two-section pattern Commit uses — read the old text under
// ix.mu.RLock, release, tokenize, reacquire exclusively to apply, all with wmu
// held throughout so nothing can have moved. Owed when a caller updates
// documents large enough for the tokenization to show up as read latency.
func (ix *Index) replacePending(id DocID, d Document, toks []string, freq map[string]int) error {
	// Before anything is written, so a refused update leaves the document it
	// could not replace exactly as it was.
	if err := ix.adoptVecDim(d, "update"); err != nil {
		return err
	}
	i := uint64(id) - uint64(ix.base)
	old := ix.docs[i]

	// The terms the old text held and the new one does not. Its postings have to
	// go, or the document keeps answering a query for words it no longer
	// contains — and a stale posting is invisible to any check that reads the
	// record, because the record is right.
	//
	// Walked as tokens rather than reduced to a set first: dropPosting is a no-op
	// for an id it does not find, so the second occurrence of a dropped term costs
	// one binary search against the map insert and the hash a set would have cost
	// it anyway.
	for _, t := range Tokenize(old.Text) {
		if _, kept := freq[t]; !kept {
			ix.dropPosting(t, id)
		}
	}
	for t, f := range freq {
		ix.setPosting(t, id, f)
	}

	// The token total moves with the length it is the sum of. Commit writes the
	// record's own count from ix.docLen, and Scrub adds a segment's postings back
	// up and refuses a disagreement, so the two have to move together or the next
	// commit writes a segment that will not scrub.
	ix.totalLen += len(toks) - ix.docLen[i]
	ix.docLen[i] = len(toks)
	ix.docs[i] = d
	return nil
}

// setPosting records id's frequency for term, keeping the list ascending by
// DocID. Requires ix.mu.
//
// The insert is what an in-place update needs and an Add does not: Add's ids only
// increase, so it appends and the list stays sorted for free. A replaced document
// keeps an id in the middle of the pending segment, and a term it did not hold
// before has to land at that id's position rather than at the end. Every reader
// of a posting list — the block encoder's delta chain, Merge, the ascending order
// TopK's tiebreak rests on — takes that ordering as given.
//
// The new list is a copy and never the old one edited where it lies. Lookup's
// pending fast path hands ix.postings[term] straight to the caller, which reads
// it after the lock is released — so an in-place write reaches a list somebody is
// walking, and what lands there is not a stale posting but a wrong one: a
// frequency from another document, or the duplicated tail slices.Delete leaves.
// It is the same rule lookupAllAt states about appending into a segment's spare
// capacity, on the one path that can write inside a list rather than past it. Add
// needs none of this because appending only ever writes past the end.
//
// ponytail: a copy per term the update touches. Bounded by the replaced
// document's vocabulary and paid under a lock that is already re-tokenizing it;
// the way out is versioned lists, which is a great deal of machinery for a write
// path nothing has measured.
func (ix *Index) setPosting(term string, id DocID, freq int) {
	pl := ix.postings[term]
	at, found := slices.BinarySearchFunc(pl, id, func(p Posting, id DocID) int {
		return cmp.Compare(p.Doc, id)
	})
	if found {
		pl = slices.Clone(pl)
		pl[at].Freq = freq
		ix.postings[term] = pl
		return
	}
	ix.postings[term] = slices.Insert(slices.Clone(pl), at, Posting{Doc: id, Freq: freq})
}

// dropPosting removes id from term's list, and the term itself when that empties
// it. Requires ix.mu.
//
// The empty case is not tidiness. encodePostings writes an entry for every term
// the source names and the decoder refuses an entry with no blocks, so a term
// left behind with an empty list would produce a segment that commits and then
// cannot be read back.
func (ix *Index) dropPosting(term string, id DocID) {
	pl := ix.postings[term]
	at, found := slices.BinarySearchFunc(pl, id, func(p Posting, id DocID) int {
		return cmp.Compare(p.Doc, id)
	})
	if !found {
		return
	}
	if len(pl) == 1 {
		delete(ix.postings, term)
		return
	}
	// Copied first, for the reason setPosting gives: slices.Delete shifts the tail
	// down and zeroes what it vacates, both inside a list Lookup may have handed
	// out.
	ix.postings[term] = slices.Delete(slices.Clone(pl), at, at+1)
}

// Delete removes the document with the given Key and reports whether there was
// one. Deleting a key twice is not an error; the second call returns false.
//
// What it does not do is reclaim anything. The document's record, its key and
// its postings stay on disk and are copied forward by every Merge that passes
// over them, because a DocID is a position and closing the gap would renumber
// every document behind it — and ids are what TopK breaks ties on, what keeps
// posting lists ascending, and what lets Merge concatenate adjacent segments
// without moving a ranking. Deleting the whole corpus therefore frees no disk
// and returns no DocIDs to the 2^32 ceiling Add enforces. A full re-index is the
// only compaction weft has.
//
// The deletion is visible to readers immediately and is durable only after the
// next Commit, exactly as an Add is.
//
// ponytail: one key per call, and each call takes the writer lock. Deleting ten
// thousand documents is ten thousand lock cycles doing O(1) work apiece. A batch
// form is worth writing when a caller's delete throughput is measured rather
// than assumed.
func (ix *Index) Delete(key string) bool {
	// wmu before mu, the order every mutator uses. See Index.wmu.
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
	ix.mu.Lock()
	defer ix.mu.Unlock()

	id, ok := ix.resolveLive(key)
	if !ok {
		return false
	}
	// The length is read before the mark, and that order is load-bearing rather
	// than stylistic: docLenAt answers 0 for a tombstone, so reading it
	// afterwards would subtract nothing from the token total and leave every
	// surviving document measured against a corpus length that includes a
	// document nobody can read.
	n := ix.docLenAt(id)
	if !ix.dead.mark(id, n) {
		return false
	}
	// The pending key map only ever names live documents, which is what lets
	// resolveLive treat a hit there as final. A committed document has no entry
	// here and this is a no-op for it.
	delete(ix.byKey, key)
	return true
}

// Len is one past the highest DocID this index has assigned.
//
// It counted documents too until deletion existed, and those two stopped being
// one number the moment a tombstone could sit between them. This is the id
// bound, tombstones included; **Stats is the live population.** The split is
// deliberate rather than an oversight, and each half has a caller that needs
// exactly it:
//
//   - A scorer that walks the corpus writes `for i := range ix.Len()` and skips
//     what Doc refuses, which is how scorer/recency is written and how it has
//     to stay — narrowing this would silently stop that walk short of the
//     newest documents, which is a wrong ranking rather than a slow one.
//   - BM25 normalizes against the collection, and a tombstone left in that
//     count makes every IDF quietly wrong. Stats and AvgDocLen subtract it.
//
// The cost of the first is that a corpus with most of its documents deleted is
// still walked in full by a scorer shaped that way. Nothing here reclaims an id.
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
// An index that never touched disk holds no mappings and Close is free. Callers
// who only ever New and Add need not call it, which is why nothing in the read
// path checks whether it has happened.
//
// Open is not the only way to acquire mappings, and this is the sentence a caller
// has to read: Commit adopts the generation it just wrote, which maps it into the
// index Commit was called on. So New + Add + Commit holds mappings and does need
// Close, exactly as an opened index does.
func (ix *Index) Close() error {
	// wmu before mu, the order every mutator uses. See Index.wmu.
	ix.wmu.Lock()
	defer ix.wmu.Unlock()
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
	ix.base, ix.totalLen = 0, 0
	// The remembered directory and its identity go together, and for the same
	// reason vecDim goes below: a closed index is documented as usable again,
	// and state describing a corpus it no longer holds is what makes that false.
	// Keeping the identity left a reused index unable to commit anywhere — the
	// old directory failed the count check, Close having just zeroed the count,
	// and every other directory failed the destination check.
	ix.dir, ix.dirID = "", nil
	// vecDim goes with them. It is the width the corpus established, and keeping
	// it past a Close that dropped that corpus would have a closed index refuse
	// an Add for mismatching a vector it no longer holds.
	ix.vecDim = 0
	// And the tombstones, for the same reason: they name ids in a corpus this
	// index no longer holds. Kept, they would make a reused index hide documents
	// added after the Close, since ids restart at 0 and the old marks land on
	// them.
	ix.dead = deadSet{}
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

// Vector is Doc for a caller that wants only the vector, and it exists for one
// measured reason: a scorer that scans candidates and reads nothing else was
// paying for a copy of each document's text at each candidate it looked at.
//
// On the evaluation corpus the vector scorer looks at about thirty thousand
// candidates a query, and the copy is measured at one per candidate: scoring 64
// documents allocated 4,208,024 bytes against 13,312 for the same vectors with
// 4 MiB less text — one full copy of text nothing read. Doc cannot know which
// fields its caller will read, so the choice belongs to the caller and this is
// how one says so.
//
// What this is *not* is the 206.6 MiB of
// [docs/FINDINGS.md](../../docs/FINDINGS.md) milestone 8 §7. That rung ran the
// `text` arm, where no scorer calls Doc at all — §9 there corrects the
// attribution. The saving here is real and is not yet measured on the corpus,
// because the arm that would show it has never had a published memory figure.
//
// Everything Doc guarantees holds here. A committed record is still checked
// against its own seeded checksum, so damage and a wrong id both answer false
// rather than handing back a plausible vector. The slice **must not be
// modified**, for the reason Document gives: a committed document's vector is a
// fresh decode and a pending one is the slice the caller passed to Add. Callers
// cannot tell the two apart, so the stricter rule is the one that holds.
//
// A document with no vector answers false, which is also the answer for an id
// that was never assigned. A scorer skips both.
func (ix *Index) Vector(id DocID) ([]float32, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.vectorAt(id)
}

// Resolve maps a caller-supplied Key to its DocID. The bool is false for a Key
// that was never added — which is exactly how dangling Links are detected.
//
// It is also the join a scorer written outside this module needs. Document is
// closed, so a signal whose data is not one of its fields keeps that data in a
// table of the caller's own keyed by Key, and Resolve turns each Key into the
// DocID a Candidate carries. See Document for the whole pattern and its one
// cost, and ExampleScorer for it in a compiling program.
func (ix *Index) Resolve(key string) (DocID, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.resolveLive(key)
}

// resolveLive is Resolve without the lock, and it is the one join Add, Delete
// and Update all ask their question through. Requires ix.mu in either mode.
//
// One live document per key, still. What changed is that a key may now be
// carried by more than one *record*: deleting a document and adding its key
// again leaves the old record where it was, and Merge copies it forward
// unchanged, so a segment can answer for a key whose document is a tombstone.
// The loop therefore keeps going where it used to stop at the first segment that
// answered — a dead answer is not an answer.
//
// byKey needs no such care. Delete drops the key it removes, so the pending map
// only ever names live documents and a hit there is final.
func (ix *Index) resolveLive(key string) (DocID, bool) {
	if id, ok := ix.byKey[key]; ok {
		return id, true
	}
	for _, s := range ix.segs {
		if id, ok := s.resolve(key); ok && !ix.dead.has(id) {
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

// LookupInto is Lookup writing into buf, which it truncates and grows as needed and
// returns. The result is the caller's to keep and to modify — that is the one contract
// difference from Lookup, which may hand back index state.
//
// It exists because a scorer walks a term's postings and is finished with them before it
// looks up the next term, while Lookup hands it a fresh slice every time. On the
// evaluation corpus that is 53.2% of what a query allocates: a term's whole posting list,
// per term, per query. One buffer reused across a query's terms holds the longest list
// instead of the sum of them, and the sum is what a peak memory figure is made of
// (docs/FINDINGS.md milestone 8).
//
// Two things it does not change, both load-bearing:
//
//   - **What the answer is.** Every posting, in the same ascending order Lookup returns,
//     committed segments before pending. Absence for a term no document holds, and
//     absence for the whole lookup when a segment that claims the term cannot decode it —
//     including the postings already written into buf, which are discarded. An iterator
//     could not promise that second one, which is why this is a buffer.
//   - **When the lock is held.** One read lock for the whole walk, released before
//     returning, exactly as Lookup. Nothing of the caller's runs inside it, so a caller
//     is still free to call DocLen per posting without the re-entrant RLock deadlock the
//     unexported read path exists to avoid.
//
// A nil buf is fine and is the same as calling Lookup once. Passing a buffer still being
// read from is not: this overwrites it.
func (ix *Index) LookupInto(term string, buf []Posting) []Posting {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.lookupInto(term, buf)
}

// Nearest returns the DocIDs worth scoring exactly for v, at least k of them
// when the index holds that many vectors. It computes no score: the metric
// belongs to the caller.
//
// That division is the milestone's design decision and D-008 records it. The
// index knows the geometry — which documents are close enough to be worth
// looking at — because the partition is a section of the segment format and
// lives where the format does. How close each one actually is stays with
// whoever defines "close": scorer/vector computes cosine, and every rule it
// holds about zero norms, non-finite components and mixed widths stays there
// untouched. Returning scores here would have moved half of a scorer into the
// engine to save it a loop.
//
// Three things the result is, and one it is not:
//
//   - Ascending and free of repeats. The docs file is in DocID order, so a
//     caller decoding these records walks the mapping forwards.
//   - At least k when there are k vectors and nothing has been deleted. A
//     segment widens its own probe until it has them, which is why no caller
//     ever has to know what nprobe is — but the widening counts candidates, not
//     live ones, and the tombstones come off afterwards. So a corpus with
//     deletions can answer with fewer than k while holding more than k live
//     vectors, which is recall lost rather than an error. D-019 prices it.
//   - A superset, not an answer. Documents with no vector, with a zero vector,
//     or in a segment with no partition are all in here. The caller is expected
//     to skip what it cannot score, which scorer/vector already did.
//
// It is not a promise that the exact top k are among them. This is an
// approximate index and its recall is measured rather than asserted —
// `weft-eval recall` against a brute-force scan is what publishes the number.
// A segment that carries no partition answers with every id it holds, so the
// answer there is exact by construction: format version 2 segments, segments
// below ivfMinDocs, and the pending in-memory segment.
//
// ponytail: the whole walk runs under one read lock and cannot be cancelled,
// because Query carries no context here and Nearest is called from inside a
// scorer that polls its own. Two things are inside that window and the second is
// the larger: the centroid scan, nlist × dim multiply-accumulates, 3.2e5 on the
// milestone 4 corpus; and the decode of up to nprobe inverted lists per segment,
// which is a uvarint and a bounds check per candidate — 30,549 of them at the
// same operating point, and a corpus-sized slice on the paths with no partition
// to narrow with. Against the 1.14e8 multiply-accumulates the scan it replaces
// used to run between polls, both are small; neither is zero. Give it a context
// when a profile shows a query waiting on it, which would be a signature change
// and therefore a visible one.
func (ix *Index) Nearest(v []float32, k int) []DocID {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	var out []DocID
	for _, s := range ix.segs {
		// Filtered as each segment answers rather than once at the end, so a
		// segment whose whole inverted list is tombstones costs one pass and not
		// a second one over ids nobody will score.
		out = appendLive(out, &ix.dead, s.nearest(v, k))
	}
	// The pending segment last, which keeps the whole list ascending for free:
	// segments are ordered by base and pending starts past all of them. Same
	// arrangement as lookupAt, for the same reason.
	//
	// All of it, unpartitioned. Nothing has been committed for these documents
	// yet, so there is no inverted list to consult — and a query that skipped
	// them would make a vector search silently stale by one commit's worth of
	// ingest. What bounds the cost is that a commit is what empties this.
	for i := range ix.docs {
		id := DocID(uint64(ix.base) + uint64(i))
		if !ix.dead.has(id) {
			out = append(out, id)
		}
	}
	return out
}

// appendLive appends the ids that are not tombstones.
//
// The whole-slice append is kept for an index with nothing deleted, which is
// both the common case and the one every published figure was measured on.
func appendLive(out []DocID, dead *deadSet, ids []DocID) []DocID {
	if dead.empty() {
		return append(out, ids...)
	}
	for _, id := range ids {
		if !dead.has(id) {
			out = append(out, id)
		}
	}
	return out
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
// Deleted documents are not counted, and Len does not agree with this number
// once anything has been deleted — see Len for which of the two each caller
// wants.
func (ix *Index) Stats() (docs int, avgDocLen float64) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return int(ix.base) + len(ix.docs) - ix.dead.n, ix.avgDocLen()
}

// avgDocLen requires ix.mu to be held. Collection-wide, so the segments'
// statistics are summed with the pending segment's — BM25 normalizes against
// the whole corpus, and an average over only the documents added since the last
// commit would rescale every score the moment a commit happened.
func (ix *Index) avgDocLen() float64 {
	// The sum is wider than its terms. Each segment's own total is ranged
	// against maxInt where it is decoded, which is all a segment can be asked
	// for — it knows nothing about the segments beside it. Incremental commit
	// makes the collection-wide sum a different quantity: on a 32-bit build a
	// handful of segments of a few hundred million tokens each overflow an int,
	// and a wrapped total is a negative average length BM25 divides every score
	// by. The document count needs no such care — readManifest already refuses a
	// segment list running past maxDocCount.
	n, total := len(ix.docs), uint64(ix.totalLen)
	for _, s := range ix.segs {
		n += s.count
		total += uint64(s.totalLen)
	}
	// The tombstones come off both halves of the ratio, and off both or neither:
	// a deleted document that left its tokens in the total would shorten every
	// surviving document relative to the average and rescale every BM25 score.
	//
	// The count cannot pass what it is taken from — Delete marks one live id at a
	// time and parseDead refuses a set larger than the corpus. The token total is
	// not so cheaply proved, and the clamp is there rather than an assertion of the
	// same shape: across a reopen the two numbers come from different files, the
	// sum out of each segment's meta and the tombstones' share out of docoff, and
	// nothing on the Open path compares those two. A damaged meta would otherwise
	// wrap this subtraction to about 1.8e19 and divide every BM25 score by it,
	// which is the plausible wrong answer this package refuses to produce. Scrub
	// is what names the damage.
	n -= ix.dead.n
	total -= min(total, ix.dead.tokens)
	if n == 0 {
		return 0
	}
	return float64(total) / float64(n)
}
