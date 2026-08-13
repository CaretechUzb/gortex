package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFacadeRepositoryValidationRejectsUnconsumedPathsFromRealRegistry(t *testing.T) {
	srv, _ := setupTestServer(t)

	for _, test := range []struct {
		name      string
		facade    string
		operation string
		input     map[string]any
		wantField string
	}{
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
		},
		{
			name: "cold pr context repository", facade: "pr", operation: "list",
			input: map[string]any{"context": map[string]any{"repository": "other-repo"}}, wantField: "context.repository",
		},
		{
			name: "external write source repo path", facade: "publish_review", operation: "post",
			input: map[string]any{"source": map[string]any{"repo_path": `C:\work\other-repo`}}, wantField: "source.repo_path",
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
