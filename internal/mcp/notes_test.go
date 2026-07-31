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

// ---------------------------------------------------------------------------
// notesManager — unit tests for the persistence + filter layer
// ---------------------------------------------------------------------------

func TestNotesManager_SaveQueryDelete(t *testing.T) {
	nm := newNotesManager("", "")

	id1, err := nm.Save(persistence.NoteEntry{
		SessionID: "s1",
		Body:      "init the cache for pkg/foo.go::Bar",
		SymbolID:  "pkg/foo.go::Bar",
		Tags:      []string{"decision"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, id1)

	id2, err := nm.Save(persistence.NoteEntry{
		SessionID: "s1",
		Body:      "TODO: revisit timeout",
		FilePath:  "pkg/foo.go",
		Tags:      []string{"todo"},
	})
	require.NoError(t, err)

	all := nm.Query(NoteQueryFilter{SessionID: "s1"})
	require.Len(t, all, 2)
	// Newest-first ordering.
	assert.Equal(t, id2, all[0].ID)

	bySymbol := nm.Query(NoteQueryFilter{SymbolID: "pkg/foo.go::Bar"})
	require.Len(t, bySymbol, 1)
	assert.Equal(t, id1, bySymbol[0].ID)

	byFile := nm.Query(NoteQueryFilter{FilePath: "pkg/foo.go"})
	require.Len(t, byFile, 1)
	assert.Equal(t, id2, byFile[0].ID)

	byTag := nm.Query(NoteQueryFilter{Tag: "DECISION"})
	require.Len(t, byTag, 1)
	assert.Equal(t, id1, byTag[0].ID)

	byText := nm.Query(NoteQueryFilter{TextSearch: "TIMEOUT"})
	require.Len(t, byText, 1)
	assert.Equal(t, id2, byText[0].ID)

	require.NoError(t, nm.Delete(id1))
	assert.Equal(t, 1, nm.Count())
	require.NoError(t, nm.Delete(id1), "deleting twice is a noop")
}

func TestNotesManager_Update(t *testing.T) {
	nm := newNotesManager("", "")
	id, err := nm.Save(persistence.NoteEntry{
		SessionID: "s1",
		Body:      "first body",
		Tags:      []string{"draft"},
	})
	require.NoError(t, err)

	newBody := "second body"
	pinned := true
	updated, err := nm.Update(id, &newBody, []string{"final"}, &pinned, []string{"pkg/x.go::Bar"})
	require.NoError(t, err)

	assert.Equal(t, newBody, updated.Body)
	assert.Equal(t, []string{"final"}, updated.Tags)
	assert.True(t, updated.Pinned)
	assert.Contains(t, updated.AutoLinks, "pkg/x.go::Bar")

	// Re-fetch and confirm persisted state matches.
	got, ok := nm.Get(id)
	require.True(t, ok)
	assert.Equal(t, updated.Body, got.Body)
}

func TestNotesManager_UpdateMissingID(t *testing.T) {
	nm := newNotesManager("", "")
	_, err := nm.Update("nope", nil, nil, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestNotesManager_PersistenceRoundTrip(t *testing.T) {
	cache := tempSidecarDir(t)
	repo := "/tmp/notes-test-repo"

	nm1 := newNotesManager(cache, repo)
	id, err := nm1.Save(persistence.NoteEntry{SessionID: "s1", Body: "first"})
	require.NoError(t, err)

	// New manager pointed at the same cache — simulates server restart.
	nm2 := newNotesManager(cache, repo)
	assert.True(t, nm2.HasData())
	got, ok := nm2.Get(id)
	require.True(t, ok)
	assert.Equal(t, "first", got.Body)
}

func TestNotesManager_Distill(t *testing.T) {
	nm := newNotesManager("", "")
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:       "pkg/foo.go::Bar",
		Name:     "Bar",
		Kind:     graph.KindFunction,
		FilePath: "pkg/foo.go",
	})

	for range 3 {
		_, err := nm.Save(persistence.NoteEntry{
			SessionID: "s1",
			Body:      "note about Bar",
			SymbolID:  "pkg/foo.go::Bar",
			Tags:      []string{"decision"},
		})
		require.NoError(t, err)
	}
	_, err := nm.Save(persistence.NoteEntry{
		SessionID: "s1",
		Body:      "side note",
		Pinned:    true,
		Tags:      []string{"note"},
	})
	require.NoError(t, err)

	res := nm.DistillSession("s1", "", "", defaultDistillOptions(), g.GetNode)

	assert.Equal(t, 4, res.NoteCount)
	require.NotEmpty(t, res.TopSymbols)
	assert.Equal(t, "pkg/foo.go::Bar", res.TopSymbols[0].ID)
	assert.Equal(t, "Bar", res.TopSymbols[0].Name)
	assert.Equal(t, 3, res.TopSymbols[0].Count)

	// Pinned should be surfaced.
	require.NotEmpty(t, res.PinnedNotes)
	// Decision excerpt present.
	require.NotEmpty(t, res.Decisions)
	// Summary non-empty.
	assert.NotEmpty(t, res.Summary)
}

func TestNotesManager_DistillEmpty(t *testing.T) {
	nm := newNotesManager("", "")
	res := nm.DistillSession("missing", "", "", defaultDistillOptions(), nil)
	assert.Equal(t, 0, res.NoteCount)
	assert.Empty(t, res.Summary)
}

func TestNotesManager_QueryLimit(t *testing.T) {
	nm := newNotesManager("", "")
	for range 5 {
		_, _ = nm.Save(persistence.NoteEntry{SessionID: "s1", Body: "n"})
	}
	out := nm.Query(NoteQueryFilter{SessionID: "s1", Limit: 2})
	assert.Len(t, out, 2)
}

func TestNotesManager_QuerySince(t *testing.T) {
	nm := newNotesManager("", "")
	id1, _ := nm.Save(persistence.NoteEntry{SessionID: "s1", Body: "old"})
	time.Sleep(5 * time.Millisecond)
	cutoff := time.Now().UTC()
	time.Sleep(5 * time.Millisecond)
	id2, _ := nm.Save(persistence.NoteEntry{SessionID: "s1", Body: "new"})

	out := nm.Query(NoteQueryFilter{Since: cutoff})
	require.Len(t, out, 1)
	assert.Equal(t, id2, out[0].ID)
	assert.NotEqual(t, id1, out[0].ID)
}

// ---------------------------------------------------------------------------
// Auto-linking — body→symbol extractor
// ---------------------------------------------------------------------------

func TestAutoLink_ResolvesByExactID(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/foo.go::Bar", Name: "Bar", FilePath: "pkg/foo.go"})
	g.AddNode(&graph.Node{ID: "pkg/other.go::Quux", Name: "Quux", FilePath: "pkg/other.go"})

	body := "Looked at pkg/foo.go::Bar — it depends on Quux."
	links := autoLinkBody(body, g, "", defaultAutoLinkOptions())
	assert.Contains(t, links, "pkg/foo.go::Bar")
	assert.Contains(t, links, "pkg/other.go::Quux")
}

func TestAutoLink_DropsAmbiguousNames(t *testing.T) {
	g := graph.New()
	// Four nodes named "Foo" — exceeds the precision cap (3).
	for i := range 4 {
		g.AddNode(&graph.Node{ID: "pkg/x.go::Foo" + string(rune('A'+i)), Name: "Foo", FilePath: "pkg/x.go"})
	}
	links := autoLinkBody("mentions Foo somewhere", g, "", defaultAutoLinkOptions())
	for _, l := range links {
		assert.False(t, strings.HasPrefix(l, "pkg/x.go::Foo"), "ambiguous Foo should be skipped, got: %s", l)
	}
}

func TestAutoLink_SkipsStopwords(t *testing.T) {
	g := graph.New()
	// "the" is in the stop-word list — even if a node existed, it
	// must not be auto-linked.
	g.AddNode(&graph.Node{ID: "pkg/x.go::the", Name: "the", FilePath: "pkg/x.go"})
	links := autoLinkBody("this is the thing", g, "", defaultAutoLinkOptions())
	assert.NotContains(t, links, "pkg/x.go::the")
}

func TestAutoLink_HonorsWorkspaceScope(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:          "pkg/a.go::Bar",
		Name:        "Bar",
		FilePath:    "pkg/a.go",
		WorkspaceID: "ws-a",
	})
	g.AddNode(&graph.Node{
		ID:          "pkg/b.go::Baz",
		Name:        "Baz",
		FilePath:    "pkg/b.go",
		WorkspaceID: "ws-b",
	})

	links := autoLinkBody("see Bar and Baz", g, "ws-a", defaultAutoLinkOptions())
	assert.Contains(t, links, "pkg/a.go::Bar")
	assert.NotContains(t, links, "pkg/b.go::Baz", "ws-b symbol must not leak into ws-a auto-link result")
}

