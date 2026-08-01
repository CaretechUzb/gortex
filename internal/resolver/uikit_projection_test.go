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

func TestResolveUIKitRefsProjectionParity(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		backend := backend
		t.Run(backend, func(t *testing.T) {
			legacy := newUIKitProjectionStore(t, backend, "legacy")
			projected := newUIKitProjectionStore(t, backend, "projected")
			populateUIKitProjectionFixture(legacy, 16, "fixture-metadata")
			populateUIKitProjectionFixture(projected, 16, "fixture-metadata")

			require.Equal(t, resolveUIKitRefsLegacyForParity(legacy), ResolveUIKitRefs(projected))
			require.Equal(t, uikitProjectionSnapshot(legacy), uikitProjectionSnapshot(projected))
		})
	}
}

func TestResolveUIKitRefsClosesProjectionBeforeReadsAndWrites(t *testing.T) {
	store := &uikitCursorGuardStore{Store: graph.New()}
	populateUIKitProjectionFixture(store, 4, "fixture-metadata")

	require.Equal(t, 4, ResolveUIKitRefs(store))
	require.False(t, store.cursorOpen)
	require.Empty(t, store.violations)
	require.Equal(t, []string{
		"scan-start", "scan-end",
		"scan-start", "scan-end",
		"scan-start", "scan-end",
		"scan-start", "scan-end",
		"placements", "refetch", "reindex",
	}, store.events)
}

func BenchmarkResolveUIKitRefsSQLiteProjection(b *testing.B) {
	for _, benchmark := range []struct {
		name string
		fn   func(graph.Store) int
	}{
		{name: "legacy-full-edge", fn: resolveUIKitRefsLegacyForParity},
		{name: "projected-identity", fn: ResolveUIKitRefs},
	} {
		benchmark := benchmark
		b.Run(benchmark.name, func(b *testing.B) {
			store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "uikit.sqlite"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, store.Close()) })
			populateUIKitProjectionFixture(store, 40000, strings.Repeat("x", 512))
			require.Equal(b, 4, benchmark.fn(store))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				require.Zero(b, benchmark.fn(store))
			}
		})
	}
}

