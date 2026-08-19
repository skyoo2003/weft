// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// Progress says how far a rung has got, without becoming part of what the rung
// measures.
//
// The problem it solves is small and the constraint on it is not. A headline rung is
// 10,000 requests at 3.41 q/s — forty-nine minutes during which `bench` prints
// nothing and looks hung (docs/FINDINGS.md milestone 5 §4.3). Milestone 7 runs that
// rung several times over, so the silence is now worth a line.
//
// The line cannot be printed from the request. `do` runs on the goroutine Drive
// spawned for that request, and everything it does is inside the window Sample.Lat
// measures — a write to stdout included, along with any wait on the lock behind it.
// A p99 is the hundredth slowest sample of ten thousand, so printing every thousandth
// request contaminates exactly ten samples in the region the milestone publishes.
// This is milestone 5 §4.1 in a different place: an instrument that makes the thing
// it measures is not one.
//
// So the two halves are split by which goroutine they run on:
//
//	Count    wraps the request. One atomic add, no allocation, no I/O.
//	Report   runs a ticker on its own goroutine and does all the writing.
//
// The zero value is ready to use.
type Progress struct {
	done atomic.Int64
}

// ProgressEvery is how often a running rung says how far it has got.
//
// Thirty seconds, against a headline rung of forty-nine minutes: often enough that an
// operator can tell a running rung from a hung one, rare enough that the reporting
// goroutine's own allocations are nothing beside a query's 43.6 MiB.
//
// Here rather than once per command, for the reason the package doc gives for sharing
// the driver at all: the comparison is two commands over one instrument, and a
// cadence that differed between them would be one more thing not matched. Two
// spellings of one figure is how one of them comes to say something the other does
// not.
const ProgressEvery = 30 * time.Second

// Count wraps do so completions are counted, and does nothing else.
//
// The add is after do returns rather than before it: what the line reports is
// requests finished, which is the number that says whether the rung is moving. In
// flight is not recoverable from it and is not what the reader is asking.
func (p *Progress) Count(do func(int)) func(int) {
	return func(i int) {
		do(i)
		p.done.Add(1)
	}
}

// Report writes a progress line to w every `every` until the returned stop is
// called, and returns a stop that does not come back until the writing has ended.
//
// Synchronous stop is the point of the signature. The caller's next act is printing
// the rung's own distribution table, and a reporting goroutine still alive at that
// moment lands a progress line in the middle of it — a table a reader has to re-read
// before trusting. Closing a channel and returning would not be enough: the tick
// already elected still writes.
//
// n is the rung's request count, carried so the line can say how far through it is.
// A non-positive `every` disables reporting and returns a stop that is still safe to
// call, so a caller can switch this off without branching around the defer.
func (p *Progress) Report(w io.Writer, n int, every time.Duration) (stop func()) {
	if every <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	ended := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(ended)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				// Errors ignored deliberately: this is a progress line to a terminal.
				// A failing write is not a reason to stop a ninety-minute rung, and
				// reporting it would mean writing to the writer that just failed.
				fmt.Fprintf(w, "  ... %d/%d done, %v elapsed\n", //nolint:errcheck // see above
					p.done.Load(), n, time.Since(start).Round(time.Second))
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		<-ended
	}
}
