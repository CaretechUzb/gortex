package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

type concurrentBoundedFileStoreProbe struct {
	graph.Store

	mu    sync.Mutex
	calls []boundedFileCall
	read  func(context.Context, string, graph.LocalizationNodeScope, int) (graph.BoundedNodeProjection, error)
}

func (store *concurrentBoundedFileStoreProbe) FindFileNodesBounded(
	ctx context.Context,
	path string,
	scope graph.LocalizationNodeScope,
	limit int,
) (graph.BoundedNodeProjection, error) {
	store.mu.Lock()
	store.calls = append(store.calls, boundedFileCall{path: path, scope: scope, limit: limit})
	store.mu.Unlock()
	return store.read(ctx, path, scope, limit)
}

func (store *concurrentBoundedFileStoreProbe) snapshotCalls() []boundedFileCall {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]boundedFileCall(nil), store.calls...)
}

func budgetTestNode(path string) *graph.Node {
	return &graph.Node{
		ID: path + "::owner", Name: "owner", Kind: graph.KindFunction,
		FilePath: path, StartLine: 1, EndLine: 1,
	}
}

func TestWrappedToolSharesAndRenewsLocalizationFileBudget(t *testing.T) {
	probe := &concurrentBoundedFileStoreProbe{Store: graph.New()}
	probe.read = func(_ context.Context, path string, _ graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
		return graph.BoundedNodeProjection{Nodes: []*graph.Node{budgetTestNode(path)}, Total: limit}, nil
	}
	server := fastPathTestServer(t)
	server.graph = probe

	handler := func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		first := server.buildFileSymbolIndexForOrderedPathsScopedContext(
			ctx, []string{"repo/a.go", "repo/b.go"}, query.QueryOptions{},
		)
		if first["repo/a.go"] == nil || first["repo/b.go"] == nil {
			return nil, errors.New("first builder invocation did not complete")
		}
		// Facade and overlay preparation may defensively seed again. The helper
		// must preserve the existing pointer instead of resetting its allowance.
		ctx = withLocalizationFileRequestBudget(ctx)
		second := server.buildFileSymbolIndexForOrderedPathsScopedContext(
			ctx, []string{"repo/c.go", "repo/d.go", "repo/e.go"}, query.QueryOptions{},
		)
		if index := second["repo/e.go"]; index == nil || !index.saturated {
			return nil, fmt.Errorf("shared budget did not saturate final path: %#v", index)
		}
		return mcpgo.NewToolResultText("ok"), nil
	}

	callWrapped(t, server, handler, "search_text")
	if calls := probe.snapshotCalls(); len(calls) != 4 {
		t.Fatalf("first wrapped request made %d storage calls, want 4", len(calls))
	}
	callWrapped(t, server, handler, "search_text")
	calls := probe.snapshotCalls()
	if len(calls) != 8 {
		t.Fatalf("second wrapped request inherited prior budget: calls=%d, want 8", len(calls))
	}
	for _, call := range calls {
		if call.limit != localizationFileNodeLimit {
			t.Fatalf("wrapped call limit = %d, want %d", call.limit, localizationFileNodeLimit)
		}
	}
}

func TestLocalizationFileBudgetSerializesParallelReservations(t *testing.T) {
	probe := &concurrentBoundedFileStoreProbe{Store: graph.New()}
	probe.read = func(_ context.Context, path string, _ graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
		return graph.BoundedNodeProjection{Nodes: []*graph.Node{budgetTestNode(path)}, Total: limit}, nil
	}
	server := &Server{graph: probe}
	ctx := withLocalizationFileRequestBudget(context.Background())

	const workers = 8
	results := make([]map[string]*fileSymbolIndex, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			path := fmt.Sprintf("repo/worker-%d.go", worker)
			results[worker] = server.buildFileSymbolIndexForOrderedPathsScopedContext(
				ctx, []string{path}, query.QueryOptions{},
			)
		}()
	}
	close(start)
	wg.Wait()

	calls := probe.snapshotCalls()
	consumed := 0
	for _, call := range calls {
		consumed += call.limit
	}
	if len(calls) != localizationFileRequestLimit/localizationFileNodeLimit || consumed != localizationFileRequestLimit {
		t.Fatalf("parallel reservations: calls=%d consumed=%d, want calls=%d consumed=%d",
			len(calls), consumed, localizationFileRequestLimit/localizationFileNodeLimit, localizationFileRequestLimit)
	}
	saturated := 0
	for worker, indexes := range results {
		path := fmt.Sprintf("repo/worker-%d.go", worker)
		if index := indexes[path]; index != nil && index.saturated {
			saturated++
		}
	}
	if saturated != workers-len(calls) {
		t.Fatalf("parallel saturated paths = %d, want %d", saturated, workers-len(calls))
	}
}

