package mcp

import (
	"go.uber.org/zap"
)

// MigrateSymbolAnchors carries stored symbol anchors across the repo-prefix
// change, once per repo.
//
// Notes, memories, notebooks and suppressions anchor to symbol ids, and
// surfacing matches those ids against the caller's by string equality. When
// repo prefixes became unconditional every id gained one — so an anchor
// written as `internal/foo.go::Bar` stops matching `gortex/internal/foo.go::Bar`
// and the memory silently stops surfacing. There is no error to see: the
// result is an empty list that reads exactly like "nothing relevant".
//
// Runs after warmup, when the graph is populated and repo prefixes are known.
// Safe to call on every start: the sidecar marks each repo done.
func (s *Server) MigrateSymbolAnchors(logger *zap.Logger) {
	if s == nil || s.memories == nil || s.memories.sidecar == nil || s.memories.repoKey == "" {
		return
	}
	if s.graph == nil {
		return
	}
	prefixes := s.trackedRepoPrefixes()
	if len(prefixes) == 0 {
		return
	}

	rewritten, err := s.memories.sidecar.PrefixSymbolIDs(s.memories.repoKey, s.symbolAnchorRewriter(prefixes))
	if err != nil {
		if logger != nil {
			logger.Warn("mcp: could not migrate stored symbol anchors onto prefixed ids — "+
				"notes and memories anchored under the old convention will not surface until re-anchored",
				zap.Error(err))
		}
		return
	}
	if rewritten == 0 {
		return
	}
	// The managers cache their rows in memory at construction; reload so the
	// rewrite is visible without a restart.
	s.memories.reload()
	if s.notes != nil {
		s.notes.reload()
	}
	if logger != nil {
		logger.Info("mcp: migrated stored symbol anchors onto prefixed node ids",
			zap.Int("rows", rewritten))
	}
}

// symbolAnchorRewriter maps a stored anchor to its prefixed form.
//
// The rule is deliberately conservative, because this rewrites irreplaceable
// user data:
//
//   - an id that still names a live node is NEVER touched. It is already
//     correct, whatever shape it has.
//   - otherwise, exactly one tracked prefix must produce a live node. One
//     match is unambiguous; zero means the symbol is simply gone (a rename or
//     deletion, not a convention change) and several means the same
//     repo-relative path exists in more than one repo and no evidence
//     distinguishes them.
//   - anything else is left exactly as stored.
func (s *Server) symbolAnchorRewriter(prefixes []string) func(string) string {
	return func(oldID string) string {
		if oldID == "" || s.graph.GetNode(oldID) != nil {
			return ""
		}
		match := ""
		for _, p := range prefixes {
			if p == "" {
				continue
			}
			candidate := p + "/" + oldID
			if s.graph.GetNode(candidate) == nil {
				continue
			}
			if match != "" {
				return "" // ambiguous across repos
			}
			match = candidate
		}
		return match
	}
}

// trackedRepoPrefixes returns the repo prefixes this server can anchor to.
func (s *Server) trackedRepoPrefixes() []string {
	if s.multiIndexer != nil {
		return s.multiIndexer.RepoPrefixes()
	}
	if lister, ok := s.graph.(interface{ RepoPrefixes() []string }); ok {
		return lister.RepoPrefixes()
	}
	return nil
}

// reload re-reads the manager's rows from the sidecar, discarding the cache
// built at construction. Used after an out-of-band rewrite of the stored
// rows so the change is visible without a restart.
func (mm *memoryManager) reload() {
	if mm == nil || mm.sidecar == nil || mm.repoKey == "" {
		return
	}
	mm.mu.Lock()
	defer mm.mu.Unlock()
	if rows, err := mm.sidecar.LoadMemoriesRows(mm.repoKey); err == nil {
		mm.store.Entries = rows
	}
}

// reload is the notes-manager counterpart of memoryManager.reload.
func (nm *notesManager) reload() {
	if nm == nil || nm.sidecar == nil || nm.repoKey == "" {
		return
	}
	nm.mu.Lock()
	defer nm.mu.Unlock()
	if rows, err := nm.sidecar.LoadNotesRows(nm.repoKey); err == nil {
		nm.store.Entries = rows
	}
}
