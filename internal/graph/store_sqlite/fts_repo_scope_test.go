package store_sqlite

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// floodedScopeStore seeds two repos: "noise" holds noiseCount symbols
// whose token bag is exactly the query token (they win BM25 outright),
// "app" holds one WidgetExtensions. This is the cross-repo flood shape:
// every unscoped head slot goes to noise rows, so any post-fetch scope
// filter starves no matter how far the caller over-fetches.
func floodedScopeStore(t *testing.T, noiseCount int) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "scope.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	appID := "app/widget.go::WidgetExtensions"
	s.AddNode(&graph.Node{
		ID: appID, Kind: graph.KindType, Name: "WidgetExtensions",
		FilePath: "app/widget.go", Language: "go", RepoPrefix: "app",
	})
	if err := s.UpsertSymbolFTS(appID, "widget extensions"); err != nil {
		t.Fatalf("UpsertSymbolFTS(app): %v", err)
	}

	for i := 0; i < noiseCount; i++ {
		id := fmt.Sprintf("noise/f%d.go::Extensions", i)
		s.AddNode(&graph.Node{
			ID: id, Kind: graph.KindFunction, Name: "Extensions",
			FilePath: fmt.Sprintf("noise/f%d.go", i), Language: "go", RepoPrefix: "noise",
		})
		if err := s.UpsertSymbolFTS(id, "extensions"); err != nil {
			t.Fatalf("UpsertSymbolFTS(noise %d): %v", i, err)
		}
	}
	return s
}

// TestSearchSymbolsRepoScoped_CrossRepoFloodDoesNotStarve: with 250
// out-of-scope rows outranking the one in-scope symbol, the scoped
// search must return that symbol — filtering inside the FTS query,
// not over a bounded head.
func TestSearchSymbolsRepoScoped_CrossRepoFloodDoesNotStarve(t *testing.T) {
	s := floodedScopeStore(t, 250)

	hits, err := s.SearchSymbolsRepoScoped("Extensions", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("SearchSymbolsRepoScoped: %v", err)
	}
	if len(hits) != 1 || hits[0].NodeID != "app/widget.go::WidgetExtensions" {
		t.Fatalf("scoped search = %+v, want exactly the app symbol", hits)
	}
}

// TestSearchSymbolsRepoScoped_ExactNameTierRespectsScope: the exact-name
// tier-0 short-circuit must not leak out-of-scope nodes; when every
// exact-name hit is out of scope it falls through to the scoped FTS.
func TestSearchSymbolsRepoScoped_ExactNameTierRespectsScope(t *testing.T) {
	s := floodedScopeStore(t, 30)

	// "Extensions" is the exact name of every noise node and no app node.
	hits, err := s.SearchSymbolsRepoScoped("Extensions", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("SearchSymbolsRepoScoped: %v", err)
	}
	for _, h := range hits {
		if h.NodeID == "" || h.NodeID[:4] == "nois" {
			t.Fatalf("exact-name tier leaked out-of-scope hit %q", h.NodeID)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("scoped search = %+v, want the single app symbol", hits)
	}
}

// TestSearchSymbolsRepoScoped_AdmitsUnownedNodes: rows with an empty
// repo_prefix (synthetic externals) pass every repo-narrow predicate
// in the codebase (see QueryOptions.ScopeAllows) — the FTS-level
// narrow must agree or scoped searches silently lose externals.
func TestSearchSymbolsRepoScoped_AdmitsUnownedNodes(t *testing.T) {
	s := floodedScopeStore(t, 3)

	extID := "dep::widgets/WidgetExtensions"
	s.AddNode(&graph.Node{
		ID: extID, Kind: graph.KindType, Name: "WidgetExtensions", Language: "go",
	})
	if err := s.UpsertSymbolFTS(extID, "widget extensions external"); err != nil {
		t.Fatalf("UpsertSymbolFTS(ext): %v", err)
	}

	hits, err := s.SearchSymbolsRepoScoped("widget", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("SearchSymbolsRepoScoped: %v", err)
	}
	found := map[string]bool{}
	for _, h := range hits {
		found[h.NodeID] = true
	}
	if !found["app/widget.go::WidgetExtensions"] || !found[extID] {
		t.Fatalf("scoped search = %+v, want the app symbol AND the unowned external", hits)
	}
}

// TestSearchSymbolsRepoScoped_EmptyScopeMatchesUnscoped: a nil allow
// list means no repo narrowing — identical behaviour to SearchSymbols.
func TestSearchSymbolsRepoScoped_EmptyScopeMatchesUnscoped(t *testing.T) {
	s := floodedScopeStore(t, 5)

	scoped, err := s.SearchSymbolsRepoScoped("Extensions", nil, 50)
	if err != nil {
		t.Fatalf("SearchSymbolsRepoScoped(nil): %v", err)
	}
	unscoped, err := s.SearchSymbols("Extensions", 50)
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	if len(scoped) != len(unscoped) {
		t.Fatalf("nil-scope hits = %d, unscoped = %d — must match", len(scoped), len(unscoped))
	}
}

// TestSearchSymbolBundlesRepoScoped_FiltersInQuery: the bundle variant
// inherits the scoped hit query and returns only in-scope bundles.
func TestSearchSymbolBundlesRepoScoped_FiltersInQuery(t *testing.T) {
	s := floodedScopeStore(t, 250)

	bundles, err := s.SearchSymbolBundlesRepoScoped("Extensions", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("SearchSymbolBundlesRepoScoped: %v", err)
	}
	if len(bundles) != 1 || bundles[0].Node == nil || bundles[0].Node.ID != "app/widget.go::WidgetExtensions" {
		t.Fatalf("scoped bundles = %+v, want exactly the app symbol", bundles)
	}
}
