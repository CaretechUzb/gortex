package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Fixture: two C# types named PricingRules in sibling namespaces. The
// lexicographic tie-break always picks Billing (smaller ID) — the
// narrowing must let the file's using directives pick Sales instead.
func csharpNSFixture() *graph.Graph {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "app/Billing/Rules/PricingRules.cs", Kind: graph.KindFile, Name: "PricingRules.cs", FilePath: "app/Billing/Rules/PricingRules.cs", Language: "csharp", RepoPrefix: "app"})
	g.AddNode(&graph.Node{
		ID: "app/Billing/Rules/PricingRules.cs::PricingRules", Kind: graph.KindType, Name: "PricingRules",
		FilePath: "app/Billing/Rules/PricingRules.cs", Language: "csharp", RepoPrefix: "app",
		Meta: map[string]any{"scope_ns": "App.Billing.Rules", "visibility": "public"},
	})
	g.AddNode(&graph.Node{ID: "app/Sales/Rules/PricingRules.cs", Kind: graph.KindFile, Name: "PricingRules.cs", FilePath: "app/Sales/Rules/PricingRules.cs", Language: "csharp", RepoPrefix: "app"})
	g.AddNode(&graph.Node{
		ID: "app/Sales/Rules/PricingRules.cs::PricingRules", Kind: graph.KindType, Name: "PricingRules",
		FilePath: "app/Sales/Rules/PricingRules.cs", Language: "csharp", RepoPrefix: "app",
		Meta: map[string]any{"scope_ns": "App.Sales.Rules", "visibility": "public"},
	})
	g.AddNode(&graph.Node{ID: "app/Web/Startup.cs", Kind: graph.KindFile, Name: "Startup.cs", FilePath: "app/Web/Startup.cs", Language: "csharp", RepoPrefix: "app"})
	g.AddNode(&graph.Node{
		ID: "app/Web/Startup.cs::Startup", Kind: graph.KindType, Name: "Startup",
		FilePath: "app/Web/Startup.cs", Language: "csharp", RepoPrefix: "app",
		Meta: map[string]any{"scope_ns": "App.Web", "visibility": "public"},
	})
	g.AddNode(&graph.Node{
		ID: "app/Web/Startup.cs::Startup.Configure", Kind: graph.KindMethod, Name: "Configure",
		FilePath: "app/Web/Startup.cs", Language: "csharp", RepoPrefix: "app",
	})
	g.AddEdge(&graph.Edge{From: "app/Web/Startup.cs", To: "app/Web/Startup.cs::Startup", Kind: graph.EdgeDefines, FilePath: "app/Web/Startup.cs", Line: 3})
	return g
}

// TestCSharpNamespaceNarrow_UsingWins: the referencing file imports
// App.Sales.Rules — the reference must bind there, not to the
// lexicographically-smaller Billing rival.
func TestCSharpNamespaceNarrow_UsingWins(t *testing.T) {
	g := csharpNSFixture()
	g.AddEdge(&graph.Edge{From: "app/Web/Startup.cs", To: "unresolved::import::App/Sales/Rules", Kind: graph.EdgeImports, FilePath: "app/Web/Startup.cs", Line: 1})
	ref := &graph.Edge{
		From: "app/Web/Startup.cs::Startup.Configure", To: "unresolved::PricingRules",
		Kind: graph.EdgeReferences, Origin: graph.OriginASTResolved, FilePath: "app/Web/Startup.cs", Line: 10,
	}
	g.AddEdge(ref)

	New(g).ResolveAll()

	assert.Equal(t, "app/Sales/Rules/PricingRules.cs::PricingRules", ref.To,
		"using directive must pick the imported namespace over the lexicographic rival")
}

