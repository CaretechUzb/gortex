package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// memberOf builds a WorkspaceMembership that derives a file's
// workspace-member id from the leading two path segments — the test
// stand-in for the indexer's manifest-driven membership index.
func memberOf(filePath string) string {
	parts := strings.Split(filePath, "/")
	if len(parts) >= 2 && parts[0] == "packages" {
		return parts[0] + "/" + parts[1]
	}
	return ""
}

// TestResolveImport_PrefersSameWorkspaceCandidate pins the
// same-workspace preference: a bare import name that matches a
// directory in two different package-manager workspace members
// resolves to the candidate sharing the importer's own member.
func TestResolveImport_PrefersSameWorkspaceCandidate(t *testing.T) {
	g := graph.New()
	// Importer lives in workspace member `packages/app`.
	g.AddNode(&graph.Node{
		ID: "packages/app/src/main.ts::main", Kind: graph.KindFunction, Name: "main",
		FilePath: "packages/app/src/main.ts", Language: "typescript",
	})
	// Two same-named `logger` packages in two different members.
	g.AddNode(&graph.Node{
		ID: "packages/app/logger/index.ts", Kind: graph.KindFile, Name: "index.ts",
		FilePath: "packages/app/logger/index.ts", Language: "typescript",
	})
	g.AddNode(&graph.Node{
		ID: "packages/admin/logger/index.ts", Kind: graph.KindFile, Name: "index.ts",
		FilePath: "packages/admin/logger/index.ts", Language: "typescript",
	})

	edge := &graph.Edge{
		From: "packages/app/src/main.ts::main", To: "unresolved::import::logger",
		Kind: graph.EdgeImports, FilePath: "packages/app/src/main.ts", Line: 1,
	}
	g.AddEdge(edge)

	r := New(g)
	r.SetWorkspaceMembership(memberOf)
	r.ResolveAll()

	assert.Equal(t, "packages/app/logger/index.ts", edge.To,
		"import should resolve to the logger in the importer's own workspace member")
}

// TestResolveImport_NoWorkspaceLookupKeepsBaseline confirms that
// without a workspace lookup the resolver's pre-existing first-hit
// behaviour is unchanged — no regression for non-workspace repos.
func TestResolveImport_NoWorkspaceLookupKeepsBaseline(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "src/main.ts::main", Kind: graph.KindFunction, Name: "main",
		FilePath: "src/main.ts", Language: "typescript",
	})
	g.AddNode(&graph.Node{
		ID: "vendor/logger/index.ts", Kind: graph.KindFile, Name: "index.ts",
		FilePath: "vendor/logger/index.ts", Language: "typescript",
	})

	edge := &graph.Edge{
		From: "src/main.ts::main", To: "unresolved::import::logger",
		Kind: graph.EdgeImports, FilePath: "src/main.ts", Line: 1,
	}
	g.AddEdge(edge)

	// No SetWorkspaceMembership — the resolver must still resolve the
	// single directory match exactly as before the feature.
	New(g).ResolveAll()

	assert.Equal(t, "vendor/logger/index.ts", edge.To,
		"single directory match must resolve regardless of workspace lookup")
}

// TestResolveImport_WorkspaceLookupNoMatchKeepsFirstHit confirms that
// when the importer belongs to no workspace member the resolver falls
// back to its first-hit tie-break rather than failing the import.
func TestResolveImport_WorkspaceLookupNoMatchKeepsFirstHit(t *testing.T) {
	g := graph.New()
	// Importer is outside every `packages/*` member directory.
	g.AddNode(&graph.Node{
		ID: "tools/cli/main.ts::main", Kind: graph.KindFunction, Name: "main",
		FilePath: "tools/cli/main.ts", Language: "typescript",
	})
	g.AddNode(&graph.Node{
		ID: "packages/app/logger/index.ts", Kind: graph.KindFile, Name: "index.ts",
		FilePath: "packages/app/logger/index.ts", Language: "typescript",
	})

	edge := &graph.Edge{
		From: "tools/cli/main.ts::main", To: "unresolved::import::logger",
		Kind: graph.EdgeImports, FilePath: "tools/cli/main.ts", Line: 1,
	}
	g.AddEdge(edge)

	r := New(g)
	r.SetWorkspaceMembership(memberOf)
	r.ResolveAll()

	assert.Equal(t, "packages/app/logger/index.ts", edge.To,
		"importer outside any workspace member should still resolve via the existing tie-break")
}

// TestPickSameWorkspaceFile_AmbiguousLeavesTieBreak pins the unit
// contract: when several candidates share the importer's workspace the
// helper reports no unique match so the caller's existing tie-break decides.
func TestPickSameWorkspaceFile_AmbiguousLeavesTieBreak(t *testing.T) {
	caller := "packages/app/src/main.ts"
	candidates := []graph.FileNodeIdentity{
		{ID: "packages/app/a/logger.ts", FilePath: "packages/app/a/logger.ts"},
		{ID: "packages/app/b/logger.ts", FilePath: "packages/app/b/logger.ts"},
	}
	if got, ok := pickSameWorkspaceFile(memberOf, caller, candidates); ok {
		t.Errorf("two candidates in the importer's workspace should be ambiguous, got %q", got.ID)
	}

	// A single same-workspace candidate among decoys resolves cleanly.
	candidates = []graph.FileNodeIdentity{
		{ID: "packages/admin/logger.ts", FilePath: "packages/admin/logger.ts"},
		{ID: "packages/app/logger.ts", FilePath: "packages/app/logger.ts"},
	}
	got, ok := pickSameWorkspaceFile(memberOf, caller, candidates)
	if !ok || got.ID != "packages/app/logger.ts" {
		t.Errorf("expected the packages/app candidate, got %v", got)
	}
}
