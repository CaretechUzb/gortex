package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestCSharpExtractor_UsingsMeta: the file node carries its plain
// namespace usings as Meta["usings"] — resolution can rewrite the
// import edges, so the resolver's namespace narrowing needs a shape
// nothing mutates. Aliases and using-static grant no bare-name
// namespace visibility and are excluded.
func TestCSharpExtractor_UsingsMeta(t *testing.T) {
	src := []byte(`using App.Billing;
using App.Sales.Rules;
global using App.Platform;
using Alias = App.Legacy.OldRules;
using static System.Math;

namespace App.Web
{
    public class Startup {}
}
`)
	res, err := NewCSharpExtractor().Extract("Startup.cs", src)
	require.NoError(t, err)

	var file *graph.Node
	for _, n := range res.Nodes {
		if n.Kind == graph.KindFile {
			file = n
		}
	}
	require.NotNil(t, file)

	usings, _ := file.Meta["usings"].([]string)
	assert.ElementsMatch(t, []string{"App.Billing", "App.Sales.Rules", "App.Platform"}, usings,
		"plain and global usings only — no aliases, no using-static")
}

// TestCSharpExtractor_GlobalAndStaticUsingsMeta: global usings and
// using-static targets get their own meta keys — a global using is
// project-scoped (the resolver propagates it beyond the declaring file),
// and a using-static class's extension methods stay callable in extension
// form, so both need shapes the per-file "usings" stamp cannot carry.
// Aliases stay excluded from every key.
func TestCSharpExtractor_GlobalAndStaticUsingsMeta(t *testing.T) {
	src := []byte(`using App.Billing;
global using App.Platform;
using static System.Math;
global using static App.Shared.Helpers;
using Alias = App.Legacy.OldRules;

namespace App.Web
{
    public class Startup {}
}
`)
	res, err := NewCSharpExtractor().Extract("Startup.cs", src)
	require.NoError(t, err)

	var file *graph.Node
	for _, n := range res.Nodes {
		if n.Kind == graph.KindFile {
			file = n
		}
	}
	require.NotNil(t, file)

	globals, _ := file.Meta["global_usings"].([]string)
	assert.ElementsMatch(t, []string{"App.Platform"}, globals,
		"only global namespace usings — static and alias forms excluded")
	statics, _ := file.Meta["using_static"].([]string)
	assert.ElementsMatch(t, []string{"System.Math", "App.Shared.Helpers"}, statics,
		"using-static targets, plain and global forms alike")
	usings, _ := file.Meta["usings"].([]string)
	assert.ElementsMatch(t, []string{"App.Billing", "App.Platform"}, usings,
		"the plain stamp keeps its shape: namespace usings incl. global, no static/alias")
}

// TestCSharpExtractor_ScopedUsingsMeta: R3 (PR #432 review) — C# using
// visibility is scope-by-scope, so each plain namespace using is also
// stamped WITH the namespace scope it is declared in ("scope|name",
// empty scope = compilation unit). Additive: the flat legacy keys keep
// their exact shape for every existing consumer.
func TestCSharpExtractor_ScopedUsingsMeta(t *testing.T) {
	src := []byte(`using W;

namespace A
{
    using X;

    namespace B
    {
        using Y;

        public class Runner {}
    }
}
`)
	res, err := NewCSharpExtractor().Extract("Caller.cs", src)
	require.NoError(t, err)

	var file *graph.Node
	for _, n := range res.Nodes {
		if n.Kind == graph.KindFile {
			file = n
		}
	}
	require.NotNil(t, file)

	scoped, _ := file.Meta["scoped_usings"].([]string)
	assert.ElementsMatch(t, []string{"|W", "A|X", "A.B|Y"}, scoped,
		"each using rides with its declaring namespace scope")
	usings, _ := file.Meta["usings"].([]string)
	assert.ElementsMatch(t, []string{"W", "X", "Y"}, usings,
		"the flat legacy stamp keeps every using, scope-blind as before")
}

