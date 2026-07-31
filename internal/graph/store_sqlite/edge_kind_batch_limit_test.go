package store_sqlite

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestScanEdgesByKindsBatchedCapsRequestedPageSize(t *testing.T) {
	store := openEdgeKindBatchTestStore(t)
	edges := make([]*graph.Edge, 0, maxEdgeKindScanBatch+1)
	for i := 0; i < maxEdgeKindScanBatch+1; i++ {
		edges = append(edges, &graph.Edge{
			From: fmt.Sprintf("from-%05d", i), To: fmt.Sprintf("to-%05d", i),
			Kind: graph.EdgeReads, FilePath: "repo/file.go", Line: i + 1,
		})
	}
	store.AddBatch(nil, edges)

	total := 0
	maxPage := 0
	store.ScanEdgesByKindsBatched([]graph.EdgeKind{graph.EdgeReads}, maxEdgeKindScanBatch*2, func(page []*graph.Edge) bool {
		total += len(page)
		if len(page) > maxPage {
			maxPage = len(page)
		}
		return true
	})
	if total != len(edges) {
		t.Fatalf("visited %d edges, want %d", total, len(edges))
	}
	if maxPage != maxEdgeKindScanBatch {
		t.Fatalf("largest page = %d, want %d", maxPage, maxEdgeKindScanBatch)
	}
}
