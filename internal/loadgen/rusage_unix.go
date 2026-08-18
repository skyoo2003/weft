// SPDX-License-Identifier: Apache-2.0

//go:build unix

package loadgen

import (
	"runtime"
	"syscall"
)

// MaxRSS is the process's peak resident set, in bytes.
//
// It is the OS's own number and it is a high-water mark rather than a current
// reading, which is what the working-set question wants: a scan that touched the
// whole corpus once leaves a mark a later cheap query cannot erase.
//
// Ru_maxrss is bytes on Darwin and kilobytes on Linux, a portability trap the
// standard library does not paper over. ios is the same XNU kernel as darwin and
// reports the same unit, so it belongs on the same side of the branch rather than
// silently 1024x high. What is left unhandled is solaris and illumos, which report
// pages: the number there reads low by the page size over a kilobyte, and nobody
// measures weft on one. `//go:build unix` is what makes them reachable at all.
//
// The two conversions look unnecessary and are not: Maxrss is int64 on the
// 64-bit targets and int32 on linux/386 and linux/arm, so unconvert is right
// about the machine it ran on and wrong about the build.
// MaxRSS is exported because recall.go reads it too; one implementation of a
// platform quirk is the point of it living here.
func MaxRSS() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		return int64(ru.Maxrss) //nolint:unconvert // int32 on 32-bit unix
	}
	return int64(ru.Maxrss) * 1024 //nolint:unconvert // int32 on 32-bit unix
}

// ProcFaults reads the counters that say what a load test was waiting on.
//
// Unlike Maxrss these need no unit branch: all four are plain counts on every
// unix. What they are counts *of* is the part worth stating, because milestone
// 5's verdict turns on telling them apart.
//
//	minor   a page the kernel already had — the mapping was in the page cache,
//	        and the cost is a trap and a PTE write, hundreds of nanoseconds
//	major   a page the kernel had to fetch — the cost is the storage device,
//	        four to five orders of magnitude more
//	nvcsw   the process yielded: it blocked on something and gave up its core
//	nivcsw  the scheduler took the core away, which is what over-subscription
//	        looks like from inside the process
//
// A tail dominated by major faults is a storage problem, a tail dominated by
// nivcsw is a concurrency problem, and a tail with neither is the collector's or
// the code's. That is the whole discrimination the plan's section 1 asks for, and
// none of it is visible in latency alone.
//
// Process-wide and monotonic, so a run's figure is always a difference between two
// snapshots — see FaultCounts.Sub. Reading them costs one syscall, which is why this
// is called once around a run rather than once around a request.
func ProcFaults() FaultCounts {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return FaultCounts{}
	}
	// int64 conversions for the same reason MaxRSS needs them: these fields are
	// int64 on 64-bit unix and int32 on linux/386 and linux/arm.
	return FaultCounts{
		Minor:  int64(ru.Minflt), //nolint:unconvert // int32 on 32-bit unix
		Major:  int64(ru.Majflt), //nolint:unconvert // int32 on 32-bit unix
		Nvcsw:  int64(ru.Nvcsw),  //nolint:unconvert // int32 on 32-bit unix
		Nivcsw: int64(ru.Nivcsw), //nolint:unconvert // int32 on 32-bit unix
	}
}
