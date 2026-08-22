// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// A rung is silent for forty-nine minutes (FINDINGS milestone 5 §4.3), and
// milestone 7 runs that rung repeatedly. What follows is the progress line, and the
// assertions are all about the same thing: the line must not become part of what is
// being measured.
//
// This is the trap milestone 5 §4.1 already fell into once. GCPauseTotal allocated a
// 163-bucket histogram per call, made ~150 MiB of garbage per ladder, and the report
// charged the collections that garbage provoked to the query. The obvious progress
// design — print from inside `do` every thousand requests — is the same mistake in a
// different place: that write happens on the request goroutine, lands in Sample.Lat,
// and a p99 is the hundredth slowest sample of ten thousand. Ten contaminated
// samples are enough to reach it.
//
// So the split under test is: counting happens in the request path and costs an
// atomic add; writing happens on a goroutine that owns no request.

// syncBuf is a writer the reporting goroutine and the test can both touch. The suite
// runs under -race, so an unsynchronised bytes.Buffer here would fail the detector
// rather than the assertion, and the failure would name the test instead of the code.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuf) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// TestProgressCountingDoesNotAllocateInTheRequestPath is the assertion the design
// exists for, in the same form the package already applies to GCPauseTotal.
//
// An allocation per request is not free at the scale this runs: a ladder is 50,000
// requests, and anything allocated on the request path enlarges the live heap the
// collector is being measured against.
func TestProgressCountingDoesNotAllocateInTheRequestPath(t *testing.T) {
	var p Progress
	do := p.Count(func(int) {})
	if n := testing.AllocsPerRun(100, func() { do(0) }); n > 0 {
		t.Errorf("the counted request allocates %.0f times per call; progress reporting "+
			"is enlarging the heap it is meant to observe", n)
	}
}

// TestProgressCountingWritesNothingFromTheRequestPath pins the whole point: the
// request path counts, it does not report.
//
// Without Report running there is nothing to print, and with it running the writes
// come from a goroutine that is not serving a request. Either way, calling the
// wrapped function must not itself write — that write would be inside the latency
// the sample records.
func TestProgressCountingWritesNothingFromTheRequestPath(t *testing.T) {
	var w syncBuf
	var p Progress
	do := p.Count(func(int) {})
	for i := range 5000 {
		do(i)
	}
	if got := w.String(); got != "" {
		t.Errorf("the request path wrote %q; every byte of that is inside a measured latency", got)
	}
}

// TestProgressReportsCompletionsWhileTheRungRuns is the feature: a line appears
// while the rung is still running, carrying how far it has got.
//
// Ten milliseconds rather than the thirty seconds the command uses. The interval is
// a parameter for exactly this reason — a test that waited for a real reporting
// interval would be a test nobody runs.
func TestProgressReportsCompletionsWhileTheRungRuns(t *testing.T) {
	var w syncBuf
	var p Progress
	do := p.Count(func(int) {})
	for i := range 300 {
		do(i)
	}

	stop := p.Report(&w, 1000, 10*time.Millisecond)
	// Polled rather than slept-then-asserted. A fixed sleep either flakes on a loaded
	// machine or wastes the time it was padded with, and this suite runs on the same
	// host the ladder does.
	deadline := time.Now().Add(2 * time.Second)
	for w.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stop()

	got := w.String()
	if got == "" {
		t.Fatal("nothing was reported in two seconds; a rung would still look hung")
	}
	if !strings.Contains(got, "300") || !strings.Contains(got, "1000") {
		t.Errorf("reported %q; the line has to carry the completed count and the total, "+
			"or it does not answer the question it exists for", got)
	}
}

// TestProgressStopWaitsSoNothingWritesAfterIt is why stop is synchronous.
//
// The caller's next act is printing the rung's own report. A reporting goroutine
// still alive at that moment interleaves a progress line into the distribution
// table, and a table with a stray line in it is one a reader has to re-read to
// trust. Closing a channel and returning is not enough: the tick already in flight
// still writes.
func TestProgressStopWaitsSoNothingWritesAfterIt(t *testing.T) {
	var w syncBuf
	var p Progress
	do := p.Count(func(int) {})
	do(0)

	stop := p.Report(&w, 10, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	stop()

	after := w.Len()
	time.Sleep(50 * time.Millisecond)
	if now := w.Len(); now != after {
		t.Errorf("%d bytes were written after stop returned; the reporter outlived the "+
			"rung and its next line lands inside the report", now-after)
	}
}

// TestProgressReportingCanBeSwitchedOffWithoutPanicking covers the guard rather than
// a feature, and the guard is not decoration: time.NewTicker panics on a
// non-positive duration. A caller that wants a silent rung passes zero, and the
// alternative — branching around the defer at each call site — is how one of two call
// sites comes to leak the goroutine the other stops.
func TestProgressReportingCanBeSwitchedOffWithoutPanicking(t *testing.T) {
	var w syncBuf
	var p Progress
	stop := p.Report(&w, 10, 0)
	stop()
	if w.Len() != 0 {
		t.Errorf("a switched-off reporter wrote %q", w.String())
	}
}

// TestProgressCountsEveryConcurrentCompletion pins the counter under the shape Drive
// actually calls it: one goroutine per in-flight request, up to four per core.
//
// Under -race this also asserts the counter is not a plain int, which is the version
// that would pass every other test in this file.
func TestProgressCountsEveryConcurrentCompletion(t *testing.T) {
	var w syncBuf
	var p Progress
	do := p.Count(func(int) {})

	const workers, each = 16, 500
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				do(i)
			}
		}()
	}
	wg.Wait()

	stop := p.Report(&w, workers*each, time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for w.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stop()

	if want := "8000"; !strings.Contains(w.String(), want) {
		t.Errorf("reported %q, want a count of %s: completions were dropped", w.String(), want)
	}
}
