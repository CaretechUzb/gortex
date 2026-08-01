package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type frameworkExecutionCountingStore struct {
	graph.Store
	getRepoNodes, getRepoEdges           int
	repoEdgesByKinds, repoNodeIDsByKinds int
	allNodesLight, repoNodesLight        int
}

func (s *frameworkExecutionCountingStore) GetRepoNodes(repoPrefix string) []*graph.Node {
	s.getRepoNodes++
	return s.Store.GetRepoNodes(repoPrefix)
}

func (s *frameworkExecutionCountingStore) GetRepoEdges(repoPrefix string) []*graph.Edge {
	s.getRepoEdges++
	return s.Store.GetRepoEdges(repoPrefix)
}

func (s *frameworkExecutionCountingStore) RepoEdgesByKinds(prefixes []string, kinds []graph.EdgeKind) []graph.RepoEdgeRow {
	s.repoEdgesByKinds++
	return s.Store.(graph.RepoEdgeKindReader).RepoEdgesByKinds(prefixes, kinds)
}

func (s *frameworkExecutionCountingStore) RepoNodeIDsByKinds(prefixes []string, kinds []graph.NodeKind) []string {
	s.repoNodeIDsByKinds++
	return s.Store.(graph.RepoNodeKindIDReader).RepoNodeIDsByKinds(prefixes, kinds)
}

func (s *frameworkExecutionCountingStore) AllNodesLight() []*graph.Node {
	s.allNodesLight++
	return graph.AllNodesLight(s.Store)
}

func (s *frameworkExecutionCountingStore) RepoNodesLight(prefixes []string) []*graph.Node {
	s.repoNodesLight++
	return graph.ReadRepoNodesLight(s.Store, prefixes)
}

func frameworkReportEdgeCounts(report FrameworkSynthReport) map[string]int {
	counts := make(map[string]int, len(report.Per))
	for _, row := range report.Per {
		counts[row.Name] = row.Edges
	}
	return counts
}

func addFrameworkExecutionRepos(g *graph.Graph) map[string][2]string {
	families := make(map[string][2]string, 2)
	for _, repo := range []string{"first", "second"} {
		caller, _, implementation := addCSharpDispatchRepo(g, repo)
		families[repo] = [2]string{caller, implementation}
	}
	return families
}

func TestFrameworkFullCoverageAttestationUsesWholeStoreExecution(t *testing.T) {
	baseline := graph.New()
	baselineFamilies := addFrameworkExecutionRepos(baseline)
	baselineStore := &frameworkExecutionCountingStore{Store: baseline}
	baselineReport := RunFrameworkSynthesizers(baselineStore)

	attested := graph.New()
	attestedFamilies := addFrameworkExecutionRepos(attested)
	attestedStore := &frameworkExecutionCountingStore{Store: attested}
	attestedReport := RunFrameworkSynthesizersScopedWithCensus(
		attestedStore,
		map[string]bool{"first": true, "second": true},
		true,
	)

	require.Equal(t, baselineReport.Total, attestedReport.Total)
	require.Equal(t, baselineReport.Gated, attestedReport.Gated)
	require.Equal(t, baselineReport.ReceiverGated, attestedReport.ReceiverGated)
	require.Equal(t, frameworkReportEdgeCounts(baselineReport), frameworkReportEdgeCounts(attestedReport))
	for repo, family := range baselineFamilies {
		require.Equal(t, 1, ifaceDispatchCount(baseline, family[0], family[1]), repo)
	}
	for repo, family := range attestedFamilies {
		require.Equal(t, 1, ifaceDispatchCount(attested, family[0], family[1]), repo)
	}

	require.Positive(t, attestedStore.allNodesLight, "full census reads the whole-store light projection")
	require.Zero(t, attestedStore.repoNodesLight)
	require.Zero(t, attestedStore.getRepoNodes)
	require.Zero(t, attestedStore.getRepoEdges)
	require.Zero(t, attestedStore.repoEdgesByKinds)
	require.Zero(t, attestedStore.repoNodeIDsByKinds)
}

func TestFrameworkPartialRunKeepsRepositoryExecution(t *testing.T) {
	g := graph.New()
	families := addFrameworkExecutionRepos(g)
	counting := &frameworkExecutionCountingStore{Store: g}

	report := RunFrameworkSynthesizersScoped(counting, map[string]bool{"first": true})

	require.Equal(t, 1, frameworkReportEdgeCounts(report)[SynthCSharpIfaceDispatch])
	require.Equal(t, 1, ifaceDispatchCount(g, families["first"][0], families["first"][1]))
	require.Zero(t, ifaceDispatchCount(g, families["second"][0], families["second"][1]))
	require.Zero(t, counting.allNodesLight)
	require.Positive(t, counting.repoNodesLight, "partial census remains repository-scoped")
	require.Positive(t, counting.getRepoEdges, "bespoke partial passes retain repository scans")
	require.Positive(t, counting.repoEdgesByKinds, "partial edge projections retain their repository frontier")
}
