package resolver

import (
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const rustAliasDepthLimit = 32

type rustAliasKey struct {
	repo string
	file string
	name string
}

type rustExportAliasKey struct {
	repo  string
	crate string
	name  string
}

type rustAliasEntry struct {
	source    string
	file      string
	ambiguous bool
}

type rustTypeAliasIndex struct {
	local    map[rustAliasKey]rustAliasEntry
	exported map[rustExportAliasKey]rustAliasEntry
}

type rustUseBinding struct {
	source string
	local  string
}

func newRustTypeAliasIndex(g graph.Store) *rustTypeAliasIndex {
	if g == nil {
		return nil
	}
	idx := &rustTypeAliasIndex{
		local:    make(map[rustAliasKey]rustAliasEntry),
		exported: make(map[rustExportAliasKey]rustAliasEntry),
	}
	for edge := range g.EdgesByKind(graph.EdgeImports) {
		if edge == nil || !strings.HasSuffix(edge.From, ".rs") {
			continue
		}
		raw := rustUsePathFromEdge(edge)
		if raw == "" {
			continue
		}
		from := g.GetNode(edge.From)
		repo := ""
		if from != nil {
			repo = from.RepoPrefix
		}
		bindings := parseRustUseBindings(raw)
		for _, binding := range bindings {
			if binding.local == "" || binding.source == "" || binding.local == "_" {
				continue
			}
			entry := rustAliasEntry{source: binding.source, file: edge.From}
			addRustAlias(idx.local, rustAliasKey{repo: repo, file: edge.From, name: binding.local}, entry)
			if edge.Meta != nil {
				if reexport, _ := edge.Meta["reexport"].(bool); reexport {
					addRustAlias(idx.exported, rustExportAliasKey{
						repo:  repo,
						crate: rustAliasCrate(edge.From),
						name:  binding.local,
					}, entry)
				}
			}
		}
	}
	if len(idx.local) == 0 && len(idx.exported) == 0 {
		return nil
	}
	return idx
}

func addRustAlias[K comparable](aliases map[K]rustAliasEntry, key K, entry rustAliasEntry) {
	current, exists := aliases[key]
	if !exists {
		aliases[key] = entry
		return
	}
	if current.ambiguous || current.source != entry.source {
		current.ambiguous = true
		aliases[key] = current
	}
}

// resolve follows aliases visible in node's source file, then public re-export
// aliases at the crate boundary. It returns the canonical Rust path, whether an
// alias was traversed, and false when the alias set is ambiguous or cyclic.
func (idx *rustTypeAliasIndex) resolve(node *graph.Node, name string) (string, bool, bool) {
	if node == nil {
		name = normalizeRustUsePath(name)
		return name, false, name != ""
	}
	return idx.resolveAt(node.RepoPrefix, node.FilePath, name)
}

func (idx *rustTypeAliasIndex) resolveAt(repo, file, name string) (string, bool, bool) {
	name = normalizeRustUsePath(name)
	if idx == nil || name == "" {
		return name, false, name != ""
	}

	crate := rustAliasCrate(file)
	current := name
	changed := false
	seen := make(map[string]struct{}, rustAliasDepthLimit)

	for depth := 0; depth < rustAliasDepthLimit; depth++ {
		lookup := rustUseBindingName(current)
		if lookup == "" {
			return current, changed, true
		}
		state := file + "\x00" + current
		if _, duplicate := seen[state]; duplicate {
			return "", changed, false
		}
		seen[state] = struct{}{}

		entry, exists := idx.local[rustAliasKey{repo: repo, file: file, name: lookup}]
		if exists && entry.ambiguous {
			return "", changed, false
		}
		if exists && normalizeRustUsePath(entry.source) == current {
			// The current value is already this file-local import's canonical
			// path. Continue at the crate re-export layer instead of applying
			// the same alias forever.
			exists = false
		}
		if !exists {
			entry, exists = idx.exported[rustExportAliasKey{repo: repo, crate: crate, name: lookup}]
			if exists && entry.ambiguous {
				return "", changed, false
			}
			if exists && normalizeRustUsePath(entry.source) == current {
				return current, changed, true
			}
		}
		if !exists {
			return current, changed, true
		}
		if entry.source == "" {
			return "", changed, false
		}
		current = normalizeRustUsePath(entry.source)
		file = entry.file
		changed = true
	}
	return "", changed, false
}

func rustUsePathFromEdge(edge *graph.Edge) string {
	if edge == nil {
		return ""
	}
	if edge.Meta != nil {
		if raw, _ := edge.Meta["rust_use_path"].(string); strings.TrimSpace(raw) != "" {
			return normalizeRustUsePath(raw)
		}
	}
	const prefix = "unresolved::import::"
	if !strings.HasPrefix(edge.To, prefix) {
		return ""
	}
	return normalizeRustUsePath(strings.TrimPrefix(edge.To, prefix))
}

func normalizeRustUsePath(raw string) string {
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), ";"))
	raw = strings.ReplaceAll(raw, "/", "::")
	for strings.Contains(raw, "::::") {
		raw = strings.ReplaceAll(raw, "::::", "::")
	}
	return strings.Trim(raw, ":")
}