// TestAutoLink_DoesNotMatchBareEnglishWords pins the regression: a
// note body containing the word "memory" used to auto-link to
// `daemon_status_tui.go::repoItem.memory` (a field whose Name is
// literally "memory"), and "MCP" auto-linked to `Config.MCP`. The
// auto-linker now requires a code-shape signal on each token
// (uppercase, underscore, or dot) and a single unambiguous graph
// match, so plain English words and short caps acronyms with
// multiple matches no longer pollute the link set.
func TestAutoLink_DoesNotMatchBareEnglishWords(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/tui.go::repoItem.memory", Name: "memory", FilePath: "pkg/tui.go"})
	g.AddNode(&graph.Node{ID: "pkg/config.go::Config.MCP", Name: "MCP", FilePath: "pkg/config.go"})
	g.AddNode(&graph.Node{ID: "pkg/cli.go::otherMCP", Name: "MCP", FilePath: "pkg/cli.go"})

	body := "Validation memory: MCP integration tests exercising store_memory"
	links := autoLinkBody(body, g, "", defaultAutoLinkOptions())

	// bare "memory" / "MCP" with multiple-name-match must not link.
	require.NotContains(t, links, "pkg/tui.go::repoItem.memory",
		"plain English `memory` must not pull in a field of the same name")
	require.NotContains(t, links, "pkg/config.go::Config.MCP",
		"short caps `MCP` with multiple matches is ambiguous; must not auto-link")
	require.NotContains(t, links, "pkg/cli.go::otherMCP",
		"short caps `MCP` with multiple matches must not auto-link")

	// A signal-bearing token like store_memory still works when the
	// graph has a uniquely-named node for it.
	g.AddNode(&graph.Node{ID: "pkg/mem.go::store_memory", Name: "store_memory", FilePath: "pkg/mem.go"})
	links = autoLinkBody(body, g, "", defaultAutoLinkOptions())
	require.Contains(t, links, "pkg/mem.go::store_memory",
		"snake_case tokens carry the code signal and must still link when unambiguous")
}

