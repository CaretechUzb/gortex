package mcp

import (
	"context"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func wikiTool(t *testing.T, srv *Server, ctx context.Context, name string,
	h func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error),
	args map[string]any,
) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := h(ctx, req)
	require.NoError(t, err)
	return res
}

// TestGenerateDocs_ConfinesAgentOutputPath closes an arbitrary-file write.
//
// generate_docs took output_path straight to os.MkdirAll + os.WriteFile with
// no confinement of any kind, while the structurally identical export_graph
// argument two files away was guarded. A prompt-injected agent could
// therefore drive the daemon into truncating any file the daemon user can
// write — the boundary SECURITY.md says is enforced.
func TestGenerateDocs_ConfinesAgentOutputPath(t *testing.T) {
	srv, _, repoRoot := newSingleRepoServer(t)
	realRoot, err := filepath.EvalSymlinks(repoRoot)
	require.NoError(t, err)
	outside := t.TempDir()

	escape := filepath.Join(outside, "escape.md")
	agentCtx := WithSessionCWD(context.Background(), realRoot)

	res := wikiTool(t, srv, agentCtx, "generate_docs", srv.handleGenerateDocs,
		map[string]any{"output_path": escape})
	require.True(t, res.IsError, "an agent write outside every repo root must be refused")
	txt, _ := singleTextContent(res)
	require.Contains(t, txt, "outside every indexed repository")
	require.NoFileExists(t, escape, "the guard must run before os.WriteFile")

	// No false positive: writing inside the repo is still allowed.
	inside := filepath.Join(realRoot, "docs-out.md")
	res = wikiTool(t, srv, agentCtx, "generate_docs", srv.handleGenerateDocs,
		map[string]any{"output_path": inside})
	require.False(t, res.IsError, "an in-repo write must still succeed: %v", res)
	require.FileExists(t, inside)

	// The operator-driven CLI / control channel carries no session cwd and
	// stays free to write where the user names — same rule as export_graph.
	cliEscape := filepath.Join(outside, "cli.md")
	res = wikiTool(t, srv, context.Background(), "generate_docs", srv.handleGenerateDocs,
		map[string]any{"output_path": cliEscape})
	require.False(t, res.IsError, "the CLI channel must stay unconfined: %v", res)
	require.FileExists(t, cliEscape)
}

// TestGenerateWiki_ConfinesAgentOutputDir is the directory-creating sibling:
// output_dir becomes the writer root, so it steers where the whole generated
// tree lands.
func TestGenerateWiki_ConfinesAgentOutputDir(t *testing.T) {
	srv, _, repoRoot := newSingleRepoServer(t)
	realRoot, err := filepath.EvalSymlinks(repoRoot)
	require.NoError(t, err)
	outside := t.TempDir()

	escapeDir := filepath.Join(outside, "escape-wiki")
	agentCtx := WithSessionCWD(context.Background(), realRoot)

	res := wikiTool(t, srv, agentCtx, "generate_wiki", srv.handleGenerateWiki,
		map[string]any{"output_dir": escapeDir})
	require.True(t, res.IsError, "an agent out-dir outside every repo root must be refused")
	require.NoDirExists(t, escapeDir, "the guard must run before MkdirAll")

	// `repo` is joined under output_dir, so it is a second route to the same
	// primitive and must be confined too.
	res = wikiTool(t, srv, agentCtx, "generate_wiki", srv.handleGenerateWiki,
		map[string]any{"output_dir": realRoot, "repo": "../../../escape-via-repo"})
	require.True(t, res.IsError, "a traversing repo segment must be refused")
}
