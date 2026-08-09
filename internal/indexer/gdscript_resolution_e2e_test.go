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

// The four GDScript attribution gaps that survived receiver-aware call
// extraction, each asserted through the real pipeline:
//
//  1. a static method's call sites resolve like an instance method's,
//     including through the `const X = preload(…)` alias that reaches a
//     script with no `class_name`;
//  2. calls originating in an autoload script are attributed to it —
//     including the class-body initialisers that run before any func and
//     the `self.`-qualified calls a script with no `class_name` makes;
//  3. a caller list contains only calls whose receiver resolves to that
//     class, so a same-named method elsewhere cannot inflate it;
//  4. a method referenced without parentheses — `connect(_on_x)`,
//     `Callable(self, "_on_x")`, a scene `[connection]` block — counts as
//     a usage, so rename rewrites it instead of silently unwiring a signal.
func gdIndexResolutionProject(t *testing.T) *graph.Graph {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"project.godot": `config_version=5

[autoload]

Game="*res://src/core/Game.gd"
`,
		// A static method on a global class, in a directory of its own.
		"src/save/Persist.gd": `class_name Persist
extends RefCounted

static func snapshot(c) -> Dictionary:
    return {}
`,
		"src/combat/Combat.gd": `class_name Combat
extends Node

func save() -> Dictionary:
    return Persist.snapshot(self)
`,
		// A static-utility script with NO class_name, reached only through
		// the preload alias each caller declares for it.
		"src/util/Maths.gd": `extends RefCounted

static func clampf(v):
    return v
`,
		"src/ui/Hud.gd": `class_name Hud
extends Control

const M = preload("res://src/util/Maths.gd")

var initial = Persist.snapshot(self)

func draw() -> void:
    M.clampf(1.0)
`,
		// The autoload entry point: no class_name, so its funcs belong to
		// no nameable class.
		"src/core/Game.gd": `extends Node

func start_mission() -> void:
    Carry.apply(1, 2, 3)
    self.tick()

func tick() -> void:
    pass
`,
		// Three unrelated classes declaring `apply`. Carry sits in the
		// caller's own directory — the phantom's usual source.
		"src/ui/Carry.gd": `class_name Carry
extends RefCounted

static func apply(world, state, payload) -> void:
    pass
`,
		"src/data/Items.gd": `class_name Items
extends RefCounted

static func give(state, av, item_id) -> void:
    pass
`,
		"src/ui/Caller.gd": `class_name Caller
extends Control

func run() -> void:
    Items.apply(1, 2, 3)
    Input.apply(1, 2, 3)
`,
		// Inherited dispatch: a call on the subclass must keep binding to
		// the base's method. The gate must not read this as a mismatch.
		"src/base/Base.gd": `class_name Base
extends Node

func shared() -> void:
    pass
`,
		"src/base/Derived.gd": `class_name Derived
extends Base
`,
		"src/base/UseDerived.gd": `class_name UseDerived
extends Node

var d: Derived

func go() -> void:
    d.shared()
`,
		// Signal wiring: by method value, by Callable string, and from the
		// scene file.
		"src/ui/Panel.gd": `class_name Panel
extends Control

func _ready() -> void:
    button.pressed.connect(_on_random)
    other.thing.connect(Callable(self, "_on_named"))

func _on_random() -> void:
    pass

func _on_named() -> void:
    pass

func _on_scene() -> void:
    pass
`,
		"src/ui/Panel.tscn": `[gd_scene load_steps=2 format=3]

[ext_resource type="Script" path="res://src/ui/Panel.gd" id="1_abc"]

[node name="Panel" type="Control"]
script = ExtResource("1_abc")

[node name="Button" type="Button" parent="."]

[connection signal="pressed" from="Button" to="." method="_on_scene"]
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

// gdEdgesInto returns the sources of every edge of kind k landing on id
// that a default query would show — a demoted edge is excluded, which is
// the whole point of the receiver gate.
func gdEdgesInto(g *graph.Graph, id string, k graph.EdgeKind) []string {
	var out []string
	for _, e := range g.GetInEdges(id) {
		if e.Kind != k {
			continue
		}
		if speculative, _ := e.Meta[graph.MetaSpeculative].(bool); speculative {
			continue
		}
		out = append(out, e.From)
	}
	return out
}

func gdOutTargets(g *graph.Graph, from string, k graph.EdgeKind) []string {
	var out []string
	for _, e := range g.GetOutEdges(from) {
		if e.Kind == k && !graph.IsUnresolvedTarget(e.To) {
			out = append(out, e.To)
		}
	}
	return out
}

func TestGDScriptResolution_StaticAndPreloadAlias(t *testing.T) {
	g := gdIndexResolutionProject(t)

	t.Run("a static method resolves across directories", func(t *testing.T) {
		assert.Contains(t, gdOutTargets(g, "src/combat/Combat.gd::save", graph.EdgeCalls),
			"src/save/Persist.gd::snapshot",
			"static is not a different kind of receiver")
	})

	t.Run("a preload alias names a script with no class_name", func(t *testing.T) {
		assert.Contains(t, gdOutTargets(g, "src/ui/Hud.gd::draw", graph.EdgeCalls),
			"src/util/Maths.gd::clampf",
			"`const M = preload(…)` is how a static-utility script is reached")
	})

	t.Run("preload binds to the indexed script, not an external stub", func(t *testing.T) {
		assert.Contains(t, gdOutTargets(g, "src/ui/Hud.gd", graph.EdgeImports),
			"src/util/Maths.gd",
			"a res:// path is rooted at the project, so it always names an indexed file")
	})

	t.Run("a scene's ext_resource script reference binds", func(t *testing.T) {
		assert.Contains(t, gdOutTargets(g, "src/ui/Panel.tscn", graph.EdgeReferences),
			"src/ui/Panel.gd",
			"in Godot a script's blast radius runs through the scene that mounts it")
	})
}

func TestGDScriptResolution_AutoloadScriptAsCaller(t *testing.T) {
	g := gdIndexResolutionProject(t)

	t.Run("a call from an autoload script is attributed to it", func(t *testing.T) {
		assert.Contains(t, gdEdgesInto(g, "src/ui/Carry.gd::apply", graph.EdgeCalls),
			"src/core/Game.gd::start_mission",
			"the autoload entry point is usually the game's most load-bearing file")
	})

	t.Run("a self call in a class_name-less script is not a weak-tier guess", func(t *testing.T) {
		var found *graph.Edge
		for _, e := range g.GetInEdges("src/core/Game.gd::tick") {
			if e.Kind == graph.EdgeCalls && e.From == "src/core/Game.gd::start_mission" {
				found = e
			}
		}
		require.NotNil(t, found, "self.tick() is a call to this script's own method")
		assert.Equal(t, graph.OriginASTResolved, found.EffectiveOrigin(),
			"a script with no class_name still owns its own methods; "+
				"the name-only tier is suppressed by default and hides the call")
	})

	t.Run("a class-body initialiser call is attributed to the file", func(t *testing.T) {
		assert.Contains(t, gdEdgesInto(g, "src/save/Persist.gd::snapshot", graph.EdgeCalls),
			"src/ui/Hud.gd",
			"`var initial = Persist.snapshot(self)` runs, so it is a call site")
	})
}

func TestGDScriptResolution_ReceiverGate(t *testing.T) {
	g := gdIndexResolutionProject(t)

	t.Run("a caller list holds only calls whose receiver names that class", func(t *testing.T) {
		callers := gdEdgesInto(g, "src/ui/Carry.gd::apply", graph.EdgeCalls)
		assert.NotContains(t, callers, "src/ui/Caller.gd::run",
			"Items.apply() and Input.apply() belong to other receivers; "+
				"counting them makes a blast radius look reassuring while pointing at the wrong code")
		assert.Contains(t, callers, "src/core/Game.gd::start_mission",
			"the real Carry.apply() call site must survive the gate")
	})

	t.Run("a mismatched bind is demoted, not deleted", func(t *testing.T) {
		var demoted int
		for _, e := range g.GetOutEdges("src/ui/Caller.gd::run") {
			if e.Kind != graph.EdgeCalls {
				continue
			}
			if reason, _ := e.Meta["demoted"].(string); reason == "receiver_type_mismatch" {
				demoted++
			}
		}
		assert.Equal(t, 2, demoted,
			"the evidence stays inspectable at the speculative tier")
	})

	t.Run("inherited dispatch is not a mismatch", func(t *testing.T) {
		assert.Contains(t, gdEdgesInto(g, "src/base/Base.gd::shared", graph.EdgeCalls),
			"src/base/UseDerived.gd::go",
			"a Derived receiver landing on Base.shared is dispatch, not a phantom")
	})
}

func TestGDScriptResolution_MethodReferencesAreUsages(t *testing.T) {
	g := gdIndexResolutionProject(t)

	t.Run("connect(_on_x) is a usage", func(t *testing.T) {
		assert.Contains(t, gdEdgesInto(g, "src/ui/Panel.gd::_on_random", graph.EdgeReferences),
			"src/ui/Panel.gd::_ready",
			"renaming past this leaves a button that silently stops working")
	})

	t.Run("Callable(self, \"_on_x\") is a usage", func(t *testing.T) {
		assert.Contains(t, gdEdgesInto(g, "src/ui/Panel.gd::_on_named", graph.EdgeReferences),
			"src/ui/Panel.gd::_ready")
	})

	t.Run("a scene [connection] is a usage of the script's method", func(t *testing.T) {
		refs := gdEdgesInto(g, "src/ui/Panel.gd::_on_scene", graph.EdgeReferences)
		assert.Contains(t, refs, "src/ui/Panel.tscn::Panel",
			"the connection names the method as a bare string in a third file")
	})

	t.Run("the connection binds through the node's script, not by name", func(t *testing.T) {
		var conn *graph.Edge
		for _, e := range g.GetInEdges("src/ui/Panel.gd::_on_scene") {
			if e.Kind == graph.EdgeReferences && e.FilePath == "src/ui/Panel.tscn" {
				conn = e
			}
		}
		require.NotNil(t, conn)
		assert.Equal(t, "godot-connection", conn.Meta["synthesized_by"],
			"binding by name alone is right only while the handler name is unique")
	})

	t.Run("an ordinary call is still a call, not a reference", func(t *testing.T) {
		assert.NotContains(t, gdEdgesInto(g, "src/core/Game.gd::tick", graph.EdgeReferences),
			"src/core/Game.gd::start_mission")
	})
}
