package resolver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestLanguageFamily(t *testing.T) {
	cases := map[string]string{
		"java": "jvm", "kotlin": "jvm", "scala": "jvm",
		"swift": "apple", "objc": "apple",
		"typescript": "web", "tsx": "web", "javascript": "web",
		"c": "c", "cpp": "c",
		"csharp": "dotnet", "fsharp": "dotnet", "razor": "dotnet",
		"go": "", "python": "", "rust": "", "": "",
	}
	for lang, fam := range cases {
		assert.Equal(t, fam, languageFamily(lang), "languageFamily(%q)", lang)
	}
}

func TestSameLanguageFamily(t *testing.T) {
	assert.True(t, sameLanguageFamily("csharp", "razor"), "csharp↔razor are both dotnet")
	assert.True(t, sameLanguageFamily("typescript", "tsx"), "ts↔tsx are both web")
	assert.True(t, sameLanguageFamily("java", "kotlin"), "java↔kotlin are both jvm")
	assert.True(t, sameLanguageFamily("razor", "razor"), "same language")
	assert.True(t, sameLanguageFamily("go", "go"), "same language, even with no family")
	assert.False(t, sameLanguageFamily("razor", "typescript"), "dotnet vs web")
	assert.False(t, sameLanguageFamily("", ""), "empty language is no family")
	assert.False(t, sameLanguageFamily("go", "python"), "two familyless languages do not match")
}

func TestGateFrameworkResult(t *testing.T) {
	// Drop: a non-bridge synth result crossing dotnet↔web.
	assert.True(t, gateFrameworkResult(SynthRustScope, "razor", "typescript"))
	// Exempt bridges: JS→native and Swift→ObjC are never gated.
	assert.False(t, gateFrameworkResult(SynthReactNative, "javascript", "swift"))
	assert.False(t, gateFrameworkResult(SynthSwiftObjC, "swift", "objc"))
	// Same family → permit.
	assert.False(t, gateFrameworkResult(SynthRustScope, "csharp", "razor"))
	// Unknown family on a side → permit.
	assert.False(t, gateFrameworkResult(SynthRustScope, "go", "typescript"))
}

// TestApplyFrameworkFamilyGate_DropsCrossFamilyRef pins the post-filter dropping
// a synthesized cross-family reference from a non-bridge synthesizer.
func TestApplyFrameworkFamilyGate_DropsCrossFamilyRef(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.razor::Page", Kind: graph.KindType, Name: "Page", Language: "razor"})
	g.AddNode(&graph.Node{ID: "b.tsx::Counter", Kind: graph.KindType, Name: "Counter", Language: "typescript"})
	g.AddEdge(&graph.Edge{
		From: "a.razor::Page", To: "b.tsx::Counter", Kind: graph.EdgeReferences,
		Meta: map[string]any{MetaSynthesizedBy: SynthRustScope},
	})

	assert.Equal(t, 1, applyFrameworkFamilyGate(g))
	assert.False(t, g.RemoveEdge("a.razor::Page", "b.tsx::Counter", graph.EdgeReferences),
		"edge already removed by the gate")
}

func TestApplyFrameworkFamilyGate_SQLiteProjectsThenExactRefetches(t *testing.T) {
	g, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, g.Close()) })

	g.AddBatch([]*graph.Node{
		{ID: "page", Kind: graph.KindType, Language: "razor"},
		{ID: "component", Kind: graph.KindType, Language: "typescript"},
	}, []*graph.Edge{
		{From: "page", To: "component", Kind: graph.EdgeReferences, FilePath: "a", Line: 1, Meta: map[string]any{MetaSynthesizedBy: SynthRustScope}},
		{From: "page", To: "component", Kind: graph.EdgeImports, FilePath: "a", Line: 2, Meta: map[string]any{MetaSynthesizedBy: SynthRustScope}},
		{From: "page", To: "component", Kind: graph.EdgeReferences, FilePath: "a", Line: 3},
	})

	require.Equal(t, 2, applyFrameworkFamilyGate(g))
	remaining := g.GetOutEdges("page")
	require.Len(t, remaining, 1)
	require.Equal(t, 3, remaining[0].Line, "the exact refetch must not remove an unowned edge")
}

// TestApplyFrameworkFamilyGate_KeepsBridge pins that a bridge-synthesizer edge
// is exempt even when it crosses families.
func TestApplyFrameworkFamilyGate_KeepsBridge(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.js::foo", Kind: graph.KindFunction, Name: "foo", Language: "javascript"})
	g.AddNode(&graph.Node{ID: "b.swift::Bar", Kind: graph.KindType, Name: "Bar", Language: "swift"})
	g.AddEdge(&graph.Edge{
		From: "a.js::foo", To: "b.swift::Bar", Kind: graph.EdgeReferences,
		Meta: map[string]any{MetaSynthesizedBy: SynthReactNative},
	})

	assert.Equal(t, 0, applyFrameworkFamilyGate(g), "bridge synthesizer edge is exempt")
}
