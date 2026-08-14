package store_sqlite_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestAllEdgesLight_SkipsMetaKeepsEndpoints proves the live light-edge scan
// returns the same endpoints/kind/line as GetOutEdges while leaving Meta nil —
// it must never pay the per-edge meta JSON decode.
func TestAllEdgesLight_SkipsMetaKeepsEndpoints(t *testing.T) {
	s := openTestStore(t)

	want := &graph.Edge{
		From:     "pkg/x.go::Caller",
		To:       "pkg/x.go::Callee",
		Kind:     graph.EdgeCalls,
		FilePath: "pkg/x.go",
		Line:     42,
		Meta:     map[string]any{"call_line": 42, "callee_target": "unresolved::Callee"},
	}
	s.AddEdge(want)

	full := s.GetOutEdges("pkg/x.go::Caller")
	require.Len(t, full, 1)
	require.NotNil(t, full[0].Meta, "GetOutEdges must decode Meta")
	assert.Equal(t, "unresolved::Callee", full[0].Meta["callee_target"])

	light := s.AllEdgesLight()
	require.Len(t, light, 1)
	assert.Equal(t, full[0].From, light[0].From)
	assert.Equal(t, full[0].To, light[0].To)
	assert.Equal(t, full[0].Kind, light[0].Kind)
	assert.Equal(t, full[0].Line, light[0].Line)
	assert.Equal(t, full[0].FilePath, light[0].FilePath)
	assert.Nil(t, light[0].Meta, "light fetch must not decode the meta blob")

}
