package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

func qualifiedLeafFixture() (exploreSyntacticAnchor, *graph.Node, *graph.Node) {
	anchor, _ := newExploreSyntacticAnchor("HipChatHandler::buildContent")
	owner := &graph.Node{
		ID: "src/HipChatHandler.php::HipChatHandler", Name: "HipChatHandler",
		Kind: graph.KindType, FilePath: "src/HipChatHandler.php", StartLine: 10,
	}
	member := &graph.Node{
		ID:   "src/HipChatHandler.php::HipChatHandler.buildcontent",
		Name: "HipChatHandler::buildcontent", QualName: "HipChatHandler.buildcontent",
		Kind: graph.KindMethod, FilePath: "src/HipChatHandler.php", StartLine: 40,
	}
	return anchor, owner, member
}

func TestExploreQualifiedLeafRecoversWithinRankedOwnerFileAcrossStores(t *testing.T) {
	forEachExactNameAnchorStore(t, func(t *testing.T, store graph.Store) {
		anchor, owner, member := qualifiedLeafFixture()
		store.AddNode(owner)
		store.AddNode(member)
		server := &Server{graph: store}
		ordinary := []*rerank.Candidate{{Node: owner}}
		got := server.exploreExactQualifiedAnchorCandidate(
			context.Background(), anchor, ordinary, query.QueryOptions{},
			map[string]struct{}{}, map[string]struct{}{},
		)
		if got == nil || got.Node == nil || got.Node.ID != member.ID {
			t.Fatalf("qualified leaf = %#v, want %q", got, member.ID)
		}
	})
}

func TestExploreQualifiedLeafRequiresRankedOwnerFile(t *testing.T) {
	anchor, owner, member := qualifiedLeafFixture()
	other := &graph.Node{
		ID: "src/Other.php::Other", Name: "Other",
		Kind: graph.KindType, FilePath: "src/Other.php", StartLine: 7,
	}
	server := exactNameAnchorServer(t, owner, member, other)
	for name, ordinary := range map[string][]*rerank.Candidate{
		"no ranking evidence":   nil,
		"different ranked file": {{Node: other}},
	} {
		t.Run(name, func(t *testing.T) {
			got := server.exploreExactQualifiedAnchorCandidate(
				context.Background(), anchor, ordinary, query.QueryOptions{},
				map[string]struct{}{}, map[string]struct{}{},
			)
			if got != nil {
				t.Fatalf("qualified leaf = %#v, want no unranked file recovery", got)
			}
		})
	}
}

func TestExploreQualifiedLeafUsesOverlayOwnerFile(t *testing.T) {
	anchor, owner, member := qualifiedLeafFixture()
	base := graph.New()
	base.AddNode(&graph.Node{
		ID: owner.ID, Name: owner.Name, Kind: owner.Kind,
		FilePath: owner.FilePath, StartLine: owner.StartLine,
	})
	layer := graph.NewOverlayLayer()
	layer.MarkFile(owner.FilePath, false)
	layer.AddNode(owner.FilePath, owner)
	layer.AddNode(member.FilePath, member)
	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(base, layer))
	server := &Server{graph: base}
	got := server.exploreExactQualifiedAnchorCandidate(
		ctx, anchor, []*rerank.Candidate{{Node: owner}}, query.QueryOptions{},
		map[string]struct{}{}, map[string]struct{}{},
	)
	if got == nil || got.Node == nil || got.Node.ID != member.ID {
		t.Fatalf("overlay qualified leaf = %#v, want %q", got, member.ID)
	}
}

func TestExploreQualifiedLeafHasRankedFileBudgetAndNoCorpusScan(t *testing.T) {
	base := graph.New()
	ordinary := make([]*rerank.Candidate, 0, 6)
	for index := 0; index < 6; index++ {
		path := fmt.Sprintf("src/owner%d.go", index)
		owner := &graph.Node{
			ID: path + "::Owner", Name: "Owner",
			Kind: graph.KindType, FilePath: path, StartLine: 5,
		}
		decoy := &graph.Node{
			ID: path + "::decoy", Name: "decoy",
			Kind: graph.KindFunction, FilePath: path, StartLine: 20,
		}
		base.AddNode(owner)
		base.AddNode(decoy)
		ordinary = append(ordinary, &rerank.Candidate{Node: decoy})
	}
	reader := &exactNameAnchorCountingReader{Reader: base}
	ctx := WithOverlayView(
		context.Background(), graph.NewOverlaidView(reader, graph.NewOverlayLayer()),
	)
	server := &Server{graph: base}
	anchor, _ := newExploreSyntacticAnchor("Owner::missingLeaf")
	got := server.exploreExactQualifiedAnchorCandidate(
		ctx, anchor, ordinary, query.QueryOptions{},
		map[string]struct{}{}, map[string]struct{}{},
	)
	if got != nil {
		t.Fatalf("qualified leaf = %#v, want no match", got)
	}
	if len(reader.nameLookups) != 3 {
		t.Fatalf("name lookups = %d (%#v), want member, qualified name, and owner", len(reader.nameLookups), reader.nameLookups)
	}
	if len(reader.fileLookups) != exploreQualifiedLeafMaxFiles {
		t.Fatalf("file lookups = %d (%#v), want cap %d", len(reader.fileLookups), reader.fileLookups, exploreQualifiedLeafMaxFiles)
	}
}
