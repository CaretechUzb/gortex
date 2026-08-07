package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// gdClassScript declares a `class_name` script: the file, its class node
// (with an optional base), and the methods that class owns.
func gdClassScript(g *graph.Graph, path, class, extends string, methods ...string) {
	g.AddNode(&graph.Node{
		ID: path, Kind: graph.KindFile, Name: path,
		FilePath: path, Language: "gdscript",
	})
	meta := map[string]any{"gd_kind": "class_name"}
	if extends != "" {
		meta["gd_extends"] = extends
	}
	g.AddNode(&graph.Node{
		ID: path + "::" + class, Kind: graph.KindType, Name: class,
		FilePath: path, Language: "gdscript", Meta: meta,
	})
	for _, m := range methods {
		g.AddNode(&graph.Node{
			ID: path + "::" + m, Kind: graph.KindMethod, Name: m,
			FilePath: path, Language: "gdscript",
			Meta: map[string]any{"receiver": class},
		})
	}
}

// gdBoundCall is a call already resolved onto a target by a weak tier,
// carrying the receiver its source stated.
func gdBoundCall(g *graph.Graph, from, file, to, recv string) *graph.Edge {
	gdBoundCallLine++
	e := &graph.Edge{
		From: from, To: to, Kind: graph.EdgeCalls, FilePath: file,
		// Distinct lines: two call sites in one func onto one target
		// differ only by line, and the store keys edges by identity.
		Line:   gdBoundCallLine,
		Origin: graph.OriginASTInferred, Confidence: 0.7,
		Meta: map[string]any{"receiver_type": recv},
	}
	g.AddEdge(e)
	return e
}

var gdBoundCallLine int

func gdDemoted(e *graph.Edge) bool {
	reason, _ := e.Meta["demoted"].(string)
	return reason == "receiver_type_mismatch"
}

// A call states its receiver. When the bind contradicts it, the caller
// list of the target is wrong in the direction that matters: the count
// looks reassuring while pointing at code the change cannot reach.
func TestDemoteGDScriptReceiverMismatches_UnrelatedClass(t *testing.T) {
	g := graph.New()
	gdClassScript(g, "src/ui/Carry.gd", "Carry", "RefCounted", "apply")
	gdClassScript(g, "src/data/Items.gd", "Items", "RefCounted", "give")
	gdClassScript(g, "src/ui/Caller.gd", "Caller", "Control", "run")
	mismatch := gdBoundCall(g, "src/ui/Caller.gd::run", "src/ui/Caller.gd",
		"src/ui/Carry.gd::apply", "Items")
	right := gdBoundCall(g, "src/ui/Caller.gd::run", "src/ui/Caller.gd",
		"src/ui/Carry.gd::apply", "Carry")

	require.Equal(t, 1, DemoteGDScriptReceiverMismatches(g))
	assert.True(t, gdDemoted(mismatch))
	assert.Equal(t, graph.OriginSpeculative, mismatch.Origin,
		"the evidence stays inspectable rather than being deleted")
	assert.False(t, gdDemoted(right))
}

// An engine singleton (`Input`, `OS`) names no in-repo class, so no
// in-repo method can be its member.
func TestDemoteGDScriptReceiverMismatches_UnknownReceiver(t *testing.T) {
	g := graph.New()
	gdClassScript(g, "src/ui/Carry.gd", "Carry", "RefCounted", "apply")
	gdClassScript(g, "src/ui/Caller.gd", "Caller", "Control", "run")
	e := gdBoundCall(g, "src/ui/Caller.gd::run", "src/ui/Caller.gd",
		"src/ui/Carry.gd::apply", "Input")

	require.Equal(t, 1, DemoteGDScriptReceiverMismatches(g))
	assert.True(t, gdDemoted(e))
}

// A receiver the calling file declares itself is a local alias whose
// target the graph does not track — a mismatch is not provable.
func TestDemoteGDScriptReceiverMismatches_LocalAliasIsNotProvable(t *testing.T) {
	g := graph.New()
	gdClassScript(g, "src/ui/Carry.gd", "Carry", "RefCounted", "apply")
	gdClassScript(g, "src/ui/Caller.gd", "Caller", "Control", "run")
	g.AddNode(&graph.Node{
		ID: "src/ui/Caller.gd::Helper", Kind: graph.KindVariable, Name: "Helper",
		FilePath: "src/ui/Caller.gd", Language: "gdscript",
		Meta: map[string]any{"const": true},
	})
	e := gdBoundCall(g, "src/ui/Caller.gd::run", "src/ui/Caller.gd",
		"src/ui/Carry.gd::apply", "Helper")

	assert.Zero(t, DemoteGDScriptReceiverMismatches(g))
	assert.False(t, gdDemoted(e))
}

