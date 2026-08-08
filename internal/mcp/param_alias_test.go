package mcp

import (
	"context"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestReconcileArgKeys_RewritesConfidentAliases(t *testing.T) {
	real := toolParams{"id": "string", "detail": "string", "repo": "string"}
	cases := []struct {
		name    string
		args    map[string]any
		wantKey string // key that must be present after reconcile
		wantOld string // key that must be gone ("" = no rewrite expected)
	}{
		{"symbol alias for id", map[string]any{"symbol": "x"}, "id", "symbol"},
		{"node_id alias for id", map[string]any{"node_id": "x"}, "id", "node_id"},
		{"typo of detail", map[string]any{"detial": "brief"}, "detail", "detial"},
		{"already-correct key untouched", map[string]any{"id": "x"}, "id", ""},
		{"unknown noise key left alone", map[string]any{"id": "x", "zzz": 1}, "id", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reconcileArgKeys(tc.args, real)
			_, ok := tc.args[tc.wantKey]
			require.Truef(t, ok, "expected key %q present after reconcile", tc.wantKey)
			if tc.wantOld != "" {
				_, gone := tc.args[tc.wantOld]
				require.Falsef(t, gone, "aliased key %q must be removed", tc.wantOld)
			}
		})
	}
}

// TestReconcileArgKeys_KeepsExplicitCanonical confirms an alias never
// overwrites a canonical parameter the caller already supplied.
func TestReconcileArgKeys_KeepsExplicitCanonical(t *testing.T) {
	real := toolParams{"query": "string"}
	args := map[string]any{"query": "real", "search": "alias"}
	reconcileArgKeys(args, real)
	require.Equal(t, "real", args["query"], "an explicit canonical value must not be displaced by an alias")
	require.Contains(t, args, "search", "the alias key stays untouched when the canonical is already set")
}

// TestReconcileArgKeys_PatternAlias confirms `pattern` rewrites to `query`
// on search-side tools that have no real `pattern` parameter, and stays
// inert on tools (search_ast / grep_results / ctx_grep) where `pattern` is
// itself a real parameter.
func TestReconcileArgKeys_PatternAlias(t *testing.T) {
	t.Run("rewrites to query on search tools", func(t *testing.T) {
		// search_symbols / search_text expose `query`, not `pattern`.
		real := toolParams{"query": "string", "path": "string", "kind": "string"}
		args := map[string]any{"pattern": "validateToken"}
		reconcileArgKeys(args, real)
		require.Equal(t, "validateToken", args["query"], "pattern must alias to query")
		require.NotContains(t, args, "pattern", "the aliased pattern key must be removed")
	})
	t.Run("inert when pattern is a real parameter", func(t *testing.T) {
		// search_ast / grep_results expose a real `pattern` parameter; an
		// agent's `pattern` is valid input and must never be rewritten.
		real := toolParams{"pattern": "string", "query": "string"}
		args := map[string]any{"pattern": "(identifier) @match"}
		reconcileArgKeys(args, real)
		require.Equal(t, "(identifier) @match", args["pattern"], "a real pattern parameter must be left untouched")
		require.NotContains(t, args, "query", "no spurious query key must be synthesized")
	})
}

func TestLevenshtein(t *testing.T) {
	require.Equal(t, 0, levenshtein("query", "query"))
	require.Equal(t, 1, levenshtein("quary", "query"))
	require.Equal(t, 1, levenshtein("path", "paths"))
}

