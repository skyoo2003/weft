// SPDX-License-Identifier: Apache-2.0

package text

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

// benchCorpus is a corpus large enough for the accumulator's sizing to matter,
// with a deliberately skewed term distribution.
//
// The skew is the point. A query's cost is decided by how many documents its
// terms reach, and the two extremes are what the sizing question is actually
// about: a term in three documents, and a term in all of them. A corpus of
// uniformly frequent terms would make every query look like the expensive one
// and hide exactly the case that is being taxed.
// It is committed and reopened, so the postings come out of a mapped segment.
// A pending index answers PostingBound from a Go map and decodes no blocks at
// all, which is the one arrangement where the thing being measured is absent.
func benchCorpus(b *testing.B, docs int) *engine.Index {
	b.Helper()
	dir := b.TempDir()
	ix := engine.New()
	rng := rand.New(rand.NewPCG(7, 11))
	for i := range docs {
		var sb strings.Builder
		// "common" is in every document; "mid" in one in fifty; "rare" in three.
		sb.WriteString("common alpha beta ")
		if i%50 == 0 {
			sb.WriteString("mid ")
		}
		if i < 3 {
			sb.WriteString("rare ")
		}
		for range 20 {
			fmt.Fprintf(&sb, "w%d ", rng.IntN(5000))
		}
		if _, err := ix.Add(engine.Document{Key: fmt.Sprintf("d%07d", i), Text: sb.String()}); err != nil {
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

// BenchmarkCandidates is the cost of one query at each end of the skew.
//
// `rare` is the case that decides whether the accumulator's sizing is a tax:
// three postings to walk, against a map sized by whatever the scorer decided to
// size it by.
func BenchmarkCandidates(b *testing.B) {
	const docs = 50000
	ix := benchCorpus(b, docs)
	s := New(ix)
	ctx := context.Background()

	for _, q := range []struct {
		name string
		text string
	}{
		{"rare-one-term", "rare"},
		{"mid-one-term", "mid"},
		{"common-one-term", "common"},
		{"mixed-three-terms", "rare mid common"},
		{"absent", "nosuchtermanywhere"},
	} {
		b.Run(q.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := s.Candidates(ctx, engine.Query{Text: q.text}, 10); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
