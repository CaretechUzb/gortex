package resolver

import (
	"fmt"
	"iter"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestResolveFnValueCallbacksCallableProjectionParity(t *testing.T) {
	projected := openFnValueProjectionSQLite(t, "projected")
	legacyStore := openFnValueProjectionSQLite(t, "legacy")
	legacy := &fnValueNoCallableProjection{Store: legacyStore}
	populateFnValueProjectionFixture(projected, 12, "fixture-metadata")
	populateFnValueProjectionFixture(legacy, 12, "fixture-metadata")

	require.Equal(t, ResolveFnValueCallbacks(legacy), ResolveFnValueCallbacks(projected))
	require.Equal(t, fnValueProjectionSnapshot(legacy), fnValueProjectionSnapshot(projected))
}

func TestResolveFnValueCallbacksClosesCallableProjectionBeforeFallbackAndWrite(t *testing.T) {
	base := graph.New()
	populateFnValueProjectionFixture(base, 4, "fixture-metadata")
	store := &fnValueProjectionGuard{Store: base}

	require.Equal(t, 3, ResolveFnValueCallbacks(store))
	require.False(t, store.cursorOpen)
	require.Empty(t, store.violations)
	require.Equal(t, []string{"projection-start", "projection-end", "file-refetch", "write"}, store.events)
}

func BenchmarkFnValueSameFileTargetLookupSQLite(b *testing.B) {
	for _, benchmark := range []struct {
		name       string
		projection bool
	}{
		{name: "legacy-full-file-nodes", projection: false},
		{name: "projected-callables", projection: true},
	} {
		benchmark := benchmark
		b.Run(benchmark.name, func(b *testing.B) {
			store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "fn-value.sqlite"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, store.Close()) })
			candidates := populateFnValueLookupBenchmark(store, 24, 400, strings.Repeat("x", 1024))
			var lookupStore graph.Store = store
			if !benchmark.projection {
				lookupStore = &fnValueNoCallableProjection{Store: store}
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				lookup := newFnValueSameFileTargetLookup(lookupStore, candidates)
				matched := 0
				for _, edge := range candidates {
					name, _ := edge.Meta[metaFnValueName].(string)
					if lookup(edge.FilePath, name) != "" {
						matched++
					}
				}
				if matched != len(candidates) {
					b.Fatalf("matched %d candidates, want %d", matched, len(candidates))
				}
			}
		})
	}
}

func openFnValueProjectionSQLite(t *testing.T, name string) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), name+".sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func populateFnValueProjectionFixture(store graph.Store, noiseNodes int, payload string) {
	const (
		file      = "src/handlers.go"
		caller    = "src/handlers.go::caller"
		unique    = "src/handlers.go::handle"
		duplicate = "src/handlers.go::dup_a"
		fallback  = "src/types.go::fallback"
	)
	nodes := []*graph.Node{
		{ID: caller, Kind: graph.KindFunction, Name: "caller", FilePath: file, Language: "go"},
		{ID: unique, Kind: graph.KindFunction, Name: "handle", FilePath: file, Language: "go", Meta: map[string]any{"payload": payload}},
		{ID: duplicate, Kind: graph.KindFunction, Name: "duplicate", FilePath: file, Language: "go", Meta: map[string]any{"payload": payload}},
		{ID: "src/handlers.go::dup_b", Kind: graph.KindMethod, Name: "duplicate", FilePath: file, Language: "go", Meta: map[string]any{"payload": payload}},
		{ID: "src/types.go::Widget.Run", Kind: graph.KindMethod, Name: "Run", FilePath: "src/types.go", Language: "go", Meta: map[string]any{"receiver": "Widget"}},
		{ID: fallback, Kind: graph.KindFunction, Name: "fallback", FilePath: "src/types.go", Language: "go", Meta: map[string]any{"payload": payload}},
	}
	for i := 0; i < noiseNodes; i++ {
		nodes = append(nodes, &graph.Node{
			ID: fmt.Sprintf("src/handlers.go::noise_%d", i), Kind: graph.KindFunction,
			Name: fmt.Sprintf("noise_%d", i), FilePath: file, Language: "go",
			Meta: map[string]any{"payload": payload},
		})
	}
	edges := []*graph.Edge{
		fnValueCandidateEdge(caller, "handle", file, 10),
		fnValueCandidateEdge(caller, "duplicate", file, 11),
		specialFnValueEdge("src/types.go::Widget.Run", "fallback", "src/types.go", 12, "<self>"),
		fnValueCandidateEdge(caller, "missing", file, 13),
	}
	store.AddBatch(nodes, edges)
}

