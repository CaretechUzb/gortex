package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

const godotManifest = `; Engine configuration file.
config_version=5

[application]

config/name="Latticefall"
run/main_scene="res://src/ui/Main.tscn"

[autoload]

Game="*res://src/core/Game.gd"
Notify="*res://src/systems/Notify.gd"
Disabled="res://src/systems/Legacy.gd"
Scene="*res://src/ui/Hud.tscn"

[display]

window/size/viewport_width=1920
`

func godotAutoloads(t *testing.T, src string) map[string]*graph.Node {
	t.Helper()
	e := NewGodotProjectExtractor()
	require.Equal(t, "godot_project", e.Language())
	require.Equal(t, []string{"project.godot"}, e.Extensions())

	res, err := e.Extract("project.godot", []byte(src))
	require.NoError(t, err)

	out := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		if n.Meta != nil && n.Meta["gd_kind"] == "autoload" {
			out[n.Name] = n
		}
	}
	return out
}

func TestGodotProjectExtractor_Autoloads(t *testing.T) {
	loads := godotAutoloads(t, godotManifest)

	require.Contains(t, loads, "Game")
	assert.Equal(t, "src/core/Game.gd", loads["Game"].Meta["gd_script"])
	assert.Equal(t, graph.KindType, loads["Game"].Kind,
		"an autoload names a singleton type for the resolver's in-repo-type gate")

	require.Contains(t, loads, "Notify")
	assert.Equal(t, "src/systems/Notify.gd", loads["Notify"].Meta["gd_script"])

	// A disabled entry (no `*`) still binds the name to the script.
	require.Contains(t, loads, "Disabled")
	assert.Equal(t, "src/systems/Legacy.gd", loads["Disabled"].Meta["gd_script"])

	// A scene autoload has no script members to bind calls to.
	assert.NotContains(t, loads, "Scene")

	// Keys from other sections never leak in.
	assert.NotContains(t, loads, "config")
	assert.NotContains(t, loads, "window")
}

func TestGodotProjectExtractor_NoAutoloadSection(t *testing.T) {
	res, err := NewGodotProjectExtractor().Extract("project.godot", []byte("config_version=5\n\n[application]\n\nconfig/name=\"X\"\n"))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
}

func TestGodotProjectExtractor_AutoloadSectionIsLast(t *testing.T) {
	loads := godotAutoloads(t, "config_version=5\n\n[autoload]\n\nGame=\"*res://src/core/Game.gd\"\n")
	require.Contains(t, loads, "Game")
	assert.Equal(t, "src/core/Game.gd", loads["Game"].Meta["gd_script"])
}

func TestGodotResPath(t *testing.T) {
	assert.Equal(t, "src/core/Game.gd", godotResPath(`*res://src/core/Game.gd`))
	assert.Equal(t, "src/core/Game.gd", godotResPath(`res://src/core/Game.gd`))
	assert.Equal(t, "", godotResPath(`*res://src/ui/Hud.tscn`), "scenes have no members")
	assert.Equal(t, "", godotResPath(`user://save.cfg`), "only project-root paths bind")
	assert.Equal(t, "", godotResPath(``))
}
