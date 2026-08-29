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

// derivedRepo is the shape of a repo that has been indexed, derived and
// enriched and has nothing outstanding — the baseline every case below varies
// one field of.
func derivedRepo() (repoStatus, readinessInputs) {
	entry := repoStatus{Name: "r", Path: "/tmp/r", Indexed: true}
	in := readinessInputs{
		deriveTable: true,
		enrichTable: true,
		passVersion: indexer.DerivePassVersion,
		repo: store_sqlite.RepoReadiness{
			ContentGen:  7,
			DeriveFound: true,
			Derive: graph.DeriveState{
				DerivedContentGen: 7,
				PassVersion:       indexer.DerivePassVersion,
			},
			EnrichProviders:     2,
			EnrichMinContentGen: 7,
			EnrichRows:          2,
		},
	}
	return entry, in
}

// The verdict ladder, one case per rung, in the order they outrank each other.
// Keeping it pure is what makes this table possible: no git, no sqlite, no
// daemon, and every state reachable from a struct literal.
func TestReadyVerdictLadder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*repoStatus, *readinessInputs)
		want   string
	}{
		{"a settled repo", func(*repoStatus, *readinessInputs) {}, readyLabelReady},

		{"a deleted checkout outranks everything — it can never be ready again",
			func(e *repoStatus, _ *readinessInputs) { e.Missing = true }, readyLabelMissing},

		{"never indexed outranks a derive verdict; there is nothing to derive from",
			func(e *repoStatus, _ *readinessInputs) { e.Indexed = false }, readyLabelNotIndexed},

		{"a stale index outranks a stale derive: fix the first and the second follows",
			func(e *repoStatus, _ *readinessInputs) { e.Stale = true }, readyLabelStale},

		{"work in flight is named, not accused",
			func(_ *repoStatus, in *readinessInputs) {
				in.deriving = true
				in.repo.DeriveFound = false
			}, readyLabelDeriving},

		{"enrichment in flight, same",
			func(_ *repoStatus, in *readinessInputs) {
				in.enriching = true
				in.repo.EnrichMinContentGen = 0
			}, readyLabelEnriching},

		{"a store older than this binary is unknown, not a missing derive",
			func(_ *repoStatus, in *readinessInputs) { in.deriveTable = false }, readyLabelUnknown},

		{"THE bug: a repo the derived passes never ran for",
			func(_ *repoStatus, in *readinessInputs) { in.repo.DeriveFound = false },
			readyLabelNeverDerived},

		{"a v13 legacy row asserts nothing and must not be laundered into ready",
			func(_ *repoStatus, in *readinessInputs) { in.repo.Derive.Legacy = true }, readyLabelUnknown},

		{"content moved since the derive",
			func(_ *repoStatus, in *readinessInputs) { in.repo.ContentGen = 8 }, readyLabelPartial},

		{"derived by an older synthesis version",
			func(_ *repoStatus, in *readinessInputs) { in.repo.Derive.PassVersion = 0 }, readyLabelPartial},

		{"derive-relevant config changed under it",
			func(_ *repoStatus, in *readinessInputs) {
				in.configHash = "beef"
				in.repo.Derive.ConfigHash = "cafe"
			}, readyLabelPartial},

		{"one enrichment provider is behind",
			func(_ *repoStatus, in *readinessInputs) { in.repo.EnrichMinContentGen = 6 },
			readyLabelPartial},

		{"no provider applies — n/a never blocks ready",
			func(_ *repoStatus, in *readinessInputs) {
				in.repo.EnrichProviders = 0
				in.repo.EnrichMinContentGen = 0
				in.repo.EnrichNoneDeclared = true
				in.repo.EnrichRows = 1
			}, readyLabelReady},

		{"no config hash published means no comparison, not a failed one",
			func(_ *repoStatus, in *readinessInputs) {
				in.configHash = ""
				in.repo.Derive.ConfigHash = "whatever"
			}, readyLabelReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entry, in := derivedRepo()
			tc.mutate(&entry, &in)
			label, reason := readyVerdict(entry, in)
			require.Equal(t, tc.want, label)
			if label == readyLabelReady {
				require.Empty(t, reason, "a ready repo needs no remediation")
			} else {
				require.NotEmpty(t, reason, "every not-ready state must say what to do")
			}
		})
	}
}

// The enrichment sub-verdict, and the distinction the __none__ sentinel exists
// to draw. "Nothing applies" is an answer; "nothing recorded" is the absence of
// one, and only the first can be trusted to let a repo through.
func TestEnrichVerdict(t *testing.T) {
	t.Parallel()
	base := store_sqlite.RepoReadiness{ContentGen: 5}

	for _, tc := range []struct {
		name  string
		table bool
		repo  store_sqlite.RepoReadiness
		want  string
	}{
		{"table absent", false, base, enrichLabelUnknown},
		{"no rows at all — nobody has looked", true, base, enrichLabelUnknown},
		{"only the __none__ sentinel — looked, nothing applies", true,
			store_sqlite.RepoReadiness{ContentGen: 5, EnrichRows: 1, EnrichNoneDeclared: true},
			enrichLabelNA},
		{"every provider current", true,
			store_sqlite.RepoReadiness{ContentGen: 5, EnrichRows: 2, EnrichProviders: 2, EnrichMinContentGen: 5},
			enrichLabelCurrent},
		{"the MINIMUM decides: one fresh provider cannot speak for a sibling that never ran", true,
			store_sqlite.RepoReadiness{ContentGen: 5, EnrichRows: 2, EnrichProviders: 2, EnrichMinContentGen: 0},
			enrichLabelStale},
		{"a provider ahead of the counter is still current", true,
			store_sqlite.RepoReadiness{ContentGen: 5, EnrichRows: 1, EnrichProviders: 1, EnrichMinContentGen: 9},
			enrichLabelCurrent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want,
				enrichVerdict(readinessInputs{enrichTable: tc.table, repo: tc.repo}))
		})
	}
}

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

// The remediation hint is deliberately narrow: only the states where an answer
// is incomplete right now and nothing is already fixing it. Listing the
// self-resolving ones too would bury the actionable ones.
func TestOnlyIncompleteAnswersEarnARemediationHint(t *testing.T) {
	t.Parallel()
	require.True(t, readyBlocksQueries(readyLabelNeverDerived))
	require.True(t, readyBlocksQueries(readyLabelPartial))
	for _, label := range []string{
		readyLabelReady, readyLabelStale, readyLabelUnknown,
		readyLabelDeriving, readyLabelEnriching, readyLabelMissing, readyLabelNotIndexed,
	} {
		require.False(t, readyBlocksQueries(label), label)
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
