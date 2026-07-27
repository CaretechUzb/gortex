package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// Regression: `recv?.M(x)` parses as an invocation_expression whose
// `function` field is a conditional_access_expression, not a
// member_access_expression. While the query alternation covered only the
// latter, every null-conditional call site was dropped silently, so a
// method reached only through `?.` — the dominant idiom for optional
// module/service dispatch — looked callerless and read as dead code.
func TestCSharpExtractor_NullConditionalInvocationCallEdge(t *testing.T) {
	src := []byte(`public class Alpha {
    public void Target(int x) { }
    public void Plain() {
        var a = new Alpha();
        a.Target(1);
    }
    public void NullConditional() {
        var a = new Alpha();
        a?.Target(2);
    }
    public void CastNullConditional(object o) {
        (o as Alpha)?.Target(3);
    }
}`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Alpha.cs", src)
	require.NoError(t, err)

	callers := map[string]bool{}
	for _, ed := range result.Edges {
		if ed.Kind != graph.EdgeCalls {
			continue
		}
		if strings.HasSuffix(ed.To, ".Target") {
			callers[ed.From] = true
		}
	}

	assert.True(t, callers["Alpha.cs::Alpha.Plain"],
		"plain member call must mint a calls edge (control)")
	assert.True(t, callers["Alpha.cs::Alpha.NullConditional"],
		"`a?.Target(2)` must mint a calls edge")
	assert.True(t, callers["Alpha.cs::Alpha.CastNullConditional"],
		"`(o as Alpha)?.Target(3)` must mint a calls edge")
}
