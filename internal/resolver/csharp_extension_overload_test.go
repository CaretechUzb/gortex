package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// namedCallTarget returns the target of the single EdgeCalls out of
// fromID whose callee is named method — resolved node or unresolved stub.
func namedCallTarget(t *testing.T, g graph.Store, fromID, method string) string {
	t.Helper()
	var hits []string
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		if graph.IsUnresolvedTarget(e.To) {
			if n := graph.UnresolvedName(e.To); n == method || n == "*."+method {
				hits = append(hits, e.To)
			}
			continue
		}
		if n := g.GetNode(e.To); n != nil && n.Name == method {
			hits = append(hits, e.To)
		}
	}
	require.Len(t, hits, 1, "expected exactly one %s call out of %s", method, fromID)
	return hits[0]
}

// Issue #534. The reporter's fixture, verbatim in shape: one static class
// declares two `AddFooDecorator` overloads over `IServiceCollection`, and
// three call sites invoke the no-argument one — from a different project,
// from the same namespace in a different file, and from a different
// namespace in the same project.
//
// Two independent defects made every one of them vanish. The call-site
// query required an `identifier`, so `services.AddFooDecorator<I, T>()`
// (a `generic_name`) emitted no edge at all; and once it did, the two
// same-receiver overloads shared a namespace, so neither type evidence
// nor visibility could split them and the binder refused. `query callers`
// on the extension answered `total_edges: 0` with the `likely_unused`
// caveat — a resolution failure wearing dead code's clothes.
func TestResolveCSharpExtension_CrossFileGenericOverload(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Lib/Extensions/ServiceCollectionExtensions.cs": `using System;

namespace Lib.Extensions
{
    public interface IServiceCollection { }

    public class DecoratorOptions { public int RetryCount { get; set; } }

    public static class ServiceCollectionExtensions
    {
        public static IServiceCollection AddFooDecorator<TInterface, TImpl>(
            this IServiceCollection services)
            where TInterface : class
            where TImpl : class, TInterface
        {
            services.AddFooDecoratorInfrastructure();
            return services;
        }

        public static IServiceCollection AddFooDecorator<TInterface, TImpl>(
            this IServiceCollection services,
            Action<DecoratorOptions> configure)
            where TInterface : class
            where TImpl : class, TInterface
        {
            services.AddFooDecoratorInfrastructure();
            return services;
        }

        public static IServiceCollection AddFooDecoratorInfrastructure(
            this IServiceCollection services) => services;
    }
}`,
		// Case 1 — different project, different namespace, `using` import.
		"Consumer/Extensions/ConsumerRegistration.cs": `using Lib.Extensions;

namespace Consumer.Extensions
{
    public interface IAlpha { }
    public class Alpha : IAlpha { }

    public static class ConsumerRegistration
    {
        public static IServiceCollection AddConsumerServices(this IServiceCollection services)
        {
            services.AddFooDecorator<IAlpha, Alpha>();
            return services;
        }
    }
}`,
		// Case 2 — same project, SAME namespace, different file: no import
		// indirection at all, so nothing but the overload tie can explain
		// a miss.
		"Lib/Extensions/SameNamespaceCaller.cs": `namespace Lib.Extensions
{
    public interface IDelta { }
    public class Delta : IDelta { }

    public static class SameNamespaceCaller
    {
        public static IServiceCollection Register(this IServiceCollection services)
        {
            services.AddFooDecorator<IDelta, Delta>();
            return services;
        }
    }
}`,
		// Case 3 — same project, different namespace, `using` import.
		"Lib/Other/DifferentNamespaceCaller.cs": `using Lib.Extensions;

namespace Lib.Other
{
    public interface IEcho { }
    public class Echo : IEcho { }

    public static class DifferentNamespaceCaller
    {
        public static IServiceCollection Register(this IServiceCollection services)
        {
            services.AddFooDecorator<IEcho, Echo>();
            return services;
        }
    }
}`,
	})
	New(g).ResolveAll()

	const decl = "Lib/Extensions/ServiceCollectionExtensions.cs::ServiceCollectionExtensions.AddFooDecorator"
	for _, caller := range []string{
		"Consumer/Extensions/ConsumerRegistration.cs::ConsumerRegistration.AddConsumerServices",
		"Lib/Extensions/SameNamespaceCaller.cs::SameNamespaceCaller.Register",
		"Lib/Other/DifferentNamespaceCaller.cs::DifferentNamespaceCaller.Register",
	} {
		assert.Equal(t, decl, namedCallTarget(t, g, caller, "AddFooDecorator"),
			"%s passes no argument, so the one-parameter overload is the only applicable one", caller)
	}

	// The intra-file control the reporter used still binds, and the
	// extension is no longer reported as uncalled.
	callers := 0
	for _, e := range g.GetInEdges(decl) {
		if e.Kind == graph.EdgeCalls {
			callers++
		}
	}
	assert.Equal(t, 3, callers, "all three call sites are incoming edges on the declaration")
}

// Argument count is decisive both ways: the same call name with one
// argument must land on the two-parameter overload.
func TestResolveCSharpExtension_ArgCountPicksOverload(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `using System;
namespace Lib {
    public interface IBag { }
    public class Opts { }
    public static class BagExt {
        public static IBag Add(this IBag bag) { return bag; }
        public static IBag Add(this IBag bag, Action<Opts> configure) { return bag; }
    }
}`,
		"Caller.cs": `using Lib;
namespace App {
    public class Runner {
        public void Bare(IBag bag) { bag.Add(); }
        public void Configured(IBag bag) { bag.Add(x => { }); }
    }
}`,
	})
	New(g).ResolveAll()

	assert.Equal(t, "Ext.cs::BagExt.Add",
		namedCallTarget(t, g, "Caller.cs::Runner.Bare", "Add"),
		"no arguments fits only the one-parameter overload")
	assert.Equal(t, "Ext.cs::BagExt.Add_L7",
		namedCallTarget(t, g, "Caller.cs::Runner.Configured", "Add"),
		"one argument fits only the two-parameter overload")
}

// A defaulted parameter widens an overload's acceptable arity, so a
// zero-argument call fits BOTH members here. Narrowing must not
// manufacture a winner — the refusal is the correct answer, and a
// missing edge beats a wrong one.
func TestResolveCSharpExtension_OptionalParamKeepsAmbiguity(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `namespace Lib {
    public interface IBag { }
    public static class BagExt {
        public static IBag Tag(this IBag bag) { return bag; }
        public static IBag Tag(this IBag bag, int depth = 0) { return bag; }
    }
}`,
		"Caller.cs": `using Lib;
namespace App {
    public class Runner {
        public void Run(IBag bag) { bag.Tag(); }
    }
}`,
	})
	New(g).ResolveAll()

	target := namedCallTarget(t, g, "Caller.cs::Runner.Run", "Tag")
	assert.True(t, graph.IsUnresolvedTarget(target),
		"both overloads accept zero arguments — refusing beats guessing, got %q", target)
}

// A trailing `params` array lifts the upper bound: `Log(this IBag, params object[])`
// accepts any argument count, while the fixed sibling accepts exactly one.
func TestResolveCSharpExtension_VariadicWidensArity(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `namespace Lib {
    public interface IBag { }
    public static class BagExt {
        public static IBag Log(this IBag bag, string only) { return bag; }
        public static IBag Log(this IBag bag, params object[] rest) { return bag; }
    }
}`,
		"Caller.cs": `using Lib;
namespace App {
    public class Runner {
        public void Many(IBag bag) { bag.Log("a", "b", "c"); }
    }
}`,
	})
	New(g).ResolveAll()

	assert.Equal(t, "Ext.cs::BagExt.Log_L5",
		namedCallTarget(t, g, "Caller.cs::Runner.Many", "Log"),
		"three arguments overflow the fixed overload and only the params one accepts them")
}

// Explicit type arguments are their own applicability rule: a call
// spelling two type arguments cannot invoke a one-type-parameter
// overload, even when both accept the same argument count.
func TestResolveCSharpExtension_TypeArgCountPicksOverload(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `namespace Lib {
    public interface IBag { }
    public static class BagExt {
        public static IBag Map<TOut>(this IBag bag) { return bag; }
        public static IBag Map<TIn, TOut>(this IBag bag) { return bag; }
    }
}`,
		"Caller.cs": `using Lib;
namespace App {
    public class Runner {
        public void One(IBag bag) { bag.Map<string>(); }
        public void Two(IBag bag) { bag.Map<int, string>(); }
    }
}`,
	})
	New(g).ResolveAll()

	assert.Equal(t, "Ext.cs::BagExt.Map",
		namedCallTarget(t, g, "Caller.cs::Runner.One", "Map"),
		"one type argument fits only the one-type-parameter overload")
	assert.Equal(t, "Ext.cs::BagExt.Map_L5",
		namedCallTarget(t, g, "Caller.cs::Runner.Two", "Map"),
		"two type arguments fit only the two-type-parameter overload")
}

// Applicability narrows; it never overrides. Here arity is decisive and
// points at a namespace the call site can neither enclose nor import: the
// one-argument call fits only the overload in `Vendor.Internal`. Binding
// it would be exactly the misattribution the visibility veto exists to
// stop, so the narrowed-to-one set must still be refused. Applicability
// running FIRST must not turn the veto into a tiebreak that never runs.
func TestResolveCSharpExtension_ArityNeverBeatsVisibility(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Lib.cs": `namespace Lib {
    public interface IBag { }
}`,
		"Visible.cs": `using Lib;
namespace Lib.Ext {
    public static class VisibleExt {
        public static void Ping(this IBag bag) { }
    }
}`,
		"Hidden.cs": `using Lib;
namespace Vendor.Internal {
    public static class HiddenExt {
        public static void Ping(this IBag bag, int depth) { }
    }
}`,
		"Caller.cs": `using Lib;
using Lib.Ext;
namespace App {
    public class Runner {
        public void Run(IBag bag) { bag.Ping(7); }
    }
}`,
	})
	New(g).ResolveAll()

	target := namedCallTarget(t, g, "Caller.cs::Runner.Run", "Ping")
	assert.True(t, graph.IsUnresolvedTarget(target),
		"the only arity-applicable overload lives in an unimported namespace, got %q", target)
}
