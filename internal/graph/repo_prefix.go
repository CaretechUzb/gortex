package graph

import "strings"

// RepoPrefixOfID returns the leading path segment of a node ID or file
// path — the text before the first "/". In multi-repo mode that segment
// IS the repository prefix (e.g. "gortex/internal/mcp/server.go::NewServer"
// → "gortex"), so this recovers the owning repo without a node lookup.
//
// IT IS A SYNTACTIC SPLIT, NOT A LOOKUP, and it cannot tell a repo prefix
// from any other leading segment. On an UNPREFIXED id — one minted in
// single-repo mode, where nodes carry RepoPrefix == "" — it returns the
// first directory name: "internal/foo.go::Bar" yields "internal", not "".
// A caller that treats the result as a repository will then key indexes and
// scopes on a directory name that matches nothing, with no error anywhere.
//
// Only "" is returned when the id has no "/" at all, which covers the
// synthetic namespaces ("unresolved::Name", "dep::mod::Sym").
//
// Prefer the RepoPrefix field of a hydrated node whenever one is
// available; use this only where the id is all you have, and treat a
// non-empty result as unverified until it matches a known tracked prefix.
func RepoPrefixOfID(id string) string {
	if i := strings.IndexByte(id, '/'); i > 0 {
		return id[:i]
	}
	return ""
}
