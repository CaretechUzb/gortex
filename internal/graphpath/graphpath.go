// Package graphpath interprets store/graph file paths. Indexed paths are
// repoPrefix + '/' + the repo-relative path in the indexing machine's native
// separators — on Windows that is one forward slash followed by backslashes
// (`repo/dir\file`). Slash-only string logic applied to that shape sees the
// whole repo as a single directory; these helpers normalize before any
// dir/prefix reasoning. Norm is filepath.ToSlash on purpose: on POSIX a
// backslash is an ordinary filename byte, not a separator, and must survive.
package graphpath

import (
	"path/filepath"
	"strings"
)

// Norm returns the path with native separators normalized to '/'.
func Norm(p string) string {
	return filepath.ToSlash(p)
}

// HasPrefix reports whether a stored path lies under prefix, comparing both
// in normalized form so a caller-supplied '/'-joined prefix still matches the
// native spelling the store holds. An empty prefix matches everything, which
// is what the `pathPrefix != "" && ...` guard at every call site meant.
//
// The comparison stays a raw string prefix, not a segment-aware one, because
// that is the behaviour these filters already had: `internal/mcp` matches
// `internal/mcpx` here exactly as it did before normalization.
func HasPrefix(path, prefix string) bool {
	if prefix == "" {
		return true
	}
	return strings.HasPrefix(Norm(path), Norm(prefix))
}

// Dir returns the '/'-joined directory of the normalized path, "" when the
// path has no directory component.
func Dir(p string) string {
	n := Norm(p)
	if i := strings.LastIndexByte(n, '/'); i >= 0 {
		return n[:i]
	}
	return ""
}

// PrefixForms returns the spellings a stored path may use for a caller's
// path prefix: the '/'-joined form plus the repo-prefixed native form
// (`repo/` + FromSlash(rest)). Deduped — a single entry on POSIX, where the
// two spellings coincide. Callers that filter storage directly (e.g. SQL
// LIKE, where the column cannot be normalized cheaply) match against every
// form.
func PrefixForms(prefix string) []string {
	n := Norm(prefix)
	forms := []string{n}
	if repo, rest, ok := strings.Cut(n, "/"); ok && rest != "" {
		if native := repo + "/" + filepath.FromSlash(rest); native != n {
			forms = append(forms, native)
		}
	}
	return forms
}
