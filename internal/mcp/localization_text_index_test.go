package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/trigram"
)

func TestSearchTextEnrichmentAndCaptureShareOneBoundedFetch(t *testing.T) {
	const path = "repo/handler_test.go"
	backing := graph.New()
	owner := &graph.Node{
		ID: path + "::testOwner", Name: "testOwner", Kind: graph.KindFunction,
		FilePath: path, StartLine: 5, EndLine: 30, Meta: map[string]any{"is_test": true},
	}
	backing.AddNode(owner)
	probe := &boundedFileStoreProbe{Store: backing}
	probe.read = func(_ context.Context, gotPath string, scope graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
		if gotPath != path || limit != localizationFileNodeLimit || scope.ExcludeTests {
			t.Fatalf("bounded search-text fetch = (%q, %#v, %d)", gotPath, scope, limit)
		}
		return backing.FindFileNodesBounded(context.Background(), gotPath, scope, limit)
	}
	server := &Server{graph: probe}
	ctx := withLocalizationPermittedEvidenceCapture(context.Background(), 91)
	enriched, indexes := server.enrichTextMatchesContext(ctx, []trigram.Match{{Path: path, Line: 10, Text: "needle"}}, query.QueryOptions{})
	if len(enriched) != 1 || enriched[0].SymbolID != owner.ID {
		t.Fatalf("test-file owner enrichment = %#v", enriched)
	}
	server.captureLocalizationSearchText(ctx, enriched, indexes)
	if len(probe.calls) != 1 {
		t.Fatalf("enrichment + capture performed %d file fetches, want one", len(probe.calls))
	}
	rows, recorded := localizationEvidenceForPermittedCall(ctx, "search", "text", 91)
	if !recorded || len(rows) != 1 || rows[0].ID != owner.ID {
		t.Fatalf("shared-index capture = %#v, recorded=%v", rows, recorded)
	}
}

func TestLocalizationTextMatchDirectSymbolIDSkipsFileIndex(t *testing.T) {
	store := graph.New()
	node := &graph.Node{ID: "repo/direct.go::direct", Name: "direct", Kind: graph.KindFunction, FilePath: "repo/direct.go"}
	store.AddNode(node)
	server := &Server{graph: store}
	got, provenance := server.localizationTextMatchNode(
		context.Background(),
		enrichedTextMatch{Path: "repo/ignored.go", SymbolID: node.ID},
		map[string]*fileSymbolIndex{"repo/ignored.go": {saturated: true}},
	)
	if got != node || provenance != "permitted_search_text" {
		t.Fatalf("direct SymbolID = (%#v, %q), want unchanged typed lookup", got, provenance)
	}
}

func TestLocalizationTextMatchUsesWindowsPathAlias(t *testing.T) {
	if filepath.Separator == '/' {
		t.Skip("Windows graph path spelling uses a distinct separator only on Windows")
	}
	server := &Server{graph: graph.New()}
	matchPath := "repo/dir/handler.go"
	aliasPath := graphMatchPathKey(matchPath, true)
	owner := &graph.Node{ID: aliasPath + "::owner", Name: "owner", Kind: graph.KindFunction, FilePath: aliasPath, StartLine: 1, EndLine: 20}
	indexes := map[string]*fileSymbolIndex{aliasPath: {syms: []*graph.Node{owner}}}
	got, provenance := server.localizationTextMatchNode(context.Background(), enrichedTextMatch{Path: matchPath, Line: 10}, indexes)
	if got != owner || provenance != "permitted_search_text_owner" {
		t.Fatalf("Windows path alias = (%#v, %q), want owner", got, provenance)
	}
}

func TestLocalizationTextMatchFailsClosedForSaturatedExactPath(t *testing.T) {
	server := &Server{graph: graph.New()}
	path := "repo/dense.go"
	wrong := &graph.Node{ID: path + "::wide", Name: "wide", Kind: graph.KindFunction, FilePath: path, StartLine: 1, EndLine: 100}
	got, provenance := server.localizationTextMatchNode(
		context.Background(),
		enrichedTextMatch{Path: path, Line: 50},
		map[string]*fileSymbolIndex{path: {saturated: true, syms: []*graph.Node{wrong}, fileNode: &graph.Node{ID: path, Kind: graph.KindFile, FilePath: path}}},
	)
	if got != nil || provenance != "" {
		t.Fatalf("saturated path misattributed owner/file: (%#v, %q)", got, provenance)
	}
}
