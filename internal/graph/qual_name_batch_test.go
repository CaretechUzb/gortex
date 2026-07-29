package graph

import "testing"

func TestGraphGetNodesByQualNamesOneShardTracksUpdatesAndEviction(t *testing.T) {
	g := newWithShardCount(1)
	if g.shardCount != 1 {
		t.Fatalf("newWithShardCount(1) created %d shards", g.shardCount)
	}

	const (
		sharedQual = "example.com/shared.Foo"
		movedQual  = "example.com/shared.MovedFoo"
	)
	first := &Node{
		ID:         "repo-a/a.go::Foo",
		Kind:       KindFunction,
		Name:       "Foo",
		QualName:   sharedQual,
		FilePath:   "repo-a/a.go",
		RepoPrefix: "repo-a",
	}
	second := &Node{
		ID:         "repo-b/b.go::Foo",
		Kind:       KindFunction,
		Name:       "Foo",
		QualName:   sharedQual,
		FilePath:   "repo-b/b.go",
		RepoPrefix: "repo-b",
	}
	g.AddNode(first)
	g.AddNode(second)

	requireQualNameBatchIDs(t, g.GetNodesByQualNames([]string{sharedQual}), sharedQual, first.ID, second.ID)

	moved := *second
	moved.QualName = movedQual
	g.AddNode(&moved)
	requireQualNameBatchIDs(t, g.GetNodesByQualNames([]string{sharedQual, movedQual}), sharedQual, first.ID)
	requireQualNameBatchIDs(t, g.GetNodesByQualNames([]string{sharedQual, movedQual}), movedQual, second.ID)

	g.EvictFile(first.FilePath)
	got := g.GetNodesByQualNames([]string{sharedQual, movedQual})
	requireQualNameBatchIDs(t, got, sharedQual)
	requireQualNameBatchIDs(t, got, movedQual, second.ID)
}

func TestOverlaidViewGetNodesByQualNamesMergesAndFiltersDuplicates(t *testing.T) {
	base := newWithShardCount(1)
	const qualName = "example.com/shared.Foo"

	hiddenBase := &Node{
		ID:         "repo-a/a.go::BaseFoo",
		Kind:       KindFunction,
		Name:       "Foo",
		QualName:   qualName,
		FilePath:   "repo-a/a.go",
		RepoPrefix: "repo-a",
	}
	visibleBase := &Node{
		ID:         "repo-b/b.go::Foo",
		Kind:       KindFunction,
		Name:       "Foo",
		QualName:   qualName,
		FilePath:   "repo-b/b.go",
		RepoPrefix: "repo-b",
	}
	base.AddNode(hiddenBase)
	base.AddNode(visibleBase)

	layer := NewOverlayLayer()
	layer.MarkFile("repo-a/a.go", false)
	overlayReplacement := &Node{
		ID:         "repo-a/a.go::OverlayFoo",
		Kind:       KindFunction,
		Name:       "Foo",
		QualName:   qualName,
		FilePath:   "repo-a/a.go",
		RepoPrefix: "repo-a",
	}
	layer.AddNode("repo-a/a.go", overlayReplacement)

	layer.MarkFile("repo-c/c.go", false)
	overlayDuplicate := &Node{
		ID:         "repo-c/c.go::Foo",
		Kind:       KindFunction,
		Name:       "Foo",
		QualName:   qualName,
		FilePath:   "repo-c/c.go",
		RepoPrefix: "repo-c",
	}
	layer.AddNode("repo-c/c.go", overlayDuplicate)

	view := NewOverlaidView(base, layer)
	got := view.GetNodesByQualNames([]string{qualName, qualName, "", "missing.QualName"})
	requireQualNameBatchIDs(t, got, qualName, overlayReplacement.ID, visibleBase.ID, overlayDuplicate.ID)
	requireQualNameBatchIDs(t, got, "")
	requireQualNameBatchIDs(t, got, "missing.QualName")
}

func requireQualNameBatchIDs(t *testing.T, got map[string][]*Node, qualName string, wantIDs ...string) {
	t.Helper()
	hits, ok := got[qualName]
	if len(wantIDs) == 0 {
		if ok {
			t.Fatalf("GetNodesByQualNames unexpectedly returned key %q with %d nodes", qualName, len(hits))
		}
		return
	}
	if !ok {
		t.Fatalf("GetNodesByQualNames omitted key %q", qualName)
	}
	if len(hits) != len(wantIDs) {
		t.Fatalf("GetNodesByQualNames[%q] returned %d nodes, want %d", qualName, len(hits), len(wantIDs))
	}
	for i, wantID := range wantIDs {
		if hits[i] == nil || hits[i].ID != wantID {
			t.Fatalf("GetNodesByQualNames[%q][%d] = %#v, want ID %q", qualName, i, hits[i], wantID)
		}
	}
}
