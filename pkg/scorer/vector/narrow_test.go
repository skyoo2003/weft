// SPDX-License-Identifier: Apache-2.0

package vector

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

// This file is the milestone 3b repayment, stated as something a test can see.
//
// The rest of vector_test.go is the contract, and none of it changed — that is
// the evidence the repayment cost nothing a caller can observe. What no test in
// this package could see before is the thing the ponytail marker on Candidates
// promised to fix: the scan was O(corpus) per query, and mapping the index in
// milestone 3a moved those bytes from the Go heap into the page cache without
// reducing how many of them a query touches.
//
// Allocation is the instrument, for the reason engine's own lazy tests give: a
// document that comes out of a mapped segment is decoded, so its vector is a
// fresh allocation. Decoding every document therefore allocates at least the
// corpus's worth of vector bytes per query, and decoding a partition of them
// allocates a fraction of it. The number below is not a performance target; it
// is the difference between a scan that narrowed and one that did not.

// committedVectorIndex writes n documents of width dim into a temporary
// directory and opens it, so the index under test is a mapped, partitioned one.
// An uncommitted index is all pending, which by design carries no partition —
// so an in-memory corpus could never show this.
func committedVectorIndex(t *testing.T, n, dim, groups int) *engine.Index {
	t.Helper()
	// A deterministic clustered corpus. Splitmix64, so the numbers below do not
	// move between runs and a failure is a failure rather than a draw.
	var s uint64
	next := func() uint64 {
		s += 0x9E3779B97F4A7C15
		z := s
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		return z ^ (z >> 31)
	}
	unit := func() float32 { return float32(int64(next()>>11))/float32(int64(1)<<52) - 1 }

	centres := make([][]float32, groups)
	for c := range centres {
		v := make([]float32, dim)
		for j := range v {
			v[j] = unit()
		}
		centres[c] = v
	}

	ix := engine.New()
	for i := range n {
		c := centres[int(next()%uint64(groups))]
		v := make([]float32, dim)
		for j := range v {
			v[j] = c[j] + unit()*0.12
		}
		if _, err := ix.Add(engine.Document{Key: fmt.Sprintf("doc-%06d", i), Vector: v}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	dir := t.TempDir()
	if err := ix.Commit(dir); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := engine.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { got.Close() }) //nolint:errcheck // teardown
	return got
}

// TestTheScanIsNarrowedByTheIndex is ponytail:36 coming due.
//
// The marker said a brute-force full scan was milestone 3's to remove, and this
// is the assertion that it was: one query must not touch the whole corpus. How
// much it narrows on the real citation corpus is `weft-eval recall`'s question —
// this one only refuses the case where the partition is written, read, and then
// not actually used.
// The corpus is large because nprobe is a constant. nlist grows as √n, so a
// fixed 64 lists is most of a small segment and a fraction of a large one — that
// is the whole reason the constant is a constant, and it means a small corpus
// cannot show narrowing even when the partition is working perfectly. At 65,536
// documents nlist is 256, so a query probes a quarter of the lists.
func TestTheScanIsNarrowedByTheIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("commits a 65,536-document corpus")
	}
	const (
		n   = 65536
		dim = 32
	)
	ix := committedVectorIndex(t, n, dim, 40)
	s := New(ix)

	q := make([]float32, dim)
	q[0] = 1
	ctx := t.Context()

	// One warm-up outside the measurement: the first query maps pages and grows
	// buffers that later queries reuse, and none of that is per-query cost.
	if _, err := s.Candidates(ctx, engine.Query{Vector: q}, 10); err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	// The baseline is a real full scan of the same index rather than a figure
	// derived from the corpus's shape: decoding one record allocates its key, its
	// text, its vector and its links, and only measuring it says what all four
	// come to. This is exactly the loop Candidates used to run.
	var b0, b1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&b0)
	for i := range ix.Len() {
		if _, ok := ix.Doc(engine.DocID(i)); !ok {
			t.Fatalf("Doc(%d) is missing", i)
		}
	}
	runtime.ReadMemStats(&b1)
	fullScan := b1.TotalAlloc - b0.TotalAlloc

	const queries = 5
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range queries {
		if _, err := s.Candidates(ctx, engine.Query{Vector: q}, 10); err != nil {
			t.Fatalf("Candidates: %v", err)
		}
	}
	runtime.ReadMemStats(&after)
	perQuery := (after.TotalAlloc - before.TotalAlloc) / queries

	t.Logf("%d B allocated per query, against %d B for a full scan of the same index (%.1f%%)",
		perQuery, fullScan, 100*float64(perQuery)/float64(fullScan))
	if perQuery > fullScan/2 {
		t.Errorf("a query allocated %d B against %d B for a full scan — the scan is still touching everything",
			perQuery, fullScan)
	}
}
