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

func TestFacadeRepositoryValidationRejectsUnconsumedPathsFromRealRegistry(t *testing.T) {
	srv, _ := setupTestServer(t)

	for _, test := range []struct {
		name          string
		facade        string
		operation     string
		input         map[string]any
		wantField     string
		wantSuggested string
	}{
		{
			name: "change detect source repo path", facade: "change", operation: "detect",
			input:     map[string]any{"source": map[string]any{"repo_path": `C:\work\other-repo`}},
			wantField: "source.repo_path", wantSuggested: "options.repo",
		},
		{
			name: "top-level repo path", facade: "change", operation: "detect",
			input: map[string]any{"repo_path": `C:\work\other-repo`}, wantField: "repo_path",
		},
		{
			name: "context repo path", facade: "change", operation: "detect",
			input: map[string]any{"context": map[string]any{"repo_path": `C:\work\other-repo`}}, wantField: "context.repo_path",
		},
		{
			name: "guard repo path", facade: "change", operation: "detect",
			input: map[string]any{"guard": map[string]any{"repo_path": `C:\work\other-repo`}}, wantField: "guard.repo_path",
		},
		{
			name: "cold pr source repo path", facade: "pr", operation: "risk",
			input: map[string]any{"source": map[string]any{"repo_path": `C:\work\other-repo`}}, wantField: "source.repo_path",
			wantSuggested: "arguments.repo",
		},
		{
			name: "cold pr context repository", facade: "pr", operation: "list",
			input: map[string]any{"context": map[string]any{"repository": "other-repo"}}, wantField: "context.repository",
		},
		{
			name: "external write source repo path", facade: "publish_review", operation: "post",
			input: map[string]any{"source": map[string]any{"repo_path": `C:\work\other-repo`}}, wantField: "source.repo_path",
			wantSuggested: "arguments.repo",
		},
		{
			name: "read symbols injected baseline", facade: "read", operation: "symbols",
			input: map[string]any{"context": map[string]any{"repo_path": `C:\work\other-repo`}}, wantField: "context.repo_path",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec, ok := srv.facades.operation(test.facade, test.operation)
			require.True(t, ok, "real registry must contain %s.%s", test.facade, test.operation)
			test.input["operation"] = test.operation

			result := srv.validateFacadeInput(spec, test.input)
			require.NotNil(t, result)
			require.True(t, result.IsError)
			var structured StructuredError
			require.NoError(t, json.Unmarshal([]byte(toolResultText(result)), &structured))
			require.Equal(t, ErrCodeInvalidArgument, structured.ErrorCode)
			require.Equal(t, test.wantField, structured.Data["field"])
			if test.wantSuggested != "" {
				require.Equal(t, test.wantSuggested, structured.Data["suggested_field"])
			}
		})
	}
}

func TestFacadeRepositoryValidationPreservesConsumedCompatibilityFields(t *testing.T) {
	srv, _ := setupTestServer(t)

	for _, test := range []struct {
		name        string
		facade      string
		operation   string
		container   string
		fields      map[string]any
		wantLowered []string
	}{
		{
			name: "change source base ref and repo", facade: "change", operation: "detect", container: "source",
			fields: map[string]any{"base_ref": "HEAD", "repo": "tracked-repo"}, wantLowered: []string{"base_ref", "repo"},
		},
		{
			name: "change output repo", facade: "change", operation: "detect", container: "output",
			fields: map[string]any{"repo": "tracked-repo"}, wantLowered: []string{"repo"},
		},
		{
			name: "review source base ref", facade: "review", operation: "run", container: "source",
			fields: map[string]any{"base_ref": "HEAD"}, wantLowered: []string{"base_ref"},
		},
		{
			name: "change ranges source fields", facade: "change", operation: "ranges", container: "source",
			fields: map[string]any{"path": "main.go", "start_line": 1, "end_line": 2}, wantLowered: []string{"path", "start_line", "end_line"},
		},
		{
			name: "change contract source fields", facade: "change", operation: "contract", container: "source",
			fields: map[string]any{"lens": "api", "risk_gate": "strict"}, wantLowered: []string{"lens", "risk_gate"},
		},
		{
			name: "change simulate source keep", facade: "change", operation: "simulate", container: "source",
			fields: map[string]any{"keep": true}, wantLowered: []string{"keep"},
		},
		{
			name: "review critique prior review", facade: "review", operation: "critique", container: "source",
			fields: map[string]any{"prior_review": "review text"}, wantLowered: []string{"prior_review"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec, ok := srv.facades.operation(test.facade, test.operation)
			require.True(t, ok, "real registry must contain %s.%s", test.facade, test.operation)
			input := map[string]any{"operation": test.operation, test.container: test.fields}

			require.Nil(t, srv.validateFacadeInput(spec, input))
			normalized := normalizeFacadeArguments(spec, input)
			for _, field := range test.wantLowered {
				_, exists := normalized[field]
				require.True(t, exists, "%s must survive normalization", field)
				require.True(t, srv.legacyDeclaresField(spec.Legacy, field), "%s must be consumed by %s", field, spec.Legacy)
			}
		})
	}
}

