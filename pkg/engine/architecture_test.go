// This file is the milestone 1 pass/fail line. Everything else in the repo is a
// search engine; this is the experiment the search engine exists to run.
//
// Three assertions, from the plan:
//
//  1. Fusion is invariant to scorer count — the call shape for four scorers is
//     the shape for three.
//  2. The fourth scorer was cheap — under 100 lines, and zero lines changed in
//     engine/ or fusion/.
//  3. Fusion is ignorant of scorers — enforced by the import graph, not by
//     anyone reading the code carefully.
//
// It is package engine_test rather than package engine on purpose: it imports
// the scorer packages, and those import engine, so an internal test file here
// would be an import cycle. That the test is the only thing in this directory
// allowed to know scorers exist is itself the point.
package engine_test

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
	"github.com/skyoo2003/weft/pkg/scorer/graph"
	"github.com/skyoo2003/weft/pkg/scorer/recency"
	"github.com/skyoo2003/weft/pkg/scorer/text"
	"github.com/skyoo2003/weft/pkg/scorer/vector"
)

const (
	modulePath = "github.com/skyoo2003/weft"

	// Library code lives under /pkg, so every weft import path except the
	// module path itself carries this prefix.
	pkgPath = modulePath + "/pkg"
)

var refNow = time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)

// corpus is small and deliberately shaped. Note "lonely": it matches no query
// term, carries no vector, and no document links to it, so text, vector and
// graph are all blind to it. Only recency can see it. That is what makes
// assertion 1 measurable rather than decorative.
func corpus(t *testing.T) *engine.Index {
	t.Helper()
	ix := engine.New()
	for _, d := range []engine.Document{
		{Key: "a", Text: "scorer fusion architecture", Vector: []float32{1, 0, 0}, Links: []string{"b"}, Time: refNow.Add(-time.Hour)},
		{Key: "b", Text: "fusion operator ranking", Vector: []float32{0.9, 0.1, 0}, Links: []string{"c"}, Time: refNow.Add(-100 * 24 * time.Hour)},
		{Key: "c", Text: "graph proximity scorer", Vector: []float32{0, 1, 0}, Time: refNow.Add(-2 * time.Hour)},
		{Key: "d", Text: "entirely unrelated prose", Vector: []float32{0, 0, 1}, Time: refNow.Add(-500 * 24 * time.Hour)},
		{Key: "lonely", Text: "zzz", Time: refNow.Add(-10 * time.Minute)},
	} {
		if _, err := ix.Add(d); err != nil {
			t.Fatalf("Add(%q): %v", d.Key, err)
		}
	}
	return ix
}

func lonelyID(t *testing.T, ix *engine.Index) engine.DocID {
	t.Helper()
	id, ok := ix.Resolve("lonely")
	if !ok {
		t.Fatal("corpus is missing the lonely document")
	}
	return id
}