// Calling an inherited method is dispatch, not a phantom. Over-demoting
// here would trade one wrong answer for another.
func TestDemoteGDScriptReceiverMismatches_InheritedDispatchSurvives(t *testing.T) {
	g := graph.New()
	gdClassScript(g, "src/base/Base.gd", "Base", "Node", "shared")
	gdClassScript(g, "src/base/Mid.gd", "Mid", "Base")
	gdClassScript(g, "src/base/Derived.gd", "Derived", "Mid")
	gdClassScript(g, "src/ui/Caller.gd", "Caller", "Control", "run")
	e := gdBoundCall(g, "src/ui/Caller.gd::run", "src/ui/Caller.gd",
		"src/base/Base.gd::shared", "Derived")

	assert.Zero(t, DemoteGDScriptReceiverMismatches(g))
	assert.False(t, gdDemoted(e))
}

// A base the extractor could not read as one in-repo class name leaves
// the ancestry unknown, so no mismatch against it is provable.
func TestDemoteGDScriptReceiverMismatches_OpaqueBaseDeclines(t *testing.T) {
	g := graph.New()
	gdClassScript(g, "src/base/Base.gd", "Base", "Node", "shared")
	gdClassScript(g, "src/ui/Caller.gd", "Caller", "Control", "run")
	g.AddNode(&graph.Node{
		ID: "src/ui/Odd.gd", Kind: graph.KindFile, Name: "src/ui/Odd.gd",
		FilePath: "src/ui/Odd.gd", Language: "gdscript",
	})
	g.AddNode(&graph.Node{
		ID: "src/ui/Odd.gd::Odd", Kind: graph.KindType, Name: "Odd",
		FilePath: "src/ui/Odd.gd", Language: "gdscript",
		Meta: map[string]any{"gd_kind": "class_name", "gd_extends_opaque": true},
	})
	e := gdBoundCall(g, "src/ui/Caller.gd::run", "src/ui/Caller.gd",
		"src/base/Base.gd::shared", "Odd")

	assert.Zero(t, DemoteGDScriptReceiverMismatches(g))
	assert.False(t, gdDemoted(e))
}

// The autoload and preload-alias binders name a script directly; the gate
// must recognise their work rather than judge it against a class name.
func TestDemoteGDScriptReceiverMismatches_AutoloadReceiverKeeps(t *testing.T) {
	g := graph.New()
	gdAutoloadNode(g, "Game", "src/core/Game.gd")
	gdScriptFile(g, "src/core/Game.gd", "set_speed")
	gdClassScript(g, "src/ui/Caller.gd", "Caller", "Control", "run")
	keep := gdBoundCall(g, "src/ui/Caller.gd::run", "src/ui/Caller.gd",
		"src/core/Game.gd::set_speed", "Game")

	assert.Zero(t, DemoteGDScriptReceiverMismatches(g))
	assert.False(t, gdDemoted(keep))
}

func TestDemoteGDScriptReceiverMismatches_AutoloadReceiverOnWrongScript(t *testing.T) {
	g := graph.New()
	gdAutoloadNode(g, "Game", "src/core/Game.gd")
	gdScriptFile(g, "src/core/Game.gd", "set_speed")
	gdClassScript(g, "src/ui/Other.gd", "Other", "Node", "set_speed")
	gdClassScript(g, "src/ui/Caller.gd", "Caller", "Control", "run")
	e := gdBoundCall(g, "src/ui/Caller.gd::run", "src/ui/Caller.gd",
		"src/ui/Other.gd::set_speed", "Game")

	require.Equal(t, 1, DemoteGDScriptReceiverMismatches(g))
	assert.True(t, gdDemoted(e))
}

// A same-file bind needs no receiver evidence — the definition is right
// there, and a `self` receiver on a script with no class_name lands on it.
func TestDemoteGDScriptReceiverMismatches_SameFileKeeps(t *testing.T) {
	g := graph.New()
	gdClassScript(g, "src/ui/Panel.gd", "Panel", "Control", "run", "tick")
	e := gdBoundCall(g, "src/ui/Panel.gd::run", "src/ui/Panel.gd",
		"src/ui/Panel.gd::tick", "Something")

	assert.Zero(t, DemoteGDScriptReceiverMismatches(g))
	assert.False(t, gdDemoted(e))
}

func TestDemoteGDScriptReceiverMismatches_NoGDScriptIsANoOp(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", Language: "go"})
	assert.Zero(t, DemoteGDScriptReceiverMismatches(g))
}
