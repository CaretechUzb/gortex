package mcp

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/trigram"
)

// enrichedTextMatch is a trigram literal-search hit decorated with the
// graph symbol that encloses the matching line. symbol_id /
// symbol_name are empty for a match in a file-level region with no
// enclosing function / method / type.
type enrichedTextMatch struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Text       string `json:"text"`
	SymbolID   string `json:"symbol_id,omitempty"`
	SymbolName string `json:"symbol_name,omitempty"`
}

// handleSearchText runs a trigram-accelerated literal code search
// across the indexed repository -- the alt grep backbone. A trigram
// index narrows the file set, then each candidate is scanned to
// confirm the match, so a repo-wide substring search costs roughly
// the size of the matching files rather than the whole tree.
//
// Each hit is enriched with the enclosing graph symbol so an agent
// can see *which function / method* a literal match landed in
// without a follow-up get_symbol call.
func (s *Server) handleSearchText(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultError("search_text: query is required"), nil
	}
	if s.indexer == nil && s.multiIndexer == nil {
		return mcp.NewToolResultError("search_text: no indexer available"), nil
	}
	resolved, errResult := s.resolveScope(ctx, req, IntentLocate)
	if errResult != nil {
		return errResult, nil
	}

	limit := req.GetInt("limit", 100)
	if limit < 1 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	// Multi-repo mode: the daemon owns a MultiIndexer and the per-repo
	// Indexer pointer (s.indexer) is unset or empty-rooted. Fan out
	// across every tracked repo's trigram searcher and stamp repo
	// prefixes on the match paths so downstream tooling sees the same
	// shape graph nodes use. Single-indexer callers (one-shot CLI,
	// tests) fall through to the legacy path.
	//
	// regexp mode runs the same trigram-accelerated backbone through a
	// compiled regular expression instead of a literal substring; a
	// bad pattern surfaces as a tool error rather than zero hits, and
	// the results flow through the identical enclosing-symbol
	// enrichment so callers get the same shape either way.
	useRegexp := req.GetBool("regexp", false)
	pathFilter := s.resolvePathFilter(req, fieldQuery{})
	scopedMultiGrep := s.multiIndexer != nil && (resolved.RepoAllow != nil || len(pathFilter) > 0)
	var matches []trigram.Match
	needsFinalLimit := false
	if useRegexp {
		var err error
		if scopedMultiGrep {
			matches, err = s.multiIndexer.GrepRegexpForRepos(query, "", resolved.RepoAllow, limit)
			needsFinalLimit = true
		} else if s.multiIndexer != nil {
			matches, err = s.multiIndexer.GrepRegexp(query, "", limit)
		} else {
			matches, err = s.indexer.GrepRegexp(query, "", limit)
		}
		if err != nil {
			return mcp.NewToolResultError("search_text: invalid regexp: " + err.Error()), nil
		}
	} else if scopedMultiGrep {
		matches = s.multiIndexer.GrepTextForRepos(query, resolved.RepoAllow, limit)
		needsFinalLimit = true
	} else if s.multiIndexer != nil {
		matches = s.multiIndexer.GrepText(query, limit)
	} else {
		matches = s.indexer.GrepText(query, limit)
	}

	// Sub-path scoping: a `path` argument or a `scope:`-named saved
	// scope's paths narrow the literal hits to a monorepo service
	// slice. In multi-repo mode MultiIndexer.GrepText stamps a repo
	// prefix onto every match path, so the repo-relative filter is
	// expanded with the tracked repo prefixes before the anchored test.
	if len(pathFilter) > 0 {
		var repoPrefixes []string
		if s.multiIndexer != nil {
			repoPrefixes = s.multiIndexer.RepoPrefixes()
		}
		matches = filterTextMatchesByPath(matches, pathFilter, repoPrefixes)
	}
	matches = s.filterTextMatchesByResolvedScope(matches, resolved)
	if needsFinalLimit {
		matches = limitTextMatches(matches, limit)
	}

	enriched := s.enrichTextMatches(matches)
	s.captureLocalizationSearchText(ctx, enriched)
	resp := map[string]any{
		"query":   query,
		"matches": enriched,
		"count":   len(enriched),
	}
	// Body-visible disclosure for a repo-narrowed zero (the _meta scope
	// fields are invisible in CLI output and most clients). No recheck
	// here — the note still names the scope and the widen escape hatch.
	if len(enriched) == 0 && len(resolved.RepoAllow) > 0 {
		resp["scope_note"] = scopeZeroNote(resolved, -1)
	}
	return s.respondScopedJSONOrTOON(ctx, req, resp, resolved)
}

