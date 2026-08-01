package goanalysis

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestDrainPendingAddsRechecksExistingNodeAndKeepsEdges(t *testing.T) {
	tests := []struct {
		name string
		open func(*testing.T) graph.Store
	}{
		{
			name: "memory",
			open: func(*testing.T) graph.Store { return graph.New() },
		},
		{
			name: "sqlite",
			open: func(t *testing.T) graph.Store {
				store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
				if err != nil {
					t.Fatalf("open SQLite store: %v", err)
				}
				t.Cleanup(func() {
					if err := store.Close(); err != nil {
						t.Errorf("close SQLite store: %v", err)
					}
				})
				return store
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := tt.open(t)
			const nodeID = "module::go:example.com/dep@v1.0.0"
			const ownerID = "ext::go:example.com/dep::Use"
			canonical := &graph.Node{
				ID:         nodeID,
				Kind:       graph.KindModule,
				Name:       "dep",
				QualName:   "example.com/dep",
				FilePath:   "repo/go.mod",
				Language:   "go",
				RepoPrefix: "repo",
			}
			owner := &graph.Node{
				ID:         ownerID,
				Kind:       graph.KindFunction,
				Name:       "Use",
				FilePath:   "repo/use.go",
				Language:   "go",
				RepoPrefix: "repo",
			}
			store.AddBatch([]*graph.Node{canonical, owner}, nil)

			staleSynthetic := &graph.Node{
				ID:       nodeID,
				Kind:     graph.KindModule,
				Name:     "dep@v1.0.0",
				FilePath: "external::go:example.com/dep",
				Language: "go",
			}
			moduleEdge := &graph.Edge{
				From:       ownerID,
				To:         nodeID,
				Kind:       graph.EdgeDependsOnModule,
				FilePath:   "repo/use.go",
				Line:       1,
				Confidence: 1,
			}
			attribution := &externalsAttribution{
				g:            store,
				pendingNodes: []*graph.Node{staleSynthetic},
				pendingEdges: []*graph.Edge{moduleEdge},
				nodesAdded:   1,
			}
			receipts, ok := store.(graph.MutationReceiptStore)
			if !ok {
				t.Fatal("test store does not expose mutation receipts")
			}
			token := receipts.BeginMutationReceipt()
			nodes, edges := attribution.drainPendingAdds()
			if len(nodes) != 0 {
				t.Fatalf("pending nodes=%d, want stale node filtered", len(nodes))
			}
			if len(edges) != 1 || edges[0] != moduleEdge {
				t.Fatalf("pending edges=%v, want module edge retained", edges)
			}
			if attribution.nodesAdded != 0 {
				t.Fatalf("nodesAdded=%d, want actual insert count 0", attribution.nodesAdded)
			}
			store.AddBatch(nodes, edges)
			receipt := receipts.EndMutationReceipt(token)
			if !receipt.Complete {
				t.Fatalf("receipt incomplete after reuse: %q", receipt.IncompleteReason)
			}

			got := store.GetNode(nodeID)
			if got == nil || got.FilePath != canonical.FilePath || got.QualName != canonical.QualName || got.RepoPrefix != canonical.RepoPrefix {
				t.Fatalf("canonical identity was replaced: %+v", got)
			}
			out := store.GetOutEdges(ownerID)
			if len(out) != 1 || out[0].To != nodeID || out[0].Kind != graph.EdgeDependsOnModule {
				t.Fatalf("module edge not applied: %+v", out)
			}
		})
	}
}

func TestDrainPendingAddsKeepsTrulyMissingNodes(t *testing.T) {
	store := graph.New()
	missing := &graph.Node{ID: "ext::go:fmt::Println", Kind: graph.KindFunction, Name: "Println"}
	attribution := &externalsAttribution{
		g:            store,
		pendingNodes: []*graph.Node{missing},
		nodesAdded:   1,
	}

	nodes, edges := attribution.drainPendingAdds()
	if len(nodes) != 1 || nodes[0] != missing {
		t.Fatalf("pending nodes=%v, want missing node retained", nodes)
	}
	if len(edges) != 0 {
		t.Fatalf("pending edges=%v, want none", edges)
	}
	if attribution.nodesAdded != 1 {
		t.Fatalf("nodesAdded=%d, want 1", attribution.nodesAdded)
	}
}
