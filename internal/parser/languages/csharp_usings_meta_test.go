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
