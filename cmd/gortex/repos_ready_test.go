package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"

	_ "modernc.org/sqlite"
)

// Scripts grep these values out of a pipe, so the non-TTY cell must be the bare
// label with no escape bytes in it.
func TestReadyCellIsBareOffATTY(t *testing.T) {
	t.Parallel()
	for _, label := range []string{
		readyLabelReady, readyLabelPartial, readyLabelNeverDerived, readyLabelUnknown,
		readyLabelDeriving, readyLabelEnriching, readyLabelStale, readyLabelNotIndexed,
		readyLabelMissing,
	} {
		cell := readyCell(repoStatus{Ready: label}, false)
		require.Equal(t, label, cell)
		require.NotContains(t, cell, "\x1b", "no ANSI on a non-TTY")
	}
}

func TestNotReadyHintNamesTheRepoAndTheFix(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	emitReposNotReadyHint(&buf, []repoStatus{
		{Name: "alpha", Ready: readyLabelNeverDerived, NotReadyReason: "never ran"},
		{Name: "beta", Ready: readyLabelReady},
	})
	out := buf.String()
	require.Contains(t, out, "alpha")
	require.Contains(t, out, "never ran")
	require.Contains(t, out, "gortex daemon restart")
	require.NotContains(t, out, "beta", "a ready repo has nothing to remediate")

	buf.Reset()
	emitReposNotReadyHint(&buf, []repoStatus{{Name: "alpha", Ready: readyLabelReady}})
	require.Empty(t, buf.String(), "no hint when nothing is blocked")
}

// seedReadiness writes derive and enrichment rows into the test's isolated
// store, alongside whatever seedIndexState already put there.
func seedReadiness(t *testing.T, prefix string, mtimes map[string]int64, derive bool, providers []string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(reposBackendPath), 0o755))
	st, err := store_sqlite.Open(reposBackendPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	if len(mtimes) > 0 {
		require.NoError(t, st.BulkSetFileMtimes(prefix, mtimes))
	}
	if providers != nil {
		require.NoError(t, st.DeclareEnrichmentProviders(prefix, providers))
		gen, err := st.RepoContentGen(prefix)
		require.NoError(t, err)
		for _, p := range providers {
			require.NoError(t, st.CompleteEnrichmentProvider(prefix, p, gen))
		}
	}
	if derive {
		require.NoError(t, st.StampDeriveState([]graph.DeriveCompletion{{
			RepoPrefix: prefix, DerivedSHA: "sha", PassVersion: indexer.DerivePassVersion,
		}}, time.Now().Unix()))
	}
}

// End to end against a real store: a fully-processed repo reads ready in the
// table and in --json, and an ordinary file edit flips it to partial with no
// commit and no HEAD movement. That last step is the whole reason this column
// exists — it is the case indexed_at could never see.
func TestRunRepos_ReadyThenPartialAfterAnEdit(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "alpha")
	head := gitInitRepo(t, repoDir)
	reposTestEnv(t, []config.RepoEntry{{Name: "alpha", Path: repoDir}})

	seedIndexState(t, "alpha", head, false, time.Now())
	seedReadiness(t, "alpha", map[string]int64{"README.md": 1}, true, []string{"go-types"})

	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))
	require.Contains(t, buf.String(), readyLabelReady)

	prevJSON := reposJSON
	reposJSON = true
	t.Cleanup(func() { reposJSON = prevJSON })

	cmd, buf = newReposCmd()
	require.NoError(t, runRepos(cmd, nil))
	var entries []repoStatus
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entries))
	require.Len(t, entries, 1)
	assert.Equal(t, readyLabelReady, entries[0].Ready)
	assert.Equal(t, enrichLabelCurrent, entries[0].Enriched)
	assert.True(t, entries[0].Derived)
	assert.Equal(t, entries[0].RepoContentGen, entries[0].DerivedContentGen)

	// One file re-parsed. No commit, no HEAD move — FRESHNESS still says fresh.
	seedReadiness(t, "alpha", map[string]int64{"README.md": 2}, false, nil)

	cmd, buf = newReposCmd()
	require.NoError(t, runRepos(cmd, nil))
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entries))
	assert.False(t, entries[0].Stale, "the index is still current with git HEAD")
	assert.Equal(t, readyLabelPartial, entries[0].Ready)
	assert.NotEmpty(t, entries[0].NotReadyReason)
	assert.Less(t, entries[0].DerivedContentGen, entries[0].RepoContentGen)
}

