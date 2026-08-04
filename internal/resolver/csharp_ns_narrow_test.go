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

// stampUsings sets the extractor's Meta["usings"] shape on a fixture
// file node — the primary using evidence the narrowing consumes.
func stampUsings(g *graph.Graph, fileID string, usings ...string) {
	n := g.GetNode(fileID)
	if n.Meta == nil {
		n.Meta = map[string]any{}
	}
	n.Meta["usings"] = usings
}

// TestCSharpNamespaceNarrow_UsingWins: the referencing file imports
// App.Sales.Rules — the reference must bind there, not to the
// lexicographically-smaller Billing rival.
func TestCSharpNamespaceNarrow_UsingWins(t *testing.T) {
	g := csharpNSFixture()
	stampUsings(g, "app/Web/Startup.cs", "App.Sales.Rules")
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
	stampUsings(g, "app/Web/Startup.cs", "App.Sales.Rules")
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
	stampUsings(g, "app/Web/Startup.cs", "ThirdParty.Sdk")
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
	stampUsings(g, "app/Billing/Module.cs", "App.Shared.Lookup")
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

// TestCSharpNamespaceNarrow_QualifiedReference: a fully qualified
// spelling needs no using directive at all — the qualifier stamped as
// Meta["target_fqn"] must pick its namespace exactly, even when the
// referencing file imports nothing relevant.
func TestCSharpNamespaceNarrow_QualifiedReference(t *testing.T) {
	g := csharpNSFixture()
	ref := &graph.Edge{
		From: "app/Web/Startup.cs::Startup.Configure", To: "unresolved::PricingRules",
		Kind: graph.EdgeReferences, Origin: graph.OriginASTResolved, FilePath: "app/Web/Startup.cs", Line: 10,
		Meta: map[string]any{"target_fqn": "App.Sales.Rules.PricingRules"},
	}
	g.AddEdge(ref)

	New(g).ResolveAll()

	assert.Equal(t, "app/Sales/Rules/PricingRules.cs::PricingRules", ref.To,
		"the written qualifier must pick the namespace without any using")
}

// TestCSharpNamespaceNarrow_PartialQualifier: C# resolves a partially
// qualified spelling (`Rules.PricingRules`) against visible namespaces —
// a qualifier suffix-match must narrow before the using tier can pick a
// same-named type in a namespace that does not end in the qualifier.
func TestCSharpNamespaceNarrow_PartialQualifier(t *testing.T) {
	g := csharpNSFixture()
	// A third rival whose namespace the file imports but which does not
	// end in the written qualifier.
	g.AddNode(&graph.Node{ID: "app/Shared/PricingRules.cs", Kind: graph.KindFile, Name: "PricingRules.cs", FilePath: "app/Shared/PricingRules.cs", Language: "csharp", RepoPrefix: "app"})
	g.AddNode(&graph.Node{
		ID: "app/Shared/PricingRules.cs::PricingRules", Kind: graph.KindType, Name: "PricingRules",
		FilePath: "app/Shared/PricingRules.cs", Language: "csharp", RepoPrefix: "app",
		Meta: map[string]any{"scope_ns": "App.Shared", "visibility": "public"},
	})
	stampUsings(g, "app/Web/Startup.cs", "App.Shared")
	ref := &graph.Edge{
		From: "app/Web/Startup.cs::Startup.Configure", To: "unresolved::PricingRules",
		Kind: graph.EdgeReferences, Origin: graph.OriginASTResolved, FilePath: "app/Web/Startup.cs", Line: 10,
		Meta: map[string]any{"target_fqn": "Sales.Rules.PricingRules"},
	}
	g.AddEdge(ref)

	New(g).ResolveAll()

	assert.Equal(t, "app/Sales/Rules/PricingRules.cs::PricingRules", ref.To,
		"the qualifier suffix must beat the imported same-named rival")
}

// TestCSharpNamespaceNarrow_RewrittenImportEdge: resolveImport rewrites
// a using edge to a file-node target when the namespace tail matches a
// directory basename — the Meta["usings"] stamp must keep the evidence
// alive regardless of what shape the edge is in.
func TestCSharpNamespaceNarrow_RewrittenImportEdge(t *testing.T) {
	g := csharpNSFixture()
	stampUsings(g, "app/Web/Startup.cs", "App.Sales.Rules")
	// The edge as resolveImport leaves it: a concrete file-node target.
	g.AddEdge(&graph.Edge{From: "app/Web/Startup.cs", To: "app/Sales/Rules/PricingRules.cs", Kind: graph.EdgeImports, FilePath: "app/Web/Startup.cs", Line: 1})
	ref := &graph.Edge{
		From: "app/Web/Startup.cs::Startup.Configure", To: "unresolved::PricingRules",
		Kind: graph.EdgeReferences, Origin: graph.OriginASTResolved, FilePath: "app/Web/Startup.cs", Line: 10,
	}
	g.AddEdge(ref)

	New(g).ResolveAll()

	assert.Equal(t, "app/Sales/Rules/PricingRules.cs::PricingRules", ref.To,
		"a rewritten import edge must not erase the using evidence")
}

// TestCSharpNamespaceNarrow_MethodDecoyCannotStealOrLose: methods carry
// scope_ns too — a same-named method in the caller's own namespace must
// not narrow the set to itself (stealing the bind on the type-or-func
// path, losing the edge outright on the type-only extends path).
func TestCSharpNamespaceNarrow_MethodDecoyCannotStealOrLose(t *testing.T) {
	g := csharpNSFixture()
	stampUsings(g, "app/Web/Startup.cs", "App.Sales.Rules")
	// A method named like the type, in the caller's own namespace — the
	// enclosing tier would outrank the imported type if kinds were ignored.
	g.AddNode(&graph.Node{
		ID: "app/Web/Helpers.cs::Helpers.PricingRules", Kind: graph.KindMethod, Name: "PricingRules",
		FilePath: "app/Web/Helpers.cs", Language: "csharp", RepoPrefix: "app",
		Meta: map[string]any{"scope_ns": "App.Web"},
	})
	ext := &graph.Edge{
		From: "app/Web/Startup.cs::Startup", To: "unresolved::PricingRules",
		Kind: graph.EdgeExtends, FilePath: "app/Web/Startup.cs", Line: 3,
	}
	inst := &graph.Edge{
		From: "app/Web/Startup.cs::Startup.Configure", To: "unresolved::PricingRules",
		Kind: graph.EdgeInstantiates, Origin: graph.OriginASTResolved, FilePath: "app/Web/Startup.cs", Line: 10,
	}
	g.AddEdge(ext)
	g.AddEdge(inst)

	New(g).ResolveAll()

	assert.Equal(t, "app/Sales/Rules/PricingRules.cs::PricingRules", ext.To,
		"extends must not be lost to a same-named method decoy")
	assert.Equal(t, "app/Sales/Rules/PricingRules.cs::PricingRules", inst.To,
		"instantiates must bind the type, not the method decoy")
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

// TestCSharpGlobalUsings_WindowsPathIDs: production Windows stores join
// the repo prefix with "/" but keep OS-native "\" below it (the
// indexer's graphRelKey shape, e.g. `repo/App\Mod\File.cs`). The
// directory walk must honour both separators — splitting on "/" alone
// collapses every project to the repo root and leaks one project's
// global usings into its siblings.
func TestCSharpGlobalUsings_WindowsPathIDs(t *testing.T) {
	g := graph.New()
	add := func(id string, meta map[string]any) {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFile, Name: id, FilePath: id, Language: "csharp", Meta: meta})
	}
	add(`Repo/ProjA\Usings.cs`, map[string]any{"global_usings": []string{"LibA"}})
	add(`Repo/ProjA\Caller.cs`, nil)
	add(`Repo/ProjB\Caller.cs`, nil)

	r := New(g)
	got := r.csharpGlobalUsingsFor(`Repo/ProjA\Caller.cs`)
	if len(got) != 1 || got[0] != "LibA" {
		t.Fatalf("same-project caller must inherit the global using, got %v", got)
	}
	if leaked := r.csharpGlobalUsingsFor(`Repo/ProjB\Caller.cs`); len(leaked) != 0 {
		t.Fatalf("sibling project must not inherit the global using, got %v", leaked)
	}
}

