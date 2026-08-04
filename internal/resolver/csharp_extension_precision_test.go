package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestResolveCSharpExtension_EnclosingShadowsBareThisParam: a bare this-param
// name resolves against the extension's own enclosing namespaces before its
// imports — `Foo(this Inner)` declared in namespace Vendor means Vendor.Inner
// even when the file also has `using Data;`. A Data.Inner receiver must not
// bind it just because Data is visible from the extension's file.
func TestResolveCSharpExtension_EnclosingShadowsBareThisParam(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Data/Inner.cs": `namespace Data {
    public class Inner {}
}`,
		"Vendor/Types.cs": `namespace Vendor {
    public class Inner {}
}`,
		"Vendor/Ext.cs": `using Data;
namespace Vendor {
    public static class VE { public static int Foo(this Inner x) { return 1; } }
}`,
		"Caller.cs": `using Vendor;
namespace App {
    public class Runner {
        public void Run() {
            Data.Inner x = new Data.Inner();
            x.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"Vendor's own Inner shadows the imported Data.Inner — the bind is a contradiction, got %q", target)
}

// TestResolveCSharpExtension_ReachWalkSeedsOnlyVisibleReceiver: the
// inheritance walk must start from the type the receiver can actually
// denote at the call site — not every same-name type in the repo. An
// unrelated Vendor.Thing : IBox must not donate its hierarchy to a
// caller whose Thing is Data.Thing (no interfaces).
func TestResolveCSharpExtension_ReachWalkSeedsOnlyVisibleReceiver(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Data/Types.cs": `namespace Data {
    public class Thing {}
}`,
		"Vendor/Types.cs": `namespace Vendor {
    public interface IBox {}
    public class Thing : IBox {}
}`,
		"Ext.cs": `namespace App {
    public static class E { public static int Foo(this IBox b) { return 1; } }
}`,
		"Caller.cs": `using Data;
namespace App {
    public class Runner {
        public void Run() {
            Thing t = new Thing();
            t.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"Data.Thing implements nothing — Vendor.Thing's hierarchy must not make IBox reachable, got %q", target)
}

// TestResolveCSharpExtension_ReachWalkAnchorsReachedType: the reached side of
// the walk is subject to the same shadowing rule as the direct match — a
// candidate's bare `this IBox` declared in namespace App means App.IBox, so
// reaching the unrelated Vendor.IBox through the receiver's hierarchy is not
// a match.
func TestResolveCSharpExtension_ReachWalkAnchorsReachedType(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Vendor/Types.cs": `namespace Vendor {
    public interface IBox {}
    public class Crate : IBox {}
}`,
		"App/Ext.cs": `namespace App {
    public interface IBox {}
    public static class E { public static int Foo(this IBox b) { return 1; } }
}`,
		"Caller.cs": `using App;
namespace Vendor {
    public class Runner {
        public void Run() {
            Crate c = new Crate();
            c.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"Crate reaches Vendor.IBox, but the candidate's this-param means App.IBox — no match, got %q", target)
}

// TestResolveCSharpExtension_WaiverSkipsInRepoThisParam: an unresolved edge in
// the receiver's hierarchy waives the veto only for candidates the unknown
// external part could satisfy. An external base can never derive from an
// in-repo type, so `Foo(this Widget)` with Widget in-repo stays provably
// unrelated to `Crate : VendorBase(external)`.
func TestResolveCSharpExtension_WaiverSkipsInRepoThisParam(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Types.cs": `namespace App {
    public class Crate : VendorBase {}
    public class Widget {}
}`,
		"Ext.cs": `namespace App {
    public static class E { public static int Foo(this Widget w) { return 1; } }
}`,
		"Caller.cs": `namespace App {
    public class Runner {
        public void Run() {
            Crate c = new Crate();
            c.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"Widget is in-repo — VendorBase cannot reach it, the waiver must not bind, got %q", target)
}

// TestResolveCSharpExtension_WaiverSkipsBuiltinThisParam: sealed builtins can
// never appear in any hierarchy, resolved or not — `Foo(this string)` stays
// vetoed even when the receiver's own hierarchy is incomplete.
func TestResolveCSharpExtension_WaiverSkipsBuiltinThisParam(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Types.cs": `namespace App {
    public class Crate : VendorBase {}
}`,
		"Ext.cs": `namespace App {
    public static class E { public static int Foo(this string s) { return 1; } }
}`,
		"Caller.cs": `namespace App {
    public class Runner {
        public void Run() {
            Crate c = new Crate();
            c.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"no type derives from string — the waiver must not bind a builtin this-param, got %q", target)
}

// TestResolveCSharpExtension_WaiverJoinsUniversalPool: with an incomplete
// hierarchy, a candidate the external base could satisfy (absent this-param)
// competes with the generic catch-all instead of being shadowed by it — two
// eligible candidates the visibility cannot split refuse rather than handing
// the call to the catch-all.
func TestResolveCSharpExtension_WaiverJoinsUniversalPool(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Types.cs": `namespace App {
    public class Crate : VendorBase {}
}`,
		"Ext.cs": `namespace App {
    public static class E {
        public static int Foo<T>(this T any) { return 1; }
        public static int Foo(this IShip s) { return 2; }
    }
}`,
		"Caller.cs": `namespace App {
    public class Runner {
        public void Run() {
            Crate c = new Crate();
            c.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"the external base may satisfy IShip — the catch-all must not win the tie unexamined, got %q", target)
}

// TestResolveCSharpExtension_TypedBindRequiresVisibility: a typed match whose
// declaring namespace the call site can neither enclose nor import is
// uncallable C# — the binder must refuse upfront instead of binding and
// letting the cross-package guard revert it every pass (bind/revert churn,
// and the revert used to leak the confidence label).
func TestResolveCSharpExtension_TypedBindRequiresVisibility(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Lib/Ext.cs": `namespace Lib.Stuff {
    public class Widget {}
    public static class E { public static int Foo(this Widget w) { return 1; } }
}`,
		"Caller.cs": `namespace App {
    public class Runner {
        public void Run() {
            Lib.Stuff.Widget w = new Lib.Stuff.Widget();
            w.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	for _, e := range g.GetOutEdges("Caller.cs::Runner.Run") {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		require.True(t, graph.IsUnresolvedTarget(e.To),
			"Lib.Stuff is visible from nowhere in Caller.cs — refuse, got %q", e.To)
		assert.Nil(t, e.Meta["guard_reverted"],
			"the binder must refuse upfront, not bind-then-revert")
		assert.Empty(t, e.ConfidenceLabel,
			"an unresolved edge must not keep a bind's confidence label")
	}
}

// TestResolveCSharpExtension_RestubbedTypedCallRefusesContradiction: a
// restubbed member call (bare unresolved name, receiver evidence intact in
// Meta) must route through the type-directed rule — the repo-wide locality
// fallback re-pinning the extension would bypass the eligibility veto.
func TestResolveCSharpExtension_RestubbedTypedCallRefusesContradiction(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Types.cs": `namespace App {
    public class Gadget {}
    public class Widget {}
}`,
		"Ext.cs": `namespace App {
    public static class E { public static int Foo(this Widget w) { return 1; } }
}`,
		"Caller.cs": `namespace App {
    public class Runner {
        public void Run() {}
    }
}`,
	})
	// The post-restub shape: member syntax lost, receiver evidence kept.
	g.AddBatch(nil, []*graph.Edge{{
		From: "Caller.cs::Runner.Run", To: "unresolved::Foo",
		Kind: graph.EdgeCalls, FilePath: "Caller.cs", Line: 4,
		Meta: map[string]any{"receiver_type": "Gadget", "member_call": true},
	}})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"a Gadget receiver contradicts Foo(this Widget) — the bare-name fallback must not re-pin it, got %q", target)
}

// TestResolveCSharpExtension_RestubbedMemberCallRebindsByVisibility: the
// evidence-less flagship shape (property receiver) restubs to a bare name
// with only the member_call marker — it must rebind through visibility
// narrowing to the caller's own module copy, not to whichever same-name
// extension the repo-wide fallback happens to reach first.
func TestResolveCSharpExtension_RestubbedMemberCallRebindsByVisibility(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"ModA/Ext.cs": `namespace ModA {
    public static class CE { public static int Foo(this IBox b) { return 1; } }
}`,
		"ModB/Ext.cs": `namespace ModB {
    public static class CE { public static int Foo(this IBox b) { return 2; } }
}`,
		"ModB/Caller.cs": `namespace ModB {
    public class Runner {
        public void Run() {}
    }
}`,
	})
	g.AddBatch(nil, []*graph.Edge{{
		From: "ModB/Caller.cs::Runner.Run", To: "unresolved::Foo",
		Kind: graph.EdgeCalls, FilePath: "ModB/Caller.cs", Line: 4,
		Meta: map[string]any{"member_call": true},
	}})
	New(g).ResolveAll()

	target := fooCallTarget(g, "ModB/Caller.cs::Runner.Run")
	require.Equal(t, "ModB/Ext.cs::CE.Foo", target,
		"the caller's enclosing namespace narrows to its own module's copy")
	for _, e := range g.GetOutEdges("ModB/Caller.cs::Runner.Run") {
		if e.Kind == graph.EdgeCalls && e.To == target {
			assert.Equal(t, "extension_method", e.Meta["resolution"],
				"the rebind must come from the extension rule, not a locality guess")
		}
	}
}

// TestCSharpShouldDemote_StaleExtensionTagDoesNotExempt: the receiver gate's
// extension exemption belongs to the target, not the tag — an edge whose
// stale `resolution: extension_method` survived a restub while the edge
// rebound to a plain method must still be demotable.
func TestCSharpShouldDemote_StaleExtensionTagDoesNotExempt(t *testing.T) {
	caller := &graph.Node{ID: "Caller.cs::Runner.Run", Name: "Run", Kind: graph.KindMethod, Language: "csharp"}
	target := &graph.Node{
		ID: "Other.cs::Api.Foo", Name: "Foo", Kind: graph.KindMethod, Language: "csharp",
		Meta: map[string]any{"receiver": "Api"},
	}
	nodes := map[string]*graph.Node{caller.ID: caller, target.ID: target}
	e := &graph.Edge{
		From: caller.ID, To: target.ID, Kind: graph.EdgeCalls,
		Origin: graph.OriginTextMatched,
		Meta:   map[string]any{"receiver_type": "Gadget", "resolution": "extension_method"},
	}
	nameToTypeIDs := map[string][]string{"Gadget": {"Types.cs::Gadget"}, "Api": {"Other.cs::Api"}}

	assert.True(t, csharpShouldDemote(nodes, e, nameToTypeIDs, map[string][]string{}, map[string]bool{}),
		"a non-extension target with a mismatched receiver must demote regardless of a stale tag")
}

// TestCSharpVisibilityCachesClearTogether: the per-file namespace sets bake
// the global-usings propagation in, so the generation-refresh clear must
// drop both — a rebuilt global index behind stale per-file sets serves the
// old generation for the rest of the page.
func TestCSharpVisibilityCachesClearTogether(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Caller.cs": `using Data;
namespace App {
    public class Runner { public void Run() {} }
}`,
	})
	r := New(g)
	r.csharpFileNamespaceSet("Caller.cs")
	require.NotEmpty(t, r.csharpNSByFile, "namespace set should be memoized")

	r.clearCSharpVisibilityCaches()
	assert.Empty(t, r.csharpNSByFile,
		"the generation-refresh clear must drop the per-file sets with the global index")
}

// TestCSharpTypeSuffixTrimFixpoint: suffix trimming strips nullable and
// array markers in any order — `int?[]` and `int[]?` both canonicalise to
// `int`, matching csharpBuiltinTypeName's read of the same spellings.
func TestCSharpTypeSuffixTrimFixpoint(t *testing.T) {
	assert.Equal(t, "int", csharpTypeSuffixTrim("int?[]"))
	assert.Equal(t, "int", csharpTypeSuffixTrim("int[]?"))
	assert.Equal(t, "int", csharpTypeSuffixTrim("int[][]"))
	assert.Equal(t, "Widget", csharpTypeSuffixTrim("Widget[]?"))
	assert.Equal(t, "List", csharpTypeSuffixTrim("List<int>[]"))
}
