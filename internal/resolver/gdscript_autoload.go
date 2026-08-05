package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Godot autoload resolution. An `[autoload]` entry in `project.godot`
// binds a project-global identifier to a script:
//
//	Game="*res://src/core/Game.gd"
//
// From then on `Game.set_speed()` is legal in every script in the
// project — with no import, no preload, and no `class_name` on the
// target. The GDScript extractor stamps `Game` as the call's
// receiver_type, but the generic receiver passes can only match that
// against a method whose own `receiver` says `Game`, and an autoload
// script typically declares no `class_name` at all. So the name and the
// definition never meet, and every cross-script autoload call stays
// unresolved.
//
// This pass supplies the missing link: it reads the autoload nodes the
// project.godot extractor emitted, maps each singleton name to its
// script, and binds residual `unresolved::*.method` calls whose
// receiver_type names an autoload to that script's member of the same
// name. Already-resolved edges are never touched, and a name the script
// does not define is left alone rather than bound to the file.
func ResolveGDScriptAutoloads(g graph.Store) int {
	if g == nil {
		return 0
	}
	scripts := gdAutoloadScripts(g)
	if len(scripts) == 0 {
		return 0
	}
	members := gdMembersByFile(g, scripts)
	resolved := 0
	var reindex []graph.EdgeReindex
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.Meta == nil || !graph.IsUnresolvedTarget(e.To) {
			continue
		}
		recv, _ := e.Meta["receiver_type"].(string)
		if recv == "" {
			continue
		}
		fileID, ok := scripts[recv]
		if !ok {
			continue
		}
		method := strings.TrimPrefix(graph.UnresolvedName(e.To), "*.")
		target := members[fileID][method]
		if target == "" {
			continue
		}
		oldTo := e.To
		e.To = target
		// The binding is structural, not a name guess: project.godot
		// states which script the receiver names, and the script
		// declares the member. That is the same grade of evidence the
		// extractor's own receiver_type match carries.
		e.Origin = graph.OriginASTResolved
		e.Confidence = 0.9
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.9)
		StampSynthesized(e, SynthGodotAutoload)
		reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		resolved++
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

// gdAutoloadScripts maps each autoload singleton name to the graph file
// ID of the script it names. An entry whose script is not indexed, and
// a name declared by two different manifests with conflicting targets,
// are both dropped — a wrong binding here is worse than none.
func gdAutoloadScripts(g graph.Store) map[string]string {
	var decls map[string]string
	for n := range g.NodesByKind(graph.KindType) {
		if n == nil || n.Meta == nil || n.Language != "godot_project" {
			continue
		}
		if kind, _ := n.Meta["gd_kind"].(string); kind != "autoload" {
			continue
		}
		script, _ := n.Meta["gd_script"].(string)
		if script == "" {
			continue
		}
		if decls == nil {
			decls = map[string]string{}
		}
		if prev, dup := decls[n.Name]; dup && prev != script {
			decls[n.Name] = ""
			continue
		}
		decls[n.Name] = script
	}
	if len(decls) == 0 {
		return nil
	}
	// `res://` paths are project-root relative; graph file IDs carry the
	// repo prefix and the repo-relative path. Match by path suffix on a
	// separator boundary, and drop any singleton whose path is ambiguous
	// across two indexed scripts. One pass over the file nodes serves
	// every autoload — NodesByKind is a full scan on a disk backend, and
	// a scan per singleton would multiply that by the manifest's length.
	out := make(map[string]string, len(decls))
	ambiguous := make(map[string]bool, len(decls))
	for f := range g.NodesByKind(graph.KindFile) {
		if f == nil || f.Language != "gdscript" {
			continue
		}
		for name, script := range decls {
			if script == "" || ambiguous[name] || !gdPathMatches(f.FilePath, script) {
				continue
			}
			if prev, seen := out[name]; seen && prev != f.ID {
				delete(out, name)
				ambiguous[name] = true
				continue
			}
			out[name] = f.ID
		}
	}
	return out
}

// gdPathMatches reports whether an indexed file path ends with the
// project-relative script path on a path-separator boundary, so
// `src/core/Game.gd` matches `<repo>/src/core/Game.gd` but never
// `src/core/NotGame.gd`.
func gdPathMatches(filePath, script string) bool {
	p := strings.ReplaceAll(filePath, "\\", "/")
	if p == script {
		return true
	}
	return strings.HasSuffix(p, "/"+script)
}

// gdMembersByFile indexes the callable members declared by each autoload
// script, by name. Signals are included: `Game.started.emit()` and a
// direct connect both reference the signal as a member.
func gdMembersByFile(g graph.Store, scripts map[string]string) map[string]map[string]string {
	want := make(map[string]bool, len(scripts))
	for _, fileID := range scripts {
		want[fileID] = true
	}
	out := make(map[string]map[string]string, len(want))
	for _, kind := range []graph.NodeKind{graph.KindMethod, graph.KindFunction} {
		for n := range g.NodesByKind(kind) {
			if n == nil || n.Language != "gdscript" {
				continue
			}
			fileID, _, ok := strings.Cut(n.ID, "::")
			if !ok || !want[fileID] {
				continue
			}
			if out[fileID] == nil {
				out[fileID] = map[string]string{}
			}
			if _, exists := out[fileID][n.Name]; !exists {
				out[fileID][n.Name] = n.ID
			}
		}
	}
	return out
}
