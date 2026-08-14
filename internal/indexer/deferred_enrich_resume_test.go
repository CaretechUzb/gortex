package indexer

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
)

// commitGitRepo builds a git repo with one committed Go file so repoHead
// returns a real, clean HEAD. Returns the repo path.
func commitGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	gitInitRepo(t, repo)
	writeFile(t, filepath.Join(repo, "main.go"), "package main\n\nfunc main() {}\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "init")
	return repo
}

func openTestSqlite(t *testing.T) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestMaybeSeedPendingEnrich_ResumesIncompleteEnrichment is the core
// regression: an unchanged repo whose prior enrichment left no completion marker
// (a partial / abandoned pass writes none) must have its deferred pass re-armed
// on a warm restart, run to completion, persist the whole-repo marker, and then
// be skipped on every subsequent restart. Without the fix pendingEnrich reflects
// only this run's re-indexing work, so the repo would short-circuit
// runDeferredEnrich forever.
func TestMaybeSeedPendingEnrich_ResumesIncompleteEnrichment(t *testing.T) {
	repo := commitGitRepo(t)
	store := openTestSqlite(t)

	spy := &spyEnrichProvider{}
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(repo)
	idx.SetSemanticManager(newSpyManager(spy))
	// A warm restart starts pendingEnrich=false; no file in this repo changed.
	require.False(t, idx.pendingEnrich.Load())

	// No completion marker persisted yet → the repo is known-incomplete, so the
	// gate is re-armed.
	assert.True(t, idx.MaybeSeedPendingEnrich(),
		"a repo with no completion marker on a clean HEAD must be re-armed")
	assert.True(t, idx.pendingEnrich.Load())

	// The re-armed deferred pass runs and completes non-partial.
	idx.runDeferredEnrich()
	assert.Equal(t, []string{"r"}, spy.invoked(), "the resumed pass must dispatch enrichment")
	assert.False(t, idx.pendingEnrich.Load(), "a clean completion clears the pending flag")

	// A subsequent restart (pendingEnrich reset to false) now sees a current
	// whole-repo marker and does NOT re-arm — the resume is one-shot.
	assert.False(t, idx.MaybeSeedPendingEnrich(),
		"a repo whose completion marker records the current HEAD must not be re-armed")
	assert.False(t, idx.pendingEnrich.Load())

	// And the gate short-circuits the pass, so the provider is never re-invoked.
	idx.runDeferredEnrich()
	assert.Equal(t, []string{"r"}, spy.invoked(),
		"an up-to-date repo must not re-run enrichment")
}

// TestMaybeSeedPendingEnrich_DirtyTreeNotSeeded: a dirty working tree is never
// re-armed. The marker is neither written nor trusted against uncommitted
// content, and resuming every restart while the tree stays dirty would defeat
// the warm-restart fast path.
func TestMaybeSeedPendingEnrich_DirtyTreeNotSeeded(t *testing.T) {
	repo := commitGitRepo(t)
	store := openTestSqlite(t)

	// Make the tree dirty with an uncommitted file.
	writeFile(t, filepath.Join(repo, "extra.go"), "package main\n\nvar Extra = 1\n")

	spy := &spyEnrichProvider{}
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(repo)
	idx.SetSemanticManager(newSpyManager(spy))

	assert.False(t, idx.MaybeSeedPendingEnrich(),
		"a dirty tree must not re-arm the deferred pass")
	assert.False(t, idx.pendingEnrich.Load())
}

// TestMaybeSeedPendingEnrich_NoProvidersNoop: with no semantic providers there
// is nothing to resume, so the seeder is a no-op even with a clean git HEAD and
// no marker.
func TestMaybeSeedPendingEnrich_NoProvidersNoop(t *testing.T) {
	repo := commitGitRepo(t)
	store := openTestSqlite(t)

	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(repo)
	// A manager with no registered providers.
	idx.SetSemanticManager(semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop()))

	assert.False(t, idx.MaybeSeedPendingEnrich())
	assert.False(t, idx.pendingEnrich.Load())
}

// TestSeedPendingEnrichAll_ResumesOnlyIncompleteRepos drives the warmup entry
// point: across a workspace of unchanged repos, only the one whose persisted
// enrichment is incomplete is re-armed and dispatched; a repo with a current
// completion marker is left alone.
func TestSeedPendingEnrichAll_ResumesOnlyIncompleteRepos(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	store := openTestSqlite(t)
	spy := &spyEnrichProvider{}
	mgr := newSpyManager(spy)

	completeRepo := commitGitRepo(t)
	incompleteRepo := commitGitRepo(t)

	complete := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	complete.SetRepoPrefix("complete")
	complete.SetRootPath(completeRepo)
	complete.SetSemanticManager(mgr)

	incomplete := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	incomplete.SetRepoPrefix("incomplete")
	incomplete.SetRootPath(incompleteRepo)
	incomplete.SetSemanticManager(mgr)

	// Pre-seed the "complete" repo's whole-repo marker at its current HEAD, as a
	// prior process's clean completion would have. The "incomplete" repo has no
	// marker (its prior pass was cut short).
	sha := repoHead(completeRepo)
	require.NotEmpty(t, sha)
	mgr.RecordRepoEnrichmentComplete(store, "complete", sha, false)

	mi := newEmptyMultiIndexer(t, store)
	mi.indexers["complete"] = complete
	mi.indexers["incomplete"] = incomplete

	// Only the incomplete repo is re-armed.
	assert.Equal(t, 1, mi.SeedPendingEnrichAll(),
		"exactly one repo (the incomplete one) must be re-armed")
	assert.True(t, incomplete.pendingEnrich.Load())
	assert.False(t, complete.pendingEnrich.Load())

	// The parallel enrich driver then dispatches only the re-armed repo.
	mi.runDeferredEnrichPool([]*Indexer{complete, incomplete})
	assert.Equal(t, []string{"incomplete"}, spy.invoked(),
		"only the resumed repo should have its enrichment dispatched")
}
