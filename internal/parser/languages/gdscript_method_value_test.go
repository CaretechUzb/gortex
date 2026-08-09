package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// gdRefsTo collects the sources of every reference edge landing on a
// target, one entry per line so a duplicate is visible.
func gdRefsTo(res *parser.ExtractionResult, to string) []int {
	var lines []int
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeReferences && e.To == to {
			lines = append(lines, e.Line)
		}
	}
	return lines
}

func gdExtract(t *testing.T, path, src string) *parser.ExtractionResult {
	t.Helper()
	res, err := NewGDScriptExtractor().Extract(path, []byte(src))
	require.NoError(t, err)
	return res
}

// A method mentioned without parentheses is a reference to the method.
// That is how Godot wires every signal, and it is invisible to a scan
// that only reads call syntax.
func TestGDScriptExtractor_MethodValueReferences(t *testing.T) {
	res := gdExtract(t, "Panel.gd", `class_name Panel
extends Control

func _ready() -> void:
    button.pressed.connect(_on_random)
    area.entered.connect(self._on_random)
    timer.timeout.connect(Callable(self, "_on_random"))
    legacy.connect("pressed", self, "_on_random")
    var f = _on_random

func _on_random() -> void:
    pass
`)

	assert.ElementsMatch(t, []int{5, 6, 7, 8, 9}, gdRefsTo(res, "Panel.gd::_on_random"),
		"every wiring shape is a usage of the handler")
}

// The scan must not turn ordinary source into references. Each of these
// would be a false rename target, and rename writes to disk.
func TestGDScriptExtractor_MethodValueReferencePrecision(t *testing.T) {
	res := gdExtract(t, "Panel.gd", `class_name Panel
extends Control

# handler is mentioned in this comment
var note = "handler"

func run(handler) -> void:
    print(handler)
    other.handler
    var handler2 = 1

func handler() -> void:
    pass
`)

	assert.Empty(t, gdRefsTo(res, "Panel.gd::handler"),
		"a comment, a plain string, a shadowing parameter and another "+
			"object's member are none of them this script's method")
}

func TestGDScriptExtractor_MethodValueSkipsOrdinaryCalls(t *testing.T) {
	res := gdExtract(t, "Panel.gd", `class_name Panel
extends Control

func run() -> void:
    handler()
    self.handler()

func handler() -> void:
    pass
`)

	assert.Empty(t, gdRefsTo(res, "Panel.gd::handler"),
		"a call is a call edge; duplicating it as a reference double-counts it")
	calls := gdCalls(res)
	assert.Contains(t, calls, "unresolved::*.handler")
}

// A call outside every func still calls something. Dropping it loses a
// script's whole initialisation layer from the callee's caller list.
func TestGDScriptExtractor_ClassBodyCallsAttributeToFile(t *testing.T) {
	res := gdExtract(t, "Hud.gd", `class_name Hud
extends Control

var hp = Stats.roll()

@onready var ui = Hud.build()

func draw() -> void:
    Paint.line()
`)

	byTarget := map[string]*graph.Edge{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls {
			byTarget[e.To+"@"+e.Meta["receiver_type"].(string)] = e
		}
	}
	require.Contains(t, byTarget, "unresolved::*.roll@Stats")
	assert.Equal(t, "Hud.gd", byTarget["unresolved::*.roll@Stats"].From)
	require.Contains(t, byTarget, "unresolved::*.build@Hud")
	assert.Equal(t, "Hud.gd", byTarget["unresolved::*.build@Hud"].From)
	require.Contains(t, byTarget, "unresolved::*.line@Paint")
	assert.Equal(t, "Hud.gd::draw", byTarget["unresolved::*.line@Paint"].From,
		"a call inside a func still belongs to that func")
}

// A script with no `class_name` — every autoload entry point is one —
// has no nameable receiver, so a member-shaped self call would carry no
// evidence and settle on the tier find_usages suppresses by default.
func TestGDScriptExtractor_SelfCallWithoutClassName(t *testing.T) {
	res := gdExtract(t, "Game.gd", `extends Node

func start() -> void:
    self.tick()
    self.free()

func tick() -> void:
    pass
`)

	calls := gdCalls(res)
	require.Contains(t, calls, "unresolved::tick",
		"the file declares tick, so the same-file tier can bind it exactly")
	assert.Contains(t, calls, "unresolved::*.free",
		"an inherited engine method is not this file's to claim")
}

func TestGDScriptExtractor_PreloadAliasAndExtends(t *testing.T) {
	res := gdExtract(t, "Hud.gd", `class_name Hud
extends BasePanel

const M = preload("res://src/util/Maths.gd")
var Late = load("res://src/util/Late.gd")

func draw() -> void:
    M.clampf(1.0)
`)

	byID := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byID[n.ID] = n
	}
	require.Contains(t, byID, "Hud.gd::M")
	assert.Equal(t, "res://src/util/Maths.gd", byID["Hud.gd::M"].Meta["gd_preload"])
	require.Contains(t, byID, "Hud.gd::Late")
	assert.Equal(t, "res://src/util/Late.gd", byID["Hud.gd::Late"].Meta["gd_preload"])

	require.Contains(t, byID, "Hud.gd::Hud")
	assert.Equal(t, "BasePanel", byID["Hud.gd::Hud"].Meta["gd_extends"],
		"the receiver gate walks this chain before calling a mismatch")

	assert.Equal(t, "M", gdCalls(res)["unresolved::*.clampf"].Meta["receiver_type"],
		"an alias is a receiver whatever its casing")
}

func TestGDScriptExtractor_OpaqueExtendsIsRecorded(t *testing.T) {
	res := gdExtract(t, "Hud.gd", `class_name Hud
extends "res://src/base/Panel.gd"
`)
	for _, n := range res.Nodes {
		if n.ID == "Hud.gd::Hud" {
			assert.Equal(t, true, n.Meta["gd_extends_opaque"],
				"a base named by path is unknown, not absent")
			assert.NotContains(t, n.Meta, "gd_extends")
			return
		}
	}
	t.Fatal("class_name node missing")
}
