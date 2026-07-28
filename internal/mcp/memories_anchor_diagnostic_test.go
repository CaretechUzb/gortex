package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/persistence"
)

// Surfacing matches the caller's symbol ids against the ids stored on each
// memory by string equality. Anything that changes symbol-id SHAPE — a repo
// prefix appearing or disappearing on node ids, a file move, a mass rename —
// severs every stored anchor at once and returns Total: 0, which is
// indistinguishable from "nothing relevant to this task".
//
// These tests pin the diagnostic that tells the two apart, because without
// it a whole workspace's memories can stop surfacing with no error anywhere.

func TestSurfaceAnchorDiagnostic_IDConventionMismatch(t *testing.T) {
	mm := newMemoryManager("", "")

	// Memories anchored under the OLD (unprefixed) id convention.
	_, err := mm.Save(persistence.MemoryEntry{
		Body:      "Bar must hold the lock before mutating cache",
		SymbolIDs: []string{"internal/foo.go::Bar"},
	})
	require.NoError(t, err)
	_, err = mm.Save(persistence.MemoryEntry{
		Body:      "Baz is not safe to call during warmup",
		SymbolIDs: []string{"internal/foo.go::Baz"},
	})
	require.NoError(t, err)

	// The graph now only knows the NEW (prefixed) ids, so every stored
	// anchor dangles.
	live := map[string]*graph.Node{
		"gortex/internal/foo.go::Bar": {ID: "gortex/internal/foo.go::Bar", FilePath: "gortex/internal/foo.go"},
	}
	resolve := func(id string) *graph.Node { return live[id] }

	res := mm.Surface(SurfaceOptions{SymbolIDs: []string{"gortex/internal/foo.go::Bar"}}, resolve)

	require.Equal(t, 0, res.Total, "the anchor should not match under the old convention")
	assert.Equal(t, 2, res.StaleAnchors, "both stored anchors dangle")
	assert.NotEmpty(t, res.StaleAnchorSamples, "the diagnostic must name offenders, not just count them")
	assert.Contains(t, res.AnchorDiagnostic, "different symbol-id convention",
		"a majority-dangling store must be reported as a convention mismatch, not ordinary drift")
}

func TestSurfaceAnchorDiagnostic_OrdinaryDriftIsNotAlarming(t *testing.T) {
	mm := newMemoryManager("", "")

	// Three live anchors, one deleted symbol: ordinary drift.
	for _, sym := range []string{"r/a.go::A", "r/b.go::B", "r/c.go::C", "r/gone.go::Gone"} {
		_, err := mm.Save(persistence.MemoryEntry{Body: "note for " + sym, SymbolIDs: []string{sym}})
		require.NoError(t, err)
	}
	live := map[string]*graph.Node{
		"r/a.go::A": {ID: "r/a.go::A", FilePath: "r/a.go"},
		"r/b.go::B": {ID: "r/b.go::B", FilePath: "r/b.go"},
		"r/c.go::C": {ID: "r/c.go::C", FilePath: "r/c.go"},
	}
	resolve := func(id string) *graph.Node { return live[id] }

	// Anchor on a symbol no memory references, so the result is empty for
	// a legitimate reason.
	res := mm.Surface(SurfaceOptions{SymbolIDs: []string{"r/unrelated.go::Z"}}, resolve)

	require.Equal(t, 0, res.Total)
	assert.Equal(t, 1, res.StaleAnchors)
	assert.NotContains(t, res.AnchorDiagnostic, "different symbol-id convention",
		"one dangling anchor out of four is drift, not a convention mismatch")
}

func TestSurfaceAnchorDiagnostic_QuietWhenMemoriesMatch(t *testing.T) {
	mm := newMemoryManager("", "")
	_, err := mm.Save(persistence.MemoryEntry{
		Body:      "Bar must hold the lock",
		SymbolIDs: []string{"gortex/internal/foo.go::Bar"},
	})
	require.NoError(t, err)

	resolve := func(id string) *graph.Node {
		return &graph.Node{ID: id, FilePath: "gortex/internal/foo.go"}
	}
	res := mm.Surface(SurfaceOptions{SymbolIDs: []string{"gortex/internal/foo.go::Bar"}}, resolve)

	require.Equal(t, 1, res.Total)
	assert.Empty(t, res.AnchorDiagnostic, "the diagnostic must stay off the happy path")
	assert.Zero(t, res.StaleAnchors)
}
