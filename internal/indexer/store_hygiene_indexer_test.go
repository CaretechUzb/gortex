package indexer

import (
	"context"
	"fmt"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// newSqliteMultiIndexer builds a MultiIndexer over a real on-disk sqlite
// store (the only backend with sidecar tables) for the two repos in repos.
// Returns the indexer and the store so a test can read the persisted
// sidecars directly.
func newSqliteMultiIndexer(t *testing.T, repos []config.RepoEntry) (*MultiIndexer, *store_sqlite.Store) {
	t.Helper()
	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: repos}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	mi := NewMultiIndexer(graph.Store(s), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	return mi, s
}

// D1: untracking a prefixed repo must take the capability path (PurgeRepo)
// and clear its sidecar rows, not just nodes+edges — otherwise a store
// accumulates file_mtimes/etc. residue across untrack/retrack cycles.
func TestUntrackRepo_PurgesSidecarRows(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")
	mi, s := newSqliteMultiIndexer(t, []config.RepoEntry{
		{Path: repoA, Name: "repo-a"},
		{Path: repoB, Name: "repo-b"},
	})
	_, err := mi.IndexAll()
	require.NoError(t, err)
	require.True(t, mi.IsMultiRepo(), "two repos -> prefixed multi-repo mode")

	require.NotEmpty(t, s.LoadFileMtimes("repo-a"), "repo-a mtimes persisted under its prefix")
	require.NotEmpty(t, s.LoadFileMtimes("repo-b"), "repo-b mtimes persisted under its prefix")

	mi.UntrackRepo("repo-a")

	// PurgeRepo cleared repo-a's sidecar (EvictRepo alone would have leaked
	// it); repo-b untouched.
	assert.Empty(t, s.LoadFileMtimes("repo-a"), "untrack purged repo-a's file_mtimes sidecar")
	assert.Empty(t, s.GetRepoNodes("repo-a"), "untrack evicted repo-a's nodes")
	assert.NotEmpty(t, s.LoadFileMtimes("repo-b"), "repo-b sidecars intact")
	assert.NotEmpty(t, s.GetRepoNodes("repo-b"), "repo-b nodes intact")
}

// D3: a lone repo persists its file_mtimes under the SAME prefix the warm
// restart will look them up by, and adding a second repo does not move them.
//
// This is the warm-restart bug in its most direct form. A lone repo used to
// index unprefixed, writing its mtimes under the empty prefix while
// EffectiveRepoPrefix returned the basename; the lookup found zero rows and
// forced a full cold
// re-index (with an API embedder, a paid re-embed) on every restart. A
// re-mint migration fired when a second repo joined, moving the residue —
// one more moving part that had to agree with the indexer's gate.
//
// Now there is one key from the first index onward and nothing to migrate,
// which is what this asserts.
func TestLoneRepo_PersistsMtimesUnderItsOwnPrefix(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	mi, s := newSqliteMultiIndexer(t, []config.RepoEntry{{Path: repoA, Name: "repo-a"}})
	_, err := mi.IndexAll()
	require.NoError(t, err)
	require.False(t, mi.IsMultiRepo(), "one tracked repo")

	require.NotEmpty(t, s.LoadFileMtimes("repo-a"), "a lone repo persists mtimes under its own prefix")
	require.Empty(t, s.LoadFileMtimes(""), "nothing is written under the empty prefix")

	// A second repo joining changes nothing about the first repo's keys —
	// there is no migration to run.
	repoB := setupRepoDir(t, "repo-b")
	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repoB, Name: "repo-b"})
	require.NoError(t, err)
	require.True(t, mi.IsMultiRepo())

	assert.NotEmpty(t, s.LoadFileMtimes("repo-a"), "repo-a's mtimes stay where they were written")
	assert.NotEmpty(t, s.LoadFileMtimes("repo-b"), "repo-b persists under its own prefix too")
	assert.Empty(t, s.LoadFileMtimes(""), "still nothing under the empty prefix")
}

// mtimeCountingStore counts BulkSetFileMtimes calls so a test can prove the
// full-index path persists mtimes INCREMENTALLY (before the final
// authoritative ReplaceFileMtimes), not just at the end. Every other method
// is the embedded on-disk store.
type mtimeCountingStore struct {
	*store_sqlite.Store
	bulkCalls atomic.Int64
}

func (m *mtimeCountingStore) BulkSetFileMtimes(prefix string, mtimes map[string]int64) error {
	m.bulkCalls.Add(1)
	return m.Store.BulkSetFileMtimes(prefix, mtimes)
}

