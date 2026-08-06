package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Calls into a record's members must bind the record's member, not a
// same-named member of an unrelated class. Before record bodies were
// extracted there was no Medal.Foo node at all, so the fallback
// confidently picked the decoy — a wrong answer, not a missing one.
func TestResolveCSharp_RecordMemberCalls(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Lib.cs": `namespace App {
    public class StencilDecoy { public int Foo(int id) { return 0; } }
    public record Medal(int Id) {
        public int Foo(int id) { return Id + id; }
    }
    public class MedalCase {
        public int Show(int id) {
            Medal medal = new Medal(id);
            return medal.Foo(id);
        }
    }
}`,
	})
	New(g).ResolveAll()

	call := fooCallEdge(g, "Lib.cs::MedalCase.Show")
	require.NotNil(t, call, "medal.Foo() must produce a call edge")
	assert.True(t, strings.Contains(call.To, "Medal.Foo"),
		"record-typed receiver binds the record's member, got %q", call.To)
	assert.False(t, strings.Contains(call.To, "StencilDecoy"),
		"decoy must not win, got %q", call.To)
	assert.GreaterOrEqual(t, call.Confidence, 0.9,
		"declared receiver type must bind confidently, not by fallback luck")
}
