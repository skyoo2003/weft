// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/scorer/graph"
)

// inspectCmd reads an index's own account of itself.
//
// Every flag here answers a question that otherwise needs a Go program, and they
// are the questions asked when a query returned something surprising: is the
// term in the index at all, under what spelling, in which documents, and what
// does the link graph around this document actually look like.
func inspectCmd(args []string, stdout, stderr io.Writer) error {
	f, err := parseInspectArgs(args, stderr)
	if err != nil {
		return err
	}

	// -analyze without an index is engine's package-level tokenizer: the default
	// one with nothing behind it. That is the only question in this command with
	// an answer before a corpus exists, and it is the one worth asking first,
	// because what a text becomes decides what a query can match.
	if f.data == "" {
		printTerms(stdout, "analyzed", tokenizeWithout(f.tok, f.tokName, f.analyze))
		return nil
	}

	ix, err := engine.Open(f.data, engine.WithTokenizer(f.tok))
	if err != nil {
		return err
	}
	defer ix.Close() //nolint:errcheck // nothing was written, so a failed close changes no answer

	return f.report(stdout, ix)
}

// inspectArgs is one invocation's flags after they have been checked.
type inspectArgs struct {
	data, tokName                                       string
	doc, term, terms, field, postings, nearest, analyze string
	stats, adjacency                                    bool
	limit                                               int

	// set is which flags were written rather than left at their default, which
	// is the difference between "walk the whole term space" and "no term walk
	// was asked for" — both of which spell -terms as the empty string.
	set map[string]bool

	tok engine.Tokenizer
}

func parseInspectArgs(args []string, stderr io.Writer) (*inspectArgs, error) {
	var f inspectArgs

	fs, err := parseFlags(cmdInspect, args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&f.data, "data", "", "directory holding the index")
		fs.StringVar(&f.tokName, "tokenizer", defaultTokenizer,
			"tokenizer the index was committed with: "+strings.Join(tokenizerNames(), ", "))
		fs.BoolVar(&f.stats, "stats", false, "document count, average length and id space; the default")
		fs.StringVar(&f.doc, "doc", "", "report on one document, by key")
		fs.StringVar(&f.term, "term", "", "how many documents hold this term, and which")
		fs.StringVar(&f.terms, "terms", "",
			"walk the term space from this prefix; the empty prefix walks all of it")
		fs.StringVar(&f.field, "field", "", "scope -term and -terms to one of Document.Fields")
		fs.StringVar(&f.postings, "postings", "", "walk this term's postings a block at a time")
		fs.StringVar(&f.nearest, "nearest", "",
			"the documents closest to this vector, comma-separated numbers")
		fs.BoolVar(&f.adjacency, "graph", false, "the link graph's size; with -doc, that document's edges")
		fs.StringVar(&f.analyze, "analyze", "", "tokenize this text; the one question here needing no index")
		fs.IntVar(&f.limit, "limit", 20, "how many terms, postings or neighbours to print")
	})
	if err != nil {
		return nil, err
	}
	f.set = map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })

	var ok bool
	if f.tok, ok = tokenizers[f.tokName]; !ok {
		return nil, badUsage("unknown tokenizer %q; the choices are %s",
			f.tokName, strings.Join(tokenizerNames(), ", "))
	}
	if f.limit <= 0 {
		return nil, badUsage("-limit must be positive, got %d", f.limit)
	}
	if f.data == "" && !f.set["analyze"] {
		return nil, badUsage("-data names the directory holding the index, and it is required; " +
			"-analyze is the only question here with an answer without one")
	}
	return &f, nil
}

