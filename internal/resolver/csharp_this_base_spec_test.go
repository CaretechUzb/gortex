package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// this./base.-qualified calls name their receiver explicitly: `this.Foo()`
// resolves against the enclosing type (a `new` member hides the base one),
// `base.Foo()` against the declared base class — no inference involved.
func TestResolveCSharp_ThisBaseQualifiedCalls(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Lib.cs": `namespace App {
    public class Tower { public int Foo() { return 1; } }
    public class Belfry : Tower {
        public new int Foo() { return 2; }
        public int RingSelf() { return this.Foo(); }
        public int RingBase() { return base.Foo(); }
    }
}`,
	})
	New(g).ResolveAll()

	selfTarget := fooCallTarget(g, "Lib.cs::Belfry.RingSelf")
	require.NotEmpty(t, selfTarget, "this.Foo() must produce a call edge")
	assert.True(t, strings.Contains(selfTarget, "Belfry.Foo"),
		"this.Foo() binds the hiding member, got %q", selfTarget)

	baseTarget := fooCallTarget(g, "Lib.cs::Belfry.RingBase")
	require.NotEmpty(t, baseTarget, "base.Foo() must produce a call edge")
	assert.True(t, strings.Contains(baseTarget, "Tower.Foo"),
		"base.Foo() binds the base member, got %q", baseTarget)
}
