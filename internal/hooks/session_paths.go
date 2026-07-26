package hooks

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/daemon"
)

// sessionScope is the caller identity a briefing builder needs in order to
// decide which dirty files are this session's own work. Both fields may be
// empty — a host that supplies neither degrades to an explicitly-labeled
// whole-tree briefing, which is what the hook did before attribution existed.
type sessionScope struct {
	SessionID string
	CWD       string
}

// hookTrackedReposFn is the daemon's tracked-repo list, injectable so tests
// never dial a real daemon.
var hookTrackedReposFn = cachedTrackedRepos

func cachedTrackedRepos() []daemon.TrackedRepoStatus {
	status, err := cachedDaemonStatus()
	if err != nil || status == nil {
		return nil
	}
	return status.TrackedRepos
}

// mapString reads a string field from a decoded JSON object.
func mapString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// writeTargetKeys are the field names a write-shaped tool input uses to name
// what it rewrites. Covers the host's own editors and the Gortex MCP write
// verbs, whose payloads spell the target as a path or as a graph symbol ID.
var writeTargetKeys = []string{"file_path", "notebook_path", "path", "file", "symbol", "id"}

// collectWriteTargets appends every write-target-shaped value in m, including
// one level of `target` nesting and the `changes` / `edits` arrays that batch
// verbs carry.
func collectWriteTargets(m map[string]any, out *[]string) {
	if m == nil || len(*out) >= sessionWritePathsPerCall {
		return
	}
	for _, key := range writeTargetKeys {
		if v := mapString(m, key); v != "" {
			appendCapped(out, v)
		}
	}
	for _, key := range []string{"symbols", "ids"} {
		switch vals := m[key].(type) {
		case []any:
			for _, v := range vals {
				if s, ok := v.(string); ok {
					appendCapped(out, strings.TrimSpace(s))
				}
			}
		case string:
			// Comma-separated form, as the legacy `ids` params accept.
			for _, s := range strings.Split(vals, ",") {
				appendCapped(out, strings.TrimSpace(s))
			}
		}
	}
	if target, ok := m["target"].(map[string]any); ok {
		for _, key := range writeTargetKeys {
			if v := mapString(target, key); v != "" {
				appendCapped(out, v)
			}
		}
		if syms, ok := target["symbols"].([]any); ok {
			for _, v := range syms {
				if s, ok := v.(string); ok {
					appendCapped(out, strings.TrimSpace(s))
				}
			}
		}
	}
	for _, key := range []string{"changes", "edits"} {
		if items, ok := m[key].([]any); ok {
			for _, item := range items {
				if child, ok := item.(map[string]any); ok {
					collectWriteTargets(child, out)
				}
			}
		}
	}
}

func appendCapped(out *[]string, v string) {
	if v == "" || len(*out) >= sessionWritePathsPerCall {
		return
	}
	for _, existing := range *out {
		if existing == v {
			return
		}
	}
	*out = append(*out, v)
}

// gortexWriteVerbs are the Gortex MCP tools that mutate source — the facade
// operations plus the legacy per-tool names still reachable by name.
var gortexWriteVerbs = map[string]bool{
	"edit": true, "refactor": true,
	"edit_file": true, "write_file": true, "edit_symbol": true,
	"batch_edit": true, "rename_symbol": true, "move_symbol": true,
	"safe_delete_symbol": true, "inline_symbol": true,
	"apply_code_action": true, "fix_all_in_file": true,
}

// writeTargetPaths returns the paths or graph symbol IDs a tool call is about
// to rewrite. Returns nil for a tool that writes nothing, which is the
// no-disk-I/O fast path every read and navigation call takes.
func writeTargetPaths(toolName string, toolInput map[string]any) []string {
	var out []string
	switch toolName {
	case "Edit", "Write", "MultiEdit":
		if v := mapString(toolInput, "file_path"); v != "" {
			out = append(out, v)
		}
	case "NotebookEdit":
		if v := mapString(toolInput, "notebook_path"); v != "" {
			out = append(out, v)
		} else if v := mapString(toolInput, "file_path"); v != "" {
			out = append(out, v)
		}
	case "Bash":
		for _, w := range classifyBashWriteTargets(mapString(toolInput, "command")) {
			appendCapped(&out, w.Path)
		}
	default:
		if !isGortexMCPToolName(toolName) {
			return nil
		}
		if !gortexWriteVerbs[shortGortexToolName(toolName)] {
			return nil
		}
		collectWriteTargets(toolInput, &out)
	}
	return out
}

