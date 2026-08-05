package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestRunnerPropagation_ParserStamped — the parser already stamped
// Meta["test_runner"] on the file node (JS / TS path). The global pass
// must copy it to every is_test function/method in that file, but not
// to any production function.
func TestRunnerPropagation_ParserStamped(t *testing.T) {
	g := graph.New()
	fileID := "src/foo.test.ts"
	g.AddNode(&graph.Node{
		ID:       fileID,
		Kind:     graph.KindFile,
		Name:     fileID,
		FilePath: fileID,
		Language: "typescript",
		Meta:     map[string]any{"test_runner": "bun-test"},
	})
	g.AddNode(&graph.Node{
		ID:       fileID + "::renders",
		Kind:     graph.KindFunction,
		Name:     "renders",
		FilePath: fileID,
		Language: "typescript",
	})
	// A production file with a same-shaped function — must NOT be marked.
	g.AddNode(&graph.Node{ID: "src/prod.ts", Kind: graph.KindFile, FilePath: "src/prod.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/prod.ts::renders", Kind: graph.KindFunction, Name: "renders", FilePath: "src/prod.ts", Language: "typescript"})

	marked, _ := markTestSymbolsAndEmitEdges(g)
	if marked != 1 {
		t.Fatalf("marked = %d, want 1", marked)
	}

	testFn := g.GetNode(fileID + "::renders")
	if got, _ := testFn.Meta["test_runner"].(string); got != "bun-test" {
		t.Fatalf("test_runner on test fn = %q, want %q", got, "bun-test")
	}
	if got, _ := testFn.Meta["test_role"].(string); got != "test" {
		t.Fatalf("test_role on test fn = %q, want %q", got, "test")
	}

	prod := g.GetNode("src/prod.ts::renders")
	if _, ok := prod.Meta["test_runner"]; ok {
		t.Fatalf("production fn must not carry test_runner: %+v", prod.Meta)
	}
}

// TestRunnerPropagation_GoDefault — every Go test file gets `gotest`
// without any parser hint, because `go test` is the only Go runner.
func TestRunnerPropagation_GoDefault(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/foo_test.go", Kind: graph.KindFile, FilePath: "pkg/foo_test.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/foo_test.go::TestFoo", Kind: graph.KindFunction, Name: "TestFoo", FilePath: "pkg/foo_test.go", Language: "go"})

	markTestSymbolsAndEmitEdges(g)

	testFn := g.GetNode("pkg/foo_test.go::TestFoo")
	if got, _ := testFn.Meta["test_runner"].(string); got != "gotest" {
		t.Fatalf("Go test_runner = %q, want %q", got, "gotest")
	}
}

// TestRunnerPropagation_PythonFromImports — Python files classify by
// outgoing import edges. unittest beats pytest only when no pytest
// import is present.
func TestRunnerPropagation_PythonFromImports(t *testing.T) {
	cases := []struct {
		name       string
		importPath string
		wantRunner string
	}{
		{"pytest import", "pytest", "pytest"},
		{"pytest submodule", "pytest.mark", "pytest"},
		{"unittest import", "unittest", "unittest"},
		{"unittest.mock import", "unittest.mock", "unittest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			fileID := "tests/test_x.py"
			g.AddNode(&graph.Node{ID: fileID, Kind: graph.KindFile, FilePath: fileID, Language: "python"})
			g.AddNode(&graph.Node{ID: fileID + "::test_x", Kind: graph.KindFunction, Name: "test_x", FilePath: fileID, Language: "python"})
			g.AddEdge(&graph.Edge{From: fileID, To: "unresolved::import::" + tc.importPath, Kind: graph.EdgeImports, FilePath: fileID, Line: 1})

			markTestSymbolsAndEmitEdges(g)

			fn := g.GetNode(fileID + "::test_x")
			if got, _ := fn.Meta["test_runner"].(string); got != tc.wantRunner {
				t.Fatalf("test_runner = %q, want %q", got, tc.wantRunner)
			}
		})
	}
}

// TestRunnerPropagation_PythonDefault — a test file with no pytest /
// unittest import still defaults to `pytest`, because pytest's
// auto-discovery is the de-facto Python runner.
func TestRunnerPropagation_PythonDefault(t *testing.T) {
	g := graph.New()
	fileID := "tests/test_y.py"
	g.AddNode(&graph.Node{ID: fileID, Kind: graph.KindFile, FilePath: fileID, Language: "python"})
	g.AddNode(&graph.Node{ID: fileID + "::test_y", Kind: graph.KindFunction, Name: "test_y", FilePath: fileID, Language: "python"})

	markTestSymbolsAndEmitEdges(g)
	fn := g.GetNode(fileID + "::test_y")
	if got, _ := fn.Meta["test_runner"].(string); got != "pytest" {
		t.Fatalf("Python default test_runner = %q, want pytest", got)
	}
}