func TestLocalizationFileBudgetRefundsSuccessfulShortPages(t *testing.T) {
	probe := &concurrentBoundedFileStoreProbe{Store: graph.New()}
	probe.read = func(_ context.Context, path string, _ graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
		total := limit
		if path == "repo/small.go" {
			total = 1
		}
		return graph.BoundedNodeProjection{Nodes: []*graph.Node{budgetTestNode(path)}, Total: total}, nil
	}
	server := &Server{graph: probe}
	ctx := withLocalizationFileRequestBudget(context.Background())

	server.buildFileSymbolIndexForOrderedPathsScopedContext(
		ctx, []string{"repo/small.go"}, query.QueryOptions{},
	)
	paths := []string{"repo/a.go", "repo/b.go", "repo/c.go", "repo/d.go", "repo/e.go"}
	indexes := server.buildFileSymbolIndexForOrderedPathsScopedContext(ctx, paths, query.QueryOptions{})

	calls := probe.snapshotCalls()
	wantLimits := []int{1024, 1024, 1024, 1024, 1023}
	if len(calls) != len(wantLimits) {
		t.Fatalf("calls after short-page refund = %d, want %d", len(calls), len(wantLimits))
	}
	for index, want := range wantLimits {
		if calls[index].limit != want {
			t.Fatalf("call %d limit = %d, want %d", index, calls[index].limit, want)
		}
	}
	if index := indexes["repo/e.go"]; index == nil || !index.saturated {
		t.Fatalf("post-refund exhausted path = %#v, want saturated", index)
	}
}

func TestLocalizationFileBudgetChargesUncertainErrorReservation(t *testing.T) {
	probe := &concurrentBoundedFileStoreProbe{Store: graph.New()}
	probe.read = func(_ context.Context, path string, _ graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
		switch path {
		case "repo/ok.go":
			return graph.BoundedNodeProjection{Nodes: []*graph.Node{budgetTestNode(path)}, Total: 1}, nil
		case "repo/boom.go":
			return graph.BoundedNodeProjection{}, errors.New("read failed")
		default:
			return graph.BoundedNodeProjection{Nodes: []*graph.Node{budgetTestNode(path)}, Total: limit}, nil
		}
	}
	server := &Server{graph: probe}
	ctx := withLocalizationFileRequestBudget(context.Background())

	first := server.buildFileSymbolIndexForOrderedPathsScopedContext(
		ctx, []string{"repo/ok.go", "repo/boom.go", "repo/later.go"}, query.QueryOptions{},
	)
	if first["repo/ok.go"] == nil || first["repo/ok.go"].saturated {
		t.Fatalf("completed path was not preserved: %#v", first["repo/ok.go"])
	}
	for _, path := range []string{"repo/boom.go", "repo/later.go"} {
		if index := first[path]; index == nil || !index.saturated {
			t.Fatalf("error path %q = %#v, want saturated", path, index)
		}
	}

	secondPaths := []string{"repo/a.go", "repo/b.go", "repo/c.go", "repo/d.go"}
	second := server.buildFileSymbolIndexForOrderedPathsScopedContext(ctx, secondPaths, query.QueryOptions{})
	calls := probe.snapshotCalls()
	wantLimits := []int{1024, 1024, 1024, 1024, 1023}
	if len(calls) != len(wantLimits) {
		t.Fatalf("calls after uncertain error = %d, want %d", len(calls), len(wantLimits))
	}
	for index, want := range wantLimits {
		if calls[index].limit != want {
			t.Fatalf("call %d limit = %d, want %d", index, calls[index].limit, want)
		}
	}
	if index := second["repo/d.go"]; index == nil || !index.saturated {
		t.Fatalf("error reservation was refunded: final path=%#v", index)
	}
}

