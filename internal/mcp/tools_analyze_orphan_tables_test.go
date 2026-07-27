package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeOrphanTables(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "orphan_tables"
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = args
	res, err := srv.handleAnalyze(context.Background(), req)
	if err != nil {
		t.Fatalf("handleAnalyze: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %+v", res.Content)
	}
	textBlock := res.Content[0].(mcplib.TextContent)
	var out map[string]any
	if err := json.Unmarshal([]byte(textBlock.Text), &out); err != nil {
		t.Fatalf("json: %v\n%s", err, textBlock.Text)
	}
	return out
}

// addTable + addQuery + addMigration are tiny helpers that mirror the
// shape the indexer produces. Kept inside the test so it doesn't grow
// production-side scaffolding.
func addTable(g graph.Store, id, table, dialect string) {
	g.AddNode(&graph.Node{
		ID:   id,
		Kind: graph.KindTable,
		Name: table,
		Meta: map[string]any{
			"table":   table,
			"dialect": dialect,
		},
	})
}

func addQueryEdge(g graph.Store, fromID, toID string) {
	g.AddEdge(&graph.Edge{
		From: fromID,
		To:   toID,
		Kind: graph.EdgeQueries,
	})
}

func addMigrationEdge(g graph.Store, fromID, toID string) {
	g.AddEdge(&graph.Edge{
		From: fromID,
		To:   toID,
		Kind: graph.EdgeProvides,
	})
}

func TestAnalyzeOrphanTables_TablesQueriedButNotProvided(t *testing.T) {
	srv, _ := setupTestServer(t)
	// users — has both a migration and queries → not orphan.
	addTable(srv.graph, "db::generic::users", "users", "generic")
	addMigrationEdge(srv.graph, "migration::db/init.sql", "db::generic::users")
	addQueryEdge(srv.graph, "pkg/users.go::List", "db::generic::users")

	// sessions — queried but no migration → orphan.
	addTable(srv.graph, "db::generic::sessions", "sessions", "generic")
	addQueryEdge(srv.graph, "pkg/auth.go::Get", "db::generic::sessions")
	addQueryEdge(srv.graph, "pkg/auth.go::Set", "db::generic::sessions")

	// audit_log — declared by migration, no queries → not orphan
	// (different problem; this analyzer doesn't surface it).
	addTable(srv.graph, "db::generic::audit_log", "audit_log", "generic")
	addMigrationEdge(srv.graph, "migration::db/init.sql", "db::generic::audit_log")

	out := callAnalyzeOrphanTables(t, srv, map[string]any{})
	rows, _ := out["orphans"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 orphan (sessions), got %d: %+v", len(rows), rows)
	}
	row := rows[0].(map[string]any)
	if row["table"] != "sessions" {
		t.Errorf("expected sessions, got %v", row["table"])
	}
	if got, _ := row["query_count"].(float64); got != 2 {
		t.Errorf("query_count = %v, want 2", got)
	}
}

func TestAnalyzeOrphanTables_SortedByQueryCount(t *testing.T) {
	srv, _ := setupTestServer(t)
	addTable(srv.graph, "db::generic::low", "low", "generic")
	addQueryEdge(srv.graph, "f.go::A", "db::generic::low")
	addTable(srv.graph, "db::generic::high", "high", "generic")
	addQueryEdge(srv.graph, "f.go::A", "db::generic::high")
	addQueryEdge(srv.graph, "f.go::B", "db::generic::high")
	addQueryEdge(srv.graph, "f.go::C", "db::generic::high")

	out := callAnalyzeOrphanTables(t, srv, map[string]any{})
	rows, _ := out["orphans"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 orphans, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["table"] != "high" {
		t.Errorf("expected high (3 queries) first, got %v", first["table"])
	}
}

func TestAnalyzeOrphanTables_NoOrphansEmpty(t *testing.T) {
	srv, _ := setupTestServer(t)
	addTable(srv.graph, "db::generic::users", "users", "generic")
	addMigrationEdge(srv.graph, "migration::a.sql", "db::generic::users")
	addQueryEdge(srv.graph, "f.go::Q", "db::generic::users")

	out := callAnalyzeOrphanTables(t, srv, map[string]any{})
	if got, _ := out["total"].(float64); got != 0 {
		t.Errorf("expected zero orphans, got total=%v", got)
	}
}

func TestAnalyzeOrphanTables_TableWithoutQueriesIsNotOrphan(t *testing.T) {
	// A table emitted by a migration but never queried is a
	// different problem (orphan migration). This analyzer doesn't
	// surface it — we want "queried but not declared", not "declared
	// but not queried".
	srv, _ := setupTestServer(t)
	addTable(srv.graph, "db::generic::unused", "unused", "generic")
	addMigrationEdge(srv.graph, "migration::a.sql", "db::generic::unused")

	out := callAnalyzeOrphanTables(t, srv, map[string]any{})
	if got, _ := out["total"].(float64); got != 0 {
		t.Errorf("declared-but-unused table must not appear as orphan, got %v", got)
	}
}

// A zero result only means "nothing to clean up" when there was
// something to cross-reference. With no query edges in the graph both
// table analyzers are structurally incapable of returning a row, so the
// response has to say the layer is empty rather than let an agent read
// the 0 as an all-clear.
func TestAnalyzeOrphanTables_NoQueryEdgesFlagsVacuousResult(t *testing.T) {
	srv, _ := setupTestServer(t)
	addTable(srv.graph, "db::generic::users", "users", "generic")
	addMigrationEdge(srv.graph, "migration::a.sql", "db::generic::users")

	out := callAnalyzeOrphanTables(t, srv, map[string]any{})
	if got, _ := out["total"].(float64); got != 0 {
		t.Fatalf("expected zero orphans, got %v", got)
	}
	if got, _ := out["query_edges"].(float64); got != 0 {
		t.Errorf("query_edges = %v, want 0", got)
	}
	note, _ := out["note"].(string)
	if note == "" {
		t.Error("empty result with zero query edges must carry a not-meaningful note")
	}
}

func TestAnalyzeOrphanTables_QueryEdgesPresentNoNote(t *testing.T) {
	srv, _ := setupTestServer(t)
	addTable(srv.graph, "db::generic::users", "users", "generic")
	addMigrationEdge(srv.graph, "migration::a.sql", "db::generic::users")
	addQueryEdge(srv.graph, "f.go::Q", "db::generic::users")

	out := callAnalyzeOrphanTables(t, srv, map[string]any{})
	if got, _ := out["query_edges"].(float64); got != 1 {
		t.Errorf("query_edges = %v, want 1", got)
	}
	if note, _ := out["note"].(string); note != "" {
		t.Errorf("a computable empty result must not be flagged: %q", note)
	}
}

func TestAnalyzeUnreferencedTables_NoQueryEdgesFlagsVacuousResult(t *testing.T) {
	srv, _ := setupTestServer(t)
	addTable(srv.graph, "db::generic::users", "users", "generic")
	addMigrationEdge(srv.graph, "migration::a.sql", "db::generic::users")

	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{"kind": "unreferenced_tables"}
	res, err := srv.handleAnalyze(context.Background(), req)
	if err != nil {
		t.Fatalf("handleAnalyze: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out); err != nil {
		t.Fatalf("json: %v", err)
	}
	// Every provided table looks unreferenced when no query edges exist,
	// so the rows are present but not trustworthy on their own.
	if got, _ := out["total"].(float64); got != 1 {
		t.Fatalf("expected the provided table to be listed, got %v", got)
	}
	if note, _ := out["note"].(string); note == "" {
		t.Error("result computed against zero query edges must carry a note")
	}
}
