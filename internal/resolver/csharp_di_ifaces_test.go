package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func diIfacesMarker(fileID, impl string) *graph.Edge {
	return &graph.Edge{
		From: fileID, To: graph.UnresolvedMarker + impl,
		Kind: graph.EdgeProvides, FilePath: fileID, Line: 3,
		Origin: graph.OriginASTInferred,
		Meta:   map[string]any{"binding": "asImplementedInterfaces", "di": "autofac"},
	}
}

func collectUseClassProvides(g *graph.Graph, fileID string) map[string]string {
	out := map[string]string{}
	for _, e := range g.GetOutEdges(fileID) {
		if e.Kind != graph.EdgeProvides || e.Meta == nil {
			continue
		}
		if b, _ := e.Meta["binding"].(string); b != "useClass" {
			continue
		}
		pf, _ := e.Meta["provides_for"].(string)
		impl, _ := e.Meta["impl_fqcn"].(string)
		out[pf] = impl
	}
	return out
}

// TestResolveCSharpDIImplementedInterfaces_ExpandsMarker is the core
// join: a marker for Foo plus Foo's declared implements set becomes one
// useClass binding per interface, and a second run adds nothing.
func TestResolveCSharpDIImplementedInterfaces_ExpandsMarker(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "Module.cs", Kind: graph.KindFile, FilePath: "Module.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: "Svc.cs::AuthService", Kind: graph.KindType, Name: "AuthService", FilePath: "Svc.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: "IA.cs::IAuthService", Kind: graph.KindInterface, Name: "IAuthService", FilePath: "IA.cs", StartLine: 5, Language: "csharp"})
	g.AddNode(&graph.Node{ID: "IT.cs::ITokenIssuer", Kind: graph.KindInterface, Name: "ITokenIssuer", FilePath: "IT.cs", StartLine: 3, Language: "csharp"})
	// One implements target already bound, one still an unresolved stub
	// that lands via the unique same-repo name lookup — both bind. An
	// external framework interface with no in-repo node does not.
	g.AddEdge(&graph.Edge{From: "Svc.cs::AuthService", To: "IA.cs::IAuthService", Kind: graph.EdgeImplements, FilePath: "Svc.cs", Line: 1})
	g.AddEdge(&graph.Edge{From: "Svc.cs::AuthService", To: "unresolved::ITokenIssuer", Kind: graph.EdgeImplements, FilePath: "Svc.cs", Line: 1})
	g.AddEdge(&graph.Edge{From: "Svc.cs::AuthService", To: "unresolved::IDisposable", Kind: graph.EdgeImplements, FilePath: "Svc.cs", Line: 1})
	g.AddEdge(diIfacesMarker("Module.cs", "AuthService"))

	require.Equal(t, 2, ResolveCSharpDIImplementedInterfaces(g))
	got := collectUseClassProvides(g, "Module.cs")
	assert.Equal(t, "AuthService", got["IAuthService"])
	assert.Equal(t, "AuthService", got["ITokenIssuer"])
	_, boundExternal := got["IDisposable"]
	assert.False(t, boundExternal, "an interface with no in-repo node must not be bound")

	// Idempotent.
	require.Equal(t, 0, ResolveCSharpDIImplementedInterfaces(g))
}

// TestResolveCSharpDIImplementedInterfaces_SkipsUnsafeJoins: an
// ambiguous impl name, a method-set-inferred implements edge, and an
// interface already bound by an explicit .As<>() must each add nothing.
func TestResolveCSharpDIImplementedInterfaces_SkipsUnsafeJoins(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "Module.cs", Kind: graph.KindFile, FilePath: "Module.cs", Language: "csharp"})

	// Ambiguous: two same-named csharp types.
	g.AddNode(&graph.Node{ID: "A.cs::Dup", Kind: graph.KindType, Name: "Dup", FilePath: "A.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: "B.cs::Dup", Kind: graph.KindType, Name: "Dup", FilePath: "B.cs", Language: "csharp"})
	g.AddEdge(&graph.Edge{From: "A.cs::Dup", To: "unresolved::IDup", Kind: graph.EdgeImplements, FilePath: "A.cs", Line: 1})
	g.AddEdge(diIfacesMarker("Module.cs", "Dup"))

	// Inferred implements: must not become a DI binding.
	g.AddNode(&graph.Node{ID: "C.cs::Solo", Kind: graph.KindType, Name: "Solo", FilePath: "C.cs", Language: "csharp"})
	g.AddEdge(&graph.Edge{From: "C.cs::Solo", To: "unresolved::IShape", Kind: graph.EdgeImplements, FilePath: "C.cs", Line: 1,
		Meta: map[string]any{"via": MetaViaMethodSetInference}})
	g.AddEdge(diIfacesMarker("Module.cs", "Solo"))

	// Already bound explicitly: dedup against the existing useClass edge.
	g.AddNode(&graph.Node{ID: "D.cs::Writer", Kind: graph.KindType, Name: "Writer", FilePath: "D.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: "IW.cs::IWriter", Kind: graph.KindInterface, Name: "IWriter", FilePath: "IW.cs", StartLine: 1, Language: "csharp"})
	g.AddEdge(&graph.Edge{From: "D.cs::Writer", To: "unresolved::IWriter", Kind: graph.EdgeImplements, FilePath: "D.cs", Line: 1})
	g.AddEdge(&graph.Edge{From: "Module.cs", To: "unresolved::Writer", Kind: graph.EdgeProvides, FilePath: "Module.cs", Line: 2,
		Origin: graph.OriginASTInferred,
		Meta:   map[string]any{"binding": "useClass", "provides_for": "IWriter", "impl_fqcn": "Writer", "di": "autofac"}})
	g.AddEdge(diIfacesMarker("Module.cs", "Writer"))

	require.Equal(t, 0, ResolveCSharpDIImplementedInterfaces(g))
}

// TestResolveCSharpDIImplementedInterfaces_PrefersCanonicalOverHusk: a
// merged-away partial fragment shares the impl's name but must not make
// the lookup ambiguous — the join lands on the canonical node, which
// carries the folded implements set.
func TestResolveCSharpDIImplementedInterfaces_PrefersCanonicalOverHusk(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "Module.cs", Kind: graph.KindFile, FilePath: "Module.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: "A.cs::Svc", Kind: graph.KindType, Name: "Svc", FilePath: "A.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: "B.cs::Svc", Kind: graph.KindType, Name: "Svc", FilePath: "B.cs", Language: "csharp",
		Meta: map[string]any{"partial_merged_into": "A.cs::Svc"}})
	g.AddNode(&graph.Node{ID: "IS.cs::ISvc", Kind: graph.KindInterface, Name: "ISvc", FilePath: "IS.cs", StartLine: 1, Language: "csharp"})
	g.AddEdge(&graph.Edge{From: "A.cs::Svc", To: "unresolved::ISvc", Kind: graph.EdgeImplements, FilePath: "A.cs", Line: 1})
	g.AddEdge(diIfacesMarker("Module.cs", "Svc"))

	require.Equal(t, 1, ResolveCSharpDIImplementedInterfaces(g))
	assert.Equal(t, "Svc", collectUseClassProvides(g, "Module.cs")["ISvc"])
}
