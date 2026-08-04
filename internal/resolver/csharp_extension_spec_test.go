package resolver

import (
	"strings"
	"sync"
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

// TestCrossRepo_RefusedCSharpMemberCallStaysUnresolved: the main
// resolver leaves a C# member call unresolved as a VERDICT (a
// constraint the receiver fails, a using another scope owns). The
// cross-repo pass's name-only fallback must not overturn it — it
// carries no C# evidence and would resurrect exactly the misbinds the
// member gates refuse, with no confidence and no origin.
func TestCrossRepo_RefusedCSharpMemberCallStaysUnresolved(t *testing.T) {
	cases := map[string]map[string]string{
		"constraint": {
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
		},
		"sibling-using": {
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
		},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			g := buildCSharpResolverGraph(t, files)
			New(g).ResolveAll()
			require.True(t, graph.IsUnresolvedTarget(fooCallTarget(g, "Caller.cs::Runner.Run")),
				"precondition: the main resolver refuses this bind")

			NewCrossRepo(g).ResolveAll()
			target := fooCallTarget(g, "Caller.cs::Runner.Run")
			assert.True(t, graph.IsUnresolvedTarget(target),
				"the cross-repo name-only fallback must not overturn the member gates, got %q", target)
		})
	}
}

// TestResolveCSharpExtension_ConstructorCallerScopeWalk: a call inside a
// CONSTRUCTOR body walks the same namespace levels as one inside a method —
// the enclosing namespace's own extension is considered before an outer
// using-imported one. Pins the caller-kind gap: ctor nodes must carry the
// scope stamp, or the walk would mistake them for global-namespace code.
func TestResolveCSharpExtension_ConstructorCallerScopeWalk(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"W.cs": `namespace W {
    public class Widget {}
}`,
		"BExt.cs": `using W;
namespace B {
    public static class BE { public static int Foo(this Widget w) { return 2; } }
}`,
		"Caller.cs": `using W;
using B;
namespace A {
    public static class AE { public static int Foo(this Widget w) { return 1; } }
    public class Runner {
        public Runner() {
            Widget w = new Widget();
            w.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.<init>")
	require.NotEmpty(t, target)
	assert.True(t, strings.Contains(target, "AE.Foo"),
		"namespace A's own extension is level 1 of the ctor's scope walk, got %q", target)
}

// TestResolveCSharpExtension_NullableAnnotatedReceiverBinds: a `Crate?`
// receiver is identity-convertible to `Crate` — the nullable annotation
// on a reference type is not a shape contradiction.
func TestResolveCSharpExtension_NullableAnnotatedReceiverBinds(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `namespace App {
    public class Crate {}
    public static class E { public static int Foo(this Crate c) { return 1; } }
}`,
		"Caller.cs": `namespace App {
    public class Runner {
        public void Run() {
            Crate? c = null;
            c.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, strings.Contains(target, "E.Foo"),
		"Crate? is identity-convertible to Crate — nullable annotation must not refuse the bind, got %q", target)
}

// TestResolveCSharpExtension_NullableValueReceiverStaysRefused: `int?` is
// Nullable<int>, a genuinely different type from `int` — the value-type
// nullable suffix keeps its veto.
func TestResolveCSharpExtension_NullableValueReceiverStaysRefused(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `namespace App {
    public static class E { public static int Foo(this int n) { return 1; } }
}`,
		"Caller.cs": `namespace App {
    public class Runner {
        public void Run() {
            int? n = 3;
            n.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"int? is Nullable<int>, not int — the extension stays inapplicable, got %q", target)
}

// TestResolveCSharpExtension_VarShapeFromOutermostCreation: a `var`
// initializer's receiver shape comes from the OUTERMOST object creation —
// a nested `new` inside a collection initializer must not override it.
func TestResolveCSharpExtension_VarShapeFromOutermostCreation(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `using System.Collections.Generic;
namespace App {
    public static class E { public static int Foo(this List<List<string>> xs) { return 1; } }
}`,
		"Caller.cs": `using System.Collections.Generic;
namespace App {
    public class Runner {
        public void Run() {
            var xs = new List<List<string>> { new List<string>() };
            xs.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, strings.Contains(target, "E.Foo"),
		"the var's shape is the outer List<List<string>>, not the nested initializer's, got %q", target)
}

// TestCrossRepo_RestubbedCSharpMemberCallStaysUnresolved: eviction
// restubs a refused member call to a BARE name (`unresolved::Foo`), which
// routes through the function-call fallback instead of the member one —
// the member_call evidence in edge Meta still marks it as a member-gate
// verdict, and the name-only tier must decline it too.
func TestCrossRepo_RestubbedCSharpMemberCallStaysUnresolved(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/Caller.cs::Runner.Run", Kind: graph.KindMethod, Name: "Run",
		FilePath: "repoA/Caller.cs", Language: "csharp", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoA/Ext.cs::E.Foo", Kind: graph.KindMethod, Name: "Foo",
		FilePath: "repoA/Ext.cs", Language: "csharp", RepoPrefix: "repoA"})
	edge := &graph.Edge{From: "repoA/Caller.cs::Runner.Run", To: "unresolved::Foo",
		Kind: graph.EdgeCalls, FilePath: "repoA/Caller.cs", Line: 5,
		Meta: map[string]any{"member_call": true}}
	g.AddEdge(edge)

	NewCrossRepo(g).ResolveAll()
	assert.Equal(t, "unresolved::Foo", edge.To,
		"a restubbed member call keeps the member-gate verdict — bare-name fallback must decline")
}

// TestCrossRepo_RazorCallerKeepsNameFallback: only csharp-language callers
// run the main resolver's member gates; a razor caller never gets a
// verdict there, so blocking its name-only fallback would just lose the
// edge. The gate is keyed to the exact language, not the family.
// (Cross-language razor→csharp binds are already refused by the
// scopedCandidates language filter — the fallback at stake is
// razor→razor, e.g. a symbol from another @code block.)
func TestCrossRepo_RazorCallerKeepsNameFallback(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/Page.razor::Page.OnInit", Kind: graph.KindMethod, Name: "OnInit",
		FilePath: "repoA/Page.razor", Language: "razor", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoA/Shared.razor::Shared.Foo", Kind: graph.KindMethod, Name: "Foo",
		FilePath: "repoA/Shared.razor", Language: "razor", RepoPrefix: "repoA"})
	edge := &graph.Edge{From: "repoA/Page.razor::Page.OnInit", To: "unresolved::*.Foo",
		Kind: graph.EdgeCalls, FilePath: "repoA/Page.razor", Line: 8}
	g.AddEdge(edge)

	NewCrossRepo(g).ResolveAll()
	assert.Equal(t, "repoA/Shared.razor::Shared.Foo", edge.To,
		"razor callers have no member-gate verdict — the name fallback stays available")
}

