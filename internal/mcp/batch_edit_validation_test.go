package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func unmarshalBatchEditError(t *testing.T, result *mcpgo.CallToolResult) map[string]any {
	t.Helper()
	require.True(t, result.IsError)
	require.NotEmpty(t, result.Content)
	text, ok := result.Content[0].(mcpgo.TextContent)
	require.True(t, ok, "expected TextContent, got %T", result.Content[0])
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(text.Text), &payload))
	return payload
}

func TestParseBatchEditsInfersCompleteLegacyShapes(t *testing.T) {
	branches := batchEditItemsSchema()["oneOf"].([]any)
	fileRequired := branches[1].(map[string]any)["required"].([]any)
	require.NotContains(t, fileRequired, "op", "schema must admit the same complete legacy file shape as the runtime")

	structured, err := parseBatchEdits([]any{map[string]any{
		"path": "a.go", "old_string": "before", "new_string": "",
	}})
	require.NoError(t, err)
	require.Len(t, structured, 1)
	require.Equal(t, "edit_file", structured[0].Op)

	legacy, err := parseBatchEdits(`[{"id":"pkg/a.go::A","old_source":"before","new_source":""}]`)
	require.NoError(t, err)
	require.Len(t, legacy, 1)
	require.Equal(t, "edit_symbol", legacy[0].Op)
}

func TestParseBatchEditsRejectsUnknownOpInLegacyJSONString(t *testing.T) {
	_, err := parseBatchEdits(`[{"op":"replace_file","path":"a.go","old_string":"before","new_string":"after"}]`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "edits[0]")
	require.Contains(t, err.Error(), `unknown op "replace_file"`)
	require.Contains(t, err.Error(), "edit_file, edit_symbol")
}

func TestParseBatchEditsRejectsAmbiguousAndIncompleteShapes(t *testing.T) {
	for _, test := range []struct {
		name string
		item map[string]any
		want string
	}{
		{
			name: "mixed",
			item: map[string]any{
				"path": "a.go", "old_string": "before", "new_string": "after",
				"id": "pkg/a.go::A", "old_source": "before", "new_source": "after",
			},
			want: "mixes edit_file and edit_symbol fields",
		},
		{
			name: "incomplete-file",
			item: map[string]any{"old_string": "before", "new_string": "after"},
			want: "incomplete edit_file shape",
		},
		{
			name: "unrecognized-alias",
			item: map[string]any{"file": "a.go"},
			want: "does not match a supported batch edit shape",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseBatchEdits([]any{test.item})
			require.Error(t, err)
			require.Contains(t, err.Error(), "edits[0]")
			require.Contains(t, err.Error(), test.want)
			require.Contains(t, err.Error(), `{"op":"edit_file"`)
			require.Contains(t, err.Error(), `{"op":"edit_symbol"`)
		})
	}
}

func TestBatchEditUnknownDiscriminatorIsStructuredAndWritesNothing(t *testing.T) {
	srv, root := setupTestServer(t)
	path := filepath.Join(root, "batch-invalid-op.txt")
	require.NoError(t, os.WriteFile(path, []byte("alpha beta\n"), 0o644))

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"edits": []any{
			map[string]any{
				"op": "edit_file", "path": path,
				"old_string": "alpha", "new_string": "ALPHA",
			},
			map[string]any{
				"op": "replace_file", "path": path,
				"old_string": "beta", "new_string": "BETA",
			},
		},
	}
	result, err := srv.handleBatchEdit(context.Background(), req)
	require.NoError(t, err)
	require.True(t, result.IsError)
	payload := unmarshalBatchEditError(t, result)
	require.Equal(t, "invalid_argument", payload["error_code"])
	require.Contains(t, payload["message"], `unknown op "replace_file"`)
	data := payload["data"].(map[string]any)
	require.Equal(t, float64(1), data["item_index"])
	require.ElementsMatch(t, []any{"edit_file", "edit_symbol"}, data["accepted_values"])
	require.Len(t, data["accepted_shapes"], 2)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "alpha beta\n", string(content), "invalid item must reject the whole batch before the first write")
}

func TestFacadeEditBatchRejectsNestedFileSelector(t *testing.T) {
	srv, root := setupTestServer(t)
	srv.facades.capture(mcpgo.NewTool("batch_edit"), srv.handleBatchEdit)
	path := filepath.Join(root, "facade-invalid-shape.txt")
	require.NoError(t, os.WriteFile(path, []byte("before\n"), 0o644))

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"operation": "batch",
		"changes": []any{map[string]any{
			"target":     map[string]any{"file": path},
			"old_string": "before", "new_string": "after",
		}},
	}
	result, err := srv.handleFacade(context.Background(), "edit", req)
	require.NoError(t, err)
	require.True(t, result.IsError)
	payload := unmarshalBatchEditError(t, result)
	require.Equal(t, "invalid_argument", payload["error_code"])
	require.Contains(t, payload["message"], "edits[0]")
	require.Contains(t, payload["message"], "incomplete edit_file shape")

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "before\n", string(content))
}
