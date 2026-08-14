package indexer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// TestCountResolvedFileEdges proves the resolved-edge count excludes stub
// (`unresolved::`) targets — the signal countFileEdges cannot give, since an
// edge demoted from a concrete target to a stub keeps the incident-edge total
// identical.
func TestCountResolvedFileEdges(t *testing.T) {
	idx, _, _ := newToggleIndexer(t)
	g := idx.graph
	g.AddBatch([]*graph.Node{
		{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go"},
		{ID: "a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "a.go"},
	}, []*graph.Edge{
		{From: "a.go::Foo", To: "b.go::Bar", Kind: graph.EdgeCalls, FilePath: "a.go"},
		{From: "a.go::Foo", To: "unresolved::Baz", Kind: graph.EdgeCalls, FilePath: "a.go"},
	})

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true}, zap.NewNop())
	require.NoError(t, err)

	assert.Equal(t, 1, w.countResolvedFileEdges(g.GetFileNodes("a.go")),
		"only the resolved out-edge counts; the unresolved stub does not")
}

// TestGuardResolvedEdgeRegression covers the F4.1 shape-degradation guard: a
// modify that kept its symbols but lost more than half its resolved edges
// enqueues a forced scoped re-resolve and bumps the regression counter, while
// below-floor / symbol-removal / modest-drop cases stay quiet.
func TestGuardResolvedEdgeRegression(t *testing.T) {
	idx, _, _ := newToggleIndexer(t)
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)

	reresolved := make(chan map[string]struct{}, 4)
	w.reconcileMu.Lock()
	w.reresolveFn = func(files map[string]struct{}) { reresolved <- files }
	w.reconcileMu.Unlock()

	// Resolved edges more than halved, symbols intact, above the floor: fires.
	before := ResolutionRegressions()
	w.guardResolvedEdgeRegression("a.go", 5, 5, 10, 2, 0, 0)
	select {
	case files := <-reresolved:
		_, ok := files["a.go"]
		assert.True(t, ok, "the degraded file must be enqueued for a forced re-resolve")
	case <-time.After(2 * time.Second):
		t.Fatal("a resolved-edge collapse must enqueue a forced re-resolve")
	}
	assert.Equal(t, before+1, ResolutionRegressions(), "the regression counter must increment")

	// None of the negatives may fire.
	base := ResolutionRegressions()
	w.guardResolvedEdgeRegression("below-floor.go", 5, 5, 3, 0, 0, 0) // resolvedBefore < floor
	w.guardResolvedEdgeRegression("symbol-removed.go", 5, 3, 10, 2, 0, 0)
	w.guardResolvedEdgeRegression("modest-drop.go", 5, 5, 10, 6, 0, 0) // dropped <= 50%
	select {
	case f := <-reresolved:
		t.Fatalf("a non-regression must not enqueue a re-resolve, got %v", f)
	case <-time.After(150 * time.Millisecond):
	}
	assert.Equal(t, base, ResolutionRegressions(), "non-regressions must not bump the counter")
}
