package mcp

import (
	"context"
	"strings"
	"testing"
)

// TestRunStaleCodeInspection guards the fix for the dead stale_code
// inspection: it once read last_authored as a bare string (always a miss,
// since blame writes a map) and gated on a never-written is_stale flag.
func TestRunStaleCodeInspection(t *testing.T) {
	srv, _ := setupTestServer(t)
	addBlameEnrichedNode(srv.graph, "f.go::Recent", "f.go", 1, "alice@x", "aaa", 30)
	addBlameEnrichedNode(srv.graph, "f.go::Stale", "f.go", 5, "bob@x", "bbb", 400)
	addBlameEnrichedNode(srv.graph, "f.go::Ancient", "f.go", 9, "carol@x", "ccc", 800)

	got := runStaleCodeInspection(context.Background(), srv, inspectionScope{})
	if len(got) != 2 {
		t.Fatalf("want 2 stale violations (Stale+Ancient, 365d default), got %d: %+v", len(got), got)
	}
	ids := map[string]bool{}
	for _, v := range got {
		ids[v.SymbolID] = true
		if v.Inspection != "stale_code" {
			t.Errorf("inspection = %q, want stale_code", v.Inspection)
		}
	}
	if !ids["f.go::Stale"] || !ids["f.go::Ancient"] || ids["f.go::Recent"] {
		t.Errorf("wrong stale set: %v", ids)
	}
	// The message must carry the author email read from the nested
	// last_authored map — proves the nested read, not a bare .(string).
	joined := ""
	for _, v := range got {
		joined += v.Message + "\n"
	}
	if !strings.Contains(joined, "bob@x") || !strings.Contains(joined, "carol@x") {
		t.Errorf("messages missing nested author email: %q", joined)
	}
}
