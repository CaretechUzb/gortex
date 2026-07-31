package resolver

import (
	"sort"
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
)

const rustTraitSuperDepthLimit = 16

func rustTypeParamTraitNames(value any, param string) []string {
	param = strings.TrimSpace(param)
	if param == "" {
		return nil
	}
	var bounds []string
	appendEntry := func(name, bound string) {
		if strings.TrimSpace(name) == param && strings.TrimSpace(bound) != "" {
			bounds = append(bounds, bound)
		}
	}
	switch entries := value.(type) {
	case []map[string]string:
		for _, entry := range entries {
			appendEntry(entry["name"], entry["bound"])
		}
	case []any:
		for _, raw := range entries {
			switch entry := raw.(type) {
			case map[string]any:
				name, _ := entry["name"].(string)
				bound, _ := entry["bound"].(string)
				appendEntry(name, bound)
			case map[string]string:
				appendEntry(entry["name"], entry["bound"])
			}
		}
	case map[string]string:
		appendEntry(param, entries[param])
	case map[string]any:
		if raw, ok := entries[param]; ok {
			if bound, ok := raw.(string); ok {
				appendEntry(param, bound)
			}
		}
	}

	seen := make(map[string]struct{})
	var names []string
	for _, bound := range bounds {
		for _, name := range parseRustTraitBoundNames(bound) {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	return names
}

func parseRustTraitBoundNames(bound string) []string {
	parts, ok := splitStructuredRustTraitBounds(bound)
	if !ok {
		return nil
	}
	seen := make(map[string]struct{})
	var names []string
	appendName := func(name string) {
		name = strings.Trim(strings.TrimSpace(name), ":")
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || strings.HasPrefix(part, "'") || strings.HasPrefix(part, "?") {
			continue
		}
		part = stripRustBoundPrefix(part)
		if part == "" || strings.HasPrefix(part, "'") || strings.HasPrefix(part, "?") || strings.HasPrefix(part, "<") {
			continue
		}
		name := rustTraitPathHead(part)
		if name == "" {
			continue
		}
		appendName(name)
	}
	return names
}

func splitStructuredRustTraitBounds(bound string) ([]string, bool) {
	var parts []string
	start := 0
	angle, paren, bracket, brace := 0, 0, 0, 0
	for i, r := range bound {
		switch r {
		case '<':
			angle++
		case '>':
			if i > 0 && bound[i-1] == '-' {
				continue
			}
			if angle == 0 {
				return nil, false
			}
			angle--
		case '(':
			paren++
		case ')':
			if paren == 0 {
				return nil, false
			}
			paren--
		case '[':
			bracket++
		case ']':
			if bracket == 0 {
				return nil, false
			}
			bracket--
		case '{':
			brace++
		case '}':
			if brace == 0 {
				return nil, false
			}
			brace--
		case '+':
			if angle == 0 && paren == 0 && bracket == 0 && brace == 0 {
				parts = append(parts, bound[start:i])
				start = i + 1
			}
		}
	}
	if angle != 0 || paren != 0 || bracket != 0 || brace != 0 {
		return nil, false
	}
	parts = append(parts, bound[start:])
	return parts, true
}

func stripRustBoundPrefix(part string) string {
	part = strings.TrimSpace(part)
	for strings.HasPrefix(part, "for") {
		rest := strings.TrimSpace(strings.TrimPrefix(part, "for"))
		if !strings.HasPrefix(rest, "<") {
			break
		}
		depth := 0
		end := -1
		for i, r := range rest {
			switch r {
			case '<':
				depth++
			case '>':
				if depth == 0 {
					return ""
				}
				depth--
				if depth == 0 {
					end = i + 1
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			return ""
		}
		part = strings.TrimSpace(rest[end:])
	}
	for {
		before := part
		for _, prefix := range []string{"~const ", "const ", "dyn ", "impl "} {
			if strings.HasPrefix(part, prefix) {
				part = strings.TrimSpace(strings.TrimPrefix(part, prefix))
				break
			}
		}
		if part == before {
			return part
		}
	}
}

func rustTraitPathHead(part string) string {
	part = strings.TrimSpace(part)
	end := len(part)
	for i, r := range part {
		switch r {
		case '<', '(', '[', '{', '=', ' ', '\t', '\r', '\n':
			end = i
		}
		if end != len(part) {
			break
		}
	}
	name := strings.TrimSpace(part[:end])
	if name == "" || strings.ContainsAny(name, "&*!,;") {
		return ""
	}
	segments := strings.Split(strings.Trim(name, ":"), "::")
	for i, segment := range segments {
		segment = strings.TrimPrefix(segment, "r#")
		if segment == "" {
			return ""
		}
		for pos, r := range segment {
			if r == '_' || unicode.IsLetter(r) || (pos > 0 && unicode.IsDigit(r)) {
				continue
			}
			return ""
		}
		segments[i] = segment
	}
	return strings.Join(segments, "::")
}

type rustTraitTargetKey struct {
	repo      string
	crateRoot string
	path      string
}

type rustTraitTargetEntry struct {
	id        string
	ambiguous bool
}

type rustTraitTargetIndex struct {
	exact    map[rustTraitTargetKey]rustTraitTargetEntry
	basename map[rustTraitTargetKey]rustTraitTargetEntry
	nodes    map[string]graph.QualifiedNodeIdentity
}

func resolveRustTraitExtendsWithIndex(g graph.Store, idx *rustTraitTargetIndex, aliases *rustTypeAliasIndex) int {
	if g == nil || idx == nil {
		return 0
	}
	var batch []graph.EdgeReindex
	for edge := range g.EdgesByKind(graph.EdgeExtends) {
		if edge == nil || !strings.HasPrefix(edge.To, "unresolved::extends::") {
			continue
		}
		child, ok := idx.nodes[edge.From]
		if !ok {
			continue
		}
		raw := strings.TrimPrefix(edge.To, "unresolved::extends::")
		if edge.Meta != nil {
			if path, _ := edge.Meta["rust_trait_path"].(string); path != "" {
				raw = path
			}
		}
		if aliases != nil {
			resolvedPath, _, ok := aliases.resolveAt(child.RepoPrefix, child.FilePath, raw)
			if !ok {
				continue
			}
			raw = resolvedPath
		}
		target := idx.resolveAt(child.FilePath, child.RepoPrefix, raw)
		if target == "" || target == edge.From {
			continue
		}
		oldTo := edge.To
		edge.To = target
		edge.Origin = graph.OriginASTResolved
		edge.Confidence = 0.95
		edge.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeExtends, edge.Confidence)
		if edge.Meta == nil {
			edge.Meta = map[string]any{}
		}
		edge.Meta["resolved_via"] = "rust_supertrait"
		batch = append(batch, graph.EdgeReindex{Edge: edge, OldTo: oldTo})
	}
	if len(batch) > 0 {
		g.ReindexEdges(batch)
	}
	return len(batch)
}

func newRustTraitTargetIndex(g graph.Store) *rustTraitTargetIndex {
	idx := &rustTraitTargetIndex{
		exact:    make(map[rustTraitTargetKey]rustTraitTargetEntry),
		basename: make(map[rustTraitTargetKey]rustTraitTargetEntry),
		nodes:    make(map[string]graph.QualifiedNodeIdentity),
	}
	for node := range graph.QualifiedNodeIdentitiesSeq(g, graph.KindInterface) {
		if node.Language != "rust" || node.ID == "" || node.Name == "" {
			continue
		}
		crateRoot, module := rustTraitCrateAndModule(node.FilePath)
		name := normalizeRustTraitIdentifier(node.Name)
		if name == "" {
			continue
		}
		idx.nodes[node.ID] = node
		baseKey := rustTraitTargetKey{repo: node.RepoPrefix, crateRoot: crateRoot, path: name}
		addRustTraitTarget(idx.basename, baseKey, node.ID)
		full := "crate::" + name
		if module != "" {
			full = "crate::" + module + "::" + name
		}
		addRustTraitTarget(idx.exact, rustTraitTargetKey{
			repo: node.RepoPrefix, crateRoot: crateRoot, path: full,
		}, node.ID)
		addRustTraitTarget(idx.exact, rustTraitTargetKey{
			repo: node.RepoPrefix, crateRoot: crateRoot, path: strings.TrimPrefix(full, "crate::"),
		}, node.ID)
		if qual := normalizeRustTraitPath(node.QualName); qual != "" {
			addRustTraitTarget(idx.exact, rustTraitTargetKey{
				repo: node.RepoPrefix, crateRoot: crateRoot, path: qual,
			}, node.ID)
		}
	}
	if len(idx.nodes) == 0 {
		return nil
	}
	return idx
}

func (idx *rustTraitTargetIndex) has(id string) bool {
	if idx == nil {
		return false
	}
	_, ok := idx.nodes[id]
	return ok
}

func addRustTraitTarget(index map[rustTraitTargetKey]rustTraitTargetEntry, key rustTraitTargetKey, id string) {
	if key.path == "" || id == "" {
		return
	}
	current, exists := index[key]
	if !exists {
		index[key] = rustTraitTargetEntry{id: id}
		return
	}
	if current.id == id && !current.ambiguous {
		return
	}
	index[key] = rustTraitTargetEntry{ambiguous: true}
}

func (idx *rustTraitTargetIndex) resolve(child *graph.Node, raw string) string {
	if child == nil {
		return ""
	}
	return idx.resolveAt(child.FilePath, child.RepoPrefix, raw)
}

func (idx *rustTraitTargetIndex) resolveAt(filePath, repoPrefix, raw string) string {
	if idx == nil {
		return ""
	}
	path := normalizeRustTraitPath(raw)
	if path == "" || strings.HasPrefix(strings.TrimSpace(raw), "?") {
		return ""
	}
	crateRoot, module := rustTraitCrateAndModule(filePath)
	lookupExact := func(candidate string) string {
		entry, ok := idx.exact[rustTraitTargetKey{
			repo: repoPrefix, crateRoot: crateRoot, path: candidate,
		}]
		if !ok || entry.ambiguous {
			return ""
		}
		return entry.id
	}
	if strings.HasPrefix(path, "crate::") {
		return lookupExact(path)
	}
	if strings.HasPrefix(path, "self::") {
		candidate := "crate::" + strings.TrimPrefix(path, "self::")
		if module != "" {
			candidate = "crate::" + module + "::" + strings.TrimPrefix(path, "self::")
		}
		return lookupExact(candidate)
	}
	if strings.HasPrefix(path, "super::") {
		parts := splitRustModulePath(module)
		rest := path
		for strings.HasPrefix(rest, "super::") {
			if len(parts) == 0 {
				return ""
			}
			parts = parts[:len(parts)-1]
			rest = strings.TrimPrefix(rest, "super::")
		}
		candidateParts := append([]string{"crate"}, parts...)
		candidateParts = append(candidateParts, rest)
		return lookupExact(strings.Join(candidateParts, "::"))
	}
	if strings.Contains(path, "::") {
		// A qualified non-crate path may name an internal crate module or an
		// external crate. Bind only when the full internal path is proven.
		return lookupExact("crate::" + path)
	}
	if module != "" {
		if id := lookupExact("crate::" + module + "::" + path); id != "" {
			return id
		}
	} else if id := lookupExact("crate::" + path); id != "" {
		return id
	}
	entry, ok := idx.basename[rustTraitTargetKey{
		repo: repoPrefix, crateRoot: crateRoot, path: path,
	}]
	if !ok || entry.ambiguous {
		return ""
	}
	return entry.id
}

func normalizeRustTraitPath(raw string) string {
	raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "unresolved::extends::"))
	if raw == "" || strings.HasPrefix(raw, "?") || strings.HasPrefix(raw, "'") {
		return ""
	}
	raw = stripRustBoundPrefix(raw)
	if raw == "" || strings.HasPrefix(raw, "?") || strings.HasPrefix(raw, "'") {
		return ""
	}
	return rustTraitPathHead(raw)
}

func normalizeRustTraitIdentifier(name string) string {
	name = strings.TrimPrefix(strings.TrimSpace(name), "r#")
	if rustTraitPathHead(name) != name {
		return ""
	}
	return name
}

func rustTraitCrateAndModule(filePath string) (string, string) {
	path := strings.Trim(strings.ReplaceAll(filePath, "\\", "/"), "/")
	marker := "/src/"
	crateRoot := ""
	rel := path
	if i := strings.LastIndex(path, marker); i >= 0 {
		crateRoot = path[:i]
		rel = path[i+len(marker):]
	} else if strings.HasPrefix(path, "src/") {
		rel = strings.TrimPrefix(path, "src/")
	}
	rel = strings.TrimSuffix(rel, ".rs")
	parts := splitRustModulePath(rel)
	if len(parts) > 0 {
		switch parts[len(parts)-1] {
		case "lib", "main", "mod":
			parts = parts[:len(parts)-1]
		}
	}
	return crateRoot, strings.Join(parts, "::")
}

func splitRustModulePath(path string) []string {
	path = strings.Trim(path, "/:")
	if path == "" {
		return nil
	}
	var parts []string
	for _, part := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == ':' }) {
		part = normalizeRustTraitIdentifier(part)
		if part == "" {
			return nil
		}
		parts = append(parts, part)
	}
	return parts
}

