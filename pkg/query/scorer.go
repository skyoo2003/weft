// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"fmt"
	"math"
	"path"
	"strings"

	"github.com/skyoo2003/weft/pkg/engine"
)

const (
	// MaxTerms bounds how many indexed terms one pattern may expand to.
	//
	// ponytail: a pattern like "*" expands to the whole vocabulary, and every
	// term it names costs a posting-list walk. The bound is on terms and not on
	// postings because terms are what the caller's pattern chose; a limit on
	// postings would make a query's meaning depend on the corpus. 4096 is a
	// convention, not a measurement — the number to replace once a pattern query
	// shows up in a latency profile.
	MaxTerms = 4096

	// PhraseOverfetch is how many candidates Phrase asks its inner scorer for,
	// as a multiple of k.
	//
	// A filter removes candidates, so asking for exactly k returns fewer than k
	// and usually far fewer — the phrase is rarer than its words. This is the
	// "fuse deeper than you display" rule the README states, applied inside the
	// one scorer that always needs it.
	//
	// ponytail: a constant multiple, so a phrase rarer than 1-in-10 of its inner
	// scorer's ranking still comes back short. The honest fix is to widen until
	// k are found or the inner scorer is exhausted, which needs a scorer that
	// can be asked again and can say when it has no more.
	PhraseOverfetch = 10
)

// termScorer is the shape all three term-selection queries share: pick a set of
// indexed terms, then score every document holding any of them.
//
// The scoring is the same for Glob, Range and Fuzzy and is deliberately **not**
// BM25. Under rank fusion what a term-selection query owes is a monotone,
// non-degenerate ordering — fusion.Fuse reads rank rather than score — so this
// is IDF times frequency, summed over the matched terms, with no length
// normalization. scorer/text is where BM25 lives and this package does not
// import it.
//
// One implementation and three selection rules, because the difference between
// these queries is entirely which terms they name. Three copies of the
// accumulate-and-rank loop would be three places for the IDF clamp, the
// cancellation polling and the "do not truncate to k" rule to drift apart.
type termScorer struct {
	ix   *engine.Index
	name string

	// pick returns the terms to score, already scoped to a field. It runs
	// before any posting is read, so a query that names nothing costs a
	// vocabulary scan and no corpus work.
	pick func() ([]string, error)
}

func (t *termScorer) Name() string { return t.name }

// Candidates implements engine.Scorer.
//
// It returns **every** matching document and does not truncate to k, which is
// what Must and MustNot need — see the package documentation for why a
// restriction truncated to k is a wrong answer rather than a narrow one. Passing
// one of these to Search as an ordinary scorer is still fine: fusion takes the
// top k at the end.
func (t *termScorer) Candidates(ctx context.Context, _ engine.Query, k int) ([]engine.Candidate, error) {
	if k <= 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	terms, err := t.pick()
	if err != nil {
		return nil, err
	}
	if len(terms) == 0 {
		return nil, nil
	}

	docs, _ := t.ix.Stats()
	if docs == 0 {
		return nil, nil
	}
	n := float64(docs)

	acc := make(map[engine.DocID]float64)
	var posts []engine.Posting
	for _, term := range terms {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// LookupInto, not Lookup: a term's postings are walked and finished with
		// before the next term is looked up, so one buffer holds the longest
		// list instead of the sum of them. Same reasoning as scorer/text.
		posts = t.ix.LookupInto(term, posts)
		if len(posts) == 0 {
			continue
		}
		// Clamped for the reason scorer/text clamps: postings are fetched after
		// Stats, so a concurrent Add can leave more postings for a term than
		// there were documents, and an unclamped count drives the log below 1
		// and the IDF negative.
		nq := math.Min(float64(len(posts)), n)
		idf := math.Log(1 + (n-nq+0.5)/(nq+0.5))
		for i, p := range posts {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			acc[p.Doc] += idf * float64(p.Freq)
		}
	}
	if len(acc) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]engine.Candidate, 0, len(acc))
	for doc, score := range acc {
		out = append(out, engine.Candidate{Doc: doc, Score: score})
	}
	return engine.TopK(out, len(out)), nil
}

// scopedTerms is the vocabulary a field-scoped query may match, with the field's
// prefix already taken off.
//
// Every selection rule below wants the same two things — the terms of one field,
// and them stated as the caller would have typed them — so the scoping and the
// unscoping live here rather than in three places. An empty field is
// Document.Text's own term space, which engine.FieldTerm already spells as "the
// token unchanged".
func scopedTerms(ix *engine.Index, field, literal string) (terms []string, prefix string) {
	prefix = engine.FieldTerm(field, "")
	return ix.Terms(prefix+literal, MaxTerms), prefix
}

