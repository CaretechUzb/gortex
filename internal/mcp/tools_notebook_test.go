package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/persistence"
)

func newNotebookTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := tempNotebookRepo(t)
	s := &Server{
		graph:      graph.New(),
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	s.InitNotebook(dir)
	return s, dir
}

func callNotebookHandler(t *testing.T, h func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	if res.IsError {
		return map[string]any{"is_error": true}
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m
}

func TestNotebook_SavePersists(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	out := callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "auth invariant",
		"body":  "Bar must hold the mutex.",
		"tags":  "invariant,auth",
	})

	id := out["id"].(string)
	require.NotEmpty(t, id)

	// The entry round-trips through the sidecar DB (no markdown file).
	shown := callNotebookHandler(t, s.handleNotebookShow, map[string]any{"id": id})
	assert.Equal(t, "auth invariant", shown["title"])
	assert.Contains(t, shown["body"], "Bar must hold the mutex")
	tags, _ := shown["tags"].([]any)
	require.Len(t, tags, 2)
	assert.Equal(t, "invariant", tags[0])
	assert.Equal(t, "auth", tags[1])
}

func TestNotebook_UpdatePreservesCreated(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	created := callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "first", "body": "body1",
	})
	id := created["id"].(string)
	originalCreated := created["created"].(string)

	// Sleep tiny bit to make sure Updated bumps.
	time.Sleep(10 * time.Millisecond)
	updated := callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"id":   id,
		"body": "body2",
	})
	assert.Equal(t, originalCreated, updated["created"], "Created preserved across update")
	assert.NotEqual(t, originalCreated, updated["updated"], "Updated bumped on save")
}

func TestNotebook_RejectsEmptySave(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	out := callNotebookHandler(t, s.handleNotebookSave, map[string]any{})
	assert.True(t, out["is_error"] == true)
}

func TestNotebook_FindByBody(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	_ = callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "alpha", "body": "tokio runtime quirks",
	})
	_ = callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "beta", "body": "axum router setup",
	})

	out := callNotebookHandler(t, s.handleNotebookFind, map[string]any{"query": "tokio"})
	entries, _ := out["entries"].([]any)
	require.Len(t, entries, 1)
	assert.Equal(t, "alpha", entries[0].(map[string]any)["title"])
}

func TestNotebook_FindByTag(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	_ = callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "a", "body": "x", "tags": "decision,api",
	})
	_ = callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "b", "body": "y", "tags": "gotcha",
	})

	out := callNotebookHandler(t, s.handleNotebookFind, map[string]any{"query": "decision"})
	entries, _ := out["entries"].([]any)
	require.Len(t, entries, 1)
}

func TestNotebook_FindCaseInsensitive(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	_ = callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "Tokio Stuff", "body": "x",
	})
	out := callNotebookHandler(t, s.handleNotebookFind, map[string]any{"query": "tokio"})
	entries, _ := out["entries"].([]any)
	assert.Len(t, entries, 1)
}

func TestNotebook_ListReturnsAllSortedDesc(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	for i := range 3 {
		_ = callNotebookHandler(t, s.handleNotebookSave, map[string]any{
			"title": "t" + string(rune('A'+i)), "body": "x",
		})
		time.Sleep(5 * time.Millisecond)
	}

	out := callNotebookHandler(t, s.handleNotebookList, map[string]any{})
	entries, _ := out["entries"].([]any)
	require.Len(t, entries, 3)
	// Most recently updated first.
	assert.Equal(t, "tC", entries[0].(map[string]any)["title"])
}

func TestNotebook_ShowReturnsBody(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	created := callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "x", "body": "the full markdown body here",
	})
	id := created["id"].(string)

	out := callNotebookHandler(t, s.handleNotebookShow, map[string]any{"id": id})
	assert.Equal(t, "the full markdown body here", out["body"], "show returns the verbatim body")
}

func TestNotebook_ShowUnknownIDErrors(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	out := callNotebookHandler(t, s.handleNotebookShow, map[string]any{"id": "doesnotexist"})
	assert.True(t, out["is_error"] == true)
}

func TestNotebook_UsedBumpsCount(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	created := callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "x", "body": "y",
	})
	id := created["id"].(string)

	for range 3 {
		_ = callNotebookHandler(t, s.handleNotebookUsed, map[string]any{"id": id})
	}
	out := callNotebookHandler(t, s.handleNotebookShow, map[string]any{"id": id})
	assert.EqualValues(t, 3, out["used_count"].(float64))
	_, hasLastUsed := out["last_used"]
	assert.True(t, hasLastUsed)
}

