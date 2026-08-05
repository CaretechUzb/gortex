package indexer

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/modules"
	"github.com/zzet/gortex/internal/parser"
)

// writeWorkspaceFile materialises a manifest under dir at a
// repo-relative slash path, creating intermediate directories — the
// shared writeFile helper expects an already-existing parent dir.
func writeWorkspaceFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	writeFile(t, abs, content)
}

// workspaceMemberTargets indexes dir and returns the sorted To
// endpoints of every EdgePackageWorkspaceMember edge in the graph.
func workspaceMemberTargets(t *testing.T, dir string) []string {
	t.Helper()
	g := graph.New()
	idx := New(g, parser.NewRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	var targets []string
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgePackageWorkspaceMember {
			targets = append(targets, e.To)
		}
	}
	sort.Strings(targets)
	return targets
}

// TestIndex_PackageWorkspaceMembership pins index-time materialisation
// of root→member edges across all three package-manager ecosystems,
// including glob and explicit members, plus the no-regression case.
func TestIndex_PackageWorkspaceMembership(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string // sorted EdgePackageWorkspaceMember targets
	}{
		{
			name: "npm workspace, glob plus explicit member",
			files: map[string]string{
				"package.json":               `{"name":"root","workspaces":["packages/*","apps/web"]}`,
				"packages/ui/package.json":   `{"name":"ui"}`,
				"packages/core/package.json": `{"name":"core"}`,
				"apps/web/package.json":      `{"name":"web"}`,
			},
			want: []string{
				"apps/web/package.json",
				"packages/core/package.json",
				"packages/ui/package.json",
			},
		},
		{
			name: "pnpm workspace, glob members",
			files: map[string]string{
				"pnpm-workspace.yaml":     "packages:\n  - 'packages/*'\n",
				"packages/a/package.json": `{"name":"a"}`,
				"packages/b/package.json": `{"name":"b"}`,
			},
			want: []string{
				"packages/a/package.json",
				"packages/b/package.json",
			},
		},
		{
			name: "cargo workspace, glob plus explicit member",
			files: map[string]string{
				"Cargo.toml":               "[workspace]\nmembers = [\"crates/*\", \"tools/gen\"]\n",
				"crates/engine/Cargo.toml": "[package]\nname = \"engine\"\n",
				"crates/cli/Cargo.toml":    "[package]\nname = \"cli\"\n",
				"tools/gen/Cargo.toml":     "[package]\nname = \"gen\"\n",
			},
			want: []string{
				"crates/cli/Cargo.toml",
				"crates/engine/Cargo.toml",
				"tools/gen/Cargo.toml",
			},
		},
		{
			name: "non-workspace repo produces no membership edges",
			files: map[string]string{
				"package.json": `{"name":"solo","version":"1.0.0","dependencies":{"react":"^18"}}`,
				"index.js":     "module.exports = 1;\n",
			},
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for rel, content := range c.files {
				writeWorkspaceFile(t, dir, rel, content)
			}
			got := workspaceMemberTargets(t, dir)
			if len(got) != len(c.want) {
				t.Fatalf("got %d membership edges %v, want %d %v", len(got), got, len(c.want), c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("membership edge[%d] = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestIndex_PackageWorkspaceRootNode verifies the synthetic
// workspace-root node carries the expected kind and meta and that
// every membership edge originates from it.
func TestIndex_PackageWorkspaceRootNode(t *testing.T) {
	dir := t.TempDir()
	writeWorkspaceFile(t, dir, "package.json", `{"name":"root","workspaces":["packages/*"]}`)
	writeWorkspaceFile(t, dir, "packages/a/package.json", `{"name":"a"}`)
	writeWorkspaceFile(t, dir, "packages/b/package.json", `{"name":"b"}`)

	g := graph.New()
	idx := New(g, parser.NewRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	rootID := modules.WorkspaceRootNodeID(modules.WorkspaceNPM, ".")
	root := g.GetNode(rootID)
	require.NotNil(t, root, "workspace root node %s must be materialised", rootID)
	require.Equal(t, graph.KindPackage, root.Kind)
	if v, _ := root.Meta["package_workspace"].(bool); !v {
		t.Errorf("root node should carry package_workspace=true meta")
	}
	require.Equal(t, "npm", root.Meta["ecosystem"])

	var memberEdges int
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgePackageWorkspaceMember {
			continue
		}
		memberEdges++
		require.Equal(t, rootID, e.From, "membership edge must originate at the workspace root")
		require.Equal(t, graph.OriginASTResolved, e.Origin)
	}
	require.Equal(t, 2, memberEdges, "expected one membership edge per resolved member")
}