// TestCSharpNamespaceNarrow_ExtendsPath: same disambiguation on the
// type-hierarchy path (resolveTypeRef) — a base-class reference follows
// the using directives too.
func TestCSharpNamespaceNarrow_ExtendsPath(t *testing.T) {
	g := csharpNSFixture()
	g.AddEdge(&graph.Edge{From: "app/Web/Startup.cs", To: "unresolved::import::App/Sales/Rules", Kind: graph.EdgeImports, FilePath: "app/Web/Startup.cs", Line: 1})
	ext := &graph.Edge{
		From: "app/Web/Startup.cs::Startup", To: "unresolved::PricingRules",
		Kind: graph.EdgeExtends, FilePath: "app/Web/Startup.cs", Line: 3,
	}
	g.AddEdge(ext)

	New(g).ResolveAll()

	assert.Equal(t, "app/Sales/Rules/PricingRules.cs::PricingRules", ext.To,
		"extends must follow the using directive")
}

// TestCSharpNamespaceNarrow_OwnNamespaceChain: no using needed — a file
// whose own namespace is App.Sales.Rules.Internal sees App.Sales.Rules
// through the enclosing-namespace chain, exactly as the compiler does.
func TestCSharpNamespaceNarrow_OwnNamespaceChain(t *testing.T) {
	g := csharpNSFixture()
	g.AddNode(&graph.Node{ID: "app/Sales/Rules/Sub/Helpers.cs", Kind: graph.KindFile, Name: "Helpers.cs", FilePath: "app/Sales/Rules/Sub/Helpers.cs", Language: "csharp", RepoPrefix: "app"})
	g.AddNode(&graph.Node{
		ID: "app/Sales/Rules/Sub/Helpers.cs::Helpers", Kind: graph.KindType, Name: "Helpers",
		FilePath: "app/Sales/Rules/Sub/Helpers.cs", Language: "csharp", RepoPrefix: "app",
		Meta: map[string]any{"scope_ns": "App.Sales.Rules.Internal", "visibility": "public"},
	})
	g.AddEdge(&graph.Edge{From: "app/Sales/Rules/Sub/Helpers.cs", To: "app/Sales/Rules/Sub/Helpers.cs::Helpers", Kind: graph.EdgeDefines, FilePath: "app/Sales/Rules/Sub/Helpers.cs", Line: 3})
	ref := &graph.Edge{
		From: "app/Sales/Rules/Sub/Helpers.cs::Helpers", To: "unresolved::PricingRules",
		Kind: graph.EdgeReferences, Origin: graph.OriginASTResolved, FilePath: "app/Sales/Rules/Sub/Helpers.cs", Line: 8,
	}
	g.AddEdge(ref)

	New(g).ResolveAll()

	assert.Equal(t, "app/Sales/Rules/PricingRules.cs::PricingRules", ref.To,
		"enclosing-namespace chain must reach the sibling namespace")
}

// TestCSharpNamespaceNarrow_NoMatchKeepsOldPick: narrowing only, never a
// loss — when no candidate namespace is imported (external assembly),
// the previous deterministic ranking stands and the edge stays resolved.
func TestCSharpNamespaceNarrow_NoMatchKeepsOldPick(t *testing.T) {
	g := csharpNSFixture()
	g.AddEdge(&graph.Edge{From: "app/Web/Startup.cs", To: "unresolved::import::ThirdParty/Sdk", Kind: graph.EdgeImports, FilePath: "app/Web/Startup.cs", Line: 1})
	ref := &graph.Edge{
		From: "app/Web/Startup.cs::Startup.Configure", To: "unresolved::PricingRules",
		Kind: graph.EdgeReferences, Origin: graph.OriginASTResolved, FilePath: "app/Web/Startup.cs", Line: 10,
	}
	g.AddEdge(ref)

	New(g).ResolveAll()

	require.NotContains(t, ref.To, "unresolved::", "edge must not be lost")
	assert.Equal(t, "app/Billing/Rules/PricingRules.cs::PricingRules", ref.To,
		"without namespace evidence the previous deterministic pick stands")
}

