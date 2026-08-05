package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// fnNodeID returns the function/method node ID named `name` defined in
// graph file `file`, failing the test if it is absent.
func fnNodeID(t *testing.T, g graph.Store, file, name string) string {
	t.Helper()
	for _, n := range g.GetFileNodes(file) {
		if n.Name == name && (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) {
			return n.ID
		}
	}
	t.Fatalf("function %q in %s not found", name, file)
	return ""
}

// callTargetFrom returns the To of the (single) EdgeCalls edge leaving
// node `fromID`.
func callTargetFrom(t *testing.T, g graph.Store, fromID string) string {
	t.Helper()
	return callEdgeFrom(t, g, fromID).To
}

// callEdgeFrom returns the (single) EdgeCalls edge pointer leaving node
// `fromID`, failing the test if there is none.
func callEdgeFrom(t *testing.T, g graph.Store, fromID string) *graph.Edge {
	t.Helper()
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind == graph.EdgeCalls {
			return e
		}
	}
	t.Fatalf("no call edge from %s", fromID)
	return nil
}

// TestIncrementalReindex_PreservesIncomingCallerEdges is the proof of
// the reverse-resolution + un-resolve fix. When file A defines Foo and
// file B calls it, B's call edge resolves to A.Foo. Re-indexing or
// deleting A must NOT silently drop B's edge:
//
//   - re-indexing A (Foo unchanged): restubIncomingRefs re-stubs B's
//     edge to unresolved::Foo before A is evicted, then
//     ResolveIncomingForFile rebinds it to A's fresh Foo — so B's caller
//     edge survives a definition edit.
//   - deleting A: B's edge survives as an unresolved::Foo stub (the
//     correct state for a call to a now-missing symbol), not dropped.
//   - re-creating A: ResolveIncomingForFile rebinds the pending stub.
//
// Against the pre-fix code, step (1) FAILS: evicting A drops B's
// incoming caller edge wholesale and ResolveFile(A) only touches A's
// outgoing edges, so get_callers(Foo) goes blank until a cold reindex.
func TestIncrementalReindex_PreservesIncomingCallerEdges(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.go")
	bPath := filepath.Join(dir, "b.go")
	writeFile(t, aPath, "package p\n\nfunc Foo() {}\n")
	writeFile(t, bPath, "package p\n\nfunc Bar() { Foo() }\n")

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewBM25()
	idx.SetRootPath(dir)
	_, err := idx.IndexCtx(testCtx(), dir)
	require.NoError(t, err)

	fooID := fnNodeID(t, g, "a.go", "Foo")
	barID := fnNodeID(t, g, "b.go", "Bar")

	require.Equal(t, fooID, callTargetFrom(t, g, barID),
		"baseline: Bar's call must resolve to Foo")

	// (1) Re-index the DEFINITION file with Foo unchanged. The caller
	// edge in b.go must survive.
	require.NoError(t, idx.IndexFile(aPath))
	assert.Equal(t, fooID, callTargetFrom(t, g, barID),
		"re-indexing Foo's own file must not drop Bar's caller edge")

	// (2) Delete the definition. Bar's edge must revert to an unresolved
	// stub, not vanish.
	idx.EvictFile(aPath)
	deletedTarget := callTargetFrom(t, g, barID)
	assert.True(t, graph.IsUnresolvedTarget(deletedTarget),
		"deleting Foo must leave Bar's call as an unresolved stub, not drop it")
	assert.Equal(t, "Foo", graph.UnresolvedName(deletedTarget),
		"the re-stubbed target must carry Foo's name")

	// (3) Re-create the definition. The pending stub must rebind.
	require.NoError(t, idx.IndexFile(aPath))
	rebound := fnNodeID(t, g, "a.go", "Foo")
	assert.Equal(t, rebound, callTargetFrom(t, g, barID),
		"re-adding Foo must rebind Bar's pending caller edge via the reverse pass")
}

