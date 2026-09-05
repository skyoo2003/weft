// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

const (
	benchDocs   = 20000
	benchDegree = 6
	benchDim    = 128
)

// benchCorpus is a committed, reopened index of linked documents that carry
// vectors.
//
// Three properties of it are load-bearing and none is decoration:
//
//   - **Committed and reopened**, so the documents live in a mapped segment. A
//     pending index hands Doc the struct the caller passed, which is free; the
//     decode this milestone removes only exists on the other side of a commit.
//   - **Carrying vectors**, because that is what a record decode costs. The
//     evaluation corpus is 69% vector bytes and a traversal reads none of them.
//   - **Linked densely enough to have a neighbourhood**, so a traversal has
//     somewhere to go.
func benchCorpus(b *testing.B, docs, degree, dim int) *engine.Index {
	b.Helper()
	dir := b.TempDir()
	ix := engine.New()
	rng := rand.New(rand.NewPCG(1, 2))

	for i := range docs {
		v := make([]float32, dim)
		for j := range v {
			v[j] = rng.Float32()
		}
		links := make([]string, degree)
		for j := range links {
			links[j] = fmt.Sprintf("d%06d", rng.IntN(docs))
		}
		d := engine.Document{
			Key:    fmt.Sprintf("d%06d", i),
			Text:   fmt.Sprintf("document %d about ranking and fusion", i),
			Vector: v,
			Links:  links,
		}
		if _, err := ix.Add(d); err != nil {
			b.Fatalf("Add: %v", err)
		}
	}
	if err := ix.Commit(context.Background(), dir); err != nil {
		b.Fatalf("Commit: %v", err)
	}
	_ = ix.Close()

	opened, err := engine.Open(dir)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = opened.Close() })
	return opened
}

func benchSeeds(n int) engine.Query {
	seeds := make([]string, n)
	for i := range seeds {
		seeds[i] = fmt.Sprintf("d%06d", i*37%benchDocs)
	}
	return engine.Query{Seeds: seeds}
}

// BenchmarkAdjacencyBuild is the cost the snapshot is paid for once, against
// which every per-query saving below has to be worth it.
func BenchmarkAdjacencyBuild(b *testing.B) {
	ix := benchCorpus(b, benchDocs, benchDegree, benchDim)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := NewAdjacency(context.Background(), ix); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGraphArm is the comparison this milestone exists to make: the same
// query answered by the traversal that walks the live index and by the walk that
// reads a resolved adjacency.
//
// The two are not the same algorithm and the number is not a speedup of one into
// the other. What it measures is what a caller pays for a graph stream, before
// and after — which is the quantity that decides whether graph search is
// affordable at all.
func BenchmarkGraphArm(b *testing.B) {
	ix := benchCorpus(b, benchDocs, benchDegree, benchDim)
	adj, err := NewAdjacency(context.Background(), ix)
	if err != nil {
		b.Fatal(err)
	}
	p, err := NewPPR(adj, nil)
	if err != nil {
		b.Fatal(err)
	}
	bfs := New(ix, nil)
	q := benchSeeds(SeedN)
	ctx := context.Background()

	b.Run("bfs-over-index", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := bfs.Candidates(ctx, q, 10); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("ppr-over-adjacency", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := p.Candidates(ctx, q, 10); err != nil {
				b.Fatal(err)
			}
		}
	})
}
