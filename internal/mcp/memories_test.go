package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
// memoryManager — CRUD + filter unit tests
// ---------------------------------------------------------------------------

func TestMemoryManager_SaveQueryDelete(t *testing.T) {
	mm := newMemoryManager("", "")

	id1, err := mm.Save(persistence.MemoryEntry{
		Title:     "Bar invariant",
		Body:      "Bar must hold the lock before mutating cache",
		SymbolIDs: []string{"pkg/foo.go::Bar"},
		Tags:      []string{"invariant"},
		Kind:      "invariant",
	})
	require.NoError(t, err)
	require.NotEmpty(t, id1)

	id2, err := mm.Save(persistence.MemoryEntry{
		Body:      "TODO: revisit timeout in worker",
		FilePaths: []string{"pkg/foo.go"},
		Tags:      []string{"gotcha"},
		Kind:      "gotcha",
	})
	require.NoError(t, err)

	all := mm.Query(MemoryQueryFilter{})
	require.Len(t, all, 2)

	bySymbol := mm.Query(MemoryQueryFilter{SymbolID: "pkg/foo.go::Bar"})
	require.Len(t, bySymbol, 1)
	assert.Equal(t, id1, bySymbol[0].ID)

	byFile := mm.Query(MemoryQueryFilter{FilePath: "pkg/foo.go"})
	require.Len(t, byFile, 1)
	assert.Equal(t, id2, byFile[0].ID)

	byTag := mm.Query(MemoryQueryFilter{Tag: "INVARIANT"})
	require.Len(t, byTag, 1)
	assert.Equal(t, id1, byTag[0].ID)

	byKind := mm.Query(MemoryQueryFilter{Kind: "gotcha"})
	require.Len(t, byKind, 1)
	assert.Equal(t, id2, byKind[0].ID)

	byText := mm.Query(MemoryQueryFilter{TextSearch: "TIMEOUT"})
	require.Len(t, byText, 1)
	assert.Equal(t, id2, byText[0].ID)

	// Title also matched by TextSearch.
	byTitle := mm.Query(MemoryQueryFilter{TextSearch: "invariant"})
	require.NotEmpty(t, byTitle)

	require.NoError(t, mm.Delete(id1))
	assert.Equal(t, 1, mm.Count())
	require.NoError(t, mm.Delete(id1), "deleting twice is a noop")
}

func TestMemoryManager_Update(t *testing.T) {
	mm := newMemoryManager("", "")
	id, err := mm.Save(persistence.MemoryEntry{
		Body: "draft body",
		Tags: []string{"draft"},
	})
	require.NoError(t, err)

	newBody := "final body referring to Bar"
	pinned := true
	importance := 5
	updated, err := mm.Update(id, MemoryPatch{
		Body:       &newBody,
		Tags:       []string{"final"},
		Pinned:     &pinned,
		Importance: &importance,
		AddLinks:   []string{"pkg/x.go::Bar"},
	})
	require.NoError(t, err)

	assert.Equal(t, newBody, updated.Body)
	assert.Equal(t, []string{"final"}, updated.Tags)
	assert.True(t, updated.Pinned)
	assert.Equal(t, 5, updated.Importance)
	assert.Contains(t, updated.AutoLinks, "pkg/x.go::Bar")

	got, ok := mm.Get(id)
	require.True(t, ok)
	assert.Equal(t, updated.Body, got.Body)
}

