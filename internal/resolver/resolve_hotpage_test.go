package resolver

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type unresolvedPageRecordingStore struct {
	graph.Store
	maxRows  int
	maxBytes int
	calls    int
}

func (s *unresolvedPageRecordingStore) BeginUnresolvedEdgeScan(context.Context) (graph.UnresolvedEdgeScan, error) {
	return graph.UnresolvedEdgeScan{HighWaterID: 1, PendingBefore: 1}, nil
}

func (s *unresolvedPageRecordingStore) ReadUnresolvedEdgePage(
	_ context.Context, _ graph.UnresolvedEdgeScan, _ int64, maxRows, maxBytes int,
) (graph.UnresolvedEdgePage, error) {
	s.calls++
	s.maxRows = maxRows
	s.maxBytes = maxBytes
	return graph.UnresolvedEdgePage{
		Edges:     []*graph.Edge{{From: "caller", To: graph.UnresolvedMarker + "Target", Kind: graph.EdgeCalls}},
		NextID:    1,
		Exhausted: true,
	}, nil
}

func TestUnresolvedSQLiteScanPageAmortisesWarmCacheWithByteBound(t *testing.T) {
	store := &unresolvedPageRecordingStore{Store: graph.New()}
	stream := newUnresolvedEdgeStream(store)
	defer stream.close()

	page, done, err := stream.nextPage()
	if err != nil {
		t.Fatal(err)
	}
	if !done || len(page) != 1 {
		t.Fatalf("page done=%v rows=%d, want true/1", done, len(page))
	}
	if store.calls != 1 {
		t.Fatalf("pager calls=%d, want 1", store.calls)
	}
	if resolvePendingScanPageRows != 16*1024 {
		t.Fatalf("scan row constant=%d, want 16384", resolvePendingScanPageRows)
	}
	if store.maxRows != resolvePendingScanPageRows {
		t.Fatalf("scan rows=%d, want %d", store.maxRows, resolvePendingScanPageRows)
	}
	if resolvePendingPageBytes != 16<<20 {
		t.Fatalf("scan byte constant=%d, want 16 MiB", resolvePendingPageBytes)
	}
	if store.maxBytes != resolvePendingPageBytes {
		t.Fatalf("scan bytes=%d, want %d", store.maxBytes, resolvePendingPageBytes)
	}
	if resolvePendingPageRows != 2048 {
		t.Fatalf("compute chunk rows=%d, want 2048", resolvePendingPageRows)
	}
}

type resolveLivenessCountingStore struct {
	graph.Store
	candidateCalls  int
	edgeExistsCalls int
}

func (s *resolveLivenessCountingStore) GetEdgeCandidates(
	endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite,
) graph.EdgeCandidateSet {
	s.candidateCalls++
	return s.Store.GetEdgeCandidates(endpoints, sites)
}

func (s *resolveLivenessCountingStore) EdgeExists(
	from, to string, kind graph.EdgeKind, filePath string, line int,
) bool {
	s.edgeExistsCalls++
	for _, edge := range s.GetOutEdges(from) {
		if edge.To == to && edge.Kind == kind && edge.FilePath == filePath && edge.Line == line {
			return true
		}
	}
	return false
}

func TestResolveJobLivenessIsOneBatchAndMatchesExactEdgeSemantics(t *testing.T) {
	g := graph.New()
	live := &graph.Edge{
		From: "repo::caller", To: graph.UnresolvedMarker + "Target",
		Kind: graph.EdgeReads, FilePath: "pkg/a.go", Line: 17,
	}
	// Same source site, different target: the stale job must not be kept just
	// because another edge at its line is live.
	other := &graph.Edge{
		From: live.From, To: graph.UnresolvedMarker + "Other",
		Kind: live.Kind, FilePath: live.FilePath, Line: live.Line,
	}
	g.AddBatch(nil, []*graph.Edge{live, other})
	store := &resolveLivenessCountingStore{Store: g}

	liveCopy := *live // SQLite returns decoded values, not graph pointer identity.
	staleCopy := *live
	staleCopy.To = graph.UnresolvedMarker + "Missing"
	jobs := [][]reindexJob{{
		{edge: &liveCopy, oldTo: liveCopy.To, newTo: "repo::Target", kind: graph.EdgeReferences},
		{edge: &staleCopy, oldTo: staleCopy.To, newTo: "repo::Missing", kind: graph.EdgeReferences},
	}}

	projection := loadResolveJobLiveness(store, jobs)
	if store.candidateCalls != 1 {
		t.Fatalf("candidate batch calls=%d, want 1 for %d jobs", store.candidateCalls, resolveJobCount(jobs))
	}
	if store.edgeExistsCalls != 0 {
		t.Fatalf("point EdgeExists calls=%d, want 0", store.edgeExistsCalls)
	}
	if !projection.contains(jobs[0][0]) {
		t.Fatal("live decoded edge was dropped")
	}
	if projection.contains(jobs[0][1]) {
		t.Fatal("stale same-site edge was retained")
	}

	current := loadEdgeLiveness(store, []*graph.Edge{&liveCopy, &staleCopy})
	if store.candidateCalls != 2 {
		t.Fatalf("current-shape candidate batch calls=%d, want 2 total", store.candidateCalls)
	}
	if !current.containsEdge(&liveCopy) || current.containsEdge(&staleCopy) {
		t.Fatal("current-shape liveness diverged from exact edge presence")
	}
	if store.edgeExistsCalls != 0 {
		t.Fatalf("point EdgeExists calls after both projections=%d, want 0", store.edgeExistsCalls)
	}
	// The new kind differs from the stored old kind; lookup must use the old
	// shape and still preserve the read-to-reference promotion.
	if jobs[0][0].kind != graph.EdgeReferences || jobs[0][0].edge.Kind != graph.EdgeReads {
		t.Fatal("test did not exercise a kind-changing resolution")
	}
}
