package hooks

import (
	"path/filepath"
	"strings"
)

// The PreToolUse matcher is deliberately broad so terminal state can stop any
// tool, but the access policy underneath it speaks in one voice: "do not read
// or search indexed source, use the graph instead". That advice only makes
// sense where a graph exists. Two predicates below narrow the policy to the
// surface its own message describes.
//
//   - hookCallTargetsTrackedRepo: a call in a checkout the daemon has never
//     indexed has nothing to redirect to. Worse, the grep/find redirect probes
//     search_symbols workspace-wide, so a pattern that happens to exist in
//     some *other* tracked repo used to hard-deny a search of an untracked one.
//   - searchRestrictedToNonSource: a search explicitly narrowed to docs or CI
//     config is not a search of indexed source, and neither `search_symbols`
//     nor `search_text` over the graph answers it.

// hookCallTargetsTrackedRepo reports whether a host tool call plausibly
// touches a repo the daemon tracks.
//
// It answers false only when the call can be *proven* to live outside every
// tracked repo. An unknown tracked-repo list (daemon down, or its status call
// timed out and came back empty), an unresolvable relative path with no cwd,
// and a Gortex MCP call all return true, which leaves the historical posture
// in place — this gate removes false fires, it does not add new silence where
// nothing was established.
func hookCallTargetsTrackedRepo(toolName string, toolInput map[string]any, cwd string) bool {
	if len(hookTrackedReposFn()) == 0 {
		return true
	}
	// An explicit path target overrides cwd: an agent working in one repo can
	// legitimately name a file in another, and it is the file's repo — not the
	// shell's — that decides whether the graph has an answer.
	if target, named := hookScopeTargetPath(toolName, toolInput); named {
		if abs, ok := hookAbsScopePath(target, cwd); ok {
			_, tracked := trackedRepoForPath(abs)
			return tracked
		}
	}
	abs, ok := hookAbsScopePath(cwd, "")
	if !ok {
		return true
	}
	_, tracked := trackedRepoForPath(abs)
	return tracked
}

// hookScopeTargetPath returns the single path a tool call names, when it names
// one. Bash and Task carry no single target — their scope is the cwd they run
// in — and a search with no `path` searches the cwd, so both fall through to
// the caller's cwd check.
func hookScopeTargetPath(toolName string, toolInput map[string]any) (string, bool) {
	key := ""
	switch toolName {
	case "Read", "Edit", "Write":
		key = "file_path"
	case "Grep", "Glob":
		key = "path"
	default:
		return "", false
	}
	value, _ := toolInput[key].(string)
	value = strings.TrimSpace(value)
	return value, value != ""
}

// hookAbsScopePath absolutises p against cwd. ok=false when there is nothing
// to resolve — an empty path, or a relative path with no absolute cwd to
// anchor it. Deliberately symlink-naive, matching repoRootForFile: a symlinked
// worktree resolves to no repo, and the caller treats "no repo" as unproven
// rather than as proof of being outside.
func hookAbsScopePath(p, cwd string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", false
	}
	if !filepath.IsAbs(p) {
		cwd = strings.TrimSpace(cwd)
		if !filepath.IsAbs(cwd) {
			return "", false
		}
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p), true
}

// nonSourceSearchTypes are the ripgrep type names a Grep can be narrowed to
// that name no indexed source. Deliberately an allowlist of *non*-source
// types rather than a check against the source list: an unrecognised type
// stays unproven and keeps the historical posture, so a new language's type
// name is never silently exempted from the policy.
var nonSourceSearchTypes = map[string]bool{
	"md": true, "markdown": true,
	"yaml": true, "yml": true,
	"json": true, "toml": true, "ini": true, "config": true,
	"txt": true, "text": true, "log": true,
	"xml": true, "csv": true, "tsv": true,
	"lock": true, "license": true, "readme": true, "svg": true,
}

// searchRestrictedToNonSource reports whether a Grep's own filters confine it
// to files the indexer does not treat as source. `type:"yaml"` and
// `glob:"**/*.md"` are the two shapes agents use to search CI config and docs;
// answering either with "use search_symbols" points at a graph that has
// nothing to say.
//
// A brace list restricts only when *every* alternative is non-source, and a
// glob with no extension at all (`internal/**`, `Makefile`) restricts nothing.
func searchRestrictedToNonSource(toolInput map[string]any) bool {
	fileType, _ := toolInput["type"].(string)
	if nonSourceSearchTypes[strings.ToLower(strings.TrimSpace(fileType))] {
		return true
	}

	glob, _ := toolInput["glob"].(string)
	glob = strings.TrimSpace(glob)
	if glob == "" {
		return false
	}
	alternatives := globAlternatives(glob)
	if len(alternatives) == 0 {
		return false
	}
	for _, alternative := range alternatives {
		if filepath.Ext(alternative) == "" || looksLikeSourceFile(alternative) {
			return false
		}
	}
	return true
}

// globAlternatives expands a single brace list into one glob per alternative:
// `**/*.{md,txt}` becomes `**/*.md` and `**/*.txt`. Anything more elaborate
// (nested or multiple brace groups) returns the pattern unexpanded, which the
// caller then judges as a whole — an unexpanded `{a,b}` has no extension and
// so restricts nothing.
func globAlternatives(glob string) []string {
	open := strings.Index(glob, "{")
	if open < 0 {
		return []string{glob}
	}
	closed := strings.Index(glob[open:], "}")
	if closed < 0 {
		return []string{glob}
	}
	closed += open
	prefix, suffix := glob[:open], glob[closed+1:]
	if strings.ContainsAny(prefix+suffix, "{}") {
		return []string{glob}
	}
	var out []string
	for _, alternative := range strings.Split(glob[open+1:closed], ",") {
		alternative = strings.TrimSpace(alternative)
		if alternative == "" {
			continue
		}
		out = append(out, prefix+alternative+suffix)
	}
	return out
}