func (idx *rustTraitTargetIndex) ownerAliases(id string) []string {
	if idx == nil {
		return nil
	}
	node, ok := idx.nodes[id]
	if !ok {
		return nil
	}
	crateRoot, module := rustTraitCrateAndModule(node.FilePath)
	name := normalizeRustTraitIdentifier(node.Name)
	if name == "" {
		return nil
	}
	full := "crate::" + name
	if module != "" {
		full = "crate::" + module + "::" + name
	}
	aliases := []string{full, strings.TrimPrefix(full, "crate::")}
	base := idx.basename[rustTraitTargetKey{repo: node.RepoPrefix, crateRoot: crateRoot, path: name}]
	baseUnique := !base.ambiguous && base.id == id
	if qual := normalizeRustTraitPath(node.QualName); qual != "" && (strings.Contains(qual, "::") || baseUnique) {
		aliases = append(aliases, qual)
	}
	if baseUnique {
		aliases = append(aliases, name)
	}
	sort.Strings(aliases)
	return dedupeRustStrings(aliases)
}

func inheritRustTraitMethods(g graph.Store, scope *rustScopeIndex, targets *rustTraitTargetIndex) {
	if g == nil || scope == nil || targets == nil {
		return
	}

	methodByID := make(map[string]*graph.Node)
	for method := range g.NodesByKind(graph.KindMethod) {
		if method == nil || method.Language != "rust" || method.Meta == nil {
			continue
		}
		if traitDecl, _ := method.Meta["trait_decl"].(string); traitDecl != "true" {
			continue
		}
		methodByID[method.ID] = method
	}

	directByTrait := make(map[string][]*graph.Node)
	assigned := make(map[string]bool)
	for edge := range graph.EdgesLightSeq(g, graph.EdgeMemberOf) {
		if edge == nil || !targets.has(edge.To) {
			continue
		}
		method := methodByID[edge.From]
		if method == nil {
			continue
		}
		directByTrait[edge.To] = append(directByTrait[edge.To], method)
		assigned[method.ID] = true
	}
	for id, method := range methodByID {
		if assigned[id] {
			continue
		}
		owner := nodeReceiverType(method)
		traitID := targets.resolve(method, owner)
		if traitID == "" {
			continue
		}
		directByTrait[traitID] = append(directByTrait[traitID], method)
	}
	for id, methods := range directByTrait {
		directByTrait[id] = mergeRustTraitMethods(nil, methods)
	}

	supers := make(map[string][]string)
	for edge := range graph.EdgesLightSeq(g, graph.EdgeExtends) {
		if edge == nil || edge.From == edge.To || !targets.has(edge.From) || !targets.has(edge.To) {
			continue
		}
		supers[edge.From] = append(supers[edge.From], edge.To)
	}
	for child, parents := range supers {
		sort.Strings(parents)
		supers[child] = dedupeRustStrings(parents)
	}

	traitIDs := make([]string, 0, len(targets.nodes))
	for id := range targets.nodes {
		traitIDs = append(traitIDs, id)
	}
	sort.Strings(traitIDs)
	for _, childID := range traitIDs {
		methods := mergeRustTraitMethods(nil, directByTrait[childID])
		for _, parentID := range rustInheritedTraitIDs(childID, supers) {
			methods = mergeRustTraitMethods(methods, directByTrait[parentID])
		}
		scope.traitMethodsByID[childID] = methods

		child := targets.nodes[childID]
		for _, owner := range targets.ownerAliases(childID) {
			key := rustOwnerKey{repo: child.RepoPrefix, owner: owner}
			scope.methodsByOwner[key] = mergeRustTraitMethods(scope.methodsByOwner[key], methods)
		}
	}
}

