package mcp

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/semantic/lsp"
)

func TestOverlayRequestSnapshotLimitsMatchLiteralEnvelope(t *testing.T) {
	require.Equal(t, 257, overlayRequestSnapshotMaxFiles)
	require.Equal(t, 4_259_840, overlayRequestSnapshotMaxBytes)
}

func TestSnapshotOverlayRequestForCtxBoundsRawAliasesBeforeCanonicalization(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fileCount int
		wantError bool
	}{
		{name: "exact", fileCount: overlayRequestSnapshotMaxFiles},
		{name: "plus_one", fileCount: overlayRequestSnapshotMaxFiles + 1, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := daemon.NewOverlayManager(time.Hour)
			const sessionID = "raw-alias-boundary"
			require.NoError(t, manager.RegisterWithID(sessionID, "workspace"))
			pushRawOverlayAliases(t, manager, sessionID, tc.fileCount)

			server := &Server{overlays: manager}
			snapshot, err := server.snapshotOverlayRequestForCtx(WithSessionID(context.Background(), sessionID))
			if tc.wantError {
				require.Nil(t, snapshot)
				requireOverlaySnapshotBoundError(t, err, "files", overlayRequestSnapshotMaxFiles)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, snapshot)
			require.Len(t, snapshot.files, overlayRequestSnapshotMaxFiles)
			require.False(t, snapshot.canonical)
		})
	}
}

func TestSnapshotOverlayRequestForCtxPropagatesByteBoundary(t *testing.T) {
	manager := daemon.NewOverlayManager(time.Hour)
	const sessionID = "raw-byte-boundary"
	require.NoError(t, manager.RegisterWithID(sessionID, "workspace"))
	server := &Server{overlays: manager}
	ctx := WithSessionID(context.Background(), sessionID)

	exact := daemon.OverlayFile{
		Path:    "p",
		Content: strings.Repeat("x", overlayRequestSnapshotMaxBytes-2),
		BaseSHA: "s",
	}
	require.NoError(t, manager.Push(sessionID, exact, nil))
	snapshot, err := server.snapshotOverlayRequestForCtx(ctx)
	require.NoError(t, err)
	require.Len(t, snapshot.files, 1)

	exact.Content += "x"
	require.NoError(t, manager.Push(sessionID, exact, nil))
	snapshot, err = server.snapshotOverlayRequestForCtx(ctx)
	require.Nil(t, snapshot)
	requireOverlaySnapshotBoundError(t, err, "bytes", overlayRequestSnapshotMaxBytes)
}

func TestSnapshotOverlayRequestForCtxPinsOnlyMissingSession(t *testing.T) {
	server := &Server{overlays: daemon.NewOverlayManager(time.Hour)}
	const sessionID = "missing-overlay-session"

	snapshot, err := server.snapshotOverlayRequestForCtx(WithSessionID(context.Background(), sessionID))
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	require.Equal(t, sessionID, snapshot.sessionID)
	require.Empty(t, snapshot.workspace)
	require.Nil(t, snapshot.files)
}

func TestSnapshotOverlayRequestForCtxChecksCancellationAroundAcquisition(t *testing.T) {
	manager := daemon.NewOverlayManager(time.Hour)
	const sessionID = "snapshot-cancellation"
	require.NoError(t, manager.RegisterWithID(sessionID, "workspace"))
	server := &Server{overlays: manager}

	preCanceled, cancel := context.WithCancel(WithSessionID(context.Background(), sessionID))
	cancel()
	snapshot, err := server.snapshotOverlayRequestForCtx(preCanceled)
	require.Nil(t, snapshot)
	require.ErrorIs(t, err, context.Canceled)

	postCanceled := &cancelAfterNErrContext{
		Context:      WithSessionID(context.Background(), sessionID),
		allowedCalls: 1,
	}
	snapshot, err = server.snapshotOverlayRequestForCtx(postCanceled)
	require.Nil(t, snapshot)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, int32(2), postCanceled.errCalls.Load())
}

