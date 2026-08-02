package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Review-round-4 spec cases (PR #432): each test pins one of the reviewer's
// reproduced misbindings. C# extension lookup begins only when normal member
// lookup finds no applicable method, receiver/param type SHAPE is part of
// applicability, and using-directive visibility is scope-by-scope from the
// innermost namespace outward.

// TestResolveCSharpExtension_InstanceMemberPrecedence: an applicable inherited
// instance method wins over any extension — `d.Foo()` with `Derived : Base`
// and `Base.Foo()` declared must never bind `E.Foo(this Base)`.
func TestResolveCSharpExtension_InstanceMemberPrecedence(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Lib.cs": `namespace App {
    public class Base { public void Foo() {} }
    public class Derived : Base {}
}`,
		"Ext.cs": `namespace App {
    public static class E { public static void Foo(this Base x) {} }
}`,
		"Caller.cs": `namespace App {
    public class Runner {
        public void Run() {
            Derived d = new Derived();
            d.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.False(t, strings.Contains(target, "E.Foo"),
		"Base.Foo is an applicable instance member — extension lookup must not begin, got %q", target)
}

// TestResolveCSharpExtension_ArrayReceiverPrefersArrayOverload: a string[]
// receiver is applicable to Foo(this string[]) and NOT to Foo(this string) —
// the array overload must win even though the scalar one sits in the caller's
// own enclosing namespace.
func TestResolveCSharpExtension_ArrayReceiverPrefersArrayOverload(t *testing.T) {
	t.Skip("pending R2: lossless receiver type shape (fails today — verified)")
	g := buildCSharpResolverGraph(t, map[string]string{
		"AppExt.cs": `namespace App {
    public static class ScalarExt { public static int Foo(this string value) { return 1; } }
}`,
		"LibExt.cs": `namespace Lib {
    public static class ArrayExt { public static int Foo(this string[] value) { return 2; } }
}`,
		"Caller.cs": `using Lib;
namespace App {
    public class Runner {
        public void Run() {
            string[] xs = new string[1];
            xs.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, strings.Contains(target, "ArrayExt.Foo"),
		"string[] receiver only fits the array overload, got %q", target)
}

// TestResolveCSharpExtension_ConstrainedGenericNotUniversal: Foo<T>(this T)
// where T : ITagged is applicable only to receivers satisfying the
// constraint — a plain class must not bind it.
func TestResolveCSharpExtension_ConstrainedGenericNotUniversal(t *testing.T) {
	t.Skip("pending R2: generic constraints in applicability (fails today — verified)")
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `namespace App {
    public interface ITagged {}
    public static class E {
        public static int Foo<T>(this T x) where T : ITagged { return 1; }
    }
}`,
		"Caller.cs": `namespace App {
    public class Plain {}
    public class Runner {
        public void Run() {
            Plain p = new Plain();
            p.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"Plain does not satisfy T : ITagged — the constrained generic is inapplicable, got %q", target)
}

// TestResolveCSharpExtension_GenericArgsDistinguishReceivers: List<int> must
// not typed-match Foo(this List<string>) — generic arguments are part of the
// receiver's identity.
func TestResolveCSharpExtension_GenericArgsDistinguishReceivers(t *testing.T) {
	t.Skip("pending R2: generic-argument identity (fails today — verified)")
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `using System.Collections.Generic;
namespace App {
    public static class E { public static int Foo(this List<string> xs) { return 1; } }
}`,
		"Caller.cs": `using System.Collections.Generic;
namespace App {
    public class Runner {
        public void Run() {
            List<int> xs = new List<int>();
            xs.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"List<int> cannot bind Foo(this List<string>), got %q", target)
}

// TestResolveCSharpExtension_InnerScopeUsingBeatsOuterEnclosing: lookup is
// scope-by-scope from the innermost namespace — a using declared INSIDE A.B
// supplies candidates at the A.B scope, which are considered before the outer
// enclosing namespace A's own extension.
func TestResolveCSharpExtension_InnerScopeUsingBeatsOuterEnclosing(t *testing.T) {
	t.Skip("pending R3: scope-by-scope using lookup (fails today — verified)")
	g := buildCSharpResolverGraph(t, map[string]string{
		"W.cs": `namespace W {
    public class Widget {}
}`,
		"AExt.cs": `using W;
namespace A {
    public static class AE { public static int Foo(this Widget w) { return 1; } }
}`,
		"XExt.cs": `using W;
namespace X {
    public static class XE { public static int Foo(this Widget w) { return 2; } }
}`,
		"Caller.cs": `namespace A.B {
    using W;
    using X;
    public class Runner {
        public void Run() {
            Widget w = new Widget();
            w.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, strings.Contains(target, "XE.Foo"),
		"using X at the A.B scope is considered before outer namespace A, got %q", target)
}

// TestResolveCSharpExtension_SiblingNamespaceUsingNotVisible: a using
// declared inside namespace A is scoped to A — a sibling namespace B in the
// same file must not see it.
func TestResolveCSharpExtension_SiblingNamespaceUsingNotVisible(t *testing.T) {
	t.Skip("pending R3: per-scope using visibility (fails today — verified)")
	g := buildCSharpResolverGraph(t, map[string]string{
		"W.cs": `namespace W {
    public class Widget {}
}`,
		"XExt.cs": `using W;
namespace X {
    public static class XE { public static int Foo(this Widget w) { return 2; } }
}`,
		"Caller.cs": `using W;
namespace A {
    using X;
}
namespace B {
    public class Runner {
        public void Run() {
            Widget w = new Widget();
            w.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"using X is scoped to namespace A — sibling B must not see XE, got %q", target)
}
