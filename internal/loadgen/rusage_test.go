// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"os"
	"runtime"
	"testing"
)

// TestProcFaultsCountsMinorFaultsOnTouch is the assertion behind milestone 5's
// section 1, and it is the reason page faults are read at all.
//
// The plan predicts weft's tail is a working-set problem rather than a GC problem:
// a query touches 210 MiB of distinct pages and a small live heap makes collections
// cheap. Deciding that needs an instrument that can tell a page-cache hit from a
// disk read, and getrusage is the only one available without a profiler — minor
// faults are pages the kernel had, major faults are pages it fetched.
//
// The check is that the counter moves at all under memory the process has not
// touched before. A stub returning a constant would satisfy every arithmetic test
// written about it and fail this one, which is the failure mode worth catching:
// a working-set verdict resting on a number that never changes.
func TestProcFaultsCountsMinorFaultsOnTouch(t *testing.T) {
	before := ProcFaults()
	if before == (FaultCounts{}) {
		// The non-unix stub reports zeros by design, the same way maxRSS does.
		// Nothing below can distinguish that from a broken read, so say so
		// rather than assert against a platform that has no answer.
		t.Skipf("getrusage reports nothing on %s", runtime.GOOS)
	}

	// Fresh, untouched, and large enough that the fault count cannot be lost in noise.
	// Written to rather than merely allocated, because Go may hand back memory it
	// already faulted in — and strided by the real page size rather than a hardcoded
	// 4096, which is the number this had and which is wrong on the platform the
	// milestone was measured on: darwin/arm64 pages are 16 KiB, so 4096 touched four
	// addresses per page and the stated margin of "16,384 pages" was 4,096.
	const n = 64 << 20
	page := os.Getpagesize()
	buf := make([]byte, n)
	for i := 0; i < n; i += page {
		buf[i] = 1
	}
	runtime.KeepAlive(buf)

	after := ProcFaults()
	if after.Minor <= before.Minor {
		t.Errorf("minor faults %d -> %d after touching %d MiB in %d %d-byte pages; the "+
			"counter is not moving", before.Minor, after.Minor, n>>20, n/page, page)
	}
	if after.Major < before.Major {
		t.Errorf("fault counters went backwards: %+v -> %+v", before, after)
	}
}

// TestProcFaultsSubIsPerField pins the delta arithmetic the report prints.
//
// A run's figure is the difference between two snapshots, never a raw counter:
// the process has already faulted in the index mapping, the query set and the Go
// runtime itself before the first request is sent, and a report quoting the
// absolute number would attribute all of that to the load.
func TestProcFaultsSubIsPerField(t *testing.T) {
	a := FaultCounts{Minor: 100, Major: 20, Nvcsw: 7, Nivcsw: 3}
	b := FaultCounts{Minor: 40, Major: 5, Nvcsw: 2, Nivcsw: 1}
	got := a.Sub(b)
	want := FaultCounts{Minor: 60, Major: 15, Nvcsw: 5, Nivcsw: 2}
	if got != want {
		t.Errorf("sub = %+v, want %+v", got, want)
	}
}