func TestFacadeRepositoryValidationLeavesNonSelectorCompatibilityAlone(t *testing.T) {
	srv, _ := setupTestServer(t)
	spec, ok := srv.facades.operation("change", "detect")
	require.True(t, ok)
	require.Nil(t, srv.validateFacadeInput(spec, map[string]any{
		"operation": "detect",
		"source":    map[string]any{"base": "HEAD", "mystery": true},
	}))
}

// Most operations publish no repository selector at all. Refusing one with a
// bare "unknown field" leaves the caller retrying spellings that can never be
// accepted, so the refusal has to say that no selector exists and how to change
// scope instead.
func TestFacadeRepositoryValidationExplainsOperationsWithoutASelector(t *testing.T) {
	srv, _ := setupTestServer(t)

	spec, ok := srv.facades.operation("read", "file")
	require.True(t, ok)
	require.Empty(t, srv.facadePublicRepositoryField(spec), "read.file publishes no repository selector")

	result := srv.validateFacadeInput(spec, map[string]any{
		"operation": "file",
		"target":    map[string]any{"file": "main.go"},
		"options":   map[string]any{"repo": "other-repo"},
	})
	require.NotNil(t, result)
	require.True(t, result.IsError)

	var structured StructuredError
	require.NoError(t, json.Unmarshal([]byte(toolResultText(result)), &structured))
	require.Equal(t, ErrCodeInvalidArgument, structured.ErrorCode)
	require.Equal(t, "options.repo", structured.Data["field"])
	require.Equal(t, "no_repository_selector", structured.Data["reason"])
	require.NotContains(t, structured.Data, "suggested_field", "there is no selector to suggest")
	require.Contains(t, structured.Message, "read.file accepts no repository selector")
	require.Contains(t, structured.Message, "workspace_admin.set_active_project")

	// An operation that does publish one keeps pointing at it.
	detect, ok := srv.facades.operation("change", "detect")
	require.True(t, ok)
	rejected := srv.validateFacadeInput(detect, map[string]any{
		"operation": "detect",
		"source":    map[string]any{"repo_path": "/work/other-repo"},
	})
	require.NotNil(t, rejected)
	var suggested StructuredError
	require.NoError(t, json.Unmarshal([]byte(toolResultText(rejected)), &suggested))
	require.Equal(t, "options.repo", suggested.Data["suggested_field"])
	require.NotContains(t, suggested.Data, "reason")
}

func TestFacadeRepositoryValidationRejectsInvalidCanonicalSelectors(t *testing.T) {
	srv, _ := setupTestServer(t)

	for _, test := range []struct {
		name      string
		facade    string
		operation string
		input     map[string]any
		wantField string
	}{
		{
			name: "empty common selector", facade: "change", operation: "detect",
			input: map[string]any{"options": map[string]any{"repo": "  "}}, wantField: "options.repo",
		},
		{
			name: "non-string common selector", facade: "change", operation: "detect",
			input: map[string]any{"options": map[string]any{"repo": 42}}, wantField: "options.repo",
		},
		{
			name: "empty cold selector", facade: "pr", operation: "risk",
			input: map[string]any{"arguments": map[string]any{"repo": ""}}, wantField: "arguments.repo",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec, ok := srv.facades.operation(test.facade, test.operation)
			require.True(t, ok)
			test.input["operation"] = test.operation

			result := srv.validateFacadeInput(spec, test.input)
			require.NotNil(t, result)
			require.True(t, result.IsError)
			var structured StructuredError
			require.NoError(t, json.Unmarshal([]byte(toolResultText(result)), &structured))
			require.Equal(t, test.wantField, structured.Data["field"])
			require.Equal(t, "non-empty string", structured.Data["expected_type"])
		})
	}
}

func TestFacadeEditFileRejectsUnconsumedRepositoryBeforeWrite(t *testing.T) {
	srv, root := setupTestServer(t)
	target := filepath.Join(root, "main.go")
	before, err := os.ReadFile(target)
	require.NoError(t, err)

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "edit"
	req.Params.Arguments = map[string]any{
		"operation":   "file",
		"target":      map[string]any{"file": "main.go"},
		"match":       string(before),
		"replacement": string(before) + "\n// must not be written\n",
		"options":     map[string]any{"repo": t.TempDir()},
	}
	result, err := srv.handleFacade(context.Background(), "edit", req)
	require.NoError(t, err)
	require.True(t, result.IsError)

	after, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
