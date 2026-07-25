package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/daemon"
)

// sampleStatus mimics a realistic StatusResponse so render functions
// can be exercised without running a live daemon.
func sampleStatus() daemon.StatusResponse {
	return daemon.StatusResponse{
		Version:       "v0.7.1",
		PID:           12345,
		SocketPath:    "/tmp/gortex.sock",
		UptimeSeconds: 180,
		Sessions:      1,
		MemoryBytes:   3_500_000_000,
		Ready:         true,
		WarmupSeconds: 42,
		TrackedRepos: []daemon.TrackedRepoStatus{
			{
				Prefix: "project1",
				Path:   "/tmp/code/project1",
				Files:  2029, Nodes: 20774, Edges: 208956,
				Memory: daemon.MemoryBreakdown{
					NodesBytes: 8_500_000, EdgesBytes: 63_000_000,
					SearchBytes: 17_000_000, VectorsBytes: 0,
					TotalBytes: 88_500_000,
				},
			},
			{
				Prefix: "project2",
				Path:   "/tmp/code/project2",
				Files:  6174, Nodes: 27578, Edges: 72190,
				Memory: daemon.MemoryBreakdown{
					NodesBytes: 12_000_000, EdgesBytes: 24_000_000,
					SearchBytes: 22_000_000, VectorsBytes: 0,
					TotalBytes: 58_000_000,
				},
			},
		},
	}
}

func TestRenderDaemonHeader_KeyFacts(t *testing.T) {
	var buf bytes.Buffer
	renderDaemonHeader(&buf, sampleStatus())
	out := buf.String()
	for _, want := range []string{"daemon", "v0.7.1", "pid", "12345", "sessions", "ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("header output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderDaemonRepos_HasTableAndOtherRow(t *testing.T) {
	var buf bytes.Buffer
	renderDaemonRepos(&buf, sampleStatus())
	out := buf.String()
	// Both repos appear, biggest-memory-first (project1 before project2).
	assert.Contains(t, out, "project1")
	assert.Contains(t, out, "project2")
	assert.Less(t, strings.Index(out, "project1"), strings.Index(out, "project2"),
		"repos should sort by memory desc")
	// "other" footer shows unattributed memory.
	assert.Contains(t, out, "other")
	assert.Contains(t, out, "embedder")
}

func TestRenderDaemonRepos_NoRepos(t *testing.T) {
	var buf bytes.Buffer
	renderDaemonRepos(&buf, daemon.StatusResponse{MemoryBytes: 100})
	assert.Contains(t, buf.String(), "tracked repos: (none)")
}

func TestRenderDaemonRepos_DiskColumnAppearsWhenDiskMode(t *testing.T) {
	st := sampleStatus()
	// Flip the biggest repo into disk mode.
	st.TrackedRepos[0].Memory.DiskBytes = 500_000_000
	var buf bytes.Buffer
	renderDaemonRepos(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "disk_b", "disk_b column must appear when any repo has DiskBytes > 0")
}

func TestRenderDaemonRepos_NoDiskColumnInMemoryMode(t *testing.T) {
	var buf bytes.Buffer
	renderDaemonRepos(&buf, sampleStatus())
	assert.NotContains(t, buf.String(), "disk_b",
		"disk_b column should be hidden when all repos are in-memory")
}

func TestRenderDaemonHeader_SearchBackendRow(t *testing.T) {
	st := sampleStatus()
	st.SearchBackend = daemon.SearchBackendStats{
		Name:          "bleve-disk",
		DocCount:      65000,
		DocCountKnown: true,
		Bytes:     200 * 1024 * 1024,
		DiskPath:  "/tmp/gortex/bleve.scorch",
		DiskBytes: 800 * 1024 * 1024,
	}
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "bleve-disk")
	assert.Contains(t, out, "65000")
	assert.Contains(t, out, "/tmp/gortex/bleve.scorch")
}

func TestRenderDaemonHeader_WarmupLabel(t *testing.T) {
	st := sampleStatus()
	st.Ready = true
	st.EnrichmentComplete = false
	st.WarmupSeconds = 203
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "warmup 3m23s", "the state row must label total time-to-queryable as warmup, not resolve")
	assert.NotContains(t, out, "resolve 203s")
	assert.NotContains(t, out, "(resolve ")
}

func TestRenderDaemonHeader_EnrichmentProgress_NoSummary(t *testing.T) {
	st := sampleStatus()
	st.Ready = true
	st.EnrichmentComplete = false
	st.Enrichment = nil
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	assert.Contains(t, buf.String(), "enrichment in progress")
}

func TestRenderDaemonHeader_EnrichmentProgress_WithCurrent(t *testing.T) {
	st := sampleStatus()
	st.Ready = true
	st.EnrichmentComplete = false
	st.Enrichment = &daemon.EnrichmentProgress{
		Running:    true,
		ReposTotal: 22,
		ReposDone:  3,
		Current: &daemon.EnrichmentCurrent{
			Repo:            "gortex",
			Provider:        "gopls",
			ElapsedSeconds:  240,
			DeadlineSeconds: 900,
		},
	}
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "enriching 3/22")
	assert.Contains(t, out, "gortex")
	assert.Contains(t, out, "4m0s/15m0s")
}

func TestRenderDaemonHeader_EnrichmentProgress_NoCurrentPass(t *testing.T) {
	st := sampleStatus()
	st.Ready = true
	st.EnrichmentComplete = false
	st.Enrichment = &daemon.EnrichmentProgress{
		Running:    false,
		ReposTotal: 5,
		ReposDone:  5,
	}
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	assert.Contains(t, buf.String(), "enriching 5/5 repos")
}

func TestRenderDaemonHeader_ReadyAndEnriched_NoWarmupLabelChange(t *testing.T) {
	st := sampleStatus()
	st.Ready = true
	st.EnrichmentComplete = true
	st.EnrichSeconds = 300
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "ready (warmup 5m0s)")
	assert.NotContains(t, out, "enrichment in progress")
}