// report runs every question the flags asked, in a fixed order so two runs of
// the same command print the same thing in the same place.
func (f *inspectArgs) report(w io.Writer, ix *engine.Index) error {
	// Stats is the default rather than an empty run: a command given only the
	// flag it cannot work without should say something about what it opened.
	selected := f.set["doc"] || f.set["term"] || f.set["terms"] || f.set["postings"] ||
		f.set["nearest"] || f.set["analyze"] || f.adjacency
	if f.stats || !selected {
		reportStats(w, ix)
	}
	if f.set["analyze"] {
		// Index.Tokenize rather than engine.Tokenize: this is the tokenizer the
		// index was committed with, and the two differ exactly when it matters.
		printTerms(w, "analyzed", ix.Tokenize(f.analyze))
	}
	if f.set["doc"] {
		if err := reportDoc(w, ix, f.doc); err != nil {
			return err
		}
	}
	if f.set["term"] {
		reportTerm(w, ix, f.field, f.term, f.limit)
	}
	if f.set["terms"] {
		reportTermSpace(w, ix, f.field, f.terms, f.limit)
	}
	if f.set["postings"] {
		reportPostings(w, ix, f.field, f.postings, f.limit)
	}
	if f.set["nearest"] {
		v, err := floats32(f.nearest)
		if err != nil {
			return badUsage("-nearest: %v", err)
		}
		reportNearest(w, ix, v, f.limit)
	}
	if f.adjacency {
		return reportGraph(w, ix, f.doc)
	}
	return nil
}

// tokenizeWithout answers -analyze with no index open.
//
// engine.Tokenize is named directly for the default rather than reached through
// the table, whose "default" entry is that same function: going through the
// table would leave the exported one looking like something no caller needs.
func tokenizeWithout(tok engine.Tokenizer, name, text string) []string {
	if name == defaultTokenizer {
		return engine.Tokenize(text)
	}
	return tok(text)
}

// reportStats prints both counts, because they answer different questions.
//
// Stats is the live population. Len is one past the highest DocID and counts
// tombstones with it, so an index that has been deleted from is larger by Len
// than by Stats — and a reader shown only one of them cannot tell a corpus of
// three from a corpus of three with nine hundred deletions behind it.
func reportStats(w io.Writer, ix *engine.Index) {
	docs, _ := ix.Stats()
	outf(w, "%d documents, average length %.1f tokens, id space %d (Len, tombstones included)\n",
		docs, ix.AvgDocLen(), ix.Len())
}

// reportDoc is everything the index will say about one key.
func reportDoc(w io.Writer, ix *engine.Index, key string) error {
	id, ok := ix.Resolve(key)
	if !ok {
		return fmt.Errorf("no live document keyed %q; Resolve did not find it, "+
			"which is also what a deleted document looks like", key)
	}
	d, ok := ix.Doc(id)
	if !ok {
		return fmt.Errorf("document %q resolves to id %d and the index will not return it", key, id)
	}

	outf(w, "%s (id %d)\n", d.Key, id)
	outf(w, "  text     %s\n", d.Text)
	outf(w, "  tokens   %d (every field's counted, which is what BM25 normalizes by)\n", ix.DocLen(id))
	if v, ok := ix.Vector(id); ok {
		outf(w, "  vector   %v\n", v)
	} else {
		outln(w, "  vector   none — the vector scorer cannot see this document")
	}
	for _, f := range d.Fields {
		outf(w, "  field    %s: %s\n", f.Name, f.Text)
	}
	if !d.Time.IsZero() {
		outf(w, "  time     %s\n", d.Time.Format("2006-01-02T15:04:05Z07:00"))
	}
	if nbrs, ok := ix.Neighbors(id); ok && len(nbrs) > 0 {
		outf(w, "  links    %s\n", strings.Join(keysOf(ix, nbrs), " "))
	} else {
		// Not the same as "this document has no Links". A link naming a key that
		// was never added stays dangling and is skipped at traversal time, so a
		// document can name three neighbours and reach none of them.
		outln(w, "  links    none reachable — the graph scorer cannot walk from this document")
	}
	return nil
}

// reportTerm answers whether a term is in the index, and where.
//
// PostingCount first because it is the cheap answer — it is what a scorer asks
// before deciding a term is worth reading at all — and Lookup after it, for the
// documents themselves.
func reportTerm(w io.Writer, ix *engine.Index, field, term string, limit int) {
	spelling := termIn(field, term)
	n := ix.PostingCount(spelling)
	outf(w, "%s: %d posting(s)\n", displayTerm(spelling), n)

	for i, p := range ix.Lookup(spelling) {
		if i >= limit {
			outf(w, "  ... %d more\n", n-limit)
			break
		}
		outf(w, "  %-14s freq %d\n", keyOf(ix, p.Doc), p.Freq)
	}
}

