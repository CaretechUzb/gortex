package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/contracts"
)

func TestTakeWarmupSnapshotPayloadsMovesAndClearsState(t *testing.T) {
	state := &daemonState{
		snapshotRepos: map[string]*snapshotRepo{
			"repo": {RepoPrefix: "repo", FileMtimes: map[string]int64{"a.go": 1}},
		},
		snapshotContracts: map[string][]contracts.Contract{
			"repo": {{ID: "contract"}},
		},
		snapshotVector: snapshotVector{Index: []byte{1, 2, 3}, Dims: 8, Count: 1},
	}

	repos, contractEntries, vector := state.takeWarmupSnapshotPayloads()

	require.Contains(t, repos, "repo")
	assert.Equal(t, int64(1), repos["repo"].FileMtimes["a.go"])
	require.Len(t, contractEntries["repo"], 1)
	assert.Equal(t, "contract", contractEntries["repo"][0].ID)
	assert.Equal(t, []byte{1, 2, 3}, vector.Index)
	assert.Equal(t, 8, vector.Dims)
	assert.Equal(t, 1, vector.Count)

	assert.Nil(t, state.snapshotRepos)
	assert.Nil(t, state.snapshotContracts)
	assert.Empty(t, state.snapshotVector.Index)
	assert.Zero(t, state.snapshotVector.Dims)
	assert.Zero(t, state.snapshotVector.Count)

	repos, contractEntries, vector = state.takeWarmupSnapshotPayloads()
	assert.Nil(t, repos)
	assert.Nil(t, contractEntries)
	assert.Empty(t, vector.Index)
}

func TestTakeWarmupSnapshotPayloadsHandlesNilState(t *testing.T) {
	var state *daemonState
	repos, contractEntries, vector := state.takeWarmupSnapshotPayloads()
	assert.Nil(t, repos)
	assert.Nil(t, contractEntries)
	assert.Empty(t, vector.Index)
}

func TestWarmupClearsSnapshotPayloadsBeforeDependencyGuard(t *testing.T) {
	state := &daemonState{
		snapshotRepos: map[string]*snapshotRepo{"repo": {RepoPrefix: "repo"}},
		snapshotContracts: map[string][]contracts.Contract{
			"repo": {{ID: "contract"}},
		},
		snapshotVector: snapshotVector{Index: []byte{1}, Dims: 1, Count: 1},
	}

	watcher, timings := warmupDaemonState(state, zap.NewNop(), nil)

	assert.Nil(t, watcher)
	require.NotNil(t, timings)
	assert.Nil(t, state.snapshotRepos)
	assert.Nil(t, state.snapshotContracts)
	assert.Empty(t, state.snapshotVector.Index)
}
