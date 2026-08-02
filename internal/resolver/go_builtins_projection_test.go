package resolver

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type goBuiltinProjectionSpy struct {
	graph.Store
	reader       graph.InEdgeIdentityBatchReader
	targeter     graph.UnresolvedEdgeTargetBatchReindexer
	fullReads    int
	compactReads int
}

func newGoBuiltinProjectionSpy(store *store_sqlite.Store) *goBuiltinProjectionSpy {
	return &goBuiltinProjectionSpy{
		Store: store, reader: store, targeter: store,
	}
}

func (s *goBuiltinProjectionSpy) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.fullReads++
	return s.Store.GetInEdgesByNodeIDs(ids)
}

func (s *goBuiltinProjectionSpy) GetInEdgeIdentitiesByNodeIDs(ids []string) map[string][]graph.EdgeIdentity {
	s.compactReads++
	return s.reader.GetInEdgeIdentitiesByNodeIDs(ids)
}

func (s *goBuiltinProjectionSpy) ReindexUnresolvedEdgeTargets(
	batch []graph.UnresolvedEdgeTargetReindex,
) {
	s.targeter.ReindexUnresolvedEdgeTargets(batch)
}

func TestAttributeGoBuiltinsUsesCompactIncomingProjection(t *testing.T) {
	store := openGoBuiltinProjectionStore(t)
	spy := newGoBuiltinProjectionSpy(store)
	sourceID := "repo/main.go::run"
	store.AddBatch(
		[]*graph.Node{{
			ID: sourceID, Kind: graph.KindFunction, Name: "run", Language: "go",
			FilePath: "repo/main.go", RepoPrefix: "repo", WorkspaceID: "workspace",
			ProjectID: "project",
		}},
		[]*graph.Edge{
			{
				From: sourceID, To: "unresolved::len", Kind: graph.EdgeCalls,
				FilePath: "repo/main.go", Line: 7,
				Meta: map[string]any{"payload": "must survive compact retarget"},
			},
			{
				From: sourceID, To: "unresolved::len", Kind: graph.EdgeContains,
				FilePath: "repo/main.go", Line: 8,
			},
		},
	)

	New(spy).attributeGoBuiltins()

	if spy.compactReads != 1 {
		t.Fatalf("compact incoming reads = %d, want 1", spy.compactReads)
	}
	if spy.fullReads != 0 {
		t.Fatalf("full incoming reads = %d, want 0", spy.fullReads)
	}
	resolvedID, _, _ := goBuiltinTarget("repo", "len")
	var resolved, untouched *graph.Edge
	for _, edge := range store.GetOutEdges(sourceID) {
		switch edge.Line {
		case 7:
			resolved = edge
		case 8:
			untouched = edge
		}
	}
	if resolved == nil || resolved.To != resolvedID {
		t.Fatalf("calls edge target = %#v, want %q", resolved, resolvedID)
	}
	if resolved.Meta["payload"] != "must survive compact retarget" {
		t.Fatalf("compact retarget lost edge metadata: %#v", resolved.Meta)
	}
	if untouched == nil || untouched.To != "unresolved::len" {
		t.Fatalf("non-candidate edge changed: %#v", untouched)
	}
	builtin := store.GetNode(resolvedID)
	if builtin == nil || builtin.RepoPrefix != "repo" ||
		builtin.WorkspaceID != "workspace" || builtin.ProjectID != "project" {
		t.Fatalf("materialised builtin boundary = %#v", builtin)
	}
}

func TestAttributeGoBuiltinsKeepsFullEdgeFallback(t *testing.T) {
	store := graph.New()
	sourceID := "repo/main.go::run"
	store.AddBatch(
		[]*graph.Node{{
			ID: sourceID, Kind: graph.KindFunction, Name: "run", Language: "go",
			FilePath: "repo/main.go", RepoPrefix: "repo",
		}},
		[]*graph.Edge{{
			From: sourceID, To: "unresolved::append", Kind: graph.EdgeCalls,
			FilePath: "repo/main.go", Line: 11,
		}},
	)

	New(store).attributeGoBuiltins()

	resolvedID, _, _ := goBuiltinTarget("repo", "append")
	edges := store.GetOutEdges(sourceID)
	if len(edges) != 1 || edges[0].To != resolvedID {
		t.Fatalf("fallback edge = %#v, want target %q", edges, resolvedID)
	}
}

var (
	benchmarkGoBuiltinFullEdges  map[string][]*graph.Edge
	benchmarkGoBuiltinIdentities map[string][]graph.EdgeIdentity
)

func BenchmarkGoBuiltinIncomingProjection(b *testing.B) {
	store := openGoBuiltinProjectionStore(b)
	const edgeCount = 8192
	payload := strings.Repeat("metadata-payload-", 128)
	edges := make([]*graph.Edge, edgeCount)
	for i := range edges {
		edges[i] = &graph.Edge{
			From: "repo/main.go::run", To: "unresolved::len", Kind: graph.EdgeCalls,
			FilePath: "repo/main.go", Line: i + 1,
			Origin: "benchmark", ConfidenceLabel: "high",
			Meta: map[string]any{"payload": payload, "ordinal": i},
		}
	}
	store.AddBatch(nil, edges)
	targets := []string{"unresolved::len"}

	b.Run("full_edges", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkGoBuiltinFullEdges = store.GetInEdgesByNodeIDs(targets)
		}
	})
	b.Run("identities", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkGoBuiltinIdentities = store.GetInEdgeIdentitiesByNodeIDs(targets)
		}
	})
}

func openGoBuiltinProjectionStore(tb testing.TB) *store_sqlite.Store {
	tb.Helper()
	store, err := store_sqlite.Open(filepath.Join(tb.TempDir(), fmt.Sprintf("%s.sqlite", tb.Name())))
	if err != nil {
		tb.Fatalf("open sqlite store: %v", err)
	}
	tb.Cleanup(func() {
		if err := store.Close(); err != nil {
			tb.Errorf("close sqlite store: %v", err)
		}
	})
	return store
}
