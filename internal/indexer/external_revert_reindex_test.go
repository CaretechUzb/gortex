package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// setupRevertWatcher indexes a two-file Go repo where caller.go::Use calls
// debug.go::IsDebugging across files, stamps that resolved caller edge
// lsp_resolved (the tier the semantic-enrichment pass mints once), and returns
// the watcher driving the live patch path plus the definition-file path and its
// stable node ID.
//
// It mirrors the external staleness probe: a definition with a cross-file
// caller, fully resolved before any live edit arrives.
func setupRevertWatcher(t *testing.T) (w *Watcher, g graph.Store, defPath, defID string) {
	t.Helper()
	dir := t.TempDir()
	defPath = filepath.Join(dir, "debug.go")
	callerPath := filepath.Join(dir, "caller.go")
	writeFile(t, defPath, "package p\n\nfunc IsDebugging() {}\n")
	writeFile(t, callerPath, "package p\n\nfunc Use() { IsDebugging() }\n")

	g = newSqliteGraph(t)
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(dir)
	_, err := idx.IndexCtx(testCtx(), dir)
	require.NoError(t, err)

	defID = fnNodeID(t, g, "debug.go", "IsDebugging")
	useID := fnNodeID(t, g, "caller.go", "Use")
	require.Equal(t, defID, callTargetFrom(t, g, useID),
		"cross-file caller must resolve to the definition before the revert")
	stampLSP(t, g, callEdgeFrom(t, g, useID))

	w, err = NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	return w, g, defPath, defID
}

// assertCrossFileCallerHealthy asserts the definition node still resolves and
// its single surviving incoming caller edge (caller.go::Use) is bound to it
// with its lsp tier intact — i.e. find_usages on the definition would return
// the cross-file caller, not a silent zero.
func assertCrossFileCallerHealthy(t *testing.T, g graph.Store, defID string) {
	t.Helper()
	require.NotNilf(t, g.GetNode(defID),
		"definition node %s must still resolve after the external revert", defID)
	incoming := incomingCallEdges(t, g, defID)
	require.Lenf(t, incoming, 1,
		"the cross-file caller edge must be restored, not stubbed/dropped (got %d incoming call edges)", len(incoming))
	assert.Falsef(t, graph.IsUnresolvedTarget(incoming[0].To),
		"restored incoming caller edge must not be a stub")
	assert.Equalf(t, defID, incoming[0].To,
		"restored incoming caller edge must resolve back to the definition")
	assert.Equalf(t, graph.OriginLSPResolved, incoming[0].Origin,
		"restored incoming caller edge must keep its lsp tier (a demotion reads as a find_usages zero)")
}

// TestPatchGraph_ExternalAdd_InPlaceWrite_KeepsCrossFileCaller locks in the
// external-ADD path (an in-place open(truncate)+write that keeps the inode):
// the watcher classifies it as ChangeModified, and the parse-then-swap +
// incoming re-resolve must keep the cross-file caller edge resolved while
// adding the new probe call site. This must not regress.
func TestPatchGraph_ExternalAdd_InPlaceWrite_KeepsCrossFileCaller(t *testing.T) {
	w, g, defPath, defID := setupRevertWatcher(t)

	// In-place truncate+write: append a compilable probe that also calls
	// the definition. Same inode; the watcher sees a bare modify.
	writeFile(t, defPath, "package p\n\nfunc IsDebugging() {}\n\nfunc _probe() { IsDebugging() }\n")
	require.NoError(t, w.patchGraph(defPath, ChangeModified))

	require.NotNil(t, g.GetNode(defID), "IsDebugging survives the in-place ADD")
	// Two callers now: cross-file Use and intra-file _probe.
	require.Len(t, incomingCallEdges(t, g, defID), 2,
		"both the cross-file and the new intra-file caller must resolve after the ADD")
	// The cross-file caller specifically must keep its lsp tier.
	useID := fnNodeID(t, g, "caller.go", "Use")
	useEdge := callEdgeFrom(t, g, useID)
	assert.False(t, graph.IsUnresolvedTarget(useEdge.To), "cross-file caller stays resolved after ADD")
	assert.Equal(t, graph.OriginLSPResolved, useEdge.Origin, "cross-file caller keeps its lsp tier after ADD")
}

