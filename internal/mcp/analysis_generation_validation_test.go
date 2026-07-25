package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// The store validates a generation as it writes it and rejects the whole
// thing on the first violation — a rejection that only ever surfaced as a
// warn, leaving the daemon to fall back to holding the analysis in memory
// and recomputing it on every restart. These tests pin the three producer
// invariants the store enforces.

func testAnalysisCandidate(t *testing.T) persistedAnalysis {
	t.Helper()
	g := graph.New()
	nodes := []*graph.Node{
		{ID: "pkg/a.go::Alpha", Kind: graph.KindFunction, Name: "Alpha", FilePath: "pkg/a.go", Language: "go"},
		{ID: "pkg/b.go::Beta", Kind: graph.KindFunction, Name: "Beta", FilePath: "pkg/b.go", Language: "go"},
		{ID: "pkg/c.go::Gamma", Kind: graph.KindFunction, Name: "Gamma", FilePath: "pkg/c.go", Language: "go"},
	}
	edges := []*graph.Edge{
		{From: "pkg/a.go::Alpha", To: "pkg/b.go::Beta", Kind: graph.EdgeCalls},
		{From: "pkg/b.go::Beta", To: "pkg/c.go::Gamma", Kind: graph.EdgeCalls},
	}
	g.AddBatch(nodes, edges)

	communities, leiden, _ := analysis.DetectCommunitiesLeidenIncremental(g, nil)
	candidate := persistedAnalysis{
		communities:  communities,
		leiden:       leiden,
		processes:    analysis.DiscoverProcesses(g),
		pageRank:     analysis.ComputePageRank(g),
		adjacency:    analysis.BuildAdjacencySnapshot(g),
		autoConcepts: search.BuildAutoConcepts(g),
		hits:         analysis.ComputeHITS(g),
	}
	require.NotNil(t, candidate.communities)
	require.NotNil(t, candidate.adjacency)
	require.NotEmpty(t, candidate.communities.Communities, "need at least one community to mutate")
	return candidate
}

func TestSanitizeAnalysisFiles(t *testing.T) {
	// Nodes without a file (synthetic / external / stub) contribute an
	// empty path, and two nodes routinely share one file.
	got := sanitizeAnalysisFiles([]string{"z.go", "", "a.go", "z.go", ""})
	require.Equal(t, []string{"a.go", "z.go"}, got)

	require.Nil(t, sanitizeAnalysisFiles(nil))
	require.Nil(t, sanitizeAnalysisFiles([]string{}))
	require.Empty(t, sanitizeAnalysisFiles([]string{"", ""}), "an all-empty list must not yield an empty-string row")
}

func TestPrepareAnalysisGenerationDropsEmptyAndDuplicateCommunityFiles(t *testing.T) {
	candidate := testAnalysisCandidate(t)
	candidate.communities.Communities[0].Files = []string{"pkg/a.go", "", "pkg/a.go"}

	input, err := prepareAnalysisGeneration(candidate, 1)
	require.NoError(t, err)

	for _, community := range input.communities {
		for _, file := range community.Files {
			require.NotEmpty(t, file, "community %q kept an empty file path", community.ID)
		}
		seen := map[string]struct{}{}
		for _, file := range community.Files {
			_, dup := seen[file]
			require.False(t, dup, "community %q repeats file %q", community.ID, file)
			seen[file] = struct{}{}
		}
	}
}

func TestPrepareAnalysisGenerationDropsStepsForNodesNotInGeneration(t *testing.T) {
	candidate := testAnalysisCandidate(t)
	known := candidate.adjacency.PersistenceSnapshot().IDs
	require.NotEmpty(t, known)

	// BuildAdjacencySnapshot skips unresolved / dangling endpoints, so a
	// process step naming one has no analysis_nodes row to join against.
	candidate.processes.Processes = []analysis.Process{{
		ID:         "process-0",
		Name:       "test",
		EntryPoint: known[0],
		Steps: []analysis.Step{
			{ID: known[0], Depth: 0},
			{ID: "unresolved::ACCEPT_TOKEN", Depth: 1},
			{ID: known[len(known)-1], Depth: 1},
		},
		StepCount: 3,
		Files:     []string{"pkg/a.go"},
	}}

	input, err := prepareAnalysisGeneration(candidate, 1)
	require.NoError(t, err)
	require.Len(t, input.processes, 1)

	steps := input.processSteps["process-0"]
	for _, step := range steps {
		require.NotEqual(t, "unresolved::ACCEPT_TOKEN", step.NodeID,
			"a step naming a node absent from the generation must be dropped")
	}

	// The store validates analysis_processes.step_count against the number
	// of persisted step rows, so the summary must count survivors only.
	require.Equal(t, len(steps), input.processes[0].StepCount)

	// Ordinals form the step primary key and drive keyset paging, so they
	// must stay contiguous after filtering.
	for i, step := range steps {
		require.Equal(t, i, step.Ordinal)
		require.Equal(t, "process-0", step.ProcessID)
	}
}
