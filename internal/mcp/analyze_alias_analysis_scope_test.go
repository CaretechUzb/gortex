package mcp

// Scope regression tests for `analyze kind=communities` and
// `analyze kind=processes`, which the facade routes to the legacy
// get_communities / get_processes tools instead of the analyze
// dispatcher. Because those aliases shadow the dispatcher, neither
// handler ran resolveScope: both read the cached whole-index analysis
// snapshot and emitted every community / flow in it, so a
// workspace-bound session saw sibling workspaces' subsystem maps, entry
// points and call chains — and the by-id detail paths handed back the
// full member / step lists for an id the session had no right to.
//
// The substring probe ("repo-a" / "repo-b") and the fixtures match
// analyze_scope_test.go: multi-repo node IDs and file paths carry the
// repo name, so a result narrowed to repo-a must never contain "repo-b".

import (
	"context"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// ─── fixtures ───────────────────────────────────────────────────────────────

// installProcessesForTest publishes a synthetic process result under the
// same graph-token discipline installCommunitiesForTest uses: getProcesses
// rejects a snapshot that is not tied to the current graph revision, so a
// bare s.processes assignment reads back as nil.
func installProcessesForTest(s *Server, processes *analysis.ProcessResult) {
	s.analysisMu.Lock()
	defer s.analysisMu.Unlock()

	s.processes = processes
	s.communitiesToken = s.currentCommunityToken()
	s.analysisEpoch++
}

// callableIDsForRepo returns the callable node IDs of one repo, sorted by
// the graph's own order, so fixtures reference symbols that really exist.
func callableIDsForRepo(t *testing.T, g graph.Store, repoPrefix string) []string {
	t.Helper()
	var ids []string
	for _, n := range g.AllNodes() {
		if n == nil || n.RepoPrefix != repoPrefix {
			continue
		}
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			ids = append(ids, n.ID)
		}
	}
	require.GreaterOrEqual(t, len(ids), 2, "fixture repo %q needs >=2 callables", repoPrefix)
	return ids
}

// filesForIDs maps node IDs to their graph file paths.
func filesForIDs(g graph.Store, ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		n := g.GetNode(id)
		if n == nil || n.FilePath == "" || seen[n.FilePath] {
			continue
		}
		seen[n.FilePath] = true
		out = append(out, n.FilePath)
	}
	return out
}

// twoWorkspaceAnalysisServer builds repo-a/alpha + repo-b/beta and installs
// one community and one process per repo, so every assertion below can name
// exactly which rows must survive a clamp.
func twoWorkspaceAnalysisServer(t *testing.T) (*Server, map[string]string) {
	t.Helper()
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "beta", body: structuralBody("b")},
	)
	installAnalysisFixtures(t, srv)
	return srv, paths
}

func installAnalysisFixtures(t *testing.T, srv *Server) {
	t.Helper()
	idsA := callableIDsForRepo(t, srv.graph, "repo-a")
	idsB := callableIDsForRepo(t, srv.graph, "repo-b")

	installCommunitiesForTest(srv, &analysis.CommunityResult{
		Communities: []analysis.Community{
			{
				ID: "community-0", Label: "repo-a/main", Size: len(idsA), Cohesion: 0.8,
				Members: idsA, Files: filesForIDs(srv.graph, idsA),
			},
			{
				ID: "community-1", Label: "repo-b/main", Size: len(idsB), Cohesion: 0.7,
				Members: idsB, Files: filesForIDs(srv.graph, idsB),
			},
		},
		Modularity: 0.42,
	})
	installProcessesForTest(srv, &analysis.ProcessResult{
		Processes: []analysis.Process{
			{
				ID: "process-0", Name: "a flow", EntryPoint: idsA[0],
				Steps:     []analysis.Step{{ID: idsA[0], Depth: 0}, {ID: idsA[1], Depth: 1}},
				StepCount: 2, Files: filesForIDs(srv.graph, idsA[:2]), Score: 0.9,
			},
			{
				ID: "process-1", Name: "b flow", EntryPoint: idsB[0],
				Steps:     []analysis.Step{{ID: idsB[0], Depth: 0}, {ID: idsB[1], Depth: 1}},
				StepCount: 2, Files: filesForIDs(srv.graph, idsB[:2]), Score: 0.9,
			},
		},
		NodeToProcs: map[string][]string{
			idsA[0]: {"process-0"}, idsA[1]: {"process-0"},
			idsB[0]: {"process-1"}, idsB[1]: {"process-1"},
		},
	})
}

// runToolOK drives a handler and returns its response text plus the
// scope_applied meta value, failing the test on any tool error.
func runToolOK(t *testing.T, srv *Server, ctx context.Context, name string, args map[string]any,
	h func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error),
) (string, string) {
	t.Helper()
	res, err := h(ctx, makeReq(name, args))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "%s %v errored: %s", name, args, toolResultText(res))
	applied := ""
	if res.Meta != nil {
		applied, _ = res.Meta.AdditionalFields["scope_applied"].(string)
	}
	return toolResultText(res), applied
}