// recordSessionWriteTargets adds a tool call's write targets to the session's
// attribution set. Best-effort: an empty session ID, an unusable state
// directory, or a tool that writes nothing is a silent no-op — and in the
// last, overwhelmingly common case it touches no disk at all.
func recordSessionWriteTargets(input HookInput) {
	if strings.TrimSpace(input.SessionID) == "" {
		return
	}
	targets := writeTargetPaths(input.ToolName, input.ToolInput)
	if len(targets) == 0 {
		return
	}

	st := loadSessionState(input.SessionID)
	existing := make(map[string]bool, len(st.WrittenPaths))
	for _, p := range st.WrittenPaths {
		existing[p] = true
	}
	changed := false
	for _, t := range targets {
		if existing[t] {
			continue
		}
		if len(st.WrittenPaths) >= sessionWrittenPathsCap {
			// Stop appending rather than evicting the oldest: an early-turn
			// edit is precisely the one most likely to still be dirty.
			if !st.WrittenPathsTruncated {
				st.WrittenPathsTruncated = true
				changed = true
			}
			break
		}
		st.WrittenPaths = append(st.WrittenPaths, t)
		existing[t] = true
		changed = true
	}
	// Avoid rewriting the file when a session re-edits the same path.
	if !changed {
		return
	}
	saveSessionState(input.SessionID, st)
}

// trackedRepoForPath returns the tracked repo containing abs, preferring the
// longest matching root. Strictly containment-based: a path outside every
// tracked repo matches nothing.
func trackedRepoForPath(abs string) (daemon.TrackedRepoStatus, bool) {
	var best daemon.TrackedRepoStatus
	found := false
	for _, r := range hookTrackedReposFn() {
		if r.Path == "" {
			continue
		}
		if abs == r.Path || strings.HasPrefix(abs, r.Path+string(filepath.Separator)) {
			if !found || len(r.Path) > len(best.Path) {
				best, found = r, true
			}
		}
	}
	return best, found
}

// hookRepoScope resolves the working tree and graph repo prefix the Stop-time
// diff was computed against, from the hook payload's cwd. Returns ("", "")
// when the cwd belongs to no tracked repo or the daemon is unreachable —
// callers then fall back to an explicitly-labeled whole-tree briefing.
//
// Deliberately symlink-naive, matching repoRootForFile: a symlinked worktree
// resolves to no repo and degrades to the labeled fallback, which is safer
// than guessing an attribution.
func hookRepoScope(cwd string) (repoRoot, repoPrefix string) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "", ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", ""
	}
	if repo, ok := trackedRepoForPath(filepath.Clean(abs)); ok {
		return repo.Path, repo.Prefix
	}
	// The server's diffRepoScope prefers the lone tracked repo over the
	// session cwd, so when exactly one repo is tracked that is the tree it
	// diffed — match it, or the hook attributes against the wrong tree.
	if repos := hookTrackedReposFn(); len(repos) == 1 && repos[0].Path != "" {
		return repos[0].Path, repos[0].Prefix
	}
	return "", ""
}

// ownershipRepoScope resolves the tree ownership is judged against. It prefers
// what detect_changes says it diffed, and only falls back to resolving the
// hook's cwd through the daemon's tracked-repo list.
//
// Preferring the echo is not just an optimization. It is authoritative — the
// server cannot disagree with itself about which tree it diffed — and it keeps
// attribution off the daemon-status call, which returns a large payload and
// times out on a daemon tracking many repos. When that call times out the
// tracked-repo list comes back empty, and a cwd-derived scope would silently
// resolve to nothing and disable attribution altogether.
func ownershipRepoScope(cwd string, changes *changeSet) (repoRoot, repoPrefix string) {
	if changes != nil {
		root := strings.TrimSpace(changes.RepoRoot)
		// "." is handleDetectChanges' own fallback for "no repo resolved", and
		// a relative root cannot anchor the absolute captured paths.
		if root != "" && root != "." && filepath.IsAbs(root) {
			return root, strings.TrimSpace(changes.Repo)
		}
	}
	return hookRepoScope(cwd)
}