func mergeRustTraitMethods(dst, src []*graph.Node) []*graph.Node {
	seen := make(map[string]struct{}, len(dst)+len(src))
	out := make([]*graph.Node, 0, len(dst)+len(src))
	for _, methods := range [][]*graph.Node{dst, src} {
		for _, method := range methods {
			if method == nil || method.ID == "" {
				continue
			}
			if _, ok := seen[method.ID]; ok {
				continue
			}
			seen[method.ID] = struct{}{}
			out = append(out, method)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func rustInheritedTraitIDs(child string, supers map[string][]string) []string {
	seen := map[string]bool{child: true}
	var inherited []string
	var visit func(string, int)
	visit = func(current string, depth int) {
		if depth >= rustTraitSuperDepthLimit {
			return
		}
		for _, parent := range supers[current] {
			if seen[parent] {
				continue
			}
			seen[parent] = true
			inherited = append(inherited, parent)
			visit(parent, depth+1)
		}
	}
	visit(child, 0)
	sort.Strings(inherited)
	return inherited
}

func dedupeRustStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func rustInheritedTraitOwners(child rustOwnerKey, supers map[rustOwnerKey][]rustOwnerKey) []rustOwnerKey {
	seen := map[rustOwnerKey]bool{child: true}
	var inherited []rustOwnerKey
	var visit func(rustOwnerKey, int)
	visit = func(current rustOwnerKey, depth int) {
		if depth >= rustTraitSuperDepthLimit {
			return
		}
		for _, parent := range supers[current] {
			if seen[parent] {
				continue
			}
			seen[parent] = true
			inherited = append(inherited, parent)
			visit(parent, depth+1)
		}
	}
	visit(child, 0)
	sortRustOwnerKeys(inherited)
	return inherited
}

func sortRustOwnerKeys(keys []rustOwnerKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].repo != keys[j].repo {
			return keys[i].repo < keys[j].repo
		}
		return keys[i].owner < keys[j].owner
	})
}
