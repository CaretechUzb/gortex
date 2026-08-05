package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// symlinkSecretMarker is the canary written to the out-of-repo file. No
// response from any tool may ever contain it.
const symlinkSecretMarker = "GORTEX-OUT-OF-REPO-CANARY-9f3a1c"

// TestSearchTextDoesNotDiscloseSymlinkedOutOfRepoContent is the regression
// test for the symlink-following disclosure: a repository containing a
// symlink whose target resolves outside the repo root must not let
// search_text serve that target's content.
//
// The shape of the attack is that the link is named like ordinary source
// (`pwn.go`), so language detection — which works off the path — admits it
// on name alone, and every downstream reader follows the link.
func TestSearchTextDoesNotDiscloseSymlinkedOutOfRepoContent(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret,
		[]byte("BEGIN OUTSIDE-REPO FILE\n"+symlinkSecretMarker+"\nEND OUTSIDE-REPO FILE\n"), 0o600))

	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"),
		[]byte("package app\n\nfunc Hello() {}\n"), 0o644))
	// The link lives INSIDE the repo; its target is OUTSIDE.
	require.NoError(t, os.Symlink(secret, filepath.Join(repo, "pwn.go")))

	srv := newSymlinkTestServer(t, repo)

	decode := func(res *mcplib.CallToolResult) struct {
		Matches []searchTextMatch `json:"matches"`
		Count   int               `json:"count"`
	} {
		t.Helper()
		require.False(t, res.IsError)
		var out struct {
			Matches []searchTextMatch `json:"matches"`
			Count   int               `json:"count"`
		}
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))
		return out
	}

	// A literal search for the exact secret must find nothing.
	lit := decode(callTool(t, srv, "search_text", map[string]any{"query": symlinkSecretMarker}))
	require.Equal(t, 0, lit.Count, "search_text disclosed out-of-repo content: %+v", lit.Matches)

	// Regexp mode runs a different scan loop over the same corpus.
	re := decode(callTool(t, srv, "search_text",
		map[string]any{"query": "GORTEX-OUT-OF-REPO-.*", "regexp": true}))
	require.Equal(t, 0, re.Count, "search_text regexp disclosed out-of-repo content: %+v", re.Matches)

	// A broad sweep needs no knowledge of the secret at all, so it is the
	// cheapest form of the attack and must come back empty too.
	broad := decode(callTool(t, srv, "search_text", map[string]any{"query": "OUTSIDE-REPO"}))
	require.Equal(t, 0, broad.Count, "broad sweep disclosed out-of-repo content: %+v", broad.Matches)

	// No response may name the link or leak a byte of its target.
	for _, q := range []string{"BEGIN", "END", "FILE"} {
		hits := decode(callTool(t, srv, "search_text", map[string]any{"query": q}))
		for _, m := range hits.Matches {
			require.NotEqual(t, "pwn.go", m.Path, "a symlinked out-of-repo file stayed searchable")
			require.NotContains(t, m.Text, symlinkSecretMarker)
		}
	}

	// The in-repo file is still indexed — the guard must not be a blanket
	// "stop searching" regression.
	ok := decode(callTool(t, srv, "search_text", map[string]any{"query": "func Hello"}))
	require.Equal(t, 1, ok.Count)
	require.Equal(t, "main.go", ok.Matches[0].Path)

	// read_file was already guarded; confirm it stays that way.
	rf := callTool(t, srv, "read_file", map[string]any{"path": "pwn.go"})
	require.True(t, rf.IsError, "read_file must refuse a symlink out of the repo")
	require.NotContains(t, rf.Content[0].(mcplib.TextContent).Text, symlinkSecretMarker)
}

// TestSearchTextStillFollowsInRepoSymlink pins the other half of the
// contract: confinement is about leaving the repository, not about symlinks
// as such. A link pointing at a file inside the same repo is legitimate
// (shared config vendored into a package directory, for instance) and must
// keep working.
func TestSearchTextStillFollowsInRepoSymlink(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "pkg"), 0o755))
	const marker = "GORTEX_IN_REPO_SHARED_MARKER"
	require.NoError(t, os.WriteFile(filepath.Join(repo, "shared.go"),
		[]byte("package app\n\n// "+marker+"\nfunc Shared() {}\n"), 0o644))
	require.NoError(t, os.Symlink(
		filepath.Join(repo, "shared.go"), filepath.Join(repo, "pkg", "linked.go")))

	srv := newSymlinkTestServer(t, repo)

	res := callTool(t, srv, "search_text", map[string]any{"query": marker})
	require.False(t, res.IsError)
	var out struct {
		Matches []searchTextMatch `json:"matches"`
		Count   int               `json:"count"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))

	var paths []string
	for _, m := range out.Matches {
		paths = append(paths, m.Path)
	}
	require.Contains(t, paths, filepath.ToSlash(filepath.Join("pkg", "linked.go")),
		"an in-repo symlink must stay indexed and searchable; got %v", paths)
}

func newSymlinkTestServer(t *testing.T, repo string) *Server {
	t.Helper()
	g := graph.New()
	cfg := config.Default()
	idx := indexer.New(g, testRegistry(), cfg.Index, zap.NewNop())
	_, err := idx.Index(repo)
	require.NoError(t, err)
	return NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
}
