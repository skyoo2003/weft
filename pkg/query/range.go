// SPDX-License-Identifier: Apache-2.0

package query

import (
	"fmt"
	"strings"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
)

// MaxEditDistance bounds what Fuzzy will accept.
//
// Two, because past it the candidate set stops being "the word you meant" and
// becomes "words of about that length": at distance 3 every four-letter term
// matches every other. It is also what keeps the cost bearable — see Fuzzy.
const MaxEditDistance = 2

// EncodeInt returns a string that sorts lexicographically in the same order the
// number sorts numerically.
//
// This is what makes a numeric range query possible without a numeric index. A
// posting list is reached by a term, and terms are strings ordered as bytes, so
// a number stored as `"42"` sorts before `"7"` and a range over it is nonsense.
// Sixteen hex digits with the sign bit flipped fixes both halves: fixed width
// removes the short-string problem, and the flip puts negatives below positives
// where two's complement would have put them above.
//
// Hex rather than raw bytes because the result has to survive tokenization — the
// default Tokenizer keeps letters and digits and cuts everything else, so an
// encoding using punctuation would be split into pieces at index time and never
// looked up whole.
//
// The caller indexes it and the caller queries it, which is the whole contract:
//
//	d.Fields = append(d.Fields, engine.Field{Name: "price", Text: query.EncodeInt(cents)})
//	q := query.Range(ix, "price", query.EncodeInt(1000), query.EncodeInt(5000))
//
// A value encoded one way and queried another matches nothing with nothing to
// report, which is why this is a named function rather than a line of advice.
func EncodeInt(v int64) string {
	// A reinterpretation, not a conversion: every int64 bit pattern is a valid
	// uint64 one and the flip is what moves negatives below positives. There is
	// no value to overflow — the two types are the same 64 bits.
	return fmt.Sprintf("%016x", uint64(v)^(1<<63)) //nolint:gosec // see above
}

// EncodeTime is EncodeInt over nanoseconds since the epoch, so times index and
// range like any other number.
//
// Nanoseconds rather than seconds because a search index that cannot separate
// two events in the same second is answering a different question than the one
// asked. The cost is the window Go's UnixNano can express — **1678 to 2262** —
// and a time outside it encodes to a wrapped number that sorts wrongly. That is
// a real limit rather than a theoretical one for a corpus of historical
// documents, and such a corpus should encode `t.Unix()` with EncodeInt directly.
//
// This does **not** read Document.Time. That field is the recency scorer's, and
// reaching it means decoding a record per document — the corpus scan this whole
// approach exists to avoid. A caller wanting to range over time indexes it as a
// field with this function; that is one mechanism instead of two.
func EncodeTime(t time.Time) string { return EncodeInt(t.UnixNano()) }

// Range ranks documents whose named field holds a term in [lo, hi], compared as
// bytes.
//
// Bounds are inclusive and either may be empty for unbounded. `Range(ix,
// "price", EncodeInt(1000), "")` is "at least ten dollars"; both empty is every
// term the field has, which is a spelling of "this field is present".
//
// **Lexicographic, always.** It compares the terms as they were indexed, so a
// range means what the caller's encoding makes it mean. Use EncodeInt or
// EncodeTime for numbers and times; a range over ordinary words is a range over
// the alphabet, which is occasionally what somebody wants.
//
// An empty field is Document.Text, the same convention Glob and text.NewField
// use. See termScorer for what it scores and why it does not truncate to k —
// this is most often the restricting stream in a query.Must.
//
// # What it costs
//
// A scan of the field's vocabulary, bounded by MaxTerms, and then a posting-list
// walk per term inside the range. There is no numeric index and no ordered term
// structure. What that means in practice is that the cost scales with **how many
// distinct values the field holds**, not with how many documents hold them — so
// a field of a thousand price points is cheap however large the corpus, and a
// field holding a distinct nanosecond per document is not.
//
// ponytail: the `terms` section is sorted on disk and a range is an extent in
// it, so the seek exists in the bytes and not in the reader — the same note
// Index.Terms carries. Buying it is a reader change and not a format change.
func Range(ix *engine.Index, field, lo, hi string) engine.Scorer {
	return &termScorer{ix: ix, name: scorerName("range", field), pick: func() ([]string, error) {
		if lo != "" && hi != "" && lo > hi {
			// An inverted range matches nothing under any encoding, and a caller
			// who wrote one has swapped two arguments. Reporting it is the
			// difference between a typo found and a query that quietly returns
			// zero hits — the same judgement Glob makes about a malformed
			// pattern.
			return nil, fmt.Errorf("range [%q, %q]: the lower bound is above the upper", lo, hi)
		}
		// The shared prefix of the two bounds narrows the vocabulary scan for
		// free, and is exact for the common case: EncodeInt is fixed width, so
		// two nearby numbers agree on their leading digits. An unbounded side
		// gives an empty prefix, which is the whole field.
		cand, prefix := scopedTerms(ix, field, commonPrefix(lo, hi))
		out := make([]string, 0, len(cand))
		for _, term := range cand {
			v := strings.TrimPrefix(term, prefix)
			if (lo == "" || v >= lo) && (hi == "" || v <= hi) {
				out = append(out, term)
			}
		}
		return out, nil
	}}
}

