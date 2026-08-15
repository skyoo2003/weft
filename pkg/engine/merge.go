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
	postings(term string) []Posting
}

// pendingSource is the in-memory half of an Index: everything added since the
// last commit. Requires ix.mu.
//
// buf is reused across terms rather than allocated per term. The pending
// postings carry index-wide ids and a segment records local ones, so every term
// needs a translated copy — and allocating one per term put 9 MB back on a
// commit that had just been measured down to 588 KB. encodePostings finishes
// with each term before asking for the next, so one buffer is enough.
type pendingSource struct {
	ix  *Index
	buf []Posting
}

func (p *pendingSource) count() int { return len(p.ix.docs) }
func (p *pendingSource) totals() (totalLen, vecDim int) {
	return p.ix.totalLen, p.ix.vecDim
}
func (p *pendingSource) doc(local int) Document { return p.ix.docs[local] }
func (p *pendingSource) docLen(local int) int   { return p.ix.docLen[local] }
func (p *pendingSource) termList() []string     { return slices.Sorted(maps.Keys(p.ix.postings)) }

func (p *pendingSource) postings(t string) []Posting {
	pl := p.ix.postings[t]
	p.buf = append(p.buf[:0], pl...)
	for i := range p.buf {
		p.buf[i].Doc -= p.ix.base
	}
	return p.buf
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
}

func (m mergedSource) count() int {
	n := 0
	for _, s := range m.segs {
		n += s.count
	}
	return n
}

func (m mergedSource) totals() (totalLen, vecDim int) {
	for _, s := range m.segs {
		totalLen += s.totalLen
		if vecDim == 0 {
			vecDim = s.vecDim
		}
	}
	return totalLen, vecDim
}

// segFor finds the segment holding a local id, and the index-wide id it maps to.
func (m mergedSource) segFor(local int) (*segment, DocID) {
	id := m.base + DocID(local)
	for _, s := range m.segs {
		if s.holds(id) {
			return s, id
		}
	}
	return nil, 0
}

func (m mergedSource) doc(local int) Document {
	s, id := m.segFor(local)
	if s == nil {
		return Document{}
	}
	d, _ := s.doc(id)
	return d
}

func (m mergedSource) docLen(local int) int {
	s, id := m.segFor(local)
	if s == nil {
		return 0
	}
	return s.docLen(id)
}

// termList is the sorted union of the merged segments' vocabularies. Sorted
// because it comes out of maps: a term order taken from map iteration would give
// two merges of the same corpus different bytes, and posting order decides
// rankings. Milestone 4 section 4.2 is what that costs when nobody checks.
func (m mergedSource) termList() []string {
	seen := map[string]struct{}{}
	for _, s := range m.segs {
		for t := range s.terms {
			seen[t] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func (m mergedSource) postings(t string) []Posting {
	var out []Posting
	for _, s := range m.segs {
		for _, e := range s.lookup(t) {
			out = append(out, Posting{Doc: e.Doc - m.base, Freq: e.Freq})
		}
	}
	return out
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

	src := mergedSource{segs: ix.segs[:k], base: ix.segs[0].base}
	merged := segInfo{name: seg, base: src.base, count: src.count()}
	if err := writeSegment(segRoot, src); err != nil {
		return fmt.Errorf("merge %s: %w", seg, err)
	}
	syncDir(segRoot)
	syncDir(root)

	published := append([]segInfo{merged}, live[k:]...)
	if err := writeManifest(root, gen+1, published); err != nil {
		return fmt.Errorf("merge %s: %w", ix.dir, err)
	}

	// Swap the merged segments for the one that replaced them. The new mapping
	// is opened before the old ones are released, so a failure above leaves the
	// index reading exactly what it read before.
	replacement, err := openSegment(root, merged.name, merged.base)
	if err != nil {
		return fmt.Errorf("merge %s: %w", seg, err)
	}
	old := ix.segs[:k]
	ix.segs = append([]*segment{replacement}, ix.segs[k:]...)
	for _, s := range old {
		s.close() //nolint:errcheck // the merge is published; there is nothing to undo
	}

	prune(root, published)
	return nil
}
