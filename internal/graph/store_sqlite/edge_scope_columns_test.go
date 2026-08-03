package store_sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The generated from_repo / to_repo_unresolved columns are SQL mirrors of
// graph.RepoPrefixOfID / graph.UnresolvedRepoPrefix. This parity test is the
// drift fence: every id shape the encodings produce must classify byte-for-
// byte identically in SQLite and in Go (NULL in SQL pairs with "not an
// unresolved shape", which no Go branch distinguishes — consumers fail open
// on it).
func TestEdgeScopeColumnsMirrorGoHelpers(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "scope_cols.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	shapes := []struct{ from, to string }{
		{"repo-a/pkg/a.go::Caller", "unresolved::Bare"},
		{"repo-a/pkg/a.go::Caller", "repo-b::unresolved::Qualified"},
		{"solo.go::Caller", "unresolved::fnvalue::fn"},
		{"repo-a/pkg/a.go::Caller", "repo-c::unresolved::fnvalue::fn"},
		{"repo-a/pkg/a.go::Caller", "unresolved::repo-b/pseudo::Name"},
		{"deep/repo/x.go::F", "multi::part::unresolved::N"},
	}
	for i, sh := range shapes {
		s.AddNode(&graph.Node{ID: sh.from, Kind: graph.KindFunction, Name: "f", FilePath: sh.from, Language: "go"})
		s.AddEdge(&graph.Edge{From: sh.from, To: sh.to, Kind: graph.EdgeCalls, FilePath: sh.from, Line: i + 1})
	}

	rows, err := s.db.Query(`SELECT from_id, to_id, from_repo, to_repo_unresolved FROM edges`)
	require.NoError(t, err)
	defer rows.Close()
	checked := 0
	for rows.Next() {
		var fromID, toID, fromRepo string
		var toRepoU sql.NullString
		require.NoError(t, rows.Scan(&fromID, &toID, &fromRepo, &toRepoU))
		require.Equal(t, graph.RepoPrefixOfID(fromID), fromRepo,
			"from_repo must mirror RepoPrefixOfID for %q", fromID)
		if graph.IsUnresolvedTarget(toID) {
			require.True(t, toRepoU.Valid, "unresolved shape %q must classify", toID)
			require.Equal(t, graph.UnresolvedRepoPrefix(toID), toRepoU.String,
				"to_repo_unresolved must mirror UnresolvedRepoPrefix for %q", toID)
		}
		checked++
	}
	require.NoError(t, rows.Err())
	require.Equal(t, len(shapes), checked)
}

// The ScopeFilter pushdown must keep exactly the rows edgeInResolveScope
// keeps, modulo fail-open NULL shapes: source in scope, target bare, or
// target repo-qualified into scope.
func TestUnresolvedPageScopeFilterPushdown(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "scope_filter.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	add := func(from, to string) {
		s.AddNode(&graph.Node{ID: from, Kind: graph.KindFunction, Name: "f", FilePath: from, Language: "go"})
		s.AddEdge(&graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: from, Line: 1})
	}
	add("repo-b/x.go::InScopeSource", "repo-z::unresolved::Out") // kept: from in scope
	add("repo-a/x.go::BareTarget", "unresolved::Bare")           // kept: bare target
	add("repo-a/x.go::TargetInScope", "repo-b::unresolved::Hit") // kept: target repo in scope
	add("repo-a/x.go::Droppable", "repo-z::unresolved::Miss")    // dropped: both out of scope

	scan, err := s.BeginUnresolvedEdgeScan(context.Background())
	require.NoError(t, err)
	scan.ScopeFilter = true
	scan.ScopeAnchors = []string{"repo-b"}

	got := make(map[string]bool)
	var afterID int64
	for {
		page, err := s.ReadUnresolvedEdgePage(context.Background(), scan, afterID, 64, 1<<20)
		require.NoError(t, err)
		for _, e := range page.Edges {
			got[e.From] = true
		}
		if page.Exhausted {
			break
		}
		afterID = page.NextID
	}
	require.True(t, got["repo-b/x.go::InScopeSource"])
	require.True(t, got["repo-a/x.go::BareTarget"])
	require.True(t, got["repo-a/x.go::TargetInScope"])
	require.False(t, got["repo-a/x.go::Droppable"],
		"a row out of scope on both endpoints must be excluded in SQL")
}

// The unresolved-insertion counter may overcount but never undercount: every
// write path that can introduce an unresolved-shaped target must bump it,
// and resolved-target writes must not.
func TestUnresolvedEdgeInsertionCounter(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "uctr.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	require.Zero(t, s.UnresolvedEdgeInsertions())
	s.AddNode(&graph.Node{ID: "a.go::F", Kind: graph.KindFunction, Name: "F", FilePath: "a.go", Language: "go"})
	s.AddNode(&graph.Node{ID: "a.go::G", Kind: graph.KindFunction, Name: "G", FilePath: "a.go", Language: "go"})

	s.AddEdge(&graph.Edge{From: "a.go::F", To: "unresolved::X", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1})
	require.Equal(t, uint64(1), s.UnresolvedEdgeInsertions(), "AddEdge with unresolved target counts")

	s.AddEdge(&graph.Edge{From: "a.go::F", To: "a.go::G", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2})
	require.Equal(t, uint64(1), s.UnresolvedEdgeInsertions(), "resolved target does not count")

	s.AddBatch(nil, []*graph.Edge{
		{From: "a.go::G", To: "repo-b::unresolved::Y", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 3},
		{From: "a.go::G", To: "a.go::F", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 4},
	})
	require.Equal(t, uint64(2), s.UnresolvedEdgeInsertions(), "batch counts only unresolved targets")

	reverted := &graph.Edge{From: "a.go::F", To: "unresolved::Z", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2}
	s.ReindexEdges([]graph.EdgeReindex{{Edge: reverted, OldFrom: "a.go::F", OldTo: "a.go::G"}})
	require.Equal(t, uint64(3), s.UnresolvedEdgeInsertions(), "reindex-to-unresolved counts")
}