// A repo the derived passes never ran for — the warmup-swallowed case, which
// had no signal at all before this column. It must read never derived, and the
// hint must name it, even though its index is perfectly fresh.
func TestRunRepos_NeverDerivedIsReportedOnAFreshIndex(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "alpha")
	head := gitInitRepo(t, repoDir)
	reposTestEnv(t, []config.RepoEntry{{Name: "alpha", Path: repoDir}})

	seedIndexState(t, "alpha", head, false, time.Now())
	seedReadiness(t, "alpha", map[string]int64{"README.md": 1}, false, nil)

	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))
	out := buf.String()
	require.Contains(t, out, readyLabelNeverDerived)
	require.Contains(t, out, "gortex daemon restart")
	require.Contains(t, out, "fresh", "FRESHNESS is unaffected — that is the point")
}

// A store written before this feature has no derive_state table. Every repo
// there is unknown; accusing them all of a missing derive would be a false
// alarm on every upgrade, delivered to every user at once.
func TestRunRepos_APreFeatureStoreReadsUnknown(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "alpha")
	head := gitInitRepo(t, repoDir)
	reposTestEnv(t, []config.RepoEntry{{Name: "alpha", Path: repoDir}})

	seedIndexState(t, "alpha", head, false, time.Now())
	dropTable(t, reposBackendPath, "derive_state")

	prevJSON := reposJSON
	reposJSON = true
	t.Cleanup(func() { reposJSON = prevJSON })

	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))
	var entries []repoStatus
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entries))
	require.Len(t, entries, 1)
	assert.Equal(t, readyLabelUnknown, entries[0].Ready)
}

// The table keeps its seventh column and its bare labels off a TTY, so a
// pipeline that greps the output still matches.
func TestRunRepos_TableCarriesTheReadyColumn(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "alpha")
	head := gitInitRepo(t, repoDir)
	reposTestEnv(t, []config.RepoEntry{{Name: "alpha", Path: repoDir}})
	seedIndexState(t, "alpha", head, false, time.Now())
	seedReadiness(t, "alpha", map[string]int64{"README.md": 1}, true, nil)

	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))
	out := buf.String()
	require.Contains(t, out, "READY")
	require.NotContains(t, out, "\x1b", "no ANSI when stderr is not a TTY")

	header := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "READY") {
			header = line
			break
		}
	}
	// READY sits after FRESHNESS so PATH stays last — scripts that take the
	// final column as the path keep working.
	require.Regexp(t,
		`REPO.*HEAD.*INDEXED.*LAST INDEXED.*FRESHNESS.*READY.*PATH`, header)
}

// dropTable simulates a store written by a daemon older than the feature under
// test: the table simply is not there. Opening through store_sqlite.Open first
// would migrate it straight back, so this goes at the file directly.
func dropTable(t *testing.T, path, table string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck // test cleanup
	_, err = db.Exec("DROP TABLE IF EXISTS " + table)
	require.NoError(t, err)
}

// The projection is the only logic this file still owns: readyVerdict lowers a
// repoStatus onto readiness.RepoState. A field dropped there is invisible —
// the verdict stays plausible and simply stops reacting to that column — so
// each of the four is pinned by the label it alone can produce, and Path by
// the remediation command it has to appear in.
func TestReadyVerdictProjectsEveryCheckoutFactItIsGiven(t *testing.T) {
	t.Parallel()
	base := readinessInputs{DeriveTable: true, EnrichTable: true}

	label, reason := readyVerdict(repoStatus{Path: "/tmp/gone", Missing: true}, base)
	require.Equal(t, readyLabelMissing, label)
	require.Contains(t, reason, "/tmp/gone", "Path must reach the remediation command")

	label, reason = readyVerdict(repoStatus{Path: "/tmp/fresh"}, base)
	require.Equal(t, readyLabelNotIndexed, label, "Indexed=false must survive the projection")
	require.Contains(t, reason, "/tmp/fresh")

	label, _ = readyVerdict(repoStatus{Path: "/tmp/r", Indexed: true, Stale: true}, base)
	require.Equal(t, readyLabelStale, label, "Stale must survive the projection")

	// The control: none of the three set, so the ladder falls past them.
	label, _ = readyVerdict(repoStatus{Path: "/tmp/r", Indexed: true}, base)
	require.NotContains(t,
		[]string{readyLabelMissing, readyLabelNotIndexed, readyLabelStale}, label)
}
