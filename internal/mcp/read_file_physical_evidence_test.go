package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadFilePhysicalEvidenceHashesFullDiskBuffer(t *testing.T) {
	srv, dir := setupTestServer(t)
	content := []byte("alpha\nbeta\ngamma\n")
	target := filepath.Join(dir, "evidence.txt")
	require.NoError(t, os.WriteFile(target, content, 0o644))

	result := callTool(t, srv, "read_file", map[string]any{
		"path":              "evidence.txt",
		"physical_evidence": true,
		"digest":            "sha256",
		"offset":            2,
		"limit":             1,
	})
	require.False(t, result.IsError)
	got := decodeFileOpsResult(t, result)
	sum := sha256.Sum256(content)

	require.Equal(t, "beta", got["content"])
	require.Equal(t, hex.EncodeToString(sum[:]), got["content_sha256"])
	require.Equal(t, "sha256", got["hash_algorithm"])
	require.Equal(t, "full_file", got["hash_scope"])
	require.Equal(t, "disk", got["hash_source"])
	require.Equal(t, "disk", got["content_source"])
	require.Equal(t, true, got["disk_verified"])
	require.Equal(t, false, got["same_buffer_as_content"])
	require.Equal(t, true, got["content_truncated"])
	require.Equal(t, float64(len(content)), got["byte_count"])
	require.Equal(t, target, got["resolved_path"])
	require.Equal(t, "regular", got["file_kind"])
	require.NotEmpty(t, got["read_at"])
}

func TestReadFilePhysicalEvidenceSameBufferForFullText(t *testing.T) {
	srv, dir := setupTestServer(t)
	content := []byte("unchanged full text\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "full.txt"), content, 0o644))

	spec, ok := srv.facades.operation("read", "file")
	require.True(t, ok)
	arguments := normalizeFacadeArguments(spec, map[string]any{
		"operation": "file",
		"target":    map[string]any{"file": "full.txt"},
		"options": map[string]any{
			"physical_evidence": true,
			"digest":            "sha256",
		},
	})
	result := callTool(t, srv, spec.Legacy, arguments)
	require.False(t, result.IsError, toolResultText(result))
	got := decodeFileOpsResult(t, result)
	require.Equal(t, string(content), got["content"])
	require.Equal(t, true, got["same_buffer_as_content"])
	require.Equal(t, false, got["content_truncated"])
}

func TestReadFilePhysicalEvidenceBinaryAndEmptyDigests(t *testing.T) {
	for _, test := range []struct {
		name    string
		content []byte
	}{
		{name: "binary", content: []byte{0x00, 0xff, 0x10, 0x80}},
		{name: "empty", content: []byte{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv, dir := setupTestServer(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "blob.bin"), test.content, 0o644))
			result := callTool(t, srv, "read_file", map[string]any{
				"path":              "blob.bin",
				"physical_evidence": true,
			})
			require.False(t, result.IsError)
			got := decodeFileOpsResult(t, result)
			sum := sha256.Sum256(test.content)
			require.Equal(t, hex.EncodeToString(sum[:]), got["content_sha256"])
			require.Equal(t, float64(len(test.content)), got["byte_count"])
			if test.name == "binary" {
				require.Equal(t, false, got["same_buffer_as_content"])
				require.Equal(t, true, got["content_truncated"])
			}
		})
	}
}

func TestReadFilePhysicalEvidenceETagIgnoresObservationTime(t *testing.T) {
	srv, dir := setupTestServer(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stable.txt"), []byte("stable\n"), 0o644))
	args := map[string]any{"path": "stable.txt", "physical_evidence": true}

	first := decodeFileOpsResult(t, callTool(t, srv, "read_file", args))
	second := decodeFileOpsResult(t, callTool(t, srv, "read_file", args))
	require.Equal(t, first["etag"], second["etag"])
}

func TestReadFilePhysicalEvidenceRejectsInvalidDigestContract(t *testing.T) {
	srv, _ := setupTestServer(t)

	for _, args := range []map[string]any{
		{"path": "main.go", "physical_evidence": true, "digest": "md5"},
		{"path": "main.go", "digest": "sha256"},
	} {
		result := callTool(t, srv, "read_file", args)
		require.True(t, result.IsError)
	}
}

func TestReadFilePhysicalEvidenceIsPublishedByFacadeSchema(t *testing.T) {
	srv, _ := setupTestServer(t)
	spec, ok := srv.facades.operation("read", "file")
	require.True(t, ok)
	capability := srv.facadeCapability(spec, true)
	schema := capability["input_schema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	options := properties["options"].(map[string]any)
	fields := options["properties"].(map[string]any)
	require.Contains(t, fields, "physical_evidence")
	require.Contains(t, fields, "digest")
}
