// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/skyoo2003/weft/internal/eval"
	"github.com/skyoo2003/weft/internal/loadgen"
)

// Milestone 27: the same ladder, aimed through a socket.
//
// The question is not whether weftd answers a search — the Go tests and
// `make compat` settle that. It is what the 27 q/s collapse published in
// docs/PERF.md looks like when the collapsing process is not the one holding the
// stopwatch. Everything about the measurement design carries over unchanged: the
// open loop, the rung structure, the saturation rule, the suspension tolerance.
//
// # The one thing that does not carry over, and it decides whether the run means anything
//
// Every memory and collector number in a benchReport is read from *this* process.
// Over HTTP this process is the load generator: it holds the in-flight requests
// and no index at all. Printing its resident set in the RSS column would be a
// number that is precise, stable, reproducible and about the wrong program —
// which is worse than no number, because it looks like one.
//
// So an HTTP run reads the server's own counters through `GET /_nodes/stats`,
// from the same instruments internal/loadgen reads in-process. If that read
// fails, the run **fails** rather than falling back to the local counters: a
// silent fallback here would produce a report that is wrong in exactly the way
// nobody checks for.

// httpTarget is a running weftd the ladder can drive.
type httpTarget struct {
	base   string
	index  string
	client *http.Client
	// k is the same frozen k the in-process arm uses, so both arms ask for the
	// same amount of work.
	k int
}

// newHTTPTarget validates the address and the index before the ladder starts.
//
// Before, not during: a typo in the address discovered on the first request of a
// three-hour run is three hours spent to learn that a URL was wrong, and the run
// it spent was the one that had the quiet machine.
func newHTTPTarget(ctx context.Context, addr, index string, k int) (*httpTarget, error) {
	base := addr
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("-http=%q is not an address: %w", addr, err)
	}
	t := &httpTarget{
		base:  strings.TrimRight(base, "/"),
		index: index,
		k:     k,
		client: &http.Client{
			// No timeout. One would silently turn the collapse this milestone
			// exists to measure into a shed request, and shed is already counted
			// for a different reason — loadgen.Drive sheds when the in-flight cap
			// binds, which is a property of the load and not of the server. Two
			// causes in one counter is one cause lost.
			Transport: &http.Transport{
				// One connection per in-flight request, which is what an open
				// loop means. The default cap of two idle connections per host
				// would queue requests inside the client and then report the
				// client's queue as the server's latency.
				MaxIdleConnsPerHost: 4096,
			},
		},
	}

	// The handshake, so a wrong address fails as a wrong address rather than as a
	// 404 on the search route.
	if _, err := t.get(ctx, "/"); err != nil {
		return nil, fmt.Errorf("weftd is not answering at %s: %w", t.base, err)
	}
	// And the index, so a missing one fails here rather than as ten thousand 404s
	// the ladder would happily time.
	if _, err := t.get(ctx, "/"+index+"/_search"); err != nil {
		return nil, fmt.Errorf("index %q is not searchable at %s: %w", index, t.base, err)
	}
	if _, err := t.stats(ctx); err != nil {
		return nil, fmt.Errorf("%s does not answer GET /_nodes/stats, so a run against it could only report "+
			"this process's memory as though it were the server's: %w", t.base, err)
	}
	return t, nil
}

func (t *httpTarget) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base+path, http.NoBody)
	if err != nil {
		return nil, err
	}
	return t.do(req)
}

func (t *httpTarget) do(req *http.Request) ([]byte, error) {
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // the status below is what matters
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: status %d: %s", req.Method, req.URL.Path, resp.StatusCode, trimTo(body, 200))
	}
	return body, nil
}

