package mcp

// Integration tests for preview_edit and simulate_chain. These hit the
// full MCP tool dispatch path (registered tools + handler) and rely on
// the shared setupOverlayServer harness defined in overlay_e2e_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
)

// buildSingleFileEdit constructs a WorkspaceEdit that replaces the
// full body of `path` with `newContent`. The range spans (0,0) ..
// (very-far-end), which lspPositionToByteOffset clamps to len(content).
// Used by every test below so the WorkspaceEdit JSON is constructed
// in one place.
func buildSingleFileEdit(path, newContent string) string {
	edit := map[string]any{
		"documentChanges": []any{
			map[string]any{
				"textDocument": map[string]any{"uri": path, "version": 0},
				"edits": []any{
					map[string]any{
						"range": map[string]any{
							"start": map[string]any{"line": 0, "character": 0},
							"end":   map[string]any{"line": 100000, "character": 0},
						},
						"newText": newContent,
					},
				},
			},
		},
	}
	b, _ := json.Marshal(edit)
	return string(b)
}

// TestPreviewEdit_ToolRegistered confirms the tool wired up correctly
// after NewServer + SetOverlayManager. Without registration every
// subsequent simulation test would explode in the lookup; isolating
// the registration check lets us read the failure faster.
func TestPreviewEdit_ToolRegistered(t *testing.T) {
	srv, _, _, _ := setupOverlayServer(t)
	require.NotNil(t, srv.MCPServer().GetTool("preview_edit"), "preview_edit must be registered")
	require.NotNil(t, srv.MCPServer().GetTool("simulate_chain"), "simulate_chain must be registered")
}

// TestPreviewEdit_RejectsInvalidJSON: malformed workspace_edit must
// surface as an MCP tool error so the agent knows to retry, not fail
// silently with an empty impact report.
func TestPreviewEdit_RejectsInvalidJSON(t *testing.T) {
	srv, _, _, _ := setupOverlayServer(t)
	ctx := WithSessionID(context.Background(), "test")
	res := callToolByName(t, srv, ctx, "preview_edit", map[string]any{
		"workspace_edit": "{not valid json}",
	})
	require.True(t, res.IsError, "preview_edit must reject malformed JSON")
	require.Contains(t, toolText(res), "invalid workspace_edit")
}

// TestPreviewEdit_RejectsEmptyEdit: a WorkspaceEdit with no document
// changes is a no-op; treating it as a success would mislead the
// caller into thinking they had a meaningful preview.
func TestPreviewEdit_RejectsEmptyEdit(t *testing.T) {
	srv, _, _, _ := setupOverlayServer(t)
	ctx := WithSessionID(context.Background(), "test")
	res := callToolByName(t, srv, ctx, "preview_edit", map[string]any{
		"workspace_edit": `{"changes":{}}`,
	})
	require.True(t, res.IsError, "preview_edit must reject empty workspace_edit")
	require.Contains(t, toolText(res), "no document changes")
}

// TestPreviewEdit_RenameSymbol simulates renaming a function with a
// distinctive signature so the rename heuristic can confidently pair
// the missing-base node with the added-overlay node. We can't use
// the harness's Target() because its parameterless void signature is
// the trivial shape that the heuristic deliberately rejects (every
// `func ()` would otherwise pair with every other `func ()`). The
// test seeds a fresh helper file with a distinctive signature and
// then renames it.
func TestPreviewEdit_RenameSymbol(t *testing.T) {
	srv, dir, _, _ := setupOverlayServer(t)
	ctx := WithSessionID(context.Background(), "test")

	// Push a helper file via overlay so the base graph picks up
	// `Encode(payload []byte) []byte`. Then simulate renaming it to
	// `Marshal(payload []byte) []byte` — same signature shape, new
	// name. The heuristic should classify this as a rename.
	helperPath := filepath.Join(dir, "encoder.go")
	require.NoError(t, srv.OverlayManager().RegisterWithID("seed", ""))
	require.NoError(t, srv.OverlayManager().Push("seed", daemon.OverlayFile{
		Path:    helperPath,
		Content: "package main\n\nfunc Encode(payload []byte) []byte { return payload }\n",
	}, nil))
	// Commit the seeded file to disk so it shows up in base on
	// reindex.
	require.NoError(t, writeSeedFile(helperPath,
		"package main\n\nfunc Encode(payload []byte) []byte { return payload }\n"))
	srv.OverlayManager().Drop("seed")
	_, err := srv.indexer.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	srv.indexer.ResolveAll()

	newContent := "package main\n\nfunc Marshal(payload []byte) []byte { return payload }\n"
	res := callToolByName(t, srv, ctx, "preview_edit", map[string]any{
		"workspace_edit": buildSingleFileEdit(helperPath, newContent),
		"diagnostics":    false, // no LSP setup in test harness
	})
	require.False(t, res.IsError, "preview_edit failed: %s", toolText(res))

	body := toolText(res)
	require.Contains(t, body, "symbols_renamed", "rename surface must be present")
	require.Contains(t, body, "encoder.go::Marshal", "renamed-to symbol must surface")
	require.Contains(t, body, "encoder.go::Encode", "renamed-from symbol must surface")
}

