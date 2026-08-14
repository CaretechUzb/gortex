package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/graph"
)

type astTargetBoundedStore struct {
	graph.Store
	calls []boundedFileCall
	read  func(context.Context, string, graph.LocalizationNodeScope, int) (graph.BoundedNodeProjection, error)
}

func (store *astTargetBoundedStore) AllNodes() []*graph.Node {
	panic("AST enclosing lookup must not call AllNodes")
}

func (store *astTargetBoundedStore) FindFileNodesBounded(
	ctx context.Context,
	path string,
	scope graph.LocalizationNodeScope,
	limit int,
) (graph.BoundedNodeProjection, error) {
	store.calls = append(store.calls, boundedFileCall{path: path, scope: scope, limit: limit})
	return store.read(ctx, path, scope, limit)
}

type astTargetUnboundedStore struct{ graph.Store }

func (store *astTargetUnboundedStore) AllNodes() []*graph.Node {
	panic("missing bounded capability must fail closed without AllNodes")
}

func TestBuildFileSymbolIndexForTargetsPreservesOrderAndDeduplicates(t *testing.T) {
	probe := &astTargetBoundedStore{Store: graph.New()}
	probe.read = func(_ context.Context, _ string, _ graph.LocalizationNodeScope, _ int) (graph.BoundedNodeProjection, error) {
		return graph.BoundedNodeProjection{}, nil
	}
	server := &Server{graph: probe}
	targets := []astquery.Target{
		{GraphPath: "repo/b.ts", Language: "typescript"},
		{GraphPath: "repo/a.go", Language: "go"},
		{GraphPath: "repo/b.ts", Language: "tsx"},
		{GraphPath: ""},
		{GraphPath: "repo/c.py", Language: "python"},
	}

	server.buildFileSymbolIndexForTargetsContext(context.Background(), targets)

	want := []string{"repo/b.ts", "repo/a.go", "repo/c.py"}
	if len(probe.calls) != len(want) {
		t.Fatalf("bounded calls = %#v, want %v", probe.calls, want)
	}
	for index, path := range want {
		if probe.calls[index].path != path || probe.calls[index].limit != localizationFileNodeLimit {
			t.Fatalf("bounded call %d = %#v, want path=%q limit=%d", index, probe.calls[index], path, localizationFileNodeLimit)
		}
	}
}

func TestBuildFileSymbolIndexForTargetsSharesRequestBudget(t *testing.T) {
	probe := &astTargetBoundedStore{Store: graph.New()}
	probe.read = func(_ context.Context, path string, _ graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
		return graph.BoundedNodeProjection{
			Nodes: []*graph.Node{{
				ID: path + "::owner", Name: "owner", Kind: graph.KindFunction,
				FilePath: path, StartLine: 1, EndLine: 2,
			}},
			Total: limit,
		}, nil
	}
	server := &Server{graph: probe}
	ctx := withLocalizationFileRequestBudget(context.Background())
	first := []astquery.Target{{GraphPath: "repo/a.go"}, {GraphPath: "repo/b.go"}, {GraphPath: "repo/c.go"}}
	second := []astquery.Target{{GraphPath: "repo/d.go"}, {GraphPath: "repo/e.go"}}

	server.buildFileSymbolIndexForTargetsContext(ctx, first)
	indexes := server.buildFileSymbolIndexForTargetsContext(ctx, second)

	if len(probe.calls) != localizationFileRequestLimit/localizationFileNodeLimit {
		t.Fatalf("bounded calls = %d, want shared request cap of %d", len(probe.calls), localizationFileRequestLimit/localizationFileNodeLimit)
	}
	if probe.calls[3].path != "repo/d.go" {
		t.Fatalf("last budgeted call = %#v, want repo/d.go", probe.calls[3])
	}
	if index := indexes["repo/e.go"]; index == nil || !index.saturated {
		t.Fatalf("budget-exhausted target did not fail closed: %#v", index)
	}
}

func TestBuildFileSymbolIndexForTargetsFailsClosedWithoutFullScan(t *testing.T) {
	t.Run("saturated", func(t *testing.T) {
		const path = "repo/dense.go"
		probe := &astTargetBoundedStore{Store: graph.New()}
		probe.read = func(_ context.Context, _ string, _ graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
			return graph.BoundedNodeProjection{
				Nodes: []*graph.Node{{
					ID: path + "::wrong", Name: "wrong", Kind: graph.KindFunction,
					FilePath: path, StartLine: 1, EndLine: 100,
				}},
				Total: limit, Truncated: true,
			}, nil
		}
		server := &Server{graph: probe}
		indexes := server.buildFileSymbolIndexForTargetsContext(context.Background(), []astquery.Target{{GraphPath: path}})
		if id, name := indexes[path].find(50); id != "" || name != "" || !indexes[path].saturated {
			t.Fatalf("saturated owner = (%q, %q, %#v), want fail closed", id, name, indexes[path])
		}
	})

	t.Run("missing capability", func(t *testing.T) {
		const path = "repo/unsupported.go"
		server := &Server{graph: &astTargetUnboundedStore{Store: graph.New()}}
		indexes := server.buildFileSymbolIndexForTargetsContext(context.Background(), []astquery.Target{{GraphPath: path}})
		if index := indexes[path]; index == nil || !index.saturated {
			t.Fatalf("unsupported base reader did not fail closed: %#v", index)
		}
	})
}

func TestBuildFileSymbolIndexForTargetsReadsBaseInsteadOfOverlay(t *testing.T) {
	const path = "repo/service.go"
	baseOwner := &graph.Node{
		ID: path + "::base", Name: "base", Kind: graph.KindFunction,
		FilePath: path, StartLine: 1, EndLine: 20,
	}
	overlayOwner := &graph.Node{
		ID: path + "::overlay", Name: "overlay", Kind: graph.KindFunction,
		FilePath: path, StartLine: 1, EndLine: 20,
	}
	probe := &astTargetBoundedStore{Store: graph.New()}
	probe.read = func(_ context.Context, gotPath string, _ graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
		if gotPath != path || limit != localizationFileNodeLimit {
			return graph.BoundedNodeProjection{}, fmt.Errorf("unexpected base read %q limit %d", gotPath, limit)
		}
		return graph.BoundedNodeProjection{Nodes: []*graph.Node{baseOwner}, Total: 1}, nil
	}
	layer := graph.NewOverlayLayer()
	layer.MarkFile(path, false)
	layer.AddNode(path, overlayOwner)
	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(probe, layer))
	server := &Server{graph: probe}

	indexes := server.buildFileSymbolIndexForTargetsContext(ctx, []astquery.Target{{GraphPath: path}})
	id, name := indexes[path].find(10)
	if id != baseOwner.ID || name != baseOwner.Name {
		t.Fatalf("AST target owner = (%q, %q), want durable base owner (%q, %q)", id, name, baseOwner.ID, baseOwner.Name)
	}
	if len(probe.calls) != 1 {
		t.Fatalf("base projection calls = %d, want one", len(probe.calls))
	}
}
