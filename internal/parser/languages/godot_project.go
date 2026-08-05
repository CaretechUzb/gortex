package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Godot's `project.godot` is the project manifest. The one thing in it
// the call graph cannot do without is the `[autoload]` section:
//
//	[autoload]
//	Game="*res://src/core/Game.gd"
//	Notify="*res://src/systems/Notify.gd"
//
// Each entry binds a PROJECT-GLOBAL identifier to a script. `Game` is
// then callable from any script in the project with no import, no
// preload and no `class_name` on the target — so without reading this
// file there is nothing anywhere in the sources that connects the name
// `Game` to `src/core/Game.gd`, and every `Game.set_speed()` call in
// the project is unresolvable by construction.
//
// The extractor emits one KindType node per autoload, carrying the
// declared script path in Meta["gd_script"]. ResolveGDScriptAutoloads
// consumes those to bind the calls. Emitting KindType (rather than a
// variable) is deliberate: an autoload names a singleton *type* for the
// resolver's in-repo-type gate, which is what keeps a receiver hint of
// `Game` from being discarded as an unknown external.
//
// The leading `*` marks the autoload as enabled; a disabled entry keeps
// the same binding and is indexed the same way — the name still
// resolves for anyone reading the code.
var (
	godotSectionRe  = regexp.MustCompile(`(?m)^\s*\[([^\]]+)\]`)
	godotAutoloadRe = regexp.MustCompile(`(?m)^\s*([A-Za-z_]\w*)\s*=\s*"([^"]*)"`)
)

// GodotProjectExtractor indexes a Godot `project.godot` manifest.
type GodotProjectExtractor struct{}

func NewGodotProjectExtractor() *GodotProjectExtractor { return &GodotProjectExtractor{} }

func (e *GodotProjectExtractor) Language() string { return "godot_project" }

// Extensions claims the manifest by exact basename. `.godot` is not
// claimed as an extension — the engine's `.godot/` cache directory and
// any other file ending that way are none of this extractor's business.
func (e *GodotProjectExtractor) Extensions() []string { return []string{"project.godot"} }

func (e *GodotProjectExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	lines := strings.Split(string(src), "\n")
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: len(lines),
		Language: "godot_project",
	}
	result.Nodes = append(result.Nodes, fileNode)

	start, end := godotAutoloadSection(src)
	if start < 0 {
		return result, nil
	}

	seen := make(map[string]bool)
	for _, m := range godotAutoloadRe.FindAllSubmatchIndex(src[start:end], -1) {
		name := string(src[start+m[2] : start+m[3]])
		value := string(src[start+m[4] : start+m[5]])
		script := godotResPath(value)
		if script == "" || seen[name] {
			continue
		}
		seen[name] = true
		line := lineAt(src, start+m[0])
		id := filePath + "::" + name
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: line, EndLine: line,
			Language: "godot_project",
			Meta:     map[string]any{"gd_kind": "autoload", "gd_script": script},
		})
		result.Edges = append(result.Edges,
			&graph.Edge{
				From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
				FilePath: filePath, Line: line,
			},
			&graph.Edge{
				From: id, To: "unresolved::import::" + script,
				Kind: graph.EdgeImports, FilePath: filePath, Line: line,
			},
		)
	}
	return result, nil
}

// godotAutoloadSection returns the byte range of the `[autoload]`
// section body — from just past its header to the start of the next
// section header (or EOF). Returns (-1, -1) when the file has none.
func godotAutoloadSection(src []byte) (int, int) {
	for _, m := range godotSectionRe.FindAllSubmatchIndex(src, -1) {
		if strings.TrimSpace(string(src[m[2]:m[3]])) != "autoload" {
			continue
		}
		body := m[1]
		if next := godotSectionRe.FindIndex(src[body:]); next != nil {
			return body, body + next[0]
		}
		return body, len(src)
	}
	return -1, -1
}

// godotResPath converts an autoload value (`*res://src/core/Game.gd`)
// to a repo-relative script path. The `*` enable-marker and the
// `res://` project-root scheme are stripped. Values naming anything but
// a script (a scene autoload, `res://x.tscn`) return "" — the resolver
// pass binds calls to script members only.
func godotResPath(value string) string {
	v := strings.TrimSpace(value)
	v = strings.TrimPrefix(v, "*")
	v, ok := strings.CutPrefix(v, "res://")
	if !ok || !strings.HasSuffix(v, ".gd") {
		return ""
	}
	return strings.TrimPrefix(v, "/")
}

var _ parser.Extractor = (*GodotProjectExtractor)(nil)
