package mcp

// Scope regression tests for `analyze kind=health`, which the facade
// routes to the legacy audit_health tool instead of the analyze
// dispatcher. Because that alias shadows the dispatcher, the handler
// never ran handleAnalyze's resolveScope — it discarded its whole
// request and graded every node in the index, so a workspace-bound
// session was graded against sibling workspaces and an explicit repo
// selector was accepted and silently ignored.
//
// The substring probe ("repo-a" / "repo-b") and the fixtures match
// analyze_scope_test.go: multi-repo node IDs and file paths carry the
// repo name, so a result narrowed to repo-a must never contain "repo-b".

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// ─── helpers ────────────────────────────────────────────────────────────────

// runAuditHealth drives the audit_health handler and decodes its report.
func runAuditHealth(t *testing.T, srv *Server, ctx context.Context, args map[string]any) (AuditReport, string, string) {
	t.Helper()
	res, err := srv.handleAuditHealth(ctx, makeReq("audit_health", args))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "audit_health %v errored: %s", args, toolResultText(res))

	text := toolResultText(res)
	var report AuditReport
	require.NoError(t, json.Unmarshal([]byte(text), &report), "audit_health must return a JSON AuditReport: %s", text)

	applied := ""
	if res.Meta != nil {
		applied, _ = res.Meta.AdditionalFields["scope_applied"].(string)
	}
	return report, applied, text
}

// callableCountForRepo counts the callable symbols the audit grades in
// one repo, straight from the graph — so assertions compare against the
// fixture's real shape instead of a hard-coded number.
func callableCountForRepo(g graph.Store, repoPrefix string) int {
	n := 0
	for _, node := range g.AllNodes() {
		if node == nil || node.RepoPrefix != repoPrefix {
			continue
		}
		if node.Kind == graph.KindFunction || node.Kind == graph.KindMethod {
			n++
		}
	}
	return n
}

// ─── audit_health (analyze kind=health) ─────────────────────────────────────

// TestAuditHealth_WorkspaceIsolation is the security invariant: a
// session bound to workspace alpha must never be graded against beta's
// symbols. Before the fix handleAuditHealth discarded its context, so
// this leaked every repo in the index.
func TestAuditHealth_WorkspaceIsolation(t *testing.T) {
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "beta", body: structuralBody("b")},
	)
	ctxA := sessionCtx("s-alpha", paths["repo-a"])

	report, _, text := runAuditHealth(t, srv, ctxA, nil)

	wantA := callableCountForRepo(srv.graph, "repo-a")
	require.Positive(t, wantA, "fixture must produce callable symbols in repo-a")
	assert.Equal(t, wantA, report.SymbolCount,
		"alpha-bound session must be graded on repo-a's callables only")
	assert.NotContains(t, text, "repo-b",
		"alpha-bound session leaked beta symbols through audit_health — cross-workspace isolation breach")
}

// TestAuditHealth_RepoNarrows is issue #349's repro 2: within a single
// workspace holding two repos, an explicit repo selector must narrow the
// grade instead of being accepted and ignored.
func TestAuditHealth_RepoNarrows(t *testing.T) {
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "shared-ws", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "shared-ws", body: structuralBody("b")},
	)
	ctx := sessionCtx("s-shared", paths["repo-a"])

	countA := callableCountForRepo(srv.graph, "repo-a")
	countB := callableCountForRepo(srv.graph, "repo-b")
	require.Positive(t, countA)
	require.Positive(t, countB)

	// No selector: the whole workspace, both repos.
	broad, _, _ := runAuditHealth(t, srv, ctx, nil)
	assert.Equal(t, countA+countB, broad.SymbolCount,
		"an unnarrowed call must grade the whole bound workspace")

	// Narrowed by tracked repo name.
	narrow, applied, text := runAuditHealth(t, srv, ctx, map[string]any{"repo": "repo-a"})
	assert.Equal(t, countA, narrow.SymbolCount, "repo selector must narrow the graded set")
	assert.Less(t, narrow.SymbolCount, broad.SymbolCount,
		"repo:repo-a must grade strictly fewer symbols than the whole workspace")
	assert.Equal(t, "repo:repo-a", applied, "the response must disclose the applied scope")
	assert.NotContains(t, text, "repo-b", "repo:repo-a must not report repo-b symbols")
}

// TestAuditHealth_RepoPathFormNarrows covers the other selector form the
// issue exercised — target={"repo":"<absolute path>"} — which reaches the
// handler as a filesystem path and must resolve to the same tracked
// prefix as the name form.
func TestAuditHealth_RepoPathFormNarrows(t *testing.T) {
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "shared-ws", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "shared-ws", body: structuralBody("b")},
	)
	ctx := sessionCtx("s-shared", paths["repo-a"])

	byName, _, _ := runAuditHealth(t, srv, ctx, map[string]any{"repo": "repo-a"})
	byPath, _, text := runAuditHealth(t, srv, ctx, map[string]any{"repo": paths["repo-a"]})

	assert.Equal(t, byName.SymbolCount, byPath.SymbolCount,
		"the path form of the repo selector must narrow identically to the name form")
	assert.NotContains(t, text, "repo-b", "the path form must not report repo-b symbols")
}

// TestAuditHealth_UnknownRepoErrors pins the "no silent no-ops" contract:
// a selector that resolves to nothing inside the workspace must fail
// loudly rather than return a plausible-looking global report.
func TestAuditHealth_UnknownRepoErrors(t *testing.T) {
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "beta", body: structuralBody("b")},
	)
	ctxA := sessionCtx("s-alpha", paths["repo-a"])

	res, err := srv.handleAuditHealth(ctxA, makeReq("audit_health", map[string]any{"repo": "repo-b"}))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.IsError,
		"naming an out-of-workspace repo must error, not silently return a global report")
}

// TestAuditHealth_SingleRepoUnaffected guards the README-badge path.
// A single-repo daemon mints nodes with an empty RepoPrefix and has no
// multi-indexer to resolve a path selector against, so `gortex audit`
// now sends a `repo` argument that matches no registry prefix. Those
// nodes must still be graded — QueryOptions.ScopeAllows admits
// unprefixed nodes rather than letting a repo narrow match vacuously.
func TestAuditHealth_SingleRepoUnaffected(t *testing.T) {
	srv, dir := setupTestServer(t)

	bare, _, _ := runAuditHealth(t, srv, context.Background(), nil)
	require.Positive(t, bare.SymbolCount, "single-repo fixture must grade its callables")

	// The path form the CLI sends, which resolves to no tracked prefix.
	withPath, _, _ := runAuditHealth(t, srv, context.Background(), map[string]any{"repo": dir})
	assert.Equal(t, bare.SymbolCount, withPath.SymbolCount,
		"a path selector in single-repo mode must not narrow the report to nothing")
}

// TestAuditHealth_UnboundSessionStaysGlobal guards the compatibility
// edge: a session with no cwd anchor has no workspace ceiling, so the
// badge/CLI path keeps grading the whole index.
func TestAuditHealth_UnboundSessionStaysGlobal(t *testing.T) {
	srv, _ := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "beta", body: structuralBody("b")},
	)

	report, _, _ := runAuditHealth(t, srv, context.Background(), nil)
	want := callableCountForRepo(srv.graph, "repo-a") + callableCountForRepo(srv.graph, "repo-b")
	assert.Equal(t, want, report.SymbolCount,
		"an unbound session has no workspace ceiling and must still grade the whole index")
}
