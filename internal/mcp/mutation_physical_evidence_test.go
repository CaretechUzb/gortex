package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// diskSHA256 hashes the file the way an operator would — an independent read,
// not the buffer the handler happened to keep. Every assertion below compares
// the receipt against this, so a receipt that merely echoes the handler's own
// intent cannot pass.
func diskSHA256(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// The point of the receipt is that after_sha256 describes bytes on disk, so it
// is checked against an independent read of the file rather than against the
// content the call was given.
func TestEditFilePhysicalEvidenceMatchesDiskAfterWrite(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "notes.txt")
	original := []byte("alpha\nbeta\n")
	require.NoError(t, os.WriteFile(target, original, 0o644))

	beforeSum := sha256.Sum256(original)

	result := callTool(t, srv, "edit_file", map[string]any{
		"path": "notes.txt", "old_string": "beta", "new_string": "gamma",
		"physical_evidence": true, "digest": "sha256",
	})
	require.False(t, result.IsError, toolResultText(result))
	got := decodeFileOpsResult(t, result)

	require.Equal(t, "applied", got["status"])
	require.Equal(t, hex.EncodeToString(beforeSum[:]), got["before_sha256"])
	require.Equal(t, diskSHA256(t, target), got["after_sha256"])
	require.Equal(t, "sha256", got["hash_algorithm"])
	require.Equal(t, "full_file", got["hash_scope"])
	require.Equal(t, "disk", got["content_source"])
	require.Equal(t, true, got["disk_verified"])
	require.Equal(t, float64(len("alpha\ngamma\n")), got["byte_count"])
	require.NotEmpty(t, got["resolved_path"])

	_, err := time.Parse(time.RFC3339Nano, got["verified_at"].(string))
	require.NoError(t, err)

	// new_sha stays what it always was — a git blob SHA-1 of the proposed
	// bytes — and must not be confused for the physical digest.
	require.NotEqual(t, got["new_sha"], got["after_sha256"])
	require.Len(t, got["new_sha"], 40)
	require.Len(t, got["after_sha256"], 64)
}

// A create has no prior bytes, and the receipt must say so rather than omit
// before_sha256 and leave the caller unable to tell "absent" from "not asked".
func TestWriteFilePhysicalEvidenceOnCreateReportsBeforeAbsent(t *testing.T) {
	srv, dir := setupTestServer(t)

	result := callTool(t, srv, "write_file", map[string]any{
		"path": "fresh.txt", "content": "brand new\n", "physical_evidence": true,
	})
	require.False(t, result.IsError, toolResultText(result))
	got := decodeFileOpsResult(t, result)

	require.Equal(t, "created", got["status"])
	require.Equal(t, true, got["before_absent"])
	require.NotContains(t, got, "before_sha256")
	require.Equal(t, diskSHA256(t, filepath.Join(dir, "fresh.txt")), got["after_sha256"])
}

// Overwriting an existing file must attest the bytes that were replaced.
func TestWriteFilePhysicalEvidenceOnOverwriteDigestsPriorBytes(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "doc.md")
	original := []byte("# old\n")
	require.NoError(t, os.WriteFile(target, original, 0o644))
	beforeSum := sha256.Sum256(original)

	result := callTool(t, srv, "write_file", map[string]any{
		"path": "doc.md", "content": "# new\n", "physical_evidence": true,
	})
	require.False(t, result.IsError, toolResultText(result))
	got := decodeFileOpsResult(t, result)

	require.Equal(t, "overwritten", got["status"])
	require.Equal(t, hex.EncodeToString(beforeSum[:]), got["before_sha256"])
	require.Equal(t, diskSHA256(t, target), got["after_sha256"])
}

func TestEditSymbolPhysicalEvidenceMatchesDiskAfterWrite(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "main.go")
	beforeSum := sha256.Sum256(func() []byte {
		content, err := os.ReadFile(target)
		require.NoError(t, err)
		return content
	}())

	result := callTool(t, srv, "edit_symbol", map[string]any{
		"id": "main.go::helper", "old_source": "func helper() {}",
		"new_source": "func helper() { _ = 1 }", "physical_evidence": true,
	})
	require.False(t, result.IsError, toolResultText(result))
	got := decodeFileOpsResult(t, result)

	require.Equal(t, "applied", got["status"])
	require.Equal(t, hex.EncodeToString(beforeSum[:]), got["before_sha256"])
	require.Equal(t, diskSHA256(t, target), got["after_sha256"])
}

