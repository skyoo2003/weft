// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package main

// maxRSS reports 0 where getrusage does not exist.
//
// Zero rather than an error, and rather than a different metric. It sits beside
// the working-set figures the recall report derives itself, and those are the
// measurement; this one is the OS's second opinion on them. A platform that
// cannot offer it should print nothing there rather than a number from a
// different definition that a reader would compare against the Linux figures in
// docs/EVAL.md.
//
// The same split as pkg/engine's mmap: unix does the real thing, everything else
// stays buildable.
func maxRSS() int64 { return 0 }

// procFaults reports nothing where getrusage does not exist, for the reason
// above: a zero prints as absent, and absent is the honest answer.
//
// The bench report reads this to decide whether a tail is storage or scheduling.
// On a platform that cannot answer, that question stays open rather than being
// answered from a fabricated zero — the report prints the counters only when they
// moved, so a run here shows a latency distribution and no fault line.
func procFaults() procFaultCounts { return procFaultCounts{} }
