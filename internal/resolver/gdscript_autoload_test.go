package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func gdAutoloadNode(g *graph.Graph, name, script string) {
	g.AddNode(&graph.Node{
		ID: "project.godot::" + name, Kind: graph.KindType, Name: name,
		FilePath: "project.godot", Language: "godot_project",
		Meta: map[string]any{"gd_kind": "autoload", "gd_script": script},
	})
}

func gdScriptFile(g *graph.Graph, path string, methods ...string) {
	g.AddNode(&graph.Node{
		ID: path, Kind: graph.KindFile, Name: path,
		FilePath: path, Language: "gdscript",
	})
	for _, m := range methods {
		g.AddNode(&graph.Node{
			ID: path + "::" + m, Kind: graph.KindMethod, Name: m,
			FilePath: path, Language: "gdscript",
		})
	}
}

func gdRecvCall(g *graph.Graph, from, file, method, recv string) {
	g.AddEdge(&graph.Edge{
		From: from, To: "unresolved::*." + method, Kind: graph.EdgeCalls, FilePath: file,
		Meta: map[string]any{"receiver_type": recv},
	})
}

func gdCallEdge(g graph.Store, from, to string) *graph.Edge {
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e != nil && e.From == from && e.To == to {
			return e
		}
	}
	return nil
}

func gdAutoloadGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New()
	gdAutoloadNode(g, "Game", "src/core/Game.gd")
	gdScriptFile(g, "src/core/Game.gd", "set_speed", "pause")
	gdScriptFile(g, "src/ui/Main.gd", "_ready")
	return g
}

// `project.godot` is the only place that says the global name `Game`
// means `src/core/Game.gd` — no import, preload or `class_name` in any
// script carries that mapping.
func TestResolveGDScriptAutoloads_BindsSingletonCall(t *testing.T) {
	g := gdAutoloadGraph(t)
	const caller = "src/ui/Main.gd::_ready"
	gdRecvCall(g, caller, "src/ui/Main.gd", "set_speed", "Game")

	require.Equal(t, 1, ResolveGDScriptAutoloads(g))

	e := gdCallEdge(g, caller, "src/core/Game.gd::set_speed")
	require.NotNil(t, e, "the call binds across directories")
	assert.Equal(t, graph.OriginASTResolved, e.Origin)
	assert.Equal(t, SynthGodotAutoload, e.Meta[MetaSynthesizedBy])
}

func TestResolveGDScriptAutoloads_UnknownMemberLeftAlone(t *testing.T) {
	g := gdAutoloadGraph(t)
	gdRecvCall(g, "src/ui/Main.gd::_ready", "src/ui/Main.gd", "no_such_method", "Game")
	assert.Equal(t, 0, ResolveGDScriptAutoloads(g),
		"a name the autoload script does not declare must not bind to it")
}

func TestResolveGDScriptAutoloads_NonAutoloadReceiverLeftAlone(t *testing.T) {
	g := gdAutoloadGraph(t)
	gdRecvCall(g, "src/ui/Main.gd::_ready", "src/ui/Main.gd", "set_speed", "Input")
	assert.Equal(t, 0, ResolveGDScriptAutoloads(g))
}

func TestResolveGDScriptAutoloads_ResolvedEdgeNotTouched(t *testing.T) {
	g := gdAutoloadGraph(t)
	const caller = "src/ui/Main.gd::_ready"
	g.AddEdge(&graph.Edge{
		From: caller, To: "src/core/Game.gd::pause", Kind: graph.EdgeCalls,
		FilePath: "src/ui/Main.gd", Origin: graph.OriginLSPResolved,
		Meta: map[string]any{"receiver_type": "Game"},
	})
	assert.Equal(t, 0, ResolveGDScriptAutoloads(g))
	assert.Equal(t, graph.OriginLSPResolved, gdCallEdge(g, caller, "src/core/Game.gd::pause").Origin)
}

// A singleton whose script two indexed files could satisfy is dropped:
// a wrong cross-directory binding is worse than an unresolved call.
func TestResolveGDScriptAutoloads_AmbiguousScriptDropped(t *testing.T) {
	g := gdAutoloadGraph(t)
	gdScriptFile(g, "addons/vendor/src/core/Game.gd", "set_speed")
	gdRecvCall(g, "src/ui/Main.gd::_ready", "src/ui/Main.gd", "set_speed", "Game")
	assert.Equal(t, 0, ResolveGDScriptAutoloads(g))
}

func TestResolveGDScriptAutoloads_NoManifest(t *testing.T) {
	g := graph.New()
	gdScriptFile(g, "src/core/Game.gd", "set_speed")
	gdRecvCall(g, "src/ui/Main.gd::_ready", "src/ui/Main.gd", "set_speed", "Game")
	assert.Equal(t, 0, ResolveGDScriptAutoloads(g))
	assert.Equal(t, 0, ResolveGDScriptAutoloads(nil))
}

func TestGDPathMatches(t *testing.T) {
	assert.True(t, gdPathMatches("src/core/Game.gd", "src/core/Game.gd"))
	assert.True(t, gdPathMatches("game/src/core/Game.gd", "src/core/Game.gd"))
	assert.True(t, gdPathMatches(`game\src\core\Game.gd`, "src/core/Game.gd"))
	assert.False(t, gdPathMatches("src/core/NotGame.gd", "src/core/Game.gd"),
		"the suffix must land on a path-separator boundary")
	assert.False(t, gdPathMatches("src/core/Game.gd", "other/Game.gd"))
}
