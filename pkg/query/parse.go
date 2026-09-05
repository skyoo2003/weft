// SPDX-License-Identifier: Apache-2.0

package query

import (
	"fmt"
	"strings"

	"github.com/skyoo2003/weft/pkg/engine"
)

// Plan is a parsed query string: the scorers to run, and the constraints to
// apply between them.
//
// Both halves, because a constraint in this package is a Fuser and not a scorer
// — see the package documentation. Parse cannot return a single scorer without
// deciding the fusion policy for the caller, and that decision is not Parse's to
// make: engine.Search takes the Fuser as a parameter precisely so the caller
// owns it.
//
// So Parse hands back the pieces and the caller assembles them:
//
//	p, err := query.Parse(ix, `+title:covid -draft "airborne transmission"`)
//	if err != nil { ... }
//	results, err := engine.Search(ctx, engine.Query{}, 10, p.Fuse(fusion.Fuse), p.Scorers...)
//
// The scorers are in the order the clauses were written, which is what makes
// Fuse's stream indexes mean something a reader can check against the query.
type Plan struct {
	// Scorers is one scorer per clause, in source order.
	Scorers []engine.Scorer

	// must and mustNot are indexes into Scorers, not scorers of their own. A
	// clause is both a stream and a constraint, and keeping the constraint as an
	// index is what lets one stream vote and restrict at once — which is what
	// `+` means.
	must, mustNot []int
}

// Fuse wraps a base Fuser with this plan's constraints.
//
// The base is the caller's, so a plan can be fused with fusion.Fuse, with
// fusion.FuseWeighted, or with anything else — Parse decides which documents are
// eligible and never how the eligible ones are ranked. A plan with no `+` and no
// `-` returns base unchanged.
func (p Plan) Fuse(base engine.Fuser) engine.Fuser {
	return Must(MustNot(base, p.mustNot...), p.must...)
}

// Parse builds a Plan from a query string.
//
// # The syntax, whole
//
//	covid             a term in Document.Text
//	title:covid       a term in the named field
//	+covid            required: every result must match this clause
//	-draft            excluded: no result may match this clause
//	"exact phrase"    a run of consecutive tokens in Document.Text
//	covid*            a term pattern — see Glob for the whole syntax
//	covid~            within one edit; covid~2 within two — see Fuzzy
//	price:[a TO b]    an inclusive range — see Range
//	price:[a TO *]    unbounded above; [* TO b] unbounded below
//
// A prefix applies to whatever follows it: `+title:covid*` is a required glob on
// the title, `-price:[* TO 0]` excludes anything priced at or below zero.
//
// # What it is not
//
// **There is no boolean algebra.** No parentheses, no OR, no nesting. A clause
// list with `+` and `-` is what rank fusion can express: a union of votes with
// an intersection and a difference applied to it. `a OR (b AND c)` has no
// meaning here, and inventing one would mean building a query tree and an
// evaluator over it — a different engine, not a parser.
//
// That is a real limit and it is the one to read before adopting this. What it
// buys is that every query is a flat list of streams whose fusion the caller can
// see, weight and reason about.
//
// **Ranges do not encode their bounds.** `price:[00 TO ff]` compares as bytes,
// so a numeric range means what the caller's indexing made it mean — write
// EncodeInt's output into the query, or build the Range scorer directly. Parse
// has no way to know a field holds numbers.
//
// # Errors
//
// An unterminated quote, a malformed range, a range whose bounds are reversed,
// a phrase with no terms, and a fuzzy distance outside [1, MaxEditDistance] are
// all errors. None of them is a query that quietly returns nothing, which is the
// failure this package refuses everywhere else.
//
// An empty query is a plan with no scorers, not an error: a caller building a
// query from user input gets that case for free.
func Parse(ix *engine.Index, q string) (Plan, error) {
	var p Plan
	for _, tok := range splitClauses(q) {
		text := tok
		required, excluded := false, false
		switch {
		case strings.HasPrefix(tok, "+"):
			text, required = tok[1:], true
		case strings.HasPrefix(tok, "-"):
			text, excluded = tok[1:], true
		}
		if text == "" {
			continue
		}
		s, err := clause(ix, text)
		if err != nil {
			return Plan{}, err
		}
		if required {
			p.must = append(p.must, len(p.Scorers))
		}
		if excluded {
			p.mustNot = append(p.mustNot, len(p.Scorers))
		}
		p.Scorers = append(p.Scorers, s)
	}
	return p, nil
}