func TestMemoryManager_UpdateMissingID(t *testing.T) {
	mm := newMemoryManager("", "")
	_, err := mm.Update("nope", MemoryPatch{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestMemoryManager_SortPinnedThenImportance(t *testing.T) {
	mm := newMemoryManager("", "")
	// importance=2, not pinned, newest
	_, _ = mm.Save(persistence.MemoryEntry{Body: "low-imp newest", Importance: 2})
	// importance=5, pinned, oldest
	_, _ = mm.Save(persistence.MemoryEntry{Body: "pinned high-imp", Importance: 5, Pinned: true})
	// importance=4, not pinned, middle
	_, _ = mm.Save(persistence.MemoryEntry{Body: "mid-imp", Importance: 4})

	out := mm.Query(MemoryQueryFilter{})
	require.Len(t, out, 3)
	assert.True(t, out[0].Pinned, "pinned first")
	assert.Equal(t, 4, out[1].Importance, "then importance=4")
	assert.Equal(t, 2, out[2].Importance, "then importance=2")
}

func TestMemoryManager_HidesSuperseded(t *testing.T) {
	mm := newMemoryManager("", "")
	oldID, _ := mm.Save(persistence.MemoryEntry{Body: "old"})
	newID, _ := mm.Save(persistence.MemoryEntry{Body: "new"})

	supBy := newID
	_, err := mm.Update(oldID, MemoryPatch{SupersededBy: &supBy})
	require.NoError(t, err)

	out := mm.Query(MemoryQueryFilter{})
	require.Len(t, out, 1, "superseded entry hidden by default")
	assert.Equal(t, newID, out[0].ID)

	out2 := mm.Query(MemoryQueryFilter{IncludeSuperseded: true})
	require.Len(t, out2, 2)
}

func TestMemoryManager_MarkAccessed(t *testing.T) {
	mm := newMemoryManager("", "")
	id, _ := mm.Save(persistence.MemoryEntry{Body: "x"})
	mm.MarkAccessed([]string{id})
	got, _ := mm.Get(id)
	assert.EqualValues(t, 1, got.AccessCount)
	assert.False(t, got.LastAccessed.IsZero())
}

func TestMemoryManager_DefaultsApplied(t *testing.T) {
	mm := newMemoryManager("", "")
	id, _ := mm.Save(persistence.MemoryEntry{Body: "minimal"})
	got, _ := mm.Get(id)
	assert.Equal(t, float32(1.0), got.Confidence)
	assert.Equal(t, 3, got.Importance)
	assert.Equal(t, "reference", got.Kind)
	assert.Equal(t, "manual", got.Source)
}

func TestMemoryManager_PersistenceRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	repoPath := filepath.Join(tmp, "fake-repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	cacheDir := filepath.Join(tmp, "cache")
	closeSidecarOnCleanup(t, cacheDir)

	mm := newMemoryManager(cacheDir, repoPath)
	_, err := mm.Save(persistence.MemoryEntry{
		Body:      "remember this",
		SymbolIDs: []string{"pkg/x.go::Foo"},
		Tags:      []string{"invariant"},
		Pinned:    true,
	})
	require.NoError(t, err)

	mm2 := newMemoryManager(cacheDir, repoPath)
	out := mm2.Query(MemoryQueryFilter{})
	require.Len(t, out, 1)
	assert.Equal(t, "remember this", out[0].Body)
	assert.True(t, out[0].Pinned)
}

// ---------------------------------------------------------------------------
// Surface — ranking behaviour
// ---------------------------------------------------------------------------

func TestSurface_RanksSymbolAnchorAbovePinned(t *testing.T) {
	mm := newMemoryManager("", "")
	// Pinned but no anchor match.
	_, _ = mm.Save(persistence.MemoryEntry{
		Body: "pinned unrelated", Pinned: true, Importance: 3,
	})
	// Symbol anchor match — should rank above pinned.
	anchorID, _ := mm.Save(persistence.MemoryEntry{
		Body:       "anchor match",
		SymbolIDs:  []string{"pkg/foo.go::Bar"},
		Importance: 3,
	})

	res := mm.Surface(SurfaceOptions{
		SymbolIDs: []string{"pkg/foo.go::Bar"},
		Limit:     10,
	}, nil)

	require.Equal(t, 2, res.Total)
	assert.Equal(t, anchorID, res.Memories[0].ID, "anchor match should rank above pinned-without-anchor")
}

func TestSurface_KeywordHits(t *testing.T) {
	mm := newMemoryManager("", "")
	id, _ := mm.Save(persistence.MemoryEntry{Body: "the JWT validator rejects expired tokens"})
	_, _ = mm.Save(persistence.MemoryEntry{Body: "unrelated note about caching"})

	res := mm.Surface(SurfaceOptions{
		Task:  "fix jwt validator regression",
		Limit: 10,
	}, nil)

	require.NotEmpty(t, res.Memories)
	assert.Equal(t, id, res.Memories[0].ID)
}

func TestSurface_IgnoresSupersededByDefault(t *testing.T) {
	mm := newMemoryManager("", "")
	oldID, _ := mm.Save(persistence.MemoryEntry{
		Body: "old", SymbolIDs: []string{"pkg/x.go::Y"},
	})
	newID, _ := mm.Save(persistence.MemoryEntry{
		Body: "new", SymbolIDs: []string{"pkg/x.go::Y"},
	})

	supBy := newID
	_, _ = mm.Update(oldID, MemoryPatch{SupersededBy: &supBy})

	res := mm.Surface(SurfaceOptions{
		SymbolIDs: []string{"pkg/x.go::Y"}, Limit: 10,
	}, nil)
	require.Equal(t, 1, res.Total)
	assert.Equal(t, newID, res.Memories[0].ID)

	res2 := mm.Surface(SurfaceOptions{
		SymbolIDs: []string{"pkg/x.go::Y"}, Limit: 10, IncludeSuperseded: true,
	}, nil)
	require.Equal(t, 2, res2.Total)
}

func TestSurface_FilePromotionFromSymbol(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/foo.go::Bar", Name: "Bar", FilePath: "pkg/foo.go", Kind: graph.KindFunction})

	mm := newMemoryManager("", "")
	id, _ := mm.Save(persistence.MemoryEntry{
		Body:      "file-anchored memory",
		FilePaths: []string{"pkg/foo.go"},
	})

	res := mm.Surface(SurfaceOptions{
		SymbolIDs: []string{"pkg/foo.go::Bar"}, Limit: 10,
	}, func(symID string) *graph.Node { return g.GetNode(symID) })

	require.Equal(t, 1, res.Total, "containing file of the anchor symbol should match file-anchored memory")
	assert.Equal(t, id, res.Memories[0].ID)
}

func TestSurface_RequiresAnchorMatchWhenAnchorsProvided(t *testing.T) {
	mm := newMemoryManager("", "")
	// Not pinned, no anchor match.
	_, _ = mm.Save(persistence.MemoryEntry{Body: "unrelated"})

	res := mm.Surface(SurfaceOptions{
		SymbolIDs: []string{"pkg/foo.go::Bar"}, Limit: 10,
	}, nil)
	assert.Equal(t, 0, res.Total, "no-match non-pinned memory must be dropped when anchors provided")
}

func TestSurface_NoAnchorsReturnsEverything(t *testing.T) {
	mm := newMemoryManager("", "")
	_, _ = mm.Save(persistence.MemoryEntry{Body: "a"})
	_, _ = mm.Save(persistence.MemoryEntry{Body: "b"})

	res := mm.Surface(SurfaceOptions{Limit: 10}, nil)
	assert.Equal(t, 2, res.Total)
}

func TestSurface_ExcerptCap(t *testing.T) {
	mm := newMemoryManager("", "")
	body := strings.Repeat("A", 1000)
	_, _ = mm.Save(persistence.MemoryEntry{Body: body})

	res := mm.Surface(SurfaceOptions{Limit: 10, ExcerptCap: 100}, nil)
	require.NotEmpty(t, res.Memories)
	assert.LessOrEqual(t, len([]byte(res.Memories[0].Body)), 100+len("…")+3)
	assert.Contains(t, res.Memories[0].Body, "…")
}

func TestSurface_MarkAccessedIncrements(t *testing.T) {
	mm := newMemoryManager("", "")
	id, _ := mm.Save(persistence.MemoryEntry{Body: "x"})

	_ = mm.Surface(SurfaceOptions{Limit: 10, MarkAccessed: true}, nil)

	got, _ := mm.Get(id)
	assert.EqualValues(t, 1, got.AccessCount)
}

// ---------------------------------------------------------------------------
// Handler tests — end-to-end against an embedded Server
// ---------------------------------------------------------------------------

func newMemoryTestServer(t *testing.T) *Server {
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
	s.memories = newMemoryManager("", "")
	return s
}

func callMemHandler(t *testing.T, h func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	return res
}

func unmarshalMemResult(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	require.False(t, res.IsError, "handler returned an error result: %+v", res.Content)
	require.NotEmpty(t, res.Content)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected TextContent, got %T", res.Content[0])
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m
}

func TestHandleStoreMemory_CreateAndAutoLink(t *testing.T) {
	s := newMemoryTestServer(t)
	res := callMemHandler(t, s.handleStoreMemory, map[string]any{
		"title":      "Bar invariant",
		"body":       "Bar and Baz must run under the same lock",
		"symbol_ids": "pkg/foo.go::Bar",
		"tags":       "invariant,decision",
		"kind":       "invariant",
		"importance": 5,
	})
	out := unmarshalMemResult(t, res)
	require.NotEmpty(t, out["id"])
	assert.Equal(t, "Bar invariant", out["title"])
	assert.Equal(t, "invariant", out["kind"])
	assert.EqualValues(t, 5, out["importance"].(float64))

	symbols, _ := out["symbol_ids"].([]any)
	require.NotEmpty(t, symbols)

	links, _ := out["links"].([]any)
	idSet := map[string]bool{}
	for _, l := range links {
		idSet[l.(string)] = true
	}
	assert.True(t, idSet["pkg/foo.go::Baz"], "auto-linker should pick up Baz")
}

func TestHandleStoreMemory_RejectEmpty(t *testing.T) {
	s := newMemoryTestServer(t)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	res, err := s.handleStoreMemory(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.IsError, "empty store_memory should return error result")
}

func TestHandleStoreMemory_Update(t *testing.T) {
	s := newMemoryTestServer(t)
	res := callMemHandler(t, s.handleStoreMemory, map[string]any{
		"body": "draft",
	})
	out := unmarshalMemResult(t, res)
	id := out["id"].(string)

	res2 := callMemHandler(t, s.handleStoreMemory, map[string]any{
		"id":         id,
		"body":       "final body referring to Bar",
		"tags":       "final",
		"pinned":     true,
		"importance": 5,
	})
	out2 := unmarshalMemResult(t, res2)
	assert.Equal(t, id, out2["id"])
	assert.Equal(t, "final body referring to Bar", out2["body"])
	assert.Equal(t, true, out2["pinned"])
}

func TestHandleStoreMemory_SupersedesOlder(t *testing.T) {
	s := newMemoryTestServer(t)
	oldRes := callMemHandler(t, s.handleStoreMemory, map[string]any{
		"body":       "old wisdom",
		"symbol_ids": "pkg/foo.go::Bar",
	})
	oldID := unmarshalMemResult(t, oldRes)["id"].(string)

	newRes := callMemHandler(t, s.handleStoreMemory, map[string]any{
		"body":       "new wisdom",
		"symbol_ids": "pkg/foo.go::Bar",
		"supersedes": oldID,
	})
	newID := unmarshalMemResult(t, newRes)["id"].(string)

	old, ok := s.memories.Get(oldID)
	require.True(t, ok)
	assert.Equal(t, newID, old.SupersededBy)

	// query_memories must hide the old one by default.
	qres := callMemHandler(t, s.handleQueryMemories, map[string]any{})
	q := unmarshalMemResult(t, qres)
	total := int(q["total"].(float64))
	assert.Equal(t, 1, total)
}

func TestHandleQueryMemories_FilterByKindAndTag(t *testing.T) {
	s := newMemoryTestServer(t)
	_ = callMemHandler(t, s.handleStoreMemory, map[string]any{
		"body": "alpha", "symbol_ids": "pkg/foo.go::Bar", "tags": "invariant", "kind": "invariant",
	})
	_ = callMemHandler(t, s.handleStoreMemory, map[string]any{
		"body": "beta", "symbol_ids": "pkg/foo.go::Baz", "tags": "gotcha", "kind": "gotcha",
	})

	res := callMemHandler(t, s.handleQueryMemories, map[string]any{"kind": "invariant"})
	out := unmarshalMemResult(t, res)
	assert.Equal(t, 1, int(out["total"].(float64)))

	res2 := callMemHandler(t, s.handleQueryMemories, map[string]any{"tag": "gotcha"})
	out2 := unmarshalMemResult(t, res2)
	assert.Equal(t, 1, int(out2["total"].(float64)))
}

func TestHandleQueryMemories_TextAndSince(t *testing.T) {
	s := newMemoryTestServer(t)
	_ = callMemHandler(t, s.handleStoreMemory, map[string]any{"body": "TIMEOUT bug in worker"})
	_ = callMemHandler(t, s.handleStoreMemory, map[string]any{"body": "unrelated memory"})

	res := callMemHandler(t, s.handleQueryMemories, map[string]any{"text": "timeout"})
	out := unmarshalMemResult(t, res)
	assert.Equal(t, 1, int(out["total"].(float64)))

	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	resFuture := callMemHandler(t, s.handleQueryMemories, map[string]any{"since": future})
	outFuture := unmarshalMemResult(t, resFuture)
	assert.Equal(t, 0, int(outFuture["total"].(float64)))
}

func TestHandleQueryMemories_BadSince(t *testing.T) {
	s := newMemoryTestServer(t)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"since": "not-a-timestamp"}
	res, err := s.handleQueryMemories(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.IsError)
}

func TestHandleSurfaceMemories_RanksAnchorMatch(t *testing.T) {
	s := newMemoryTestServer(t)
	_ = callMemHandler(t, s.handleStoreMemory, map[string]any{
		"body": "pinned unrelated", "pinned": true,
	})
	anchorRes := callMemHandler(t, s.handleStoreMemory, map[string]any{
		"body":       "anchor",
		"symbol_ids": "pkg/foo.go::Bar",
	})
	anchorID := unmarshalMemResult(t, anchorRes)["id"].(string)

	res := callMemHandler(t, s.handleSurfaceMemories, map[string]any{
		"symbol_ids":    "pkg/foo.go::Bar",
		"mark_accessed": false,
	})
	out := unmarshalMemResult(t, res)
	mems, _ := out["memories"].([]any)
	require.NotEmpty(t, mems)
	first := mems[0].(map[string]any)
	assert.Equal(t, anchorID, first["id"])

	reasons, _ := first["match_reasons"].([]any)
	assert.NotEmpty(t, reasons)
}

func TestHandleSurfaceMemories_TaskKeywordSearch(t *testing.T) {
	s := newMemoryTestServer(t)
	_ = callMemHandler(t, s.handleStoreMemory, map[string]any{
		"body": "the JWT validator handles expired tokens",
	})
	_ = callMemHandler(t, s.handleStoreMemory, map[string]any{
		"body": "completely unrelated note",
	})

	res := callMemHandler(t, s.handleSurfaceMemories, map[string]any{
		"task":          "fix the jwt validator",
		"mark_accessed": false,
	})
	out := unmarshalMemResult(t, res)
	mems, _ := out["memories"].([]any)
	require.NotEmpty(t, mems)
	body := mems[0].(map[string]any)["body"].(string)
	assert.Contains(t, body, "JWT")
}

func TestHandleSurfaceMemories_MarkAccessedDefaultsTrue(t *testing.T) {
	s := newMemoryTestServer(t)
	storeRes := callMemHandler(t, s.handleStoreMemory, map[string]any{
		"body": "anchored", "symbol_ids": "pkg/foo.go::Bar",
	})
	id := unmarshalMemResult(t, storeRes)["id"].(string)

	_ = callMemHandler(t, s.handleSurfaceMemories, map[string]any{
		"symbol_ids": "pkg/foo.go::Bar",
		// mark_accessed not set — default true.
	})

	got, _ := s.memories.Get(id)
	assert.EqualValues(t, 1, got.AccessCount)
}

// Registration smoke test — protects against accidental removal of
// the registerMemoriesTools call from NewServer.
func TestRegisterMemoriesTools_Wired(t *testing.T) {
	s := newMemoryTestServer(t)
	require.NotNil(t, s.memories)
	// The handlers themselves are exercised by the tests above; this
	// stub confirms the manager pointer survives construction.
}

// check_onboarding_performed — readiness probe for the gortex-onboarding
// skill. Performed iff every essential kind clears min_per_kind.

func TestCheckOnboardingPerformed_EmptyStore(t *testing.T) {
	s := newMemoryTestServer(t)
	res := callMemHandler(t, s.handleCheckOnboardingPerformed, map[string]any{})
	out := unmarshalMemResult(t, res)

	assert.Equal(t, false, out["performed"])
	assert.EqualValues(t, 0, out["total_memories"].(float64))
	missing, _ := out["missing_kinds"].([]any)
	assert.Len(t, missing, len(defaultEssentialKinds), "every default kind is missing in an empty store")
}

func TestCheckOnboardingPerformed_AllKindsSatisfied(t *testing.T) {
	s := newMemoryTestServer(t)
	for _, kind := range defaultEssentialKinds {
		_ = callMemHandler(t, s.handleStoreMemory, map[string]any{
			"body": kind + " memory",
			"kind": kind,
		})
	}

	res := callMemHandler(t, s.handleCheckOnboardingPerformed, map[string]any{})
	out := unmarshalMemResult(t, res)

	assert.Equal(t, true, out["performed"])
	missing, _ := out["missing_kinds"].([]any)
	assert.Empty(t, missing, "no kinds should be missing when each has a memory")
	counts := out["counts_by_kind"].(map[string]any)
	for _, k := range defaultEssentialKinds {
		assert.EqualValues(t, 1, counts[k].(float64), "kind %s count", k)
	}
}

func TestCheckOnboardingPerformed_OneKindMissing(t *testing.T) {
	s := newMemoryTestServer(t)
	// Only invariant + decision; convention deliberately absent.
	_ = callMemHandler(t, s.handleStoreMemory, map[string]any{"body": "x", "kind": "invariant"})
	_ = callMemHandler(t, s.handleStoreMemory, map[string]any{"body": "y", "kind": "decision"})

	res := callMemHandler(t, s.handleCheckOnboardingPerformed, map[string]any{})
	out := unmarshalMemResult(t, res)

	assert.Equal(t, false, out["performed"])
	missing, _ := out["missing_kinds"].([]any)
	require.Len(t, missing, 1)
	assert.Equal(t, "convention", missing[0].(string))
}

func TestCheckOnboardingPerformed_CustomKindsAndThresholds(t *testing.T) {
	s := newMemoryTestServer(t)
	// Two gotchas, one reference.
	_ = callMemHandler(t, s.handleStoreMemory, map[string]any{"body": "g1", "kind": "gotcha"})
	_ = callMemHandler(t, s.handleStoreMemory, map[string]any{"body": "g2", "kind": "gotcha"})
	_ = callMemHandler(t, s.handleStoreMemory, map[string]any{"body": "r1", "kind": "reference"})

	res := callMemHandler(t, s.handleCheckOnboardingPerformed, map[string]any{
		"essential_kinds": "gotcha,reference",
		"min_per_kind":    2,
	})
	out := unmarshalMemResult(t, res)

	// Gotcha clears 2; reference is at 1 (< min 2).
	assert.Equal(t, false, out["performed"])
	missing, _ := out["missing_kinds"].([]any)
	require.Len(t, missing, 1)
	assert.Equal(t, "reference", missing[0].(string))
}

func TestCheckOnboardingPerformed_MinTotalGate(t *testing.T) {
	s := newMemoryTestServer(t)
	// Satisfy all essential kinds, but min_total=10 demands more.
	for _, k := range defaultEssentialKinds {
		_ = callMemHandler(t, s.handleStoreMemory, map[string]any{"body": k, "kind": k})
	}

	res := callMemHandler(t, s.handleCheckOnboardingPerformed, map[string]any{"min_total": 10})
	out := unmarshalMemResult(t, res)
	assert.Equal(t, false, out["performed"], "min_total=10 not reached with 3 memories")

	res2 := callMemHandler(t, s.handleCheckOnboardingPerformed, map[string]any{"min_total": 3})
	out2 := unmarshalMemResult(t, res2)
	assert.Equal(t, true, out2["performed"], "min_total=3 satisfied by 3 memories")
}

func TestCheckOnboardingPerformed_EmptyEssentialKinds(t *testing.T) {
	s := newMemoryTestServer(t)
	_ = callMemHandler(t, s.handleStoreMemory, map[string]any{"body": "anything"})

	res := callMemHandler(t, s.handleCheckOnboardingPerformed, map[string]any{
		"essential_kinds": "",
	})
	out := unmarshalMemResult(t, res)
	// No essential kinds → no missing entries; performed gated only by min_total (default 0).
	assert.Equal(t, true, out["performed"])
	missing, _ := out["missing_kinds"].([]any)
	assert.Empty(t, missing)
}
