package graph

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

type recordingBoundedIncomingReader struct {
	Reader
	bounded BoundedIncomingSourceReader
	calls   int
	limits  []int
}

func (reader *recordingBoundedIncomingReader) FindIncomingSourcesBounded(
	ctx context.Context,
	targetIDs []string,
	kind EdgeKind,
	limit int,
) (BoundedIncomingSourceProjection, error) {
	reader.calls++
	reader.limits = append(reader.limits, limit)
	return reader.bounded.FindIncomingSourcesBounded(ctx, targetIDs, kind, limit)
}

func TestGraphFindIncomingSourcesBoundedCountsDistinctRelevantSources(t *testing.T) {
	memory := New()
	const targetID = "repo/target.go::target"
	for index := 0; index < 8; index++ {
		sourceID := fmt.Sprintf("repo/source-%02d.go::source", index)
		for line := 1; line <= 3; line++ {
			memory.AddEdge(&Edge{From: sourceID, To: targetID, Kind: EdgeCalls, Line: line})
		}
		memory.AddEdge(&Edge{From: fmt.Sprintf("repo/noise-%02d.go::noise", index), To: targetID, Kind: EdgeReferences})
	}

	page, err := memory.FindIncomingSourcesBounded(context.Background(), []string{targetID}, EdgeCalls, 8)
	if err != nil {
		t.Fatalf("bounded incoming projection: %v", err)
	}
	if page.Truncated[targetID] || len(page.Sources[targetID]) != 8 {
		t.Fatalf("projection = %#v, want eight distinct CALLS sources", page)
	}
	if !sort.StringsAreSorted(page.Sources[targetID]) {
		t.Fatalf("sources are not deterministic: %v", page.Sources[targetID])
	}

	memory.AddEdge(&Edge{From: "repo/source-08.go::source", To: targetID, Kind: EdgeCalls})
	page, err = memory.FindIncomingSourcesBounded(context.Background(), []string{targetID}, EdgeCalls, 8)
	if err != nil {
		t.Fatalf("saturated incoming projection: %v", err)
	}
	if !page.Truncated[targetID] || len(page.Sources[targetID]) != 0 {
		t.Fatalf("saturated projection exposed a partial source set: %#v", page)
	}
}

