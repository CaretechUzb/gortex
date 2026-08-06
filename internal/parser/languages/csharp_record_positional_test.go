package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Positional record parameters synthesize public properties — the
// defining feature of records and their most-used members. They have no
// declaration node for the member walk, so the extractor fabricates the
// property nodes from the record's parameter list.
func TestCSharpExtractor_RecordPositionalProperties(t *testing.T) {
	src := []byte(`namespace App {
    public record Medal(int Id, string Motto) {
        public int Rank { get { return Id * 10; } }
    }
    public readonly record struct RibbonClip(int Width);
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Medal.cs", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	id := byID["Medal.cs::Medal.Id"]
	require.NotNil(t, id, "positional parameter Id must emit a property node")
	assert.Equal(t, graph.KindField, id.Kind)
	require.NotNil(t, id.Meta)
	assert.Equal(t, "Medal", id.Meta["receiver"])
	assert.Equal(t, "property", id.Meta["kind"])
	assert.Equal(t, "int", id.Meta["field_type"],
		"the declared parameter type rides the node")
	assert.Equal(t, VisibilityPublic, id.Meta["visibility"],
		"synthesized positional properties are public")

	require.NotNil(t, byID["Medal.cs::Medal.Motto"], "second positional emits too")
	assert.NotNil(t, byID["Medal.cs::Medal.Rank"], "explicit body member still emits")

	clip := byID["Medal.cs::RibbonClip.Width"]
	require.NotNil(t, clip, "record STRUCT positional emits a property node")
	assert.Equal(t, "string", byID["Medal.cs::Medal.Motto"].Meta["field_type"])

	var memberOf bool
	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeMemberOf && ed.From == "Medal.cs::Medal.Id" && ed.To == "Medal.cs::Medal" {
			memberOf = true
		}
	}
	assert.True(t, memberOf, "positional property carries a member_of edge to its record")
}

// An explicit redeclaration of a positional property (legal C#: the
// explicit member replaces the synthesized one) must not produce a
// duplicate node.
func TestCSharpExtractor_RecordPositionalRedeclaration(t *testing.T) {
	src := []byte(`namespace App {
    public record Badge(int Code) {
        public int Code { get; init; }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Badge.cs", src)
	require.NoError(t, err)

	count := 0
	for _, n := range result.Nodes {
		if n.ID == "Badge.cs::Badge.Code" {
			count++
		}
	}
	assert.Equal(t, 1, count, "one node for Code, not two")
}