// TestIncrementalReindex_DoubleCycle_PreservesIncomingTier is the F3
// regression: re-parsing a definition file twice (append then revert — the
// exact staleness-probe sequence) must not ratchet an incoming caller edge's
// tier down or leave it a stub. Before the fix the first cycle preserved the
// caller edge but the second could collapse it; this asserts stability across
// BOTH cycles.
func TestIncrementalReindex_DoubleCycle_PreservesIncomingTier(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.go")
	bPath := filepath.Join(dir, "b.go")
	writeFile(t, aPath, "package p\n\nfunc Foo() {}\n")
	writeFile(t, bPath, "package p\n\nfunc Bar() { Foo() }\n")

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewBM25()
	idx.SetRootPath(dir)
	_, err := idx.IndexCtx(testCtx(), dir)
	require.NoError(t, err)

	fooID := fnNodeID(t, g, "a.go", "Foo")
	barID := fnNodeID(t, g, "b.go", "Bar")
	require.Equal(t, fooID, callTargetFrom(t, g, barID))

	// Stamp B's incoming call edge compiler-grade.
	e := callEdgeFrom(t, g, barID)
	g.SetEdgeProvenance(e, graph.OriginLSPResolved)
	e.Tier = graph.ResolvedBy(graph.OriginLSPResolved)

	cycles := []string{
		"package p\n\nfunc Foo() {}\n\nfunc Extra() {}\n", // append
		"package p\n\nfunc Foo() {}\n",                    // revert
	}
	for i, content := range cycles {
		writeFile(t, aPath, content)
		require.NoError(t, idx.IndexFile(aPath))
		after := callEdgeFrom(t, g, fnNodeID(t, g, "b.go", "Bar"))
		assert.Falsef(t, graph.IsUnresolvedTarget(after.To),
			"cycle %d: Bar's caller edge must not remain a stub", i)
		assert.Equalf(t, fnNodeID(t, g, "a.go", "Foo"), after.To,
			"cycle %d: Bar's caller edge must still resolve to Foo", i)
		assert.Equalf(t, graph.OriginLSPResolved, after.Origin,
			"cycle %d: incoming edge must keep its lsp origin (no ratchet-down)", i)
	}
}

// TestIncrementalReindex_DeletedDefinition_NoStaleTierOnStub locks F3's
// provenance honesty: when a definition is genuinely deleted, the incoming
// caller edge reverts to an unresolved stub and must NOT keep claiming the
// compiler-grade tier it carried while bound — an unresolved stub advertising
// lsp_resolved is a provenance lie.
func TestIncrementalReindex_DeletedDefinition_NoStaleTierOnStub(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.go")
	bPath := filepath.Join(dir, "b.go")
	writeFile(t, aPath, "package p\n\nfunc Foo() {}\n")
	writeFile(t, bPath, "package p\n\nfunc Bar() { Foo() }\n")

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewBM25()
	idx.SetRootPath(dir)
	_, err := idx.IndexCtx(testCtx(), dir)
	require.NoError(t, err)

	barID := fnNodeID(t, g, "b.go", "Bar")
	e := callEdgeFrom(t, g, barID)
	g.SetEdgeProvenance(e, graph.OriginLSPResolved)
	e.Tier = graph.ResolvedBy(graph.OriginLSPResolved)

	idx.EvictFile(aPath)

	stub := callEdgeFrom(t, g, barID)
	require.True(t, graph.IsUnresolvedTarget(stub.To),
		"deleting Foo must leave Bar's call as an unresolved stub")
	assert.NotEqual(t, graph.OriginLSPResolved, stub.Origin,
		"an unresolved stub must not keep advertising a compiler-grade origin")
	assert.Empty(t, stub.Tier,
		"an unresolved stub must not keep a resolved tier")
}

