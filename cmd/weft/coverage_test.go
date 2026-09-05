// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// This file is the ledger: every callable symbol weft exports, and the command
// that reaches it.
//
// It exists because "the CLI supports the whole library" is a claim that decays
// silently in both directions. A new export nobody wired up is invisible — the
// golden API files record it and no test asks whether anything can call it. A
// command deleted or renamed leaves the claim in a README that still reads true.
// So the table below is read against pkg/engine/testdata/*.txt on every run: a
// symbol missing from it fails, a stale row naming a symbol that no longer
// exists fails, and a symbol claimed reachable whose call site is gone fails.
//
// The shape is milestone 25's, deliberately. TestTheRefusalRateIsCounted holds
// the OpenSearch DSL table as data so that a quietly implemented row and a
// quietly broken one both break the build; this holds the API surface the same
// way, and prints the coverage figure rather than leaving it to a sentence in a
// document that nobody re-derives.
//
// What it proves and what it does not: the proof is a call site in the surface's
// own source, found after comments are stripped. That catches an API nothing
// calls. It does not prove the call sits on a path a user can actually reach,
// and it cannot tell two same-named methods apart — `.Len(` is Index.Len and
// Adjacency.Len both. Where that mattered the row carries an explicit call
// string instead of the derived one.

// The two surfaces, and the directories that are each one's implementation.
const (
	surfWeft  = "weft"
	surfWeftd = "weftd"
)

var sourceDirs = map[string][]string{
	surfWeft:  {"."},
	surfWeftd: {filepath.Join("..", "weftd"), filepath.Join("..", "..", "internal", "opensearch")},
}

// entry is one row: which surfaces reach this symbol, and how that is proven.
type entry struct {
	// on lists the surfaces reaching the symbol. Empty means nothing does, and
	// why then has to say what would have to change for that to stop being true.
	on []string

	// call overrides the derived call site. Set it where the derived string is
	// ambiguous — `.Fuse(` matches both fusion.Fuse and Plan.Fuse — or where the
	// symbol is an interface method a caller never names: every Candidates is
	// reached through engine.Search and nothing else, so that is what is looked
	// for.
	call string

	// why is required when on is empty.
	why string
}

// searchDriven is the proof for every Candidates method. A caller does not call
// Candidates; it hands scorers to Search, which does. Looking for the method name
// would find nothing, and looking for the constructor would prove the wrong thing —
// a scorer built and never searched.
const searchDriven = "engine.Search("

// nameCall is the proof for every Name method. One line in the search breakdown
// calls it on whatever scorer it is holding, so the concrete implementations are
// all reached by the same call site and none of them is named there.
const nameCall = ".Name("

// collectorWhy is one reason written once, because the three Collector symbols
// share it exactly.
const collectorWhy = "Collector is the top-k buffer a Scorer implementation keeps while it walks " +
	"postings. A command runs scorers; it does not write one. Reaching it would mean inventing a " +
	"scorer for a flag to select, and pkg/scorer is what that would duplicate."

