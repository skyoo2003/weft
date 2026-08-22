// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"maps"
	"os"
	"slices"
)

// maxSegments is how many segments an index carries before Merge collapses the
// oldest ones into one.
//
// Every commit adds a segment and every point query consults all of them, so
// without a ceiling an index committed often enough answers a lookup by walking
// a list that grows without limit.
//
// ponytail: eight, chosen for being a small number rather than for being
// measured. What it trades against is write amplification — a document written
// on the first commit is copied again every time a merge sweeps past it — and
// nothing here counts those copies. Milestone 5's load test is the instrument
// that would justify a size-tiered policy instead; picking one now would be
// tuning against a guess.
const maxSegments = 8

// segSource is what the segment encoders read from.
//
// Two things produce a segment: a commit, from the documents added since the
// last one, and a merge, from segments already on disk. They differ in where the
// bytes come from and in nothing else, so they share one encoder rather than
// each carrying a copy of the block layout and the D-001 metadata. Two
// implementations of that would be two chances to disagree about what the format
// says, and only one of them would be covered by the corruption matrix.
//
// Ids handed to and returned by a source are segment-local, counting from zero:
// a segment's bytes do not depend on where the manifest puts it.
type segSource interface {
	count() int
	totals() (totalLen, vecDim int)
	doc(local int) Document
	docLen(local int) int
	termList() []string // sorted
	// postings hands term's postings to yield, ascending by segment-local
	// DocID, and reports how many it handed over.
	//
	// Streamed rather than returned, because a term present in most of the
	// corpus has a posting list the size of the corpus — and a merge reads
	// segments that were mapped rather than loaded precisely because they do
	// not fit. It is called twice per term: once to count, since the block
	// count stands in front of the blocks on disk, and once to emit.
	postings(term string, yield func(Posting)) int
}

// pendingSource is the in-memory half of an Index: everything added since the
// last commit. Requires ix.mu.
type pendingSource struct {
	ix *Index
}

func (p *pendingSource) count() int { return len(p.ix.docs) }
func (p *pendingSource) totals() (totalLen, vecDim int) {
	// The width this segment holds, not the width the corpus established.
	//
	// ix.vecDim outlives a commit on purpose — it is what Add enforces so one
	// corpus cannot end up holding two embedding spaces — but adopt clears
	// everything else a generation owns, and a batch that carried no vectors
	// writes documents that decode with width zero. meta claiming the corpus
	// width then makes Scrub reject a segment weft itself just wrote, which is
	// what routine incremental ingestion looks like when vectors arrive in some
	// batches and not others. Add allows exactly one non-zero width, so the
	// first vector found is this segment's.
	for _, d := range p.ix.docs {
		if len(d.Vector) != 0 {
			vecDim = len(d.Vector)
			break
		}
	}
	return p.ix.totalLen, vecDim
}
func (p *pendingSource) doc(local int) Document { return p.ix.docs[local] }
func (p *pendingSource) docLen(local int) int   { return p.ix.docLen[local] }
func (p *pendingSource) termList() []string     { return slices.Sorted(maps.Keys(p.ix.postings)) }

// postings translates as it yields. The pending postings carry index-wide ids
// and a segment records local ones, so every term needs its ids shifted — which
// used to mean a copy per term, 9 MB back on a commit measured down to 588 KB,
// and then one reused buffer to take that 9 MB off again. Yielding the shifted
// posting needs neither.
func (p *pendingSource) postings(t string, yield func(Posting)) int {
	pl := p.ix.postings[t]
	for _, e := range pl {
		yield(Posting{Doc: e.Doc - p.ix.base, Freq: e.Freq})
	}
	return len(pl)
}

// mergedSource concatenates a run of adjacent segments.
//
// Adjacent is the whole trick. Segments that sit side by side in the DocID space
// concatenate: every document keeps the id it had, so nothing is renumbered and
// no ranking can move because of a merge. A non-adjacent pair would leave a hole
// that the segments between them would have to be renumbered to close — and
// TopK breaks ties on DocID, where milestone 4 measured 241 reported slots
// decided by that tiebreak alone.
type mergedSource struct {
	segs []*segment
	base DocID // segs[0].base, the id the merged segment starts at

	// err holds the first thing this source could not read.
	//
	// A mapped segment answers a damaged record with absence, because Doc's
	// signature is (Document, bool) and D-006 is why. That answer is right at
	// the read API and wrong here: copied into a merge it becomes an empty-key
	// document, or a term with no postings, written into the segment that
	// replaces the damaged one — turning contained damage that Scrub can name
	// into a segment no reader will ever accept, with the original bytes
	// pruned. writeSegment has no error to return for it, so the failure is
	// collected here and Merge refuses before it publishes anything.
	err error
}

