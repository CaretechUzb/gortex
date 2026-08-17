package indexer

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// R6 (PR #432 review): the extractor version is mixed into the Merkle
// leaf salt, so under Merkle change detection a bump restages the
// language's files on its own. The mtime ledger knows nothing of
// versions — a non-Merkle store upgraded to a binary with a bumped
// extractor kept serving the OLD extraction of mtime-unchanged files
// forever. A full-tree pass must count those files stale and, once the
// restage lands, re-stamp the stored versions so it happens only once.

// indexStateStore wraps the in-memory graph with a durable-index-state
// capability so the version comparison has a stored row to read.
type indexStateStore struct {
	graph.Store
	st    graph.RepoIndexState
	found bool
}

func (s *indexStateStore) GetRepoIndexState(string) (graph.RepoIndexState, bool, error) {
	return s.st, s.found, nil
}

func (s *indexStateStore) SetRepoIndexState(st graph.RepoIndexState) error {
	s.st, s.found = st, true
	return nil
}

func TestIncrementalReindex_NonMerkleExtractorBumpRestagesLanguage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "W.cs"), csVisCrate)

	store := &indexStateStore{Store: graph.New()}
	reg := parser.NewRegistry()
	reg.Register(languages.NewCSharpExtractor())
	idx := New(store, reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(dir)
	_, err := idx.IndexCtx(testCtx(), dir)
	require.NoError(t, err)

	// Simulate the upgrade: the stored row predates the current csharp
	// extractor version. File content and mtimes are untouched.
	stale, _ := json.Marshal(map[string]int{"csharp": extractorVersionForLang("csharp") - 1})
	store.st = graph.RepoIndexState{ExtractorVersions: string(stale)}
	store.found = true

	res, err := idx.incrementalReindexPathsMode(dir, nil, incrementalPathMode{detectDeletions: true})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.StaleFileCount, 1,
		"a version-stale language's files must restage on a full-tree pass even with unchanged mtimes")

	var restamped map[string]int
	require.NoError(t, json.Unmarshal([]byte(store.st.ExtractorVersions), &restamped))
	assert.Equal(t, extractorVersionForLang("csharp"), restamped["csharp"],
		"a clean restage must re-stamp the stored versions so it does not repeat forever")

	res, err = idx.incrementalReindexPathsMode(dir, nil, incrementalPathMode{detectDeletions: true})
	require.NoError(t, err)
	assert.Zero(t, res.StaleFileCount,
		"after the re-stamp an unchanged tree must be a no-op again")
}

// TestIncrementalReindex_NonMerkleVersionCurrentNoRestage: a stored row
// at the current versions must not trigger any restage.
func TestIncrementalReindex_NonMerkleVersionCurrentNoRestage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "W.cs"), csVisCrate)

	store := &indexStateStore{Store: graph.New()}
	reg := parser.NewRegistry()
	reg.Register(languages.NewCSharpExtractor())
	idx := New(store, reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(dir)
	_, err := idx.IndexCtx(testCtx(), dir)
	require.NoError(t, err)

	current, _ := json.Marshal(map[string]int{"csharp": extractorVersionForLang("csharp")})
	store.st = graph.RepoIndexState{ExtractorVersions: string(current)}
	store.found = true

	res, err := idx.incrementalReindexPathsMode(dir, nil, incrementalPathMode{detectDeletions: true})
	require.NoError(t, err)
	assert.Zero(t, res.StaleFileCount,
		"current extractor versions with unchanged mtimes must stay a no-op")
	if !strings.Contains(store.st.ExtractorVersions, "csharp") {
		t.Fatalf("stored row lost: %q", store.st.ExtractorVersions)
	}
}