var ledger = map[string]entry{
	// ------------------------------------------------------------ engine
	"engine.FieldTerm":     {on: []string{surfWeft, surfWeftd}},
	"engine.New":           {on: []string{surfWeft, surfWeftd}},
	"engine.NewCollector":  {why: collectorWhy},
	"engine.Open":          {on: []string{surfWeft, surfWeftd}},
	"engine.Scrub":         {on: []string{surfWeft}},
	"engine.Search":        {on: []string{surfWeft, surfWeftd}},
	"engine.Tokenize":      {on: []string{surfWeft}},
	"engine.TopK":          {on: []string{surfWeftd}},
	"engine.WithTokenizer": {on: []string{surfWeft}},

	// `.Err(` alone is satisfied by bufio.Scanner's, which the index command
	// calls on stdin, so the cursor's own name is part of the proof here.
	"engine.BlockCursor.Err":  {on: []string{surfWeft}, call: "cur.Err("},
	"engine.BlockCursor.Next": {on: []string{surfWeft}, call: "cur.Next("},
	"engine.Collector.Offer":  {why: collectorWhy},
	"engine.Collector.Take":   {why: collectorWhy},

	"engine.Index.Add":          {on: []string{surfWeft, surfWeftd}},
	"engine.Index.AvgDocLen":    {on: []string{surfWeft}},
	"engine.Index.BlockCursor":  {on: []string{surfWeft}, call: "ix.BlockCursor("},
	"engine.Index.Close":        {on: []string{surfWeft, surfWeftd}},
	"engine.Index.Commit":       {on: []string{surfWeft, surfWeftd}},
	"engine.Index.Delete":       {on: []string{surfWeft, surfWeftd}},
	"engine.Index.Doc":          {on: []string{surfWeft, surfWeftd}},
	"engine.Index.DocLen":       {on: []string{surfWeft}},
	"engine.Index.Len":          {on: []string{surfWeft}, call: "ix.Len("},
	"engine.Index.Lookup":       {on: []string{surfWeft}},
	"engine.Index.LookupInto":   {on: []string{surfWeft}},
	"engine.Index.Merge":        {on: []string{surfWeft, surfWeftd}},
	"engine.Index.Nearest":      {on: []string{surfWeft}},
	"engine.Index.Neighbors":    {on: []string{surfWeft}},
	"engine.Index.PostingCount": {on: []string{surfWeft}},
	"engine.Index.Resolve":      {on: []string{surfWeft, surfWeftd}},
	"engine.Index.Stats":        {on: []string{surfWeft, surfWeftd}},
	"engine.Index.Terms":        {on: []string{surfWeft, surfWeftd}},
	"engine.Index.Tokenize":     {on: []string{surfWeft, surfWeftd}},
	"engine.Index.Update":       {on: []string{surfWeft, surfWeftd}},
	"engine.Index.Vector":       {on: []string{surfWeft}},

	"engine.Scorer.Name":       {on: []string{surfWeft}, call: nameCall},
	"engine.Scorer.Candidates": {on: []string{surfWeft, surfWeftd}, call: searchDriven},

	// ------------------------------------------------------------ fusion
	// Fuse is handed over as a value rather than called, so the derived
	// `fusion.Fuse(` finds nothing and the bare name is a prefix of
	// FuseWeighted's. The closing parenthesis is what separates them:
	// `engine.Fuser(fusion.Fuse)` is the conversion both surfaces write.
	"fusion.Fuse":         {on: []string{surfWeft, surfWeftd}, call: "fusion.Fuse)"},
	"fusion.FuseWeighted": {on: []string{surfWeft, surfWeftd}},

	// ------------------------------------------------------------- query
	"query.EncodeInt":  {on: []string{surfWeft, surfWeftd}},
	"query.EncodeTime": {on: []string{surfWeft, surfWeftd}},
	"query.Fuzzy":      {on: []string{surfWeftd}},
	"query.Glob":       {on: []string{surfWeftd}},
	"query.Must":       {on: []string{surfWeftd}},
	"query.MustNot":    {on: []string{surfWeftd}},
	"query.Parse":      {on: []string{surfWeft}},
	"query.Phrase":     {on: []string{surfWeftd}},
	"query.Range":      {on: []string{surfWeftd}},
	"query.Plan.Fuse":  {on: []string{surfWeft}, call: "plan.Fuse("},

	// ------------------------------------------------------ scorer/graph
	"scorer/graph.New":               {on: []string{surfWeft}, call: "graph.New("},
	"scorer/graph.NewAdjacency":      {on: []string{surfWeft}},
	"scorer/graph.NewIncludingSeeds": {on: []string{surfWeft}},
	"scorer/graph.NewPPR":            {on: []string{surfWeft}},
	"scorer/graph.WithPrecision":     {on: []string{surfWeft}},
	"scorer/graph.WithRestart":       {on: []string{surfWeft}},
	"scorer/graph.Adjacency.Degree":  {on: []string{surfWeft}},
	"scorer/graph.Adjacency.Edges":   {on: []string{surfWeft}},
	"scorer/graph.Adjacency.In":      {on: []string{surfWeft}, call: "adj.In("},
	"scorer/graph.Adjacency.Len":     {on: []string{surfWeft}, call: "adj.Len("},
	"scorer/graph.Adjacency.Out":     {on: []string{surfWeft}, call: "adj.Out("},
	"scorer/graph.PPR.Candidates":    {on: []string{surfWeft}, call: searchDriven},
	"scorer/graph.PPR.Name":          {on: []string{surfWeft}, call: nameCall},
	"scorer/graph.Scorer.Candidates": {on: []string{surfWeft}, call: searchDriven},
	"scorer/graph.Scorer.Name":       {on: []string{surfWeft}, call: nameCall},

	// ---------------------------------------------------- scorer/recency
	"scorer/recency.New":               {on: []string{surfWeft}, call: "recency.New("},
	"scorer/recency.NewAt":             {on: []string{surfWeft, surfWeftd}},
	"scorer/recency.Scorer.Candidates": {on: []string{surfWeft, surfWeftd}, call: searchDriven},
	"scorer/recency.Scorer.Name":       {on: []string{surfWeft}, call: nameCall},

	// ------------------------------------------------------- scorer/text
	"scorer/text.New":               {on: []string{surfWeft}, call: "text.New("},
	"scorer/text.NewField":          {on: []string{surfWeft}},
	"scorer/text.Scorer.Candidates": {on: []string{surfWeft}, call: searchDriven},
	"scorer/text.Scorer.Name":       {on: []string{surfWeft}, call: nameCall},

	// ----------------------------------------------------- scorer/vector
	"scorer/vector.New":               {on: []string{surfWeft, surfWeftd}, call: "vector.New("},
	"scorer/vector.Scorer.Candidates": {on: []string{surfWeft, surfWeftd}, call: searchDriven},
	"scorer/vector.Scorer.Name":       {on: []string{surfWeft}, call: nameCall},
}