// TestReconcileToolParams_EndToEnd drives an aliased argument through the
// real wrapToolHandler chain and confirms the handler accepts it.
func TestReconcileToolParams_EndToEnd(t *testing.T) {
	srv, _ := setupTestServer(t)
	st := srv.MCPServer().GetTool("get_symbol")
	require.NotNil(t, st, "get_symbol must be a live tool")

	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_symbol"
	// "symbol" is a hallucinated alias for the real "id" parameter.
	req.Params.Arguments = map[string]any{"symbol": "main.go::helper"}

	res, err := st.Handler(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Falsef(t, res.IsError, "the aliased 'symbol' argument must be accepted as 'id'")
}

// TestReconcileArgKeys_NeverMovesContainersIntoScalars pins the guard that
// keeps hallucination tolerance from eating the compact surface's own
// vocabulary. `target` is an alias for `id` on tools that take a bare symbol
// ID, but an object-valued target is the public selector envelope: renaming
// it into a string parameter makes the handler read "" and answer a different
// question with no error — the exact fail-open shape the facade lowering
// exists to prevent.
func TestReconcileArgKeys_NeverMovesContainersIntoScalars(t *testing.T) {
	t.Run("object target is not renamed to a string id", func(t *testing.T) {
		real := toolParams{"id": "string", "kind": "string", "path": "string"}
		args := map[string]any{"kind": "impact", "target": map[string]any{"symbol": "pkg/x.go::Y"}}
		require.Empty(t, reconcileArgKeys(args, real))
		require.Contains(t, args, "target", "the selector envelope must survive for the facade to lower")
		require.NotContains(t, args, "id")
	})
	t.Run("scalar target still aliases to id", func(t *testing.T) {
		real := toolParams{"id": "string"}
		args := map[string]any{"target": "pkg/x.go::Y"}
		require.NotEmpty(t, reconcileArgKeys(args, real))
		require.Equal(t, "pkg/x.go::Y", args["id"])
	})
	t.Run("object context is not renamed to file content", func(t *testing.T) {
		// edit declares `content` (a file body) but not `context`. Without the
		// shape guard a read-shaped context envelope was rewritten into the
		// text an edit would write.
		real := toolParams{"content": "string", "target": "object", "operation": "string"}
		args := map[string]any{"operation": "write", "context": map[string]any{"window": 20}}
		require.Empty(t, reconcileArgKeys(args, real))
		require.NotContains(t, args, "content")
	})
	t.Run("array value is not renamed into a string parameter", func(t *testing.T) {
		real := toolParams{"id": "string"}
		args := map[string]any{"symbol": []any{"a", "b"}}
		require.Empty(t, reconcileArgKeys(args, real))
		require.NotContains(t, args, "id")
	})
	t.Run("object aliases into an object parameter", func(t *testing.T) {
		real := toolParams{"query": "object"}
		args := map[string]any{"search": map[string]any{"text": "x"}}
		require.NotEmpty(t, reconcileArgKeys(args, real))
		require.Equal(t, map[string]any{"text": "x"}, args["query"])
	})
}

// TestReconcileToolParams_PreservesFacadeEnvelope drives the real registered
// analyze tool: every public envelope container must reach the dispatcher
// unrenamed.
func TestReconcileToolParams_PreservesFacadeEnvelope(t *testing.T) {
	srv, _ := setupTestServer(t)
	for _, tool := range []string{"analyze", "edit", "read", "explore", "review"} {
		real := srv.toolParamNames(tool)
		if real == nil {
			continue
		}
		args := map[string]any{
			"target":  map[string]any{"symbol": "pkg/x.go::Y"},
			"options": map[string]any{"limit": 1},
			"source":  map[string]any{"scope": "unstaged"},
			"context": map[string]any{"window": 5},
			"guard":   map[string]any{"expected_occurrences": 1},
			"output":  map[string]any{"format": "json"},
			"to":      map[string]any{"symbol": "pkg/x.go::Z"},
		}
		require.Emptyf(t, reconcileArgKeys(args, real),
			"%s must not rewrite any public envelope container", tool)
		require.Lenf(t, args, 7, "%s lost an envelope container", tool)
	}
}

// TestReconcileArgKeys_NeverInvertsMeaning pins the guard against the typo
// matcher's worst failure: "include" and "exclude" are two substitutions
// apart, the same budget a real typo gets, so a caller's filter was accepted
// as its own negation. The handler then returns a well-formed result set that
// is close to the complement of the requested one, with nothing in the
// response saying a key was rewritten.
func TestReconcileArgKeys_NeverInvertsMeaning(t *testing.T) {
	cases := []struct {
		name      string
		real      toolParams
		key       string
		mustNotBe string
	}{
		{"include vs exclude", toolParams{"exclude_tests": "boolean", "id": "string"}, "include_tests", "exclude_tests"},
		{"exclude vs include", toolParams{"include_speculative": "boolean", "id": "string"}, "exclude_speculative", "include_speculative"},
		{"superseded memories", toolParams{"include_superseded": "boolean"}, "exclude_superseded", "include_superseded"},
		{"max vs min", toolParams{"min_score": "number"}, "max_score", "min_score"},
		{"min vs max", toolParams{"max_tier": "number"}, "min_tier", "max_tier"},
		{"enable vs disable", toolParams{"disable_cache": "boolean"}, "enable_cache", "disable_cache"},
		{"from vs to", toolParams{"to_id": "string"}, "from_id", "to_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{tc.key: true}
			require.Empty(t, reconcileArgKeys(args, tc.real),
				"%q must not be reconciled into its own negation", tc.key)
			require.NotContains(t, args, tc.mustNotBe)
			require.Contains(t, args, tc.key, "the caller's key stays as sent")
		})
	}
	t.Run("a real typo of the same polarity still resolves", func(t *testing.T) {
		real := toolParams{"include_tests": "boolean"}
		args := map[string]any{"include_test": true}
		require.NotEmpty(t, reconcileArgKeys(args, real))
		require.Equal(t, true, args["include_tests"])
	})
}