// TestCSharpExtractor_ScopedUsingsMeta_QualifiedAndFileScoped: a using
// inside `namespace A.B { }` belongs to the A.B scope; usings BEFORE a
// file-scoped `namespace N;` stay at the compilation-unit scope.
// Aliases and using-static stay out, matching the flat stamp.
func TestCSharpExtractor_ScopedUsingsMeta_QualifiedAndFileScoped(t *testing.T) {
	res, err := NewCSharpExtractor().Extract("Q.cs", []byte(`namespace A.B
{
    using X;
    using Alias = Legacy.Old;
    using static Helpers.Math;

    public class Runner {}
}
`))
	require.NoError(t, err)
	var file *graph.Node
	for _, n := range res.Nodes {
		if n.Kind == graph.KindFile {
			file = n
		}
	}
	require.NotNil(t, file)
	scoped, _ := file.Meta["scoped_usings"].([]string)
	assert.ElementsMatch(t, []string{"A.B|X"}, scoped,
		"qualified namespace blocks scope their usings under the full dotted name")

	res, err = NewCSharpExtractor().Extract("F.cs", []byte(`using W;
namespace N;

public class Runner {}
`))
	require.NoError(t, err)
	file = nil
	for _, n := range res.Nodes {
		if n.Kind == graph.KindFile {
			file = n
		}
	}
	require.NotNil(t, file)
	scoped, _ = file.Meta["scoped_usings"].([]string)
	assert.ElementsMatch(t, []string{"|W"}, scoped,
		"usings preceding a file-scoped namespace are compilation-unit scoped")
}

// TestCSharpExtractor_UsingsMeta_NoUsings: a file without using
// directives must not grow an empty Meta entry.
func TestCSharpExtractor_UsingsMeta_NoUsings(t *testing.T) {
	res, err := NewCSharpExtractor().Extract("Plain.cs", []byte("public class Plain {}\n"))
	require.NoError(t, err)
	for _, n := range res.Nodes {
		if n.Kind == graph.KindFile {
			assert.Nil(t, n.Meta["usings"])
		}
	}
}

// TestCSharpExtractor_BuiltinReceiverStamp: primitive receivers are
// deliberately absent from receiver_type (the receiver-gate passes key
// on user types), but extension eligibility still needs them — the
// member-call edge carries the builtin under its own key.
func TestCSharpExtractor_BuiltinReceiverStamp(t *testing.T) {
	src := []byte(`namespace App {
    public class C {
        public void M() {
            int n = 0;
            n.Foo();
        }
    }
}
`)
	res, err := NewCSharpExtractor().Extract("C.cs", src)
	require.NoError(t, err)
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls && e.To == "unresolved::*.Foo" {
			require.Nil(t, e.Meta["receiver_type"], "primitives must stay out of receiver_type")
			require.Equal(t, "int", e.Meta["receiver_builtin"], "builtin receiver must be stamped")
			return
		}
	}
	t.Fatal("no member-call edge to unresolved::*.Foo found")
}

// TestCSharpExtractor_BuiltinReceiverStampMethodScoped: locals are
// method-scoped — a same-named local of a different type in a SIBLING
// method must not bleed into this method's receiver stamp (the
// file-scoped last-declaration-wins shape mis-typed the receiver).
func TestCSharpExtractor_BuiltinReceiverStampMethodScoped(t *testing.T) {
	src := []byte(`namespace App {
    public class C {
        public void A() {
            int n = 0;
            n.Foo();
        }
        public void B() {
            string n = "x";
        }
    }
}
`)
	res, err := NewCSharpExtractor().Extract("C.cs", src)
	require.NoError(t, err)
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls && e.To == "unresolved::*.Foo" {
			require.Equal(t, "int", e.Meta["receiver_builtin"],
				"method A's int local must win, not method B's string")
			return
		}
	}
	t.Fatal("no member-call edge to unresolved::*.Foo found")
}

// TestCSharpExtractor_GenericThisParamStamp: `Foo<T>(this T v)` marks
// the this-param as a generic type parameter — the binder treats it as
// matching any receiver rather than a concrete type named "T".
func TestCSharpExtractor_GenericThisParamStamp(t *testing.T) {
	src := []byte(`namespace App {
    public static class E {
        public static T Any<T>(this T v) { return v; }
        public static int Conc(this string s) { return 1; }
    }
}
`)
	res, err := NewCSharpExtractor().Extract("E.cs", src)
	require.NoError(t, err)
	var anyM, concM *graph.Node
	for _, n := range res.Nodes {
		switch n.Name {
		case "Any":
			anyM = n
		case "Conc":
			concM = n
		}
	}
	require.NotNil(t, anyM)
	require.NotNil(t, concM)
	assert.Equal(t, true, anyM.Meta["this_param_generic"], "generic this-param must be stamped")
	assert.Nil(t, concM.Meta["this_param_generic"], "concrete this-param must not be stamped")
}
