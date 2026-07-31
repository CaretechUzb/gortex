package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIndexHealth_SurfacesToolPreset: the tool-surface state (active preset
// + learned-promotion count) rides on index_health so the policy is
// inspectable without a separate call.
func TestIndexHealth_SurfacesToolPreset(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "agent", Mode: "defer"})
	payload := srv.buildIndexHealthPayload()
	require.Equal(t, "agent", payload["tool_preset"])
	require.Equal(t, "defer", payload["tool_preset_mode"])
	// No learned promotions yet on a fresh server → the key is omitted.
	_, hasLearned := payload["learned_tools"]
	require.False(t, hasLearned)

	// After a learned promotion, the count + names surface.
	srv.InitLearnedTools(tempSidecarDir(t), tempSidecarDir(t))
	srv.RecordLearnedPromotion("get_architecture", "")
	payload = srv.buildIndexHealthPayload()
	require.Equal(t, 1, payload["learned_tools"])
	require.Contains(t, payload["learned_tool_names"], "get_architecture")
}