func populateFnValueLookupBenchmark(store graph.Store, fileCount, nodesPerFile int, payload string) []*graph.Edge {
	const chunkSize = 1000
	var batch []*graph.Node
	var candidates []*graph.Edge
	flush := func() {
		if len(batch) > 0 {
			store.AddBatch(batch, nil)
			batch = nil
		}
	}
	for fileIndex := 0; fileIndex < fileCount; fileIndex++ {
		file := fmt.Sprintf("generated/file_%02d.go", fileIndex)
		handler := fmt.Sprintf("handler_%02d", fileIndex)
		for nodeIndex := 0; nodeIndex < nodesPerFile; nodeIndex++ {
			name := fmt.Sprintf("noise_%02d_%04d", fileIndex, nodeIndex)
			if nodeIndex == nodesPerFile/2 {
				name = handler
			}
			batch = append(batch, &graph.Node{
				ID: fmt.Sprintf("%s::%s", file, name), Kind: graph.KindFunction,
				Name: name, FilePath: file, Language: "go",
				Meta: map[string]any{"payload": payload, "ordinal": nodeIndex},
			})
			if len(batch) == chunkSize {
				flush()
			}
		}
		candidates = append(candidates, fnValueCandidateEdge("caller", handler, file, fileIndex+1))
	}
	flush()
	return candidates
}

type fnValueNoCallableProjection struct {
	graph.Store
}

func (store *fnValueNoCallableProjection) FnValuePlaceholderEdges() iter.Seq[*graph.Edge] {
	if scanner, ok := store.Store.(graph.FnValuePlaceholderScanner); ok {
		return scanner.FnValuePlaceholderEdges()
	}
	return store.EdgesByKind(graph.EdgeReferences)
}

type fnValueProjectionGuard struct {
	graph.Store
	cursorOpen bool
	events     []string
	violations []string
}

func (store *fnValueProjectionGuard) CallableBindingNodesSeq(kinds ...graph.NodeKind) iter.Seq[graph.CallableBindingNode] {
	sequence := graph.CallableBindingNodesSeq(store.Store, kinds...)
	return func(yield func(graph.CallableBindingNode) bool) {
		store.events = append(store.events, "projection-start")
		store.cursorOpen = true
		defer func() {
			store.cursorOpen = false
			store.events = append(store.events, "projection-end")
		}()
		for node := range sequence {
			if !yield(node) {
				return
			}
		}
	}
}

func (store *fnValueProjectionGuard) GetFileNodes(filePath string) []*graph.Node {
	store.observeOutsideProjection("file-refetch")
	return store.Store.GetFileNodes(filePath)
}

func (store *fnValueProjectionGuard) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	store.observeOutsideProjection("write")
	store.Store.AddBatch(nodes, edges)
}

func (store *fnValueProjectionGuard) observeOutsideProjection(event string) {
	store.events = append(store.events, event)
	if store.cursorOpen {
		store.violations = append(store.violations, event)
	}
}

func fnValueProjectionSnapshot(store graph.Store) []graph.Edge {
	edges := store.AllEdges()
	out := make([]graph.Edge, 0, len(edges))
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		clone := *edge
		if edge.Meta != nil {
			clone.Meta = make(map[string]any, len(edge.Meta))
			for key, value := range edge.Meta {
				clone.Meta[key] = value
			}
		}
		out = append(out, clone)
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := out[i], out[j]
		if left.From != right.From {
			return left.From < right.From
		}
		if left.To != right.To {
			return left.To < right.To
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.FilePath != right.FilePath {
			return left.FilePath < right.FilePath
		}
		return left.Line < right.Line
	})
	return out
}
