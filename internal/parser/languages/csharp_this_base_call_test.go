package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// this./base.-qualified invocations must emit member-call edges. The grammar
// keeps `this` and `base` as anonymous tokens, so a bare `(_)` receiver
// capture never matches them — both calls used to vanish from the graph.
func TestCSharpExtractor_ThisBaseQualifiedCalls(t *testing.T) {
	src := []byte(`namespace App {
    public class Tower {
        public int Chime() { return 1; }
    }
    public class Belfry : Tower {
        public new int Chime() { return 2; }
        public int RingSelf() { return this.Chime(); }
        public int RingBase() { return base.Chime(); }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Belfry.cs", src)
	require.NoError(t, err)

	var selfCall, baseCall *graph.Edge
	for _, ed := range result.Edges {
		if ed.Kind != graph.EdgeCalls {
			continue
		}
		switch ed.From {
		case "Belfry.cs::Belfry.RingSelf":
			selfCall = ed
		case "Belfry.cs::Belfry.RingBase":
			baseCall = ed
		}
	}
	require.NotNil(t, selfCall, "this.Chime() must emit a call edge")
	require.NotNil(t, selfCall.Meta)
	assert.Equal(t, true, selfCall.Meta["member_call"])
	assert.Equal(t, "Belfry", selfCall.Meta["receiver_type"],
		"this-receiver is the enclosing type")

	require.NotNil(t, baseCall, "base.Chime() must emit a call edge")
	require.NotNil(t, baseCall.Meta)
	assert.Equal(t, true, baseCall.Meta["member_call"])
	assert.Equal(t, "Tower", baseCall.Meta["receiver_type"],
		"base-receiver is the declared base class")
}
