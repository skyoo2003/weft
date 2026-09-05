// SPDX-License-Identifier: Apache-2.0

// Head-to-head query latency against bleve, on one synthetic corpus built the
// same way for both engines.
//
// # Why this exists beside the ladder
//
// main.go drives an open-loop load ladder against a prepared TREC-COVID index,
// which is where docs/PERF.md's published tail comes from. It needs a corpus
// that is not in the repository, it takes about 90 minutes, and it has come back
// void three rounds running (docs/FINDINGS.md milestone 14).
//
// This asks a smaller question the ladder cannot be waited on for: **for one
// query, sequentially, how much work does each engine do?** That is not a tail
// and not a throughput figure, and it must not be read as either. It is the
// number that moves when a scorer stops sorting fifty thousand candidates to
// return ten, and it is the number a change to weft's query path can be checked
// against on a laptop in seconds.
//
// # What is matched and what is not
//
// Matched: the documents, the query strings, the result count, and the machine.
// Both engines index the same 50,000 generated documents with the same skewed
// term distribution, and both are asked for the top 10.
//
// **Not matched, and each of these favours one side:**
//
//   - bleve stems and removes stopwords through its standard analyzer; weft's
//     default tokenizer lowercases and splits at non-letters and does neither.
//     Different term spaces, so this is not a quality comparison and no nDCG
//     figure may be derived from it.
//   - bleve's scorch index is on disk behind its own cache; weft's is committed
//     and reopened, so both read mapped bytes, but the two cache designs are not
//     the same design.
//   - bleve returns stored fields and weft returns DocIDs. bleve is asked not to
//     load them, which narrows the gap without closing it.
//
// So the honest reading is an order-of-magnitude one: whether weft is in the
// same class as bleve for the work a query costs, and which direction a change
// moved it.
package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/blevesearch/bleve/v2"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/scorer/text"
)

const (
	headDocs = 50000
	headK    = 10
)

// headQueries span the shape that decides a query's cost: how many documents its
// rarest term reaches.
var headQueries = []struct{ name, text string }{
	{"absent", "nosuchtermanywhere"},
	{"rare", "rare"},
	{"mid", "mid"},
	{"common", "common"},
	{"mixed", "rare mid common"},
}

type headDoc struct{ Key, Text string }

// headCorpus generates the documents both engines index.
//
// Deterministic from a fixed seed, so the two engines see byte-identical text
// and a rerun compares against the same corpus rather than a new one.
func headCorpus() []headDoc {
	rng := rand.New(rand.NewPCG(7, 11))
	out := make([]headDoc, headDocs)
	for i := range out {
		var sb strings.Builder
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
		out[i] = headDoc{fmt.Sprintf("d%07d", i), sb.String()}
	}
	return out
}

func weftIndex(b *testing.B) *engine.Index {
	b.Helper()
	dir := b.TempDir()
	ix := engine.New()
	for _, d := range headCorpus() {
		if _, err := ix.Add(engine.Document{Key: d.Key, Text: d.Text}); err != nil {
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

func bleveIndex(b *testing.B) bleve.Index {
	b.Helper()
	ix, err := bleve.New(b.TempDir()+"/idx", indexMapping())
	if err != nil {
		b.Fatalf("bleve.New: %v", err)
	}
	b.Cleanup(func() { _ = ix.Close() })

	batch := ix.NewBatch()
	for i, d := range headCorpus() {
		if err := batch.Index(d.Key, map[string]any{"body": d.Text}); err != nil {
			b.Fatalf("batch.Index: %v", err)
		}
		if i%2000 == 1999 {
			if err := ix.Batch(batch); err != nil {
				b.Fatalf("Batch: %v", err)
			}
			batch = ix.NewBatch()
		}
	}
	if err := ix.Batch(batch); err != nil {
		b.Fatalf("Batch: %v", err)
	}
	return ix
}

// BenchmarkHeadToHeadWeft is weft's side: one BM25 scorer, fused, top 10.
//
// Through engine.Search and fusion.Fuse rather than calling the scorer directly,
// because that is what a caller writes and the fusion is part of what a query
// costs.
func BenchmarkHeadToHeadWeft(b *testing.B) {
	ix := weftIndex(b)
	txt := text.New(ix)
	ctx := context.Background()

	for _, q := range headQueries {
		b.Run(q.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				got, err := engine.Search(ctx, engine.Query{Text: q.text}, headK,
					engine.Fuser(fusion.Fuse), txt)
				if err != nil {
					b.Fatal(err)
				}
				_ = got
			}
		})
	}
}

// BenchmarkHeadToHeadBleve is bleve's side, asked for the same ten documents and
// told not to load stored fields — weft returns DocIDs, and loading documents
// would measure something weft does not do.
func BenchmarkHeadToHeadBleve(b *testing.B) {
	ix := bleveIndex(b)

	for _, q := range headQueries {
		b.Run(q.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				req := bleve.NewSearchRequestOptions(bleve.NewQueryStringQuery(q.text), headK, 0, false)
				req.Fields = nil
				res, err := ix.Search(req)
				if err != nil {
					b.Fatal(err)
				}
				_ = res
			}
		})
	}
}