func TestGraphFindIncomingSourcesBoundedRejectsImpossibleLimitAndCancellation(t *testing.T) {
	memory := New()
	memory.AddEdge(&Edge{From: "source", To: "target", Kind: EdgeCalls})
	maxInt := int(^uint(0) >> 1)
	if page, err := memory.FindIncomingSourcesBounded(context.Background(), []string{"target"}, EdgeCalls, maxInt); err == nil || len(page.Sources) != 0 {
		t.Fatalf("max-int projection = %#v, %v; want empty typed limit error", page, err)
	} else {
		var limitErr *BoundedLocalizationLimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("max-int error = %T %v, want BoundedLocalizationLimitError", err, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if page, err := memory.FindIncomingSourcesBounded(ctx, []string{"target"}, EdgeCalls, 8); !errors.Is(err, context.Canceled) || len(page.Sources) != 0 {
		t.Fatalf("cancelled projection = %#v, %v", page, err)
	}
}

func TestOverlaidViewFindIncomingSourcesBoundedReappliesLimitAfterCompensation(t *testing.T) {
	base := New()
	const targetID = "repo/target.go::target"
	for index := 0; index < 9; index++ {
		base.AddEdge(&Edge{
			From: fmt.Sprintf("repo/visible-%02d.go::source", index), To: targetID, Kind: EdgeCalls,
		})
	}
	layer := NewOverlayLayer()
	layer.MarkFile("repo/unrelated.go", false)
	for index := 0; index < 300; index++ {
		layer.AddNode("repo/unrelated.go", &Node{
			ID: fmt.Sprintf("repo/unrelated.go::shadow-%03d", index), Name: "shadow",
			Kind: KindFunction, FilePath: "repo/unrelated.go",
		})
	}
	recording := &recordingBoundedIncomingReader{Reader: base, bounded: base}
	page, err := NewOverlaidView(recording, layer).FindIncomingSourcesBounded(
		context.Background(), []string{targetID}, EdgeCalls, 8,
	)
	if err != nil {
		t.Fatalf("compensated overlay projection: %v", err)
	}
	if !page.Truncated[targetID] || len(page.Sources[targetID]) != 0 {
		t.Fatalf("compensation bypassed caller cap: %#v", page)
	}
	if !reflect.DeepEqual(recording.limits, []int{308}) {
		t.Fatalf("base compensation limit = %v, want 308", recording.limits)
	}
}

func TestOverlaidViewFindIncomingSourcesBoundedSeparatesStandardAndDetachedShadows(t *testing.T) {
	base := New()
	const (
		targetID = "repo/target.go::target"
		filePath = "repo/edited.go"
	)
	layer := NewOverlayLayer()
	layer.MarkFile(filePath, false)
	for index := 0; index < 300; index++ {
		id := fmt.Sprintf("%s::source-%03d", filePath, index)
		base.AddNode(&Node{ID: id, Name: "source", Kind: KindFunction, FilePath: filePath})
		base.AddEdge(&Edge{From: id, To: targetID, Kind: EdgeCalls})
		layer.AddNode(filePath, &Node{ID: id, Name: "source", Kind: KindFunction, FilePath: filePath})
	}
	currentID := filePath + "::source-000"
	layer.AddEdge(&Edge{From: currentID, To: targetID, Kind: EdgeCalls})
	recording := &recordingBoundedIncomingReader{Reader: base, bounded: base}
	page, err := NewOverlaidView(recording, layer).FindIncomingSourcesBounded(
		context.Background(), []string{targetID}, EdgeCalls, 8,
	)
	if err != nil {
		t.Fatalf("ordinary overlay shadows above detached cap: %v", err)
	}
	if page.Truncated[targetID] || !reflect.DeepEqual(page.Sources[targetID], []string{currentID}) {
		t.Fatalf("overlay replacement sources = %#v, want only current %q", page, currentID)
	}
	if recording.calls != 1 || len(recording.limits) != 1 || recording.limits[0] != 308 {
		t.Fatalf("base calls/limits = %d/%v, want one exact compensation call at 308", recording.calls, recording.limits)
	}

	for _, test := range []struct {
		name      string
		shadows   int
		wantError bool
	}{
		{name: "exact detached cap", shadows: overlayDetachedShadowLimit},
		{name: "above detached cap", shadows: overlayDetachedShadowLimit + 1, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			empty := New()
			counted := &recordingBoundedIncomingReader{Reader: empty, bounded: empty}
			detached := NewOverlayLayer()
			for index := 0; index < test.shadows; index++ {
				detached.MarkRemoved("source", fmt.Sprintf("legacy-source-%03d", index))
			}
			got, gotErr := NewOverlaidView(counted, detached).FindIncomingSourcesBounded(
				context.Background(), []string{targetID}, EdgeCalls, 8,
			)
			if test.wantError {
				var limitErr *BoundedLocalizationLimitError
				if !errors.As(gotErr, &limitErr) ||
					limitErr.Resource != "overlay incoming-source detached shadows" ||
					limitErr.Limit != overlayDetachedShadowLimit || counted.calls != 0 ||
					len(got.Sources) != 0 || len(got.Truncated) != 0 {
					t.Fatalf("overflow = %#v, %v, base calls %d; want exact pre-base detached limit failure", got, gotErr, counted.calls)
				}
				return
			}
			if gotErr != nil || counted.calls != 1 || counted.limits[0] != 8+overlayDetachedShadowLimit {
				t.Fatalf("exact-cap projection = %#v, %v, calls/limits %d/%v", got, gotErr, counted.calls, counted.limits)
			}
		})
	}
}

func TestOverlaidViewFindIncomingSourcesBoundedHonorsDetachedSourceAndTargetState(t *testing.T) {
	base := New()
	const (
		targetID = "legacy-target"
		staleID  = "legacy-stale-source"
		freshID  = "legacy-fresh-source"
	)
	base.AddNode(&Node{ID: targetID, Name: "target", Kind: KindFunction})
	base.AddNode(&Node{ID: staleID, Name: "stale", Kind: KindFunction})
	base.AddEdge(&Edge{From: staleID, To: targetID, Kind: EdgeCalls})

	tombstoneSource := NewOverlayLayer()
	tombstoneSource.MarkRemoved("stale", staleID)
	page, err := NewOverlaidView(base, tombstoneSource).FindIncomingSourcesBounded(
		context.Background(), []string{targetID}, EdgeCalls, 8,
	)
	if err != nil || len(page.Sources[targetID]) != 0 {
		t.Fatalf("detached source tombstone leaked stale caller: %#v, %v", page, err)
	}

	tombstoneTarget := NewOverlayLayer()
	tombstoneTarget.MarkRemoved("target", targetID)
	page, err = NewOverlaidView(base, tombstoneTarget).FindIncomingSourcesBounded(
		context.Background(), []string{targetID}, EdgeCalls, 8,
	)
	if err != nil || len(page.Sources[targetID]) != 0 {
		t.Fatalf("detached target tombstone leaked base adjacency: %#v, %v", page, err)
	}

	replacement := NewOverlayLayer()
	replacement.MarkRemoved("target", targetID)
	replacement.AddNode("repo/replacement.go", &Node{ID: targetID, Name: "target", Kind: KindFunction, FilePath: "repo/replacement.go"})
	replacement.AddNode("repo/caller.go", &Node{ID: freshID, Name: "fresh", Kind: KindFunction, FilePath: "repo/caller.go"})
	replacement.AddEdge(&Edge{From: freshID, To: targetID, Kind: EdgeCalls})
	page, err = NewOverlaidView(base, replacement).FindIncomingSourcesBounded(
		context.Background(), []string{targetID}, EdgeCalls, 8,
	)
	if err != nil || !reflect.DeepEqual(page.Sources[targetID], []string{freshID, staleID}) {
		t.Fatalf("detached target replacement callers = %#v, %v; want current overlay plus unaffected base caller", page, err)
	}
}

func TestOverlaidViewGetNodesByIDsContextPreservesInputAndDetachedMasks(t *testing.T) {
	base := New()
	base.AddNode(&Node{ID: "base", Name: "base", Kind: KindFunction})
	base.AddNode(&Node{ID: "removed", Name: "removed", Kind: KindFunction})
	layer := NewOverlayLayer()
	layer.MarkRemoved("removed", "removed")
	layer.AddNode("repo/overlay.go", &Node{ID: "overlay", Name: "overlay", Kind: KindFunction, FilePath: "repo/overlay.go"})
	view := NewOverlaidView(base, layer)
	ids := []string{"overlay", "base", "removed", "base"}
	before := append([]string(nil), ids...)
	nodes, err := view.GetNodesByIDsContext(context.Background(), ids)
	if err != nil {
		t.Fatalf("contextual overlay refetch: %v", err)
	}
	if !reflect.DeepEqual(ids, before) {
		t.Fatalf("caller IDs mutated: got %v want %v", ids, before)
	}
	if nodes["overlay"] == nil || nodes["base"] == nil || nodes["removed"] != nil {
		t.Fatalf("contextual overlay refetch = %#v", nodes)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if cancelled, err := view.GetNodesByIDsContext(ctx, ids); !errors.Is(err, context.Canceled) || len(cancelled) != 0 {
		t.Fatalf("cancelled contextual refetch = %#v, %v", cancelled, err)
	}
}

func TestBoundedIncomingSourcesCancelsDuringGraphAndOverlayInspection(t *testing.T) {
	memory := New()
	for index := 0; index < 256; index++ {
		memory.AddEdge(&Edge{
			From: fmt.Sprintf("source-%03d", index), To: "target", Kind: EdgeCalls,
		})
	}
	ctx := &cancelAfterLocalizationChecksContext{
		Context: context.Background(), remaining: 3, done: make(chan struct{}),
	}
	page, err := memory.FindIncomingSourcesBounded(ctx, []string{"target"}, EdgeCalls, 512)
	if !errors.Is(err, context.Canceled) || len(page.Sources) != 0 || len(page.Truncated) != 0 {
		t.Fatalf("mid-graph cancellation = %#v, %v", page, err)
	}

	base := New()
	recording := &recordingBoundedIncomingReader{Reader: base, bounded: base}
	layer := NewOverlayLayer()
	layer.MarkFile("repo/edited.go", false)
	for index := 0; index < 512; index++ {
		layer.MarkRemoved("source", fmt.Sprintf("repo/edited.go::source-%03d", index))
	}
	ctx = &cancelAfterLocalizationChecksContext{
		Context: context.Background(), remaining: 3, done: make(chan struct{}),
	}
	page, err = NewOverlaidView(recording, layer).FindIncomingSourcesBounded(
		ctx, []string{"target"}, EdgeCalls, 8,
	)
	if !errors.Is(err, context.Canceled) || len(page.Sources) != 0 || len(page.Truncated) != 0 || recording.calls != 0 {
		t.Fatalf("mid-overlay cancellation = %#v, %v, base calls=%d", page, err, recording.calls)
	}
}

func TestOverlaidViewFindIncomingSourcesBoundedDedupesShadowsAndGuardsCompensation(t *testing.T) {
	base := New()
	recording := &recordingBoundedIncomingReader{Reader: base, bounded: base}
	layer := NewOverlayLayer()
	layer.MarkRemoved("first", "legacy-source")
	layer.MarkRemoved("second", "legacy-source")
	layer.AddNode("repo/overlay.go", &Node{
		ID: "legacy-source", Name: "replacement", Kind: KindFunction, FilePath: "repo/overlay.go",
	})
	page, err := NewOverlaidView(recording, layer).FindIncomingSourcesBounded(
		context.Background(), []string{"target"}, EdgeCalls, 8,
	)
	if err != nil || len(page.Sources) != 0 || recording.calls != 1 || !reflect.DeepEqual(recording.limits, []int{9}) {
		t.Fatalf("deduped shadow projection = %#v, %v, calls/limits=%d/%v", page, err, recording.calls, recording.limits)
	}

	overflowBase := New()
	overflowRecording := &recordingBoundedIncomingReader{Reader: overflowBase, bounded: overflowBase}
	overflowLayer := NewOverlayLayer()
	overflowLayer.MarkRemoved("first", "legacy-one")
	overflowLayer.MarkRemoved("second", "legacy-two")
	maxInt := int(^uint(0) >> 1)
	page, err = NewOverlaidView(overflowRecording, overflowLayer).FindIncomingSourcesBounded(
		context.Background(), []string{"target"}, EdgeCalls, maxInt-1,
	)
	var limitErr *BoundedLocalizationLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != "overlay incoming-source compensation" ||
		overflowRecording.calls != 0 || len(page.Sources) != 0 || len(page.Truncated) != 0 {
		t.Fatalf("compensation overflow = %#v, %v, base calls=%d", page, err, overflowRecording.calls)
	}
}

type contextualExactNodeSpy struct {
	Reader
	nodes map[string]*Node
	err   error
	calls int
	ctx   context.Context
}

func (spy *contextualExactNodeSpy) GetNodesByIDs([]string) map[string]*Node {
	panic("non-contextual exact refetch must not be used")
}

func (spy *contextualExactNodeSpy) GetNodesByIDsContext(ctx context.Context, ids []string) (map[string]*Node, error) {
	spy.calls++
	spy.ctx = ctx
	out := make(map[string]*Node, len(ids))
	for _, id := range ids {
		if node := spy.nodes[id]; node != nil {
			out[id] = node
		}
	}
	return out, spy.err
}

func TestOverlaidViewGetNodesByIDsContextDelegatesContextAndDropsPartialErrors(t *testing.T) {
	type contextKey string
	const key contextKey = "request"
	requestCtx := context.WithValue(context.Background(), key, "bounded")
	spy := &contextualExactNodeSpy{
		nodes: map[string]*Node{"base": {ID: "base", Name: "base", Kind: KindFunction}},
	}
	view := NewOverlaidView(spy, NewOverlayLayer())
	nodes, err := view.GetNodesByIDsContext(requestCtx, []string{"base"})
	if err != nil || nodes["base"] == nil || spy.calls != 1 || spy.ctx.Value(key) != "bounded" {
		t.Fatalf("contextual delegation = %#v, %v, calls=%d, ctx=%v", nodes, err, spy.calls, spy.ctx)
	}

	spy.err = errors.New("base refetch failed")
	nodes, err = view.GetNodesByIDsContext(requestCtx, []string{"base"})
	if err == nil || len(nodes) != 0 {
		t.Fatalf("base error returned partial exact nodes: %#v, %v", nodes, err)
	}
}