func (m *mergedSource) count() int {
	n := 0
	for _, s := range m.segs {
		n += s.count
	}
	return n
}

func (m *mergedSource) totals() (totalLen, vecDim int) {
	for _, s := range m.segs {
		// The same sum avgDocLen widens, for the same reason: a segment's total
		// is ranged against maxInt on its own and nothing ranges the collection.
		// What differs is where a wrapped one lands. This total is written into
		// meta, so it is not a bad answer to a query — it is a segment claiming
		// a token count no walk of its documents can reach, published and then
		// rejected by the next Scrub. Refuse before the merge names anything,
		// the way every other unreadable source does.
		if s.totalLen > maxInt-totalLen {
			m.failf("merge: the segments hold more than %d tokens between them", maxInt)
			return 0, vecDim
		}
		totalLen += s.totalLen
		if vecDim == 0 {
			vecDim = s.vecDim
		}
	}
	return totalLen, vecDim
}

// segFor finds the segment holding a local id, and the index-wide id it maps to.
func (m *mergedSource) segFor(local int) (*segment, DocID) {
	id := m.base + DocID(local)
	for _, s := range m.segs {
		if s.holds(id) {
			return s, id
		}
	}
	return nil, 0
}

// failf records the first unreadable thing. See mergedSource.err.
func (m *mergedSource) failf(format string, args ...any) {
	if m.err == nil {
		m.err = fmt.Errorf(format+": %w", append(args, ErrCorrupt)...)
	}
}

func (m *mergedSource) doc(local int) Document {
	s, id := m.segFor(local)
	if s == nil {
		m.failf("merge: document %d belongs to no segment being merged", id)
		return Document{}
	}
	d, ok := s.doc(id)
	if !ok {
		m.failf("merge: document %d does not read back", id)
	}
	return d
}

func (m *mergedSource) docLen(local int) int {
	s, id := m.segFor(local)
	if s == nil {
		m.failf("merge: document %d belongs to no segment being merged", id)
		return 0
	}
	return s.docLen(id)
}

