package store_sqlite

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestEdgesByNodeIDsAndKindsPlanUsesEndpointKindIndex is the plan lock for the
// kind-narrowed batch edge read.
//
// The narrowing only pays off if SQLite drives BOTH columns of the composite
// (endpoint, kind) index: seeking on the endpoint alone still walks a hub
// node's whole degree and merely discards the rows later, which is the cost
// the predicate exists to remove. This repo has a recorded incident of ANALYZE
// flipping a plan on the edges table, and the query deliberately carries no
// INDEXED BY hint — edges_by_to and edges_by_from are in bulkDroppableIndexes,
// so a hint would be a HARD ERROR during a cold-load window rather than a slow
// plan (bulk_load.go keeps nodes_by_qual live for exactly that reason). This
// test is what stands in for the hint.
func TestEdgesByNodeIDsAndKindsPlanUsesEndpointKindIndex(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	store.AddNode(&graph.Node{ID: "hub", Kind: graph.KindType, Name: "Hub", FilePath: "hub.go"})
	store.AddNode(&graph.Node{ID: "m", Kind: graph.KindMethod, Name: "M", FilePath: "hub.go"})
	store.AddEdge(&graph.Edge{From: "m", To: "hub", Kind: graph.EdgeMemberOf, FilePath: "hub.go", Line: 1})
	store.AddEdge(&graph.Edge{From: "hub", To: "m", Kind: graph.EdgeCalls, FilePath: "hub.go", Line: 2})

	for _, test := range []struct {
		name        string
		col         string
		index       string
		constraints string
	}{
		{name: "incoming", col: "to_id", index: "EDGES_BY_TO", constraints: "(TO_ID=? AND KIND=?)"},
		{name: "outgoing", col: "from_id", index: "EDGES_BY_FROM", constraints: "(FROM_ID=? AND KIND=?)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := edgesByEndpointKindsQuery(test.col, 1, 2)
			rows, err := store.db.Query("EXPLAIN QUERY PLAN "+query,
				"hub", string(graph.EdgeMemberOf), string(graph.EdgeParamOf), baseViewGeneration)
			if err != nil {
				t.Fatalf("explain: %v", err)
			}
			defer rows.Close()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatalf("scan plan: %v", err)
				}
				details = append(details, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("plan rows: %v", err)
			}
			plan := strings.ToUpper(strings.Join(details, "\n"))
			if strings.Contains(plan, "SCAN EDGES") {
				t.Fatalf("kind-narrowed edge read scans the table:\n%s", plan)
			}
			if !strings.Contains(plan, "USING INDEX "+test.index) || !strings.Contains(plan, test.constraints) {
				t.Fatalf("plan missed %s %s — the kind conjunct is not driving the index, "+
					"so a hub node's whole degree is still being read:\n%s",
					test.index, test.constraints, plan)
			}
		})
	}

	// The unfiltered shape stays what it always was: endpoint-only seek, no
	// kind conjunct. Pinned so the shared builder cannot start emitting a
	// kind clause for GetInEdgesByNodeIDs / GetOutEdgesByNodeIDs.
	if got := edgesByEndpointKindsQuery("to_id", 2, 0); strings.Contains(got, "kind IN") {
		t.Fatalf("unfiltered query grew a kind predicate: %s", got)
	}
}