// writeSeedFile is a thin os.WriteFile wrapper kept local to this
// test file so the test imports don't grow.
func writeSeedFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// TestPreviewEdit_RemoveSymbol simulates a true removal: Target() is
// gone and nothing else replaces it (rename heuristic can't match).
// The broken_callers payload MUST include caller.go::Caller because
// the cross-file call edge can no longer resolve.
func TestPreviewEdit_RemoveSymbol(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	ctx := WithSessionID(context.Background(), "test")

	// Replace target.go with an empty package — Target() simply
	// disappears and there's no rename candidate.
	newContent := "package main\n"
	res := callToolByName(t, srv, ctx, "preview_edit", map[string]any{
		"workspace_edit": buildSingleFileEdit(targetFile, newContent),
		"diagnostics":    false,
	})
	require.False(t, res.IsError, "preview_edit failed: %s", toolText(res))

	body := toolText(res)
	require.Contains(t, body, "Caller", "caller.go::Caller must show in broken_callers")
	require.Contains(t, body, "target removed or renamed")
}

// TestPreviewEdit_AddOnly: adding a brand-new function alongside
// Target() should produce a non-empty symbols_added list and no
// broken callers (Target stays put, the original caller resolves).
func TestPreviewEdit_AddOnly(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	ctx := WithSessionID(context.Background(), "test")

	newContent := "package main\n\nfunc Target() {}\n\nfunc NewHelper() {}\n"
	res := callToolByName(t, srv, ctx, "preview_edit", map[string]any{
		"workspace_edit": buildSingleFileEdit(targetFile, newContent),
		"diagnostics":    false,
	})
	require.False(t, res.IsError, "preview_edit failed: %s", toolText(res))

	body := toolText(res)
	require.Contains(t, body, "NewHelper", "new symbol must surface in symbols_added")
	require.NotContains(t, body, "target removed or renamed",
		"the unchanged Target() must not surface as broken")
}

// TestPreviewEdit_OverlayPathsReported: the response must include the
// overlay_paths field so the caller knows exactly which paths the
// simulation touched (useful when the WorkspaceEdit spans multiple
// files and the agent needs a checklist).
func TestPreviewEdit_OverlayPathsReported(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	ctx := WithSessionID(context.Background(), "test")

	res := callToolByName(t, srv, ctx, "preview_edit", map[string]any{
		"workspace_edit": buildSingleFileEdit(targetFile, "package main\n\nfunc Target() {}\n\nfunc Added() {}\n"),
		"diagnostics":    false,
	})
	require.False(t, res.IsError, "preview_edit failed: %s", toolText(res))
	body := toolText(res)
	require.Contains(t, body, "overlay_paths")
	require.Contains(t, body, "touched_files")
}

// TestPreviewEdit_DoesNotMutateBase is the load-bearing isolation
// guarantee for the simulation engine — the same property the
// overlay layer enforces, but verified through the simulation tool
// surface to catch any future refactor that accidentally takes the
// promote-to-session path implicitly.
func TestPreviewEdit_DoesNotMutateBase(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	beforeIDs := baseNodeIDs(srv)

	ctx := WithSessionID(context.Background(), "test")
	res := callToolByName(t, srv, ctx, "preview_edit", map[string]any{
		"workspace_edit": buildSingleFileEdit(targetFile, "package main\n\nfunc Renamed() {}\n"),
		"diagnostics":    false,
	})
	require.False(t, res.IsError, "preview_edit failed: %s", toolText(res))

	afterIDs := baseNodeIDs(srv)
	require.Equal(t, beforeIDs, afterIDs,
		"base graph must be byte-identical before and after preview_edit")
}

