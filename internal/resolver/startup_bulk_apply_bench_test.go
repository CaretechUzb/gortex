package resolver

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const startupBulkApplyBenchmarkEdges = 32 * 1024

// BenchmarkResolveAllStartupBulkApply isolates the compute/apply barrier cost
// on the production SQLite backend. Fixture construction is outside the timer;
// both variants resolve the exact same edge corpus and execute the same tail.
func BenchmarkResolveAllStartupBulkApply(b *testing.B) {
	b.Setenv("GORTEX_BACKEND_RESOLVER", "0")
	b.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	b.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "")

	for _, startup := range []bool{false, true} {
		name := "ordinary_2048"
		if startup {
			name = "startup_page"
		}
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				storePath := filepath.Join(b.TempDir(), fmt.Sprintf("graph-%d.sqlite", i))
				store, err := store_sqlite.Open(storePath)
				if err != nil {
					b.Fatal(err)
				}
				source := &graph.Node{
					ID:         "repo/src.go::caller",
					Kind:       graph.KindFunction,
					Name:       "caller",
					FilePath:   "repo/src.go",
					Language:   "go",
					RepoPrefix: "repo",
				}
				target := &graph.Node{
					ID:         "repo/src.go::foo",
					Kind:       graph.KindFunction,
					Name:       "foo",
					FilePath:   "repo/src.go",
					Language:   "go",
					RepoPrefix: "repo",
				}
				edges := make([]*graph.Edge, startupBulkApplyBenchmarkEdges)
				for j := range edges {
					edges[j] = &graph.Edge{
						From:     source.ID,
						To:       "unresolved::foo",
						Kind:     graph.EdgeCalls,
						FilePath: source.FilePath,
						Line:     j + 1,
						Origin:   "ast",
					}
				}
				store.AddBatch([]*graph.Node{source, target}, edges)
				resolver := New(store)
				resolver.SetStartupBulkApply(startup)

				b.StartTimer()
				stats := resolver.ResolveAll()
				b.StopTimer()

				if stats.Resolved != startupBulkApplyBenchmarkEdges {
					b.Fatalf("resolved = %d, want %d", stats.Resolved, startupBulkApplyBenchmarkEdges)
				}
				if edge := store.GetOutEdges(source.ID); len(edge) != startupBulkApplyBenchmarkEdges {
					b.Fatalf("out edges = %d, want %d", len(edge), startupBulkApplyBenchmarkEdges)
				} else if edge[0].To != target.ID {
					b.Fatalf("first target = %q, want %q", edge[0].To, target.ID)
				}
				if err := store.Close(); err != nil {
					b.Fatal(fmt.Errorf("close benchmark store: %w", err))
				}
			}
		})
	}
}
