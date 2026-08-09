package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// this?./base?.-qualified invocations must emit member-call edges like
// their unconditional twins — the conditional-access pattern's `(_)`
// condition capture has the same anonymous-token blindness the plain
// member-access pattern had.
func TestCSharpExtractor_ConditionalThisBaseCalls(t *testing.T) {
	src := []byte(`namespace App {
    public class Tower {
        public int Chime() { return 1; }
    }
    public class Belfry : Tower {
        public new int Chime() { return 2; }
        public int RingSelf() { return this?.Chime() ?? 0; }
        public int RingBase() { return base?.Chime() ?? 0; }
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
	require.NotNil(t, selfCall, "this?.Chime() must emit a call edge")
	require.NotNil(t, selfCall.Meta)
	assert.Equal(t, true, selfCall.Meta["member_call"])
	assert.Equal(t, "Belfry", selfCall.Meta["receiver_type"],
		"this-receiver is the enclosing type")

	require.NotNil(t, baseCall, "base?.Chime() must emit a call edge")
	require.NotNil(t, baseCall.Meta)
	assert.Equal(t, true, baseCall.Meta["member_call"])
	assert.Equal(t, "Tower", baseCall.Meta["receiver_type"],
		"base-receiver is the declared base class")
}
