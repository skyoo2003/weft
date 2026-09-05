// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"

	"github.com/skyoo2003/weft/pkg/engine"
)

// Adjacency is an index's link structure in DocID space: every edge resolved
// once, held forward and reverse, so a hop is array indexing instead of a
// document decode and a key lookup.
//
// # Why this exists rather than traversing the index directly
//
// Scorer.bfs walks the live index, and each node it visits costs an
// engine.Index.Doc — which materialises the key, the text and the vector to
// reach the one field a traversal wants — plus an engine.Index.Resolve per link,
// each taking the index-wide read lock. On the evaluation corpus that is a
// 768-wide vector faulted in and dropped per node, and one lock acquisition per
// edge. Here the whole corpus costs one walk, and every hop afterwards is a
// slice bound.
//
// That is what makes an algorithm with a real work bound affordable. PPR's push
// loop touches a node's edges tens of thousands of times per query; against Doc
// and Resolve it would not be a scorer, it would be a full corpus scan.
//
// # It is a snapshot, and that is the contract
//
// The edges are read once, at construction. A document added, updated or deleted
// afterwards is not in here: its edges are missing, and an edge into a document
// deleted since still points at it. Rebuild after ingest. This is the same shape
// the package documentation recommends for any corpus-sized side store — build
// it once, hand it to the scorers, and keep the per-query work per-query — and
// it is why nothing here takes a lock.
//
// The index reference it keeps is not the edges. It is what resolves a query's
// own Query.Seeds, which are Keys and are supplied per query, so that half does
// see the live index.
type Adjacency struct {
	ix *engine.Index

	// CSR, twice over. A node's out-edges are out[outOff[id]:outOff[id+1]] and
	// its out-degree is the difference between the two — which is the whole
	// reason for the shape, since a push step needs the degree before it reads a
	// single edge.
	//
	// The offset arrays hold one entry per id plus a terminator, so they are
	// indexed by DocID with no translation. Ids are dense and start at zero, and
	// a tombstoned id keeps its slot with an empty row rather than shifting
	// everything behind it — the same reason engine never renumbers.
	out, in       []engine.DocID
	outOff, inOff []int
}

// NewAdjacency reads every document's links and resolves them into DocID space.
//
// It walks the whole id space once, through engine.Index.Neighbors, so it costs
// one pass over the link keys of the corpus and touches no vector page. ctx is
// polled during the walk; a cancelled build returns its error and no adjacency.
//
// Dangling links — a Key no live document holds — are dropped, which is what
// Neighbors already does and what Document.Links documents as ordinary. A
// deleted document keeps its slot in the id space with no edges of its own, and
// no surviving edge points at it.
//
// Duplicates are kept. A document listing one Key twice yields that edge twice,
// and a traversal that would double-count has to say so for itself; Neighbors
// carries the argument.
//
// ponytail: the build is dominated by key resolution, not by reading the links.
// BenchmarkAdjacencyBuild is 93.6 ms and 4.4 million allocations over 20,000
// documents and 120,000 edges — about 37 allocations an edge, which is one
// binary search through the keys table decoding a string at every probe. The
// links themselves are 7 allocations a document. It is paid once and
// BenchmarkGraphArm saves 1.15 ms a query against it, so it repays in 81
// queries; that is the whole of the argument for leaving it. The way out is a
// key-to-id cache across the walk, which needs the raw keys and therefore a
// second engine accessor beside Neighbors — worth buying when a corpus makes
// this show up as ingest latency rather than as a startup cost.
func NewAdjacency(ctx context.Context, ix *engine.Index) (*Adjacency, error) {
	n := ix.Len()
	a := &Adjacency{
		ix:     ix,
		outOff: make([]int, n+1),
		inOff:  make([]int, n+1),
	}

	for id := range n {
		// The corpus is unbounded and each step decodes a record, so the poll is
		// inside the walk rather than around it. Every 1024, as in the link and
		// posting scans elsewhere in this package.
		if id&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		// Ignoring the bool: an id that is deleted or was never assigned has no
		// edges, which is the empty row this leaves behind. Distinguishing the
		// two would mean a third array to say which, and no traversal asks — a
		// node with no edges is neither reachable nor expandable either way.
		nb, _ := ix.Neighbors(engine.DocID(id))
		a.out = append(a.out, nb...)
		// Written as the walk goes, in id order, so the forward direction is CSR
		// by construction and needs no sort.
		a.outOff[id+1] = len(a.out)
	}

	a.buildReverse(n)
	return a, nil
}

