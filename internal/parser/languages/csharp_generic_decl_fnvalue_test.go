package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Issue #534, second symptom: the reporter saw an edge into the extension
// set anchored at an overload's DECLARATION line rather than any call
// site.
//
// Same root cause as the dropped call edges — an identifier followed by
// `<`. The function-as-value capture treats a following `(` as proof that
// an identifier is a callee rather than a value use, and relies on that
// to skip a declaration's own name. A generic declaration
// (`AddFooDecorator<TInterface, TImpl>(`) puts `<` there instead, so the
// method's own name was captured as a function reference and landed a
// placeholder edge stamped at the declaration line.
//
// The `(` heuristic cannot be widened to `<` — in JavaScript and
// TypeScript `f < g` is a comparison — so the fix reads the tree: an
// identifier that IS a callable declaration's name field is a
// declaration, whatever byte follows it.
func TestCSharpExtractor_GenericDeclNameIsNotFnValue(t *testing.T) {
	src := []byte(`using System;

namespace Lib.Extensions
{
    public interface IServiceCollection { }
    public class DecoratorOptions { }

    public static class ServiceCollectionExtensions
    {
        public static IServiceCollection AddFooDecorator<TInterface, TImpl>(
            this IServiceCollection services)
            where TInterface : class
            where TImpl : class, TInterface
        {
            return services;
        }

        public static IServiceCollection AddFooDecorator<TInterface, TImpl>(
            this IServiceCollection services,
            Action<DecoratorOptions> configure)
            where TInterface : class
            where TImpl : class, TInterface
        {
            return services;
        }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Ext.cs", src)
	require.NoError(t, err)

	// Declaration lines, 1-based: overload A at 10, overload B at 18.
	declLines := map[int]bool{10: true, 18: true}
	for _, ed := range result.Edges {
		if ed.Kind != graph.EdgeReferences {
			continue
		}
		assert.False(t, declLines[ed.Line],
			"a generic method's own declaration is not a reference to it: %s -> %s at line %d",
			ed.From, ed.To, ed.Line)
	}

	// The non-generic control has always been excluded by the `(` rule;
	// assert the generic spelling now matches it exactly.
	fnValue := 0
	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeReferences && ed.Meta != nil && ed.Meta["via"] != nil {
			fnValue++
		}
	}
	assert.Zero(t, fnValue,
		"no function-as-value candidate exists in a file that only declares methods")
}
