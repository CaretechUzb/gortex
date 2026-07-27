package entrypoints

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func hierType(id, name, lang string, meta map[string]any) *graph.Node {
	return &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: id[:len(id)-len("::"+name)], Language: lang, Meta: meta,
	}
}

func controllerMeta() map[string]any {
	return map[string]any{MetaEntryPoint: true, MetaEntryKind: "aspnet:controller"}
}

func extendsEdge(from, to string) *graph.Edge {
	return &graph.Edge{From: from, To: to, Kind: graph.EdgeExtends, FilePath: "x.cs", Line: 1}
}

// TestPropagateEntryPointsDownHierarchy_StampsTransitiveDescendants is
// the measured reference-codebase gap: extraction stamps BaseController
// (it extends Controller directly, visible in its own file) but not the
// 44 controllers that extend BaseController — the base's own base list
// lives in another file. The inverted walk seeds from the stamped base
// and stamps every descendant, including depth > 1.
func TestPropagateEntryPointsDownHierarchy_StampsTransitiveDescendants(t *testing.T) {
	g := graph.New()
	base := hierType("Base/BaseController.cs::BaseController", "BaseController", "csharp", controllerMeta())
	claim := hierType("AM/ClaimController.cs::ClaimController", "ClaimController", "csharp", nil)
	special := hierType("AM/SpecialClaimController.cs::SpecialClaimController", "SpecialClaimController", "csharp", nil)
	unrelated := hierType("AM/ClaimDto.cs::ClaimDto", "ClaimDto", "csharp", nil)
	g.AddNode(base)
	g.AddNode(claim)
	g.AddNode(special)
	g.AddNode(unrelated)
	g.AddEdge(extendsEdge(claim.ID, base.ID))
	g.AddEdge(extendsEdge(special.ID, claim.ID))

	require.Equal(t, 2, PropagateEntryPointsDownHierarchy(g))

	for _, n := range []*graph.Node{claim, special} {
		got := g.GetNode(n.ID)
		require.NotNil(t, got)
		assert.Equal(t, true, got.Meta[MetaEntryPoint], "%s must be stamped", n.ID)
		assert.Equal(t, "aspnet:controller", got.Meta[MetaEntryKind])
	}
	assert.Nil(t, g.GetNode(unrelated.ID).Meta[MetaEntryPoint], "non-derived type must stay unstamped")

	// Idempotent: a second sweep restamps nothing.
	require.Equal(t, 0, PropagateEntryPointsDownHierarchy(g))
}

// TestPropagateEntryPointsDownHierarchy_Gates: only same-language
// KindType descendants along resolved extends edges qualify; a
// descendant already stamped with a DIFFERENT kind (a derived test
// fixture) is left alone; a non-controller stamp does not seed.
func TestPropagateEntryPointsDownHierarchy_Gates(t *testing.T) {
	g := graph.New()
	base := hierType("Base/BaseController.cs::BaseController", "BaseController", "csharp", controllerMeta())
	g.AddNode(base)

	// Wrong-language child of the stamped base (contrived; the gate is real).
	tsChild := hierType("web/Ctl.ts::Ctl", "Ctl", "typescript", nil)
	g.AddNode(tsChild)
	g.AddEdge(extendsEdge(tsChild.ID, base.ID))

	// Child already stamped as a test must NOT be overwritten to controller.
	fixture := hierType("T/ControllerFixture.cs::ControllerFixture", "ControllerFixture", "csharp",
		map[string]any{MetaEntryPoint: true, MetaEntryKind: "xunit:test"})
	g.AddNode(fixture)
	g.AddEdge(extendsEdge(fixture.ID, base.ID))

	// A non-controller entry kind does not seed a walk.
	testSeed := hierType("T/BaseFixture.cs::BaseFixture", "BaseFixture", "csharp",
		map[string]any{MetaEntryPoint: true, MetaEntryKind: "xunit:test"})
	derivedFixture := hierType("T/DerivedFixture.cs::DerivedFixture", "DerivedFixture", "csharp", nil)
	g.AddNode(testSeed)
	g.AddNode(derivedFixture)
	g.AddEdge(extendsEdge(derivedFixture.ID, testSeed.ID))

	// Still-unresolved base target: nothing to walk.
	orphan := hierType("O/OrphanController.cs::OrphanController", "OrphanController", "csharp", nil)
	g.AddNode(orphan)
	g.AddEdge(extendsEdge(orphan.ID, "unresolved::MissingBase"))

	require.Equal(t, 0, PropagateEntryPointsDownHierarchy(g))
	assert.Nil(t, g.GetNode(tsChild.ID).Meta[MetaEntryPoint])
	assert.Equal(t, "xunit:test", g.GetNode(fixture.ID).Meta[MetaEntryKind], "existing stamp must survive")
	assert.Nil(t, g.GetNode(derivedFixture.ID).Meta[MetaEntryPoint])
	assert.Nil(t, g.GetNode(orphan.ID).Meta[MetaEntryPoint])
}

