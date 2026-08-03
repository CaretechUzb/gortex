package resolver

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"unsafe"

	"github.com/zzet/gortex/internal/graph"
)

type cancelingUnresolvedPager struct {
	graph.Store
	edge      *graph.Edge
	cancel    context.CancelFunc
	pageCalls int
}

func (s *cancelingUnresolvedPager) BeginUnresolvedEdgeScan(context.Context) (graph.UnresolvedEdgeScan, error) {
	return graph.UnresolvedEdgeScan{HighWaterID: 2, PendingBefore: -1}, nil
}

func (s *cancelingUnresolvedPager) ReadUnresolvedEdgePage(
	ctx context.Context, _ graph.UnresolvedEdgeScan, afterID int64, _, _ int,
) (graph.UnresolvedEdgePage, error) {
	s.pageCalls++
	if afterID == 0 {
		return graph.UnresolvedEdgePage{Edges: []*graph.Edge{s.edge}, NextID: 1}, nil
	}
	s.cancel()
	<-ctx.Done()
	return graph.UnresolvedEdgePage{}, ctx.Err()
}

func TestResolveAllContextStopsBeforeReadinessAndTailsOnPagerCancel(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "caller", Kind: graph.KindFunction, Name: "caller", FilePath: "x.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "caller", To: graph.UnresolvedMarker + "missing", Kind: graph.EdgeCalls, FilePath: "x.go", Line: 1})
	ctx, cancel := context.WithCancel(t.Context())
	store := &cancelingUnresolvedPager{Store: g, edge: g.GetOutEdges("caller")[0], cancel: cancel}
	r := New(store)
	ready := false
	r.OnComputeDone = func() { ready = true }

	stats, err := r.ResolveAllContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ResolveAllContext error = %v, want context.Canceled", err)
	}
	if ready {
		t.Fatal("canceled resolve published compute readiness")
	}
	if store.pageCalls != 2 {
		t.Fatalf("pager calls = %d, want 2", store.pageCalls)
	}
	if stats == nil || stats.PendingBefore != 1 {
		t.Fatalf("partial stats = %+v, want one dynamically counted pending edge", stats)
	}
}

type failedUnresolvedPager struct {
	graph.Store
	err error
}

func (s *failedUnresolvedPager) BeginUnresolvedEdgeScan(context.Context) (graph.UnresolvedEdgeScan, error) {
	return graph.UnresolvedEdgeScan{}, s.err
}

func (s *failedUnresolvedPager) ReadUnresolvedEdgePage(context.Context, graph.UnresolvedEdgeScan, int64, int, int) (graph.UnresolvedEdgePage, error) {
	panic("page read after failed begin")
}

func TestCanceledNativePagerNeverFallsBackToLegacySpool(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stream := newUnresolvedEdgeStreamContext(ctx, &failedUnresolvedPager{Store: graph.New(), err: context.Canceled})
	defer stream.close()
	if !errors.Is(stream.initErr, context.Canceled) {
		t.Fatalf("stream error = %v, want context.Canceled", stream.initErr)
	}
	if stream.legacy != nil {
		t.Fatal("canceled native pager fell back to legacy whole-corpus spool")
	}
}

func TestResolveGuardRecordSizeCoversFixedMemory(t *testing.T) {
	fixed := resolveGuardRecordSize(resolveGuardRecord{})
	structBytes := int(unsafe.Sizeof(resolveGuardRecord{}))
	if fixed < structBytes+32 {
		t.Fatalf("fixed record accounting=%d, want at least %d-byte struct + 32 bytes headroom", fixed, structBytes)
	}
}

