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

// A caller who invents a spelling for "operate on that repository" has stated
// the same intent as one who writes `repo`. Answering about the active
// repository instead is wrong for every spelling, so each must fail closed —
// and on a write, before anything is written.
func TestFacadeRefusesInventedRepositorySelectorsBeforeWrite(t *testing.T) {
	srv, root := setupTestServer(t)
	target := filepath.Join(root, "main.go")
	before, err := os.ReadFile(target)
	require.NoError(t, err)

	elsewhere := t.TempDir()
	for _, container := range []string{"options", "arguments", "source", "context", "guard", "output"} {
		for _, spelling := range []string{
			"repo", "repo_path", "repository", "repository_path",
			"repo_root", "repository_root", "repoPath", "repo_dir",
			"root", "cwd", "dir", "worktree", "base_repo",
			"workspace", "project",
		} {
			t.Run(container+"."+spelling, func(t *testing.T) {
				req := mcpgo.CallToolRequest{}
				req.Params.Name = "edit"
				req.Params.Arguments = map[string]any{
					"operation":   "file",
					"target":      map[string]any{"file": "main.go"},
					"match":       string(before),
					"replacement": string(before) + "\n// must not be written\n",
					container:     map[string]any{spelling: elsewhere},
				}
				result, err := srv.handleFacade(context.Background(), "edit", req)
				require.NoError(t, err)
				require.True(t, result.IsError, "edit.file must refuse %s.%s", container, spelling)

				after, err := os.ReadFile(target)
				require.NoError(t, err)
				require.Equal(t, before, after, "the active repository must be untouched")
			})
		}
	}
}

// The refusal must not cost a working capability: an operation that reads the
// selector still gets it, whichever container carried it.
func TestFacadeKeepsConsumedRepositorySelectors(t *testing.T) {
	srv, _ := setupTestServer(t)
	spec, ok := srv.facades.operation("change", "detect")
	require.True(t, ok)

	for _, container := range []string{"options", "source", "output", "arguments"} {
		t.Run(container, func(t *testing.T) {
			input := map[string]any{
				"operation": "detect",
				container:   map[string]any{"repo": "tracked-repo"},
			}
			require.Nil(t, srv.validateFacadeInput(spec, input))
			require.Equal(t, "tracked-repo", normalizeFacadeArguments(spec, input)["repo"])
		})
	}
}

// `path` and `scope` name a file or a working-tree scope far more often than a
// repository on this surface. Promoting them to target selectors would refuse
// working calls, so the exclusion is deliberate and pinned here.
func TestFacadeSelectorClassExcludesOverloadedNames(t *testing.T) {
	for _, field := range []string{"path", "scope", "file", "query", "symbol", "target"} {
		require.False(t, facadeRepositorySelectorLike(field),
			"%q addresses something other than a repository on this surface", field)
	}
	for _, field := range []string{"REPO", "Repo_Path", " repository "} {
		require.True(t, facadeRepositorySelectorLike(field),
			"%q is a repository selector whatever its casing or padding", field)
	}
}

// The friendly match/replacement pair belongs to the edit facade. Translating
// it everywhere silently dropped remember.edit_memory's replacement into a
// field edit_memory does not declare, so the memory was edited with no
// replacement text and the call still reported success.
func TestFacadeEditMemoryReceivesItsOwnReplacementVocabulary(t *testing.T) {
	srv, _ := setupTestServer(t)
	spec, ok := srv.facades.operation("remember", "edit_memory")
	require.True(t, ok)
	require.True(t, srv.legacyDeclaresField(spec.Legacy, "replacement"),
		"edit_memory speaks replacement natively; the premise of this test is that it must receive it")

	captured, ok := srv.facades.legacy(spec.Legacy)
	require.True(t, ok)
	var got map[string]any
	srv.facades.capture(captured.tool, func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		got = req.GetArguments()
		return mcpgo.NewToolResultText("{}"), nil
	})

	req := mcpgo.CallToolRequest{}
	req.Params.Name = "remember"
	req.Params.Arguments = map[string]any{
		"operation": "edit_memory",
		"arguments": map[string]any{"id": "mem-1", "pattern": "old", "replacement": "new"},
	}
	result, err := srv.handleFacade(context.Background(), "remember", req)
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", toolResultText(result))

	require.Equal(t, "new", got["replacement"], "the handler must receive the caller's replacement")
	require.NotContains(t, got, "new_string", "the edit facade's vocabulary must not leak here")
}

// The edit facade still translates its own published vocabulary.
func TestFacadeEditAliasTranslationMatchesPublishedVocabulary(t *testing.T) {
	for _, facade := range facadeToolNames() {
		properties := facadeToolDefinition(facade).InputSchema.Properties
		_, publishesMatch := properties["match"]
		_, publishesReplacement := properties["replacement"]
		require.Equal(t, publishesMatch && publishesReplacement, facadeTranslatesEditAliases(facade),
			"%s translates match/replacement only if it publishes them", facade)
	}

	srv, _ := setupTestServer(t)
	spec, ok := srv.facades.operation("edit", "file")
	require.True(t, ok)
	lowered := normalizeFacadeArguments(spec, map[string]any{
		"operation": "file", "match": "old", "replacement": "new",
	})
	require.Equal(t, "old", lowered["old_string"])
	require.Equal(t, "new", lowered["new_string"])
}

// A refusal names the operation and, where one exists, the selector to use.
func TestFacadeInventedSelectorRefusalIsActionable(t *testing.T) {
	srv, _ := setupTestServer(t)

	detect, ok := srv.facades.operation("change", "detect")
	require.True(t, ok)
	refused := srv.validateFacadeInput(detect, map[string]any{
		"operation": "detect",
		"options":   map[string]any{"repo_root": "/work/other"},
	})
	require.NotNil(t, refused)
	var suggested StructuredError
	require.NoError(t, json.Unmarshal([]byte(toolResultText(refused)), &suggested))
	require.Equal(t, "options.repo_root", suggested.Data["field"])
	require.Equal(t, "options.repo", suggested.Data["suggested_field"])

	file, ok := srv.facades.operation("edit", "file")
	require.True(t, ok)
	refusedWrite := srv.validateFacadeInput(file, map[string]any{
		"operation": "file",
		"options":   map[string]any{"workspace": "other"},
	})
	require.NotNil(t, refusedWrite)
	var explained StructuredError
	require.NoError(t, json.Unmarshal([]byte(toolResultText(refusedWrite)), &explained))
	require.Equal(t, "options.workspace", explained.Data["field"])
	require.Equal(t, "no_repository_selector", explained.Data["reason"])
	require.Contains(t, explained.Message, "workspace_admin.set_active_project")
}
