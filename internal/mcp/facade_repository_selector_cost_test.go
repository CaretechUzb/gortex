package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// stubFacadeLegacy re-captures a legacy handler under its real schema so a
// measurement sees facade plumbing only, without the handler's own work.
func stubFacadeLegacy(t *testing.T, srv *Server, legacy string) {
	t.Helper()
	existing, ok := srv.facades.legacy(legacy)
	require.True(t, ok, "legacy tool %s must be captured", legacy)
	srv.facades.capture(existing.tool, func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("{}"), nil
	})
}

func facadeRequest(facade string, args map[string]any) mcpgo.CallToolRequest {
	req := mcpgo.CallToolRequest{}
	req.Params.Name = facade
	req.Params.Arguments = args
	return req
}

// Resolving the operation's published repository selector builds the whole
// public capability schema, which re-materialises the immutable operation table
// and re-probes every legacy property. Doing that per request made a call
// carrying the canonical selector cost ~40x the allocations of the same call
// without one. The selector path must stay in the same order of magnitude as
// the plain path.
func TestFacadeRepositorySelectorDoesNotDominateDispatchCost(t *testing.T) {
	srv, _ := setupTestServer(t)
	stubFacadeLegacy(t, srv, "detect_changes")

	measure := func(args map[string]any) float64 {
		req := facadeRequest("change", args)
		return testing.AllocsPerRun(50, func() {
			_, _ = srv.handleFacade(context.Background(), "change", req)
		})
	}

	plain := measure(map[string]any{"operation": "detect"})
	selector := measure(map[string]any{
		"operation": "detect",
		"options":   map[string]any{"repo": "repo-a"},
	})

	// 4x leaves generous headroom over the ~1.6x this actually costs while
	// still failing loudly on an uncached schema build (~40x).
	require.LessOrEqual(t, selector, plain*4,
		"selector dispatch allocated %.0f vs %.0f without a selector — the published-schema lookup is being rebuilt per request",
		selector, plain)
}

// The memo is keyed on the captured tool set, so a handler registered after the
// first lookup must not be answered from the pre-registration result.
func TestFacadeRepositorySelectorCacheDropsOnLateRegistration(t *testing.T) {
	srv, _ := setupTestServer(t)
	spec, ok := srv.facades.operation("change", "detect")
	require.True(t, ok)

	noop := func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("{}"), nil
	}
	srv.facades.capture(mcpgo.NewTool("detect_changes", mcpgo.WithString("scope")), noop)
	require.Empty(t, srv.facadePublicRepositoryField(spec),
		"a handler that declares no repo must publish no repository selector")

	srv.facades.capture(mcpgo.NewTool("detect_changes",
		mcpgo.WithString("scope"), mcpgo.WithString("repo")), noop)
	require.Equal(t, "options.repo", srv.facadePublicRepositoryField(spec),
		"the late registration declares repo, so the memoized answer must be dropped")
}
