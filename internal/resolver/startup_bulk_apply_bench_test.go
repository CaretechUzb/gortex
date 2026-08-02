package resolver

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const resolveApplyBenchmarkEdges = 32 * 1024

// BenchmarkResolveAllBoundedApply measures the ordinary 2,048-edge compute and
// apply slices on the production SQLite backend. Fixture construction and
// correctness verification stay outside the timer.
func BenchmarkResolveAllBoundedApply(b *testing.B) {
	b.Setenv("GORTEX_BACKEND_RESOLVER", "0")
	b.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	b.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "")

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
		edges := make([]*graph.Edge, resolveApplyBenchmarkEdges)
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

		b.StartTimer()
		stats := New(store).ResolveAll()
		b.StopTimer()

		if stats.Resolved != resolveApplyBenchmarkEdges {
			b.Fatalf("resolved = %d, want %d", stats.Resolved, resolveApplyBenchmarkEdges)
		}
		out := store.GetOutEdges(source.ID)
		if len(out) != resolveApplyBenchmarkEdges {
			b.Fatalf("out edges = %d, want %d", len(out), resolveApplyBenchmarkEdges)
		}
		for j, edge := range out {
			if edge.To != target.ID {
				b.Fatalf("edge %d target = %q, want %q", j, edge.To, target.ID)
			}
		}
		if err := store.Close(); err != nil {
			b.Fatal(fmt.Errorf("close benchmark store: %w", err))
		}
	}
}
