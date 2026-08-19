package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// #605: the per-file sweep gate. Under the demand default a file used to earn
// its sweep slot by declaring ANY type or interface — true for essentially
// every C# file, so the gate admitted the whole repo. The dispatch half must
// be as discriminating as the per-callable incoming-calls gate already is:
// a file sweeps when it carries unresolved demand, a dispatch-relevant
// callable, or a type actually involved in a super/subtype hierarchy. A
// plain data type with no bases, no subtypes, and no dispatch-relevant
// members buys nothing from hover or hierarchy interrogation.

// The type half of the file gate. Interfaces stay in unconditionally: an
// interface is the dispatch surface by definition, and the AST edges of its
// implementers may be exactly what failed to resolve — the case where it
// looks adjacency-less is the case where the sweep is most needed. A class
// earns its slot only through hierarchy involvement, and edge KINDS survive
// even when the AST could not resolve the target, so a class with an
// unresolvable base list still qualifies.
func TestEnrichTypeIsDispatchRelevantFromView(t *testing.T) {
	iface := &graph.Node{ID: "s.cs::IShape", Kind: graph.KindInterface, Name: "IShape"}
	impl := &graph.Node{ID: "s.cs::Circle", Kind: graph.KindType, Name: "Circle"}
	unresolvedBase := &graph.Node{ID: "s.cs::Widget", Kind: graph.KindType, Name: "Widget"}
	superType := &graph.Node{ID: "s.cs::Animal", Kind: graph.KindType, Name: "Animal"}
	sub := &graph.Node{ID: "s.cs::Dog", Kind: graph.KindType, Name: "Dog"}
	poco := &graph.Node{ID: "s.cs::Box", Kind: graph.KindType, Name: "Box"}
	method := &graph.Node{ID: "s.cs::Box.Size", Kind: graph.KindMethod, Name: "Size"}

	view := newLSPGraphView(
		[]*graph.Node{iface, impl, unresolvedBase, superType, sub, poco, method},
		[]*graph.Edge{
			{From: impl.ID, To: iface.ID, Kind: graph.EdgeImplements},
			{From: unresolvedBase.ID, To: graph.UnresolvedMarker + "VendorBase", Kind: graph.EdgeExtends},
			{From: sub.ID, To: superType.ID, Kind: graph.EdgeExtends},
			{From: method.ID, To: poco.ID, Kind: graph.EdgeMemberOf},
		},
	)

	assert.True(t, enrichTypeIsDispatchRelevantFromView(view, iface), "an interface is always dispatch surface")
	assert.True(t, enrichTypeIsDispatchRelevantFromView(view, impl), "a type implementing an interface")
	assert.True(t, enrichTypeIsDispatchRelevantFromView(view, unresolvedBase), "an unresolvable base list still counts — the sweep is the only path that recovers it")
	assert.True(t, enrichTypeIsDispatchRelevantFromView(view, superType), "a type something else extends")
	assert.False(t, enrichTypeIsDispatchRelevantFromView(view, poco), "a bare data type is not")
	assert.False(t, enrichTypeIsDispatchRelevantFromView(view, method), "callables have their own predicate")
	assert.False(t, enrichTypeIsDispatchRelevantFromView(view, nil))
}

// TestLSP_Enrich_SweepGate drives one pass over three files under the demand
// default and asserts per-file sweep decisions by the hover requests the
// server saw:
//   - poco.go: a bare struct + plain method, no demand — must be SKIPPED.
//   - hier.go: a type implementing an interface — must be swept.
//   - want.go: no types, but a declaration with an unresolved same-name
//     candidate — the demand half must still admit it.
func TestLSP_Enrich_SweepGate(t *testing.T) {
	t.Setenv(SweepEnv, "") // demand default

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "poco.go"),
		[]byte("package p\n\ntype Box struct{}\n\nfunc (b Box) Size() int { return 0 }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "hier.go"),
		[]byte("package p\n\ntype Shape interface{ Area() float64 }\n\ntype Circle struct{}\n\nfunc (c Circle) Area() float64 { return 0 }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "want.go"),
		[]byte("package p\n\nfunc Free() {}\n"), 0o644))

	server := newFakeLSPServer()
	var mu sync.Mutex
	hoveredURIs := map[string]bool{}
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		var req struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(params, &req)
		mu.Lock()
		hoveredURIs[req.TextDocument.URI] = true
		mu.Unlock()
		return nil, nil
	})

	p, cleanup := providerWithFakeServer(t, server, []string{"go"})
	defer cleanup()

	g := graph.New()
	// poco.go — a type and its method, hierarchy-uninvolved, no demand.
	g.AddNode(&graph.Node{ID: "poco.go::Box", Kind: graph.KindType, Name: "Box",
		FilePath: "poco.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddNode(&graph.Node{ID: "poco.go::Box.Size", Kind: graph.KindMethod, Name: "Size",
		FilePath: "poco.go", StartLine: 5, EndLine: 5, Language: "go"})
	g.AddEdge(&graph.Edge{From: "poco.go::Box.Size", To: "poco.go::Box", Kind: graph.EdgeMemberOf})
	// hier.go — a type that implements an interface declared beside it.
	g.AddNode(&graph.Node{ID: "hier.go::Shape", Kind: graph.KindInterface, Name: "Shape",
		FilePath: "hier.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddNode(&graph.Node{ID: "hier.go::Circle", Kind: graph.KindType, Name: "Circle",
		FilePath: "hier.go", StartLine: 5, EndLine: 5, Language: "go"})
	g.AddNode(&graph.Node{ID: "hier.go::Circle.Area", Kind: graph.KindMethod, Name: "Area",
		FilePath: "hier.go", StartLine: 7, EndLine: 7, Language: "go"})
	g.AddEdge(&graph.Edge{From: "hier.go::Circle.Area", To: "hier.go::Circle", Kind: graph.EdgeMemberOf})
	g.AddEdge(&graph.Edge{From: "hier.go::Circle", To: "hier.go::Shape", Kind: graph.EdgeImplements})
	// want.go — no types at all; Free still has an unresolved same-name
	// candidate, so the demand half of the gate must admit the file.
	g.AddNode(&graph.Node{ID: "want.go::Free", Kind: graph.KindFunction, Name: "Free",
		FilePath: "want.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddEdge(&graph.Edge{From: "hier.go::Circle.Area", To: graph.UnresolvedMarker + "*.Free",
		Kind: graph.EdgeCalls, FilePath: "hier.go", Line: 7})

	require.NoError(t, runEnrich(t, p, g, repoRoot, 3*time.Second))

	mu.Lock()
	defer mu.Unlock()
	assert.False(t, hoveredURIs[pathToURI(filepath.Join(repoRoot, "poco.go"))],
		"a hierarchy-uninvolved data type must not keep its file in the sweep")
	assert.True(t, hoveredURIs[pathToURI(filepath.Join(repoRoot, "hier.go"))],
		"a type involved in a hierarchy keeps its file in the sweep")
	assert.True(t, hoveredURIs[pathToURI(filepath.Join(repoRoot, "want.go"))],
		"unresolved demand keeps a type-less file in the sweep")
}