func TestResolveGuardSpoolHonorsInlineByteLimit(t *testing.T) {
	spool, err := newResolveGuardSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()

	record := resolveGuardRecord{Payload: persistedEdgeSpoolSnapshot{Meta: make([]byte, 32<<10)}}
	recordBytes := resolveGuardRecordSize(record)
	inlineRecords := resolveSpoolInlineBytes / recordBytes
	if inlineRecords == 0 || inlineRecords >= resolvePendingPageRows {
		t.Fatalf("invalid byte-boundary fixture: records=%d bytes=%d", inlineRecords, recordBytes)
	}
	for i := 0; i < inlineRecords; i++ {
		record.Payload.Meta = make([]byte, 32<<10)
		if err := spool.append(record); err != nil {
			t.Fatal(err)
		}
	}
	if spool.file != nil {
		t.Fatal("guard spool spilled before reaching the inline byte limit")
	}
	record.Payload.Meta = make([]byte, 32<<10)
	if err := spool.append(record); err != nil {
		t.Fatal(err)
	}
	if spool.file == nil {
		t.Fatal("guard spool did not spill after crossing the inline byte limit")
	}
}

func TestUnresolvedEdgeStreamKeepsSmallLegacyCorpusInline(t *testing.T) {
	store := graph.New()
	edges := make([]*graph.Edge, 0, 8)
	for i := 0; i < cap(edges); i++ {
		edges = append(edges, &graph.Edge{
			From: fmt.Sprintf("source-%d", i), To: fmt.Sprintf("unresolved::target-%d", i),
			Kind: graph.EdgeCalls, FilePath: "x.go", Line: i + 1,
		})
	}
	store.AddBatch(nil, edges)

	stream := newUnresolvedEdgeStream(store)
	defer stream.close()
	if stream.legacy == nil {
		t.Fatal("in-memory graph unexpectedly selected the native pager")
	}
	if stream.legacy.file != nil || stream.legacy.decoder != nil {
		t.Fatal("small legacy frontier opened a disk spool")
	}
	if len(stream.legacy.records) != len(edges) {
		t.Fatalf("inline records = %d, want %d", len(stream.legacy.records), len(edges))
	}

	page, done, err := stream.nextPage()
	if err != nil {
		t.Fatal(err)
	}
	if !done || len(page) != len(edges) {
		t.Fatalf("inline replay len=%d done=%v, want %d/true", len(page), done, len(edges))
	}
}

func TestUnresolvedEdgeStreamBoundsLegacyCorpus(t *testing.T) {
	store := graph.New()
	edges := make([]*graph.Edge, 0, resolvePendingPageRows*2+17)
	for i := 0; i < cap(edges); i++ {
		edges = append(edges, &graph.Edge{
			From: fmt.Sprintf("source-%06d", i), To: fmt.Sprintf("unresolved::target-%06d", i),
			Kind: graph.EdgeCalls, FilePath: "x.go", Line: i + 1,
		})
	}
	store.AddBatch(nil, edges)

	stream := newUnresolvedEdgeStream(store)
	defer stream.close()
	if stream.legacy == nil || stream.legacy.file == nil {
		t.Fatal("large legacy frontier did not spill to disk")
	}
	total, peak := 0, 0
	for {
		page, done, err := stream.nextPage()
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > peak {
			peak = len(page)
		}
		total += len(page)
		if done {
			break
		}
	}
	if total != len(edges) {
		t.Fatalf("streamed %d edges, want %d", total, len(edges))
	}
	if peak > resolvePendingPageRows {
		t.Fatalf("peak retained page = %d, bound = %d", peak, resolvePendingPageRows)
	}
}

func TestResolveGuardSpoolKeepsSmallCorpusInline(t *testing.T) {
	spool, err := newResolveGuardSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	edge := &graph.Edge{From: "repo::source", FilePath: "x.go", Line: 1}
	job := reindexJob{
		edge: edge, oldTo: "unresolved::target", newTo: "repo::target",
		kind: graph.EdgeCalls, confidence: 0.5, origin: graph.OriginTextMatched,
	}
	if err := spool.appendJobs([][]reindexJob{{job}}); err != nil {
		t.Fatal(err)
	}
	if spool.file != nil || spool.decoder != nil {
		t.Fatal("small guard corpus opened a disk spool")
	}
	if len(spool.records) != 1 {
		t.Fatalf("inline guard records = %d, want 1", len(spool.records))
	}
	records, done, err := spool.nextPage(16)
	if err != nil {
		t.Fatal(err)
	}
	if !done || len(records) != 1 {
		t.Fatalf("inline guard replay len=%d done=%v, want 1/true", len(records), done)
	}
}

