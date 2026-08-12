package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestValidateFacadeContainerFieldsRejectsUnknownNestedFields(t *testing.T) {
	schema := facadeContainerValidationSchema()

	for _, test := range []struct {
		name      string
		input     map[string]any
		wantField string
		wantHint  string
	}{
		{
			name:      "change detect source repo path",
			input:     map[string]any{"source": map[string]any{"repo_path": `C:\work\other-repo`}},
			wantField: "source.repo_path",
			wantHint:  "options.repo",
		},
		{
			name:      "review run source repo",
			input:     map[string]any{"source": map[string]any{"repo": "other-repo"}},
			wantField: "source.repo",
			wantHint:  "options.repo",
		},
		{
			name:      "unknown source field",
			input:     map[string]any{"source": map[string]any{"mystery": true}},
			wantField: "source.mystery",
			wantHint:  "scope",
		},
		{
			name:      "unknown options field",
			input:     map[string]any{"options": map[string]any{"repo_path": "/work/repo"}},
			wantField: "options.repo_path",
			wantHint:  "options.repo",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := validateFacadeContainerFields(test.input, schema)
			require.NotNil(t, result)
			require.True(t, result.IsError)
			text := toolResultText(result)
			require.Contains(t, text, test.wantField)
			require.Contains(t, text, test.wantHint)
		})
	}
}

func TestValidateFacadeContainerFieldsAcceptsPublishedAndOpenFields(t *testing.T) {
	schema := facadeContainerValidationSchema()
	for _, repo := range []string{`C:\work\tracked-repo`, "tracked-prefix"} {
		require.Nil(t, validateFacadeContainerFields(map[string]any{
			"source":  map[string]any{"scope": "unstaged"},
			"options": map[string]any{"repo": repo},
		}, schema))
	}
	require.Nil(t, validateFacadeContainerFields(map[string]any{
		"arguments": map[string]any{"provider_specific": true},
	}, schema), "additionalProperties=true must remain open")
}

func TestFacadeRejectsUnknownNestedFieldsBeforeLegacyDispatch(t *testing.T) {
	for _, test := range []struct {
		facade    string
		operation string
		legacy    string
	}{
		{facade: "change", operation: "detect", legacy: "detect_changes"},
		{facade: "review", operation: "run", legacy: "review"},
	} {
		t.Run(test.facade+"."+test.operation, func(t *testing.T) {
			srv := &Server{facades: newFacadeRegistry()}
			called := false
			tool := mcpgo.NewTool(test.legacy,
				mcpgo.WithString("scope"),
				mcpgo.WithString("base_ref"),
				mcpgo.WithString("base"),
				mcpgo.WithString("diff"),
				mcpgo.WithString("repo"),
			)
			srv.facades.capture(tool, func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				called = true
				return mcpgo.NewToolResultJSON(req.GetArguments())
			})

			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{
				"operation": test.operation,
				"source":    map[string]any{"repo_path": `C:\work\other-repo`},
			}
			result, err := srv.handleFacade(context.Background(), test.facade, req)
			require.NoError(t, err)
			require.True(t, result.IsError)
			require.False(t, called, "unknown selector-like fields must fail before legacy dispatch")
			var structured StructuredError
			require.NoError(t, json.Unmarshal([]byte(toolResultText(result)), &structured))
			require.Equal(t, ErrCodeInvalidArgument, structured.ErrorCode)
			require.Equal(t, "source.repo_path", structured.Data["field"])
			require.Equal(t, "options.repo", structured.Data["suggested_field"])
		})
	}
}

func TestFacadeForwardsSupportedRepositorySelectors(t *testing.T) {
	srv := &Server{facades: newFacadeRegistry()}
	tool := mcpgo.NewTool("detect_changes",
		mcpgo.WithString("scope"),
		mcpgo.WithString("base_ref"),
		mcpgo.WithString("repo"),
	)
	srv.facades.capture(tool, func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultJSON(req.GetArguments())
	})

	for _, repo := range []string{`C:\work\tracked-repo`, "tracked-prefix", `C:\work\untracked-repo`} {
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"operation": "detect",
			"source":    map[string]any{"scope": "unstaged"},
			"options":   map[string]any{"repo": repo},
		}
		result, err := srv.handleFacade(context.Background(), "change", req)
		require.NoError(t, err)
		require.False(t, result.IsError)
		require.Equal(t, repo, unmarshalResult(t, result)["repo"])
	}
}

func facadeContainerValidationSchema() map[string]any {
	closed := func(fields ...string) map[string]any {
		properties := make(map[string]any, len(fields))
		for _, field := range fields {
			properties[field] = map[string]any{"type": "string"}
		}
		return map[string]any{
			"type":                 "object",
			"properties":           properties,
			"additionalProperties": false,
		}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"source":    closed("scope"),
			"options":   closed("repo", "base_ref"),
			"output":    closed("summary_only"),
			"arguments": map[string]any{"type": "object", "additionalProperties": true},
		},
		"additionalProperties": false,
	}
}
