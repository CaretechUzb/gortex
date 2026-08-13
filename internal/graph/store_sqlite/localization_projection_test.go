package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestFindNodesByNameBoundedCapsTenThousandHomonyms(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const homonyms = 10_000
	nodes := make([]*graph.Node, 0, homonyms+2)
	for index := 0; index < homonyms; index++ {
		nodes = append(nodes, &graph.Node{
			ID:          fmt.Sprintf("repo/src/handler-%05d.go::handle", index),
			Name:        "handle",
			Kind:        graph.KindFunction,
			FilePath:    fmt.Sprintf("repo/src/handler-%05d.go", index),
			RepoPrefix:  "repo",
			WorkspaceID: "workspace",
			ProjectID:   "project",
			Meta: map[string]any{
				"doc":             "payload that the localization projection must not hydrate",
				"search_doc":      "large search-only payload",
				"custom_metadata": stringsRepeatForProjectionTest("payload", 16),
			},
		})
	}
	// These rows prove every scope predicate is applied before COUNT and LIMIT.
	nodes = append(nodes,
		&graph.Node{ID: "foreign/src/f.go::handle", Name: "handle", Kind: graph.KindFunction, FilePath: "foreign/src/f.go", RepoPrefix: "foreign", WorkspaceID: "foreign"},
		&graph.Node{ID: "repo/src/value.go::handle", Name: "handle", Kind: graph.KindVariable, FilePath: "repo/src/value.go", RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project"},
	)
	store.BeginBulkLoad()
	store.AddBatch(nodes, nil)
	store.FlushBulk()

	page, err := store.FindNodesByNameBounded(
		context.Background(),
		"handle",
		graph.LocalizationNodeScope{
			WorkspaceID: "workspace",
			ProjectID:   "project",
			RepoAllow:   map[string]bool{"repo": true},
			Kinds:       map[graph.NodeKind]bool{graph.KindFunction: true},
		},
		8,
	)
	if err != nil {
		t.Fatalf("bounded lookup: %v", err)
	}
	if page.Total != 9 {
		t.Fatalf("total = %d, want threshold total 9", page.Total)
	}
	if !page.Truncated {
		t.Fatal("truncated = false, want LIMIT+1 sentinel saturation")
	}
	if len(page.Nodes) != 8 {
		t.Fatalf("nodes = %d, want hard cap 8", len(page.Nodes))
	}
	for index, node := range page.Nodes {
		want := fmt.Sprintf("repo/src/handler-%05d.go::handle", index)
		if node.ID != want {
			t.Fatalf("node[%d] = %q, want deterministic %q", index, node.ID, want)
		}
		if _, hydrated := node.Meta["doc"]; hydrated {
			t.Fatalf("node[%d] hydrated promoted doc payload", index)
		}
		if _, hydrated := node.Meta["custom_metadata"]; hydrated {
			t.Fatalf("node[%d] retained unrelated metadata", index)
		}
	}
}

func TestFindNodesByNameBoundedFindsProductionRowsBehindTests(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	nodes := make([]*graph.Node, 0, 42)
	for index := 0; index < 32; index++ {
		nodes = append(nodes, &graph.Node{
			ID: fmt.Sprintf("a-tests/%02d.go::handle", index), Name: "handle",
			Kind: graph.KindFunction, FilePath: fmt.Sprintf("a-tests/%02d.go", index),
			Meta: map[string]any{"is_test": true},
		})
	}
	for index := 0; index < 10; index++ {
		nodes = append(nodes, &graph.Node{
			ID: fmt.Sprintf("z-prod/%02d.go::handle", index), Name: "handle",
			Kind: graph.KindFunction, FilePath: fmt.Sprintf("z-prod/%02d.go", index),
		})
	}
	store.BeginBulkLoad()
	store.AddBatch(nodes, nil)
	store.FlushBulk()

	page, err := store.FindNodesByNameBounded(
		context.Background(), "handle", graph.LocalizationNodeScope{ExcludeTests: true}, 8,
	)
	if err != nil {
		t.Fatalf("bounded lookup: %v", err)
	}
	if len(page.Nodes) != 8 || page.Total != 9 || !page.Truncated {
		t.Fatalf("page = %#v, want eight production nodes and saturation sentinel", page)
	}
	for _, node := range page.Nodes {
		if len(node.FilePath) < len("z-prod/") || node.FilePath[:len("z-prod/")] != "z-prod/" {
			t.Fatalf("test declaration leaked into production page: %#v", node)
		}
	}
}

func TestFindNodesByNameBoundedPreservesTestClassification(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddNode(&graph.Node{
		ID: "repo/handler_test.go::handle", Name: "handle", Kind: graph.KindFunction,
		FilePath: "repo/handler_test.go", Meta: map[string]any{"is_test": true, "doc": "omit me"},
	})

	page, err := store.FindNodesByNameBounded(context.Background(), "handle", graph.LocalizationNodeScope{}, 1)
	if err != nil {
		t.Fatalf("bounded lookup: %v", err)
	}
	if len(page.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(page.Nodes))
	}
	if isTest, _ := page.Nodes[0].Meta["is_test"].(bool); !isTest {
		t.Fatalf("projected meta = %#v, want is_test classification", page.Nodes[0].Meta)
	}
	if _, hydrated := page.Nodes[0].Meta["doc"]; hydrated {
		t.Fatal("bounded projection hydrated doc")
	}
}

func TestFindNodesByNameBoundedHonorsCancellation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddNode(&graph.Node{ID: "repo/f.go::handle", Name: "handle", Kind: graph.KindFunction, FilePath: "repo/f.go"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	page, err := store.FindNodesByNameBounded(ctx, "handle", graph.LocalizationNodeScope{}, 8)
	if err == nil {
		t.Fatalf("error = nil with cancelled context; page = %#v", page)
	}
	if len(page.Nodes) != 0 || page.Total != 0 {
		t.Fatalf("cancelled lookup returned partial page %#v", page)
	}
}

func stringsRepeatForProjectionTest(value string, count int) string {
	out := ""
	for index := 0; index < count; index++ {
		out += value
	}
	return out
}
