// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"runtime"
	"syscall"
)

// maxRSS is the process's peak resident set, in bytes.
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
func maxRSS() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		return int64(ru.Maxrss) //nolint:unconvert // int32 on 32-bit unix
	}
	return int64(ru.Maxrss) * 1024 //nolint:unconvert // int32 on 32-bit unix
}
