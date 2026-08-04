package resolver

import (
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// switchedClock returns t0 while un-flipped. Once flipped, every call
// advances a cumulative two hours — so the very next cutoff check after the
// flip observes a deadline blown mid-pass, regardless of where the pass
// anchored its cutoff. (A static jump cannot model this: the cutoff anchors
// at now() per pass, so a pre-jumped constant clock never trips it.)
type switchedClock struct {
	t0    time.Time
	flip  atomic.Bool
	steps atomic.Int64
}

func (c *switchedClock) now() time.Time {
	if c.flip.Load() {
		return c.t0.Add(time.Duration(c.steps.Add(1)) * 2 * time.Hour)
	}
	return c.t0
}

// flippingLSPHelper answers like slowLSPBudgetHelper but flips the shared
// clock after its first successful answer, so the expensive-path cutoff
// trips deterministically at the next per-page check site.
type flippingLSPHelper struct {
	clock     *switchedClock
	flipAfter int
	defPath   string
	defLine   int
	mu        sync.Mutex
	calls     []string
}

func (h *flippingLSPHelper) SupportsPath(string) bool { return true }

func (h *flippingLSPHelper) Definition(_ string, _ int, name string) (string, int, bool) {
	h.mu.Lock()
	h.calls = append(h.calls, name)
	n := len(h.calls)
	h.mu.Unlock()
	if h.clock != nil && n >= h.flipAfter {
		h.clock.flip.Store(true)
	}
	return h.defPath, h.defLine, true
}

func (h *flippingLSPHelper) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func lspSpoolCounts(t *testing.T, spool *deferredLSPSpool) (total, carried int) {
	t.Helper()
	require.NotNil(t, spool)
	require.NoError(t, spool.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(carried),0) FROM work`).Scan(&total, &carried))
	return total, carried
}

// The cutoff must stop the expensive per-page path after the page in flight:
// later pages are drained record-only — no hydration, no helper calls — with
// every row retained as carried, exclusions recorded, and the resume cursor
// pointing at the first unprocessed key.
func TestResolveAllLSPPhaseCutoffDrainsRemainingPages(t *testing.T) {
	g, edges := lspBudgetGraph("Alpha", "Beta", "Gamma", "Delta", "Epsilon", "Zeta")
	g.AddNode(&graph.Node{
		ID: "src/target.ts::Target", Kind: graph.KindFunction, Name: "Target",
		FilePath: "src/target.ts", StartLine: 50, Language: "typescript",
	})
	clock := &switchedClock{t0: time.Now()}
	helper := &flippingLSPHelper{clock: clock, flipAfter: 2, defPath: "src/target.ts", defLine: 50}
	r := New(g)
	r.SetLSPHelper(helper)
	r.SetLSPResolvePassBudget(15 * time.Second)
	r.SetStampTerminal(true)
	r.lspSpoolPageRows = 2
	r.lspNow = clock.now

	stats := r.ResolveAll()

	assert.Equal(t, 2, stats.LSPAttempted, "only the first page's edges may attempt")
	assert.Equal(t, 2, stats.LSPResolved)
	assert.Equal(t, 2, helper.callCount(), "drained pages must never invoke the helper")
	assert.False(t, stats.LSPBudgetExhausted, "the attempt budget never expired — this is the cutoff")

	resolved := 0
	for _, edge := range edges {
		if edge.Origin == graph.OriginLSPResolved {
			resolved++
		} else {
			assert.True(t, graph.IsUnresolvedTarget(edge.To))
			assert.False(t, edgeTerminalFlag(edge), "drained edge %s must not be stamped terminal", edge.To)
		}
	}
	assert.Equal(t, 2, resolved)

	require.NotNil(t, r.lspDeferredSpool, "drained rows must keep the spool alive")
	total, carried := lspSpoolCounts(t, r.lspDeferredSpool)
	assert.Equal(t, 4, total, "all drained rows retained")
	assert.Equal(t, 4, carried, "all drained rows marked carried")
	require.True(t, r.lspDeferredCursorSet)
	assert.Equal(t, deferredLSPWorkKeyForEdge(edges[2]), r.lspDeferredCursor,
		"cursor must resume at the first drained key")
}

// Cutoff tripping before ANY attempt: nothing may be hydrated or attempted,
// every row is retained, and the cursor still pins the first unprocessed key.
func TestResolveAllLSPCutoffBeforeFirstAttemptPreservesCursor(t *testing.T) {
	g, edges := lspBudgetGraph("Alpha", "Beta", "Gamma", "Delta")
	clock := &switchedClock{t0: time.Now()}
	clock.flip.Store(true) // past the cutoff from the very first check
	helper := &flippingLSPHelper{defPath: "src/target.ts", defLine: 50}
	r := New(g)
	r.SetLSPHelper(helper)
	r.SetLSPResolvePassBudget(15 * time.Second)
	r.SetStampTerminal(true)
	r.lspSpoolPageRows = 2
	r.lspNow = clock.now

	stats := r.ResolveAll()

	assert.Zero(t, stats.LSPAttempted)
	assert.Zero(t, helper.callCount())
	require.NotNil(t, r.lspDeferredSpool)
	total, carried := lspSpoolCounts(t, r.lspDeferredSpool)
	assert.Equal(t, len(edges), total)
	assert.Equal(t, len(edges), carried)
	require.True(t, r.lspDeferredCursorSet)
	assert.Equal(t, deferredLSPWorkKeyForEdge(edges[0]), r.lspDeferredCursor)
	for _, edge := range edges {
		assert.False(t, edgeTerminalFlag(edge), "drained edge %s must not be stamped terminal", edge.To)
	}
}

// Zero attempt budget preserves the historical unlimited mode: no cutoff,
// even under a clock far past any derived deadline.
func TestResolveAllLSPZeroBudgetDisablesCutoff(t *testing.T) {
	g, edges := lspBudgetGraph("Alpha", "Beta", "Gamma")
	g.AddNode(&graph.Node{
		ID: "src/target.ts::Target", Kind: graph.KindFunction, Name: "Target",
		FilePath: "src/target.ts", StartLine: 50, Language: "typescript",
	})
	clock := &switchedClock{t0: time.Now()}
	clock.flip.Store(true)
	helper := &flippingLSPHelper{defPath: "src/target.ts", defLine: 50}
	r := New(g)
	r.SetLSPHelper(helper)
	r.SetLSPResolvePassBudget(0)
	r.lspSpoolPageRows = 2
	r.lspNow = clock.now

	stats := r.ResolveAll()

	assert.Equal(t, len(edges), stats.LSPAttempted)
	assert.Equal(t, len(edges), stats.LSPResolved)
	for _, edge := range edges {
		assert.Equal(t, graph.OriginLSPResolved, edge.Origin)
	}
}

// Two consecutive drained passes must hold the cursor stable at the first
// unprocessed key (no starvation, no skip-ahead); a later healthy pass
// resumes there, wraps, and finishes everything.
func TestResolveAllLSPConsecutiveDrainedPassesKeepCursorThenRecover(t *testing.T) {
	g, edges := lspBudgetGraph("Alpha", "Beta", "Gamma", "Delta", "Epsilon", "Zeta")
	g.AddNode(&graph.Node{
		ID: "src/target.ts::Target", Kind: graph.KindFunction, Name: "Target",
		FilePath: "src/target.ts", StartLine: 50, Language: "typescript",
	})
	clock := &switchedClock{t0: time.Now()}
	helper := &flippingLSPHelper{clock: clock, flipAfter: 2, defPath: "src/target.ts", defLine: 50}
	r := New(g)
	r.SetLSPHelper(helper)
	r.SetLSPResolvePassBudget(15 * time.Second)
	r.lspSpoolPageRows = 2
	r.lspNow = clock.now

	// Pass 1: page 1 attempts, pages 2-3 drain. Cursor = first drained key.
	r.ResolveAll()
	require.True(t, r.lspDeferredCursorSet)
	firstDrained := deferredLSPWorkKeyForEdge(edges[2])
	assert.Equal(t, firstDrained, r.lspDeferredCursor)

	// Pass 2: still past the cutoff from the start — a fully drained pass.
	// The cursor must remain exactly where pass 1 stopped.
	stats2 := r.ResolveAll()
	assert.Zero(t, stats2.LSPAttempted)
	require.True(t, r.lspDeferredCursorSet)
	assert.Equal(t, firstDrained, r.lspDeferredCursor, "a drained pass must not move the resume cursor")

	// Pass 3: healthy clock — resumes at the cursor, wraps, resolves the rest.
	clock.flip.Store(false)
	helper.clock = nil
	stats3 := r.ResolveAll()
	assert.Equal(t, 4, stats3.LSPAttempted, "healthy pass resumes exactly the drained tail")
	for _, edge := range edges {
		assert.Equal(t, graph.OriginLSPResolved, edge.Origin, "edge %s must resolve after recovery", edge.From)
	}
	assert.Nil(t, r.lspDeferredSpool, "empty spool must close after full recovery")
	assert.False(t, r.lspDeferredCursorSet)
}

// Compute readiness must be published BEFORE the deferred LSP batch: the
// batch's store-standing yield measured 0.19% of the pending set while its
// cold wall was minutes, and time-to-queryable gates on this hook. The batch
// still runs — after the hook, like the guard and cross-repo tails.
func TestOnComputeDoneFiresBeforeDeferredLSPBatch(t *testing.T) {
	g, edges := lspBudgetGraph("Alpha", "Beta")
	g.AddNode(&graph.Node{
		ID: "src/target.ts::Target", Kind: graph.KindFunction, Name: "Target",
		FilePath: "src/target.ts", StartLine: 50, Language: "typescript",
	})
	helper := &flippingLSPHelper{defPath: "src/target.ts", defLine: 50}
	r := New(g)
	r.SetLSPHelper(helper)
	r.SetLSPResolvePassBudget(0)
	attemptsAtHook := -1
	r.OnComputeDone = func() { attemptsAtHook = helper.callCount() }

	r.ResolveAll()

	require.Zero(t, attemptsAtHook, "readiness hook must fire before any LSP attempt")
	assert.Equal(t, len(edges), helper.callCount(), "the batch must still run after the hook")
}

// A guard revert must re-snapshot the spool record to the post-revert edge
// state; without the refresh the next pass's exact matching declares the row
// stale and silently drops the queued verify.
func TestGuardRevertRefreshSpoolRecordStaysLive(t *testing.T) {
	g := graph.New()
	edge := &graph.Edge{
		From: "src/a.ts::CallerFoo", To: "unresolved::Foo", Kind: graph.EdgeCalls,
		FilePath: "src/a.ts", Line: 7,
	}
	g.AddEdge(edge)

	spool, err := newDeferredLSPSpool()
	require.NoError(t, err)
	defer spool.close()

	// Heuristic bind happens before the spool snapshot — the record observes
	// the BOUND state, exactly like a verify-record in production.
	oldBound := "pkg/b.ts::Foo"
	edge.To = oldBound
	edge.Confidence = 0.7
	edge.Origin = "heuristic"
	require.NoError(t, spool.append([]deferredLSPEdge{{edge: edge, target: "Foo"}}))

	// Guard revert: restore the placeholder, drop confidence + provenance.
	edge.To = "unresolved::Foo"
	edge.Confidence = 0
	edge.Origin = ""

	// Without the refresh, hydration declares the record stale (negative
	// control proving the test can see the hole).
	records, _, err := spool.iterator(nil).next(16)
	require.NoError(t, err)
	require.Len(t, records, 1)
	_, stale := lspEdgesFromRecords(g, records, nil)
	require.Len(t, stale, 1, "un-refreshed record must be stale after a revert")

	require.NoError(t, spool.refreshRevertedEdges([]lspSpoolRevert{{edge: edge, oldBoundTo: oldBound}}))
	records, _, err = spool.iterator(nil).next(16)
	require.NoError(t, err)
	require.Len(t, records, 1)
	live, stale := lspEdgesFromRecords(g, records, nil)
	assert.Empty(t, stale, "refreshed record must match the reverted edge")
	require.Len(t, live, 1)
	assert.Same(t, edge, live[0].edge)
	assert.Equal(t, "Foo", live[0].target)
}

// Reconciliation must match exclusions through the To-less work key: an edge
// guard-reverted after its exclusion was recorded still matches, is never
// stamped, and a pre-existing stamp is cleared.
func TestReconcileExclusionMatchesGuardRevertedEdge(t *testing.T) {
	g := graph.New()
	edge := &graph.Edge{
		From: "src/a.ts::CallerFoo", To: "unresolved::Foo", Kind: graph.EdgeCalls,
		FilePath: "src/a.ts", Line: 7,
	}
	g.AddEdge(edge)
	setEdgeTerminal(edge, "test-stale-stamp")
	require.True(t, edgeTerminalFlag(edge))

	// The exclusion was recorded while the edge was heuristically BOUND —
	// its key must still match the now-reverted (unresolved) live edge.
	bound := *edge
	bound.To = "pkg/b.ts::Foo"
	excluded := map[deferredLSPWorkKey]struct{}{
		{filePath: edge.FilePath, line: edge.Line, from: edge.From, kind: edge.Kind, target: "Foo"}: {},
	}
	r := New(g)
	_, unstamped := r.reconcileTerminalStampsExcluding(excluded)
	assert.GreaterOrEqual(t, unstamped, 1)
	assert.False(t, edgeTerminalFlag(edge), "excluded edge must have its stale stamp cleared")
	_ = bound
}

// Spool mutation failures must never lose retry work: operations on a broken
// spool error out, and the rows remain durable in the on-disk file.
func TestSpoolFailureLeavesRowsDurable(t *testing.T) {
	spool, err := newDeferredLSPSpool()
	require.NoError(t, err)
	path := spool.path
	g, edges := lspBudgetGraph("Alpha", "Beta")
	_ = g
	require.NoError(t, spool.append([]deferredLSPEdge{
		{edge: edges[0], target: "Alpha"},
		{edge: edges[1], target: "Beta"},
	}))
	require.NoError(t, spool.db.Close())

	keys := []deferredLSPWorkKey{deferredLSPWorkKeyForEdge(edges[0])}
	assert.Error(t, spool.markCarried(keys))
	assert.Error(t, spool.deleteKeys(keys))
	assert.Error(t, spool.markCarriedRange(nil, nil))
	_, _, err = spool.keysPage(nil, nil, 4)
	assert.Error(t, err)

	reopened, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer reopened.Close()
	var total int
	require.NoError(t, reopened.QueryRow(`SELECT COUNT(*) FROM work`).Scan(&total))
	assert.Equal(t, 2, total, "no retry work may disappear when spool mutation fails")
}

// One shared normalization on both sides of the exclusion match, across the
// awkward target shapes: member calls, extern paths, and two different
// targets on the same source line producing distinct keys.
func TestExclusionKeyNormalizationShapes(t *testing.T) {
	mk := func(to string, line int) *graph.Edge {
		return &graph.Edge{
			From: "src/a.ts::Caller", To: to, Kind: graph.EdgeCalls,
			FilePath: "src/a.ts", Line: line,
		}
	}
	memberEdge := mk("unresolved::*.member", 3)
	externEdge := mk("unresolved::extern::path::symbol", 4)
	lineA := mk("unresolved::Alpha", 9)
	lineB := mk("unresolved::Beta", 9)

	for _, edge := range []*graph.Edge{memberEdge, externEdge, lineA, lineB} {
		spoolTarget := lspTargetFromUnresolvedTo(edge.To)
		spoolKey := deferredLSPWorkKeyFor(deferredLSPEdge{edge: edge, target: spoolTarget})
		liveKey := deferredLSPWorkKeyForEdge(edge)
		assert.Equal(t, spoolKey, liveKey, "spool and reconcile keys must agree for %s", edge.To)
	}
	assert.Equal(t, "*.member", lspTargetFromUnresolvedTo(memberEdge.To))
	assert.Equal(t, "extern::path::symbol", lspTargetFromUnresolvedTo(externEdge.To))
	assert.NotEqual(t, deferredLSPWorkKeyForEdge(lineA), deferredLSPWorkKeyForEdge(lineB),
		"two targets on one line must produce distinct exclusion keys")
}

// A pre-existing terminal stamp on a drained row is not cleared by the drain
// itself — the parity proof that this deferral is safe: the row stays
// carried, the next hydrated visit clears the stamp, and the retry succeeds.
func TestDrainedStampClearsOnNextHydratedVisit(t *testing.T) {
	g, edges := lspBudgetGraph("Alpha", "Beta")
	g.AddNode(&graph.Node{
		ID: "src/target.ts::Target", Kind: graph.KindFunction, Name: "Target",
		FilePath: "src/target.ts", StartLine: 50, Language: "typescript",
	})
	setEdgeTerminal(edges[1], "pre-fix-stale-stamp")

	clock := &switchedClock{t0: time.Now()}
	clock.flip.Store(true)
	helper := &flippingLSPHelper{defPath: "src/target.ts", defLine: 50}
	r := New(g)
	r.SetLSPHelper(helper)
	r.SetLSPResolvePassBudget(15 * time.Second)
	r.SetStampTerminal(true)
	r.lspSpoolPageRows = 2
	r.lspNow = clock.now

	// Fully drained pass: the full-pass reconcile runs with drain exclusions
	// and already clears the stale stamp (exclusion-driven un-stamping).
	r.ResolveAll()
	assert.False(t, edgeTerminalFlag(edges[1]),
		"full-pass reconcile must un-stamp an excluded drained edge")

	// Recovery pass hydrates and resolves everything.
	clock.flip.Store(false)
	r.ResolveAll()
	for _, edge := range edges {
		assert.Equal(t, graph.OriginLSPResolved, edge.Origin)
		assert.False(t, edgeTerminalFlag(edge))
	}
}
