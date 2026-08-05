package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// A GDScript call's receiver decides its target. This indexes a Godot
// project shaped like the reported one — a `class_name` global called
// across directories, a same-named method sitting next door to the
// caller, and an autoload singleton — and asserts the whole pipeline
// binds each call to the definition its receiver names.
//
// Every case here failed before receivers were extracted: the call
// degraded to a bare name, so the resolver's locality fallback answered
// `Notify.event()` with the unrelated `BannerPath.event` in the
// caller's own directory, while the cross-directory calls were reverted
// by the cross-package guard for want of an import edge GDScript has no
// syntax to express.
func gdIndexProject(t *testing.T) *graph.Graph {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"project.godot": `config_version=5

[application]

config/name="Latticefall"

[autoload]

Game="*res://src/core/Game.gd"
`,
		// The real target: a global class in a directory of its own.
		"src/systems/Notify.gd": `class_name Notify
extends Node

func event(msg: String) -> void:
    print(msg)
`,
		// A same-named method next door to the caller — the phantom.
		"src/ui/BannerPath.gd": `class_name BannerPath
extends Node2D

func event(kind: int) -> void:
    pass
`,
		"src/ui/Main.gd": `class_name Main
extends Control

func _on_night_rest_toggle() -> void:
    Notify.event("ui_siege.05")
`,
		"src/data/Techs.gd": `class_name Techs
extends RefCounted

func get_tech(id: String):
    return id
`,
		// A cross-directory caller with no preload and no import — the
		// language has no syntax for either.
		"src/systems/Research.gd": `class_name ResearchSystem
extends Node

func run() -> void:
    Techs.get_tech("smithing")
`,
		// An autoload target: reachable project-wide as `Game` purely
		// because project.godot says so. No class_name.
		"src/core/Game.gd": `extends Node

func set_speed(v: float) -> void:
    pass
`,
		// In a directory of its own, so the call to the autoload cannot
		// ride same-package locality to its target.
		"src/boot/Setup.gd": `class_name Setup
extends Node

func boot() -> void:
    Game.set_speed(1.0)
`,
	}
	for name, src := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(src), 0o644))
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()
	return g
}

// gdCallTargets returns the resolved call targets of a caller symbol.
func gdCallTargets(g *graph.Graph, from string) []string {
	var out []string
	for _, e := range g.GetOutEdges(from) {
		if e.Kind == graph.EdgeCalls && !graph.IsUnresolvedTarget(e.To) {
			out = append(out, e.To)
		}
	}
	return out
}

func TestGDScriptReceiver_EndToEnd(t *testing.T) {
	g := gdIndexProject(t)

	t.Run("receiver picks the global class over the same-named neighbour", func(t *testing.T) {
		targets := gdCallTargets(g, "src/ui/Main.gd::_on_night_rest_toggle")
		assert.Contains(t, targets, "src/systems/Notify.gd::event",
			"Notify.event() must bind to Notify")
		assert.NotContains(t, targets, "src/ui/BannerPath.gd::event",
			"the same-named method in the caller's own directory is a phantom")
	})

	t.Run("a class_name global resolves across directories", func(t *testing.T) {
		assert.Contains(t, gdCallTargets(g, "src/systems/Research.gd::run"),
			"src/data/Techs.gd::get_tech",
			"class_name is project-global; no import exists to corroborate it")
	})

	t.Run("an autoload singleton resolves to its declared script", func(t *testing.T) {
		assert.Contains(t, gdCallTargets(g, "src/boot/Setup.gd::boot"),
			"src/core/Game.gd::set_speed",
			"project.godot is the only place the name Game names this script")
	})

	t.Run("callers see every call site", func(t *testing.T) {
		var callers []string
		for _, e := range g.GetInEdges("src/data/Techs.gd::get_tech") {
			if e.Kind == graph.EdgeCalls {
				callers = append(callers, e.From)
			}
		}
		assert.Contains(t, callers, "src/systems/Research.gd::run",
			"blast-radius analysis is only as honest as the caller list")
	})
}