// D5: on the direct-to-disk full-index path, mtimes are flushed in batches as
// files land, so a kill mid-track resumes instead of re-tracking from
// scratch. Proven by counting the incremental BulkSetFileMtimes flushes.
func TestFullIndex_PersistsMtimesIncrementallyOnDiskPath(t *testing.T) {
	// Force the direct-to-disk path (no in-memory shadow) and a small flush
	// batch so a handful of files triggers several flushes.
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
	prev := mtimeStreamPersistEvery
	mtimeStreamPersistEvery = 2
	t.Cleanup(func() { mtimeStreamPersistEvery = prev })

	dir := t.TempDir()
	const nFiles = 6
	for i := 0; i < nFiles; i++ {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%d.go", i)),
			fmt.Sprintf("package main\n\nfunc F%d() {}\n", i))
	}

	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Close() })
	s := &mtimeCountingStore{Store: base}

	idx := New(graph.Store(s), newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRootPath(dir)
	_, err = idx.IndexCtx(context.Background(), dir)
	require.NoError(t, err)

	// nFiles files flushed every 2 => the incremental path fired at least
	// twice DURING the parse (the final replace uses ReplaceFileMtimes, which
	// this counter does not see).
	assert.GreaterOrEqual(t, s.bulkCalls.Load(), int64(2),
		"disk-path full index must persist mtimes incrementally via BulkSetFileMtimes")

	// Every file's mtime is durable at the end (final authoritative replace).
	got := base.LoadFileMtimes("")
	for i := 0; i < nFiles; i++ {
		assert.Contains(t, got, fmt.Sprintf("f%d.go", i), "every file's mtime persisted")
	}
}

// D4: the end-of-track content sweep keeps only the files that STREAMED
// content sections this walk — so a file that still exists on disk but
// stopped yielding content (doc emptied) loses its stale rows, and a walk
// that ends with zero content files wipes the repo's content outright. A
// keep-set derived from surviving files would protect both leaks forever.
func TestContentSweep_ReapsEmptiedFilesAndContentlessRepo(t *testing.T) {
	// Force the direct-to-disk path — the crash-relevant one the per-file
	// wipe + end sweep were built for (mirrors content_split's disk_path).
	t.Setenv("GORTEX_SHADOW_MAX_BYTES", "1")

	dir := t.TempDir()
	body := strings.Repeat("searchable prose for the content splitter ", 40)
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc Keep() {}\n")
	writeFile(t, filepath.Join(dir, "doc1.txt"), body)
	writeFile(t, filepath.Join(dir, "doc2.txt"), body)

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default().Index
	cfg.Workers = 2
	_, err = New(graph.Store(store), reg, cfg, zap.NewNop()).IndexCtx(context.Background(), dir)
	require.NoError(t, err)

	contentFiles := func() map[string]bool {
		got := map[string]bool{}
		require.NoError(t, store.ScanContent("", func(_, filePath, _ string) bool {
			got[filePath] = true
			return true
		}))
		return got
	}
	first := contentFiles()
	require.True(t, first["doc1.txt"], "doc1 content indexed on the first walk")
	require.True(t, first["doc2.txt"], "doc2 content indexed on the first walk")

	// doc2 is emptied but SURVIVES on disk: the next full walk streams no
	// sections for it, so it is absent from the streamed set and the sweep
	// reaps its stale rows, while doc1's re-streamed rows survive.
	writeFile(t, filepath.Join(dir, "doc2.txt"), "")
	_, err = New(graph.Store(store), reg, cfg, zap.NewNop()).IndexCtx(context.Background(), dir)
	require.NoError(t, err)

	second := contentFiles()
	assert.True(t, second["doc1.txt"], "still-content file keeps its rows")
	assert.False(t, second["doc2.txt"], "emptied file's stale rows reaped despite surviving on disk")

	// Both docs emptied: the walk streams zero content, so the zero-content
	// branch fires the repo-wide wipe (the sweep's empty-keep no-op guard
	// alone would leak doc1's stale rows).
	writeFile(t, filepath.Join(dir, "doc1.txt"), "")
	_, err = New(graph.Store(store), reg, cfg, zap.NewNop()).IndexCtx(context.Background(), dir)
	require.NoError(t, err)
	assert.Empty(t, contentFiles(), "contentless repo ends with an empty content index")
}
