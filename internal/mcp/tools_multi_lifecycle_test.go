package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// newCatalogMCPServer builds a Server over a real sqlite store, which is
// what gives it a checkout catalog. The in-memory graph used elsewhere has
// none, so only this shape can show the tools recording identities.
func newCatalogMCPServer(t *testing.T) (*Server, *indexer.MultiIndexer, *store_sqlite.Catalog, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	store, err := store_sqlite.Open(filepath.Join(dir, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	reg := testRegistry()
	mi := indexer.NewMultiIndexer(store, reg, search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })

	eng := query.NewEngine(store)
	singleton := indexer.New(store, reg, config.IndexConfig{}, zap.NewNop())
	srv := NewServer(eng, store, singleton, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})
	return srv, mi, store.Catalog(), dir
}

// TestTrackUntrackRepositoryDrivesTheCheckoutLifecycle proves the MCP
// surface goes through the shared lifecycle rather than its own copy of the
// sequence: a track records the checkout under the MCP intent source, and an
// untrack revokes it and takes the identity with it.
func TestTrackUntrackRepositoryDrivesTheCheckoutLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	ctx := context.Background()
	srv, mi, catalog, dir := newCatalogMCPServer(t)

	repo := filepath.Join(dir, "tracked-repo")
	gitInitWorktreeRepo(t, repo)

	prefix, status := trackRepoPrefix(t, srv, map[string]any{"path": repo})
	require.Equal(t, "tracked", status)
	require.NotNil(t, mi.GetMetadata(prefix))

	binding, ok, err := catalog.GetDedicatedGraph(ctx, indexer.GraphIDFor(prefix))
	require.NoError(t, err)
	require.True(t, ok, "the track recorded a dedicated-graph binding")
	require.NotEmpty(t, binding.OwnerCheckoutID)

	intents, err := catalog.ListTrackingIntents(ctx, binding.OwnerCheckoutID)
	require.NoError(t, err)
	require.Len(t, intents, 1)
	assert.Equal(t, store_sqlite.IntentSourceMCPTrack, intents[0].SourceKind,
		"the tool call is what is recorded as having asked")

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": repo}
	res, err := srv.handleUntrackRepository(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, "untrack_repository failed: %+v", res.Content)
	var payload struct {
		Status  string   `json:"status"`
		Prefix  string   `json:"prefix"`
		Revoked []string `json:"revoked_intents"`
	}
	require.NoError(t, json.Unmarshal([]byte(extractTextFromContent(t, res.Content)), &payload))
	assert.Equal(t, "untracked", payload.Status)
	assert.Equal(t, prefix, payload.Prefix)
	assert.Equal(t, []string{string(store_sqlite.IntentSourceMCPTrack)}, payload.Revoked)

	assert.Nil(t, mi.GetMetadata(prefix), "the repo leaves the corpus")
	checkouts, err := catalog.ListCheckouts(ctx, binding.FamilyID)
	require.NoError(t, err)
	assert.Empty(t, checkouts, "the forget saga removed the identity")

	saved, err := config.LoadGlobal(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	for _, entry := range saved.Repos {
		assert.NotEqual(t, repo, entry.Path, "the untrack is persisted")
	}
	_, err = os.Stat(repo)
	require.NoError(t, err, "untracking never touches the working tree")
}