func TestNotebook_RejectsMissingNotebook(t *testing.T) {
	s := &Server{
		graph:      graph.New(),
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	// No InitNotebook — s.notebook is nil.
	out := callNotebookHandler(t, s.handleNotebookSave, map[string]any{"body": "x"})
	assert.True(t, out["is_error"] == true)
}

func TestNotebook_FrontmatterRoundTrip(t *testing.T) {
	entry := notebookEntry{
		ID:      "abc",
		Title:   "a title: with colon",
		Tags:    []string{"x", "y"},
		Created: time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC),
		Updated: time.Date(2026, 5, 19, 11, 0, 0, 0, time.UTC),
		Body:    "line one\nline two\n",
	}
	md := notebookMarshal(entry)
	parsed, err := notebookUnmarshal(md)
	require.NoError(t, err)
	assert.Equal(t, entry.Title, parsed.Title)
	assert.Equal(t, entry.Tags, parsed.Tags)
	assert.Equal(t, entry.Created.Unix(), parsed.Created.Unix())
	assert.Equal(t, entry.Body, parsed.Body)
}

func TestNotebook_NoDirManagerStillSafe(t *testing.T) {
	nm := newNotebookManager("")
	// Save must NOT silently succeed — the prior behaviour returned a
	// fresh ID and timestamps but never persisted, so subsequent
	// list/find/show/used all returned empty and the caller had no
	// signal anything was wrong. Honest error here.
	_, err := nm.Save(notebookEntry{Title: "x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not initialised")
	// Get / List / Find continue to return empty/false consistently.
	_, ok := nm.Get("anything")
	assert.False(t, ok)
	assert.Empty(t, nm.List())
}

func TestNotebook_PrunesByTTL(t *testing.T) {
	dir := tempNotebookRepo(t)
	nm := newNotebookManager(dir)
	// TTL must comfortably exceed the Save→prune latency. pruneLocked's
	// cutoff is computed *after* the fresh entry's Updated stamp is
	// written, so a sub-millisecond TTL lets a loaded runner sweep the
	// just-saved entry itself (0 remain instead of 1). A minute sits far
	// below the stale entry's 1h age yet far above any realistic save
	// latency, so the prune is deterministic.
	nm.ttl = time.Minute
	require.NotNil(t, nm.sidecar)
	// Insert a row with Updated far in the past directly into the
	// sidecar so the next Save's prune sweeps it.
	require.NoError(t, nm.sidecar.UpsertNotebook(nm.repoKey, persistence.NotebookRow{
		ID:      "stale",
		Title:   "stale",
		Updated: time.Now().UTC().Add(-time.Hour),
	}))

	// Trigger a save which fires the prune.
	_, err := nm.Save(notebookEntry{Title: "fresh", Body: "x"})
	require.NoError(t, err)

	// The stale entry should be gone; the fresh one survives.
	_, ok := nm.Get("stale")
	assert.False(t, ok, "TTL-expired entry pruned")
	assert.Len(t, nm.List(), 1, "only the fresh entry remains")
}

func TestNotebook_DeleteIdempotent(t *testing.T) {
	dir := tempNotebookRepo(t)
	nm := newNotebookManager(dir)
	_, _ = nm.Save(notebookEntry{ID: "x", Title: "x"})
	require.NoError(t, nm.Delete("x"))
	require.NoError(t, nm.Delete("x"), "second delete on missing entry is not an error")
}

func TestParseYAMLInlineList(t *testing.T) {
	assert.Equal(t, []string{"a", "b"}, parseYAMLInlineList("[a, b]"))
	assert.Equal(t, []string{"a", "b"}, parseYAMLInlineList(" a , b "))
	assert.Nil(t, parseYAMLInlineList("[]"))
	assert.Nil(t, parseYAMLInlineList(""))
}

func TestYAMLEscapeRoundTrip(t *testing.T) {
	cases := []string{
		`plain`,
		`with: colon`,
		`with "quote"`,
		``,
	}
	for _, c := range cases {
		escaped := yamlEscapeOneLine(c)
		back := yamlUnescapeOneLine(escaped)
		assert.Equal(t, c, back, "roundtrip %q", c)
		_ = strings.HasPrefix(escaped, `"`) // suppress unused
	}
}
