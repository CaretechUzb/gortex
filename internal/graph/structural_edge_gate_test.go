package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Structural kinds can never target a parameter or local node — the class a
// mis-mapped interface object once wrote 130k of. The backstop must drop the
// shape at every write funnel while passing every legitimate edge untouched.
func TestStructuralEdgeTargetInvalid(t *testing.T) {
	assert.True(t, StructuralEdgeTargetInvalid(EdgeImplements, "a/b.go::F#param:ctx"))
	assert.True(t, StructuralEdgeTargetInvalid(EdgeExtends, "a/b.go::F#local:x@+3"))
	assert.True(t, StructuralEdgeTargetInvalid(EdgeOverrides, "a/b.go::F#param:v"))
	assert.True(t, StructuralEdgeTargetInvalid(EdgeMemberOf, "a/b.go::F#local:acc@+9"))
	assert.True(t, StructuralEdgeTargetInvalid(EdgeInstantiates, "a/b.go::F#param:cfg"))

	assert.False(t, StructuralEdgeTargetInvalid(EdgeImplements, "a/b.go::Iface"))
	assert.False(t, StructuralEdgeTargetInvalid(EdgeCalls, "a/b.go::F#param:ctx"),
		"non-structural kinds may reference params (dataflow, reads)")
	assert.False(t, StructuralEdgeTargetInvalid(EdgeReferences, "a/b.go::F#local:x@+1"))
}

func TestFilterStructuralEdgeViolationsCopiesOnlyOnViolation(t *testing.T) {
	clean := []*Edge{
		{From: "a::T", To: "a::I", Kind: EdgeImplements},
		{From: "a::f", To: "a::g#param:x", Kind: EdgeCalls},
	}
	kept, dropped := FilterStructuralEdgeViolations(clean)
	assert.Zero(t, dropped)
	assert.Equal(t, &clean[0], &kept[0], "clean path must return the input slice, not a copy")

	mixed := []*Edge{
		{From: "a::T", To: "a::I", Kind: EdgeImplements},
		{From: "a::T2", To: "a::g#param:ctx", Kind: EdgeImplements},
		{From: "a::T3", To: "a::I", Kind: EdgeExtends},
	}
	kept, dropped = FilterStructuralEdgeViolations(mixed)
	require.Len(t, dropped, 1)
	assert.Same(t, mixed[1], dropped[0])
	require.Len(t, kept, 2)
	assert.Equal(t, "a::I", kept[0].To)
	assert.Equal(t, "a::I", kept[1].To)
}

// Both in-memory entry points must refuse the shape end-to-end.
func TestGraphWriteFunnelsDropStructuralViolations(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "a::T", Kind: KindType})
	g.AddNode(&Node{ID: "a::g#param:ctx", Kind: KindParam})

	g.AddEdge(&Edge{From: "a::T", To: "a::g#param:ctx", Kind: EdgeImplements})
	assert.Empty(t, g.GetOutEdges("a::T"), "AddEdge must drop a structural violation")

	g.AddBatch(nil, []*Edge{
		{From: "a::T", To: "a::g#param:ctx", Kind: EdgeMemberOf},
		{From: "a::T", To: "a::g#param:ctx", Kind: EdgeCalls},
	})
	out := g.GetOutEdges("a::T")
	require.Len(t, out, 1, "AddBatch must drop the structural violation and keep the call")
	assert.Equal(t, EdgeCalls, out[0].Kind)
}
