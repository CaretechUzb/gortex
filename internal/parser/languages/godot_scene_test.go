package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

const godotScene = `[gd_scene load_steps=4 format=3 uid="uid://bqxyz"]

[ext_resource type="Script" path="res://src/ui/Main.gd" id="1_main"]
[ext_resource type="PackedScene" uid="uid://c123" path="res://src/ui/Panel.tscn" id="2_panel"]
[ext_resource type="Texture2D" path="res://assets/icon.png" id="3_icon"]

[sub_resource type="StyleBoxFlat" id="StyleBoxFlat_abc"]
bg_color = Color(0, 0, 0, 1)

[node name="Main" type="Control"]
script = ExtResource("1_main")

[node name="Panel" parent="." instance=ExtResource("2_panel")]

[node name="Icon" type="TextureRect" parent="Panel"]
texture = ExtResource("3_icon")
`

// godotRefTargets collects the res:// placeholders each owner references.
func godotRefTargets(edges []*graph.Edge, owner string) map[string]bool {
	out := map[string]bool{}
	for _, e := range edges {
		if e.Kind == graph.EdgeReferences && e.From == owner {
			out[e.To] = true
		}
	}
	return out
}

// TestGodotSceneExtractor_ExtResourceEdges is the issue this extractor
// exists for: a scene must reference the script it mounts, so a script's
// blast radius includes the scenes that instantiate it.
func TestGodotSceneExtractor_ExtResourceEdges(t *testing.T) {
	res, err := NewGodotSceneExtractor().Extract("game/scenes/Main.tscn", []byte(godotScene))
	require.NoError(t, err)

	fileRefs := godotRefTargets(res.Edges, "game/scenes/Main.tscn")
	for _, want := range []string{
		"unresolved::import::res://src/ui/Main.gd",
		"unresolved::import::res://src/ui/Panel.tscn",
		"unresolved::import::res://assets/icon.png",
	} {
		assert.True(t, fileRefs[want], "missing scene reference %q (have %v)", want, fileRefs)
	}

	// The declared resource type rides the edge so a consumer can tell a
	// script from an asset without re-reading the scene.
	var scriptEdge *graph.Edge
	for _, e := range res.Edges {
		if e.To == "unresolved::import::res://src/ui/Main.gd" && e.From == "game/scenes/Main.tscn" {
			scriptEdge = e
		}
	}
	require.NotNil(t, scriptEdge)
	assert.Equal(t, "Script", scriptEdge.Meta["godot_ext_resource"])
	assert.Equal(t, 3, scriptEdge.Line)
}

// TestGodotSceneExtractor_SceneTree pins the node symbols and the
// per-node ExtResource attribution: a `script =` / `instance=` assignment
// binds to the scene node that carries it, not just to the file.
func TestGodotSceneExtractor_SceneTree(t *testing.T) {
	res, err := NewGodotSceneExtractor().Extract("game/scenes/Main.tscn", []byte(godotScene))
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byID[n.ID] = n
	}
	root := byID["game/scenes/Main.tscn::Main"]
	require.NotNil(t, root, "scene root node missing (have %v)", byID)
	assert.Equal(t, graph.KindField, root.Kind)
	assert.Equal(t, "Control", root.Meta["godot_type"])

	// parent="." is a child of the root; parent="Panel" is one level deeper.
	assert.NotNil(t, byID["game/scenes/Main.tscn::Main/Panel"])
	assert.NotNil(t, byID["game/scenes/Main.tscn::Main/Panel/Icon"])

	assert.True(t, godotRefTargets(res.Edges, "game/scenes/Main.tscn::Main")["unresolved::import::res://src/ui/Main.gd"],
		"root node must reference its script")
	assert.True(t, godotRefTargets(res.Edges, "game/scenes/Main.tscn::Main/Panel")["unresolved::import::res://src/ui/Panel.tscn"],
		"instance=ExtResource on the header must attribute to that node")
	assert.True(t, godotRefTargets(res.Edges, "game/scenes/Main.tscn::Main/Panel/Icon")["unresolved::import::res://assets/icon.png"],
		"a property assignment must attribute to the enclosing node")
}

// TestGodotSceneExtractor_Godot3IntegerIDs covers the Godot 3 dialect,
// where a resource id is an unquoted integer and ExtResource takes it
// bare.
func TestGodotSceneExtractor_Godot3IntegerIDs(t *testing.T) {
	const scene = `[gd_scene load_steps=2 format=2]

[ext_resource path="res://src/Player.gd" type="Script" id=1]

[node name="Player" type="KinematicBody2D"]
script = ExtResource( 1 )
`
	res, err := NewGodotSceneExtractor().Extract("Player.tscn", []byte(scene))
	require.NoError(t, err)

	assert.True(t, godotRefTargets(res.Edges, "Player.tscn")["unresolved::import::res://src/Player.gd"],
		"the file must reference the script")
	assert.True(t, godotRefTargets(res.Edges, "Player.tscn::Player")["unresolved::import::res://src/Player.gd"],
		"a bare integer ExtResource( 1 ) must attribute to the node")
}

// The manifest belongs to GodotProjectExtractor, which claims it by
// exact basename and owns `[autoload]`. Registering both is only safe
// because this extractor never claims `.godot` — assert the boundary so
// a future extension claim cannot silently produce competing edges.
func TestGodotSceneExtractor_DoesNotClaimTheProjectManifest(t *testing.T) {
	reg := parser.NewRegistry()
	RegisterAll(reg)

	lang, ok := reg.DetectLanguage("project.godot")
	require.True(t, ok)
	assert.Equal(t, "godot_project", lang, "the manifest is GodotProjectExtractor's")

	// The engine's cache directory must stay unparsed by either extractor.
	_, claimed := reg.DetectLanguage(".godot/uid_cache.bin")
	assert.False(t, claimed, "the .godot extension must stay unclaimed")

	for _, path := range []string{"scenes/Main.tscn", "themes/Main.tres"} {
		lang, ok := reg.DetectLanguage(path)
		require.True(t, ok, "%s must be detected", path)
		assert.Equal(t, "godot_resource", lang, "path %s", path)
	}
}

// TestGodotSceneExtractor_TresResource covers a standalone .tres, which
// shares the ext_resource grammar but has no scene tree.
func TestGodotSceneExtractor_TresResource(t *testing.T) {
	const tres = `[gd_resource type="Theme" load_steps=2 format=3]

[ext_resource type="FontFile" path="res://assets/font.ttf" id="1_font"]

[resource]
default_font = ExtResource("1_font")
`
	res, err := NewGodotSceneExtractor().Extract("themes/Main.tres", []byte(tres))
	require.NoError(t, err)

	assert.True(t, godotRefTargets(res.Edges, "themes/Main.tres")["unresolved::import::res://assets/font.ttf"])
	// No [node] sections — the file node is the only symbol.
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
}

// TestGodotSceneExtractor_IgnoresNonResPaths guards against minting a
// placeholder for a path that names no project file.
func TestGodotSceneExtractor_IgnoresNonResPaths(t *testing.T) {
	const scene = `[gd_scene format=3]

[ext_resource type="Script" path="user://cache/tmp.gd" id="1_x"]
`
	res, err := NewGodotSceneExtractor().Extract("Odd.tscn", []byte(scene))
	require.NoError(t, err)
	assert.Empty(t, godotRefTargets(res.Edges, "Odd.tscn"))
}