// TestCrossRepo_MissingCSharpCallerNodeFailsClosed: when the caller node
// is gone (the eviction window — exactly when resurrecting a refused
// bind matters most), the .cs file path is still proof enough that the
// member gates own this call.
func TestCrossRepo_MissingCSharpCallerNodeFailsClosed(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/Ext.cs::E.Foo", Kind: graph.KindMethod, Name: "Foo",
		FilePath: "repoA/Ext.cs", Language: "csharp", RepoPrefix: "repoA"})
	edge := &graph.Edge{From: "repoA/Caller.cs::Runner.Run", To: "unresolved::*.Foo",
		Kind: graph.EdgeCalls, FilePath: "repoA/Caller.cs", Line: 5}
	g.AddEdge(edge)

	NewCrossRepo(g).ResolveAll()
	assert.Equal(t, "unresolved::*.Foo", edge.To,
		"no caller node + .cs path: the gate fails closed, not open")
}

// TestCSharpMemberNamesOf_ParallelWorkers: the member-claims memo is
// reached from resolveEdge's worker pool — its reads and writes must
// share csharpNSMu like every sibling memo. Meaningful under -race.
func TestCSharpMemberNamesOf_ParallelWorkers(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Lib.cs": `namespace App {
    public class Base { public void Foo() {} }
    public class Derived : Base { public void Bar() {} }
}`,
	})
	r := New(g)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				r.csharpMemberNamesOf("Lib.cs::Base")
				r.csharpMemberNamesOf("Lib.cs::Derived")
			}
		}()
	}
	wg.Wait()
}

// TestResolveCSharpExtension_SiblingNamespaceUsingNotVisible: a using
// declared inside namespace A is scoped to A — a sibling namespace B in the
// same file must not see it.
func TestResolveCSharpExtension_SiblingNamespaceUsingNotVisible(t *testing.T) {
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
