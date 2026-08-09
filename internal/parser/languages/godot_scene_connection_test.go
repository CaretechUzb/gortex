package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// A scene's `[connection]` block names a handler as a bare string. It is
// the third place a signal wiring lives — alongside the bare method value
// and the `Callable(self, "…")` string in the script itself — and the one
// no script mentions at all.
func TestGodotSceneExtractor_ConnectionReferencesHandler(t *testing.T) {
	res, err := NewGodotSceneExtractor().Extract("ui/Panel.tscn", []byte(
		`[gd_scene load_steps=2 format=3]

[ext_resource type="Script" path="res://ui/Panel.gd" id="1_abc"]

[node name="Panel" type="Control"]
script = ExtResource("1_abc")

[node name="Button" type="Button" parent="."]

[node name="Inner" type="Control" parent="Panel/Box"]

[connection signal="pressed" from="Button" to="." method="_on_pressed"]

[connection signal="hovered" from="Button" to="Panel/Box/Inner" method="_on_hover"]

[connection signal="broken" from="Button" to="."]
`))
	require.NoError(t, err)

	byMethod := map[string]*graph.Edge{}
	for _, e := range res.Edges {
		if e.Kind != graph.EdgeReferences || e.Meta == nil {
			continue
		}
		if conn, _ := e.Meta["godot_connection"].(bool); conn {
			byMethod[e.Meta["godot_method"].(string)] = e
		}
	}

	require.Contains(t, byMethod, "_on_pressed")
	assert.Equal(t, "ui/Panel.tscn::Panel", byMethod["_on_pressed"].From,
		`to="." is the scene root, which is where the script usually hangs`)
	assert.Equal(t, "unresolved::*._on_pressed", byMethod["_on_pressed"].To)
	assert.Equal(t, "pressed", byMethod["_on_pressed"].Meta["godot_signal"])

	require.Contains(t, byMethod, "_on_hover")
	assert.Equal(t, "ui/Panel.tscn::Panel/Panel/Box/Inner", byMethod["_on_hover"].From,
		"a deeper NodePath is relative to the root, exactly as node IDs are built")

	assert.Len(t, byMethod, 2, "a connection with no method names no handler")
}
