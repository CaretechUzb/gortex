package mcp

// Scope regression tests for `analyze kind=clones`, which the facade
// routes to the legacy find_clones tool instead of the analyze
// dispatcher. Because that alias shadows the dispatcher, the handler
// never ran handleAnalyze's resolveScope: it matched `repo` as a bare
// RepoPrefix equality — honouring an exact tracked name but silently
// ignoring the path form — and never applied the session's workspace
// ceiling, so a bound session saw sibling workspaces' clone clusters.
//
// The substring probe ("repo-a" / "repo-b") and the fixtures match
// analyze_scope_test.go: multi-repo node IDs and file paths carry the
// repo name, so a result narrowed to repo-a must never contain "repo-b".

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// firstTwoCallables returns two callable node IDs from one repo, used to
// synthesise a clone pair inside that repo.
func firstTwoCallables(t *testing.T, g graph.Store, repoPrefix string) (string, string) {
	t.Helper()
	var ids []string
	for _, node := range g.AllNodes() {
		if node == nil || node.RepoPrefix != repoPrefix {
			continue
		}
		if node.Kind == graph.KindFunction || node.Kind == graph.KindMethod {
			ids = append(ids, node.ID)
		}
	}
	require.GreaterOrEqual(t, len(ids), 2, "fixture repo %q needs >=2 callables", repoPrefix)
	// Sorted for determinism — AllNodes order is not guaranteed.
	if ids[0] > ids[1] {
		ids[0], ids[1] = ids[1], ids[0]
	}
	return ids[0], ids[1]
}

// addClonePair records a symmetric EdgeSimilarTo pair, the shape the
// index-time MinHash/LSH pass materialises and find_clones reads.
func addClonePair(t *testing.T, g graph.Store, a, b string) {
	t.Helper()
	for _, e := range []*graph.Edge{
		{From: a, To: b, Kind: graph.EdgeSimilarTo, Meta: map[string]any{"similarity": 0.95}},
		{From: b, To: a, Kind: graph.EdgeSimilarTo, Meta: map[string]any{"similarity": 0.95}},
	} {
		g.AddEdge(e)
	}
}

// TestFindClones_WorkspaceIsolation covers issue #349's repro 1 for
// clones: an unnarrowed call from a bound session returned clusters from
// sibling workspaces, because find_clones filtered only on an explicit
// `repo` string and never applied the workspace ceiling.
func TestFindClones_WorkspaceIsolation(t *testing.T) {
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "beta", body: structuralBody("b")},
	)

	// One clone pair wholly inside each workspace.
	a1, a2 := firstTwoCallables(t, srv.graph, "repo-a")
	b1, b2 := firstTwoCallables(t, srv.graph, "repo-b")
	addClonePair(t, srv.graph, a1, a2)
	addClonePair(t, srv.graph, b1, b2)

	ctxA := sessionCtx("s-alpha", paths["repo-a"])
	res, err := srv.handleFindClones(ctxA, makeReq("find_clones", nil))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "find_clones errored: %s", toolResultText(res))

	text := toolResultText(res)
	assert.Contains(t, text, "repo-a", "alpha session must still see its own clone cluster")
	assert.NotContains(t, text, "repo-b",
		"alpha-bound session leaked a beta clone cluster — cross-workspace isolation breach")
}

// TestFindClones_RepoPathFormNarrows pins the selector normalisation the
// handler gained with the workspace clamp: `repo` used to be an exact
// RepoPrefix match, so the path form silently matched nothing.
func TestFindClones_RepoPathFormNarrows(t *testing.T) {
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "shared-ws", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "shared-ws", body: structuralBody("b")},
	)

	a1, a2 := firstTwoCallables(t, srv.graph, "repo-a")
	b1, b2 := firstTwoCallables(t, srv.graph, "repo-b")
	addClonePair(t, srv.graph, a1, a2)
	addClonePair(t, srv.graph, b1, b2)

	ctx := sessionCtx("s-shared", paths["repo-a"])

	// Unnarrowed: the whole workspace, so both clusters.
	res, err := srv.handleFindClones(ctx, makeReq("find_clones", nil))
	require.NoError(t, err)
	require.False(t, res.IsError, "find_clones errored: %s", toolResultText(res))
	broad := toolResultText(res)
	assert.Contains(t, broad, "repo-a")
	assert.Contains(t, broad, "repo-b", "an unnarrowed call must span the bound workspace")

	// Both selector forms must narrow identically.
	for _, selector := range []string{"repo-a", paths["repo-a"]} {
		res, err = srv.handleFindClones(ctx, makeReq("find_clones", map[string]any{"repo": selector}))
		require.NoError(t, err)
		require.False(t, res.IsError, "find_clones repo=%s errored: %s", selector, toolResultText(res))
		text := toolResultText(res)
		assert.Contains(t, text, "repo-a", "repo=%s must keep repo-a's cluster", selector)
		assert.NotContains(t, text, "repo-b", "repo=%s must drop repo-b's cluster", selector)
	}
}