func contains(cands []engine.Candidate, want engine.DocID) bool {
	for _, c := range cands {
		if c.Doc == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Assertion 1: fusion is invariant to scorer count.
// ---------------------------------------------------------------------------

func TestAddingAFourthScorerDoesNotChangeTheCallShape(t *testing.T) {
	ix := corpus(t)
	txt := text.New(ix)

	three := []engine.Scorer{txt, vector.New(ix), graph.New(ix, txt)}
	four := []engine.Scorer{txt, vector.New(ix), graph.New(ix, txt), recency.NewAt(ix, refNow)}

	q := engine.Query{Text: "fusion scorer", Vector: []float32{1, 0, 0}}

	// These two calls are the same expression with a different slice: same
	// function, same fuser, same arguments. Nothing about invoking fusion
	// depended on there being three scorers. This compiling is the assertion.
	got3, err := engine.Search(t.Context(), q, 5, fusion.Fuse, three...)
	if err != nil {
		t.Fatalf("Search with 3 scorers: %v", err)
	}
	got4, err := engine.Search(t.Context(), q, 5, fusion.Fuse, four...)
	if err != nil {
		t.Fatalf("Search with 4 scorers: %v", err)
	}

	// And the fourth scorer has to actually reach the ranking, or this test
	// would pass just as happily against a scorer that returned nothing.
	lonely := lonelyID(t, ix)
	if contains(got3, lonely) {
		t.Fatalf("three scorers already found the lonely doc — the corpus no longer isolates recency: %+v", got3)
	}
	if !contains(got4, lonely) {
		t.Fatalf("four scorers did not surface the lonely doc — recency is not reaching fusion: %+v", got4)
	}
}

func TestAnyNumberOfScorersFuses(t *testing.T) {
	ix := corpus(t)
	txt := text.New(ix)
	all := []engine.Scorer{txt, vector.New(ix), graph.New(ix, txt), recency.NewAt(ix, refNow)}
	q := engine.Query{Text: "fusion scorer", Vector: []float32{1, 0, 0}}

	for n := 0; n <= len(all); n++ {
		got, err := engine.Search(t.Context(), q, 5, fusion.Fuse, all[:n]...)
		if err != nil {
			t.Fatalf("Search with %d scorers: %v", n, err)
		}
		if n == 0 && len(got) != 0 {
			t.Fatalf("Search with no scorers returned %+v", got)
		}
		if n > 0 && len(got) == 0 {
			t.Fatalf("Search with %d scorers returned nothing", n)
		}
	}
}

// ---------------------------------------------------------------------------
// Assertion 2: the fourth scorer was cheap.
// ---------------------------------------------------------------------------

func TestFourthScorerIsUnderOneHundredLines(t *testing.T) {
	const budget = 100

	// Implementation lines only, not tests. The metric asks what a new scorer
	// costs to build; counting its tests against it would make the budget
	// reward untested scorers.
	impl := countGoLines(t, filepath.Join("..", "scorer", "recency"), false)
	tests := countGoLines(t, filepath.Join("..", "scorer", "recency"), true)
	t.Logf("scorer/recency: %d implementation lines (budget %d), %d test lines", impl, budget, tests)

	if impl >= budget {
		t.Fatalf("scorer/recency is %d implementation lines, budget is %d", impl, budget)
	}
}

func TestNeitherEngineNorFusionImportsAScorer(t *testing.T) {
	// This is the mechanical stand-in for "the diff to engine/ and fusion/ was
	// zero lines". A diff needs a baseline commit; this needs nothing, and it
	// keeps holding for every future scorer. It reads the import graph, so the
	// words "text" and "vector" appearing in engine's comments do not trip it.
	for _, dir := range []string{".", filepath.Join("..", "fusion")} {
		for _, imp := range importsOf(t, dir) {
			if strings.HasPrefix(imp, pkgPath+"/scorer/") {
				t.Errorf("%s imports %s — engine and fusion must not know that any scorer exists", dir, imp)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Assertion 3: fusion is ignorant of scorers, according to the go tool.
// ---------------------------------------------------------------------------

func TestGoListDepsProvesFusionIgnorance(t *testing.T) {
	tests := []struct {
		pkg   string
		allow []string // local packages this one is permitted to depend on
	}{
		// fusion may know the Candidate type and nothing else in the module.
		{"./fusion", []string{pkgPath + "/engine"}},
		// engine is the leaf: it depends on no weft package at all.
		{"./engine", nil},
	}

	for _, tc := range tests {
		t.Run(tc.pkg, func(t *testing.T) {
			own := pkgPath + "/" + strings.TrimPrefix(tc.pkg, "./")
			allowed := map[string]bool{own: true}
			for _, a := range tc.allow {
				allowed[a] = true
			}
			for _, dep := range goList(t, "-deps", tc.pkg) {
				if !strings.HasPrefix(dep, modulePath) {
					continue // standard library
				}
				if !allowed[dep] {
					t.Errorf("go list -deps %s includes %s, which is not allowed", tc.pkg, dep)
				}
			}
		})
	}
}

func TestNoExternalDependencies(t *testing.T) {
	// The PRD's operational metric: `go list -m all` prints this module and
	// nothing else.
	mods := goList(t, "-m", "all")
	if len(mods) != 1 || mods[0] != modulePath {
		t.Fatalf("go list -m all = %v, want exactly [%s]", mods, modulePath)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// goList runs the go tool from pkg/, where "./fusion" and "./engine" resolve.
// Tests execute in their own package directory, so that is one level up.
func goList(t *testing.T, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = ".."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Fields(string(out))
}

// countGoLines totals the lines of .go files in dir, either the test files or
// the implementation files but never both.
func countGoLines(t *testing.T, dir string, testFiles bool) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	total := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") != testFiles {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		total += strings.Count(string(b), "\n")
	}
	return total
}

// importsOf returns the import paths of every non-test .go file in dir.
func importsOf(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	fset := token.NewFileSet()
	var imports []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", name, err)
		}
		for _, spec := range f.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("bad import path %s in %s", spec.Path.Value, name)
			}
			imports = append(imports, path)
		}
	}
	return imports
}
