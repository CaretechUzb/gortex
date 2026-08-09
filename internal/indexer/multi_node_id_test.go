package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// setupGoRepoWithTypes creates a Go repo with a Handler type and a method —
// enough to exercise multi-repo indexing without needing the Go toolchain
// for semantic enrichment.
func setupGoRepoWithTypes(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "api"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "go.mod"),
		[]byte("module example.com/"+name+"\n\ngo 1.21\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "api", "handler.go"),
		[]byte(`package api

type Handler struct{}

func (h *Handler) CreateTuck() string { return "ok" }
`),
		0o644,
	))
	return dir
}

// TestMultiRepo_ResolvesCallEdges guards two invariants of multi-repo
// resolution:
//
//  1. Sentinel hygiene. applyRepoPrefix used to prefix the
//     "unresolved::" sentinel (producing "web/unresolved::X") which the
//     resolver's HasPrefix check wouldn't recognize — leaving every
//     call edge permanently unresolved. No edge target may carry a
//     prefixed sentinel.
//  2. Repo-boundary discipline. repo B's Main() calls Greet() with NO
//     import of repo A — two unrelated `package main` files in separate
//     repos. That is not a real dependency (and not valid Go), so the
//     resolver must NOT resolve the call across the repo boundary on a
//     bare name match. It stays a clean "unresolved::Greet" sentinel.
//     Resolving it would be the M3 cross-repo name-collision false
//     positive. (Genuine cross-repo resolution — caller imports callee
//     — is covered by the resolver package's own tests.)
func TestMultiRepo_ResolvesCallEdges(t *testing.T) {
	// repo A defines Greet(); repo B's Main() calls a bare Greet() with
	// no import linking the two repos.
	repoA := filepath.Join(t.TempDir(), "lib-svc")
	require.NoError(t, os.MkdirAll(repoA, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoA, "greet.go"),
		[]byte("package main\n\nfunc Greet() string { return \"hi\" }\n"),
		0o644))

	repoB := filepath.Join(t.TempDir(), "app-svc")
	require.NoError(t, os.MkdirAll(repoB, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoB, "main.go"),
		[]byte("package main\n\nfunc Main() {\n\t_ = Greet()\n}\n"),
		0o644))

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "lib-svc"},
			{Path: repoB, Name: "app-svc"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err, "track %s", entry.Name)
	}

	// Zero edge targets may remain under the unresolved sentinel with any
	// prefix applied. The invariant is stricter than "Main → Greet resolved"
	// because the original bug showed up as contains("unresolved::") for
	// the vast majority of call edges.
	var leaked []string
	for _, e := range g.AllEdges() {
		if strings.Contains(e.To, "/unresolved::") {
			leaked = append(leaked, e.From+" → "+e.To)
		}
	}
	assert.Empty(t, leaked,
		"edges with repo-prefixed unresolved targets indicate applyRepoPrefix "+
			"polluted the resolver sentinel:\n  %s",
		strings.Join(leaked, "\n  "))

	// Repo-boundary check: Main()'s call to Greet() must NOT resolve to
	// repo A's Greet — repo B never imports repo A, so a bare-name match
	// across the repo line is the cross-repo false positive. The call
	// edge must remain a clean "unresolved::Greet" sentinel.
	main := "app-svc/main.go::Main"
	crossRepoGreet := "lib-svc/greet.go::Greet"
	var callEdge *graph.Edge
	for _, e := range g.GetOutEdges(main) {
		if e.Kind == graph.EdgeCalls {
			callEdge = e
			break
		}
	}
	if assert.NotNil(t, callEdge, "Main should have a call edge; out-edges: %v", outEdgeSummaries(g, main)) {
		assert.NotEqual(t, crossRepoGreet, callEdge.To,
			"call must not resolve across the repo boundary without an import")
		assert.False(t, callEdge.CrossRepo,
			"call edge must not be flagged cross-repo")
		assert.Equal(t, "unresolved::Greet", callEdge.To,
			"unimported cross-repo call stays a clean unresolved sentinel; got %q", callEdge.To)
	}
}

func outEdgeSummaries(g graph.Store, id string) []string {
	var out []string
	for _, e := range g.GetOutEdges(id) {
		out = append(out, string(e.Kind)+":"+e.To)
	}
	return out
}

