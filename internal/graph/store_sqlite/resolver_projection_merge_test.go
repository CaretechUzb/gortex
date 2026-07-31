package store_sqlite

import (
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestResolverScopedProjectionMergesRepositoriesByGlobalID(t *testing.T) {
	store := openResolverProjectionTestStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "a-id", Kind: graph.KindFile, FilePath: "a-id", RepoPrefix: "z"},
		{ID: "b-id", Kind: graph.KindFile, FilePath: "b-id", RepoPrefix: "a"},
		{ID: "c-id", Kind: graph.KindFile, FilePath: "c-id", RepoPrefix: "z"},
		{ID: "d-id", Kind: graph.KindFile, FilePath: "d-id", RepoPrefix: "ignored"},
	}, nil)

	rows := collectResolverProjection(store.FileNodeIdentitiesSeq([]string{"z", "a", "z"}))
	var ids []string
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	if want := []string{"a-id", "b-id", "c-id"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("scoped IDs = %v, want %v", ids, want)
	}
}
