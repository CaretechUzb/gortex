package indexer

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// seedWatchIndexedFile writes the KindFile node a live re-parse leaves behind
// when semantic enrichment was deferred: present in the graph, stamped with the
// durable graph.MetaReparsePendingEnrichment ledger marker. Returns the graph
// path.
func seedWatchIndexedFile(t *testing.T, g graph.Store, repoPrefix, relPath string) string {
	t.Helper()
	graphPath := filepath.Join(repoPrefix, relPath)
	g.AddBatch([]*graph.Node{{
		ID:         graphPath,
		Kind:       graph.KindFile,
		Name:       filepath.Base(relPath),
		FilePath:   graphPath,
		Language:   "go",
		RepoPrefix: repoPrefix,
		Meta:       map[string]any{graph.MetaReparsePendingEnrichment: true},
	}}, nil)
	return graphPath
}

// pendingLedger reports the repo's durable per-file deferral ledger, read back
// through the graph so the assertion covers persistence rather than the
// indexer's in-memory scope set.
func pendingLedger(t *testing.T, idx *Indexer) []string {
	t.Helper()
	return idx.pendingEnrichFrontier()
}

// TestMaybeSeedPendingEnrich_NonGitRepoResumesFromLedger is the headline
// regression. A tracked directory that is not a git repository can never have a
// whole-repo completion marker — recordEnrichMarker no-ops on an empty sha — so
// the marker gate declined forever and files the watcher indexed under
// enrich_on_watch=false stayed at their heuristic tier across every restart.
// The durable per-file ledger needs no git state, so it re-arms the pass.
func TestMaybeSeedPendingEnrich_NonGitRepoResumesFromLedger(t *testing.T) {
	repo := t.TempDir() // deliberately NOT a git repository
	store := openTestSqlite(t)

	spy := &spyEnrichProvider{}
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(repo)
	idx.SetSemanticManager(newSpyManager(spy))

	require.Empty(t, repoHead(repo), "the fixture must not be a git repository")
	assert.False(t, idx.MaybeSeedPendingEnrich(),
		"a non-git repo with an empty ledger has nothing to resume")

	// The watcher indexes a file while enrichment is deferred.
	seedWatchIndexedFile(t, store, "r", "watched.go")

	assert.True(t, idx.MaybeSeedPendingEnrich(),
		"a non-git repo whose ledger names outstanding files must be re-armed")
	assert.True(t, idx.pendingEnrich.Load())

	// The resume is file-scoped, not a whole-repo re-enrich.
	files, full, _ := idx.deferredEnrichScope()
	assert.False(t, full, "the ledger names an exact frontier — no full pass")
	assert.Equal(t, []string{filepath.Join("r", "watched.go")}, files)
}

// TestMaybeSeedPendingEnrich_LedgerResumeConverges: the resumed pass must
// discharge the ledger it consumed, or every restart re-arms the same files
// forever. Nothing but a watch-time enrichment cleared the marker before, and
// watch-time enrichment is off by construction on the deferral path.
func TestMaybeSeedPendingEnrich_LedgerResumeConverges(t *testing.T) {
	repo := t.TempDir()
	store := openTestSqlite(t)

	spy := &spyEnrichProvider{}
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(repo)
	idx.SetSemanticManager(newSpyManager(spy))

	seedWatchIndexedFile(t, store, "r", "watched.go")
	require.True(t, idx.MaybeSeedPendingEnrich())

	idx.runDeferredEnrich()
	assert.Equal(t, []string{filepath.Join("r", "watched.go")}, spy.enrichedFiles(),
		"the resumed pass must dispatch enrichment for exactly the ledger's files")
	assert.False(t, idx.pendingEnrich.Load())
	assert.Empty(t, pendingLedger(t, idx),
		"a completed file-scoped pass must discharge the ledger entries it covered")

	// A later restart sees an empty ledger and declines — the resume is one-shot.
	assert.False(t, idx.MaybeSeedPendingEnrich(),
		"a discharged ledger must not re-arm the pass on the next restart")
}

