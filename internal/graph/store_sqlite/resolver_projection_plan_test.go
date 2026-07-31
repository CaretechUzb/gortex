package store_sqlite

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestResolverScopedProjectionMergesAcrossPageBoundaries(t *testing.T) {
	store := openResolverProjectionTestStore(t)
	rowCount := resolverProjectionPageSize*2 + 2
	nodes := make([]*graph.Node, 0, rowCount)
	want := make([]string, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		id := fmt.Sprintf("id-%04d", i)
		prefix := "a"
		if i%2 != 0 {
			prefix = "b"
		}
		nodes = append(nodes, &graph.Node{
			ID: id, Kind: graph.KindFile, FilePath: id, RepoPrefix: prefix,
		})
		want = append(want, id)
	}
	store.AddBatch(nodes, nil)

	var got []string
	for row := range store.FileNodeIdentitiesSeq([]string{"b", "a", "b"}) {
		got = append(got, row.ID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scoped page IDs = %v, want %v", got, want)
	}
}

func TestResolverScopedProjectionQueriesUseRepoKindIndex(t *testing.T) {
	store := openResolverProjectionTestStore(t)
	tests := []struct {
		name  string
		query string
		args  []any
	}{
		{
			name:  "high water",
			query: `SELECT id FROM nodes WHERE repo_prefix = ? AND kind = ? ORDER BY id DESC LIMIT 1`,
			args:  []any{"repo", graph.KindFile},
		},
		{
			name:  "first page",
			query: `SELECT id, file_path, repo_prefix, workspace_id FROM nodes WHERE repo_prefix = ? AND kind = ? AND id <= ? ORDER BY id LIMIT ?`,
			args:  []any{"repo", graph.KindFile, "repo::z", resolverProjectionPageSize},
		},
		{
			name:  "next page",
			query: `SELECT id, file_path, repo_prefix, workspace_id FROM nodes WHERE repo_prefix = ? AND kind = ? AND id > ? AND id <= ? ORDER BY id LIMIT ?`,
			args:  []any{"repo", graph.KindFile, "repo::a", "repo::z", resolverProjectionPageSize},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := store.db.Query(`EXPLAIN QUERY PLAN `+tt.query, tt.args...)
			if err != nil {
				t.Fatalf("explain query: %v", err)
			}
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					_ = rows.Close()
					t.Fatalf("scan query plan: %v", err)
				}
				details = append(details, detail)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				t.Fatalf("query plan rows: %v", err)
			}
			if err := rows.Close(); err != nil {
				t.Fatalf("close query plan: %v", err)
			}
			plan := strings.Join(details, "\n")
			if !strings.Contains(plan, "nodes_by_repo_kind") {
				t.Fatalf("query plan does not use nodes_by_repo_kind:\n%s", plan)
			}
			if strings.Contains(plan, "USE TEMP B-TREE") || strings.Contains(plan, "SCAN nodes") {
				t.Fatalf("query plan is not an indexed keyset seek:\n%s", plan)
			}
		})
	}
}
