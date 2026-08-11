// This file is the milestone 1 pass/fail line. Everything else in the repo is a
// search engine; this is the experiment the search engine exists to run.
//
// Three assertions, from the plan:
//
//  1. Fusion is invariant to scorer count — the call shape for four scorers is
//     the shape for three.
//  2. The fourth scorer was cheap — under 100 lines, no change to fusion/, and
//     whatever it cost engine/ recorded in a golden API file rather than
//     asserted to be nothing.
//  3. Fusion is ignorant of scorers — enforced by the import graph, not by
//     anyone reading the code carefully.
//
// It is package engine_test rather than package engine on purpose: it imports
// the scorer packages, and those import engine, so an internal test file here
// would be an import cycle. That the test is the only thing in this directory
// allowed to know scorers exist is itself the point.
package engine_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	// This proves package-level ignorance, not a zero diff: engine and fusion
	// never name a scorer, for every future scorer, without needing a baseline
	// commit. What it cannot see — a widened Document or Scorer — is
	// TestEngineAPISurfaceIsUnchanged's job. It reads the import graph, so the
	// words "text" and "vector" appearing in engine's comments do not trip it.
	for _, dir := range []string{".", filepath.Join("..", "fusion")} {
		for _, imp := range importsOf(t, dir) {
			if strings.HasPrefix(imp, pkgPath+"/scorer/") {
				t.Errorf("%s imports %s — engine and fusion must not know that any scorer exists", dir, imp)
			}
		}
	}
}

// TestEngineAPISurfaceIsUnchanged measures the part of a new scorer's cost the
// import check cannot see.
//
// The import check proves engine and fusion never name a scorer package. It does
// not prove that adding a scorer required no engine change, and that distinction
// matters: a scorer needing new input data has to read it from engine.Document,
// because scorers are not allowed their own store (docs/FINDINGS.md section 2.2).
// Document.Time exists solely for the recency scorer and was written before that
// scorer existed, which flattered the original "zero lines changed" figure.
//
// This golden file fails when engine's exported surface changes, so the engine
// cost of a new scorer becomes a deliberate edit instead of passing unnoticed.
// It records signatures and member types, not just names, so the three ways a
// scorer can quietly widen the shared contract are all covered: a field on
// Document, a method on the Scorer interface, and a parameter on Search or
// Fuser. Refresh with:
//
//	WEFT_UPDATE_GOLDEN=1 go test ./pkg/engine/
func TestEngineAPISurfaceIsUnchanged(t *testing.T) {
	got := strings.Join(exportedAPI(t, "."), "\n") + "\n"
	golden := filepath.Join("testdata", "engine_api.txt")

	if os.Getenv("WEFT_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		t.Logf("updated %s", golden)
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v — regenerate with WEFT_UPDATE_GOLDEN=1", golden, err)
	}
	if got != string(want) {
		t.Errorf("engine's exported API changed.\n--- recorded\n%s\n--- current\n%s\n"+
			"A new scorer needing a new Document field is a real engine cost. Record it in "+
			"docs/FINDINGS.md, then refresh with WEFT_UPDATE_GOLDEN=1.", want, got)
	}
}

// exportedAPI lists dir's exported declarations one per line, sorted, with
// member and signature types included — see typeAPI for why names alone are not
// enough.
func exportedAPI(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	fset := token.NewFileSet()
	var api []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", name, err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv == nil {
					api = append(api, "func "+d.Name.Name+signature(d.Type))
				} else if recv := receiverName(d.Recv); ast.IsExported(recv) {
					api = append(api, "method "+recv+"."+d.Name.Name+signature(d.Type))
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if !s.Name.IsExported() {
							continue
						}
						api = append(api, typeAPI(s.Name.Name, s.Type)...)
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								api = append(api, d.Tok.String()+" "+n.Name)
							}
						}
					}
				}
			}
		}
	}
	sort.Strings(api)
	return api
}

// typeAPI renders one exported type: the declaration, then the members that make
// up its contract. Member types are recorded alongside member names, because
// retyping Document.Vector or adding a parameter to Fuser changes what engine
// promises without changing a single name.
func typeAPI(name string, expr ast.Expr) []string {
	switch t := expr.(type) {
	case *ast.StructType:
		out := []string{"type " + name + " struct"}
		for _, fld := range t.Fields.List {
			if len(fld.Names) == 0 {
				// Embedded: contributes a whole surface of its own, so it cannot
				// be skipped for having no name.
				out = append(out, "embeds "+name+"."+types.ExprString(fld.Type))
				continue
			}
			for _, f := range fld.Names {
				if f.IsExported() {
					out = append(out, "field "+name+"."+f.Name+" "+types.ExprString(fld.Type))
				}
			}
		}
		return out

	case *ast.InterfaceType:
		// A method added to engine.Scorer is the most expensive change a new
		// scorer can force on engine: every existing scorer stops compiling.
		// Recording method signatures is what makes that cost visible, and
		// recording embedded interfaces covers the Streamer extension sketched in
		// docs/FINDINGS.md section 3.1.
		out := []string{"type " + name + " interface"}
		for _, m := range t.Methods.List {
			ft, ok := m.Type.(*ast.FuncType)
			if !ok {
				out = append(out, "embeds "+name+"."+types.ExprString(m.Type))
				continue
			}
			for _, n := range m.Names {
				if n.IsExported() {
					out = append(out, "method "+name+"."+n.Name+signature(ft))
				}
			}
		}
		return out

	default:
		// A defined type or a func type: DocID's width and Fuser's signature are
		// both part of the contract.
		return []string{"type " + name + " " + types.ExprString(expr)}
	}
}

// signature renders a function's parameters and results, dropping the leading
// "func" keyword so the line reads "func Search(...) (...)".
func signature(ft *ast.FuncType) string {
	return strings.TrimPrefix(types.ExprString(ft), "func")
}

// receiverName returns the bare type name of a method receiver.
func receiverName(fl *ast.FieldList) string {
	if fl == nil || len(fl.List) == 0 {
		return ""
	}
	switch t := fl.List[0].Type.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
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
	// GOWORK=off because `go list -m all` in workspace mode reports every
	// workspace root module. Without this, checking out weft beneath a parent
	// go.work fails the dependency assertion even though weft still has no
	// external dependencies.
	cmd.Env = append(os.Environ(), "GOWORK=off")
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