// Without the flag the response shape is exactly what it was before, so the
// receipt cannot quietly become always-on.
func TestMutationWithoutEvidenceFlagCarriesNoReceipt(t *testing.T) {
	srv, dir := setupTestServer(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("alpha\n"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path": "notes.txt", "old_string": "alpha", "new_string": "beta",
	})
	require.False(t, result.IsError, toolResultText(result))
	got := decodeFileOpsResult(t, result)

	for _, field := range []string{
		"before_sha256", "after_sha256", "hash_algorithm", "hash_scope",
		"content_source", "disk_verified", "resolved_path", "byte_count", "verified_at",
	} {
		require.NotContains(t, got, field)
	}
	require.NotEmpty(t, got["new_sha"])
}

// Every refusal below must also leave the file untouched: an option the handler
// cannot honour has to be rejected before the atomic write, not after it.
func TestMutationEvidenceRefusalsWriteNothing(t *testing.T) {
	original := "alpha\nbeta\n"

	for name, extra := range map[string]map[string]any{
		"unsupported_digest":   {"physical_evidence": true, "digest": "md5"},
		"digest_without_flag":  {"digest": "sha256"},
		"dry_run_has_no_bytes": {"physical_evidence": true, "dry_run": true},
	} {
		t.Run(name, func(t *testing.T) {
			srv, dir := setupTestServer(t)
			target := filepath.Join(dir, "notes.txt")
			require.NoError(t, os.WriteFile(target, []byte(original), 0o644))

			args := map[string]any{"path": "notes.txt", "old_string": "beta", "new_string": "gamma"}
			for key, value := range extra {
				args[key] = value
			}
			result := callTool(t, srv, "edit_file", args)

			require.True(t, result.IsError, "an option the handler cannot honour must fail loudly, not be dropped")
			content, err := os.ReadFile(target)
			require.NoError(t, err)
			require.Equal(t, original, string(content), "a refused call must not have written")
		})
	}
}

// write_file takes the same refusals, including on the create path where there
// is no prior file to protect but still no receipt to issue.
func TestWriteFileEvidenceRefusesDryRunAndBadDigest(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"unsupported_digest":  {"physical_evidence": true, "digest": "sha1"},
		"digest_without_flag": {"digest": "sha256"},
		"dry_run":             {"physical_evidence": true, "dry_run": true},
	} {
		t.Run(name, func(t *testing.T) {
			srv, dir := setupTestServer(t)
			args := map[string]any{"path": "fresh.txt", "content": "x\n"}
			for key, value := range extra {
				args[key] = value
			}
			result := callTool(t, srv, "write_file", args)

			require.True(t, result.IsError, toolResultText(result))
			_, err := os.Stat(filepath.Join(dir, "fresh.txt"))
			require.True(t, os.IsNotExist(err), "a refused write must not have created the file")
		})
	}
}

// before_sha256 is a digest of bytes the caller did not supply, so a secrets
// file needs the same explicit intent read_file demands. The refusal has to
// land before the write, or the gate would cost the caller their edit.
func TestMutationEvidenceWithheldForSecretConfigLeaf(t *testing.T) {
	srv, dir := setupTestServer(t)
	secrets := "# local development environment\nPASSWORD=hunter2\nAPI_TOKEN=sk-live-abcdef\n"
	target := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(target, []byte(secrets), 0o600))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path": ".env", "old_string": "hunter2", "new_string": "hunter3",
		"physical_evidence": true,
	})
	require.True(t, result.IsError, "a secrets file must not yield a mutation receipt")

	body := toolResultText(result)
	sum := sha256.Sum256([]byte(secrets))
	require.NotContains(t, body, hex.EncodeToString(sum[:]),
		"the refusal must not carry the digest it refused to issue")

	content, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, secrets, string(content), "the gate must refuse before the write, not after")
}

// The same edit without the receipt still works, so the gate is scoped to the
// digest rather than blocking edits to config files.
func TestSecretConfigLeafStillEditableWithoutEvidence(t *testing.T) {
	srv, dir := setupTestServer(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"),
		[]byte("PASSWORD=hunter2\n"), 0o600))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path": ".env", "old_string": "hunter2", "new_string": "hunter3",
	})
	require.False(t, result.IsError, toolResultText(result))
}

