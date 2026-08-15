package mcp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The post-read confinement guard is the only thing standing between a won
// symlink-flip race and out-of-repo bytes leaving the handler, but the guard
// helpers can be unit-tested green while the handler no longer calls them.
// Hand back a resolution outside every repo root — what a won race produces —
// and require read_file to refuse rather than serve the buffer.
func TestReadFilePhysicalEvidenceHandlerRefusesOutOfRepoResolution(t *testing.T) {
	srv, dir := setupTestServer(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "inside.txt"), []byte("inside"), 0o644))

	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o644))
	resolvedOutside, err := filepath.EvalSymlinks(outside)
	require.NoError(t, err)

	srv.physicalEvidenceOverride = func(string) ([]byte, physicalReadEvidence, error) {
		return []byte("outside"), physicalReadEvidence{
			resolvedPath:  resolvedOutside,
			contentSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
			byteCount:     len("outside"),
			readAt:        time.Unix(0, 0).UTC(),
		}, nil
	}

	result := callTool(t, srv, "read_file", map[string]any{
		"path": "inside.txt", "physical_evidence": true,
	})
	require.True(t, result.IsError, "handler must reject evidence resolved outside every repo root")
	body := toolResultText(result)
	require.Contains(t, body, "outside every indexed repository root")
	require.NotContains(t, body, "outside\"", "the out-of-repo buffer must not be served")
}

// The in-repo resolution the handler normally sees must still pass, so the
// guard above is a boundary check rather than a blanket refusal.
func TestReadFilePhysicalEvidenceHandlerAcceptsInRepoResolution(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "inside.txt")
	require.NoError(t, os.WriteFile(target, []byte("inside"), 0o644))
	resolvedInside, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)

	srv.physicalEvidenceOverride = func(string) ([]byte, physicalReadEvidence, error) {
		return []byte("inside"), physicalReadEvidence{
			resolvedPath:  resolvedInside,
			contentSHA256: "1111111111111111111111111111111111111111111111111111111111111111",
			byteCount:     len("inside"),
			readAt:        time.Unix(0, 0).UTC(),
		}, nil
	}

	result := callTool(t, srv, "read_file", map[string]any{
		"path": "inside.txt", "physical_evidence": true,
	})
	require.False(t, result.IsError, toolResultText(result))
	got := decodeFileOpsResult(t, result)
	require.Equal(t, resolvedInside, got["resolved_path"])
}