// ─── get_communities (analyze kind=communities) ─────────────────────────────

// TestGetCommunities_WorkspaceIsolation is the isolation invariant: an
// alpha-bound session must not be shown beta's community. Before the fix
// the handler returned the global partition verbatim, and the summary's
// repo_prefix named the sibling repo outright.
func TestGetCommunities_WorkspaceIsolation(t *testing.T) {
	srv, paths := twoWorkspaceAnalysisServer(t)

	text, applied := runToolOK(t, srv, sessionCtx("s-alpha", paths["repo-a"]), "get_communities", nil,
		srv.handleGetCommunities)

	assert.Contains(t, text, "repo-a", "alpha session must still see its own community")
	assert.NotContains(t, text, "repo-b",
		"alpha-bound session leaked a beta community — cross-workspace isolation breach")
	assert.NotEmpty(t, applied, "every scoped response must disclose scope_applied")
}

// TestGetCommunities_DetailByIDIsNotProbeable pins the by-id path: an
// out-of-workspace community id must report the same miss as a fabricated
// one, so the boundary cannot be enumerated. Before the fix the id lookup
// scanned the global partition and returned beta's full member and file
// lists.
func TestGetCommunities_DetailByIDIsNotProbeable(t *testing.T) {
	srv, paths := twoWorkspaceAnalysisServer(t)
	ctxA := sessionCtx("s-alpha", paths["repo-a"])

	own, _ := runToolOK(t, srv, ctxA, "get_communities", map[string]any{"id": "community-0"},
		srv.handleGetCommunities)
	assert.Contains(t, own, "repo-a", "alpha session must still be able to open its own community")

	foreign, err := srv.handleGetCommunities(ctxA, makeReq("get_communities", map[string]any{"id": "community-1"}))
	require.NoError(t, err)
	require.True(t, foreign.IsError, "a beta community id must not resolve for an alpha session")
	assert.Equal(t, "community not found: community-1", toolResultText(foreign))

	fabricated, err := srv.handleGetCommunities(ctxA, makeReq("get_communities", map[string]any{"id": "community-99"}))
	require.NoError(t, err)
	require.True(t, fabricated.IsError)
	assert.Equal(t, "community not found: community-99", toolResultText(fabricated),
		"an out-of-workspace id must be indistinguishable from a genuine miss")
}

// TestGetCommunities_StraddlingCommunityDropsForeignMembers covers the case
// a 404-only fix would miss: a community whose members span both workspaces
// is legitimately the alpha session's, but must come back with beta's
// members and files removed.
func TestGetCommunities_StraddlingCommunityDropsForeignMembers(t *testing.T) {
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "beta", body: structuralBody("b")},
	)
	idsA := callableIDsForRepo(t, srv.graph, "repo-a")
	idsB := callableIDsForRepo(t, srv.graph, "repo-b")
	mixed := append(append([]string{}, idsA...), idsB...)
	// Label, Hub and ParentID are computed at detection time over the whole
	// cell, so a straddling community carries the other side's content in
	// all three: the label is the modal directory across every file (here
	// repo-b's root, which contributes the most), the hub is the
	// highest-degree member's symbol name, and the parent is a grouping key
	// built from that label. Reproduce that shape exactly.
	mixedFiles := append(filesForIDs(srv.graph, mixed), "repo-b/a.go", "repo-b/b.go")
	label := analysis.RelabelNarrowedCommunity(mixed, mixedFiles)
	require.Contains(t, label, "repo-b", "fixture must reproduce a label derived from the beta side")
	installCommunitiesForTest(srv, &analysis.CommunityResult{
		Communities: []analysis.Community{{
			ID:       "community-0",
			Label:    label,
			Hub:      "bBillingService",
			ParentID: "group/repo-b",
			Size:     len(mixed),
			Cohesion: 0.5,
			Members:  mixed,
			Files:    mixedFiles,
		}},
	})

	text, _ := runToolOK(t, srv, sessionCtx("s-alpha", paths["repo-a"]), "get_communities",
		map[string]any{"id": "community-0"}, srv.handleGetCommunities)

	assert.Contains(t, text, "repo-a")
	assert.NotContains(t, text, "repo-b",
		"a straddling community must be served with its out-of-workspace members removed")
	assert.NotContains(t, text, "bBillingService",
		"the hub was derived from members that were removed — it must not survive the clamp")
}

// TestGetCommunities_UnboundSessionUnchanged pins the strict no-op: an
// unbound session (no cwd — CLI, control clients, the dashboard) must still
// see the whole partition.
func TestGetCommunities_UnboundSessionUnchanged(t *testing.T) {
	srv, _ := twoWorkspaceAnalysisServer(t)

	text, _ := runToolOK(t, srv, context.Background(), "get_communities", nil, srv.handleGetCommunities)

	assert.Contains(t, text, "repo-a")
	assert.Contains(t, text, "repo-b", "an unbound session must not be clamped")
}

