// SPDX-License-Identifier: Apache-2.0

package query_test

import (
	"context"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/query"
)

// priced indexes each key with its value encoded into a "price" field, which is
// the pattern Range documents: the caller encodes, the caller queries, and
// EncodeInt is the seam that keeps the two spellings the same.
func priced(t *testing.T, prices map[string]int64) *engine.Index {
	t.Helper()
	ix := engine.New()
	names := make([]string, 0, len(prices))
	for k := range prices {
		names = append(names, k)
	}
	slices.Sort(names) // DocIDs in a stable order, so a failure reads the same twice.
	for _, k := range names {
		d := engine.Document{Key: k, Text: "item",
			Fields: []engine.Field{{Name: "price", Text: query.EncodeInt(prices[k])}}}
		if _, err := ix.Add(d); err != nil {
			t.Fatalf("Add(%q): %v", k, err)
		}
	}
	return ix
}

// TestEncodeIntSortsLikeANumber is the property everything else here rests on.
// A posting list is reached by a term and terms order as bytes, so an encoding
// that does not agree with the numbers makes every range answer wrongly rather
// than fail.
func TestEncodeIntSortsLikeANumber(t *testing.T) {
	vals := []int64{math.MinInt64, -1 << 40, -257, -256, -1, 0, 1, 255, 256, 1 << 40, math.MaxInt64}
	for i := 1; i < len(vals); i++ {
		lo, hi := query.EncodeInt(vals[i-1]), query.EncodeInt(vals[i])
		if lo >= hi {
			t.Errorf("EncodeInt(%d) = %q is not below EncodeInt(%d) = %q", vals[i-1], lo, vals[i], hi)
		}
		if len(lo) != len(hi) {
			t.Errorf("EncodeInt is not fixed width: %q and %q", lo, hi)
		}
	}
	// It has to survive the default tokenizer, or a value is split at index time
	// and never looked up whole.
	for _, v := range vals {
		if got := engine.Tokenize(query.EncodeInt(v)); len(got) != 1 || got[0] != query.EncodeInt(v) {
			t.Errorf("EncodeInt(%d) tokenizes to %v, want one token", v, got)
		}
	}
}

func TestEncodeTimeSortsLikeATime(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	prev := query.EncodeTime(base.Add(-24 * time.Hour))
	for _, d := range []time.Duration{0, time.Nanosecond, time.Second, 24 * time.Hour, 365 * 24 * time.Hour} {
		got := query.EncodeTime(base.Add(d))
		if got <= prev {
			t.Errorf("EncodeTime at +%v = %q, not above the previous %q", d, got, prev)
		}
		prev = got
	}
}

func TestRangeSelectsByValueNotByString(t *testing.T) {
	ix := priced(t, map[string]int64{
		"cheap": 7, "mid": 42, "dear": 500, "free": 0, "owed": -100,
	})
	for _, tc := range []struct {
		name   string
		lo, hi string
		want   []string
	}{
		{"closed", query.EncodeInt(7), query.EncodeInt(500), []string{"cheap", "mid", "dear"}},
		{"inclusive at both ends", query.EncodeInt(42), query.EncodeInt(42), []string{"mid"}},
		{"unbounded above", query.EncodeInt(42), "", []string{"mid", "dear"}},
		{"unbounded below", "", query.EncodeInt(7), []string{"owed", "free", "cheap"}},
		{"negatives sort below zero", "", query.EncodeInt(-1), []string{"owed"}},
		{"both unbounded is every value", "", "", []string{"owed", "free", "cheap", "mid", "dear"}},
		{"empty range", query.EncodeInt(100), query.EncodeInt(200), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sorted(keys(t, ix, candidates(t, query.Range(ix, "price", tc.lo, tc.hi), engine.Query{}, 100)))
			if !slices.Equal(got, sorted(tc.want)) {
				t.Errorf("Range = %v, want %v", got, sorted(tc.want))
			}
		})
	}
}

// TestRangeReportsAnInvertedRange. Two swapped arguments match nothing under
// every encoding, so reporting it is the difference between a typo found and a
// query that quietly returns zero hits.
func TestRangeReportsAnInvertedRange(t *testing.T) {
	ix := priced(t, map[string]int64{"a": 1})
	_, err := query.Range(ix, "price", query.EncodeInt(100), query.EncodeInt(1)).
		Candidates(context.Background(), engine.Query{}, 10)
	if err == nil {
		t.Error("an inverted range returned no error")
	}
}

