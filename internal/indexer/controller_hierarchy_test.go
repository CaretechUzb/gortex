package indexer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func csTypeNode(id, name string, meta map[string]any) *graph.Node {
	return &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: id[:len(id)-len("::"+name)], Language: "csharp", Meta: meta,
	}
}

// TestPropagateAspNetControllerHierarchy_StampsTransitiveDescendants is
// the measured FleetLogistics gap: extraction stamps BaseController (it
// extends Controller directly, visible in its own file) but not the 44
// controllers that extend BaseController — the base's own base list
// lives in another file. The propagation walks the resolved hierarchy
// downward and stamps every descendant, including depth > 1.
func TestPropagateAspNetControllerHierarchy_StampsTransitiveDescendants(t *testing.T) {
	g := graph.New()
	base := csTypeNode("Base/BaseController.cs::BaseController", "BaseController",
		map[string]any{"entry_point": true, "entry_point_kind": "aspnet:controller"})
	claim := csTypeNode("AM/ClaimController.cs::ClaimController", "ClaimController", nil)
	special := csTypeNode("AM/SpecialClaimController.cs::SpecialClaimController", "SpecialClaimController", nil)
	unrelated := csTypeNode("AM/ClaimDto.cs::ClaimDto", "ClaimDto", nil)
	g.AddNode(base)
	g.AddNode(claim)
	g.AddNode(special)
	g.AddNode(unrelated)
	g.AddEdge(&graph.Edge{From: claim.ID, To: base.ID, Kind: graph.EdgeExtends, FilePath: "AM/ClaimController.cs", Line: 1})
	g.AddEdge(&graph.Edge{From: special.ID, To: claim.ID, Kind: graph.EdgeExtends, FilePath: "AM/SpecialClaimController.cs", Line: 1})

	stamped := propagateAspNetControllerHierarchy(g)
	require.Equal(t, 2, stamped)

	for _, n := range []*graph.Node{claim, special} {
		got := g.GetNode(n.ID)
		require.NotNil(t, got)
		assert.Equal(t, true, got.Meta["entry_point"], "%s must be stamped", n.ID)
		assert.Equal(t, "aspnet:controller", got.Meta["entry_point_kind"])
	}
	assert.Nil(t, g.GetNode(unrelated.ID).Meta["entry_point"], "non-derived type must stay unstamped")

	// Idempotent: a second sweep restamps nothing.
	require.Equal(t, 0, propagateAspNetControllerHierarchy(g))
}

// TestPropagateAspNetControllerHierarchy_Gates: only csharp KindType
// descendants along resolved extends edges qualify. An unresolved base
// target, a non-csharp child, and a non-controller seed all stay out.
func TestPropagateAspNetControllerHierarchy_Gates(t *testing.T) {
	g := graph.New()
	base := csTypeNode("Base/BaseController.cs::BaseController", "BaseController",
		map[string]any{"entry_point": true, "entry_point_kind": "aspnet:controller"})
	g.AddNode(base)

	// Non-csharp child of the controller hierarchy (contrived, but the
	// gate must hold).
	tsChild := &graph.Node{ID: "a.ts::Widget", Kind: graph.KindType, Name: "Widget",
		FilePath: "a.ts", Language: "typescript"}
	g.AddNode(tsChild)
	g.AddEdge(&graph.Edge{From: tsChild.ID, To: base.ID, Kind: graph.EdgeExtends, FilePath: "a.ts", Line: 1})

	// Descendant of a NON-controller entry point (a test class) must not
	// become a controller.
	testSeed := csTypeNode("T/Suite.cs::Suite", "Suite",
		map[string]any{"entry_point": true, "entry_point_kind": "xunit:test"})
	testChild := csTypeNode("T/DerivedSuite.cs::DerivedSuite", "DerivedSuite", nil)
	g.AddNode(testSeed)
	g.AddNode(testChild)
	g.AddEdge(&graph.Edge{From: testChild.ID, To: testSeed.ID, Kind: graph.EdgeExtends, FilePath: "T/DerivedSuite.cs", Line: 1})

	// Still-unresolved base target: nothing to walk.
	orphan := csTypeNode("O/OrphanController.cs::OrphanController", "OrphanController", nil)
	g.AddNode(orphan)
	g.AddEdge(&graph.Edge{From: orphan.ID, To: "unresolved::MissingBase", Kind: graph.EdgeExtends, FilePath: "O/OrphanController.cs", Line: 1})

	require.Equal(t, 0, propagateAspNetControllerHierarchy(g))
	assert.Nil(t, g.GetNode(tsChild.ID).Meta["entry_point"])
	assert.Nil(t, g.GetNode(testChild.ID).Meta["entry_point"])
	assert.Nil(t, g.GetNode(orphan.ID).Meta["entry_point"])
}