// noCSharpHierarchyStore reports "no csharp indexed" the way the on-disk
// backend can; the wrapped graph holds a stampable hierarchy, so any
// stamping proves the language gate was ignored.
type noCSharpHierarchyStore struct{ graph.Store }

func (noCSharpHierarchyStore) HasLanguage(string) bool { return false }

func TestPropagateEntryPointsDownHierarchy_LanguageGate(t *testing.T) {
	g := graph.New()
	base := hierType("B.cs::BaseController", "BaseController", "csharp", controllerMeta())
	derived := hierType("D.cs::DerivedController", "DerivedController", "csharp", nil)
	g.AddNode(base)
	g.AddNode(derived)
	g.AddEdge(extendsEdge(derived.ID, base.ID))

	require.Equal(t, 0, PropagateEntryPointsDownHierarchy(noCSharpHierarchyStore{g}))
	assert.Nil(t, g.GetNode(derived.ID).Meta[MetaEntryPoint], "gated-off pass must not stamp")
}

// TestPropagateEntryPointsDownHierarchyScoped_SeedInFrontier: an edit
// that (re-)stamps a base controller carries it in the changed-type
// frontier; its whole existing subtree must be stamped from that seed.
func TestPropagateEntryPointsDownHierarchyScoped_SeedInFrontier(t *testing.T) {
	g := graph.New()
	base := hierType("B.cs::BaseController", "BaseController", "csharp", controllerMeta())
	derived := hierType("D.cs::DerivedController", "DerivedController", "csharp", nil)
	g.AddNode(base)
	g.AddNode(derived)
	g.AddEdge(extendsEdge(derived.ID, base.ID))

	require.Equal(t, 1, PropagateEntryPointsDownHierarchyScoped(g, []string{base.ID}))
	assert.Equal(t, "aspnet:controller", g.GetNode(derived.ID).Meta[MetaEntryKind])
}

// TestPropagateEntryPointsDownHierarchyScoped_DerivedFindsAncestor: a
// save that adds a NEW derived type carries only that type in the
// frontier; the stamped base lives in an unchanged file (possibly an
// unchanged repo). The upward walk must find the stamped ancestor —
// through an unstamped intermediate — and stamp the whole path down.
func TestPropagateEntryPointsDownHierarchyScoped_DerivedFindsAncestor(t *testing.T) {
	g := graph.New()
	base := hierType("B.cs::BaseController", "BaseController", "csharp", controllerMeta())
	mid := hierType("M.cs::MidController", "MidController", "csharp", nil)
	fresh := hierType("F.cs::FreshController", "FreshController", "csharp", nil)
	g.AddNode(base)
	g.AddNode(mid)
	g.AddNode(fresh)
	g.AddEdge(extendsEdge(mid.ID, base.ID))
	g.AddEdge(extendsEdge(fresh.ID, mid.ID))

	require.Equal(t, 2, PropagateEntryPointsDownHierarchyScoped(g, []string{fresh.ID}))
	assert.Equal(t, "aspnet:controller", g.GetNode(fresh.ID).Meta[MetaEntryKind])
	assert.Equal(t, "aspnet:controller", g.GetNode(mid.ID).Meta[MetaEntryKind],
		"the unstamped intermediate on the path must be stamped too")
}

// TestPropagateEntryPointsDownHierarchyScoped_NoOps: empty frontiers and
// frontiers without a registered kind's language cost nothing and stamp
// nothing.
func TestPropagateEntryPointsDownHierarchyScoped_NoOps(t *testing.T) {
	g := graph.New()
	goType := hierType("m.go::Handler", "Handler", "go", nil)
	g.AddNode(goType)

	assert.Equal(t, 0, PropagateEntryPointsDownHierarchyScoped(g, nil))
	assert.Equal(t, 0, PropagateEntryPointsDownHierarchyScoped(g, []string{goType.ID}))
	assert.Equal(t, 0, PropagateEntryPointsDownHierarchyScoped(g, []string{"missing::ID"}))
}
