package semantic

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// EnrichmentOwed is what the warm-restart gate consults once git has run out
// of things to say. On a dirty tree the completion marker is not trustworthy
// and the per-file ledger is drained by whoever indexed the files, so a repo
// whose completions never landed had nothing left that could re-arm it — not
// the marker, not the ledger, and not a restart. It stayed unenriched forever.

// A provider declared applicable and never completed is the shape the hole
// leaves behind, and the one thing that can re-arm the repo.
func TestAProviderThatNeverCompletedIsOwedAPass(t *testing.T) {
	t.Parallel()
	s := applStore(t, "go")
	require.NoError(t, s.BulkSetFileMtimes(applRepo, map[string]int64{"src/main.go": 1}))
	require.NoError(t, s.DeclareEnrichmentProviders(applRepo, []string{"go-types"}))

	owed, known := EnrichmentOwed(s, applRepo)
	require.True(t, known)
	require.True(t, owed, "declared applicable, gen still 0 — no pass has ever run")

	current, err := s.RepoContentGen(applRepo)
	require.NoError(t, err)
	require.NoError(t, s.CompleteEnrichmentProvider(applRepo, "go-types", current))

	owed, known = EnrichmentOwed(s, applRepo)
	require.True(t, known)
	require.False(t, owed, "a completed pass discharges the claim")
}

// The narrowing that keeps this from becoming a restart loop. A repo under
// active edit is behind its content by definition; re-arming on that would
// re-enrich it whole on every daemon start, forever, chasing a number the next
// save moves again. Keeping it current is the watcher's job, and the readiness
// column already reports it as partial.
func TestAProviderMerelyBehindTheContentIsNotOwedAPass(t *testing.T) {
	t.Parallel()
	s := applStore(t, "go")
	require.NoError(t, s.BulkSetFileMtimes(applRepo, map[string]int64{"src/main.go": 1}))
	require.NoError(t, s.DeclareEnrichmentProviders(applRepo, []string{"go-types"}))

	first, err := s.RepoContentGen(applRepo)
	require.NoError(t, err)
	require.NoError(t, s.CompleteEnrichmentProvider(applRepo, "go-types", first))

	// Content moves on. The provider is now stale, but it has run.
	require.NoError(t, s.BulkSetFileMtimes(applRepo, map[string]int64{"src/main.go": 2}))
	later, err := s.RepoContentGen(applRepo)
	require.NoError(t, err)
	require.Greater(t, later, first, "the edit must have moved the content counter")

	owed, known := EnrichmentOwed(s, applRepo)
	require.True(t, known)
	require.False(t, owed, "behind is not the same as never run")
}

// No rows at all is nobody having looked — the exact residue the swallowed
// track-time writes left, and a repo that must be re-armed rather than left
// reporting enrichment as unknown forever.
func TestARepoWithNoEnrichmentRecordAtAllIsOwedAPass(t *testing.T) {
	t.Parallel()
	s := applStore(t, "go")
	require.NoError(t, s.BulkSetFileMtimes(applRepo, map[string]int64{"src/main.go": 1}))

	owed, known := EnrichmentOwed(s, applRepo)
	require.True(t, known)
	require.True(t, owed)
}

// "No provider applies" is a real answer, not an absence, and must never
// schedule a pass — a repo of Markdown would otherwise re-arm on every start.
func TestARepoWhereNothingAppliesIsOwedNothing(t *testing.T) {
	t.Parallel()
	s := applStore(t, "go")
	require.NoError(t, s.BulkSetFileMtimes(applRepo, map[string]int64{"src/main.go": 1}))
	require.NoError(t, s.DeclareEnrichmentProviders(applRepo, nil))

	owed, known := EnrichmentOwed(s, applRepo)
	require.True(t, known)
	require.False(t, owed)
}

// A repo with nothing indexed owes nothing. Without the content check a
// freshly tracked prefix would re-arm a pass over an empty graph.
func TestARepoWithNoIndexedContentIsOwedNothing(t *testing.T) {
	t.Parallel()
	s := applStore(t, "go")

	owed, known := EnrichmentOwed(s, applRepo)
	require.True(t, known)
	require.False(t, owed)
}

// A backend that models no applicability answers "I don't know", never "no".
// The caller must be able to tell the two apart: acting on a false "no" is how
// the gate went quiet in the first place.
func TestABackendThatModelsNothingReportsUnknown(t *testing.T) {
	t.Parallel()
	owed, known := EnrichmentOwed(graph.New(), applRepo)
	require.False(t, known)
	require.False(t, owed)
}
