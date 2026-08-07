package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// Renaming a Godot signal handler must rewrite the wiring too.
//
// Godot connects a signal by passing the method itself, without
// parentheses — `pressed.connect(_on_random)` — or by naming it in a
// string, either in `Callable(self, "…")` or in the scene's own
// `[connection]` block. None of those is a call, so a rename that only
// followed call edges rewrote the definition and the ordinary call sites
// and left the wiring pointing at a name that no longer exists. The
// project still parses, nothing raises an error anywhere, and the button
// simply stops working — the most expensive failure shape in the report,
// because it is silent.
func TestRenameSymbol_GDScriptSignalWiring(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"Panel.gd": `class_name Panel
extends Control

func _ready() -> void:
    button.pressed.connect(_on_random)
    timer.timeout.connect(Callable(self, "_on_random"))
    _on_random()

func _on_random() -> void:
    pass
`,
		"Panel.tscn": `[gd_scene load_steps=2 format=3]

[ext_resource type="Script" path="res://Panel.gd" id="1_abc"]

[node name="Panel" type="Control"]
script = ExtResource("1_abc")

[connection signal="pressed" from="Panel" to="." method="_on_random"]
`,
	}
	for name, src := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644))
	}

	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
	srv.RunAnalysis()

	res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
		"id":       "Panel.gd::_on_random",
		"new_name": "_on_shuffle",
	})
	resp := decodeRenameResp(t, res)
	assert.Equal(t, "applied", resp["status"])

	script, err := os.ReadFile(filepath.Join(dir, "Panel.gd"))
	require.NoError(t, err)
	assert.Contains(t, string(script), "func _on_shuffle() -> void:", "the definition")
	assert.Contains(t, string(script), "connect(_on_shuffle)", "the bare method value")
	assert.Contains(t, string(script), `Callable(self, "_on_shuffle")`, "the Callable string")
	assert.NotContains(t, string(script), "_on_random",
		"a surviving mention is a signal wired to a method that no longer exists")

	scene, err := os.ReadFile(filepath.Join(dir, "Panel.tscn"))
	require.NoError(t, err)
	assert.Contains(t, string(scene), `method="_on_shuffle"`,
		"the scene's own connection block wires the handler too")
}
