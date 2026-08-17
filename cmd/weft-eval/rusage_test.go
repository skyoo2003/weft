// SPDX-License-Identifier: Apache-2.0

package main

import (
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
	before := procFaults()
	if before == (procFaultCounts{}) {
		// The non-unix stub reports zeros by design, the same way maxRSS does.
		// Nothing below can distinguish that from a broken read, so say so
		// rather than assert against a platform that has no answer.
		t.Skipf("getrusage reports nothing on %s", runtime.GOOS)
	}

	// Fresh, untouched, and large enough that the fault count cannot be lost in
	// noise: 64 MiB is 16,384 pages. Written to rather than merely allocated,
	// because Go may hand back memory it already faulted in.
	const n = 64 << 20
	buf := make([]byte, n)
	for i := 0; i < n; i += 4096 {
		buf[i] = 1
	}
	runtime.KeepAlive(buf)

	after := procFaults()
	if after.minor <= before.minor {
		t.Errorf("minor faults %d -> %d after touching %d MiB; the counter is not moving",
			before.minor, after.minor, n>>20)
	}
	if after.major < before.major {
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
	a := procFaultCounts{minor: 100, major: 20, nvcsw: 7, nivcsw: 3}
	b := procFaultCounts{minor: 40, major: 5, nvcsw: 2, nivcsw: 1}
	got := a.sub(b)
	want := procFaultCounts{minor: 60, major: 15, nvcsw: 5, nivcsw: 2}
	if got != want {
		t.Errorf("sub = %+v, want %+v", got, want)
	}
}
