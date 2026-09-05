// SPDX-License-Identifier: Apache-2.0

package query

import "github.com/skyoo2003/weft/pkg/engine"

// Must returns a Fuser that drops every document missing from any of the named
// streams, then hands what is left to fuse.
//
// The named streams keep their votes. That is a conjunction in the sense bleve's
// `must` means it — a required clause still contributes to the score — and not a
// filter that only narrows. To restrict without voting, give that position a
// weight of 0 with fusion.FuseWeighted.
//
// Composes with itself and with MustNot, innermost first:
//
//	query.Must(query.MustNot(fusion.Fuse, 2), 0, 1)
//
// **A stream index past the end of the list drops everything.** A restriction
// that cannot be evaluated is not satisfied, so a mis-wired call answers with
// nothing rather than answering as though the constraint were not there. That is
// the same rule the segment readers hold — absence beats a plausible wrong
// answer — and it is the direction that gets noticed.
//
// No streams named is fuse unchanged, which is what makes this safe to build
// from a list assembled at runtime.
func Must(fuse engine.Fuser, streams ...int) engine.Fuser {
	if len(streams) == 0 {
		return fuse
	}
	return func(all [][]engine.Candidate, k int) []engine.Candidate {
		keep, ok := intersect(all, streams)
		if !ok {
			return nil
		}
		return fuse(retain(all, keep), k)
	}
}

// MustNot returns a Fuser that drops every document appearing in any of the
// named streams, then hands what is left to fuse.
//
// The excluded streams are removed from the fusion input as well. A stream whose
// whole purpose is to name documents that must not appear has no ranking to
// contribute, and leaving it in would give every document it names a vote in the
// result it is being kept out of.
//
// **A stream index past the end of the list drops everything**, for the reason
// Must gives: a constraint that cannot be evaluated is not satisfied. It is the
// louder of the two available wrong answers.
func MustNot(fuse engine.Fuser, streams ...int) engine.Fuser {
	if len(streams) == 0 {
		return fuse
	}
	return func(all [][]engine.Candidate, k int) []engine.Candidate {
		if !inRange(all, streams) {
			return nil
		}
		banned := make(map[engine.DocID]struct{})
		for _, s := range streams {
			for _, c := range all[s] {
				banned[c.Doc] = struct{}{}
			}
		}
		kept := make([][]engine.Candidate, 0, len(all))
		for i, stream := range all {
			if contains(streams, i) {
				continue
			}
			kept = append(kept, without(stream, banned))
		}
		return fuse(kept, k)
	}
}

// intersect is the set of documents present in every named stream, and false
// when a named stream does not exist.
func intersect(all [][]engine.Candidate, streams []int) (map[engine.DocID]struct{}, bool) {
	if !inRange(all, streams) {
		return nil, false
	}
	// Seeded from the first named stream rather than from the union, so the
	// working set is bounded by a constraint rather than by the corpus. Which
	// constraint depends on the caller's order and this does not reorder them: a
	// Fuser that sorted its own inputs would make the stream indexes above mean
	// something other than what the caller wrote.
	keep := make(map[engine.DocID]struct{}, len(all[streams[0]]))
	for _, c := range all[streams[0]] {
		keep[c.Doc] = struct{}{}
	}
	for _, s := range streams[1:] {
		next := make(map[engine.DocID]struct{}, len(keep))
		for _, c := range all[s] {
			if _, ok := keep[c.Doc]; ok {
				next[c.Doc] = struct{}{}
			}
		}
		keep = next
		if len(keep) == 0 {
			break
		}
	}
	return keep, true
}

// retain rebuilds the streams with only the documents in keep.
//
// Every stream is rebuilt, including the ones that named the constraint: a
// document failing it has to disappear from the fusion entirely, and leaving it
// in one stream would let that stream's vote carry it back into the result.
func retain(all [][]engine.Candidate, keep map[engine.DocID]struct{}) [][]engine.Candidate {
	out := make([][]engine.Candidate, len(all))
	for i, stream := range all {
		filtered := make([]engine.Candidate, 0, len(stream))
		for _, c := range stream {
			if _, ok := keep[c.Doc]; ok {
				filtered = append(filtered, c)
			}
		}
		out[i] = filtered
	}
	return out
}

func without(stream []engine.Candidate, banned map[engine.DocID]struct{}) []engine.Candidate {
	out := make([]engine.Candidate, 0, len(stream))
	for _, c := range stream {
		if _, bad := banned[c.Doc]; !bad {
			out = append(out, c)
		}
	}
	return out
}

func inRange(all [][]engine.Candidate, streams []int) bool {
	for _, s := range streams {
		if s < 0 || s >= len(all) {
			return false
		}
	}
	return true
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