// TestCSharpNamespaceNarrow_EnclosingBeatsImported: the compiler searches
// enclosing namespaces before using directives — an internal type in the
// file's own namespace wins over a public same-named type in an imported
// one, even though the ranker prefers exported candidates.
func TestCSharpNamespaceNarrow_EnclosingBeatsImported(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "app/Billing/Module.cs", Kind: graph.KindFile, Name: "Module.cs", FilePath: "app/Billing/Module.cs", Language: "csharp", RepoPrefix: "app"})
	g.AddNode(&graph.Node{
		ID: "app/Billing/Module.cs::Module", Kind: graph.KindType, Name: "Module",
		FilePath: "app/Billing/Module.cs", Language: "csharp", RepoPrefix: "app",
		Meta: map[string]any{"scope_ns": "App.Billing", "visibility": "public"},
	})
	g.AddEdge(&graph.Edge{From: "app/Billing/Module.cs", To: "app/Billing/Module.cs::Module", Kind: graph.EdgeDefines, FilePath: "app/Billing/Module.cs", Line: 3})
	g.AddEdge(&graph.Edge{From: "app/Billing/Module.cs", To: "unresolved::import::App/Shared/Lookup", Kind: graph.EdgeImports, FilePath: "app/Billing/Module.cs", Line: 1})
	// Own-namespace candidate: internal (ranked below public by the
	// canonical-definition ranker).
	g.AddNode(&graph.Node{ID: "app/Billing/Rules.cs", Kind: graph.KindFile, Name: "Rules.cs", FilePath: "app/Billing/Rules.cs", Language: "csharp", RepoPrefix: "app"})
	g.AddNode(&graph.Node{
		ID: "app/Billing/Rules.cs::Rules", Kind: graph.KindType, Name: "Rules",
		FilePath: "app/Billing/Rules.cs", Language: "csharp", RepoPrefix: "app",
		Meta: map[string]any{"scope_ns": "App.Billing", "visibility": "internal"},
	})
	// Imported-namespace candidate: public.
	g.AddNode(&graph.Node{ID: "app/Shared/Lookup/Rules.cs", Kind: graph.KindFile, Name: "Rules.cs", FilePath: "app/Shared/Lookup/Rules.cs", Language: "csharp", RepoPrefix: "app"})
	g.AddNode(&graph.Node{
		ID: "app/Shared/Lookup/Rules.cs::Rules", Kind: graph.KindType, Name: "Rules",
		FilePath: "app/Shared/Lookup/Rules.cs", Language: "csharp", RepoPrefix: "app",
		Meta: map[string]any{"scope_ns": "App.Shared.Lookup", "visibility": "public"},
	})
	ref := &graph.Edge{
		From: "app/Billing/Module.cs::Module", To: "unresolved::Rules",
		Kind: graph.EdgeReferences, Origin: graph.OriginASTResolved, FilePath: "app/Billing/Module.cs", Line: 10,
	}
	g.AddEdge(ref)

	New(g).ResolveAll()

	assert.Equal(t, "app/Billing/Rules.cs::Rules", ref.To,
		"the enclosing-namespace type wins over an imported one regardless of visibility rank")
}

// TestCSharpNamespaceNarrow_ExternalImportShape: by the time a reference
// resolves, the same-pass import resolution may already have rewritten
// the using edge to its external:: form — both shapes must count.
func TestCSharpNamespaceNarrow_ExternalImportShape(t *testing.T) {
	g := csharpNSFixture()
	g.AddEdge(&graph.Edge{From: "app/Web/Startup.cs", To: "external::App/Sales/Rules", Kind: graph.EdgeImports, FilePath: "app/Web/Startup.cs", Line: 1})
	ref := &graph.Edge{
		From: "app/Web/Startup.cs::Startup.Configure", To: "unresolved::PricingRules",
		Kind: graph.EdgeReferences, Origin: graph.OriginASTResolved, FilePath: "app/Web/Startup.cs", Line: 10,
	}
	g.AddEdge(ref)

	New(g).ResolveAll()

	assert.Equal(t, "app/Sales/Rules/PricingRules.cs::PricingRules", ref.To,
		"an already-resolved external:: using edge still carries the namespace")
}
