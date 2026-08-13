package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

type boundedFileCall struct {
	path  string
	scope graph.LocalizationNodeScope
	limit int
}

type boundedFileStoreProbe struct {
	graph.Store
	calls []boundedFileCall
	read  func(context.Context, string, graph.LocalizationNodeScope, int) (graph.BoundedNodeProjection, error)
}

func (store *boundedFileStoreProbe) FindFileNodesBounded(
	ctx context.Context,
	path string,
	scope graph.LocalizationNodeScope,
	limit int,
) (graph.BoundedNodeProjection, error) {
	store.calls = append(store.calls, boundedFileCall{path: path, scope: scope, limit: limit})
	return store.read(ctx, path, scope, limit)
}

func TestBoundedFileSymbolIndexFailsClosedOnSaturationAndPushesScope(t *testing.T) {
	const path = "repo/dense_test.go"
	probe := &boundedFileStoreProbe{Store: graph.New()}
	probe.read = func(_ context.Context, gotPath string, scope graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
		if gotPath != path || limit != localizationFileNodeLimit {
			t.Fatalf("bounded call = (%q, %d), want (%q, %d)", gotPath, limit, path, localizationFileNodeLimit)
		}
		if scope.WorkspaceID != "workspace" || scope.ProjectID != "project" || !scope.RepoAllow["repo"] {
			t.Fatalf("scope was not pushed before cap: %#v", scope)
		}
		if scope.ExcludeTests {
			t.Fatal("file localization excluded test declarations")
		}
		for _, kind := range localizationFileIndexKinds {
			if !scope.Kinds[kind] {
				t.Fatalf("kind %q missing from storage projection: %#v", kind, scope.Kinds)
			}
		}
		owner := &graph.Node{ID: path + "::owner", Name: "owner", Kind: graph.KindFunction, FilePath: path, StartLine: 1, EndLine: 100}
		return graph.BoundedNodeProjection{Nodes: []*graph.Node{owner}, Total: limit + 1, Truncated: true}, nil
	}
	server := &Server{graph: probe}
	indexes := server.buildFileSymbolIndexForOrderedPathsScopedContext(
		context.Background(), []string{path},
		query.QueryOptions{WorkspaceID: "workspace", ProjectID: "project", RepoAllow: map[string]bool{"repo": true}},
	)
	if index := indexes[path]; index == nil || !index.saturated || index.smallestEnclosing(10) != nil {
		t.Fatalf("saturated file admitted an owner: %#v", index)
	}
}

func TestBoundedFileSymbolIndexStopsAtRequestBudget(t *testing.T) {
	probe := &boundedFileStoreProbe{Store: graph.New()}
	probe.read = func(_ context.Context, path string, _ graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
		nodes := make([]*graph.Node, limit)
		for index := range nodes {
			nodes[index] = &graph.Node{
				ID: fmt.Sprintf("%s::owner-%04d", path, index), Name: "owner",
				Kind: graph.KindFunction, FilePath: path, StartLine: index + 1, EndLine: index + 1,
			}
		}
		return graph.BoundedNodeProjection{Nodes: nodes, Total: len(nodes)}, nil
	}
	server := &Server{graph: probe}
	paths := []string{"repo/a.go", "repo/b.go", "repo/c.go", "repo/d.go", "repo/e.go"}
	indexes := server.buildFileSymbolIndexForOrderedPathsScopedContext(context.Background(), paths, query.QueryOptions{})
	if len(probe.calls) != localizationFileRequestLimit/localizationFileNodeLimit {
		t.Fatalf("bounded calls = %d, want request budget to stop after %d", len(probe.calls), localizationFileRequestLimit/localizationFileNodeLimit)
	}
	if index := indexes[paths[4]]; index == nil || !index.saturated {
		t.Fatalf("budget-exhausted path was not failed closed: %#v", index)
	}
}

type unboundedFileStore struct{ graph.Store }

func TestBoundedFileSymbolIndexFailsClosedWithoutCapabilityAndOnCancellation(t *testing.T) {
	path := "repo/handler.go"
	missing := (&Server{graph: &unboundedFileStore{Store: graph.New()}}).buildFileSymbolIndexForPaths(
		map[string]struct{}{path: {}},
	)
	if index := missing[path]; index == nil || !index.saturated {
		t.Fatalf("missing capability did not fail closed: %#v", index)
	}

	probe := &boundedFileStoreProbe{Store: graph.New()}
	probe.read = func(context.Context, string, graph.LocalizationNodeScope, int) (graph.BoundedNodeProjection, error) {
		t.Fatal("cancelled request reached storage")
		return graph.BoundedNodeProjection{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := (&Server{graph: probe}).buildFileSymbolIndexForOrderedPathsScopedContext(ctx, []string{path}, query.QueryOptions{})
	if index := cancelled[path]; index == nil || !index.saturated || len(probe.calls) != 0 {
		t.Fatalf("cancelled lookup = %#v, calls=%d", index, len(probe.calls))
	}
}

func TestBoundedFileSymbolIndexHonorsOverlayReplacementAndTombstone(t *testing.T) {
	const path = "repo/handler.go"
	base := graph.New()
	base.AddNode(&graph.Node{ID: path + "::old", Name: "old", Kind: graph.KindFunction, FilePath: path, StartLine: 1, EndLine: 20})
	server := &Server{graph: base}

	replacement := graph.NewOverlayLayer()
	replacement.MarkFile(path, false)
	replacement.AddNode(path, &graph.Node{ID: path + "::new", Name: "new", Kind: graph.KindFunction, FilePath: path, StartLine: 1, EndLine: 20})
	replacementCtx := WithOverlayView(context.Background(), graph.NewOverlaidView(base, replacement))
	indexes := server.buildFileSymbolIndexForPathsScopedContext(replacementCtx, map[string]struct{}{path: {}}, query.QueryOptions{})
	if owner := indexes[path].smallestEnclosing(10); owner == nil || owner.Name != "new" {
		t.Fatalf("overlay replacement owner = %#v, want new", owner)
	}

	tombstone := graph.NewOverlayLayer()
	tombstone.MarkFile(path, true)
	tombstoneCtx := WithOverlayView(context.Background(), graph.NewOverlaidView(base, tombstone))
	indexes = server.buildFileSymbolIndexForPathsScopedContext(tombstoneCtx, map[string]struct{}{path: {}}, query.QueryOptions{})
	if index := indexes[path]; index != nil {
		t.Fatalf("overlay tombstone leaked a file index: %#v", index)
	}
}

func TestSaturatedFileSymbolIndexCannotPromoteRange(t *testing.T) {
	index := &fileSymbolIndex{
		saturated: true,
		syms:      []*graph.Node{{ID: "repo/a.go::wrong", Kind: graph.KindFunction, FilePath: "repo/a.go", StartLine: 1, EndLine: 20}},
	}
	if got := exploreSourceRangeDefinitions(index, 10, 10); len(got) != 0 {
		t.Fatalf("saturated range promoted omitted candidate: %#v", got)
	}
}
