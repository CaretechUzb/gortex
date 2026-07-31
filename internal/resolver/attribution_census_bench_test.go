package resolver

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const attributionCensusBenchmarkEdges = 100_000

// BenchmarkPostBareAttributionCensusSQLite100K compares the pre-census tail
// (six separate corpus reads, including full metadata hydration) with the
// shared light projection plus bounded exact hydration. The candidate density
// is about 3.4%, close to the measured production corpus, and every edge carries
// a payload that the light projection deliberately avoids decoding.
func BenchmarkPostBareAttributionCensusSQLite100K(b *testing.B) {
	for _, benchmark := range []struct {
		name string
		run  func(*testing.B, *Resolver)
	}{
		{
			name: "LegacyFullScans",
			run: func(_ *testing.B, resolver *Resolver) {
				resolver.bindDataflowCalleeRefs()
				resolver.bindGenericParamRefs()
			},
		},
		{
			name: "SharedLightCensus",
			run: func(b *testing.B, resolver *Resolver) {
				census, stats := resolver.buildPostBareAttributionCensus(defaultAttributionCensusLimits)
				if census == nil {
					b.Fatalf("unexpected census fallback: %s", stats.fallbackReason)
				}
				resolver.bindDataflowCalleeRefsFromCensus(census)
				census.releaseDataflow()
				resolver.bindGenericParamRefsFromCensus(census)
				census.close()
			},
		},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			store := buildAttributionCensusSQLiteBenchmarkStore(b)
			b.Cleanup(func() { _ = store.Close() })
			resolver := New(store)
			b.ReportAllocs()
			b.ReportMetric(attributionCensusBenchmarkEdges, "edges/op")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchmark.run(b, resolver)
			}
		})
	}
}

func buildAttributionCensusSQLiteBenchmarkStore(b *testing.B) *store_sqlite.Store {
	b.Helper()
	store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "attribution-census.sqlite"))
	if err != nil {
		b.Fatal(err)
	}

	owner := &graph.Node{
		ID: "repo::Owner", Kind: graph.KindFunction, Name: "Owner",
		FilePath: "pkg/owner.go", Language: "go", RepoPrefix: "repo",
	}
	typeParam := &graph.Node{
		ID: "repo::Owner#tparam:T", Kind: graph.KindGenericParam, Name: "T",
		FilePath: owner.FilePath, Language: "go", RepoPrefix: "repo",
	}
	store.AddBatch([]*graph.Node{owner, typeParam}, nil)

	kinds := []graph.EdgeKind{
		graph.EdgeCalls,
		graph.EdgeReferences,
		graph.EdgeArgOf,
		graph.EdgeValueFlow,
		graph.EdgeTypedAs,
		graph.EdgeReturns,
		graph.EdgeInstantiates,
	}
	payload := strings.Repeat("m", 512)
	const batchSize = 5_000
	for start := 0; start < attributionCensusBenchmarkEdges; start += batchSize {
		end := min(start+batchSize, attributionCensusBenchmarkEdges)
		edges := make([]*graph.Edge, 0, end-start)
		for i := start; i < end; i++ {
			kind := kinds[i%len(kinds)]
			to := "repo::resolved-target"
			if i%25 == 0 {
				switch kind {
				case graph.EdgeArgOf, graph.EdgeValueFlow:
					to = graph.UnresolvedMarker + "MissingCallee"
				case graph.EdgeReferences, graph.EdgeTypedAs, graph.EdgeReturns, graph.EdgeInstantiates:
					to = graph.UnresolvedMarker + "MissingTypeParam"
				}
			}
			edges = append(edges, &graph.Edge{
				From:     fmt.Sprintf("repo::Other#local:%06d", i),
				To:       to,
				Kind:     kind,
				FilePath: fmt.Sprintf("pkg/corpus-%03d.go", i%100),
				Line:     i + 1,
				Meta:     map[string]any{"payload": payload, "sequence": i},
			})
		}
		store.AddBatch(nil, edges)
	}
	return store
}