// TestCSharpFileNamespaceSet_EdgeFallbackSurvivesSiblingGlobals: a file
// extracted before the usings stamp existed (no Meta["usings"], only
// EdgeImports) must keep its edge-derived usings even when a sibling
// file contributes global usings — inherited globals are not "this file
// has a stamp".
func TestCSharpFileNamespaceSet_EdgeFallbackSurvivesSiblingGlobals(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "Proj/Legacy.cs", Kind: graph.KindFile, FilePath: "Proj/Legacy.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: "Proj/Usings.cs", Kind: graph.KindFile, FilePath: "Proj/Usings.cs", Language: "csharp",
		Meta: map[string]any{"global_usings": []string{"GlobalNs"}}})
	g.AddEdge(&graph.Edge{From: "Proj/Legacy.cs", To: "unresolved::import::My/Ns", Kind: graph.EdgeImports, FilePath: "Proj/Legacy.cs"})

	r := New(g)
	ns := r.csharpFileNamespaceSet("Proj/Legacy.cs")
	if _, ok := ns.imported["My.Ns"]; !ok {
		t.Fatalf("edge-derived using lost when a sibling stamps globals: %v", ns.imported)
	}
	if _, ok := ns.imported["GlobalNs"]; !ok {
		t.Fatalf("sibling global using missing: %v", ns.imported)
	}
}