// termList is the sorted union of the merged segments' vocabularies. Sorted
// because it comes out of maps: a term order taken from map iteration would give
// two merges of the same corpus different bytes, and posting order decides
// rankings. Milestone 4 section 4.2 is what that costs when nobody checks.
func (m *mergedSource) termList() []string {
	seen := map[string]struct{}{}
	for _, s := range m.segs {
		for t := range s.terms {
			seen[t] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func (m *mergedSource) postings(t string, yield func(Posting)) int {
	total := 0
	for _, s := range m.segs {
		// Each segment that claims the term is asked separately, and each one
		// has to answer. Checking only that something came back would let a
		// term held by nine segments lose one segment's list and still look
		// whole: the merge would publish a replacement missing those documents'
		// postings, prune the segment that could have supplied them, and leave
		// an index Scrub cannot tell from a smaller corpus.
		//
		// A term in s.terms always has at least one posting when the file is
		// intact — encodePostings writes no empty entries — so nil here is
		// damage, which is exactly what lookup returns it for.
		if _, claimed := s.terms[t]; !claimed {
			continue
		}
		// nil size: this streams a term into the writer and never holds the
		// postings, which is the whole reason scanPostings exists beside lookup.
		n := s.scanPostings(t, nil, func(e Posting) {
			yield(Posting{Doc: e.Doc - m.base, Freq: e.Freq})
		})
		if n == 0 {
			m.failf("merge: the segment at %d indexes term %q and its postings do not decode", s.base, t)
			continue
		}
		total += n
	}
	// termList only names terms some segment's index holds, so nothing at all
	// coming back means no segment claimed a term every segment was asked about.
	if total == 0 {
		m.failf("merge: term %q has postings in no segment being merged", t)
	}
	return total
}

// Merge collapses the oldest segments into one until the index holds at most
// maxSegments of them, and publishes the result.
//
// Below the ceiling it does nothing, so calling it after every commit is
// reasonable and never calling it is a decision to let reads walk a longer list.
// weft does not call it for the caller: a merge rewrites the oldest generations,
// and the caller is the one who knows when that is affordable.
//
// The oldest run is merged rather than the smallest, because segments have to be
// adjacent in the DocID space for the merge to be a concatenation — see
// mergedSource. Adjacency is what keeps every document's id, and keeping ids is
// what keeps rankings.
//
// Atomicity is the manifest flip Commit already uses, so a crash leaves either
// the pre-merge state or the post-merge one.
func (ix *Index) Merge() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	if len(ix.segs) <= maxSegments {
		return nil
	}
	if ix.dir == "" {
		return fmt.Errorf("merge: this index has no directory to merge in")
	}
	// Merging k segments leaves one in their place, so k = n - maxSegments + 1
	// brings the count to exactly the ceiling.
	k := len(ix.segs) - maxSegments + 1

	root, err := os.OpenRoot(ix.dir)
	if err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	defer root.Close()
	// The same question Commit asks of its destination, for the same reason: the
	// remembered path names whatever stands there now, and a merge aimed at a
	// stranger's directory would rewrite that stranger's oldest generations from
	// this index's segments.
	if err := ix.sameDir(root); err != nil {
		return fmt.Errorf("merge %s: %w", ix.dir, err)
	}

	gen, live, err := readManifest(root)
	if err != nil {
		return fmt.Errorf("merge %s: %w", ix.dir, err)
	}
	if len(live) != len(ix.segs) {
		return fmt.Errorf("merge %s: the directory holds %d segments, this index has %d: %w",
			ix.dir, len(live), len(ix.segs), ErrCorrupt)
	}

	seg := segDirName(gen + 1)
	if err := root.RemoveAll(seg); err != nil {
		return fmt.Errorf("merge %s: clearing stale segment: %w", ix.dir, err)
	}
	if err := root.Mkdir(seg, 0o700); err != nil {
		return fmt.Errorf("merge %s: %w", ix.dir, err)
	}
	segRoot, err := root.OpenRoot(seg)
	if err != nil {
		return fmt.Errorf("merge %s: %w", ix.dir, err)
	}
	defer segRoot.Close()

	src := &mergedSource{segs: ix.segs[:k], base: ix.segs[0].base}
	merged := segInfo{name: seg, base: src.base, count: src.count()}
	if err := writeSegment(segRoot, src); err != nil {
		return fmt.Errorf("merge %s: %w", seg, err)
	}
	// A segment built out of something that would not read is not a segment to
	// publish. writeSegment cannot report it — the source answers damage the way
	// the read API does, with absence — so the failure comes off the source, and
	// it is checked before anything on disk is named. See mergedSource.err.
	if src.err != nil {
		return fmt.Errorf("merge %s: %w", seg, src.err)
	}
	syncDir(segRoot)
	syncDir(root)

	// Mapped before it is published, not after. The files are durable and
	// nothing names them yet, so a mapping that fails here leaves the directory
	// and this index exactly as they were. Publishing first would leave the
	// manifest a merge ahead of ix.segs, and every later Merge refusing the pair
	// as corrupt — a durable success reported as a failure that cannot be
	// retried.
	replacement, err := openSegment(root, merged.name, merged.base)
	if err != nil {
		return fmt.Errorf("merge %s: %w", seg, err)
	}

	published := append([]segInfo{merged}, live[k:]...)
	if err := writeManifest(root, gen+1, published); err != nil {
		replacement.close() //nolint:errcheck // already returning an error
		return fmt.Errorf("merge %s: %w", ix.dir, err)
	}

	// Swap the merged segments for the one that replaced them. The new mapping
	// was opened above, so the old ones are only released once nothing can fail.
	old := ix.segs[:k]
	ix.segs = append([]*segment{replacement}, ix.segs[k:]...)
	for _, s := range old {
		s.close() //nolint:errcheck // the merge is published; there is nothing to undo
	}

	prune(root, published)
	return nil
}