func resolveUIKitRefsLegacyForParity(g graph.Store) int {
	if g == nil {
		return 0
	}
	resolved := 0
	var reindex []graph.EdgeReindex
	for _, kind := range []graph.EdgeKind{graph.EdgeInstantiates, graph.EdgeReferences, graph.EdgeTypedAs, graph.EdgeCalls} {
		for edge := range g.EdgesByKind(kind) {
			if edge == nil || !graph.IsUnresolvedTarget(edge.To) {
				continue
			}
			name := graph.UnresolvedName(edge.To)
			if name == "" || strings.ContainsRune(name, '.') {
				continue
			}
			dirs, ok := uikitDirsFor(name)
			if !ok {
				continue
			}
			fromFile := ""
			if node := g.GetNode(edge.From); node != nil {
				fromFile = node.FilePath
			}
			if !isAppleSourceFile(fromFile) {
				continue
			}
			targetID, confidence := ResolveByConvention(g, name, "", dirs, fromFile)
			if targetID == "" {
				continue
			}
			oldTo := edge.To
			edge.To = targetID
			edge.Origin = graph.OriginASTInferred
			edge.Confidence = confidence
			edge.ConfidenceLabel = graph.ConfidenceLabelFor(edge.Kind, confidence)
			StampSynthesized(edge, SynthUIKitResolve)
			reindex = append(reindex, graph.EdgeReindex{Edge: edge, OldTo: oldTo})
			resolved++
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func newUIKitProjectionStore(t *testing.T, backend, name string) graph.Store {
	t.Helper()
	if backend == "memory" {
		return graph.New()
	}
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), name+".sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func populateUIKitProjectionFixture(store graph.Store, noiseEdges int, payload string) {
	const (
		sourceID     = "App/Sources/Screen.swift::Screen"
		nonAppleID   = "Web/Sources/use.ts::use"
		targetID     = "App/ViewControllers/FeedViewController.swift::FeedViewController"
		unresolved   = "unresolved::FeedViewController"
		sourceFile   = "App/Sources/Screen.swift"
		nonAppleFile = "Web/Sources/use.ts"
	)
	store.AddBatch([]*graph.Node{
		{ID: sourceID, Kind: graph.KindType, Name: "Screen", FilePath: sourceFile, Language: "swift"},
		{ID: nonAppleID, Kind: graph.KindFunction, Name: "use", FilePath: nonAppleFile, Language: "typescript"},
		{ID: targetID, Kind: graph.KindType, Name: "FeedViewController", FilePath: "App/ViewControllers/FeedViewController.swift", Language: "swift"},
	}, nil)

	kinds := []graph.EdgeKind{graph.EdgeInstantiates, graph.EdgeReferences, graph.EdgeTypedAs, graph.EdgeCalls}
	edges := make([]*graph.Edge, 0, len(kinds)+3)
	for i, kind := range kinds {
		edges = append(edges, &graph.Edge{
			From: sourceID, To: unresolved, Kind: kind, FilePath: sourceFile, Line: i + 1,
			Origin: "sourcekit", Confidence: 0.42, ConfidenceLabel: "medium",
			Meta: map[string]any{"payload": payload, "ordinal": i},
		})
	}
	edges = append(edges,
		&graph.Edge{From: nonAppleID, To: unresolved, Kind: graph.EdgeReferences, FilePath: nonAppleFile, Line: 50, Meta: map[string]any{"payload": payload}},
		&graph.Edge{From: sourceID, To: "unresolved::UIKit.FeedViewController", Kind: graph.EdgeReferences, FilePath: sourceFile, Line: 51, Meta: map[string]any{"payload": payload}},
		&graph.Edge{From: sourceID, To: targetID, Kind: graph.EdgeReferences, FilePath: sourceFile, Line: 52, Meta: map[string]any{"payload": payload}},
	)
	store.AddBatch(nil, edges)

	const chunkSize = 2000
	for base := 0; base < noiseEdges; base += chunkSize {
		end := min(base+chunkSize, noiseEdges)
		batch := make([]*graph.Edge, 0, end-base)
		for i := base; i < end; i++ {
			kind := kinds[i%len(kinds)]
			batch = append(batch, &graph.Edge{
				From: sourceID, To: fmt.Sprintf("unresolved::Noise%d", i), Kind: kind,
				FilePath: sourceFile, Line: 100 + i,
				Meta: map[string]any{"payload": payload, "ordinal": i},
			})
		}
		store.AddBatch(nil, batch)
	}
}

func uikitProjectionSnapshot(store graph.Store) []graph.Edge {
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

type uikitCursorGuardStore struct {
	graph.Store
	cursorOpen bool
	events     []string
	violations []string
}

func (store *uikitCursorGuardStore) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	sequence := graph.EdgesLightSeq(store.Store, kinds...)
	return func(yield func(*graph.Edge) bool) {
		store.events = append(store.events, "scan-start")
		store.cursorOpen = true
		defer func() {
			store.cursorOpen = false
			store.events = append(store.events, "scan-end")
		}()
		for edge := range sequence {
			if !yield(edge) {
				return
			}
		}
	}
}

func (store *uikitCursorGuardStore) NodePlacementsByIDs(ids []string) map[string]graph.NodePlacement {
	store.observeOutsideCursor("placements")
	return graph.NodePlacementsByIDs(store.Store, ids)
}

func (store *uikitCursorGuardStore) FindEdgesByIdentities(identities []graph.EdgeIdentity) map[graph.EdgeIdentity]*graph.Edge {
	store.observeOutsideCursor("refetch")
	finder := store.Store.(graph.EdgeIdentityBatchFinder)
	return finder.FindEdgesByIdentities(identities)
}

func (store *uikitCursorGuardStore) ReindexEdges(batch []graph.EdgeReindex) {
	store.observeOutsideCursor("reindex")
	store.Store.ReindexEdges(batch)
}

func (store *uikitCursorGuardStore) observeOutsideCursor(event string) {
	store.events = append(store.events, event)
	if store.cursorOpen {
		store.violations = append(store.violations, event)
	}
}