// TestTrackRepoCtx_FirstOfManyStillGetsPrefix guards against the bug where
// the first repo tracked via TrackRepoCtx at daemon warmup was indexed
// without a RepoPrefix because willBeMultiRepo was decided by counting
// `mi.repos` (which is empty at iteration 0). The symptom was asymmetric
// IDs across repos: one repo's nodes under "internal/api/handler.go::X",
// the rest under "worker/internal/api/handler.go::X". Halved Go edge
// density in multi-repo graphs.
func TestTrackRepoCtx_FirstOfManyStillGetsPrefix(t *testing.T) {
	repoA := setupGoRepoWithTypes(t, "repo-a")
	repoB := setupGoRepoWithTypes(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	// Simulate warmupDaemonState's loop: TrackRepoCtx each config'd repo
	// in order. The first call is the one that used to skip prefixing.
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err, "tracking %s", entry.Name)
	}

	require.True(t, mi.IsMultiRepo(), "setup must produce multi-repo mode")

	// Every node must carry a non-empty RepoPrefix and its FilePath must
	// live under that prefix. Any violation means a code path bypassed
	// applyRepoPrefix. KindModule and KindBuiltin are deliberately
	// cross-repo singletons (one `module::pypi:requests` /
	// `builtin::go::type::string` shared across every repo that uses
	// them) so they're exempt from the per-repo prefix rule.
	var missingPrefix, badFilePaths []string
	for _, n := range g.AllNodes() {
		if n.Kind == graph.KindModule || n.Kind == graph.KindBuiltin {
			continue
		}
		if ext, _ := n.Meta["external"].(bool); ext {
			// External call targets the resolver materialises as
			// KindFunction with meta.external=true are cross-repo
			// singletons (one `stdlib::fmt::Sprintf` shared across
			// every repo that calls it) — same as KindModule.
			continue
		}
		if n.RepoPrefix == "" {
			missingPrefix = append(missingPrefix, n.ID)
			continue
		}
		if n.FilePath != "" && !strings.HasPrefix(n.FilePath, n.RepoPrefix+"/") {
			badFilePaths = append(badFilePaths,
				n.ID+" (FilePath="+n.FilePath+", RepoPrefix="+n.RepoPrefix+")")
		}
	}
	assert.Empty(t, missingPrefix,
		"nodes without RepoPrefix leaked into multi-repo graph (first-repo prefix bug):\n  %s",
		strings.Join(missingPrefix, "\n  "))
	assert.Empty(t, badFilePaths,
		"nodes with FilePath outside their RepoPrefix:\n  %s",
		strings.Join(badFilePaths, "\n  "))

	// No node ID should begin with an absolute filesystem path — that's
	// the shape stale snapshot nodes take, and no current indexing path
	// should produce it.
	for _, n := range g.AllNodes() {
		assert.False(t, strings.HasPrefix(n.ID, "/"),
			"node ID begins with absolute path: %s", n.ID)
	}

	// Both repos must have contributed Handler.CreateTuck, each under its
	// own prefix. This is the positive counterpart to the prefix check.
	want := map[string]bool{
		"repo-a/api/handler.go::Handler.CreateTuck": false,
		"repo-b/api/handler.go::Handler.CreateTuck": false,
	}
	for _, n := range g.AllNodes() {
		if _, ok := want[n.ID]; ok {
			want[n.ID] = true
		}
	}
	for id, found := range want {
		assert.True(t, found, "expected prefixed node %s not found in graph", id)
	}
}

func TestTrackRepoCtx_DuplicateKubernetesQualNamesAreRepoScoped(t *testing.T) {
	manifest := func(repo, relPath, resource string) string {
		t.Helper()
		root := filepath.Join(t.TempDir(), repo)
		path := filepath.Join(root, relPath)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(resource), 0o644))
		return root
	}

	repoA := manifest("repo-a", "deploy/rbac.yaml", `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: prometheus
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get"]
`)
	repoB := manifest("repo-b", "manifests/roles.yaml", `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: prometheus
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["list"]
`)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: repoA, Name: "repo-a"},
		{Path: repoB, Name: "repo-b"},
	}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	registry := newTestRegistry()
	registry.Register(languages.NewYAMLExtractor())
	mi := NewMultiIndexer(g, registry, search.NewNull(), cm, zap.NewNop())
	for _, entry := range cm.Global().Repos {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err, "tracking %s", entry.Name)
	}

	for _, repo := range []string{"repo-a", "repo-b"} {
		id := repo + "/k8s::ClusterRole::_default::prometheus"
		n := g.GetNode(id)
		require.NotNil(t, n, "resource node %s", id)
		assert.Equal(t, repo, n.RepoPrefix)
		assert.Equal(t, "ClusterRole/prometheus", n.QualName)
		assert.True(t, strings.HasPrefix(n.FilePath, repo+"/"), "resource path %q must be owned by %s", n.FilePath, repo)
	}
}