func TestValidateOverlayBuildEnvelopeExactAndPlusOne(t *testing.T) {
	exact := daemon.OverlayFile{
		Path:    "p",
		Content: strings.Repeat("x", overlayRequestSnapshotMaxBytes-2),
		BaseSHA: "s",
	}
	require.NoError(t, validateOverlayBuildEnvelope([]daemon.OverlayFile{exact}))
	require.NoError(t, validateOverlayBuildMapEnvelope(map[string]daemon.OverlayFile{"p": exact}))

	server := &Server{}
	layer, paths, err := server.constructOverlayLayer(context.Background(), []daemon.OverlayFile{exact})
	require.NoError(t, err)
	require.Nil(t, layer)
	require.Nil(t, paths)

	tooLarge := exact
	tooLarge.BaseSHA += "s"
	requireOverlaySnapshotBoundError(
		t,
		validateOverlayBuildEnvelope([]daemon.OverlayFile{tooLarge}),
		"bytes",
		overlayRequestSnapshotMaxBytes,
	)
	requireOverlaySnapshotBoundError(
		t,
		validateOverlayBuildMapEnvelope(map[string]daemon.OverlayFile{"p": tooLarge}),
		"bytes",
		overlayRequestSnapshotMaxBytes,
	)
	layer, paths, err = server.constructOverlayLayer(context.Background(), []daemon.OverlayFile{tooLarge})
	require.Nil(t, layer)
	require.Nil(t, paths)
	requireOverlaySnapshotBoundError(t, err, "bytes", overlayRequestSnapshotMaxBytes)

	tooMany := make([]daemon.OverlayFile, overlayRequestSnapshotMaxFiles+1)
	requireOverlaySnapshotBoundError(t, validateOverlayBuildEnvelope(tooMany), "files", overlayRequestSnapshotMaxFiles)
	tooManyMap := make(map[string]daemon.OverlayFile, overlayRequestSnapshotMaxFiles+1)
	for i := range tooMany {
		path := strings.Repeat("x", i+1)
		tooManyMap[path] = daemon.OverlayFile{Path: path}
	}
	requireOverlaySnapshotBoundError(t, validateOverlayBuildMapEnvelope(tooManyMap), "files", overlayRequestSnapshotMaxFiles)
}

func TestConstructOverlayLayerCancellationPrecedesRawValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	files := make([]daemon.OverlayFile, overlayRequestSnapshotMaxFiles+1)

	layer, paths, err := (&Server{}).constructOverlayLayer(ctx, files)
	require.Nil(t, layer)
	require.Nil(t, paths)
	require.ErrorIs(t, err, context.Canceled)
}

func TestBuildSimulationBoundsInheritedSnapshotAndPreservesPinnedEmpty(t *testing.T) {
	t.Run("oversized inheritance", func(t *testing.T) {
		manager := daemon.NewOverlayManager(time.Hour)
		const sessionID = "oversized-inheritance"
		require.NoError(t, manager.RegisterWithID(sessionID, "workspace"))
		pushRawOverlayAliases(t, manager, sessionID, overlayRequestSnapshotMaxFiles+1)

		sim, err := (&Server{overlays: manager}).buildSimulation(
			WithSessionID(context.Background(), sessionID),
			nil,
			true,
		)
		require.Nil(t, sim)
		requireOverlaySnapshotBoundError(t, err, "files", overlayRequestSnapshotMaxFiles)
	})

	t.Run("empty inheritance", func(t *testing.T) {
		manager := daemon.NewOverlayManager(time.Hour)
		const sessionID = "empty-inheritance"
		require.NoError(t, manager.RegisterWithID(sessionID, "workspace"))

		sim, err := (&Server{overlays: manager}).buildSimulation(
			WithSessionID(context.Background(), sessionID),
			nil,
			true,
		)
		require.NoError(t, err)
		require.NotNil(t, sim)
		require.Empty(t, sim.initial)
		require.Empty(t, sim.snapshots)
	})
}

func TestBuildSimulationPreflightsStepBeforeRetainingSnapshot(t *testing.T) {
	path := t.TempDir() + "/new.go"
	for _, tc := range []struct {
		name      string
		extraByte int
		wantError bool
	}{
		{name: "exact"},
		{name: "plus_one", extraByte: 1, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contentBytes := overlayRequestSnapshotMaxBytes - len(path) + tc.extraByte
			edit := fullFileSimulationEdit(path, strings.Repeat("x", contentBytes))

			sim, err := (&Server{}).buildSimulation(context.Background(), []lsp.WorkspaceEdit{edit}, false)
			if tc.wantError {
				require.Nil(t, sim)
				requireOverlaySnapshotBoundError(t, err, "bytes", overlayRequestSnapshotMaxBytes)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, sim)
			require.Len(t, sim.snapshots, 1)
			require.Len(t, sim.snapshots[0], 1)
		})
	}
}

func TestSimulationStepLimitExactAndPlusOne(t *testing.T) {
	exact := make([]lsp.WorkspaceEdit, overlaySimulationMaxSteps)
	sim, err := (&Server{}).buildSimulation(context.Background(), exact, false)
	require.NoError(t, err)
	require.NotNil(t, sim)
	require.Len(t, sim.steps, overlaySimulationMaxSteps)
	require.Len(t, sim.snapshots, overlaySimulationMaxSteps)

	sim, err = (&Server{}).buildSimulation(
		context.Background(),
		make([]lsp.WorkspaceEdit, overlaySimulationMaxSteps+1),
		false,
	)
	require.Nil(t, sim)
	require.ErrorContains(t, err, "simulation steps exceed limit 16")
}