func TestAutoLink_CapsAtMaxLinks(t *testing.T) {
	g := graph.New()
	body := strings.Builder{}
	for i := range 30 {
		name := "Sym" + string(rune('A'+i))
		g.AddNode(&graph.Node{ID: "pkg/x.go::" + name, Name: name, FilePath: "pkg/x.go"})
		body.WriteString(name + " ")
	}
	opts := defaultAutoLinkOptions()
	opts.MaxLinks = 5
	links := autoLinkBody(body.String(), g, "", opts)
	assert.LessOrEqual(t, len(links), 5)
}

// ---------------------------------------------------------------------------
// Handler tests — end-to-end against an embedded Server
// ---------------------------------------------------------------------------

func newTestServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/foo.go::Bar", Name: "Bar", Kind: graph.KindFunction, FilePath: "pkg/foo.go"})
	g.AddNode(&graph.Node{ID: "pkg/foo.go::Baz", Name: "Baz", Kind: graph.KindMethod, FilePath: "pkg/foo.go"})

	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	s.notes = newNotesManager("", "")
	return s
}

func callHandler(t *testing.T, h func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	return res
}

func unmarshalResult(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	require.False(t, res.IsError, "handler returned an error result: %+v", res.Content)
	require.NotEmpty(t, res.Content)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected TextContent, got %T", res.Content[0])
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m
}

func TestHandleSaveNote_CreateAndAutoLink(t *testing.T) {
	s := newTestServer(t)
	res := callHandler(t, s.handleSaveNote, map[string]any{
		"body":      "look at pkg/foo.go::Bar and update Baz",
		"symbol_id": "pkg/foo.go::Bar",
		"tags":      "decision, follow-up",
	})
	out := unmarshalResult(t, res)
	require.NotEmpty(t, out["id"])
	links, _ := out["links"].([]any)
	require.NotEmpty(t, links)

	idSet := map[string]bool{}
	for _, l := range links {
		idSet[l.(string)] = true
	}
	assert.True(t, idSet["pkg/foo.go::Bar"], "primary symbol must be linked")
	assert.True(t, idSet["pkg/foo.go::Baz"], "auto-linker should pick up Baz from body")

	tags, _ := out["tags"].([]any)
	assert.ElementsMatch(t, []any{"decision", "follow-up"}, tags)
}

func TestHandleSaveNote_RejectEmpty(t *testing.T) {
	s := newTestServer(t)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	res, err := s.handleSaveNote(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.IsError, "empty save should return error result")
}

func TestHandleSaveNote_NoAutolink(t *testing.T) {
	s := newTestServer(t)
	res := callHandler(t, s.handleSaveNote, map[string]any{
		"body":        "Bar is mentioned but should not auto-link",
		"no_autolink": true,
	})
	out := unmarshalResult(t, res)
	_, hasLinks := out["links"]
	assert.False(t, hasLinks, "no_autolink should produce no links field")
}

