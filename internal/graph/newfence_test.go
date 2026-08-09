package graph

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The in-memory Store is a staging buffer, not a persistence backend. The
// indexer parses a cold index into one and drains it into the durable store —
// that staging hop is worth several times the index wall-clock, which is why
// the type still exists outside tests. Everything else that needs a Store must
// take a real one.
//
// stagingCallers lists the non-test files allowed to construct it. Keeping the
// list this short is the point: a new entry means some production path is about
// to hold graph data that no restart can recover.
var stagingCallers = []string{
	"internal/indexer/indexer.go",
}

// graphImportPath is the package under fence. The scan resolves whatever local
// name each file binds it to, so an alias or a dot import is caught the same as
// the ordinary qualified form.
const graphImportPath = "github.com/zzet/gortex/internal/graph"

// fenceSkippedDirs are directory names the scan never descends into: version
// control, fixture trees that intentionally contain unbuildable Go, and vendored
// or generated third-party code.
var fenceSkippedDirs = map[string]struct{}{
	".git":         {},
	"testdata":     {},
	"vendor":       {},
	"node_modules": {},
}

func TestNewIsFencedToIndexerStaging(t *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		t.Fatalf("locating repository root: %v", err)
	}

	allowed := make(map[string]struct{}, len(stagingCallers))
	for _, p := range stagingCallers {
		allowed[p] = struct{}{}
	}

	var offenders []string
	seen := make(map[string]struct{}, len(stagingCallers))

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if _, skip := fenceSkippedDirs[d.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		// This package calls New unqualified and does not import itself, so it
		// can never match; skipping it keeps the scan honest about that.
		if path.Dir(rel) == "internal/graph" {
			return nil
		}
		constructs, parseErr := fileConstructsStagingGraph(p)
		if parseErr != nil {
			t.Errorf("parsing %s: %v", rel, parseErr)
			return nil
		}
		if !constructs {
			return nil
		}
		if _, ok := allowed[rel]; ok {
			seen[rel] = struct{}{}
			return nil
		}
		offenders = append(offenders, rel)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("scanning %s: %v", root, walkErr)
	}

	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Fatalf("in-memory Store constructed outside the indexer's staging path:\n  %s\n\n"+
			"That Store keeps everything in process memory: nothing written to it survives a "+
			"restart, and no other process can read it. Production code that needs a graph should "+
			"accept a Store from its caller or open a durable one. If you really are adding an "+
			"indexer staging buffer that is drained into a durable store, add the file to "+
			"stagingCallers in this test and say why in the commit.",
			strings.Join(offenders, "\n  "))
	}

	for _, p := range stagingCallers {
		if _, ok := seen[p]; !ok {
			t.Errorf("%s no longer constructs the in-memory Store — drop it from stagingCallers", p)
		}
	}
}

// fileConstructsStagingGraph reports whether the file names the graph package's
// New symbol in executable code.
//
// The check is syntactic rather than textual: it resolves the local name the
// file binds the graph import to and then looks for that name's New. A textual
// scan for "graph.New()" reads as equivalent but is not — an aliased import
// (g "…/internal/graph"; g.New()) or a dot import walks straight past it, and
// the fence exists precisely to stop a new in-memory store from appearing
// somewhere nobody looked.
//
// A reference counts whether or not it is immediately called: taking New as a
// function value and invoking it elsewhere constructs exactly the same store.
func fileConstructsStagingGraph(filePath string) (bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filePath, nil, parser.SkipObjectResolution)
	if err != nil {
		return false, err
	}

	qualifiers := make(map[string]struct{})
	dotImported := false
	for _, spec := range file.Imports {
		importPath, uErr := strconv.Unquote(spec.Path.Value)
		if uErr != nil || importPath != graphImportPath {
			continue
		}
		switch {
		case spec.Name == nil:
			qualifiers["graph"] = struct{}{} // package name, not the last path element
		case spec.Name.Name == ".":
			dotImported = true
		case spec.Name.Name == "_":
			// Blank import: the package cannot be named, so nothing to fence.
		default:
			qualifiers[spec.Name.Name] = struct{}{}
		}
	}
	if len(qualifiers) == 0 && !dotImported {
		return false, nil
	}

	found := false
	// Selector fields (the Sel in x.New) are idents too, and Inspect walks
	// them. Remembering the ones already accounted for keeps the dot-import
	// arm below from reading someOther.New as an unqualified New.
	qualified := make(map[*ast.Ident]struct{})
	ast.Inspect(file, func(n ast.Node) bool {
		if found {
			return false
		}
		switch node := n.(type) {
		case *ast.SelectorExpr:
			if node.Sel == nil || node.Sel.Name != "New" {
				return true
			}
			qualified[node.Sel] = struct{}{}
			pkg, ok := node.X.(*ast.Ident)
			if !ok {
				return true
			}
			if _, ok := qualifiers[pkg.Name]; ok {
				found = true
				return false
			}
		case *ast.Ident:
			// Only meaningful for a dot import: an unqualified New in a file
			// that dot-imports the graph package resolves to graph.New.
			if _, isSelector := qualified[node]; isSelector {
				return true
			}
			if dotImported && node.Name == "New" {
				found = true
				return false
			}
		}
		return true
	})
	return found, nil
}
