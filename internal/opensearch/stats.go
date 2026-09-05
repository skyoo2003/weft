// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"net/http"
	"runtime"

	"github.com/skyoo2003/weft/internal/loadgen"
)

// NodeStats is what this process can say about itself, in the shape
// `GET /_nodes/stats` puts it.
//
// It exists for one reason: milestone 27 points internal/loadgen at the HTTP
// surface, and a load generator running in another process can only read *its
// own* counters. Reporting the client's resident set as the server's would be a
// wrong number with nothing to mark it as wrong — the failure this package
// refuses everywhere else, relocated into a benchmark table.
//
// So the numbers the ladder prints for an HTTP run come from here, read from the
// same instruments internal/loadgen reads in-process. That is what makes the
// two arms comparable at all: one reads these counters directly and the other
// reads them across a socket, and neither is reading a different meter.
//
// It is a subset of OpenSearch's route and deliberately not a pretence at the
// rest. A client asking for index or thread-pool statistics gets the fields that
// exist here and no invented ones.
type NodeStats struct {
	// Process-wide, from getrusage: the peak resident set and the page faults.
	// MaxRSS is a high-water mark that cannot be reset, so a caller wanting a
	// per-rung figure subtracts two reads — which is what the ladder does.
	MaxRSSBytes int64 `json:"max_rss_in_bytes"`
	MinorFaults int64 `json:"minor_faults"`
	MajorFaults int64 `json:"major_faults"`

	// Collector totals, all cumulative and monotonic, so the difference of two
	// reads is the interval between them.
	GCCycles      uint64  `json:"gc_cycles"`
	GCPauseNanos  int64   `json:"gc_pause_total_in_nanos"`
	GCCPUSeconds  float64 `json:"gc_cpu_seconds"`
	CPUSeconds    float64 `json:"cpu_seconds"`
	TotalAlloc    uint64  `json:"total_alloc_in_bytes"`
	Mallocs       uint64  `json:"mallocs"`
	HeapAllocated uint64  `json:"heap_alloc_in_bytes"`

	Goroutines int `json:"goroutines"`
}

// nodeStats answers GET /_nodes/stats.
//
// One stop-the-world per call, from ReadMemStats. That is why this is a route a
// benchmark calls between rungs and not something any other handler touches: an
// instrument that stops the world to answer must not be on the per-request path,
// which is the rule internal/loadgen states and this obeys from the other side.
func (s *Server) nodeStats(w http.ResponseWriter, _ *http.Request) error {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	faults := loadgen.ProcFaults()
	gcCPU, totalCPU := loadgen.GCCPUSeconds()

	stats := NodeStats{
		MaxRSSBytes:   loadgen.MaxRSS(),
		MinorFaults:   faults.Minor,
		MajorFaults:   faults.Major,
		GCCycles:      loadgen.GCCycles(),
		GCPauseNanos:  loadgen.GCPauseTotal().Nanoseconds(),
		GCCPUSeconds:  gcCPU,
		CPUSeconds:    totalCPU,
		TotalAlloc:    mem.TotalAlloc,
		Mallocs:       mem.Mallocs,
		HeapAllocated: mem.HeapAlloc,
		Goroutines:    runtime.NumGoroutine(),
	}

	// Nested under a node id, which is the shape OpenSearch uses and the shape a
	// client's own parser expects. One node, named for what it is.
	//
	// The empty jvm, os and fs objects are the sections that have no analogue
	// here. Present so a client's parser does not fail on their absence, and
	// empty so nothing invented can be read out of them — which is the same
	// choice `_shards` makes by being a constant.
	writeJSON(w, http.StatusOK, map[string]any{
		keyClusterName: clusterName,
		"nodes": map[string]any{
			clusterName: map[string]any{
				"name": clusterName,
				"weft": stats,
				"jvm":  map[string]any{},
				"os":   map[string]any{},
				"fs":   map[string]any{},
			},
		},
	})
	return nil
}