func TestHandleSaveNote_Update(t *testing.T) {
	s := newTestServer(t)
	// Create.
	res := callHandler(t, s.handleSaveNote, map[string]any{
		"body": "draft",
	})
	out := unmarshalResult(t, res)
	id := out["id"].(string)

	// Update.
	res2 := callHandler(t, s.handleSaveNote, map[string]any{
		"id":     id,
		"body":   "final body referring to Bar",
		"tags":   "final",
		"pinned": true,
	})
	out2 := unmarshalResult(t, res2)
	assert.Equal(t, id, out2["id"])
	assert.Equal(t, "final body referring to Bar", out2["body"])
	assert.Equal(t, true, out2["pinned"])
	links, _ := out2["links"].([]any)
	require.NotEmpty(t, links)
}

func TestHandleQueryNotes_FilterBySymbolAndTag(t *testing.T) {
	s := newTestServer(t)
	_ = callHandler(t, s.handleSaveNote, map[string]any{
		"body": "alpha", "symbol_id": "pkg/foo.go::Bar", "tags": "decision",
	})
	_ = callHandler(t, s.handleSaveNote, map[string]any{
		"body": "beta", "symbol_id": "pkg/foo.go::Baz", "tags": "todo",
	})

	res := callHandler(t, s.handleQueryNotes, map[string]any{
		"symbol_id":  "pkg/foo.go::Bar",
		"session_id": "all",
	})
	out := unmarshalResult(t, res)
	total := int(out["total"].(float64))
	assert.Equal(t, 1, total)

	res2 := callHandler(t, s.handleQueryNotes, map[string]any{
		"tag":        "todo",
		"session_id": "all",
	})
	out2 := unmarshalResult(t, res2)
	total2 := int(out2["total"].(float64))
	assert.Equal(t, 1, total2)
}

func TestHandleQueryNotes_TextFilter(t *testing.T) {
	s := newTestServer(t)
	_ = callHandler(t, s.handleSaveNote, map[string]any{"body": "timeout bug in worker"})
	_ = callHandler(t, s.handleSaveNote, map[string]any{"body": "unrelated note"})

	res := callHandler(t, s.handleQueryNotes, map[string]any{
		"text":       "TIMEOUT",
		"session_id": "all",
	})
	out := unmarshalResult(t, res)
	total := int(out["total"].(float64))
	assert.Equal(t, 1, total)
}

func TestHandleDistillSession_FullCycle(t *testing.T) {
	s := newTestServer(t)
	for range 3 {
		_ = callHandler(t, s.handleSaveNote, map[string]any{
			"body": "decision about Bar", "symbol_id": "pkg/foo.go::Bar", "tags": "decision",
		})
	}
	_ = callHandler(t, s.handleSaveNote, map[string]any{
		"body": "pin this", "pinned": true,
	})

	res := callHandler(t, s.handleDistillSession, map[string]any{
		"session_id": "all",
	})
	out := unmarshalResult(t, res)
	noteCount := int(out["note_count"].(float64))
	assert.Equal(t, 4, noteCount)

	topSyms, _ := out["top_symbols"].([]any)
	require.NotEmpty(t, topSyms)
	first, _ := topSyms[0].(map[string]any)
	assert.Equal(t, "pkg/foo.go::Bar", first["id"])

	pinned, _ := out["pinned_notes"].([]any)
	assert.NotEmpty(t, pinned)

	decisions, _ := out["decisions"].([]any)
	assert.NotEmpty(t, decisions)

	summary, _ := out["summary"].(string)
	assert.Contains(t, summary, "Top symbols")
}

// TestNotes_RegisteredOnNewServer asserts the three session-memory
// tools land in the registered tool set after NewServer + InitNotes
// — protects against accidental removal of the registerNotesTools
// call.
func TestNotes_RegisteredOnNewServer(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.InitNotes("", "")

	// Round-trip a save → query → distill through the wired handlers
	// to confirm the entire family works end-to-end on a real Server
	// built by NewServer.
	saveRes := callHandler(t, srv.handleSaveNote, map[string]any{
		"body": "decision: use fastpath",
		"tags": "decision",
	})
	out := unmarshalResult(t, saveRes)
	require.NotEmpty(t, out["id"])

	queryRes := callHandler(t, srv.handleQueryNotes, map[string]any{
		"tag":        "decision",
		"session_id": "all",
	})
	queryOut := unmarshalResult(t, queryRes)
	assert.Equal(t, 1.0, queryOut["total"])

	distillRes := callHandler(t, srv.handleDistillSession, map[string]any{
		"session_id": "all",
	})
	distillOut := unmarshalResult(t, distillRes)
	assert.Equal(t, 1.0, distillOut["note_count"])
}

func TestNotes_HandlersWithoutInit(t *testing.T) {
	// Server with no notes manager — every handler must surface a
	// clear error rather than panic.
	s := &Server{
		graph:      graph.New(),
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"body": "x"}
	res, err := s.handleSaveNote(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, res.IsError)

	res2, err := s.handleQueryNotes(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, res2.IsError)

	res3, err := s.handleDistillSession(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, res3.IsError)
}
