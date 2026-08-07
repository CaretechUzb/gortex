package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// GodotSceneExtractor extracts Godot's text resource formats — scenes
// (`.tscn`) and standalone resources (`.tres`). Both share one
// INI-flavoured grammar: bracketed section headers with quoted
// attributes, followed by `key = value` lines.
//
// The load-bearing output is the **scene → script** edge. In Godot a
// script is normally reached through the scene that instantiates it, so
// without it the blast radius of a `.gd` file stops at its own callers
// and misses every scene that mounts it:
//
//	[ext_resource type="Script" path="res://src/ui/Main.gd" id="1_a1b2c"]
//
// becomes an EdgeReferences from the scene file to an
// `unresolved::import::res://…` placeholder, which
// Resolver.resolveGodotResPaths binds to the indexed file. The same shape
// covers sub-scenes (`type="PackedScene"`) and assets.
//
// Scene nodes (`[node name="X" type="Y" parent="P"]`) become KindField
// symbols keyed by their scene path, so a scene has a searchable
// structure and a `script`/`instance` assignment attributes to the
// specific node that carries it rather than to the file as a whole.
//
// `project.godot` is NOT handled here — GodotProjectExtractor claims that
// manifest by exact basename and owns its `[autoload]` section. The
// `.godot` extension stays unclaimed by both, so the engine's `.godot/`
// cache directory never gets parsed.
//
// The language key is `godot_resource`, matching the generic forest
// grammar this replaces — registration order in RegisterAll keeps the
// forest entry from claiming these extensions.
type GodotSceneExtractor struct{}

// NewGodotSceneExtractor constructs a Godot text-resource extractor.
func NewGodotSceneExtractor() *GodotSceneExtractor { return &GodotSceneExtractor{} }

func (e *GodotSceneExtractor) Language() string { return "godot_resource" }

func (e *GodotSceneExtractor) Extensions() []string {
	return []string{".tscn", ".tres"}
}

var (
	// godotSceneSectionRe matches a section header line — `[ext_resource …]`,
	// `[node …]`. The body is parsed for quoted attributes. Distinct from
	// godot_project.go's godotSectionRe, which scans whole-file for a
	// section NAME only and does not carry the attribute tail.
	godotSceneSectionRe = regexp.MustCompile(`^\s*\[([A-Za-z_]\w*)\b([^\]]*)\]\s*$`)
	// godotAttrRe matches one `key="value"` attribute inside a header.
	godotAttrRe = regexp.MustCompile(`([A-Za-z_]\w*)\s*=\s*"([^"]*)"`)
	// godotBareIDRe matches the Godot 3 form of a resource id, which is an
	// unquoted integer (`id=1`) rather than Godot 4's quoted string.
	godotBareIDRe = regexp.MustCompile(`\bid\s*=\s*(\d+)\b`)
	// godotExtResourceRefRe matches an `ExtResource("id")` reference in a
	// property assignment — `script = ExtResource("1_a1b2c")` or
	// `instance=ExtResource("3_c3d4e")`. Godot 3 used a bare integer id.
	godotExtResourceRefRe = regexp.MustCompile(`ExtResource\(\s*"?([^"()\s]+)"?\s*\)`)
)

// godotResImportTarget is the placeholder an unbound `res://` path parks
// on until resolveGodotResPaths lands it on the indexed file.
func godotResImportTarget(resPath string) string {
	return "unresolved::import::" + resPath
}