// TestRunnerPropagation_RubyFileSuffix — Ruby falls back to `_spec.rb`
// → rspec, `_test.rb` → minitest with no import signal.
func TestRunnerPropagation_RubyFileSuffix(t *testing.T) {
	cases := []struct{ file, want string }{
		{"spec/user_spec.rb", "rspec"},
		{"test/user_test.rb", "minitest"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: tc.file, Kind: graph.KindFile, FilePath: tc.file, Language: "ruby"})
			g.AddNode(&graph.Node{ID: tc.file + "::test_x", Kind: graph.KindFunction, Name: "test_x", FilePath: tc.file, Language: "ruby"})

			markTestSymbolsAndEmitEdges(g)
			fn := g.GetNode(tc.file + "::test_x")
			if got, _ := fn.Meta["test_runner"].(string); got != tc.want {
				t.Fatalf("test_runner = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunnerPropagation_JSImportEdgeFallback — when the parser didn't
// stamp the file (e.g. an older index built before this change), the
// global pass still derives the runner from outgoing import edges.
func TestRunnerPropagation_JSImportEdgeFallback(t *testing.T) {
	cases := []struct {
		name string
		path string
		imp  string
		want string
	}{
		{"bun-test via edge", "src/x.test.ts", "bun:test", "bun-test"},
		{"vitest via edge", "src/x.test.ts", "vitest", "vitest"},
		{"jest via edge", "src/x.test.ts", "@jest/globals", "jest"},
		{"mocha via edge", "test/x.js", "mocha", "mocha"},
		{"node-test via edge", "test/x.mjs", "node:test", "node-test"},
		{"playwright via edge", "e2e/x.spec.ts", "@playwright/test", "playwright"},
		{"cypress via edge", "cypress/foo.spec.js", "cypress", "cypress"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: tc.path, Kind: graph.KindFile, FilePath: tc.path, Language: "typescript"})
			g.AddNode(&graph.Node{ID: tc.path + "::it_x", Kind: graph.KindFunction, Name: "it_x", FilePath: tc.path, Language: "typescript"})
			g.AddEdge(&graph.Edge{From: tc.path, To: "unresolved::import::" + tc.imp, Kind: graph.EdgeImports, FilePath: tc.path, Line: 1})

			markTestSymbolsAndEmitEdges(g)
			fn := g.GetNode(tc.path + "::it_x")
			if got, _ := fn.Meta["test_runner"].(string); got != tc.want {
				t.Fatalf("test_runner = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunnerPropagation_ProductionFileUnchanged — a non-test file with
// a test_runner-looking import edge must not be classified. Only test
// files (per IsTestFile) get a runner stamped on their functions.
func TestRunnerPropagation_ProductionFileUnchanged(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/api.ts", Kind: graph.KindFile, FilePath: "src/api.ts", Language: "typescript"})
	g.AddNode(&graph.Node{ID: "src/api.ts::serve", Kind: graph.KindFunction, Name: "serve", FilePath: "src/api.ts", Language: "typescript"})
	g.AddEdge(&graph.Edge{From: "src/api.ts", To: "unresolved::import::mocha", Kind: graph.EdgeImports, FilePath: "src/api.ts", Line: 1})

	markTestSymbolsAndEmitEdges(g)
	fn := g.GetNode("src/api.ts::serve")
	if _, ok := fn.Meta["test_runner"]; ok {
		t.Fatalf("production fn must not carry test_runner: %+v", fn.Meta)
	}
}

// TestRunnerPropagation_FileNodeMetaStamped — the file node itself
// also gets Meta["test_runner"], so file-level analyses can group
// tests by runner without walking functions.
func TestRunnerPropagation_FileNodeMetaStamped(t *testing.T) {
	g := graph.New()
	fileID := "src/x.test.ts"
	g.AddNode(&graph.Node{
		ID: fileID, Kind: graph.KindFile, FilePath: fileID, Language: "typescript",
		Meta: map[string]any{"test_runner": "vitest"},
	})

	markTestSymbolsAndEmitEdges(g)

	fn := g.GetNode(fileID)
	if got, _ := fn.Meta["test_runner"].(string); got != "vitest" {
		t.Fatalf("file node test_runner = %q, want vitest", got)
	}
	if v, _ := fn.Meta["is_test_file"].(bool); !v {
		t.Fatalf("file node is_test_file must be true")
	}
}