// TestPatchGraph_ExternalRevert_RenameOver_KeepsCrossFileCaller reproduces the
// git-checkout revert shape: git writes a sibling temp file and os.Renames it
// over the target, so the worktree file gets a NEW inode. FSEvents accumulates
// ItemRemoved|ItemCreated|ItemModified flags for the path, and pickKind — which
// ranks Remove/Rename above Modify — reduces that to ChangeDeleted. So the live
// watcher delivers patchGraph(path, ChangeDeleted) even though a file is right
// back at the same path. Pre-fix that hard-evicts the definition and stubs its
// cross-file callers with nothing to rebind them: find_usages goes silently to
// zero. The file's on-disk presence at patch time must reclassify this as a
// modify so the definition and its callers are restored.
func TestPatchGraph_ExternalRevert_RenameOver_KeepsCrossFileCaller(t *testing.T) {
	w, g, defPath, defID := setupRevertWatcher(t)
	dir := filepath.Dir(defPath)

	// ADD first (in-place), so the revert has something to undo.
	writeFile(t, defPath, "package p\n\nfunc IsDebugging() {}\n\nfunc _probe() { IsDebugging() }\n")
	require.NoError(t, w.patchGraph(defPath, ChangeModified))
	require.Len(t, incomingCallEdges(t, g, defID), 2, "sanity: two callers after ADD")

	// REVERT via rename-over: new inode, file present at the same path.
	tmp := filepath.Join(dir, "debug.go.tmp")
	writeFile(t, tmp, "package p\n\nfunc IsDebugging() {}\n")
	require.NoError(t, os.Rename(tmp, defPath))
	require.NoError(t, w.patchGraph(defPath, ChangeDeleted))

	// The probe caller is gone with the revert; the cross-file caller must
	// survive with its lsp tier.
	assertCrossFileCallerHealthy(t, g, defID)
}

// TestPatchGraph_ExternalRevert_UnlinkRecreate_KeepsCrossFileCaller is the
// unlink+recreate variant of the same revert: git removes the worktree file
// and writes a fresh one at the same path (also a new inode). FSEvents shows
// ItemRemoved|ItemRenamed|ItemCreated; pickKind may reduce it to
// ChangeRenamed. The live watcher then delivers patchGraph(path, ChangeRenamed)
// for a path that exists on disk. Same requirement: reclassify as a modify and
// restore the definition + its cross-file callers.
func TestPatchGraph_ExternalRevert_UnlinkRecreate_KeepsCrossFileCaller(t *testing.T) {
	w, g, defPath, defID := setupRevertWatcher(t)

	// ADD first (in-place).
	writeFile(t, defPath, "package p\n\nfunc IsDebugging() {}\n\nfunc _probe() { IsDebugging() }\n")
	require.NoError(t, w.patchGraph(defPath, ChangeModified))
	require.Len(t, incomingCallEdges(t, g, defID), 2, "sanity: two callers after ADD")

	// REVERT via unlink + recreate.
	require.NoError(t, os.Remove(defPath))
	writeFile(t, defPath, "package p\n\nfunc IsDebugging() {}\n")
	require.NoError(t, w.patchGraph(defPath, ChangeRenamed))

	assertCrossFileCallerHealthy(t, g, defID)
}

// TestPatchGraph_TrueDelete_StillEvicts guards the reconciliation: a genuine
// deletion (the file is gone from disk) must still evict the definition and
// drop its incoming callers to stubs. The disk-existence check must not turn a
// real delete into a no-op modify.
func TestPatchGraph_TrueDelete_StillEvicts(t *testing.T) {
	w, g, defPath, defID := setupRevertWatcher(t)

	require.NoError(t, os.Remove(defPath))
	require.NoError(t, w.patchGraph(defPath, ChangeDeleted))

	assert.Nil(t, g.GetNode(defID), "a genuine delete must evict the definition")
	for _, e := range incomingCallEdges(t, g, defID) {
		assert.True(t, graph.IsUnresolvedTarget(e.To),
			"a genuine delete must leave its former callers on unresolved stubs")
	}
}
