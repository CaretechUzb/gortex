package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// csharpCallEdgeFrom returns the single call edge leaving callerID.
func csharpCallEdgeFrom(t *testing.T, res *parser.ExtractionResult, callerID string) *graph.Edge {
	t.Helper()
	var found *graph.Edge
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls && e.From == callerID {
			require.Nil(t, found, "expected a single call edge from %s", callerID)
			found = e
		}
	}
	require.NotNil(t, found, "no call edge from %s", callerID)
	return found
}

// TestCSharpExtractor_UserTypeLocalScopedToMethod: a user-typed local in one
// method must not stamp receiver_type onto a same-named local's call in a
// sibling method — the local type environment is method-scoped, exactly like
// the builtin stamp. Here method B's `s` is a string, so its call carries the
// builtin stamp and no receiver_type at all.
func TestCSharpExtractor_UserTypeLocalScopedToMethod(t *testing.T) {
	src := []byte(`namespace App {
    public class Runner {
        public void A() { Widget s = new Widget(); }
        public void B() { string s = "x"; s.Foo(); }
    }
}
`)
	res, err := NewCSharpExtractor().Extract("Runner.cs", src)
	require.NoError(t, err)

	e := csharpCallEdgeFrom(t, res, "Runner.cs::Runner.B")
	assert.Nil(t, e.Meta["receiver_type"],
		"method A's Widget must not bleed into method B's receiver")
	assert.Equal(t, "string", e.Meta["receiver_builtin"],
		"B's own string local supplies the builtin stamp")
}

// TestCSharpExtractor_UserTypeLocalStampsOwnMethod: the method-scoping must
// not lose the normal case — a user-typed local still stamps receiver_type
// on calls within its own method.
func TestCSharpExtractor_UserTypeLocalStampsOwnMethod(t *testing.T) {
	src := []byte(`namespace App {
    public class Runner {
        public void A() { Widget w = new Widget(); w.Foo(); }
    }
}
`)
	res, err := NewCSharpExtractor().Extract("Runner.cs", src)
	require.NoError(t, err)

	e := csharpCallEdgeFrom(t, res, "Runner.cs::Runner.A")
	assert.Equal(t, "Widget", e.Meta["receiver_type"])
}

// TestCSharpExtractor_MemberCallMarker: member-call edges carry a
// member_call stamp — eviction restubs them to a bare unresolved name, and
// without the marker the resolver cannot tell a former `x.Foo()` from a
// genuine bare `Foo()` when deciding whether the extension rule owns the
// rebind. Bare calls stay unstamped.
func TestCSharpExtractor_MemberCallMarker(t *testing.T) {
	src := []byte(`namespace App {
    public class Runner {
        public void A() { Bus.Publish(); }
        public void B() { Helper(); }
    }
}
`)
	res, err := NewCSharpExtractor().Extract("Runner.cs", src)
	require.NoError(t, err)

	member := csharpCallEdgeFrom(t, res, "Runner.cs::Runner.A")
	assert.Equal(t, true, member.Meta["member_call"], "member syntax must be marked")

	bare := csharpCallEdgeFrom(t, res, "Runner.cs::Runner.B")
	if bare.Meta != nil {
		assert.Nil(t, bare.Meta["member_call"], "a bare call is not a member call")
	}
}

// TestCSharpBuiltinTypeNameSuffixOrder: csharpBuiltinTypeName reads nullable
// and array suffixes in any order, agreeing with the resolver's suffix trim
// on spellings like `int?[]`.
func TestCSharpBuiltinTypeNameSuffixOrder(t *testing.T) {
	assert.Equal(t, "int", csharpBuiltinTypeName("int?[]"))
	assert.Equal(t, "int", csharpBuiltinTypeName("int[]?"))
	assert.Equal(t, "string", csharpBuiltinTypeName("string[][]"))
	assert.Equal(t, "", csharpBuiltinTypeName("Widget?[]"))
}
