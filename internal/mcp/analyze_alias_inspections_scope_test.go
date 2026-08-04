package mcp

// Scope regression tests for `analyze kind=inspections`, which the facade
// routes to the legacy run_inspections tool instead of the analyze
// dispatcher. Because that alias shadows the dispatcher, the handler never
// ran resolveScope, and every inspector sourced its rows from the whole
// graph: a workspace-bound session got a punch list naming sibling
// workspaces' symbols in `file`, `symbol_id` and `message`. The `cycles`
// inspector was the sharpest — it joined the entire cycle path into the
// message before any filter ran, so one visible anchor leaked the rest of
// the chain.
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
)

// runInspections drives the handler and returns its response text plus the
// scope_applied meta value.
func runInspections(t *testing.T, srv *Server, ctx context.Context, args map[string]any) (string, string) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	if _, ok := args["inspections"]; !ok {
		args["inspections"] = "all"
	}
	return runToolOK(t, srv, ctx, "run_inspections", args, srv.handleRunInspections)
}

// TestRunInspections_WorkspaceIsolation is the isolation invariant. The
// structural fixture produces dead code (unreferenced helpers) and a call
// cycle in each repo, so every inspector that reads the node table has rows
// to leak.
func TestRunInspections_WorkspaceIsolation(t *testing.T) {
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "beta", body: structuralBody("b")},
	)

	text, applied := runInspections(t, srv, sessionCtx("s-alpha", paths["repo-a"]), nil)

	assert.Contains(t, text, "repo-a", "alpha session must still see its own violations")
	assert.NotContains(t, text, "repo-b",
		"alpha-bound session leaked beta violations — cross-workspace isolation breach")
	assert.NotEmpty(t, applied, "every scoped response must disclose scope_applied")
}

// TestRunInspections_CycleChainDoesNotLeak covers the cycles inspector
// specifically: its message rolls the whole path into one string, so a
// row-level filter on the anchor alone would still have leaked every other
// member of the chain.
func TestRunInspections_CycleChainDoesNotLeak(t *testing.T) {
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "beta", body: structuralBody("b")},
	)

	text, _ := runInspections(t, srv, sessionCtx("s-alpha", paths["repo-a"]),
		map[string]any{"inspections": "cycles"})

	assert.NotContains(t, text, "repo-b",
		"a cycle message must not carry out-of-workspace members of the chain")
}

// TestRunInspections_RepoNarrows pins the explicit selector: inspectors are
// whole-graph rollups over per-node rows, so this kind honours a repo
// narrowing like health_score / dead_code / hotspots do.
func TestRunInspections_RepoNarrows(t *testing.T) {
	srv, paths := newStructuralWorkspaceServer(t, true)
	ctx := sessionCtx("s-shared", paths["repo-a"])

	wide, _ := runInspections(t, srv, ctx, nil)
	require.Contains(t, wide, "repo-b", "both repos share one workspace, so both start visible")

	byName, applied := runInspections(t, srv, ctx, map[string]any{"repo": "repo-a"})
	assert.Contains(t, byName, "repo-a")
	assert.NotContains(t, byName, "repo-b", "an explicit repo selector must narrow every inspector")
	assert.Equal(t, "repo:repo-a", applied)

	byPath, _ := runInspections(t, srv, ctx, map[string]any{"repo": paths["repo-a"]})
	assert.NotContains(t, byPath, "repo-b", "the path form of repo must narrow like the tracked name")
	// Compare the per-inspector counts rather than the whole body: the
	// cycles inspector picks its anchor from an SCC whose member order is
	// not stable, so two identically-narrowed calls can name either end of
	// the same two-node cycle.
	assert.Equal(t, inspectionSummary(t, byName), inspectionSummary(t, byPath),
		"the tracked-name and path forms of repo must narrow identically")
}

// inspectionSummary decodes the by-inspection violation counts.
func inspectionSummary(t *testing.T, body string) map[string]any {
	t.Helper()
	var payload struct {
		Summary map[string]any `json:"summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	require.NotEmpty(t, payload.Summary)
	return payload.Summary
}

// TestRunInspections_OutOfWorkspaceRepoErrors pins that a cross-workspace
// selector is refused outright rather than answered with a plausible
// global punch list.
func TestRunInspections_OutOfWorkspaceRepoErrors(t *testing.T) {
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "beta", body: structuralBody("b")},
	)

	res, err := srv.handleRunInspections(sessionCtx("s-alpha", paths["repo-a"]),
		makeReq("run_inspections", map[string]any{"inspections": "all", "repo": "repo-b"}))
	require.NoError(t, err)
	require.True(t, res.IsError, "a sibling-workspace repo selector must error, not answer")
}

// TestRunInspections_UnboundSessionUnchanged is the compatibility fence:
// an unbound caller must still get the whole index.
func TestRunInspections_UnboundSessionUnchanged(t *testing.T) {
	srv, _ := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "beta", body: structuralBody("b")},
	)

	text, _ := runInspections(t, srv, context.Background(), nil)

	assert.Contains(t, text, "repo-a")
	assert.Contains(t, text, "repo-b", "an unbound session must not be clamped")
}
