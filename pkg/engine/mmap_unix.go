// SPDX-License-Identifier: Apache-2.0

//go:build unix

package engine

import (
	"os"
	"syscall"
)

// mapFile maps the whole of f read-only.
//
// mmap rather than ReadAt is the choice this milestone rests on, and the reason
// is the read API rather than performance. The decoders already work over a
// []byte, so a mapped region reaches them with the parsing, the bounds checks
// and the verification unchanged — and an access to mapped memory cannot fail,
// so Index.Doc keeps returning (Document, bool) instead of growing an error.
// Reading through ReadAt would give every one of the six read methods an error
// to return and change all four scorers to handle it, which is exactly the cost
// the milestone 1 golden API file exists to make visible.
//
// What it does not buy: the pages still have to exist while they are read.
// mmap moves a corpus out of the Go heap and into the page cache, which is a
// different accounting rather than a smaller working set — the milestone 3
// section of docs/FINDINGS.md reports both.
//
// An empty file maps to nil rather than to a zero-length mapping, because mmap
// rejects a zero length. No section is ever empty — every one carries at least
// a frame — so this is a guard, not a case.
func mapFile(f *os.File, size int) ([]byte, error) {
	if size == 0 {
		return nil, nil
	}
	return syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
}

// unmapFile releases a mapping returned by mapFile. Every byte slice derived
// from it dangles afterwards, and dereferencing one is a segmentation fault
// rather than a Go panic, so a caller has to be certain nothing it kept still
// points into the region.
func unmapFile(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return syscall.Munmap(b)
}

// mapsLazily is whether this build reaches a section's bytes through the page
// cache rather than through the heap. The milestone 3 memory assertions measure
// the difference between the two, so they have nothing to measure where mapFile
// reads the file instead.
const mapsLazily = true
