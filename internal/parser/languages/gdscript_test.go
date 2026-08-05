package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// gdCalls indexes a GDScript extraction's call edges by target.
func gdCalls(res *parser.ExtractionResult) map[string]*graph.Edge {
	out := map[string]*graph.Edge{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls {
			out[e.To] = e
		}
	}
	return out
}

func TestGDScriptExtractor_PlayerScript(t *testing.T) {
	src := []byte(`class_name Player
extends CharacterBody2D

const SPEED := 200.0
var health: int = 100

signal died(reason: String)

enum State { IDLE, RUNNING, JUMPING }

class InnerHelper:
    var count := 0
    func bump():
        count += 1

func _ready() -> void:
    print("ready")
    _spawn_weapon()

func _spawn_weapon() -> void:
    var w := preload("res://weapons/sword.tres")
    add_child(w)
`)
	e := NewGDScriptExtractor()
	require.Equal(t, "gdscript", e.Language())
	require.Equal(t, []string{".gd"}, e.Extensions())

	res, err := e.Extract("player.gd", src)
	require.NoError(t, err)

	types, methods, vars, signals, imports := 0, 0, 0, 0, 0
	byName := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byName[n.Name] = n
		switch n.Kind {
		case graph.KindType:
			types++
		case graph.KindVariable:
			vars++
		case graph.KindMethod:
			if n.Meta != nil && n.Meta["gd_kind"] == "signal" {
				signals++
			} else {
				methods++
			}
		}
	}
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeImports {
			imports++
		}
	}

	assert.GreaterOrEqual(t, types, 3, "Player + InnerHelper + State enum")
	assert.GreaterOrEqual(t, vars, 2, "SPEED const + health")
	assert.GreaterOrEqual(t, signals, 1, "died signal")
	assert.GreaterOrEqual(t, imports, 2, "extends CharacterBody2D + preload")

	// A script IS a class: its funcs are methods of the `class_name`,
	// and an inner class owns the funcs in its block.
	assert.GreaterOrEqual(t, methods, 3, "bump + _ready + _spawn_weapon")
	require.Contains(t, byName, "_ready")
	assert.Equal(t, graph.KindMethod, byName["_ready"].Kind)
	assert.Equal(t, "Player", byName["_ready"].Meta["receiver"])
	require.Contains(t, byName, "bump")
	assert.Equal(t, "InnerHelper", byName["bump"].Meta["receiver"],
		"a func inside `class InnerHelper:` belongs to InnerHelper")
}

func TestGDScriptExtractor_CallEdges(t *testing.T) {
	src := []byte(`func main():
    hello()
    world()

func hello():
    pass
`)
	res, err := NewGDScriptExtractor().Extract("x.gd", src)
	require.NoError(t, err)
	calls := 0
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls {
			calls++
		}
	}
	assert.GreaterOrEqual(t, calls, 2)
}

// A call's receiver decides its target. `Notify.event(...)` must not
// degrade to the bare name `event`, which the resolver's locality
// fallback answers with whatever same-named method sits nearest the
// caller — the phantom edge to an unrelated `BannerPath.event`.
func TestGDScriptExtractor_ReceiverStampedOnCall(t *testing.T) {
	src := []byte(`class_name Main
extends Control

func _on_night_rest_toggle():
    Notify.event(L10n.t("ui_siege.05"))
`)
	res, err := NewGDScriptExtractor().Extract("src/ui/Main.gd", src)
	require.NoError(t, err)
	calls := gdCalls(res)

	require.NotContains(t, calls, "unresolved::event",
		"a call with a receiver must not be emitted as a bare name")
	ev := calls["unresolved::*.event"]
	require.NotNil(t, ev, "Notify.event() is a member call")
	assert.Equal(t, "Notify", ev.Meta["receiver_type"])
	assert.Equal(t, "src/ui/Main.gd::_on_night_rest_toggle", ev.From)

	tr := calls["unresolved::*.t"]
	require.NotNil(t, tr, "the nested L10n.t() call keeps its own receiver")
	assert.Equal(t, "L10n", tr.Meta["receiver_type"])
}

