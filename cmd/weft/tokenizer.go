// SPDX-License-Identifier: Apache-2.0

package main

import (
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/skyoo2003/weft/pkg/engine"
)

// tokenizers are the tokenizers a command can select by name.
//
// They live here rather than in pkg/ on purpose. engine.WithTokenizer takes a
// function, and a function is not something a shell can pass — so a command that
// wants to reach that replacement point has to bring its own table of choices.
// Putting the table in the library instead would mean the library shipping a
// tokenizer policy, which is the thing milestone 13 deliberately did not do:
// engine has one default and one seam, and what goes through the seam is the
// caller's business.
//
// The alternatives are here to be *different*, not to be good. `whitespace`
// keeps the punctuation the default drops; `bigram` is the shape a language the
// default cannot segment needs — milestone 13's Korean query returned nothing
// under the default tokenizer and something under a replacement, and this is the
// smallest replacement with that property. Neither is a recommendation. A corpus
// that needs real segmentation needs a real segmenter, and weft does not have
// one.
var tokenizers = map[string]engine.Tokenizer{
	// The default, named so that `-tokenizer default` is something a script can
	// say rather than a flag it has to omit. It is engine's own exported
	// function, so selecting it commits an index no different from one committed
	// without the option at all.
	"default":    engine.Tokenize,
	"whitespace": strings.Fields,
	"bigram":     bigrams,
}

// defaultTokenizer is the name -tokenizer starts at.
const defaultTokenizer = "default"

// tokenizerNames is the sorted list, for an error that tells a caller what they
// could have typed instead.
func tokenizerNames() []string {
	return slices.Sorted(maps.Keys(tokenizers))
}

// bigrams splits at anything that is not a letter or a digit, then cuts each run
// into overlapping pairs of runes.
//
// Overlapping, because a language written without spaces gives a tokenizer no
// word boundary to find: 검색엔진 is one term under the default tokenizer, and a
// query for 엔진 then matches nothing while reporting nothing. Pairs that
// straddle every position mean a query for any two adjacent characters finds the
// document holding them.
//
// What it costs, stated rather than hidden: a run of n runes becomes n−1 terms
// instead of 1, so this inflates the term space and every document's length, and
// BM25's length normalization then sees a corpus of long documents. A run of one
// rune is emitted whole, because there is no pair to make of it.
func bigrams(s string) []string {
	var out []string
	for _, run := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		runes := []rune(run)
		if len(runes) == 1 {
			out = append(out, run)
			continue
		}
		for i := 0; i+1 < len(runes); i++ {
			out = append(out, string(runes[i:i+2]))
		}
	}
	return out
}
