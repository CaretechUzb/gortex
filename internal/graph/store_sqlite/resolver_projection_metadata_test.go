package store_sqlite

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestResolverFixedColumnProjectionsDoNotDecodeMetadata(t *testing.T) {
	store := openResolverProjectionTestStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "repo::main.go", Kind: graph.KindFile, FilePath: "repo::main.go", RepoPrefix: "repo", Language: "go", WorkspaceID: "workspace"},
		{ID: "repo::main.go::fn", Kind: graph.KindFunction, Name: "fn", FilePath: "repo::main.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo::main.go::T", Kind: graph.KindGenericParam, Name: "T", FilePath: "repo::main.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo::main.go::Iface", Kind: graph.KindInterface, Name: "Iface", QualName: "pkg.Iface", FilePath: "repo::main.go", RepoPrefix: "repo", Language: "go"},
	}, nil)
	if _, err := store.writerDB.Exec(`UPDATE nodes SET meta = ?`, []byte{0xff, 0x00, 0x7f}); err != nil {
		t.Fatalf("corrupt residual metadata fixture: %v", err)
	}

	if got := collectResolverProjection(store.CallableBindingNodesSeq(graph.KindFunction)); len(got) != 1 || got[0].ID != "repo::main.go::fn" {
		t.Fatalf("callable projection = %#v", got)
	}
	if got := collectResolverProjection(store.FileLanguageNodesSeq()); len(got) != 1 || got[0].Language != "go" {
		t.Fatalf("file-language projection = %#v", got)
	}
	if got := collectResolverProjection(store.RepoNodeIdentitiesSeq([]string{"repo"}, graph.KindFunction)); len(got) != 1 || got[0].RepoPrefix != "repo" {
		t.Fatalf("repository projection = %#v", got)
	}
	if got := collectResolverProjection(store.NamedLanguageNodesSeq(graph.KindGenericParam)); len(got) != 1 || got[0].Name != "T" {
		t.Fatalf("named-language projection = %#v", got)
	}
	if got := collectResolverProjection(store.QualifiedNodeIdentitiesSeq(graph.KindInterface)); len(got) != 1 || got[0].QualName != "pkg.Iface" {
		t.Fatalf("qualified projection = %#v", got)
	}
	if got := collectResolverProjection(store.FileNodeIdentitiesSeq([]string{"repo"})); len(got) != 1 || got[0].WorkspaceID != "workspace" {
		t.Fatalf("file identity projection = %#v", got)
	}
}
