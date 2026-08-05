package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zzet/gortex/internal/graph"
)

// godotRef seeds an unbound `res://` reference of the shape the scene
// extractor (EdgeReferences) and GDScript preload (EdgeImports) emit.
func godotRef(g graph.Store, from, resPath string, kind graph.EdgeKind) *graph.Edge {
	e := &graph.Edge{From: from, To: "unresolved::import::" + resPath, Kind: kind, FilePath: from}
	g.AddEdge(e)
	return e
}

// TestResolveGodotResPaths_SceneToScript is the edge the issue is about:
// a scene's ext_resource must land on the script file it mounts, so the
// script's usage set includes the scene.
func TestResolveGodotResPaths_SceneToScript(t *testing.T) {
	g := graph.New()
	seedFile(g, "scenes/Main.tscn", "godot_resource")
	seedFile(g, "src/ui/Main.gd", "gdscript")
	e := godotRef(g, "scenes/Main.tscn", "res://src/ui/Main.gd", graph.EdgeReferences)

	New(g).resolveGodotResPaths()

	assert.Equal(t, "src/ui/Main.gd", e.To)
	assert.Equal(t, graph.OriginASTResolved, e.Origin)
}

// TestResolveGodotResPaths_Preload covers the other producer: a GDScript
// `preload("res://...")` parks on the same placeholder and never resolved
// before this pass existed.
func TestResolveGodotResPaths_Preload(t *testing.T) {
	g := graph.New()
	seedFile(g, "src/core/Setup.gd", "gdscript")
	seedFile(g, "src/data/Techs.gd", "gdscript")
	e := godotRef(g, "src/core/Setup.gd", "res://src/data/Techs.gd", graph.EdgeImports)

	New(g).resolveGodotResPaths()

	assert.Equal(t, "src/data/Techs.gd", e.To)
}

// TestResolveGodotResPaths_ProjectRootBelowRepoRoot pins the reason the
// pass matches by suffix: `res://` is rooted at the Godot project, which
// need not be the repository root.
func TestResolveGodotResPaths_ProjectRootBelowRepoRoot(t *testing.T) {
	g := graph.New()
	seedFile(g, "game/client/scenes/Main.tscn", "godot_resource")
	seedFile(g, "game/client/src/ui/Main.gd", "gdscript")
	e := godotRef(g, "game/client/scenes/Main.tscn", "res://src/ui/Main.gd", graph.EdgeReferences)

	New(g).resolveGodotResPaths()

	assert.Equal(t, "game/client/src/ui/Main.gd", e.To)
}

// TestResolveGodotResPaths_AmbiguousRefuses pins the refusal: two Godot
// projects in one workspace can hold the same relative path, and guessing
// between them would manufacture a confidently wrong edge.
func TestResolveGodotResPaths_AmbiguousRefuses(t *testing.T) {
	g := graph.New()
	seedFile(g, "clientA/scenes/Main.tscn", "godot_resource")
	seedFile(g, "clientA/src/ui/Main.gd", "gdscript")
	seedFile(g, "clientB/src/ui/Main.gd", "gdscript")
	e := godotRef(g, "clientA/scenes/Main.tscn", "res://src/ui/Main.gd", graph.EdgeReferences)

	New(g).resolveGodotResPaths()

	assert.Equal(t, "unresolved::import::res://src/ui/Main.gd", e.To,
		"an ambiguous res:// path must stay unresolved rather than guess")
}

// TestResolveGodotResPaths_UnindexedTargetStaysUnresolved covers an
// asset the corpus filters never admitted: the reference is real but has
// no node to land on.
func TestResolveGodotResPaths_UnindexedTargetStaysUnresolved(t *testing.T) {
	g := graph.New()
	seedFile(g, "scenes/Main.tscn", "godot_resource")
	e := godotRef(g, "scenes/Main.tscn", "res://assets/huge.blend", graph.EdgeReferences)

	New(g).resolveGodotResPaths()

	assert.Equal(t, "unresolved::import::res://assets/huge.blend", e.To)
}

// TestResolveGodotResPaths_LeavesNonGodotImports guards the blast radius:
// the pass must not touch the `unresolved::import::` placeholders other
// languages own.
func TestResolveGodotResPaths_LeavesNonGodotImports(t *testing.T) {
	g := graph.New()
	seedFile(g, "scenes/Main.tscn", "godot_resource")
	seedFile(g, "src/ui/Main.gd", "gdscript")
	e := godotRef(g, "scenes/Main.tscn", "src/ui/Main.gd", graph.EdgeImports)

	New(g).resolveGodotResPaths()

	assert.Equal(t, "unresolved::import::src/ui/Main.gd", e.To,
		"only the res:// scheme belongs to this pass")
}