func TestGDScriptExtractor_ReceiverTypeFromDeclarations(t *testing.T) {
	src := []byte(`class_name ResearchSystem

var overlay: CodexOverlay
var techs := Techs.new()

func apply(store: Techs, raw):
    store.get_tech(raw)
    techs.get_tech(raw)
    overlay.refresh()
    self.recompute()
    recompute()
    untyped.whatever()

func recompute():
    pass
`)
	res, err := NewGDScriptExtractor().Extract("src/sys/ResearchSystem.gd", src)
	require.NoError(t, err)
	calls := gdCalls(res)

	getTech := calls["unresolved::*.get_tech"]
	require.NotNil(t, getTech)
	assert.Equal(t, "Techs", getTech.Meta["receiver_type"],
		"a typed parameter names the receiver's type")

	refresh := calls["unresolved::*.refresh"]
	require.NotNil(t, refresh)
	assert.Equal(t, "CodexOverlay", refresh.Meta["receiver_type"],
		"a typed member var names the receiver's type")

	recompute := calls["unresolved::*.recompute"]
	require.NotNil(t, recompute)
	assert.Equal(t, "ResearchSystem", recompute.Meta["receiver_type"],
		"self. and a bare call to an own method both resolve to the script's class")

	// An untyped local receiver still produces a member call — the
	// resolver's lone-definition rule can bind it — but claims no type
	// it cannot support.
	whatever := calls["unresolved::*.whatever"]
	require.NotNil(t, whatever)
	assert.Nil(t, whatever.Meta, "no receiver type may be invented for an untyped local")
}

// A bare call to something the script does not declare (an engine
// builtin, a method inherited from Node) keeps the free-function shape:
// claiming the script's own class as its receiver would be a lie.
func TestGDScriptExtractor_BareBuiltinCallKeepsFreeFunctionShape(t *testing.T) {
	src := []byte(`class_name Spawner

func spawn():
    add_child(self)
    emit_signal("done")
`)
	res, err := NewGDScriptExtractor().Extract("src/Spawner.gd", src)
	require.NoError(t, err)
	calls := gdCalls(res)
	assert.Contains(t, calls, "unresolved::add_child")
	assert.NotContains(t, calls, "unresolved::*.add_child")
}

func TestGDScriptExtractor_CommentsAndStringsAreNotCalls(t *testing.T) {
	src := []byte(`class_name Doc

func run():
    # never_called() in a comment
    var s = "also_not_called() in a string"
    real_call()

func real_call():
    pass
`)
	res, err := NewGDScriptExtractor().Extract("doc.gd", src)
	require.NoError(t, err)
	calls := gdCalls(res)
	assert.NotContains(t, calls, "unresolved::never_called")
	assert.NotContains(t, calls, "unresolved::also_not_called")
	assert.Contains(t, calls, "unresolved::*.real_call")
}

// `signal died(reason: String)` declares a signal; it is not a call to
// something named `died`.
func TestGDScriptExtractor_SignalDeclarationIsNotACall(t *testing.T) {
	src := []byte(`class_name Actor

signal died(reason: String)

func hit():
    died.emit("hp")
`)
	res, err := NewGDScriptExtractor().Extract("actor.gd", src)
	require.NoError(t, err)
	calls := gdCalls(res)
	assert.NotContains(t, calls, "unresolved::died")
	assert.Contains(t, calls, "unresolved::*.emit")
}

// A script with no `class_name` has no nameable owner, so its funcs stay
// plain functions rather than methods of a class that does not exist.
func TestGDScriptExtractor_NoClassNameKeepsFunctions(t *testing.T) {
	src := []byte(`extends Node

func helper():
    pass
`)
	res, err := NewGDScriptExtractor().Extract("anon.gd", src)
	require.NoError(t, err)
	for _, n := range res.Nodes {
		if n.Name == "helper" {
			assert.Equal(t, graph.KindFunction, n.Kind)
			assert.Nil(t, n.Meta)
		}
	}
}

func TestGDScriptExtractor_EmptyInput(t *testing.T) {
	res, err := NewGDScriptExtractor().Extract("empty.gd", []byte(""))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
}

func TestGDLooksGlobalName(t *testing.T) {
	for _, s := range []string{"Notify", "Techs", "CodexOverlay"} {
		assert.True(t, gdLooksGlobalName(s), s)
	}
	for _, s := range []string{"", "notify", "MAX_HP", "self", "String"} {
		assert.False(t, gdLooksGlobalName(s), s)
	}
}
