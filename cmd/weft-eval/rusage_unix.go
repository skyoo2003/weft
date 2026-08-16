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
// standard library does not paper over.
func maxRSS() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return int64(ru.Maxrss)
	}
	return int64(ru.Maxrss) * 1024
}