func TestResolveGuardSpoolPagesBoundChangedJobs(t *testing.T) {
	spool, err := newResolveGuardSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()

	jobs := make([]reindexJob, 0, resolvePendingPageRows*2+31)
	for i := 0; i < cap(jobs); i++ {
		edge := &graph.Edge{From: fmt.Sprintf("r::source-%06d", i), FilePath: "x.go", Line: i + 1}
		jobs = append(jobs, reindexJob{
			edge: edge, oldTo: fmt.Sprintf("unresolved::target-%06d", i),
			newTo: fmt.Sprintf("r::target-%06d", i), kind: graph.EdgeCalls,
			confidence: 0.5, origin: graph.OriginTextMatched,
		})
	}
	if err := spool.appendJobs([][]reindexJob{jobs}); err != nil {
		t.Fatal(err)
	}
	if spool.file == nil {
		t.Fatal("large guard corpus did not spill to disk")
	}
	if spool.count != len(jobs) {
		t.Fatalf("spooled %d jobs, want %d", spool.count, len(jobs))
	}

	total, peak, done := 0, 0, false
	for !done {
		page, exhausted, err := spool.nextPage(resolvePendingPageRows)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > peak {
			peak = len(page)
		}
		total += len(page)
		done = exhausted
	}
	if total != len(jobs) || peak > resolvePendingPageRows {
		t.Fatalf("guard replay total=%d peak=%d, want total=%d peak<=%d", total, peak, len(jobs), resolvePendingPageRows)
	}
}

func TestDeferredLSPSpoolKeysetOrderDedupAndPageBound(t *testing.T) {
	spool, err := newDeferredLSPSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()

	work := make([]deferredLSPEdge, 0, resolvePendingPageRows*2+43)
	for i := 0; i < cap(work); i++ {
		edge := &graph.Edge{
			From: fmt.Sprintf("source-%06d", i), To: fmt.Sprintf("resolved-%06d", i),
			Kind: graph.EdgeCalls, FilePath: fmt.Sprintf("file-%06d.go", cap(work)-i), Line: i + 1,
		}
		work = append(work, deferredLSPEdge{edge: edge, target: fmt.Sprintf("target-%06d", i)})
	}
	if err := spool.append(work); err != nil {
		t.Fatal(err)
	}
	duplicate := work[0]
	duplicate.edge = cloneEdgeForResolve(work[0].edge)
	duplicate.edge.To = "updated-target"
	duplicate.carried = true
	if err := spool.append([]deferredLSPEdge{duplicate}); err != nil {
		t.Fatal(err)
	}
	if got := spool.count(); got != len(work) {
		t.Fatalf("deduped work count = %d, want %d", got, len(work))
	}

	iterator := spool.iterator(nil)
	var previous deferredLSPWorkKey
	havePrevious := false
	total, peak := 0, 0
	updatedSeen := false
	for {
		page, done, err := iterator.next(resolvePendingPageRows)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > peak {
			peak = len(page)
		}
		for _, record := range page {
			if havePrevious && deferredLSPWorkKeyLess(record.key, previous) {
				t.Fatalf("spool order regressed: %#v before %#v", previous, record.key)
			}
			previous, havePrevious = record.key, true
			if record.key == deferredLSPWorkKeyFor(duplicate) {
				updatedSeen = record.currentTo == "updated-target" && record.carried
			}
		}
		total += len(page)
		if done {
			break
		}
	}
	if total != len(work) || peak > resolvePendingPageRows || !updatedSeen {
		t.Fatalf("LSP replay total=%d peak=%d updated=%v; want total=%d peak<=%d updated", total, peak, updatedSeen, len(work), resolvePendingPageRows)
	}
}