// filterTextMatchesByPath keeps only the trigram matches whose file
// path sits under one of the anchored sub-path prefixes. repoPrefixes
// carries the tracked repo prefixes (empty in single-repo mode) so a
// repo-relative filter still matches the repo-prefixed paths that
// MultiIndexer.GrepText stamps onto matches in multi-repo mode.
func filterTextMatchesByPath(matches []trigram.Match, paths, repoPrefixes []string) []trigram.Match {
	norm := normalizePathPrefixes(paths)
	if len(norm) == 0 {
		return matches
	}
	prefixes := expandPathPrefixesWithRepos(norm, repoPrefixes)
	out := make([]trigram.Match, 0, len(matches))
	for _, m := range matches {
		if pathMatchesAnyPrefix(m.Path, prefixes) {
			out = append(out, m)
		}
	}
	return out
}

func limitTextMatches(matches []trigram.Match, limit int) []trigram.Match {
	if limit > 0 && len(matches) > limit {
		return matches[:limit]
	}
	return matches
}

func (s *Server) filterTextMatchesByResolvedScope(matches []trigram.Match, resolved ResolvedScope) []trigram.Match {
	if resolved.WorkspaceID == "" && resolved.ProjectID == "" && len(resolved.RepoAllow) == 0 {
		return matches
	}
	opts := query.QueryOptions{
		WorkspaceID: resolved.WorkspaceID,
		ProjectID:   resolved.ProjectID,
		RepoAllow:   resolved.RepoAllow,
	}
	out := make([]trigram.Match, 0, len(matches))
	for _, m := range matches {
		repo, _, ok := strings.Cut(m.Path, "/")
		// Repo allow-set fast path: a match whose stamped repo prefix
		// is outside the allow-set is dropped outright. Only applies
		// when the first path segment actually names a tracked repo —
		// on an unstamped (standalone-indexer) path the first segment
		// is an ordinary directory and must not be mistaken for a repo
		// prefix. The node-attribution check below stays authoritative
		// either way.
		knownRepo := ok && repo != "" && s.multiIndexer != nil && s.multiIndexer.GetMetadata(repo) != nil
		if len(resolved.RepoAllow) > 0 && knownRepo && !resolved.RepoAllow[repo] {
			continue
		}
		// Fail CLOSED under active narrowing: keep a match only when it
		// can be positively attributed to an in-scope graph node. A match
		// whose path resolves to no node (graph unavailable, or a file the
		// graph never turned into a node) cannot be proven in-scope, so
		// dropping it is the safe choice — keeping it was a latent
		// cross-scope leak.
		if s.graph == nil {
			continue
		}
		n := s.graph.GetNode(m.Path)
		if n == nil {
			// A trigram match path is always forward-slash, but node IDs
			// keep the repo-relative remainder in the OS separator. The two
			// spellings agree only for a file at the repo root, so on
			// Windows every match below the root failed attribution here and
			// the fail-closed drop below emptied the entire result set.
			if key := graphMatchPathKey(m.Path, knownRepo); key != m.Path {
				n = s.graph.GetNode(key)
			}
		}
		// GrepTextForRepos stamps the registry prefix onto every match path,
		// and graph file keys carry that same prefix, so the two agree by
		// construction. They used to diverge for a lone repo — which minted
		// unprefixed node IDs — and attribution needed a provenance-gated
		// retry with the prefix stripped, or it fail-closed-dropped every
		// match in a solo daemon.
		if n == nil || !opts.ScopeAllows(n) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// graphMatchPathKey spells a trigram match path the way graph node IDs
// spell it. Match paths are always forward-slash; a node ID joins the repo
// prefix with "/" but keeps the repo-relative remainder in the OS separator,
// so the two forms diverge on Windows for every file below the repo root.
// repoPrefixed says whether the first segment names a tracked repo rather
// than an ordinary directory. Returns path unchanged where the separators
// already agree, so POSIX callers pay nothing.
func graphMatchPathKey(path string, repoPrefixed bool) string {
	if filepath.Separator == '/' {
		return path
	}
	if repoPrefixed {
		if repo, rest, ok := strings.Cut(path, "/"); ok {
			return repo + "/" + filepath.FromSlash(rest)
		}
	}
	return filepath.FromSlash(path)
}

// enrichTextMatches decorates every trigram match with its enclosing
// graph symbol. It builds one per-file symbol index for the set of
// matched files, then resolves each match's line through it.
//
// The index is queried under both path spellings: file nodes are fetched
// and keyed by the graph's own FilePath, which on Windows is not the
// forward-slash path the match carries.
func (s *Server) enrichTextMatches(matches []trigram.Match) []enrichedTextMatch {
	out := make([]enrichedTextMatch, 0, len(matches))
	paths := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		paths[m.Path] = struct{}{}
		if key := graphMatchPathKey(m.Path, true); key != m.Path {
			paths[key] = struct{}{}
		}
	}
	idx := s.buildFileSymbolIndexForPaths(paths)
	for _, m := range matches {
		em := enrichedTextMatch{Path: m.Path, Line: m.Line, Text: m.Text}
		fi := idx[m.Path]
		if fi == nil {
			if key := graphMatchPathKey(m.Path, true); key != m.Path {
				fi = idx[key]
			}
		}
		if fi != nil {
			em.SymbolID, em.SymbolName = fi.find(m.Line)
		}
		out = append(out, em)
	}
	return out
}
