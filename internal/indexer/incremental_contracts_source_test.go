package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestExtractContractsSkipsSourceForUnsupportedLanguageAndKeepsDI(t *testing.T) {
	root := t.TempDir()
	// A directory can be statted but cannot be read as a source file. The
	// unsupported-language path must therefore succeed only if no source read
	// is attempted.
	if err := os.Mkdir(filepath.Join(root, "parser.c"), 0o755); err != nil {
		t.Fatalf("create unreadable source stand-in: %v", err)
	}

	const graphPath = "repo/parser.c"
	fileNode := &graph.Node{
		ID:         "file::parser.c",
		Kind:       graph.KindFile,
		FilePath:   graphPath,
		Language:   "c",
		RepoPrefix: "repo",
	}
	symbolNode := &graph.Node{
		ID:         "symbol::consumer",
		Kind:       graph.KindFunction,
		FilePath:   graphPath,
		Language:   "c",
		RepoPrefix: "repo",
	}
	diEdge := &graph.Edge{
		From:     symbolNode.ID,
		To:       "token::Service",
		Kind:     graph.EdgeConsumes,
		FilePath: graphPath,
		Line:     12,
		Meta: map[string]any{
			"via":             "@Inject",
			graph.MetaDIToken: "Service",
		},
	}
	idx := &Indexer{
		rootPath:    root,
		repoPrefix:  "repo",
		workspaceID: "workspace",
		projectID:   "project",
	}

	fresh, mtimeNano, exists, preservePrior := idx.extractContractsForGraphFileFromBatch(
		graphPath,
		nil,
		[]*graph.Node{fileNode, symbolNode},
		map[string][]*graph.Edge{symbolNode.ID: {diEdge}},
	)
	if !exists || preservePrior {
		t.Fatalf("exists=%v preservePrior=%v, want true/false", exists, preservePrior)
	}
	if mtimeNano == 0 {
		t.Fatal("expected stat mtime for unsupported-language cache entry")
	}
	if len(fresh) != 1 {
		t.Fatalf("contracts=%d, want one graph-derived DI contract", len(fresh))
	}
	if got := fresh[0]; got.ID != "di::Service" || got.FilePath != graphPath || got.WorkspaceID != "workspace" || got.ProjectID != "project" {
		t.Fatalf("unexpected DI contract: %+v", got)
	}
}
