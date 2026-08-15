// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package engine

import (
	"io"
	"os"
)

// mapFile reads the whole of f into the heap on platforms weft has no mapping
// for.
//
// Correctness is identical and laziness is not: everything above this line
// behaves the same, and the file is read in full rather than on demand. That is
// the honest shape of the fallback. Pretending otherwise would mean a platform
// where the milestone 3 memory assertions quietly measure nothing.
//
// Windows has file mapping and weft does not use it, because the standard
// library does not expose one and this project has no external dependencies —
// the PRD's operational metric is that `go list -m all` prints one module. A
// syscall-level implementation is possible and is not free: CreateFileMapping
// and MapViewOfFile have their own lifetime rules, and getting them wrong is a
// crash rather than an error. Not worth it before somebody runs weft there.
func mapFile(f *os.File, size int) ([]byte, error) {
	if size == 0 {
		return nil, nil
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(f, b); err != nil {
		return nil, err
	}
	return b, nil
}

// unmapFile has nothing to release: mapFile returned heap memory the garbage
// collector owns.
func unmapFile([]byte) error { return nil }