// reportTermSpace walks the terms from a prefix.
//
// One buffer, reused across every lookup in the loop, which is the whole reason
// LookupInto exists beside Lookup — and a term walk over a real corpus is where
// it pays for itself.
func reportTermSpace(w io.Writer, ix *engine.Index, field, prefix string, limit int) {
	found := ix.Terms(termIn(field, prefix), limit)
	outf(w, "%d term(s) from %q\n", len(found), prefix)

	var buf []engine.Posting
	for _, t := range found {
		buf = ix.LookupInto(t, buf[:0])
		outf(w, "  %-24s %d document(s)\n", displayTerm(t), len(buf))
	}
}

// reportPostings walks one term's postings a block at a time, which is how the
// scorers read them.
func reportPostings(w io.Writer, ix *engine.Index, field, term string, limit int) {
	spelling := termIn(field, term)
	cur := ix.BlockCursor(spelling)

	var buf []engine.Posting
	printed, block := 0, 0
	for {
		buf = cur.Next(buf[:0])
		if len(buf) == 0 {
			break
		}
		block++
		outf(w, "%s: block %d, %d posting(s)\n", displayTerm(spelling), block, len(buf))
		for _, p := range buf {
			if printed >= limit {
				break
			}
			outf(w, "  %-14s freq %d\n", keyOf(ix, p.Doc), p.Freq)
			printed++
		}
	}
	if block == 0 {
		outf(w, "%s: no blocks — the term is not in the index\n", displayTerm(spelling))
	}
	// Reported after the walk rather than instead of it: a cursor that failed
	// halfway has already yielded real postings, and treating that short read as
	// the whole list is how a term comes to look rarer than it is.
	if err := cur.Err(); err != nil {
		outf(w, "  the walk stopped early: %v\n", err)
	}
}

func reportNearest(w io.Writer, ix *engine.Index, v []float32, limit int) {
	ids := ix.Nearest(v, limit)
	outf(w, "%d nearest by vector\n", len(ids))
	for i, id := range ids {
		outf(w, "  %d. %s\n", i+1, keyOf(ix, id))
	}
}

// reportGraph is the Adjacency type, which pkg/scorer/graph exports in full and
// nothing has ever printed.
func reportGraph(w io.Writer, ix *engine.Index, key string) error {
	adj, err := graph.NewAdjacency(context.Background(), ix)
	if err != nil {
		return err
	}
	outf(w, "%d node(s), %d edge(s)\n", adj.Len(), adj.Edges())
	if key == "" {
		return nil
	}

	id, ok := ix.Resolve(key)
	if !ok {
		return fmt.Errorf("no live document keyed %q", key)
	}
	outf(w, "  %s degree %d\n", key, adj.Degree(id))
	outf(w, "  out: %s\n", listOrNone(keysOf(ix, adj.Out(id))))
	outf(w, "  in:  %s\n", listOrNone(keysOf(ix, adj.In(id))))
	return nil
}

// termIn spells a term in a field's space, or leaves it in Document.Text's.
//
// This is engine.FieldTerm's whole purpose at a command line: a field's terms
// are ordinary postings under a name, and the name is not something a caller can
// spell without the library saying how.
func termIn(field, term string) string {
	if field == "" {
		return term
	}
	return engine.FieldTerm(field, term)
}

// displayTerm strips the field prefix back off for printing, so a field walk
// reads as that field's own terms rather than as the encoding they are stored
// under.
func displayTerm(term string) string {
	if i := strings.LastIndexByte(term, 0); i >= 0 {
		return term[i+1:]
	}
	return term
}

func keyOf(ix *engine.Index, id engine.DocID) string {
	d, ok := ix.Doc(id)
	if !ok {
		return fmt.Sprintf("(id %d, gone)", id)
	}
	return d.Key
}

func keysOf(ix *engine.Index, ids []engine.DocID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, keyOf(ix, id))
	}
	return out
}

func listOrNone(keys []string) string {
	if len(keys) == 0 {
		return "none"
	}
	return strings.Join(keys, " ")
}

func printTerms(w io.Writer, label string, terms []string) {
	outf(w, "%s: %d term(s)\n", label, len(terms))
	for _, t := range terms {
		outf(w, "  %s\n", t)
	}
}
