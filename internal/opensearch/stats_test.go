// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The route exists so milestone 27's ladder can report the *server's* memory
// rather than the load generator's. What that needs from it is narrow and exact:
// the counters have to be present, they have to be this process's, and the
// cumulative ones have to move in the direction that makes a difference of two
// reads mean something.
func TestNodeStatsReportsThisProcessAndNotAnEmptyShape(t *testing.T) {
	srv := dslCorpus(t)

	read := func() map[string]any {
		t.Helper()
		status, raw := do(t, srv, http.MethodGet, "/_nodes/stats", "")
		if status != http.StatusOK {
			t.Fatalf("status %d (body %s)", status, raw)
		}
		var doc struct {
			Nodes map[string]struct {
				Weft map[string]any `json:"weft"`
				JVM  map[string]any `json:"jvm"`
				OS   map[string]any `json:"os"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decode: %v (body %s)", err, raw)
		}
		if len(doc.Nodes) != 1 {
			t.Fatalf("%d nodes, want 1 (body %s)", len(doc.Nodes), raw)
		}
		for _, n := range doc.Nodes {
			// The sections with no analogue are present and empty rather than
			// invented — a number in here is one somebody could read out.
			if len(n.JVM) != 0 || len(n.OS) != 0 {
				t.Errorf("jvm=%v os=%v; these have no analogue here and must stay empty", n.JVM, n.OS)
			}
			return n.Weft
		}
		return nil
	}

	first := read()
	// Every field the ladder subtracts. A missing one decodes as zero and makes the
	// difference of two reads look like "nothing happened", which is the silent
	// wrong answer this package is arranged against.
	for _, field := range []string{
		"max_rss_in_bytes", "minor_faults", "major_faults",
		"gc_cycles", "gc_pause_total_in_nanos", "gc_cpu_seconds", "cpu_seconds",
		"total_alloc_in_bytes", "mallocs", "heap_alloc_in_bytes", "goroutines",
	} {
		if _, ok := first[field]; !ok {
			t.Errorf("_nodes/stats has no %q; a ladder subtracting it would read zero and report no change", field)
		}
	}
	if n, _ := first["goroutines"].(float64); n < 1 {
		t.Errorf("goroutines is %v; these are not a running process's numbers", first["goroutines"])
	}

	// Doing work between the reads is what makes the rest a test rather than a
	// coincidence.
	for range 50 {
		hitIDs(t, srv, `{"query":{"match":{"text":"reciprocal rank fusion combines rankings"}}}`)
	}
	second := read()

	// Monotonic, all of them: a counter that can decrease makes the difference of
	// two reads meaningless, which is the whole arithmetic a rung is built from.
	for _, field := range []string{
		"total_alloc_in_bytes", "mallocs", "cpu_seconds", "gc_cpu_seconds",
		"gc_cycles", "gc_pause_total_in_nanos", "minor_faults", "max_rss_in_bytes",
	} {
		a, _ := first[field].(float64)
		b, _ := second[field].(float64)
		if b < a {
			t.Errorf("%s went backwards, %v then %v", field, a, b)
		}
	}

	// And two of them actually move, which is what says the route is reading this
	// process rather than a zero value. Only the allocation counters are asserted
	// on: `/cpu/classes/total:cpu-seconds` is sampled by the runtime and can still
	// read 0 across a workload this small, so requiring it to move would be a
	// flaky test rather than a stricter one.
	for _, field := range []string{"total_alloc_in_bytes", "mallocs"} {
		a, _ := first[field].(float64)
		b, _ := second[field].(float64)
		if b == a {
			t.Errorf("%s did not move across fifty searches (%v): the counter is not wired to this process", field, a)
		}
	}
}