func (e *GodotSceneExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	lines := strings.Split(string(src), "\n")
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: len(lines),
		Language: "godot_resource",
	}
	result.Nodes = append(result.Nodes, fileNode)

	// extResources maps an ext_resource id to its `res://` path, so a
	// later `script = ExtResource("id")` can attribute to the same target.
	extResources := make(map[string]string)
	// currentNode is the graph ID of the `[node …]` section in scope, so a
	// property assignment binds to the scene node that declares it. Empty
	// outside a node section.
	currentNode := ""
	seenNode := make(map[string]bool)
	// rootName is the scene's root node name, which every other node's
	// declared `parent` path is relative to. Prefixing it reproduces the
	// full NodePath, so a child can never collide with the root.
	rootName := ""

	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		lineNo := i + 1

		if m := godotSceneSectionRe.FindStringSubmatch(line); m != nil {
			section, attrs := m[1], parseGodotAttrs(m[2])
			currentNode = ""
			switch section {
			case "ext_resource":
				path := attrs["path"]
				if !strings.HasPrefix(path, "res://") {
					continue
				}
				if id := attrs["id"]; id != "" {
					extResources[id] = path
				}
				result.Edges = append(result.Edges, &graph.Edge{
					From: fileNode.ID, To: godotResImportTarget(path),
					Kind: graph.EdgeReferences, FilePath: filePath, Line: lineNo,
					Meta: map[string]any{"godot_ext_resource": attrs["type"]},
				})
			case "node":
				if attrs["parent"] == "" && rootName == "" {
					rootName = attrs["name"]
				}
				id := godotSceneNodeID(filePath, rootName, attrs["parent"], attrs["name"])
				if id == "" || seenNode[id] {
					// A duplicate scene path is malformed input; keep the
					// first declaration rather than minting a second node
					// on the same ID.
					currentNode = id
					continue
				}
				seenNode[id] = true
				currentNode = id
				result.Nodes = append(result.Nodes, &graph.Node{
					ID: id, Kind: graph.KindField, Name: attrs["name"],
					FilePath: filePath, StartLine: lineNo, EndLine: lineNo,
					Language: "godot_resource",
					Meta:     map[string]any{"godot_type": attrs["type"]},
				})
				result.Edges = append(result.Edges, &graph.Edge{
					From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
					FilePath: filePath, Line: lineNo,
				})
				// `instance=ExtResource("id")` rides the header itself.
				e.addExtResourceRef(result, extResources, id, line, filePath, lineNo)
			case "connection":
				// `[connection signal="pressed" from="Button" to="."
				// method="_on_random"]` wires a signal to a method of the
				// script attached to the `to` node. The method is named as
				// a bare string, so nothing in the scripts themselves
				// records the link: without this edge a rename rewrites the
				// method and leaves the scene calling a name that no longer
				// exists, with nothing raising an error.
				method := attrs["method"]
				if method == "" {
					continue
				}
				owner := godotScenePathNodeID(filePath, rootName, attrs["to"])
				if owner == "" {
					owner = fileNode.ID
				}
				// The method name rides in Meta as well as in the
				// placeholder: once a weaker pass binds the placeholder to
				// some same-named method, the name is the only way back to
				// what the scene actually wrote.
				meta := map[string]any{"godot_connection": true, "godot_method": method}
				if sig := attrs["signal"]; sig != "" {
					meta["godot_signal"] = sig
				}
				result.Edges = append(result.Edges, &graph.Edge{
					From: owner, To: "unresolved::*." + method,
					Kind: graph.EdgeReferences, FilePath: filePath, Line: lineNo,
					Meta: meta,
				})
			}
			continue
		}

		// A property assignment inside a node section — `script =
		// ExtResource("1_a1b2c")` is the per-node script binding.
		if currentNode != "" {
			e.addExtResourceRef(result, extResources, currentNode, line, filePath, lineNo)
		}
	}

	return result, nil
}

// addExtResourceRef emits an EdgeReferences from owner to every
// `ExtResource("id")` named in line whose id was declared by an
// `[ext_resource]` header earlier in the file. Godot always declares
// resources before use, so a forward reference cannot occur.
func (e *GodotSceneExtractor) addExtResourceRef(
	result *parser.ExtractionResult, extResources map[string]string,
	owner, line, filePath string, lineNo int,
) {
	for _, m := range godotExtResourceRefRe.FindAllStringSubmatch(line, -1) {
		path := extResources[m[1]]
		if path == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: owner, To: godotResImportTarget(path),
			Kind: graph.EdgeReferences, FilePath: filePath, Line: lineNo,
		})
	}
}

// parseGodotAttrs collects the quoted `key="value"` attributes of a
// section header, plus the one unquoted attribute that carries meaning:
// a Godot 3 integer resource id. Other unquoted values (`format=3`,
// `instance=ExtResource(…)`) are deliberately skipped — the callers that
// need them read the raw line.
func parseGodotAttrs(header string) map[string]string {
	attrs := make(map[string]string, 4)
	for _, m := range godotAttrRe.FindAllStringSubmatch(header, -1) {
		attrs[m[1]] = m[2]
	}
	if attrs["id"] == "" {
		if m := godotBareIDRe.FindStringSubmatch(header); m != nil {
			attrs["id"] = m[1]
		}
	}
	return attrs
}

// godotSceneNodeID builds a scene node's graph ID from its declared
// parent path and name. The root node has no `parent`; `parent="."` means
// a direct child of the root, and any deeper node names its path relative
// to the root (`parent="Panel/VBox"`). Prefixing root reproduces the
// NodePath Godot itself uses, so the ID is stable and collision-free
// within the scene — including a child that shares the root's name.
// godotScenePathNodeID resolves a NodePath as written in a `[connection]`
// header (`to="."`, `to="Panel/VBox"`) to the scene node's graph ID. The
// path is relative to the scene root, which `godotSceneNodeID` also
// prefixes, so the two agree on every node in the scene.
func godotScenePathNodeID(filePath, root, nodePath string) string {
	if root == "" {
		return ""
	}
	p := strings.Trim(strings.TrimSpace(nodePath), "/")
	if p == "" || p == "." {
		return filePath + "::" + root
	}
	return filePath + "::" + root + "/" + p
}

func godotSceneNodeID(filePath, root, parent, name string) string {
	if name == "" {
		return ""
	}
	path := name
	switch parent {
	case "":
		// The root itself (or a malformed scene with no root declared).
	case ".":
		path = root + "/" + name
	default:
		path = root + "/" + strings.Trim(parent, "/") + "/" + name
	}
	return filePath + "::" + strings.TrimPrefix(path, "/")
}

var _ parser.Extractor = (*GodotSceneExtractor)(nil)