func TestRangeLoweringSharesExhaustedRequestFileBudget(t *testing.T) {
	server, graphStore := setupNavServer(t)
	start := graphStore.GetNode(navFindMethod(t, graphStore, "Start"))
	if start == nil {
		t.Fatal("Start node is missing")
	}

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"source":     "ranges",
		"path":       "svc.go",
		"start_line": float64(start.StartLine),
		"end_line":   float64(start.EndLine),
	}
	exhaustedContext := func() context.Context {
		ctx := withLocalizationFileRequestBudget(context.Background())
		if got := localizationFileBudgetFor(ctx).reserve(localizationFileRequestLimit); got != localizationFileRequestLimit {
			t.Fatalf("reserved %d, want %d", got, localizationFileRequestLimit)
		}
		return ctx
	}

	t.Run("symbols for ranges", func(t *testing.T) {
		result, err := server.handleSymbolsForRanges(exhaustedContext(), req)
		if err != nil || result == nil || result.IsError {
			t.Fatalf("handleSymbolsForRanges = (%#v, %v)", result, err)
		}
		var payload map[string]any
		text := result.Content[0].(mcpgo.TextContent).Text
		if err := json.Unmarshal([]byte(text), &payload); err != nil {
			t.Fatal(err)
		}
		if symbols, _ := payload["symbols"].([]any); len(symbols) != 0 {
			t.Fatalf("exhausted request budget admitted range symbols: %#v", symbols)
		}
		if unresolved, _ := payload["unresolved_files"].([]any); len(unresolved) != 1 || unresolved[0] != "svc.go" {
			t.Fatalf("saturated range unresolved files = %#v, want [svc.go]", unresolved)
		}
	})

	t.Run("change contract range source", func(t *testing.T) {
		prediction, err := server.lowerRangeSource(exhaustedContext(), req)
		if err == nil || prediction != nil || !strings.Contains(err.Error(), "bounded range ownership is unavailable for: svc.go") {
			t.Fatalf("change-contract saturation = (%#v, %v)", prediction, err)
		}
	})

	t.Run("change contract workspace edit", func(t *testing.T) {
		editReq := mcpgo.CallToolRequest{}
		editReq.Params.Arguments = map[string]any{
			"source": "edit",
			"workspace_edit": buildSingleFileEdit(
				filepath.Join(server.indexer.RootPath(), "svc.go"),
				"package svc\n\nfunc Changed() {}\n",
			),
		}
		prediction, err := server.lowerEditSource(exhaustedContext(), editReq)
		if err == nil || prediction != nil || !strings.Contains(err.Error(), "bounded range ownership is unavailable for: svc.go") {
			t.Fatalf("workspace-edit saturation = (%#v, %v)", prediction, err)
		}
	})

	t.Run("ordinary unresolved remains tolerated", func(t *testing.T) {
		missingReq := mcpgo.CallToolRequest{}
		missingReq.Params.Arguments = map[string]any{
			"source": "ranges", "path": "missing.go", "start_line": float64(1),
		}
		prediction, err := server.lowerRangeSource(withLocalizationFileRequestBudget(context.Background()), missingReq)
		if err != nil || prediction == nil {
			t.Fatalf("ordinary unresolved range = (%#v, %v), want tolerated empty prediction", prediction, err)
		}
		if len(prediction.changedIDs) != 0 || len(prediction.changed) != 0 {
			t.Fatalf("ordinary unresolved range changed prediction: %#v", prediction)
		}
	})
}
