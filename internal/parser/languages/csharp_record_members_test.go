package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Record bodies must extract like class bodies: methods, properties,
// fields, and ctors declared inside a record (or record struct) get
// member nodes, and calls out of record methods emit edges. Previously
// every member emitter's owner walk recognised class/struct/interface
// only, so a record's declaration_list members vanished entirely — no
// nodes, no call edges, and calls INTO record members mis-bound to
// same-named members of unrelated classes.
func TestCSharpExtractor_RecordMembers(t *testing.T) {
	src := []byte(`namespace App {
    public class Engraver {
        public string Etch(string text) { return text; }
    }
    public record Medal(int Id, string Motto) {
        private int _polish;
        public Medal(int id) : this(id, "none") { _polish = 1; }
        public string Inscribe(Engraver engraver) { return engraver.Etch(Motto); }
        public int Rank { get { return Id * 10; } }
    }
    public readonly record struct RibbonClip(int Width) {
        public int Span() { return Width * 2; }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Medal.cs", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	method := byID["Medal.cs::Medal.Inscribe"]
	require.NotNil(t, method, "record method must emit a node")
	assert.Equal(t, graph.KindMethod, method.Kind)
	require.NotNil(t, method.Meta)
	assert.Equal(t, "Medal", method.Meta["receiver"])

	assert.NotNil(t, byID["Medal.cs::Medal.Rank"], "record property must emit a node")
	assert.NotNil(t, byID["Medal.cs::Medal._polish"], "record field must emit a node")
	assert.NotNil(t, byID["Medal.cs::Medal.<init>"], "record ctor must emit a node")
	assert.NotNil(t, byID["Medal.cs::RibbonClip.Span"], "record STRUCT method must emit a node")

	var etchCall *graph.Edge
	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeCalls && ed.From == "Medal.cs::Medal.Inscribe" && strings.Contains(ed.To, "Etch") {
			etchCall = ed
		}
	}
	require.NotNil(t, etchCall, "call out of a record method must emit an edge")
	require.NotNil(t, etchCall.Meta)
	assert.Equal(t, true, etchCall.Meta["member_call"])
}