// TestCSharpGlobalUsings_CsprojUnitScoping: the compilation unit is the
// nearest-ancestor csproj directory, in both directions — a global
// using in Proj/Features/ applies project-wide (not just its subtree),
// and a nested project with its own csproj is a separate compilation
// that inherits nothing from the outer one.
func TestCSharpGlobalUsings_CsprojUnitScoping(t *testing.T) {
	g := graph.New()
	file := func(id string, meta map[string]any) {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFile, Name: id, FilePath: id, Language: "csharp", Meta: meta})
	}
	file("Proj/App.csproj", nil)
	file("Proj/Usings.cs", map[string]any{"global_usings": []string{"LibB"}})
	file("Proj/Features/Usings.cs", map[string]any{"global_usings": []string{"LibA"}})
	file("Proj/Web/Caller.cs", nil)
	file("Proj/SubProj/Sub.csproj", nil)
	file("Proj/SubProj/Caller.cs", nil)

	r := New(g)
	got := r.csharpGlobalUsingsFor("Proj/Web/Caller.cs")
	set := map[string]bool{}
	for _, u := range got {
		set[u] = true
	}
	if !set["LibA"] || !set["LibB"] || len(got) != 2 {
		t.Fatalf("project-wide inheritance across subtrees expected [LibA LibB], got %v", got)
	}
	if leaked := r.csharpGlobalUsingsFor("Proj/SubProj/Caller.cs"); len(leaked) != 0 {
		t.Fatalf("nested project with its own csproj must not inherit the outer globals, got %v", leaked)
	}
}

// TestCSharpFileNamespaceSet_AnySliceMetaShapes: legacy sqlite rows
// deserialize the usings stamps as []any, not []string — every consumer
// path must read both shapes, or narrowing silently dies on old stores.
func TestCSharpFileNamespaceSet_AnySliceMetaShapes(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "Proj/Caller.cs", Kind: graph.KindFile, FilePath: "Proj/Caller.cs", Language: "csharp",
		Meta: map[string]any{
			"usings":       []any{"My.Ns"},
			"using_static": []any{"LibB.EB"},
		}})
	g.AddNode(&graph.Node{ID: "Proj/Usings.cs", Kind: graph.KindFile, FilePath: "Proj/Usings.cs", Language: "csharp",
		Meta: map[string]any{"global_usings": []any{"LibA"}}})

	r := New(g)
	ns := r.csharpFileNamespaceSet("Proj/Caller.cs")
	if _, ok := ns.imported["My.Ns"]; !ok {
		t.Fatalf("[]any usings not read: %v", ns.imported)
	}
	if _, ok := ns.imported["LibA"]; !ok {
		t.Fatalf("[]any global_usings not propagated: %v", ns.imported)
	}
	if _, ok := ns.statics["LibB.EB"]; !ok {
		t.Fatalf("[]any using_static not read: %v", ns.statics)
	}
}
