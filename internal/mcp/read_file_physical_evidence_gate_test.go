package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The secret-intent gate must be judged on the bytes the digest covers, not on
// what survived into the response. Every one of these option sets drops the
// secret-shaped lines from the returned representation, which used to leave
// redaction with zero hits and ship the full-file digest of a secrets file
// without allow_secrets.
func TestReadFilePhysicalEvidenceSecretGateSurvivesContentTransforms(t *testing.T) {
	secrets := []byte("# local development environment\nPASSWORD=hunter2\nAPI_TOKEN=sk-live-abcdef\n")

	for name, options := range map[string]map[string]any{
		"whole_file":     {},
		"line_window":    {"offset": 1, "limit": 1},
		"window_to_head": {"offset": 1, "limit": 2},
		"max_lines":      {"max_lines": 1},
		"max_chars":      {"max_chars": 4},
	} {
		t.Run(name, func(t *testing.T) {
			srv, dir := setupTestServer(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), secrets, 0o600))

			args := map[string]any{"path": ".env", "physical_evidence": true}
			for key, value := range options {
				args[key] = value
			}
			result := callTool(t, srv, "read_file", args)

			require.True(t, result.IsError, "a secrets file must not yield physical evidence without allow_secrets")
			body := toolResultText(result)
			require.Contains(t, body, "requires allow_secrets=true")

			sum := sha256.Sum256(secrets)
			require.NotContains(t, body, hex.EncodeToString(sum[:]),
				"the refusal must not carry the digest it refused to issue")
		})
	}
}

// allow_secrets=true is the documented opt-in and still returns both the
// plaintext and the digest, so the gate stays an intent check rather than a
// capability the caller cannot reach.
func TestReadFilePhysicalEvidenceSecretGateHonoursIntentUnderWindow(t *testing.T) {
	srv, dir := setupTestServer(t)
	secrets := []byte("# local development environment\nPASSWORD=hunter2\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), secrets, 0o600))

	got := decodeFileOpsResult(t, callTool(t, srv, "read_file", map[string]any{
		"path": ".env", "physical_evidence": true, "allow_secrets": true,
		"offset": 1, "limit": 1,
	}))
	sum := sha256.Sum256(secrets)
	require.Equal(t, hex.EncodeToString(sum[:]), got["content_sha256"])
	require.Equal(t, "# local development environment", got["content"])
}

// A config leaf with no secret-shaped values is unaffected: the gate must not
// refuse every dotfile that happens to be a config leaf.
func TestReadFilePhysicalEvidenceSecretGateAllowsCleanConfigLeaf(t *testing.T) {
	srv, dir := setupTestServer(t)
	clean := []byte("# no secrets here\nLOG_LEVEL=debug\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), clean, 0o600))

	got := decodeFileOpsResult(t, callTool(t, srv, "read_file", map[string]any{
		"path": ".env", "physical_evidence": true,
	}))
	sum := sha256.Sum256(clean)
	require.Equal(t, hex.EncodeToString(sum[:]), got["content_sha256"])
	require.Equal(t, true, got["same_buffer_as_content"])
}