// TestEvictFile_DropsEnrichmentSidecars proves the change-A eviction
// cascade: deleting a file drops its nodes' churn/coverage/blame
// sidecar rows, leaving no orphan enrichment.
func TestEvictFile_DropsEnrichmentSidecars(t *testing.T) {
	idx, _ := newToggleIndexer(t)
	dir := t.TempDir()
	idx.SetRootPath(dir)
	g := idx.graph

	g.AddBatch([]*graph.Node{
		{ID: "main.fk", Kind: graph.KindFile, Name: "main.fk", FilePath: "main.fk"},
		{ID: "main.fk::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "main.fk"},
	}, nil)
	require.NoError(t, g.(graph.ChurnEnrichmentWriter).BulkSetChurn("", []graph.ChurnEnrichment{{NodeID: "main.fk::Foo", CommitCount: 3}}))
	require.NoError(t, g.(graph.CoverageEnrichmentWriter).BulkSetCoverage("", []graph.CoverageEnrichment{{NodeID: "main.fk::Foo", CoveragePct: 50}}))
	require.NoError(t, g.(graph.BlameEnrichmentWriter).BulkSetBlame("", []graph.BlameEnrichment{{NodeID: "main.fk::Foo", Email: "x@y"}}))

	require.NotEmpty(t, g.(graph.ChurnEnrichmentReader).ChurnRows(""), "churn seeded")

	idx.EvictFile("main.fk")

	assert.Empty(t, g.(graph.ChurnEnrichmentReader).ChurnRows(""), "churn rows must be evicted with the file")
	assert.Empty(t, g.(graph.CoverageEnrichmentReader).CoverageRows(""), "coverage rows must be evicted")
	assert.Empty(t, g.(graph.BlameEnrichmentReader).BlameRows(""), "blame rows must be evicted")
}

// TestIncrementalReuse_SameFileEdge_KeepsTier is the F1 regression: a call
// whose target lives in the SAME file must keep its resolved provenance across
// a structural re-parse of that file. Before the fix, eviction removed the
// target node before applyResolvedOutEdges ran, so the same-file edge missed
// reuse (GetNode(v.to) == nil) and the forward resolver rebound it at the
// heuristic default — silently demoting an lsp_resolved edge, which find_usages
// then suppresses. Node IDs are file::Name (line/content-independent), so an
// EOF append re-adds the target under an identical ID and the reuse must
// recover the prior resolution AND its tier.
func TestIncrementalReuse_SameFileEdge_KeepsTier(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.go")
	writeFile(t, aPath, "package p\n\nfunc Foo() {}\n\nfunc Baz() { Foo() }\n")

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewBM25()
	idx.SetRootPath(dir)
	_, err := idx.IndexCtx(testCtx(), dir)
	require.NoError(t, err)

	fooID := fnNodeID(t, g, "a.go", "Foo")
	bazID := fnNodeID(t, g, "a.go", "Baz")
	require.Equal(t, fooID, callTargetFrom(t, g, bazID),
		"baseline: Baz's same-file call must resolve to Foo")

	// Stamp the same-file call edge compiler-grade (lsp), the tier the
	// semantic-enrichment pass mints once at track time.
	e := callEdgeFrom(t, g, bazID)
	g.SetEdgeProvenance(e, graph.OriginLSPResolved)
	e.Tier = graph.ResolvedBy(graph.OriginLSPResolved)
	e.Confidence = 1.0

	// EOF-append a new function: a structural edit that drives the single-file
	// incremental reindex (evict -> re-parse -> reuse -> resolve) without
	// shifting Foo's or Baz's lines.
	writeFile(t, aPath, "package p\n\nfunc Foo() {}\n\nfunc Baz() { Foo() }\n\nfunc Extra() {}\n")
	require.NoError(t, idx.IndexFile(aPath))

	// The call still points at Foo AND keeps its resolved provenance — not
	// demoted to the resolver's heuristic default.
	after := callEdgeFrom(t, g, fnNodeID(t, g, "a.go", "Baz"))
	assert.Equal(t, fnNodeID(t, g, "a.go", "Foo"), after.To,
		"same-file call must still resolve to Foo after the re-parse")
	assert.Equal(t, graph.OriginLSPResolved, after.Origin,
		"same-file edge must keep its lsp_resolved origin across the re-parse (F1)")
	assert.Equal(t, graph.ResolvedBy(graph.OriginLSPResolved), after.Tier,
		"same-file edge must keep its lsp tier across the re-parse (F1)")
}

// TestSetReparsePendingEnrichment_SetAndClear covers the F2.2 file-node marker
// the MCP find_usages rider reads: it sets the marker on the KindFile node and
// clears it, and is a no-op when the marker is already in the desired state.
func TestSetReparsePendingEnrichment_SetAndClear(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go"})
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())

	fileMeta := func() map[string]any {
		for _, n := range g.GetFileNodes("a.go") {
			if n.Kind == graph.KindFile {
				return n.Meta
			}
		}
		t.Fatalf("file node a.go not found")
		return nil
	}

	_, had := fileMeta()[graph.MetaReparsePendingEnrichment]
	require.False(t, had, "marker absent on a freshly-added file node")

	idx.setReparsePendingEnrichment("a.go", true)
	_, had = fileMeta()[graph.MetaReparsePendingEnrichment]
	assert.True(t, had, "marker must be set when the live re-parse skipped enrichment")

	idx.setReparsePendingEnrichment("a.go", false)
	_, had = fileMeta()[graph.MetaReparsePendingEnrichment]
	assert.False(t, had, "marker must be cleared when enrichment re-ran")
}