// TestSimulateChain_AppliesStepsInOrder: a two-step chain that first
// adds Helper(), then removes Target() entirely. The response must
// include both step records — step 0 reports Helper as added, step 1
// reports the removal and the resulting broken-caller surface.
func TestSimulateChain_AppliesStepsInOrder(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	ctx := WithSessionID(context.Background(), "test")

	step1 := buildSingleFileEdit(targetFile, "package main\n\nfunc Target() {}\n\nfunc Helper() {}\n")
	step2 := buildSingleFileEdit(targetFile, "package main\n\nfunc Helper() {}\n")
	steps := fmt.Sprintf(`[%s,%s]`, step1, step2)

	res := callToolByName(t, srv, ctx, "simulate_chain", map[string]any{
		"steps":         steps,
		"diagnostics":   false,
		"stop_on_error": false,
	})
	require.False(t, res.IsError, "simulate_chain failed: %s", toolText(res))

	body := toolText(res)
	require.Contains(t, body, `"total_steps":2`, "chain must record two applied steps")
	require.Contains(t, body, `"applied_steps":2`)
	require.Contains(t, body, "Helper", "step 1 must surface the added Helper symbol")
	require.Contains(t, body, "target removed or renamed",
		"step 2 must report the broken caller after Target is removed")
}

// TestSimulateChain_RejectsEmpty: an empty chain is an obvious caller
// error; the response must say so explicitly so the agent doesn't
// chase a phantom no-op result.
func TestSimulateChain_RejectsEmpty(t *testing.T) {
	srv, _, _, _ := setupOverlayServer(t)
	ctx := WithSessionID(context.Background(), "test")
	res := callToolByName(t, srv, ctx, "simulate_chain", map[string]any{
		"steps": "[]",
	})
	require.True(t, res.IsError)
	require.Contains(t, toolText(res), "steps array is empty")
}

// TestSimulateChain_RejectsNonArray: a JSON object passed where the
// schema demands an array must error with a helpful message rather
// than panic in the unmarshaller.
func TestSimulateChain_RejectsNonArray(t *testing.T) {
	srv, _, _, _ := setupOverlayServer(t)
	ctx := WithSessionID(context.Background(), "test")
	res := callToolByName(t, srv, ctx, "simulate_chain", map[string]any{
		"steps": `{"changes":{}}`,
	})
	require.True(t, res.IsError)
	require.Contains(t, toolText(res), "JSON array")
}

// TestSimulateChain_KeepPromotesOverlay verifies the `keep: true`
// promotion path: after a successful chain, the resulting state is
// pushed into a real overlay session bound to the caller's MCP
// session ID. A follow-up overlay_list must report the simulated file.
func TestSimulateChain_KeepPromotesOverlay(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	sessID := "sim-keep"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessID, ""))
	ctx := WithSessionID(context.Background(), sessID)

	step := buildSingleFileEdit(targetFile, "package main\n\nfunc Target() {}\n\nfunc Kept() {}\n")
	steps := fmt.Sprintf(`[%s]`, step)
	res := callToolByName(t, srv, ctx, "simulate_chain", map[string]any{
		"steps":       steps,
		"keep":        true,
		"diagnostics": false,
	})
	require.False(t, res.IsError, "simulate_chain failed: %s", toolText(res))

	body := toolText(res)
	require.Contains(t, body, `"kept":true`)
	require.Contains(t, body, `"overlay_session_id":"`+sessID+`"`)

	// And the overlay manager itself must now hold the file.
	files, err := srv.OverlayManager().Files(sessID)
	require.NoError(t, err)
	require.NotEmpty(t, files, "keep must persist the simulated overlay")

	// A subsequent get_file_summary on the touched file must reflect
	// the promoted overlay state.
	listRes := callToolByName(t, srv, ctx, "get_file_summary", map[string]any{
		"path": filepath.Base(targetFile),
	})
	require.False(t, listRes.IsError)
	require.Contains(t, toolText(listRes), "Kept",
		"after keep:true the overlay must surface the simulated symbol")
}

// TestSimulateChain_InheritOverlay: when a session already has an
// overlay pushed, simulate_chain with inherit_overlay:true must
// layer on top of that buffer state — not on top of base.
func TestSimulateChain_InheritOverlay(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	sessID := "sim-inherit"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessID, ""))
	// Editor has an unsaved buffer that adds Helper().
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n\nfunc Helper() {}\n",
	}, nil))

	ctx := WithSessionID(context.Background(), sessID)
	// The simulation adds a third function on top of the inherited
	// buffer; the resulting state must contain Helper AND Helper2.
	step := buildSingleFileEdit(targetFile,
		"package main\n\nfunc Target() {}\n\nfunc Helper() {}\n\nfunc Helper2() {}\n")
	res := callToolByName(t, srv, ctx, "simulate_chain", map[string]any{
		"steps":           fmt.Sprintf(`[%s]`, step),
		"inherit_overlay": true,
		"diagnostics":     false,
	})
	require.False(t, res.IsError, "simulate_chain failed: %s", toolText(res))

	body := toolText(res)
	require.Contains(t, body, "Helper2", "added symbol must be visible")
	require.Contains(t, body, `"inherit_overlay":true`)
}

