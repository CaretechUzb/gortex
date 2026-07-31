package store_sqlite

import (
	"bytes"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

const (
	resolverProjectionBenchRows = 2048
	resolverProjectionBenchMeta = 32 << 10
)

var resolverProjectionBenchSink uint64

func openResolverProjectionBenchStore(b *testing.B, kind graph.NodeKind) *Store {
	b.Helper()
	store, err := Open(filepath.Join(b.TempDir(), "graph.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })

	payload := bytes.Repeat([]byte{0xa5}, resolverProjectionBenchMeta)
	nodes := make([]*graph.Node, 0, resolverProjectionBenchRows)
	for i := 0; i < resolverProjectionBenchRows; i++ {
		file := fmt.Sprintf("bench/file-%06d.go", i)
		id, name := file, filepath.Base(file)
		if kind == graph.KindLocal {
			id, name = file+"::local", "local"
		}
		nodes = append(nodes, &graph.Node{
			ID: id, Kind: kind, Name: name, FilePath: file,
			RepoPrefix: "bench", WorkspaceID: "bench-workspace", StartLine: i + 1,
			Meta: map[string]any{"bench_payload": payload},
		})
	}
	store.AddBatch(nodes, nil)
	runtime.GC()
	return store
}

func BenchmarkResolverNodeProjection(b *testing.B) {
	b.Run("ScopeBinding/FullNodesByKind", func(b *testing.B) {
		store := openResolverProjectionBenchStore(b, graph.KindLocal)
		b.ReportAllocs()
		b.ResetTimer()
		b.ReportMetric(resolverProjectionBenchRows, "rows/op")
		var sink uint64
		for i := 0; i < b.N; i++ {
			for node := range store.NodesByKind(graph.KindLocal) {
				sink += uint64(len(node.ID) + len(node.Name) + node.StartLine + len(node.Meta))
			}
		}
		resolverProjectionBenchSink = sink
	})
	b.Run("ScopeBinding/Projected", func(b *testing.B) {
		store := openResolverProjectionBenchStore(b, graph.KindLocal)
		b.ReportAllocs()
		b.ResetTimer()
		b.ReportMetric(resolverProjectionBenchRows, "rows/op")
		var sink uint64
		for i := 0; i < b.N; i++ {
			for node := range store.ScopeBindingNodesSeq(graph.KindLocal) {
				sink += uint64(len(node.ID) + len(node.Name) + node.StartLine)
			}
		}
		resolverProjectionBenchSink = sink
	})
	b.Run("FileIdentity/FullNodesByKind", func(b *testing.B) {
		store := openResolverProjectionBenchStore(b, graph.KindFile)
		b.ReportAllocs()
		b.ResetTimer()
		b.ReportMetric(resolverProjectionBenchRows, "rows/op")
		var sink uint64
		for i := 0; i < b.N; i++ {
			for node := range store.NodesByKind(graph.KindFile) {
				sink += uint64(len(node.ID) + len(node.FilePath) + len(node.RepoPrefix) + len(node.WorkspaceID) + len(node.Meta))
			}
		}
		resolverProjectionBenchSink = sink
	})
	b.Run("FileIdentity/Projected", func(b *testing.B) {
		store := openResolverProjectionBenchStore(b, graph.KindFile)
		b.ReportAllocs()
		b.ResetTimer()
		b.ReportMetric(resolverProjectionBenchRows, "rows/op")
		var sink uint64
		for i := 0; i < b.N; i++ {
			for node := range store.FileNodeIdentitiesSeq(nil) {
				sink += uint64(len(node.ID) + len(node.FilePath) + len(node.RepoPrefix) + len(node.WorkspaceID))
			}
		}
		resolverProjectionBenchSink = sink
	})
}