func TestSimulationRawInputExactAndPlusOne(t *testing.T) {
	exactEdit := "{}" + strings.Repeat(" ", overlaySimulationInputMaxBytes-2)
	_, err := parseWorkspaceEdit(exactEdit)
	require.NoError(t, err)
	_, err = parseWorkspaceEdit(exactEdit + " ")
	require.ErrorContains(t, err, "workspace_edit input exceeds limit")

	exactArray := "[{}]" + strings.Repeat(" ", overlaySimulationInputMaxBytes-4)
	_, err = parsePRReviewEdits(exactArray)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "input exceeds limit")
	_, err = parsePRReviewEdits(exactArray + " ")
	require.ErrorContains(t, err, "edits input exceeds limit")

	server, _, _, _ := setupOverlayServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"steps": exactArray}
	result, callErr := server.handleSimulateChain(context.Background(), req)
	require.NoError(t, callErr)
	require.NotNil(t, result)
	require.True(t, result.IsError)
	require.NotContains(t, toolText(result), "input exceeds limit")

	req.Params.Arguments = map[string]any{"steps": exactArray + " "}
	result, callErr = server.handleSimulateChain(context.Background(), req)
	require.NoError(t, callErr)
	require.NotNil(t, result)
	require.True(t, result.IsError)
	require.Contains(t, toolText(result), "steps input exceeds limit")
}

func TestSimulationArrayStepLimitCheckedBeforeElementParsing(t *testing.T) {
	exact := rawEmptyWorkspaceEditArray(overlaySimulationMaxSteps)
	_, err := parsePRReviewEdits(exact)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "exceed limit")

	_, err = parsePRReviewEdits(rawEmptyWorkspaceEditArray(overlaySimulationMaxSteps + 1))
	require.ErrorContains(t, err, "edits exceed limit 16")

	server, _, _, _ := setupOverlayServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"steps": exact}
	result, callErr := server.handleSimulateChain(context.Background(), req)
	require.NoError(t, callErr)
	require.NotContains(t, toolText(result), "steps exceed limit")

	req.Params.Arguments = map[string]any{"steps": rawEmptyWorkspaceEditArray(overlaySimulationMaxSteps + 1)}
	result, callErr = server.handleSimulateChain(context.Background(), req)
	require.NoError(t, callErr)
	require.Contains(t, toolText(result), "steps exceed limit 16")
}

func TestCompareBranchesPropagatesCancellationDuringConstruction(t *testing.T) {
	server, sessionID, _, targetFile, _, baseCtx := branchBootstrap(t)
	for _, branch := range []string{"a", "b"} {
		_, err := server.OverlayManager().Fork(sessionID, daemon.ForkOptions{Name: branch})
		require.NoError(t, err)
		require.NoError(t, server.OverlayManager().PushToBranch(
			sessionID,
			branch,
			daemon.OverlayFile{Path: targetFile, Content: "package main\nfunc Target() {}\n"},
			nil,
		))
	}

	ctx := &cancelAfterNErrContext{Context: baseCtx, allowedCalls: 4}
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"a": "a", "b": "b", "kind": "get_callers", "id": "target.go::Target",
	}
	result, err := server.handleCompareBranches(ctx, req)
	require.Nil(t, result)
	require.ErrorIs(t, err, context.Canceled)
	require.GreaterOrEqual(t, ctx.errCalls.Load(), int32(5))
}

func TestRequestContextErrorUsesOnlyParentRequestState(t *testing.T) {
	componentErr := errors.Join(errors.New("extractor timeout"), context.DeadlineExceeded)
	require.NoError(t, requestContextError(context.Background(), componentErr))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, requestContextError(ctx, errors.New("ordinary failure")), context.Canceled)
}

