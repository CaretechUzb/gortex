package mcp

import (
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// fieldQuery is a search query split into its free-text component and
// any field-qualified clauses (kind: / lang: / path: / repo: /
// project:) lifted out of the raw query string.
type fieldQuery struct {
	Text    string // residual free text after clauses are removed
	Kind    string // kind: clause — comma-separated node kinds
	Flavor  string // flavor: clause — comma-separated structural flavors
	Lang    string // lang: / language: clause — node language
	Path    string // path: clause — file-path substring
	Repo    string // repo: clause — repository prefix
	Project string // project: clause — project slug
	Name    string // name: clause — symbol-name substring post-filter
}

// hasFieldFilters reports whether any post-filter clause (kind / lang
// / path / repo) was supplied. project: is excluded — it merges into
// the query scope rather than acting as a post-filter.
func (fq fieldQuery) hasFieldFilters() bool {
	repo := strings.TrimSpace(fq.Repo)
	return fq.Kind != "" || fq.Flavor != "" || fq.Lang != "" || fq.Path != "" || (repo != "" && repo != "*") || fq.Name != ""
}

// parseFieldQuery splits a raw search string into its free text and
// field-qualified clauses. A whitespace-delimited token of the form
// `field:value` is lifted into a clause when `field` is one of the
// recognised names (kind, lang/language, path, repo, project) and the
// value is non-empty; every other token — including identifiers that
// merely contain a colon, such as `pkg::Type` or a URL — stays in the
// free text verbatim. Field names are case-insensitive; a field that
// appears more than once keeps the last value.
func parseFieldQuery(raw string) fieldQuery {
	var fq fieldQuery
	var text []string
	for _, tok := range strings.Fields(raw) {
		name, value, ok := strings.Cut(tok, ":")
		if !ok || value == "" {
			text = append(text, tok)
			continue
		}
		switch strings.ToLower(name) {
		case "kind":
			fq.Kind = value
		case "flavor":
			fq.Flavor = value
		case "lang", "language":
			fq.Lang = value
		case "path":
			fq.Path = value
		case "repo":
			fq.Repo = value
		case "project":
			fq.Project = value
		case "name":
			fq.Name = value
		default:
			text = append(text, tok)
		}
	}
	fq.Text = strings.Join(text, " ")
	return fq
}

// requestWithInlineScopeClauses returns a request whose repo/project args
// include inline field-query scope clauses when the corresponding explicit
// request arg is absent. The original request is left untouched.
func requestWithInlineScopeClauses(req mcp.CallToolRequest, fq fieldQuery) mcp.CallToolRequest {
	inlineRepo := strings.TrimSpace(fq.Repo)
	inlineProject := strings.TrimSpace(fq.Project)
	if inlineRepo == "" && inlineProject == "" {
		return req
	}

	repoArg := strings.TrimSpace(req.GetString("repo", ""))
	projectArg := strings.TrimSpace(req.GetString("project", ""))
	if (repoArg != "" || inlineRepo == "") && (projectArg != "" || inlineProject == "") {
		return req
	}

	merged := req
	args := make(map[string]any, len(req.GetArguments())+2)
	for k, v := range req.GetArguments() {
		args[k] = v
	}
	if repoArg == "" && inlineRepo != "" {
		args["repo"] = inlineRepo
	}
	if projectArg == "" && inlineProject != "" {
		args["project"] = inlineProject
	}
	merged.Params.Arguments = args
	return merged
}

// normalizeLang folds the common short language aliases (ts, js, py,
// …) onto the canonical language names the indexer stamps on nodes.
// An unrecognised value is returned lowercased and trimmed.
func normalizeLang(l string) string {
	switch v := strings.ToLower(strings.TrimSpace(l)); v {
	case "ts":
		return "typescript"
	case "js":
		return "javascript"
	case "py":
		return "python"
	case "rb":
		return "ruby"
	case "rs":
		return "rust"
	case "kt":
		return "kotlin"
	case "yml":
		return "yaml"
	default:
		return v
	}
}

// applyFieldFilters narrows a node slice by the lang / path / repo
// clauses of a field query. The kind clause is applied separately via
// filterNodesByKind so it can merge with the explicit kind argument.
// Language matching is exact (after alias folding); path matches as a
// case-insensitive substring of the node's file path; repo matches
// the node's repository prefix exactly. Empty clauses are skipped, and
// a node with no repo prefix is never dropped by a repo clause —
// mirroring repoNarrowAdmits. Such a node is owned by no repository (a
// synthetic global external), so a repo clause can only exclude it
// vacuously.
func applyFieldFilters(nodes []*graph.Node, fq fieldQuery) []*graph.Node {
	lang := normalizeLang(fq.Lang)
	path := strings.ToLower(strings.TrimSpace(fq.Path))
	repo := strings.TrimSpace(fq.Repo)
	if repo == "*" {
		repo = ""
	}
	name := strings.ToLower(strings.TrimSpace(fq.Name))
	if lang == "" && path == "" && repo == "" && name == "" {
		return nodes
	}
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if lang != "" && strings.ToLower(n.Language) != lang {
			continue
		}
		if path != "" && !strings.Contains(strings.ToLower(n.FilePath), path) {
			continue
		}
		if repo != "" && n.RepoPrefix != "" && n.RepoPrefix != repo {
			continue
		}
		// name: is a case-insensitive substring post-filter on the symbol's
		// own name — narrows "search for X but only nodes whose name contains Y".
		if name != "" && !strings.Contains(strings.ToLower(n.Name), name) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// applyPathFilter narrows a node slice to those whose file path sits
// under one of the given sub-paths. Unlike the inline `path:` clause
// (a loose substring match via applyFieldFilters), this is an
// ANCHORED, slash-segment-normalised prefix test: the path
// "services/billing" matches "services/billing/invoice.go" but NOT
// "other/services/billingX/y.go" -- the prefix must align on a
// directory boundary at the start of the path.
//
// In multi-repo mode a node's FilePath is repo-prefixed
// ("<repo>/services/billing/x.go"); the repo prefix is stripped
// before matching so a sub-path is expressed relative to the repo
// root regardless of repo mode.
//
// An empty paths slice is a no-op (every node passes). A node passes
// when it matches ANY of the paths.
func applyPathFilter(nodes []*graph.Node, paths []string) []*graph.Node {
	norm := normalizePathPrefixes(paths)
	if len(norm) == 0 {
		return nodes
	}
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if pathMatchesAnyPrefix(repoRelativePath(n), norm) {
			out = append(out, n)
		}
	}
	return out
}

// repoRelativePath returns a node's file path with its repo prefix
// stripped and back-slashes normalised to forward slashes, so a
// sub-path filter is always expressed relative to the repo root.
func repoRelativePath(n *graph.Node) string {
	p := strings.ReplaceAll(n.FilePath, "\\", "/")
	if n.RepoPrefix != "" {
		p = strings.TrimPrefix(p, n.RepoPrefix+"/")
	}
	return p
}

// normalizePathPrefixes cleans a set of sub-path filters: trims
// whitespace, normalises separators, strips a leading "./" and any
// leading/trailing slashes, and drops empties and duplicates.
func normalizePathPrefixes(paths []string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, p := range paths {
		p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
		p = strings.TrimPrefix(p, "./")
		p = strings.Trim(p, "/")
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// pathMatchesAnyPrefix reports whether a repo-relative file path sits
// under any of the (already-normalised) sub-path prefixes. A prefix
// matches when the path equals it exactly or continues past it at a
// slash boundary -- so "services/billing" matches
// "services/billing/x.go" but not "services/billingX/x.go".
func pathMatchesAnyPrefix(path string, prefixes []string) bool {
	for _, pre := range prefixes {
		if path == pre {
			return true
		}
		if strings.HasPrefix(path, pre+"/") {
			return true
		}
	}
	return false
}

// expandPathPrefixesWithRepos returns the normalised sub-path prefixes
// plus, for every non-empty repo prefix, the prefix-qualified form
// (<repoPrefix>/<subpath>). In multi-repo mode trigram match paths carry
// a repo prefix (gortex/internal/...) while callers pass repo-relative
// sub-paths (internal/...); expanding the filter lets the repo-relative
// form still match without loosening the anchored, segment-boundary
// matching of pathMatchesAnyPrefix. Returns norm unchanged when the
// store reports no repo prefixes at all (the standalone Indexer, whose
// paths are unprefixed).
func expandPathPrefixesWithRepos(norm, repoPrefixes []string) []string {
	if len(repoPrefixes) == 0 {
		return norm
	}
	out := make([]string, 0, len(norm)*(1+len(repoPrefixes)))
	for _, p := range norm {
		out = append(out, p)
		for _, rp := range repoPrefixes {
			if rp = strings.Trim(rp, "/"); rp != "" {
				out = append(out, rp+"/"+p)
			}
		}
	}
	return out
}

// resolvePathFilter collects the sub-path filters that apply to a
// search request, from three additive sources:
//
//   - the explicit `path` request argument (comma-separated),
//   - the inline `path:` clause already lifted into fq.Path,
//   - the Paths of any `scope:`-named SavedScope.
//
// It is the path-scoping sibling of resolveRepoFilter -- a separate
// function rather than a signature change, so resolveRepoFilter's
// existing callers are untouched. The returned slice is the union of
// all three sources (deduplication happens downstream in
// applyPathFilter); an empty slice means "no sub-path filter".
//
// fq may be the zero fieldQuery when the caller has no inline clause.
func (s *Server) resolvePathFilter(req mcp.CallToolRequest, fq fieldQuery) []string {
	var paths []string
	for _, p := range strings.Split(req.GetString("path", ""), ",") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	if p := strings.TrimSpace(fq.Path); p != "" {
		paths = append(paths, p)
	}
	if scopeArg := strings.TrimSpace(req.GetString("scope", "")); scopeArg != "" {
		if sc, ok := s.lookupScope(scopeArg); ok {
			paths = append(paths, sc.Paths...)
		}
	}
	return paths
}

// knownFlavorValues is the closed structural-flavor vocabulary that
// producers stamp onto Meta["type_flavor"] (plus the cross-key
// "component" bridge). It is what the codegraph-compat shim consults
// to tell a flavor value apart from a real node kind.
var knownFlavorValues = map[string]struct{}{
	"class": {}, "struct": {}, "enum": {}, "interface": {}, "trait": {},
	"protocol": {}, "object": {}, "record": {}, "type_alias": {}, "newtype": {},
	"anonymous_class": {}, "component": {}, "message": {}, "service": {},
	"table": {}, "view": {}, "module": {}, "signature": {}, "type_def": {},
	"instance": {}, "hook": {}, "play": {}, "typedef": {},
}

// isKnownFlavor reports whether v (case-insensitive) is in the closed
// flavor vocabulary.
func isKnownFlavor(v string) bool {
	_, ok := knownFlavorValues[strings.ToLower(strings.TrimSpace(v))]
	return ok
}

// splitFlavors splits a comma-separated flavor argument into its
// non-empty, trimmed values (the OR set).
func splitFlavors(arg string) []string {
	var out []string
	for _, f := range strings.Split(arg, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// nodeMatchesFlavor reports whether a node matches any of the given
// flavor values (case-insensitive on both sides). The value
// "component" is a special bridging sentinel: it matches any node
// carrying a non-empty Meta["ui_component"] OR a
// Meta["type_flavor"]=="component" — so a React function component
// (KindFunction, ui_component=react, no type_flavor) and a Svelte SFC
// type (type_flavor=component) both match. Every other value is an
// exact, case-insensitive match against Meta["type_flavor"].
func nodeMatchesFlavor(n *graph.Node, flavors []string) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	tf, _ := n.Meta["type_flavor"].(string)
	uc, _ := n.Meta["ui_component"].(string)
	return flavorMatchesResolved(tf, uc, flavors)
}

// flavorMatchesResolved is the matcher core shared by nodeMatchesFlavor
// (type_flavor + ui_component read off one node) and the find_usages
// owner-resolution path (type flavor read off the FROM node's enclosing
// owner type, ui_component off the FROM node itself). Both values are
// matched case-insensitively; "component" is the cross-key bridge.
func flavorMatchesResolved(typeFlavor, uiComponent string, flavors []string) bool {
	tf := strings.ToLower(typeFlavor)
	for _, f := range flavors {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" {
			continue
		}
		if f == "component" {
			if uiComponent != "" || tf == "component" {
				return true
			}
			continue
		}
		if tf == f {
			return true
		}
	}
	return false
}

// applyFlavorFilter narrows a node slice to those whose structural
// flavor matches the comma-separated flavor argument (union / OR). An
// empty argument is a no-op.
func applyFlavorFilter(nodes []*graph.Node, flavorArg string) []*graph.Node {
	flavors := splitFlavors(flavorArg)
	if len(flavors) == 0 {
		return nodes
	}
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if nodeMatchesFlavor(n, flavors) {
			out = append(out, n)
		}
	}
	return out
}

// reclassifyKindFlavor is the codegraph-compatibility shim. codegraph
// exposes structural kinds as kind:class|struct|enum|component, but
// those are flavor values, not gortex node kinds — a bare
// filterNodesByKind on them returns empty. This splits a
// comma-separated kind argument into the values that are real node
// kinds (returned as kinds) and the values that are flavor-only
// (returned as flavors), so the caller can route each to the right
// filter. A value that is both a kind and a flavor (interface, table,
// module) keeps its node-kind meaning. Returns the original kindArg
// and an empty flavors when nothing reclassifies.
func reclassifyKindFlavor(kindArg string) (kinds string, flavors string) {
	var ks, fs []string
	for _, v := range strings.Split(kindArg, ",") {
		t := strings.TrimSpace(v)
		if t == "" {
			continue
		}
		if !graph.IsValidNodeKind(strings.ToLower(t)) && isKnownFlavor(t) {
			fs = append(fs, t)
		} else {
			ks = append(ks, t)
		}
	}
	return strings.Join(ks, ","), strings.Join(fs, ",")
}