// The post-write read-back is a second file-touching sink, and a symlink can be
// retargeted between path resolution and that read. Hand back a resolution
// outside every repo root — what a won race produces — and require the receipt
// to be withheld. The write itself already landed, so the call must still
// report success rather than invite a retry that edits twice.
func TestMutationEvidenceRefusesOutOfRepoResolutionAfterWrite(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(target, []byte("alpha\nbeta\n"), 0o644))

	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
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

	result := callTool(t, srv, "edit_file", map[string]any{
		"path": "notes.txt", "old_string": "beta", "new_string": "gamma",
		"physical_evidence": true,
	})
	require.False(t, result.IsError, "the write landed; the call must report it truthfully")
	got := decodeFileOpsResult(t, result)

	require.Equal(t, "applied", got["status"])
	require.Contains(t, got, "evidence_error")
	require.Contains(t, got["evidence_error"], "outside every indexed repository root")
	require.NotContains(t, got, "after_sha256")
	require.NotContains(t, got, "resolved_path")
	require.NotContains(t, got, "before_sha256")

	// The edit itself is real and unaffected by the withheld receipt.
	content, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "alpha\ngamma\n", string(content))
}

// AtomicWriteFile writes exactly the buffer it is handed, so on a healthy run a
// digest of that buffer and a digest of the file are identical — which means
// the happy-path tests alone cannot tell the two apart, and a regression that
// hashed the intended bytes would sail through them. Force the two to disagree:
// the receipt must carry what the disk read-back reported, never a digest the
// handler could have computed from its own buffer.
func TestMutationEvidenceComesFromTheDiskReadBack(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(target, []byte("alpha\nbeta\n"), 0o644))
	resolvedTarget, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)

	const sentinel = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	srv.physicalEvidenceOverride = func(string) ([]byte, physicalReadEvidence, error) {
		return []byte("whatever is really on disk"), physicalReadEvidence{
			resolvedPath:  resolvedTarget,
			contentSHA256: sentinel,
			byteCount:     4242,
			readAt:        time.Unix(0, 0).UTC(),
		}, nil
	}

	result := callTool(t, srv, "edit_file", map[string]any{
		"path": "notes.txt", "old_string": "beta", "new_string": "gamma",
		"physical_evidence": true,
	})
	require.False(t, result.IsError, toolResultText(result))
	got := decodeFileOpsResult(t, result)

	require.Equal(t, sentinel, got["after_sha256"],
		"after_sha256 must come from the disk read-back, not from the buffer the handler wrote")
	require.Equal(t, float64(4242), got["byte_count"])
	require.NotEqual(t, diskSHA256(t, target), got["after_sha256"])
}

// A read-back that cannot be completed at all is reported the same way: the
// mutation stands, the receipt does not.
func TestMutationEvidenceReportsReadBackFailure(t *testing.T) {
	srv, dir := setupTestServer(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("alpha\nbeta\n"), 0o644))

	srv.physicalEvidenceOverride = func(string) ([]byte, physicalReadEvidence, error) {
		return nil, physicalReadEvidence{}, os.ErrPermission
	}

	result := callTool(t, srv, "edit_file", map[string]any{
		"path": "notes.txt", "old_string": "beta", "new_string": "gamma",
		"physical_evidence": true,
	})
	require.False(t, result.IsError, toolResultText(result))
	got := decodeFileOpsResult(t, result)

	require.Equal(t, "applied", got["status"])
	require.Contains(t, got, "evidence_error")
	require.NotContains(t, got, "after_sha256")
}

// The options have to reach the published schema, or an agent can only find
// them by guessing.
func TestMutationPhysicalEvidenceIsPublishedByFacadeSchema(t *testing.T) {
	srv, _ := setupTestServer(t)

	for _, operation := range []string{"file", "write", "symbol"} {
		t.Run(operation, func(t *testing.T) {
			spec, ok := srv.facades.operation("edit", operation)
			require.True(t, ok, "edit.%s must be a registered facade operation", operation)
			capability := srv.facadeCapability(spec, true)
			schema := capability["input_schema"].(map[string]any)
			properties := schema["properties"].(map[string]any)
			options := properties["options"].(map[string]any)
			fields := options["properties"].(map[string]any)
			require.Contains(t, fields, "physical_evidence")
			require.Contains(t, fields, "digest")
		})
	}
}