// TestSimulateChain_StopsOnError: when stop_on_error is true (default)
// the chain must abort the moment a step introduces a new ERROR-level
// diagnostic delta. Without an LSP wired we can't drive this directly,
// so we exercise the parameter plumbing only — the diagnostics path
// is covered separately when an LSP is available.
func TestSimulateChain_StopsOnErrorFlag(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	ctx := WithSessionID(context.Background(), "sim-stop")
	step := buildSingleFileEdit(targetFile, "package main\n\nfunc Target() {}\n")
	res := callToolByName(t, srv, ctx, "simulate_chain", map[string]any{
		"steps":         fmt.Sprintf(`[%s]`, step),
		"stop_on_error": true,
		"diagnostics":   false,
	})
	require.False(t, res.IsError, "simulate_chain failed: %s", toolText(res))
	body := toolText(res)
	require.Contains(t, body, `"stopped_at":-1`,
		"with no errors stopped_at must be -1")
}

// TestSimulateChain_KeepRequiresOverlayManager: the keep flag is a
// no-op (with kept:false) when the calling session has no MCP
// session ID — embedded stdio without ctx-attached session. Verify
// we don't drop a stack trace in that case.
func TestSimulateChain_KeepWithoutSessionDegrades(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	step := buildSingleFileEdit(targetFile, "package main\n\nfunc Target() {}\n")
	ctx := context.Background() // no session ID
	res := callToolByName(t, srv, ctx, "simulate_chain", map[string]any{
		"steps":       fmt.Sprintf(`[%s]`, step),
		"keep":        true,
		"diagnostics": false,
	})
	require.False(t, res.IsError, "simulate_chain must not error when no MCP session: %s", toolText(res))
	require.Contains(t, toolText(res), `"kept":false`)
}

// TestPreviewEdit_CumulativeImpactReported confirms the response shape
// includes the impact rollup that the agent consumes to decide on
// test selection.
func TestPreviewEdit_CumulativeImpactReported(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	ctx := WithSessionID(context.Background(), "test")
	res := callToolByName(t, srv, ctx, "preview_edit", map[string]any{
		"workspace_edit": buildSingleFileEdit(targetFile, "package main\n\nfunc Renamed() {}\n"),
		"diagnostics":    false,
	})
	require.False(t, res.IsError, "preview_edit failed: %s", toolText(res))
	body := toolText(res)
	require.Contains(t, body, "impact", "impact rollup must be present")
	require.Contains(t, body, "summary", "summary line must be present")
}

// TestSimulateChain_ConcurrentSessions: two MCP sessions running
// simulate_chain in parallel against the same workspace must not
// step on each other's overlay state, mirroring the OverlayManager
// multi-tenant guarantee.
func TestSimulateChain_ConcurrentSessions(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)

	run := func(t *testing.T, name string, contentTag string, expect string) {
		t.Helper()
		ctx := WithSessionID(context.Background(), name)
		step := buildSingleFileEdit(targetFile, fmt.Sprintf("package main\n\nfunc Target() {}\n\nfunc %s() {}\n", contentTag))
		res := callToolByName(t, srv, ctx, "simulate_chain", map[string]any{
			"steps":       fmt.Sprintf(`[%s]`, step),
			"diagnostics": false,
		})
		require.False(t, res.IsError, "simulate_chain failed: %s", toolText(res))
		require.Contains(t, toolText(res), expect)
	}

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		run(t, "sim-a", "AlphaSym", "AlphaSym")
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		run(t, "sim-b", "BetaSym", "BetaSym")
	}()
	for range 2 {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("simulation goroutines did not finish in time")
		}
	}
}

// TestSimulateChain_NewFile exercises the new-file path: a
// WorkspaceEdit whose target file doesn't exist on disk should
// produce an overlay that emits the file's contents as a fresh
// buffer. The simulator treats the (0,0) range with newText as the
// initial content for an absent file.
func TestSimulateChain_NewFile(t *testing.T) {
	srv, dir, _, _ := setupOverlayServer(t)
	newPath := filepath.Join(dir, "fresh.go")
	step := buildSingleFileEdit(newPath, "package main\n\nfunc Fresh() {}\n")
	ctx := WithSessionID(context.Background(), "sim-new")
	res := callToolByName(t, srv, ctx, "simulate_chain", map[string]any{
		"steps":       fmt.Sprintf(`[%s]`, step),
		"diagnostics": false,
	})
	require.False(t, res.IsError, "simulate_chain failed: %s", toolText(res))
	body := toolText(res)
	require.True(t, strings.Contains(body, "Fresh") || strings.Contains(body, "fresh.go"),
		"new-file simulation must surface the new file: %s", body)
}