// Glob ranks documents holding a term matching pattern, in the named field.
//
// An empty field is Document.Text. Any other name is one of Document.Fields, and
// the pattern is matched against that field's tokens alone — the same scoping
// text.NewField gives BM25, so a caller does not have to know that a field's
// terms are spelled with a separator.
//
// The syntax is path.Match's: `*` matches any run of non-separator characters,
// `?` matches one, and `[a-z]` is a character class. `go*` is a prefix query,
// `*ing` a suffix query, `organi[sz]e` an alternation. The pattern is matched
// against indexed terms, so it is matched against whatever this index's
// tokenizer produced — lowercased and split at non-letters by default.
//
// Two consequences of borrowing path.Match rather than writing a matcher:
//
//   - `/` is a separator to it, so `*` will not cross one. The default
//     tokenizer cannot produce a term containing `/`, but a caller-supplied one
//     can, and there such a term is only reachable by naming the slash.
//   - A malformed pattern — an unterminated `[` — is reported by Candidates as
//     an error rather than silently matching nothing, which is the difference
//     between a typo you find and a query that quietly returns zero hits.
//
// See termScorer for what it scores and why it does not truncate to k.
func Glob(ix *engine.Index, field, pattern string) engine.Scorer {
	return &termScorer{ix: ix, name: scorerName("glob", field), pick: func() ([]string, error) {
		// Validated once, against a term that cannot match, before the
		// vocabulary scan. path.Match reports a bad pattern on every call, so
		// checking inside the loop would ask the same question once per term and
		// answer it only after paying for the scan.
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("glob %q: %w", pattern, err)
		}
		// The literal head narrows what has to be examined. It is the whole
		// pattern for a prefix query, and empty for one starting in a wildcard —
		// where the scan is the field's vocabulary and MaxTerms bounds the result.
		cand, prefix := scopedTerms(ix, field, literalPrefix(pattern))
		out := make([]string, 0, len(cand))
		for _, term := range cand {
			// The error cannot happen — the pattern was validated above — and
			// re-reporting it here would be a second judge of one question.
			if ok, _ := path.Match(pattern, strings.TrimPrefix(term, prefix)); ok {
				out = append(out, term)
			}
		}
		return out, nil
	}}
}

// literalPrefix is the part of a pattern before its first metacharacter, which
// every term it can match must start with.
//
// `\` is path.Match's escape, so a pattern may name a literal `*`. Stopping at
// the backslash rather than trying to unescape keeps this a narrowing hint that
// is never wrong: a shorter prefix examines more terms and matches the same set.
func literalPrefix(pattern string) string {
	if i := strings.IndexAny(pattern, `*?[\`); i >= 0 {
		return pattern[:i]
	}
	return pattern
}

// scorerName is what a stream calls itself in a diagnostic. A field-scoped query
// names its field, so a caller fusing several can tell them apart — the same
// convention scorer/text uses.
func scorerName(kind, field string) string {
	if field == "" {
		return kind
	}
	return kind + ":" + field
}

// Phrase wraps inner and keeps only the candidates whose text contains phrase as
// a run of consecutive tokens.
//
// # Why it re-reads the document
//
// An engine.Posting carries a term's document and frequency, **not its
// positions**, so no exact phrase can be decided from the index —
// docs/FORMAT.md section 8 prices the format change that would alter this and it
// is not made. What is left is the document text, and this is the shape the
// README recommends for it: wrap a scorer and filter its candidates, at one
// record decode each, rather than sweeping every DocID and paying that decode
// across the corpus.
//
// The cost is therefore bounded by what inner nominates and not by the corpus:
// k times PhraseOverfetch record decodes and tokenizations per query.
//
// # What it reads, and what it does not
//
// **Document.Text only.** A phrase inside a Document.Field is not found, because
// this checks the one text a document has always had. Scoping it would mean a
// second parameter and a second rule about which text a run may cross, and
// neither is worth guessing at before somebody asks.
//
// # What it scores
//
// Whatever inner scored. The phrase is a constraint and not a signal, so a
// surviving candidate keeps its position in inner's ranking — which means a
// caller can wrap the text scorer and still be ranking by BM25.
//
// Candidates returns every surviving candidate rather than truncating to k, for
// the reason termScorer gives. Note the difference though: what survives is
// bounded by what inner nominated, so this restricts *inner's candidates* and
// not the corpus.
//
// A phrase that tokenizes to nothing matches every candidate, because a caller
// asking to filter on an empty phrase has asked for no constraint. A phrase of
// one token is an ordinary term match and is allowed.
func Phrase(ix *engine.Index, inner engine.Scorer, phrase string) engine.Scorer {
	return &phraseScorer{ix: ix, inner: inner, phrase: phrase}
}

type phraseScorer struct {
	ix     *engine.Index
	inner  engine.Scorer
	phrase string
}

func (p *phraseScorer) Name() string { return "phrase" }

func (p *phraseScorer) Candidates(ctx context.Context, q engine.Query, k int) ([]engine.Candidate, error) {
	if k <= 0 {
		return nil, nil
	}
	if p.inner == nil {
		return nil, fmt.Errorf("phrase %q: no inner scorer to filter", p.phrase)
	}
	// Through the index's tokenizer, so the phrase is split the way the
	// documents were. A phrase split differently from the text it is checked
	// against matches nothing, with nothing to report.
	want := p.ix.Tokenize(p.phrase)

	cands, err := p.inner.Candidates(ctx, q, k*PhraseOverfetch)
	if err != nil {
		return nil, fmt.Errorf("inner scorer %s: %w", p.inner.Name(), err)
	}
	if len(want) == 0 || len(cands) == 0 {
		return cands, nil
	}

	out := make([]engine.Candidate, 0, len(cands))
	for i, c := range cands {
		// A record decode and a tokenization apiece, so the poll is per
		// candidate rather than per batch — this is the one loop in this package
		// whose single iteration is expensive.
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		d, ok := p.ix.Doc(c.Doc)
		if !ok {
			// Deleted between inner's scan and this one, or damaged. Either way
			// it is not a document that can hold the phrase.
			continue
		}
		if containsRun(p.ix.Tokenize(d.Text), want) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// containsRun reports whether want appears in have as consecutive elements.
//
// A straight scan, not an index: have is one document's tokens and want is a
// phrase, so the product is small and bounded by the decode that produced have.
func containsRun(have, want []string) bool {
	if len(want) > len(have) {
		return false
	}
	for i := 0; i+len(want) <= len(have); i++ {
		matched := true
		for j, w := range want {
			if have[i+j] != w {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
