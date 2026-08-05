package resolver

import "strings"

// uniqueFileSuffixMatch returns the single indexed file equal to, or
// ending with `/`+path, or "" when there is none or more than one.
// filesByBase indexes candidate file IDs by basename, so the scan is
// bounded by the number of files sharing the leaf name rather than by
// the corpus.
//
// Refusing on ambiguity is the point: a path-suffix match is the net
// under a resolver that could not name the file exactly (a Lua module
// path, a Godot `res://` path), and picking arbitrarily between two
// equally-good candidates manufactures a confidently wrong edge.
func uniqueFileSuffixMatch(filesByBase map[string][]string, path string) string {
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	suffix := "/" + path
	match := ""
	for _, cand := range filesByBase[base] {
		if cand == path || strings.HasSuffix(cand, suffix) {
			if match != "" && match != cand {
				return "" // ambiguous across roots
			}
			match = cand
		}
	}
	return match
}