// buildReverse fills the in-edges by counting sort over the out-edges.
//
// Counted rather than collected: an explicit edge list would be eight bytes per
// edge held alongside both CSRs, and the counts are already recoverable from the
// forward direction. What it does cost is one cursor per node for the duration,
// which is the same width as an offset array and is dropped on return.
//
// Each row comes out ascending by source, because the fill walks sources in
// order. Nothing requires that today; it is what makes a traversal reproducible
// across builds, which the float accumulation in PPR does require.
func (a *Adjacency) buildReverse(n int) {
	for _, to := range a.out {
		a.inOff[to+1]++
	}
	for i := 1; i <= n; i++ {
		a.inOff[i] += a.inOff[i-1]
	}
	a.in = make([]engine.DocID, len(a.out))

	cursor := make([]int, n)
	copy(cursor, a.inOff[:n])
	for from := range n {
		for _, to := range a.out[a.outOff[from]:a.outOff[from+1]] {
			a.in[cursor[to]] = engine.DocID(from)
			cursor[to]++
		}
	}
}

// Len is the id space the adjacency covers: one past the highest DocID the index
// had assigned when it was built. It is engine.Index.Len at that moment,
// tombstones included, so a DocID from that index indexes into this without
// translation.
func (a *Adjacency) Len() int { return len(a.outOff) - 1 }

// Edges is how many resolved edges the adjacency holds, counting each direction
// once. An edge appears in both the forward and the reverse structure, so the
// memory is twice this many ids plus the two offset arrays.
func (a *Adjacency) Edges() int { return len(a.out) }

// Out are the documents id links to, in Document.Links order with the dangling
// keys removed. The result aliases the adjacency and must not be modified.
//
// An id past Len, or one that had no links, answers nil. Those are the same
// answer on purpose — see NewAdjacency.
func (a *Adjacency) Out(id engine.DocID) []engine.DocID { return a.row(a.out, a.outOff, id) }

// In are the documents that link to id, ascending. The result aliases the
// adjacency and must not be modified.
//
// This is the direction a citation graph does not store and a query usually
// wants: Document.Links says what a paper cites, and "what cites this paper" is
// the reverse of every edge in the corpus. Deriving it per query would mean
// scanning every document's links; deriving it once is what this type is for.
func (a *Adjacency) In(id engine.DocID) []engine.DocID { return a.row(a.in, a.inOff, id) }

func (a *Adjacency) row(vals []engine.DocID, off []int, id engine.DocID) []engine.DocID {
	// Widened to int64 on both sides rather than compared in uint64. A DocID is
	// uint32, so on a 32-bit build int(id) wraps negative at 1<<31 and the guard
	// passes where it must not; int64 holds every value of both types exactly on
	// every target. Index.Doc compares in uint64 for the mirror image of this.
	if int64(id) >= int64(len(off)-1) {
		return nil
	}
	return vals[off[id]:off[id+1]]
}

// Degree is how many edges touch id in either direction.
//
// Both directions, because that is the graph the traversals in this package walk
// and a degree that disagreed with the edge list a push loop iterates would
// break the arithmetic bounding its work. A caller wanting one direction takes
// len of Out or In.
func (a *Adjacency) Degree(id engine.DocID) int {
	if int64(id) >= int64(len(a.outOff)-1) {
		return 0
	}
	return (a.outOff[id+1] - a.outOff[id]) + (a.inOff[id+1] - a.inOff[id])
}