// commonPrefix is the leading run both bounds share, which every term between
// them must also start with.
//
// Empty when either bound is, because an unbounded side admits terms sharing
// nothing with the other. It is a narrowing hint that is never wrong: a shorter
// prefix examines more terms and selects the same set.
func commonPrefix(lo, hi string) string {
	if lo == "" || hi == "" {
		return ""
	}
	n := min(len(lo), len(hi))
	i := 0
	for i < n && lo[i] == hi[i] {
		i++
	}
	return lo[:i]
}

// Fuzzy ranks documents holding a term within maxDist edits of term, in the
// named field.
//
// Edits are Levenshtein: insertion, deletion or substitution of one byte.
// maxDist must be between 1 and MaxEditDistance; anything else is an error from
// Candidates rather than a silently narrowed search.
//
// **Bytes, not runes.** A substitution inside a multi-byte character counts as
// several edits, so this is a poor match for Korean, Japanese or Chinese — where
// the useful notion of "one typo" is a syllable rather than a byte. It is
// accurate for the Latin text the default tokenizer was written for and it is
// honest about the rest; a rune-aware form is a different function, not a flag
// on this one.
//
// An empty field is Document.Text. See termScorer for what it scores and why it
// does not truncate to k.
//
// # What it costs
//
// A scan of the field's vocabulary with a length filter in front of it: a term
// whose length differs from the query's by more than maxDist cannot be within
// maxDist edits, and that check is a subtraction. What survives it pays a banded
// edit-distance computation, which is O(len × maxDist) and stops early once
// every cell in a row exceeds the budget.
//
// ponytail: still a vocabulary scan, and unlike Glob and Range there is no
// prefix to narrow it with — an edit in the first character is exactly what this
// is for. The structure that fixes it is a Levenshtein automaton intersected
// with an ordered term dictionary, which is a great deal of machinery and wants
// the ordered dictionary Index.Terms does not have yet.
func Fuzzy(ix *engine.Index, field, term string, maxDist int) engine.Scorer {
	return &termScorer{ix: ix, name: scorerName("fuzzy", field), pick: func() ([]string, error) {
		if maxDist < 1 || maxDist > MaxEditDistance {
			return nil, fmt.Errorf("fuzzy %q: edit distance %d is not in [1, %d]", term, maxDist, MaxEditDistance)
		}
		if term == "" {
			return nil, nil
		}
		cand, prefix := scopedTerms(ix, field, "")
		out := make([]string, 0, 16)
		for _, t := range cand {
			v := strings.TrimPrefix(t, prefix)
			// The length filter is not an optimisation that could be dropped: it
			// is what keeps the quadratic part off the terms that cannot match,
			// which on a real vocabulary is nearly all of them.
			if abs(len(v)-len(term)) > maxDist {
				continue
			}
			if withinEdits(v, term, maxDist) {
				out = append(out, t)
			}
		}
		return out, nil
	}}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// withinEdits reports whether a and b are within maxDist Levenshtein edits.
//
// One row of the matrix rather than the whole of it, because the answer needs
// only the previous row — the difference is O(len) memory against O(len²) for a
// vocabulary scan that runs this once per candidate term.
//
// The early exit is what makes the bound real: if every cell of a row already
// exceeds maxDist, no later row can come back under it, since a row's minimum
// never decreases. Without it a long term pays the full product to discover a
// distance nobody asked the size of.
func withinEdits(a, b string, maxDist int) bool {
	if a == b {
		return true
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		best := curr[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, min(curr[j-1]+1, prev[j-1]+cost))
			best = min(best, curr[j])
		}
		if best > maxDist {
			return false
		}
		prev, curr = curr, prev
	}
	return prev[len(b)] <= maxDist
}
