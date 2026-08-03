package store_sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// A scoped resolve drops durably-stamped terminal edges anyway; the pager
// must exclude them in SQL when asked — except stamped edges mentioning a
// scope anchor in either endpoint, which stay loadable for the consumer's
// exact anchored-to-scope re-check. Unstamped and NULL-column rows always
// flow.
func TestUnresolvedPageSkipTerminal(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "term_skip.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	addPending := func(from, to string, meta map[string]any) {
		s.AddNode(&graph.Node{ID: from, Kind: graph.KindFunction, Name: "f", FilePath: from, Language: "go"})
		s.AddEdge(&graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: from, Line: 1, Meta: meta})
	}
	terminal := map[string]any{"resolve_terminal": true, "resolve_terminal_reason": "no_definition"}

	addPending("repo-a/a.go::A", "unresolved::PlainPending", nil)
	addPending("repo-a/b.go::B", "unresolved::StampedBare", terminal)
	addPending("repo-b/c.go::C", "unresolved::StampedInScopeSource", terminal)
	addPending("repo-a/d.go::D", "repo-b::unresolved::Target", terminal)

	collect := func(scan graph.UnresolvedEdgeScan) map[string]bool {
		got := make(map[string]bool)
		var afterID int64
		for {
			page, err := s.ReadUnresolvedEdgePage(context.Background(), scan, afterID, 64, 1<<20)
			require.NoError(t, err)
			for _, e := range page.Edges {
				got[e.To] = true
			}
			if page.Exhausted {
				return got
			}
			afterID = page.NextID
		}
	}

	scan, err := s.BeginUnresolvedEdgeScan(context.Background())
	require.NoError(t, err)

	all := collect(scan)
	require.Len(t, all, 4, "an unfiltered scan returns every pending edge")

	scan.SkipTerminal = true
	scan.ScopeAnchors = []string{"repo-b"}
	skipped := collect(scan)
	require.True(t, skipped["unresolved::PlainPending"], "unstamped edges always flow")
	require.False(t, skipped["unresolved::StampedBare"], "a bare stamped edge is excluded in SQL")
	require.True(t, skipped["unresolved::StampedInScopeSource"], "a stamped edge whose source is under a scope anchor stays loadable")
	require.True(t, skipped["repo-b::unresolved::Target"], "a stamped edge repo-qualified into a scope anchor stays loadable")
}