// TestMaybeSeedPendingEnrich_DischargesFilesNoProviderCovers: the discharge
// covers the whole dispatched frontier, not only the paths a provider claimed.
// A file whose language no provider handles still had its deferred pass run;
// leaving it marked would re-arm the repo on every restart in perpetuity.
func TestMaybeSeedPendingEnrich_DischargesFilesNoProviderCovers(t *testing.T) {
	repo := t.TempDir()
	store := openTestSqlite(t)

	spy := &spyEnrichProvider{} // Languages() == ["go"]
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(repo)
	idx.SetSemanticManager(newSpyManager(spy))

	// A Ruby file: stamped by the incremental path (providersPresent is
	// repo-global, not per-language) but dispatched to no provider.
	graphPath := filepath.Join("r", "script.rb")
	store.AddBatch([]*graph.Node{{
		ID: graphPath, Kind: graph.KindFile, Name: "script.rb",
		FilePath: graphPath, Language: "ruby", RepoPrefix: "r",
		Meta: map[string]any{graph.MetaReparsePendingEnrichment: true},
	}}, nil)

	require.True(t, idx.MaybeSeedPendingEnrich())
	idx.runDeferredEnrich()

	assert.Empty(t, pendingLedger(t, idx),
		"an uncovered language must still be discharged by the pass that ran for it")
	assert.False(t, idx.MaybeSeedPendingEnrich(),
		"an uncovered language must not re-arm the repo forever")
}

// TestMaybeSeedPendingEnrich_DirtyTreeResumesFromLedger covers the second half
// of the hole: a real git repo whose completion marker is current for HEAD, but
// whose watch-indexed edits are still uncommitted. The marker gate saw "current"
// and declined; the warmup parse saw the watcher's own persisted results and
// reported nothing changed. Only the ledger knows those files skipped enrichment.
func TestMaybeSeedPendingEnrich_DirtyTreeResumesFromLedger(t *testing.T) {
	repo := commitGitRepo(t)
	store := openTestSqlite(t)

	spy := &spyEnrichProvider{}
	mgr := newSpyManager(spy)
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(repo)
	idx.SetSemanticManager(mgr)

	// A prior process completed enrichment at this HEAD on a clean tree.
	sha := repoHead(repo)
	require.NotEmpty(t, sha)
	mgr.RecordRepoEnrichmentComplete(store, "r", sha, false)
	require.False(t, idx.MaybeSeedPendingEnrich(),
		"a current marker on a clean tree with an empty ledger declines")

	// Then the watcher indexed an uncommitted file with enrichment deferred.
	writeFile(t, filepath.Join(repo, "extra.go"), "package main\n\nvar Extra = 1\n")
	seedWatchIndexedFile(t, store, "r", "extra.go")

	assert.True(t, idx.MaybeSeedPendingEnrich(),
		"uncommitted watch-indexed files must resume even though the marker names HEAD")
	files, full, _ := idx.deferredEnrichScope()
	assert.False(t, full)
	assert.Equal(t, []string{filepath.Join("r", "extra.go")}, files)
}

// TestMaybeSeedPendingEnrich_StaleMarkerStillWinsOverLedger: an absent or stale
// completion marker on a CLEAN tree means the last whole-repo pass never
// finished, so files outside the ledger are unenriched too. That signal must
// keep dominating — a file-scoped resume would silently narrow the recovery.
func TestMaybeSeedPendingEnrich_StaleMarkerStillWinsOverLedger(t *testing.T) {
	repo := commitGitRepo(t)
	store := openTestSqlite(t)

	spy := &spyEnrichProvider{}
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(repo)
	idx.SetSemanticManager(newSpyManager(spy))

	// No completion marker (a partial pass writes none) AND a ledger entry.
	seedWatchIndexedFile(t, store, "r", "watched.go")

	require.True(t, idx.MaybeSeedPendingEnrich())
	_, full, _ := idx.deferredEnrichScope()
	assert.True(t, full,
		"a known-incomplete whole-repo pass must re-arm full scope, not the file frontier")
}

