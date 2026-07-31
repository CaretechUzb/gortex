package graph

import (
	"reflect"
	"sort"
	"testing"
)

// resolverProjectionStoreOnly deliberately hides optional projection
// capabilities to lock the compatibility path required by third-party Stores.
type resolverProjectionStoreOnly struct{ Store }

func collectResolverProjectionRows[T any](seq func(func(T) bool)) []T {
	var rows []T
	for row := range seq {
		rows = append(rows, row)
	}
	return rows
}

func TestResolverProjectionFallbacks(t *testing.T) {
	memory := New()
	memory.AddBatch([]*Node{
		{
			ID: "a::main.go", Kind: KindFile, Name: "main.go", FilePath: "a::main.go",
			RepoPrefix: "a", WorkspaceID: "workspace-a", Meta: map[string]any{"large": make([]byte, 4096)},
		},
		{
			ID: "a::main.go::run", Kind: KindFunction, Name: "run", FilePath: "a::main.go",
			RepoPrefix: "a", StartLine: 7, Meta: map[string]any{"large": make([]byte, 4096)},
		},
		{
			ID: "b::lib.go", Kind: KindFile, Name: "lib.go", FilePath: "b::lib.go",
			RepoPrefix: "b", WorkspaceID: "workspace-b", Meta: map[string]any{"large": make([]byte, 4096)},
		},
	}, nil)
	store := resolverProjectionStoreOnly{Store: memory}

	entries := collectResolverProjectionRows(CallableBindingNodesSeq(store, KindFunction, KindFunction, ""))
	wantEntries := []CallableBindingNode{{
		ID: "a::main.go::run", Kind: KindFunction, Name: "run", FilePath: "a::main.go",
	}}
	if !reflect.DeepEqual(entries, wantEntries) {
		t.Fatalf("index entries = %#v, want %#v", entries, wantEntries)
	}

	files := collectResolverProjectionRows(FileNodeIdentitiesSeq(store, nil))
	sort.Slice(files, func(i, j int) bool { return files[i].ID < files[j].ID })
	wantFiles := []FileNodeIdentity{
		{ID: "a::main.go", FilePath: "a::main.go", RepoPrefix: "a", WorkspaceID: "workspace-a"},
		{ID: "b::lib.go", FilePath: "b::lib.go", RepoPrefix: "b", WorkspaceID: "workspace-b"},
	}
	if !reflect.DeepEqual(files, wantFiles) {
		t.Fatalf("file identities = %#v, want %#v", files, wantFiles)
	}
	if empty := collectResolverProjectionRows(FileNodeIdentitiesSeq(store, []string{})); len(empty) != 0 {
		t.Fatalf("non-nil empty repository scope returned %d rows", len(empty))
	}
	scoped := collectResolverProjectionRows(FileNodeIdentitiesSeq(store, []string{"a", "a"}))
	if want := wantFiles[:1]; !reflect.DeepEqual(scoped, want) {
		t.Fatalf("scoped file identities = %#v, want %#v", scoped, want)
	}

	placements := NodePlacementsByIDs(store, []string{"", "a::main.go::run", "missing", "a::main.go::run"})
	wantPlacements := map[string]NodePlacement{
		"a::main.go::run": {Kind: KindFunction, FilePath: "a::main.go", RepoPrefix: "a"},
	}
	if !reflect.DeepEqual(placements, wantPlacements) {
		t.Fatalf("placements = %#v, want %#v", placements, wantPlacements)
	}
	if got := NodePlacementsByIDs(store, nil); len(got) != 0 {
		t.Fatalf("nil ID batch returned %d rows", len(got))
	}
}