func parseRustUseBindings(raw string) []rustUseBinding {
	bindings := expandRustUseTree("", normalizeRustUsePath(raw))
	seen := make(map[rustUseBinding]struct{}, len(bindings))
	out := bindings[:0]
	for _, binding := range bindings {
		binding.source = normalizeRustUsePath(binding.source)
		binding.local = strings.TrimSpace(binding.local)
		if binding.source == "" || binding.local == "" || binding.local == "*" {
			continue
		}
		if _, duplicate := seen[binding]; duplicate {
			continue
		}
		seen[binding] = struct{}{}
		out = append(out, binding)
	}
	return out
}

func expandRustUseTree(prefix, tree string) []rustUseBinding {
	tree = strings.TrimSpace(tree)
	if tree == "" {
		return nil
	}
	if open := strings.IndexByte(tree, '{'); open >= 0 {
		close := matchingRustUseBrace(tree, open)
		if close < 0 || strings.TrimSpace(tree[close+1:]) != "" {
			return nil
		}
		base := strings.TrimSuffix(strings.TrimSpace(tree[:open]), "::")
		base = joinRustUsePath(prefix, base)
		var out []rustUseBinding
		for _, item := range splitRustUseGroup(tree[open+1 : close]) {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			leaf, alias := splitRustUseAlias(item)
			if leaf == "self" && base != "" {
				if alias == "" {
					alias = rustUseBindingName(base)
				}
				out = append(out, rustUseBinding{source: base, local: alias})
				continue
			}
			out = append(out, expandRustUseTree(base, item)...)
		}
		return out
	}

	leaf, alias := splitRustUseAlias(tree)
	if leaf == "" || leaf == "*" {
		return nil
	}
	source := joinRustUsePath(prefix, leaf)
	source = strings.TrimSuffix(source, "::self")
	if alias == "" {
		alias = rustUseBindingName(source)
	}
	return []rustUseBinding{{source: source, local: alias}}
}

func splitRustUseAlias(tree string) (string, string) {
	tree = strings.TrimSpace(tree)
	if i := strings.LastIndex(tree, " as "); i >= 0 {
		return strings.TrimSpace(tree[:i]), strings.TrimSpace(tree[i+4:])
	}
	return tree, ""
}

func matchingRustUseBrace(tree string, open int) int {
	depth := 0
	for i := open; i < len(tree); i++ {
		switch tree[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func splitRustUseGroup(group string) []string {
	start, depth := 0, 0
	var out []string
	for i := 0; i < len(group); i++ {
		switch group[i] {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(group[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(group[start:]))
	return out
}

func joinRustUsePath(prefix, suffix string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), ":")
	suffix = strings.Trim(strings.TrimSpace(suffix), ":")
	switch {
	case prefix == "":
		return suffix
	case suffix == "":
		return prefix
	default:
		return prefix + "::" + suffix
	}
}

func rustUseBindingName(source string) string {
	source = normalizeRustUsePath(source)
	if source == "" {
		return ""
	}
	if i := strings.LastIndex(source, "::"); i >= 0 {
		return strings.TrimSpace(source[i+2:])
	}
	return strings.TrimSpace(source)
}

func rustAliasCrate(file string) string {
	if crate := rustCrateOf(file); crate != "" {
		return crate
	}
	root := rustCrateRootDir(file)
	if root == "." || root == "/" {
		return path.Dir(file)
	}
	return root
}
