package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// fooCallTarget returns the To-end of the single Foo call edge leaving fromID.
func fooCallTarget(g graph.Store, fromID string) string {
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		if graph.IsUnresolvedTarget(e.To) {
			if graph.UnresolvedName(e.To) == "*.Foo" || graph.UnresolvedName(e.To) == "Foo" {
				return e.To
			}
			continue
		}
		if n := g.GetNode(e.To); n != nil && n.Name == "Foo" {
			return e.To
		}
	}
	return ""
}

// TestResolveCSharpExtension_UniqueBinds: a call on a literal receiver with no
// type evidence binds to the sole extension method of that name.
func TestResolveCSharpExtension_UniqueBinds(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `namespace App {
    public static class StringExt {
        public static int Foo(this string s) { return 1; }
    }
}`,
		"Caller.cs": `namespace App {
    public class Runner {
        public void Run() {
            "a".Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.Equal(t, "Ext.cs::StringExt.Foo", target)
	e := g.GetNode(target)
	require.NotNil(t, e)
}

// TestResolveCSharpExtension_TypedDisambiguates: with a known receiver type, the
// extension whose this_param_type matches wins over a same-named extension on
// another type.
func TestResolveCSharpExtension_TypedDisambiguates(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Exts.cs": `namespace App {
    public static class E {
        public static int Foo(this Widget w) { return 1; }
        public static int Foo(this Gadget x) { return 2; }
    }
    public class Widget {}
    public class Gadget {}
}`,
		"Caller.cs": `namespace App {
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
	require.False(t, graph.IsUnresolvedTarget(target), "typed receiver should resolve")
	n := g.GetNode(target)
	require.NotNil(t, n)
	assert.Equal(t, "Widget", n.Meta["this_param_type"], "should bind the Widget extension")
}

// TestResolveCSharpExtension_AmbiguousStaysUnresolved: two same-named extensions
// on different types + a receiver with no type evidence must not be guessed —
// neither the extension rule nor the locality fallback may pick one.
func TestResolveCSharpExtension_AmbiguousStaysUnresolved(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Exts.cs": `namespace App {
    public static class E1 { public static int Foo(this string s) { return 1; } }
    public static class E2 { public static int Foo(this int n) { return 2; } }
}`,
		"Caller.cs": `namespace App {
    public class Runner {
        public void Run(Thing t) {
            t.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"ambiguous extension call must stay unresolved, got %q", target)
}

// TestResolveCSharpExtension_InstanceWinsUntypedReceiver: with an untyped
// receiver (a field, no receiver_type), the sole extension must NOT preempt a
// same-named instance method — the locality fallback binds the instance method.
func TestResolveCSharpExtension_InstanceWinsUntypedReceiver(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Types.cs": `namespace App {
    public class Repo { public int Count() { return 0; } }
    public static class StringExt { public static int Count(this string s) { return s.Length; } }
}`,
		"Consumer.cs": `namespace App {
    public class Consumer {
        private Repo _repo = new Repo();
        public void Run() {
            _repo.Count();
        }
    }
}`,
	})
	New(g).ResolveAll()

	var target string
	for _, e := range g.GetOutEdges("Consumer.cs::Consumer.Run") {
		if e.Kind == graph.EdgeCalls && !graph.IsUnresolvedTarget(e.To) {
			if n := g.GetNode(e.To); n != nil && n.Name == "Count" {
				target = e.To
			}
		}
	}
	assert.Equal(t, "Types.cs::Repo.Count", target,
		"an untyped receiver must not let the extension preempt the instance method")
}

// TestIsCSharpExtension_LanguageGated: the extension guard is C#-only, so a
// Scala extension-method node (which also stamps Meta[extension]) is never
// treated as a C# extension — keeping Scala's locality resolution unchanged.
func TestIsCSharpExtension_LanguageGated(t *testing.T) {
	cs := &graph.Node{Kind: graph.KindMethod, Language: "csharp", Meta: map[string]any{"extension": true}}
	scala := &graph.Node{Kind: graph.KindMethod, Language: "scala", Meta: map[string]any{"extension": true}}
	assert.True(t, isCSharpExtension(cs))
	assert.False(t, isCSharpExtension(scala),
		"a Scala extension node must not be treated as a C# extension")
}

// TestResolveCSharpExtension_EnclosingNamespaceNarrows: two same-named,
// same-receiver-type extensions in different module namespaces; the caller's
// own enclosing namespace makes exactly one visible, so an untyped receiver
// (a property — no receiver_type evidence) still binds to its module's copy.
// This is the DI-configuration shape: every module defines its own
// `ContainerExtensions.Validate(this IContainer)` and calls it from Startup.
func TestResolveCSharpExtension_EnclosingNamespaceNarrows(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"ModA/Ext.cs": `namespace ModA {
    public static class ContainerExtensions {
        public static int Foo(this IBox b) { return 1; }
    }
}`,
		"ModB/Ext.cs": `namespace ModB {
    public static class ContainerExtensions {
        public static int Foo(this IBox b) { return 2; }
    }
}`,
		"ModB/Startup.cs": `namespace ModB {
    public class Startup {
        public IBox Container { get; set; }
        public void Configure() {
            Container.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "ModB/Startup.cs::Startup.Configure")
	require.Equal(t, "ModB/Ext.cs::ContainerExtensions.Foo", target,
		"caller in ModB must bind ModB's extension, not stay unresolved or cross modules")
}

// TestResolveCSharpExtension_UsingDirectiveBreaksTypedTie: a typed receiver
// matches two same-named extensions on the same receiver type; the caller
// file's using directive makes only one namespace visible, which breaks the
// tie the typed pass alone refuses to guess on.
func TestResolveCSharpExtension_UsingDirectiveBreaksTypedTie(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"LibA/Ext.cs": `namespace LibA {
    public static class EA {
        public static int Foo(this Widget w) { return 1; }
    }
}`,
		"LibB/Ext.cs": `namespace LibB {
    public static class EB {
        public static int Foo(this Widget w) { return 2; }
    }
}`,
		"Widget.cs": `namespace App {
    public class Widget {}
}`,
		"Caller.cs": `using LibA;
namespace App {
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
	require.Equal(t, "LibA/Ext.cs::EA.Foo", target,
		"the using-visible extension must win the typed tie")
	for _, e := range g.GetOutEdges("Caller.cs::Runner.Run") {
		if e.Kind == graph.EdgeCalls && e.To == target {
			assert.InDelta(t, 0.9, e.Confidence, 0.001, "typed+visible bind keeps the typed tier")
		}
	}
}

// TestResolveCSharpExtension_BothVisibleStaysUnresolved: visibility narrowing
// must narrow, not guess — when the caller imports both candidate namespaces,
// an untyped receiver still refuses to pick.
func TestResolveCSharpExtension_BothVisibleStaysUnresolved(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"LibA/Ext.cs": `namespace LibA {
    public static class EA { public static int Foo(this string s) { return 1; } }
}`,
		"LibB/Ext.cs": `namespace LibB {
    public static class EB { public static int Foo(this int n) { return 2; } }
}`,
		"Caller.cs": `using LibA;
using LibB;
namespace App {
    public class Runner {
        public void Run(Thing t) {
            t.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"two visible candidates must stay unresolved, got %q", target)
}

// TestResolveCSharpExtension_NothingVisibleFallsBackToUnique: narrowing is
// narrowing-only — when no candidate namespace is visible from the caller
// (stale or partial using data), the pre-existing unique-name bind survives.
func TestResolveCSharpExtension_NothingVisibleFallsBackToUnique(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Lib/Ext.cs": `namespace LibX {
    public static class E { public static int Foo(this string s) { return 1; } }
}`,
		"Caller.cs": `namespace App {
    public class Runner {
        public void Run() {
            "a".Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.Equal(t, "Lib/Ext.cs::E.Foo", target,
		"a repo-unique extension must still bind when nothing is visible")
}

// TestResolveCSharpExtension_TypedContradictionRefuses: receiver-type
// evidence that matches no candidate's this_param_type must refuse —
// visibility is not applicability. The classic shape is a BCL instance
// call (`xs.Add(1)`) whose real target isn't in the graph: a visible
// same-name extension on an unrelated type must not swallow it.
func TestResolveCSharpExtension_TypedContradictionRefuses(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"LibA/Ext.cs": `namespace LibA {
    public static class EA { public static int Foo(this string s) { return 1; } }
}`,
		"LibB/Ext.cs": `namespace LibB {
    public static class EB { public static int Foo(this Gadget x) { return 2; } }
}`,
		"Widget.cs": `namespace App {
    public class Widget {}
}`,
		"Caller.cs": `using LibA;
namespace App {
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
		"a typed receiver contradicting every candidate must stay unresolved, got %q", target)
}

// TestResolveCSharpExtension_GlobalUsingPropagates: a `global using` declared
// in a sibling file (the Usings.cs convention) applies to every file under the
// declaring file's directory — the caller file itself has no usings at all.
func TestResolveCSharpExtension_GlobalUsingPropagates(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Proj/Usings.cs": `global using LibA;
`,
		"Proj/Caller.cs": `namespace App {
    public class Runner {
        public void Run(Thing t) {
            t.Foo();
        }
    }
}`,
		"LibA/Ext.cs": `namespace LibA {
    public static class EA { public static int Foo(this string s) { return 1; } }
}`,
		"LibB/Ext.cs": `namespace LibB {
    public static class EB { public static int Foo(this int n) { return 2; } }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Proj/Caller.cs::Runner.Run")
	require.Equal(t, "LibA/Ext.cs::EA.Foo", target,
		"the sibling file's global using must make LibA visible to the caller")
}

// TestResolveCSharpExtension_GlobalUsingDoesNotLeakAcrossProjects: a global
// using is scoped to its compilation — a sibling project's Usings.cs must not
// grant visibility, or every module's globals would cross-contaminate the
// per-module disambiguation.
func TestResolveCSharpExtension_GlobalUsingDoesNotLeakAcrossProjects(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"ProjA/Usings.cs": `global using LibA;
`,
		"ProjB/Caller.cs": `namespace App {
    public class Runner {
        public void Run(Thing t) {
            t.Foo();
        }
    }
}`,
		"LibA/Ext.cs": `namespace LibA {
    public static class EA { public static int Foo(this string s) { return 1; } }
}`,
		"LibB/Ext.cs": `namespace LibB {
    public static class EB { public static int Foo(this int n) { return 2; } }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "ProjB/Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"ProjA's global using must not leak into ProjB, got %q", target)
}

// TestResolveCSharpExtension_UsingStaticMakesExtensionVisible: `using static
// LibA.EA;` brings EA's extension methods into scope even though namespace
// LibA itself is never imported.
func TestResolveCSharpExtension_UsingStaticMakesExtensionVisible(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"LibA/Ext.cs": `namespace LibA {
    public static class EA { public static int Foo(this string s) { return 1; } }
}`,
		"LibB/Ext.cs": `namespace LibB {
    public static class EB { public static int Foo(this int n) { return 2; } }
}`,
		"Caller.cs": `using static LibA.EA;
namespace App {
    public class Runner {
        public void Run(Thing t) {
            t.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.Equal(t, "LibA/Ext.cs::EA.Foo", target,
		"using static of the declaring class must make its extensions visible")
}

// TestResolveCSharpExtension_InstanceWins: an instance method beats an extension
// of the same name (C# member-lookup precedence).
func TestResolveCSharpExtension_InstanceWins(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Types.cs": `namespace App {
    public class Widget {
        public int Foo() { return 0; }
    }
    public static class E {
        public static int Foo(this Widget w) { return 1; }
    }
}`,
		"Caller.cs": `namespace App {
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
	require.Equal(t, "Types.cs::Widget.Foo", target, "instance method should win over the extension")
}

// TestResolveCSharpExtension_BuiltinReceiverTypeDirected: a primitive
// receiver (`int n; n.Foo()`) must type-direct the bind — the int
// extension wins even when a same-name string extension sits in the
// caller's own (enclosing-visible) namespace. Real C# checks
// eligibility before scope preference.
func TestResolveCSharpExtension_BuiltinReceiverTypeDirected(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"ModA/Ext.cs": `namespace ModA {
    public static class EA { public static int Foo(this int n) { return 1; } }
}`,
		"ModB/Ext.cs": `namespace ModB {
    public static class EB { public static int Foo(this string s) { return 2; } }
}`,
		"ModB/Caller.cs": `using ModA;
namespace ModB {
    public class Runner {
        public void Run() {
            int n = 1;
            n.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "ModB/Caller.cs::Runner.Run")
	require.Equal(t, "ModA/Ext.cs::EA.Foo", target,
		"the int extension must win on receiver eligibility, not the enclosing string extension")
}

// TestResolveCSharpExtension_QualifiedReceiverTypeMatches: a receiver
// declared with a namespace-qualified type (`Models.Widget w`) must
// still match `this Widget` — the two sides arrive differently
// normalised and compare on the shared canonical key.
func TestResolveCSharpExtension_QualifiedReceiverTypeMatches(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Models/Widget.cs": `namespace Models {
    public class Widget {}
}`,
		"LibA/Ext.cs": `namespace LibA {
    public static class EA { public static int Foo(this Widget w) { return 1; } }
}`,
		"LibB/Ext.cs": `namespace LibB {
    public static class EB { public static int Foo(this Gadget x) { return 2; } }
}`,
		"Caller.cs": `using LibA;
namespace App {
    public class Runner {
        public void Run() {
            Models.Widget w = new Models.Widget();
            w.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.Equal(t, "LibA/Ext.cs::EA.Foo", target,
		"a qualified receiver type must match the namespace-stripped this_param_type")
}

// TestResolveCSharpExtension_SiblingNamespaceBlockDoesNotPoison: a file
// declaring two namespace blocks must attribute each call site to ITS
// block's namespace — the sibling block's (string-longer) namespace
// must not out-rank the call site's own in the enclosing tier.
func TestResolveCSharpExtension_SiblingNamespaceBlockDoesNotPoison(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Longer/Ext.cs": `namespace Elsewhere.Longer {
    public static class E1 { public static int Foo(this Widget w) { return 1; } }
}`,
		"AB/Ext.cs": `namespace A.B {
    public static class E2 { public static int Foo(this Widget w) { return 2; } }
}`,
		"Caller.cs": `namespace Elsewhere.Longer {
    public class Unrelated {}
}
namespace A.B {
    public class Runner {
        public void Run(Widget w) {
            w.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.Equal(t, "AB/Ext.cs::E2.Foo", target,
		"the call site's own namespace block must win, not the sibling block's")
}

// TestCSharpExtensionGuardKeep_Gates: each of the keep-rule's gates
// refuses on its own — the rule must never shield anything but a
// genuine, visible extension bind from the reachability revert.
func TestCSharpExtensionGuardKeep_Gates(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"LibA/Ext.cs": `namespace LibA {
    public static class EA { public static int Foo(this string s) { return 1; } }
}`,
		"Caller.cs": `using LibA;
namespace App {
    public class Runner {
        public void Run() {}
    }
}`,
	})
	r := New(g)
	target := g.GetNode("LibA/Ext.cs::EA.Foo")
	require.NotNil(t, target)
	edge := &graph.Edge{From: "Caller.cs::Runner.Run", FilePath: "Caller.cs",
		Meta: map[string]any{"resolution": "extension_method"}}

	assert.True(t, r.csharpExtensionGuardKeep(edge, "Caller.cs", target),
		"visible extension bind must be kept")
	assert.False(t, r.csharpExtensionGuardKeep(edge, "", target),
		"empty caller file must refuse")
	noMeta := &graph.Edge{From: edge.From, FilePath: edge.FilePath}
	assert.False(t, r.csharpExtensionGuardKeep(noMeta, "Caller.cs", target),
		"an edge without the extension_method resolution must refuse")
	instance := &graph.Node{ID: "zz::inst", Kind: graph.KindMethod, Language: "csharp",
		Meta: map[string]any{"scope_ns": "LibA"}}
	assert.False(t, r.csharpExtensionGuardKeep(edge, "Caller.cs", instance),
		"a non-extension target must refuse")
	invisible := &graph.Node{ID: "zz::inv", Kind: graph.KindMethod, Language: "csharp",
		Meta: map[string]any{"extension": true, "this_param_type": "string", "scope_ns": "LibZ"}}
	assert.False(t, r.csharpExtensionGuardKeep(edge, "Caller.cs", invisible),
		"an invisible namespace must refuse")
}

// TestResolveCSharpExtension_EnclosingKeepAcrossDirectories: an
// enclosing-visible extension in a DIFFERENT directory (namespace ModB
// spanning two dirs) — the guard's import-reachability closure has no
// same-dir shortcut and no import edge, so only the visibility
// keep-rule preserves the bind.
func TestResolveCSharpExtension_EnclosingKeepAcrossDirectories(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"ModAExt/Ext.cs": `namespace ModA {
    public static class ContainerExtensions { public static int Foo(this IBox b) { return 1; } }
}`,
		"ModBExt/Ext.cs": `namespace ModB {
    public static class ContainerExtensions { public static int Foo(this IBox b) { return 2; } }
}`,
		"ModB/Startup.cs": `namespace ModB {
    public class Startup {
        public IBox Container { get; set; }
        public void Configure() {
            Container.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "ModB/Startup.cs::Startup.Configure")
	require.Equal(t, "ModBExt/Ext.cs::ContainerExtensions.Foo", target,
		"the enclosing-visible extension must survive the guard across directories")
}

// TestResolveCSharpExtension_FileScopedNamespaces: the C# 10 file-scoped
// form (`namespace X;`) must ride the whole pipeline — extension stamp,
// caller scope, visibility narrowing.
func TestResolveCSharpExtension_FileScopedNamespaces(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"ModA/Ext.cs": `namespace ModA;
public static class ContainerExtensions { public static int Foo(this IBox b) { return 1; } }
`,
		"ModB/Ext.cs": `namespace ModB;
public static class ContainerExtensions { public static int Foo(this IBox b) { return 2; } }
`,
		"ModB/Startup.cs": `namespace ModB;
public class Startup {
    public IBox Container { get; set; }
    public void Configure() {
        Container.Foo();
    }
}
`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "ModB/Startup.cs::Startup.Configure")
	require.Equal(t, "ModB/Ext.cs::ContainerExtensions.Foo", target,
		"file-scoped namespaces must narrow identically to block form")
}
