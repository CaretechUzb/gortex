package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func TestSearchSymbolsCursorRecordsReturnedPageAndClearsOutOfRange(t *testing.T) {
	store := graph.New()
	store.AddBatch([]*graph.Node{
		{ID: "repo/a.go::CursorNeedleAlpha", Kind: graph.KindFunction, Name: "CursorNeedleAlpha", FilePath: "repo/a.go"},
		{ID: "repo/b.go::CursorNeedleBeta", Kind: graph.KindFunction, Name: "CursorNeedleBeta", FilePath: "repo/b.go"},
		{ID: "repo/c.go::CursorNeedleGamma", Kind: graph.KindFunction, Name: "CursorNeedleGamma", FilePath: "repo/c.go"},
	}, nil)
	server := NewServer(query.NewEngine(store), store, nil, nil, zap.NewNop(), nil)
	ctx := context.Background()

	callPage := func(cursor string) map[string]any {
		t.Helper()
		arguments := map[string]any{"query": "CursorNeedle", "limit": 1}
		if cursor != "" {
			arguments["cursor"] = cursor
		}
		request := mcplib.CallToolRequest{}
		request.Params.Name = "search_symbols"
		request.Params.Arguments = arguments
		result, err := findAndCallHandler(server, "search_symbols", ctx, request)
		if err != nil {
			t.Fatalf("search_symbols: %v", err)
		}
		if result.IsError {
			t.Fatalf("search_symbols returned an error: %#v", result.Content)
		}
		text, ok := result.Content[0].(mcplib.TextContent)
		if !ok {
			t.Fatalf("search_symbols content = %T, want text", result.Content[0])
		}
		var response map[string]any
		if err := json.Unmarshal([]byte(text.Text), &response); err != nil {
			t.Fatalf("decode search response: %v", err)
		}
		return response
	}

	assertRecordedPage := func(response map[string]any) string {
		t.Helper()
		rows, ok := response["results"].([]any)
		if !ok || len(rows) != 1 {
			t.Fatalf("results = %#v, want one row", response["results"])
		}
		row, ok := rows[0].(map[string]any)
		if !ok {
			t.Fatalf("result row = %T, want object", rows[0])
		}
		id, _ := row["id"].(string)
		if id == "" {
			t.Fatalf("result row has no id: %#v", row)
		}

		session := server.sessionFor(ctx)
		session.mu.Lock()
		recorded := append([]string(nil), session.lastSearch.returned...)
		session.mu.Unlock()
		if len(recorded) != 1 || recorded[0] != id {
			t.Fatalf("recorded page = %#v, response id = %q", recorded, id)
		}
		return id
	}

	firstID := assertRecordedPage(callPage(""))
	secondID := assertRecordedPage(callPage(encodeCursor(1)))
	if secondID == firstID {
		t.Fatalf("cursor page repeated first result %q", firstID)
	}

	response := callPage(encodeCursor(10_000))
	if raw := response["results"]; raw != nil {
		if rows, ok := raw.([]any); !ok || len(rows) != 0 {
			t.Fatalf("out-of-range results = %#v, want empty", raw)
		}
	}
	session := server.sessionFor(ctx)
	session.mu.Lock()
	state := session.lastSearch
	session.mu.Unlock()
	if state.query != "" || len(state.returned) != 0 || len(state.returnedIDs) != 0 || len(state.fileIDs) != 0 || len(state.consumed) != 0 || !state.at.IsZero() {
		t.Fatalf("out-of-range page retained attribution: %#v", state)
	}
}