// clause builds one scorer from one clause, prefix already stripped.
func clause(ix *engine.Index, text string) (engine.Scorer, error) {
	// A quoted phrase first, because it is the one clause whose body may hold a
	// colon, a bracket or a star without meaning any of them.
	if rest, quoted := strings.CutPrefix(text, `"`); quoted {
		phrase, closed := strings.CutSuffix(rest, `"`)
		if !closed {
			return nil, fmt.Errorf("query %q: unterminated quote", text)
		}
		// Over a Glob rather than over the whole corpus: Phrase filters an inner
		// scorer's candidates, so it needs one nominating every document that
		// holds the phrase's words. The first word's Glob is that, and it is what
		// keeps the record decodes bounded by a posting list rather than by the
		// corpus.
		words := ix.Tokenize(phrase)
		if len(words) == 0 {
			return nil, fmt.Errorf("query %q: the phrase has no terms", text)
		}
		return Phrase(ix, Glob(ix, "", words[0]), phrase), nil
	}

	field, body := "", text
	if f, rest, ok := strings.Cut(text, ":"); ok && f != "" && rest != "" {
		field, body = f, rest
	}

	if lo, hi, ok := parseRange(body); ok {
		if lo != "" && hi != "" && lo > hi {
			return nil, fmt.Errorf("query %q: the lower bound is above the upper", text)
		}
		return Range(ix, field, lo, hi), nil
	}
	// A body that opens a range and does not close one is a typo, not a term
	// that happens to start with a bracket — the default tokenizer cannot
	// produce a term containing either.
	if strings.HasPrefix(body, "[") {
		return nil, fmt.Errorf("query %q: a range is [lo TO hi]", text)
	}

	if term, dist, ok := parseFuzzy(body); ok {
		if dist < 1 || dist > MaxEditDistance {
			return nil, fmt.Errorf("query %q: edit distance %d is not in [1, %d]", text, dist, MaxEditDistance)
		}
		return Fuzzy(ix, field, term, dist), nil
	}

	// Everything else is a Glob, which covers a plain term too: a pattern with no
	// metacharacter matches itself and nothing else. One clause kind fewer, and
	// one fewer place for the field scoping to be forgotten.
	return Glob(ix, field, body), nil
}

// parseRange reads `[lo TO hi]`, with `*` for an unbounded side.
func parseRange(body string) (lo, hi string, ok bool) {
	inner, hadOpen := strings.CutPrefix(body, "[")
	if !hadOpen {
		return "", "", false
	}
	inner, hadClose := strings.CutSuffix(inner, "]")
	if !hadClose {
		return "", "", false
	}
	l, h, hadTO := strings.Cut(inner, " TO ")
	if !hadTO {
		return "", "", false
	}
	l, h = strings.TrimSpace(l), strings.TrimSpace(h)
	// `*` is the unbounded spelling, because an empty side would make `[ TO b]`
	// and a typo indistinguishable.
	if l == "*" {
		l = ""
	}
	if h == "*" {
		h = ""
	}
	return l, h, true
}

// parseFuzzy reads `term~` or `term~N`.
func parseFuzzy(body string) (term string, dist int, ok bool) {
	i := strings.LastIndex(body, "~")
	if i <= 0 {
		return "", 0, false
	}
	term, suffix := body[:i], body[i+1:]
	if suffix == "" {
		return term, 1, true
	}
	// One digit, because MaxEditDistance is a single digit and anything longer is
	// a term that happens to contain a tilde rather than a distance.
	if len(suffix) != 1 || suffix[0] < '0' || suffix[0] > '9' {
		return "", 0, false
	}
	return term, int(suffix[0] - '0'), true
}

// splitClauses cuts a query into clauses at whitespace, keeping a quoted run and
// a bracketed range whole.
//
// A hand-written scan rather than strings.Fields, because **two** clause kinds
// contain whitespace that does not separate them. A phrase is the obvious one. A
// range is the one that is easy to miss: `price:[a TO b]` reads as three fields
// to any splitter that only knows about quotes, and the failure is not a parse
// error — it is three clauses, one of which is a term query for `TO`.
//
// An unterminated quote or bracket is left for clause to report. This function's
// job is to cut, and a scanner that also validated would be two judges of one
// question; what it does here is stop cutting, so the whole malformed run
// reaches the place that names it.
func splitClauses(q string) []string {
	var out []string
	var cur strings.Builder
	quoted, bracketed := false, false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range q {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case r == '[' && !quoted:
			bracketed = true
			cur.WriteRune(r)
		case r == ']' && !quoted:
			bracketed = false
			cur.WriteRune(r)
		case !quoted && !bracketed && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}