// TestRangeIsScopedToItsField. A range over "price" must not see a value indexed
// under "weight", or the encoding's whole point — that the terms are comparable
// — quietly stops holding.
func TestRangeIsScopedToItsField(t *testing.T) {
	ix := engine.New()
	if _, err := ix.Add(engine.Document{Key: "a", Text: "item", Fields: []engine.Field{
		{Name: "price", Text: query.EncodeInt(10)},
		{Name: "weight", Text: query.EncodeInt(500)},
	}}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := candidates(t, query.Range(ix, "price", query.EncodeInt(400), query.EncodeInt(600)),
		engine.Query{}, 10); len(got) != 0 {
		t.Errorf("a price range matched the weight: %v", got)
	}
	if got := candidates(t, query.Range(ix, "weight", query.EncodeInt(400), query.EncodeInt(600)),
		engine.Query{}, 10); len(got) != 1 {
		t.Errorf("the weight range matched %v, want the document", got)
	}
}

// TestRangeComposesAsARestriction is the shape a range is usually used in: it
// narrows another scorer rather than ranking on its own.
func TestRangeComposesAsARestriction(t *testing.T) {
	ix := engine.New()
	for _, d := range []struct {
		key   string
		text  string
		price int64
	}{
		{"cheap-match", "widget", 10},
		{"dear-match", "widget", 900},
		{"cheap-miss", "gadget", 20},
	} {
		if _, err := ix.Add(engine.Document{Key: d.key, Text: d.text,
			Fields: []engine.Field{{Name: "price", Text: query.EncodeInt(d.price)}}}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	got, err := engine.Search(context.Background(), engine.Query{}, 10,
		// Both streams required: rank fusion is a union, so restricting on the
		// range alone would let a cheap gadget in on the range's own vote.
		query.Must(fusion.Fuse, 0, 1),
		query.Glob(ix, "", "widget"),
		query.Range(ix, "price", "", query.EncodeInt(100)))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if names := keys(t, ix, got); !slices.Equal(names, []string{"cheap-match"}) {
		t.Errorf("got %v, want [cheap-match] — a widget under a dollar", names)
	}
}

func TestFuzzyFindsWhatWasMeant(t *testing.T) {
	ix := corpus(t,
		"exact", "receive",
		"one-sub", "receeve",
		// A transposition, which plain Levenshtein counts as two substitutions —
		// so it is out at distance 1 and in at 2. Spelled wrong on purpose.
		"one-del", "recieve", //nolint:misspell // the typo is the fixture
		"one-ins", "receivee",
		"far", "retrieve",
		"unrelated", "banana",
	)
	for _, tc := range []struct {
		dist int
		want []string
	}{
		{1, []string{"exact", "one-sub", "one-ins"}},
		// "far" is "retrieve", which is 3 edits from "receive" and stays out at 2 —
		// the case that says the bound is a bound rather than a suggestion.
		{2, []string{"exact", "one-sub", "one-del", "one-ins"}},
	} {
		t.Run(fmt.Sprintf("distance %d", tc.dist), func(t *testing.T) {
			got := sorted(keys(t, ix, candidates(t, query.Fuzzy(ix, "", "receive", tc.dist), engine.Query{}, 100)))
			if !slices.Equal(got, sorted(tc.want)) {
				t.Errorf("Fuzzy = %v, want %v", got, sorted(tc.want))
			}
		})
	}
}

// TestFuzzyRefusesADistanceItCannotHonour. Past MaxEditDistance the candidate
// set stops being "the word you meant" and becomes "words of about that
// length", so the bound is refused rather than silently applied.
func TestFuzzyRefusesADistanceItCannotHonour(t *testing.T) {
	ix := corpus(t, "a", "alpha")
	for _, dist := range []int{0, -1, query.MaxEditDistance + 1} {
		_, err := query.Fuzzy(ix, "", "alpha", dist).Candidates(context.Background(), engine.Query{}, 10)
		if err == nil {
			t.Errorf("Fuzzy accepted a distance of %d", dist)
		}
	}
}

// TestFuzzyAndGlobScopeToAField, so a caller never has to know that a field's
// terms are spelled with a separator — which is the wart milestone 18's findings
// recorded as carried forward.
func TestFuzzyAndGlobScopeToAField(t *testing.T) {
	ix := engine.New()
	if _, err := ix.Add(engine.Document{Key: "a", Text: "banana", Fields: []engine.Field{
		{Name: "title", Text: "receive"},
	}}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := candidates(t, query.Fuzzy(ix, "title", "receeve", 1), engine.Query{}, 10); len(got) != 1 {
		t.Errorf("a fuzzy title query matched %v, want the document", got)
	}
	if got := candidates(t, query.Fuzzy(ix, "", "receeve", 1), engine.Query{}, 10); len(got) != 0 {
		t.Errorf("a fuzzy Text query matched %v; the word is only in the title", got)
	}
	if got := candidates(t, query.Glob(ix, "title", "rec*"), engine.Query{}, 10); len(got) != 1 {
		t.Errorf("a glob title query matched %v, want the document", got)
	}
	if got := candidates(t, query.Glob(ix, "title", "ban*"), engine.Query{}, 10); len(got) != 0 {
		t.Errorf("a glob title query matched %v; the word is only in Text", got)
	}
}

// TestScopedScorersNameTheirField, so a caller fusing several can tell the
// streams apart in a diagnostic — the same convention scorer/text uses.
func TestScopedScorersNameTheirField(t *testing.T) {
	ix := corpus(t, "a", "alpha")
	for _, tc := range []struct{ got, want string }{
		{query.Glob(ix, "", "a*").Name(), "glob"},
		{query.Glob(ix, "title", "a*").Name(), "glob:title"},
		{query.Range(ix, "price", "", "").Name(), "range:price"},
		{query.Fuzzy(ix, "author", "kim", 1).Name(), "fuzzy:author"},
		{query.Phrase(ix, query.Glob(ix, "", "a*"), "alpha").Name(), "phrase"},
	} {
		if tc.got != tc.want {
			t.Errorf("Name() = %q, want %q", tc.got, tc.want)
		}
	}
}
