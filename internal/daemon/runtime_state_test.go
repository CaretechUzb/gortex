package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func withStateFile(t *testing.T) {
	t.Helper()
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(t.TempDir(), "daemon.state.json"))
}

// The scoping rule, which is the whole reason DerivingScope exists. A post-track
// derive covers only the newly-tracked sibling checkouts; without the scope,
// marking every tracked repo as deriving would hide six perfectly queryable
// repos behind a run that has nothing to do with them.
func TestDerivingScopeLimitsTheMarkerToTheCoveredRepos(t *testing.T) {
	st := RuntimeState{DerivingSince: 1700, DerivingScope: []string{"repoA", "repoC"}}

	require.True(t, st.IsDeriving("repoA"))
	require.True(t, st.IsDeriving("repoC"))
	require.False(t, st.IsDeriving("repoB"), "an out-of-scope repo must show its real verdict")
}

// An in-flight run with no recorded scope is the historical whole-workspace
// shape, not a missing value — so it covers everything.
func TestAnUnscopedDeriveCoversEveryRepo(t *testing.T) {
	st := RuntimeState{DerivingSince: 1700}
	require.True(t, st.IsDeriving("anything"))

	require.False(t, RuntimeState{}.IsDeriving("anything"),
		"no run in flight is not a whole-workspace run")
	require.False(t, RuntimeState{DerivingScope: []string{"repoA"}}.IsDeriving("repoA"),
		"a scope with no start time is a stale field, not a live run")
}

// Enrichment has no whole-workspace form: it is always per-repo, so an absent
// list means nothing is enriching rather than everything is.
func TestIsEnrichingIsAlwaysPerRepo(t *testing.T) {
	st := RuntimeState{EnrichingRepos: []string{"repoA"}}
	require.True(t, st.IsEnriching("repoA"))
	require.False(t, st.IsEnriching("repoB"))
	require.False(t, RuntimeState{}.IsEnriching("repoA"))
}

// The markers make the record mutable mid-run, so an update must carry forward
// every field it does not touch — losing BackendPath would send `gortex repos`
// to the wrong store, which is the record's original and load-bearing job.
func TestUpdateRuntimeStatePreservesFieldsItDoesNotTouch(t *testing.T) {
	withStateFile(t)
	require.NoError(t, WriteRuntimeState(RuntimeState{BackendPath: "/tmp/graph.sqlite"}))

	require.NoError(t, UpdateRuntimeState(func(st *RuntimeState) {
		st.DerivingSince = 1700
		st.DerivingScope = []string{"repoA"}
	}))
	require.NoError(t, UpdateRuntimeState(func(st *RuntimeState) {
		st.EnrichingRepos = []string{"repoB"}
	}))

	st, ok := ReadRuntimeState()
	require.True(t, ok)
	require.Equal(t, "/tmp/graph.sqlite", st.BackendPath)
	require.Equal(t, int64(1700), st.DerivingSince)
	require.Equal(t, []string{"repoA"}, st.DerivingScope)
	require.Equal(t, []string{"repoB"}, st.EnrichingRepos)

	// Closing the run clears both derive fields and leaves the rest alone.
	require.NoError(t, UpdateRuntimeState(func(st *RuntimeState) {
		st.DerivingSince = 0
		st.DerivingScope = nil
	}))
	st, ok = ReadRuntimeState()
	require.True(t, ok)
	require.False(t, st.IsDeriving("repoA"))
	require.True(t, st.IsEnriching("repoB"))
	require.Equal(t, "/tmp/graph.sqlite", st.BackendPath)
}

// Crash safety is free, and this is what buys it: a daemon killed mid-derive
// leaves DerivingSince set forever, and the liveness gate discards the whole
// record rather than letting a reader believe a run that ended with the
// process.
func TestADeadDaemonsMarkersAreNeverBelieved(t *testing.T) {
	withStateFile(t)
	require.NoError(t, WriteRuntimeState(RuntimeState{
		BackendPath:    "/tmp/graph.sqlite",
		DerivingSince:  1700,
		EnrichingRepos: []string{"repoA"},
	}))

	// Re-stamp with a PID that cannot be running. WriteRuntimeState always
	// stamps the caller's own PID, so this has to go through the raw file.
	blob, err := json.Marshal(RuntimeState{
		PID:            -1,
		BackendPath:    "/tmp/graph.sqlite",
		DerivingSince:  1700,
		EnrichingRepos: []string{"repoA"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(RuntimeStatePath(), blob, 0o600))

	_, ok := ReadRuntimeState()
	require.False(t, ok, "a killed daemon's record must be discarded wholesale")
}