// TestGetCommunities_DisclosesWidenedNarrowing pins the honesty contract:
// the partition is global, so a repo selector is widened to the workspace
// rather than silently ignored — and the response says so.
func TestGetCommunities_DisclosesWidenedNarrowing(t *testing.T) {
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "shared-ws", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "shared-ws", body: structuralBody("b")},
	)
	installAnalysisFixtures(t, srv)
	ctx := sessionCtx("s-shared", paths["repo-a"])

	text, applied := runToolOK(t, srv, ctx, "get_communities", map[string]any{"repo": "repo-a"},
		srv.handleGetCommunities)

	assert.Equal(t, "repo:repo-a", applied, "scope_applied must report the scope that was resolved")
	assert.Contains(t, text, "not repo/project-narrowed",
		"a kind that cannot honour the narrowing must disclose that it widened it")
}

// TestGetCommunities_RejectsUnknownSavedScope pins that the selectors are
// now validated rather than dropped on the floor.
func TestGetCommunities_RejectsUnknownSavedScope(t *testing.T) {
	srv, paths := twoWorkspaceAnalysisServer(t)

	res, err := srv.handleGetCommunities(sessionCtx("s-alpha", paths["repo-a"]),
		makeReq("get_communities", map[string]any{"scope": "no-such-scope"}))
	require.NoError(t, err)
	require.True(t, res.IsError, "an unknown saved scope must be an error, not a silent no-op")
}

// ─── get_processes (analyze kind=processes) ─────────────────────────────────

// TestGetProcesses_WorkspaceIsolation is the isolation invariant for the
// list path: entry_point and repo_prefixes are raw node IDs and repo names,
// so an unclamped list hands an alpha session beta's flow inventory.
func TestGetProcesses_WorkspaceIsolation(t *testing.T) {
	srv, paths := twoWorkspaceAnalysisServer(t)

	text, applied := runToolOK(t, srv, sessionCtx("s-alpha", paths["repo-a"]), "get_processes", nil,
		srv.handleGetProcesses)

	assert.Contains(t, text, "repo-a", "alpha session must still see its own flow")
	assert.NotContains(t, text, "repo-b",
		"alpha-bound session leaked a beta process — cross-workspace isolation breach")
	assert.NotEmpty(t, applied)
}

// TestGetProcesses_DetailByIDIsNotProbeable pins the by-id path, which
// returned the entire Process — every step ID and file path — for any id.
func TestGetProcesses_DetailByIDIsNotProbeable(t *testing.T) {
	srv, paths := twoWorkspaceAnalysisServer(t)
	ctxA := sessionCtx("s-alpha", paths["repo-a"])

	own, _ := runToolOK(t, srv, ctxA, "get_processes", map[string]any{"id": "process-0"},
		srv.handleGetProcesses)
	assert.Contains(t, own, "repo-a")

	foreign, err := srv.handleGetProcesses(ctxA, makeReq("get_processes", map[string]any{"id": "process-1"}))
	require.NoError(t, err)
	require.True(t, foreign.IsError, "a beta process id must not resolve for an alpha session")
	assert.Equal(t, "process not found: process-1", toolResultText(foreign))
}

// TestGetProcesses_RepoNarrows is the difference from get_communities: a
// flow's rows are per-step and every step belongs to exactly one repo, so
// this kind honours an explicit repo selector instead of widening it.
func TestGetProcesses_RepoNarrows(t *testing.T) {
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "shared-ws", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "shared-ws", body: structuralBody("b")},
	)
	installAnalysisFixtures(t, srv)
	ctx := sessionCtx("s-shared", paths["repo-a"])

	wide, _ := runToolOK(t, srv, ctx, "get_processes", nil, srv.handleGetProcesses)
	require.Contains(t, wide, "repo-b", "both repos share one workspace, so both flows start visible")

	byName, applied := runToolOK(t, srv, ctx, "get_processes", map[string]any{"repo": "repo-a"},
		srv.handleGetProcesses)
	assert.Contains(t, byName, "repo-a")
	assert.NotContains(t, byName, "repo-b", "an explicit repo selector must narrow the flow list")
	assert.Equal(t, "repo:repo-a", applied)

	byPath, _ := runToolOK(t, srv, ctx, "get_processes", map[string]any{"repo": paths["repo-a"]},
		srv.handleGetProcesses)
	assert.Equal(t, byName, byPath, "the tracked-name and path forms of repo must narrow identically")
}

// TestGetProcesses_UnboundSessionUnchanged pins the strict no-op for
// unbound callers (the dashboard reads this tool with no session cwd).
func TestGetProcesses_UnboundSessionUnchanged(t *testing.T) {
	srv, _ := twoWorkspaceAnalysisServer(t)

	text, _ := runToolOK(t, srv, context.Background(), "get_processes", nil, srv.handleGetProcesses)

	assert.Contains(t, text, "repo-a")
	assert.Contains(t, text, "repo-b", "an unbound session must not be clamped")
}