// repoRelativeForOwnership maps one captured write target into the
// repo-relative spelling detect_changes reports. Returns "" when the target
// cannot belong to the diffed repo.
//
// Captured targets are heterogeneous: the host's Edit/Write give absolute
// paths, while the Gortex MCP verbs are called with absolute, repo-relative,
// and repo-prefixed spellings in practice — plus graph symbol IDs, whose path
// part is everything before "::".
func repoRelativeForOwnership(raw, repoRoot string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return ""
	}
	if idx := strings.Index(p, "::"); idx >= 0 {
		p = p[:idx]
	}
	p = filepath.ToSlash(filepath.Clean(p))
	if p == "." || p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		if repoRoot == "" {
			return ""
		}
		rel, err := filepath.Rel(repoRoot, p)
		if err != nil {
			return ""
		}
		rel = filepath.ToSlash(rel)
		if rel == ".." || strings.HasPrefix(rel, "../") {
			return ""
		}
		return rel
	}
	if strings.HasPrefix(p, "../") {
		return ""
	}
	return p
}

// attribution is the resolved ownership view of one detect_changes result.
type attribution struct {
	// Attributed is false when the session could not be identified at all,
	// in which case the caller must report the whole tree and say so.
	Attributed   bool
	OwnedSymbols []changedSymbol
	OwnedFiles   []string
	Unattributed []string
	Capped       bool
}

// attributeChanges partitions a change set into this session's work and
// everything else. Never drops a dirty file: every entry in ChangedFiles
// lands in exactly one of OwnedFiles / Unattributed.
func attributeChanges(scope sessionScope, changes *changeSet) attribution {
	if changes == nil {
		return attribution{}
	}
	// Normalize the dirty set once. changed_files is repo-relative and never
	// repo-prefixed, whichever mode the daemon runs in.
	dirty := make(map[string]string, len(changes.ChangedFiles))
	for _, f := range changes.ChangedFiles {
		if key := filepath.ToSlash(filepath.Clean(strings.TrimSpace(f))); key != "" && key != "." {
			dirty[key] = f
		}
	}

	if strings.TrimSpace(scope.SessionID) == "" {
		return attribution{}
	}
	st := loadSessionState(scope.SessionID)
	if len(st.WrittenPaths) == 0 {
		return attribution{}
	}
	repoRoot, repoPrefix := ownershipRepoScope(scope.CWD, changes)
	if repoRoot == "" {
		return attribution{}
	}

	owned := make(map[string]bool, len(st.WrittenPaths))
	for _, raw := range st.WrittenPaths {
		rel := repoRelativeForOwnership(raw, repoRoot)
		if rel == "" {
			continue
		}
		// Two candidates, stripping only — never adding a prefix. changed_files
		// is always the unprefixed spelling, and a captured path may already
		// carry the prefix. Both candidates are tested against the same repo's
		// dirty set, so a false hit can only misattribute within the diffed
		// repo, never across repos.
		if _, ok := dirty[rel]; ok {
			owned[rel] = true
		}
		if repoPrefix != "" && strings.HasPrefix(rel, repoPrefix+"/") {
			if stripped := strings.TrimPrefix(rel, repoPrefix+"/"); dirty[stripped] != "" {
				owned[stripped] = true
			}
		}
	}

	result := attribution{Attributed: true, Capped: st.WrittenPathsTruncated}
	for key, original := range dirty {
		if owned[key] {
			result.OwnedFiles = append(result.OwnedFiles, original)
		} else {
			result.Unattributed = append(result.Unattributed, original)
		}
	}
	sort.Strings(result.OwnedFiles)
	sort.Strings(result.Unattributed)

	for _, cs := range changes.ChangedSymbols {
		if owned[symbolOwnerFile(cs, repoPrefix)] {
			result.OwnedSymbols = append(result.OwnedSymbols, cs)
		}
	}
	return result
}

// symbolOwnerFile resolves a changed symbol to the repo-relative,
// unprefixed file spelling used as the ownership key. The graph node's
// file_path carries the repo prefix in multi-repo mode; its ID has the same
// path ahead of "::" when file_path is absent.
func symbolOwnerFile(cs changedSymbol, repoPrefix string) string {
	path := strings.TrimSpace(cs.FilePath)
	if path == "" {
		if idx := strings.Index(cs.ID, "::"); idx >= 0 {
			path = cs.ID[:idx]
		}
	}
	path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	if path == "." || path == "" {
		return ""
	}
	if repoPrefix != "" {
		path = strings.TrimPrefix(path, repoPrefix+"/")
	}
	return path
}