// TestEveryPublicSymbolHasACommandOrAReason is the judgment sentence of this
// round: the library's callable surface, and what reaches each part of it.
func TestEveryPublicSymbolHasACommandOrAReason(t *testing.T) {
	symbols := goldenSymbols(t)

	source := map[string]string{}
	for surf, dirs := range sourceDirs {
		var b strings.Builder
		for _, d := range dirs {
			b.WriteString(sourceWithoutComments(t, d))
		}
		source[surf] = b.String()
	}

	// A row for a symbol that no longer exists is the stale half of the check, and
	// it is reported first: without it a renamed export leaves a row that keeps
	// passing while nothing tests the new name.
	for key := range ledger {
		if !slices.Contains(symbols, key) {
			t.Errorf("the ledger names %q, which is in neither golden API file. "+
				"Renamed or removed? Update the row rather than dropping it silently.", key)
		}
	}

	var unreachable []string
	covered := 0
	for _, key := range symbols {
		e, ok := ledger[key]
		if !ok {
			t.Errorf("%s is exported and the ledger has no row for it. Add one: "+
				"a surface that calls it, or the reason a command cannot.", key)
			continue
		}
		want := e.call
		if want == "" {
			want = derivedCall(key)
		}

		if len(e.on) == 0 {
			if strings.TrimSpace(e.why) == "" {
				t.Errorf("%s is recorded as reached by nothing, with no reason given.", key)
			}
			for surf, src := range source {
				if strings.Contains(src, want) {
					t.Errorf("%s is recorded as reached by nothing, but %s calls %q. "+
						"The reason is stale.", key, surf, want)
				}
			}
			unreachable = append(unreachable, key)
			continue
		}

		found := false
		for _, surf := range e.on {
			src, ok := source[surf]
			if !ok {
				t.Errorf("%s names surface %q, which has no source directories.", key, surf)
				continue
			}
			if strings.Contains(src, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is recorded as reached by %v, and no call site %q is there.",
				key, e.on, want)
			continue
		}
		covered++
	}

	// Counted and printed, not asserted against a threshold. A number in a log is
	// what milestone 25 did with the refusal rate, and for the same reason: the
	// figure is evidence whichever way it comes out.
	slices.Sort(unreachable)
	t.Logf("public callable symbols: %d, reached by a command: %d (%.1f%%), reached by none: %d",
		len(symbols), covered, 100*float64(covered)/float64(len(symbols)), len(unreachable))
	for _, key := range unreachable {
		t.Logf("  nothing reaches %s: %s", key, ledger[key].why)
	}
}

// derivedCall spells the call site for a symbol whose row does not override it.
//
//	engine.Scrub            -> engine.Scrub(
//	engine.Index.Merge      -> .Merge(
//	scorer/graph.NewPPR     -> graph.NewPPR(
func derivedCall(key string) string {
	pkg, rest, _ := strings.Cut(key, ".")
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		pkg = pkg[i+1:]
	}
	if strings.Contains(rest, ".") {
		_, method, _ := strings.Cut(rest, ".")
		return "." + method + "("
	}
	return pkg + "." + rest + "("
}

// goldenSymbols reads the two golden API files and returns every callable symbol,
// keyed the way the ledger keys them.
//
// The goldens rather than the packages themselves, because those files are what
// TestEngineAPISurfaceIsUnchanged and TestPublicAPISurfaceIsUnchanged already
// guard: a symbol arrives here only after it has been recorded as a deliberate
// widening of the surface, which is the same moment it becomes something a
// command ought to be able to reach.
func goldenSymbols(t *testing.T) []string {
	t.Helper()
	testdata := filepath.Join("..", "..", "pkg", "engine", "testdata")
	out := symbolsFrom(t, filepath.Join(testdata, "engine_api.txt"), "engine")
	out = append(out, symbolsFrom(t, filepath.Join(testdata, "public_api.txt"), "")...)
	slices.Sort(out)
	return out
}

// symbolsFrom parses one golden. A non-empty pkg names every symbol in the file;
// an empty one means the file carries `# ../<dir>` headers that name them.
func symbolsFrom(t *testing.T, path, pkg string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	header := pkg == ""
	var out []string
	for line := range strings.Lines(string(b)) {
		line = strings.TrimRight(line, "\n")
		switch {
		case header && strings.HasPrefix(line, "# "):
			pkg = strings.TrimPrefix(strings.TrimPrefix(line, "# "), "../")
		case strings.HasPrefix(line, "func "), strings.HasPrefix(line, "method "):
			_, sig, _ := strings.Cut(line, " ")
			name, _, _ := strings.Cut(sig, "(")
			out = append(out, pkg+"."+name)
		}
	}
	return out
}

// sourceWithoutComments returns one directory's non-test Go source with every
// comment removed.
//
// Removed because a comment is where this repository explains what it is *not*
// doing, and those sentences name the API they are declining: internal/opensearch
// writes "engine.Scrub is not this" beside the call it deliberately does not make.
// A raw text scan reads that as a call site and reports a command that does not
// exist — the exact failure this ledger is here to catch.
func sourceWithoutComments(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	fset := token.NewFileSet()
	var b bytes.Buffer
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", name, err)
		}
		if err := printer.Fprint(&b, fset, f); err != nil {
			t.Fatalf("printer.Fprint(%s): %v", name, err)
		}
		b.WriteString("\n")
	}
	return b.String()
}