func trimTo(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// searchBody is the request one replayed query becomes.
//
// A `match` over the query text, which is the closest HTTP equivalent of what the
// in-process `text` arm runs. It is **not identical** — the in-process arm scores
// with scorer/text's BM25 and this one fuses the server's term scorers — and that
// difference is published rather than papered over. What the two share is the
// shape of the work: a vocabulary lookup and a posting walk per token, then a
// fusion, then a top-k.
func (t *httpTarget) searchBody(q eval.Query) ([]byte, error) {
	return json.Marshal(map[string]any{
		"size":  t.k,
		"query": map[string]any{"match": map[string]any{"text": q.Query.Text}},
	})
}

// driver returns the function the ladder drives, in the shape benchDo returns one.
func (t *httpTarget) driver(ctx context.Context, qs []eval.Query, failed *atomic.Int64) (func(int), error) {
	// Bodies are built once, before the run. Marshalling inside the driven
	// function would charge every rung with this process's JSON encoding and put
	// it inside the latency the server is being measured by.
	bodies := make([][]byte, len(qs))
	for i, q := range qs {
		b, err := t.searchBody(q)
		if err != nil {
			return nil, fmt.Errorf("build the search body for query %d: %w", i, err)
		}
		bodies[i] = b
	}
	path := t.base + "/" + t.index + "/_search"

	return func(i int) {
		body := bodies[i%len(bodies)]
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
		if err != nil {
			failed.Add(1)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if _, err := t.do(req); err != nil && ctx.Err() == nil {
			failed.Add(1)
		}
	}, nil
}

// procStats is one instant's reading of every counter a benchReport needs.
//
// Deliberately the same set on both arms, so benchRung's arithmetic — every field
// a difference of two reads except the resident set, which is a high-water mark —
// is the same arithmetic whichever process it came from.
type procStats struct {
	maxRSS     int64
	faults     loadgen.FaultCounts
	gcCycles   uint64
	pause      time.Duration
	gcCPU      float64
	cpu        float64
	totalAlloc uint64
	mallocs    uint64
	heapAlloc  uint64
	goroutines int
}

// stats reads GET /_nodes/stats.
func (t *httpTarget) stats(ctx context.Context) (procStats, error) {
	raw, err := t.get(ctx, "/_nodes/stats")
	if err != nil {
		return procStats{}, err
	}
	var doc struct {
		Nodes map[string]struct {
			Weft struct {
				MaxRSSBytes   int64   `json:"max_rss_in_bytes"`
				MinorFaults   int64   `json:"minor_faults"`
				MajorFaults   int64   `json:"major_faults"`
				GCCycles      uint64  `json:"gc_cycles"`
				GCPauseNanos  int64   `json:"gc_pause_total_in_nanos"`
				GCCPUSeconds  float64 `json:"gc_cpu_seconds"`
				CPUSeconds    float64 `json:"cpu_seconds"`
				TotalAlloc    uint64  `json:"total_alloc_in_bytes"`
				Mallocs       uint64  `json:"mallocs"`
				HeapAllocated uint64  `json:"heap_alloc_in_bytes"`
				Goroutines    int     `json:"goroutines"`
			} `json:"weft"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return procStats{}, fmt.Errorf("decode _nodes/stats: %w", err)
	}
	for _, n := range doc.Nodes {
		return procStats{
			maxRSS:     n.Weft.MaxRSSBytes,
			faults:     loadgen.FaultCounts{Minor: n.Weft.MinorFaults, Major: n.Weft.MajorFaults},
			gcCycles:   n.Weft.GCCycles,
			pause:      time.Duration(n.Weft.GCPauseNanos),
			gcCPU:      n.Weft.GCCPUSeconds,
			cpu:        n.Weft.CPUSeconds,
			totalAlloc: n.Weft.TotalAlloc,
			mallocs:    n.Weft.Mallocs,
			heapAlloc:  n.Weft.HeapAllocated,
			goroutines: n.Weft.Goroutines,
		}, nil
	}
	return procStats{}, fmt.Errorf("_nodes/stats named no node: %s", trimTo(raw, 200))
}

// counters is where a rung's memory and collector figures come from.
//
// Two implementations: the local one reads this process and the HTTP one reads
// the server's. benchRung takes the interface rather than branching, so there is
// exactly one place that decides which process a report is about, and it is not
// inside the measurement.
type counters interface {
	// snapshot reads every counter at one instant. An error is fatal to the run —
	// see this file's opening comment for why there is no fallback.
	snapshot() (procStats, error)
	// subject names the process the numbers describe, for the report header.
	subject() string
}

// localCounters is the behaviour every milestone before this one had: the ladder
// and the index are in one process, so this process's counters are the index's.
type localCounters struct{}

func (localCounters) subject() string { return "in-process" }

func (localCounters) snapshot() (procStats, error) {
	faults := loadgen.ProcFaults()
	gcCPU, cpu := loadgen.GCCPUSeconds()
	// Last, because it is the only read here that stops the world: the cycle,
	// pause and fault counters above would otherwise be charged with this
	// instrument's own stop. Same ordering rule benchRung states.
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	return procStats{
		maxRSS:     loadgen.MaxRSS(),
		faults:     faults,
		gcCycles:   loadgen.GCCycles(),
		pause:      loadgen.GCPauseTotal(),
		gcCPU:      gcCPU,
		cpu:        cpu,
		totalAlloc: mem.TotalAlloc,
		mallocs:    mem.Mallocs,
		heapAlloc:  mem.HeapAlloc,
		goroutines: runtime.NumGoroutine(),
	}, nil
}

// httpCounters reads the server being driven.
type httpCounters struct {
	ctx context.Context
	t   *httpTarget
}

func (h httpCounters) subject() string { return "weftd at " + h.t.base }

func (h httpCounters) snapshot() (procStats, error) { return h.t.stats(h.ctx) }