func TestSimulationPublicHandlersPropagateCancellation(t *testing.T) {
	server, _, targetFile, _ := setupOverlayServer(t)
	rawEdit := buildSingleFileEdit(targetFile, "package main\nfunc Target() {}\n")

	for _, tc := range []struct {
		name string
		call func(context.Context) (*mcplib.CallToolResult, error)
	}{
		{
			name: "preview edit",
			call: func(ctx context.Context) (*mcplib.CallToolResult, error) {
				req := mcplib.CallToolRequest{}
				req.Params.Arguments = map[string]any{"workspace_edit": rawEdit, "diagnostics": false}
				return server.handlePreviewEdit(ctx, req)
			},
		},
		{
			name: "simulate chain",
			call: func(ctx context.Context) (*mcplib.CallToolResult, error) {
				req := mcplib.CallToolRequest{}
				req.Params.Arguments = map[string]any{"steps": "[" + rawEdit + "]", "diagnostics": false}
				return server.handleSimulateChain(ctx, req)
			},
		},
		{
			name: "change contract",
			call: func(ctx context.Context) (*mcplib.CallToolResult, error) {
				req := mcplib.CallToolRequest{}
				req.Params.Arguments = map[string]any{"source": "edit", "workspace_edit": rawEdit}
				return server.handleChangeContract(ctx, req)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			result, err := tc.call(ctx)
			require.Nil(t, result)
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestCompareWithOverlayPropagatesPreparationCancellation(t *testing.T) {
	server, _, targetFile, _ := setupOverlayServer(t)
	const sessionID = "canceled-overlay-diff"
	require.NoError(t, server.OverlayManager().RegisterWithID(sessionID, ""))
	require.NoError(t, server.OverlayManager().Push(sessionID, daemon.OverlayFile{
		Path: targetFile, Content: "package main\nfunc Target() {}\n",
	}, nil))

	ctx, cancel := context.WithCancel(WithSessionID(context.Background(), sessionID))
	cancel()
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"kind": "get_callers", "id": "target.go::Target"}
	result, err := server.handleCompareWithOverlay(ctx, req)
	require.Nil(t, result)
	require.ErrorIs(t, err, context.Canceled)
}

func TestPRReviewSimulationPropagatesCancellation(t *testing.T) {
	server, _, targetFile, _ := setupOverlayServer(t)
	const sessionID = "canceled-pr-simulation"
	require.NoError(t, server.OverlayManager().RegisterWithID(sessionID, ""))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": sessionID,
		"edits":      "[" + buildSingleFileEdit(targetFile, "package main\nfunc Target() {}\n") + "]",
	}
	result, gate, err := server.buildPRReviewSimulation(ctx, req)
	require.Nil(t, result)
	require.Equal(t, reviewGate{}, gate)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCompareBranchesBoundsNamedBranchSnapshot(t *testing.T) {
	server, sessionID, _, _, _, ctx := branchBootstrap(t)
	_, err := server.OverlayManager().Fork(sessionID, daemon.ForkOptions{Name: "oversized"})
	require.NoError(t, err)
	for i := 0; i < overlayRequestSnapshotMaxFiles+1; i++ {
		require.NoError(t, server.OverlayManager().PushToBranch(
			sessionID,
			"oversized",
			daemon.OverlayFile{
				Path:    strings.Repeat("./", i) + "sentinel.go",
				Content: "package sentinel\n",
			},
			nil,
		))
	}

	result, body := invokeTool(t, server, ctx, "compare_branches", map[string]any{
		"a":    "oversized",
		"b":    daemon.MainBranchName,
		"kind": "get_callers",
		"id":   "sentinel.go::Sentinel",
	})
	require.True(t, result.IsError)
	require.Contains(t, body, "overlay snapshot too large: files")
}

func rawEmptyWorkspaceEditArray(count int) string {
	if count <= 0 {
		return "[]"
	}
	return "[" + strings.TrimSuffix(strings.Repeat("{},", count), ",") + "]"
}

func pushRawOverlayAliases(t *testing.T, manager *daemon.OverlayManager, sessionID string, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		require.NoError(t, manager.Push(sessionID, daemon.OverlayFile{
			Path:    strings.Repeat("./", i) + "sentinel.go",
			Content: "package sentinel\n",
		}, nil))
	}
}

func requireOverlaySnapshotBoundError(t *testing.T, err error, resource string, limit int) {
	t.Helper()
	var tooLarge *daemon.ErrOverlaySnapshotTooLarge
	require.Error(t, err)
	require.True(t, errors.As(err, &tooLarge), "expected ErrOverlaySnapshotTooLarge, got %T: %v", err, err)
	require.Equal(t, resource, tooLarge.Resource)
	require.Equal(t, limit, tooLarge.Limit)
}

type cancelAfterNErrContext struct {
	context.Context
	allowedCalls int32
	errCalls     atomic.Int32
}

func (c *cancelAfterNErrContext) Err() error {
	if c.errCalls.Add(1) > c.allowedCalls {
		return context.Canceled
	}
	return nil
}

func fullFileSimulationEdit(path, content string) lsp.WorkspaceEdit {
	return lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{
		path: {{
			Range: lsp.Range{
				Start: lsp.Position{},
				End:   lsp.Position{Line: 100_000},
			},
			NewText: content,
		}},
	}}
}