// TestRunDeferredEnrich_FullPassDischargesLedger: a whole-repo pass covers every
// file, so it retires the durable ledger outright. Without this the markers
// accumulate for the life of the graph and find_usages keeps riding every one of
// those files' suppressions with a re-verification-pending flag that no
// completed enrichment could ever retire.
func TestRunDeferredEnrich_FullPassDischargesLedger(t *testing.T) {
	// The whole-repo entry point applies the language admission floor; these
	// fixtures carry a handful of nodes, so disable it to exercise dispatch.
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	repo := commitGitRepo(t)
	store := openTestSqlite(t)

	spy := &spyEnrichProvider{}
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(repo)
	idx.SetSemanticManager(newSpyManager(spy))

	seedWatchIndexedFile(t, store, "r", "a.go")
	seedWatchIndexedFile(t, store, "r", "b.go")
	require.Len(t, pendingLedger(t, idx), 2)

	idx.markPendingEnrichFull()
	idx.runDeferredEnrich()

	assert.Equal(t, []string{"r"}, spy.invoked())
	assert.Empty(t, pendingLedger(t, idx),
		"a completed whole-repo pass must discharge the durable ledger")
}

// TestRunDeferredEnrich_PartialPassKeepsLedger: a partial / abandoned pass must
// not discharge anything, or the outstanding work becomes invisible to the next
// restart — the exact failure mode this ledger exists to prevent.
func TestRunDeferredEnrich_PartialPassKeepsLedger(t *testing.T) {
	repo := t.TempDir()
	store := openTestSqlite(t)

	spy := &spyEnrichProvider{partial: true}
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(repo)
	idx.SetSemanticManager(newSpyManager(spy))

	seedWatchIndexedFile(t, store, "r", "watched.go")
	require.True(t, idx.MaybeSeedPendingEnrich())

	idx.runDeferredEnrich()

	assert.True(t, idx.pendingEnrich.Load(), "a partial pass keeps the in-memory gate armed")
	assert.Equal(t, []string{filepath.Join("r", "watched.go")}, pendingLedger(t, idx),
		"a partial pass must leave the durable ledger intact for the next restart")
}

// TestSeedPendingEnrichAll_ResumesNonGitRepo drives the warmup entry point over
// a mixed workspace: the non-git repo with outstanding ledger entries is
// resumed, the fully-enriched git repo is left alone.
func TestSeedPendingEnrichAll_ResumesNonGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	store := openTestSqlite(t)
	spy := &spyEnrichProvider{}
	mgr := newSpyManager(spy)

	gitRepo := commitGitRepo(t)
	complete := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	complete.SetRepoPrefix("complete")
	complete.SetRootPath(gitRepo)
	complete.SetSemanticManager(mgr)
	mgr.RecordRepoEnrichmentComplete(store, "complete", repoHead(gitRepo), false)

	plain := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	plain.SetRepoPrefix("plain")
	plain.SetRootPath(t.TempDir()) // not a git repository
	plain.SetSemanticManager(mgr)
	seedWatchIndexedFile(t, store, "plain", "watched.go")

	mi := newEmptyMultiIndexer(t, store)
	mi.indexers["complete"] = complete
	mi.indexers["plain"] = plain

	assert.Equal(t, 1, mi.SeedPendingEnrichAll(),
		"only the repo with outstanding ledger entries is re-armed")
	assert.True(t, plain.pendingEnrich.Load())
	assert.False(t, complete.pendingEnrich.Load())

	mi.runDeferredEnrichParallel([]*Indexer{complete, plain})
	assert.Equal(t, []string{filepath.Join("plain", "watched.go")}, spy.enrichedFiles(),
		"only the resumed repo should have its enrichment dispatched")
	assert.Empty(t, spy.invoked(),
		"a ledger resume is file-scoped — it must not trigger a whole-repo pass")
}

// TestMaybeSeedPendingEnrich_NoLedgerNoProvidersNoop keeps the cheapest decline
// free of graph work: without providers there is nothing to resume, ledger or
// not, so the projection is never issued.
func TestMaybeSeedPendingEnrich_NoLedgerNoProvidersNoop(t *testing.T) {
	store := openTestSqlite(t)
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(t.TempDir())
	idx.SetSemanticManager(semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop()))

	seedWatchIndexedFile(t, store, "r", "watched.go")

	assert.False(t, idx.MaybeSeedPendingEnrich(),
		"a repo with no semantic providers has nothing to resume")
	assert.False(t, idx.pendingEnrich.Load())
}
