package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/semantic"
)

func TestEnrichmentProgressFromStatuses_Nil(t *testing.T) {
	assert.Nil(t, enrichmentProgressFromStatuses(nil))
	assert.Nil(t, enrichmentProgressFromStatuses([]semantic.EnrichmentStatus{}))
}

func TestEnrichmentProgressFromStatuses_AllDone(t *testing.T) {
	statuses := []semantic.EnrichmentStatus{
		{Repo: "repo-a", Provider: "gopls", State: semantic.EnrichStateCompleted},
		{Repo: "repo-b", Provider: "gopls", State: semantic.EnrichStatePartial},
		{Repo: "repo-c", Provider: "gopls", State: semantic.EnrichStateAbandoned},
	}
	out := enrichmentProgressFromStatuses(statuses)
	require.NotNil(t, out)
	assert.False(t, out.Running)
	assert.Nil(t, out.Current)
	assert.Equal(t, 3, out.ReposTotal)
	assert.Equal(t, 3, out.ReposDone, "completed/partial/abandoned are all terminal states")
}

func TestEnrichmentProgressFromStatuses_OneRunning(t *testing.T) {
	start := time.Now().Add(-4 * time.Minute)
	statuses := []semantic.EnrichmentStatus{
		{Repo: "repo-a", Provider: "gopls", State: semantic.EnrichStateCompleted},
		{
			Repo: "repo-b", Provider: "gopls", State: semantic.EnrichStateRunning,
			StartedAt: start, DeadlineSeconds: 900,
		},
	}
	out := enrichmentProgressFromStatuses(statuses)
	require.NotNil(t, out)
	assert.True(t, out.Running)
	assert.Equal(t, 2, out.ReposTotal)
	assert.Equal(t, 1, out.ReposDone)
	require.NotNil(t, out.Current)
	assert.Equal(t, "repo-b", out.Current.Repo)
	assert.Equal(t, "gopls", out.Current.Provider)
	assert.Equal(t, 900.0, out.Current.DeadlineSeconds)
	assert.InDelta(t, 240, out.Current.ElapsedSeconds, 5)
}

func TestEnrichmentProgressFromStatuses_RepoWithMixedProviders(t *testing.T) {
	// repo-a has one completed and one still-running provider — the repo
	// as a whole must not count as done until every provider finishes.
	statuses := []semantic.EnrichmentStatus{
		{Repo: "repo-a", Provider: "gopls", State: semantic.EnrichStateCompleted},
		{Repo: "repo-a", Provider: "lsp-jdtls", State: semantic.EnrichStateRunning, DeadlineSeconds: 60},
	}
	out := enrichmentProgressFromStatuses(statuses)
	require.NotNil(t, out)
	assert.Equal(t, 1, out.ReposTotal)
	assert.Equal(t, 0, out.ReposDone)
	assert.True(t, out.Running)
}

// TestStatusResponse_EnrichmentJSONRoundTrip locks in that the new
// Enrichment block survives a JSON marshal/unmarshal round trip over
// the daemon control socket, and that it's omitted entirely (not
// rendered as a null block) when nil.
func TestStatusResponse_EnrichmentJSONRoundTrip(t *testing.T) {
	st := daemon.StatusResponse{
		Version: "v1.2.3",
		Ready:   true,
		Enrichment: &daemon.EnrichmentProgress{
			Running:    true,
			ReposTotal: 22,
			ReposDone:  3,
			Current: &daemon.EnrichmentCurrent{
				Repo:            "gortex",
				Provider:        "gopls",
				ElapsedSeconds:  240,
				DeadlineSeconds: 900,
			},
		},
	}

	raw, err := json.Marshal(st)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"enrichment":`)
	assert.Contains(t, string(raw), `"repos_total":22`)
	assert.Contains(t, string(raw), `"repos_done":3`)

	var round daemon.StatusResponse
	require.NoError(t, json.Unmarshal(raw, &round))
	require.NotNil(t, round.Enrichment)
	assert.Equal(t, st.Enrichment.ReposTotal, round.Enrichment.ReposTotal)
	assert.Equal(t, st.Enrichment.ReposDone, round.Enrichment.ReposDone)
	require.NotNil(t, round.Enrichment.Current)
	assert.Equal(t, "gortex", round.Enrichment.Current.Repo)
	assert.Equal(t, 900.0, round.Enrichment.Current.DeadlineSeconds)

	var empty daemon.StatusResponse
	rawEmpty, err := json.Marshal(empty)
	require.NoError(t, err)
	assert.NotContains(t, string(rawEmpty), `"enrichment"`, "Enrichment must be omitted (omitempty) when nil")
}

// TestSearchBackendStats_DiskResidentJSONRoundTrip locks in that the
// DiskResident flag survives the wire round trip and is omitted when
// false (only the store-native FTS index sets it; a backend reporting a
// heap figure leaves it clear).
func TestSearchBackendStats_DiskResidentJSONRoundTrip(t *testing.T) {
	sb := daemon.SearchBackendStats{Name: "sqlite-fts5", DocCount: 48572, DiskResident: true}
	raw, err := json.Marshal(sb)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"disk_resident":true`)

	var round daemon.SearchBackendStats
	require.NoError(t, json.Unmarshal(raw, &round))
	assert.True(t, round.DiskResident)

	heapResident := daemon.SearchBackendStats{Name: "unknown", DocCount: 10}
	raw2, err := json.Marshal(heapResident)
	require.NoError(t, err)
	assert.NotContains(t, string(raw2), "disk_resident")
}
