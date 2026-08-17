package mcp

import (
	"context"
	"fmt"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// Neutral persist-middleware shape: several members of one options type rank
// above the type itself; folding promotes the owner ahead of its first
// member without evicting anything.
func newOwnerFoldFixture(t *testing.T) (*Server, *graph.Graph) {
	t.Helper()
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/mw/persist.ts::PersistOptions", Kind: graph.KindType, Name: "PersistOptions", FilePath: "repo/mw/persist.ts", Language: "typescript"},
		{ID: "repo/mw/persist.ts::PersistOptions.serialize", Kind: graph.KindMethod, Name: "serialize", FilePath: "repo/mw/persist.ts", Language: "typescript"},
		{ID: "repo/mw/persist.ts::PersistOptions.hydrate", Kind: graph.KindMethod, Name: "hydrate", FilePath: "repo/mw/persist.ts", Language: "typescript"},
		{ID: "repo/mw/other.ts::unrelated", Kind: graph.KindFunction, Name: "unrelated", FilePath: "repo/mw/other.ts", Language: "typescript"},
	}, []*graph.Edge{
		{From: "repo/mw/persist.ts::PersistOptions.serialize", To: "repo/mw/persist.ts::PersistOptions", Kind: graph.EdgeMemberOf},
		{From: "repo/mw/persist.ts::PersistOptions.hydrate", To: "repo/mw/persist.ts::PersistOptions", Kind: graph.EdgeMemberOf},
	})
	return NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil), g
}

func TestFoldMemberOwnersPromotesSharedOwner(t *testing.T) {
	s, g := newOwnerFoldFixture(t)
	targets := []exploreTarget{
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions.serialize"), score: 1.0},
		{node: g.GetNode("repo/mw/other.ts::unrelated"), score: 0.9},
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions.hydrate"), score: 0.8},
	}
	folded := s.foldMemberOwners(context.Background(), targets, graph.LocalizationNodeScope{})
	if folded[0].node.ID != "repo/mw/persist.ts::PersistOptions" {
		t.Fatalf("owner not promoted ahead of first member: head=%s", folded[0].node.ID)
	}
	if len(folded) != len(targets)+1 {
		t.Fatalf("folding must not evict members: len=%d", len(folded))
	}

	// One member alone must not fold its owner.
	single := []exploreTarget{
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions.serialize"), score: 1.0},
		{node: g.GetNode("repo/mw/other.ts::unrelated"), score: 0.9},
	}
	unfolded := s.foldMemberOwners(context.Background(), single, graph.LocalizationNodeScope{})
	if unfolded[0].node.ID != "repo/mw/persist.ts::PersistOptions.serialize" {
		t.Fatal("a lone member must not trigger folding")
	}

	// An owner already leading its members stays put; no duplicate appears.
	led := []exploreTarget{
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions"), score: 1.0},
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions.serialize"), score: 0.9},
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions.hydrate"), score: 0.8},
	}
	same := s.foldMemberOwners(context.Background(), led, graph.LocalizationNodeScope{})
	if len(same) != 3 || same[0].node.ID != "repo/mw/persist.ts::PersistOptions" {
		t.Fatalf("leading owner must be untouched, got %d entries head=%s", len(same), same[0].node.ID)
	}
}

func TestFoldMemberOwnersPromotesTwoGroupsAtCurrentMemberRanks(t *testing.T) {
	g := graph.New()
	ownerA := &graph.Node{ID: "repo/groups.go::OwnerA", Kind: graph.KindType, Name: "OwnerA", FilePath: "repo/groups.go"}
	ownerB := &graph.Node{ID: "repo/groups.go::OwnerB", Kind: graph.KindType, Name: "OwnerB", FilePath: "repo/groups.go"}
	memberA1 := &graph.Node{ID: "repo/groups.go::OwnerA.a", Kind: graph.KindMethod, Name: "a", FilePath: "repo/groups.go"}
	memberA2 := &graph.Node{ID: "repo/groups.go::OwnerA.b", Kind: graph.KindMethod, Name: "b", FilePath: "repo/groups.go"}
	memberB1 := &graph.Node{ID: "repo/groups.go::OwnerB.a", Kind: graph.KindMethod, Name: "a", FilePath: "repo/groups.go"}
	memberB2 := &graph.Node{ID: "repo/groups.go::OwnerB.b", Kind: graph.KindMethod, Name: "b", FilePath: "repo/groups.go"}
	unrelatedX := &graph.Node{ID: "repo/groups.go::x", Kind: graph.KindFunction, Name: "x", FilePath: "repo/groups.go"}
	unrelatedY := &graph.Node{ID: "repo/groups.go::y", Kind: graph.KindFunction, Name: "y", FilePath: "repo/groups.go"}
	g.AddBatch([]*graph.Node{ownerA, ownerB, memberA1, memberA2, memberB1, memberB2, unrelatedX, unrelatedY}, []*graph.Edge{
		{From: memberA1.ID, To: ownerA.ID, Kind: graph.EdgeMemberOf},
		{From: memberA2.ID, To: ownerA.ID, Kind: graph.EdgeMemberOf},
		{From: memberB1.ID, To: ownerB.ID, Kind: graph.EdgeMemberOf},
		{From: memberB2.ID, To: ownerB.ID, Kind: graph.EdgeMemberOf},
	})
	server := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	targets := []exploreTarget{
		{node: memberA1, score: 1},
		{node: unrelatedX, score: .9},
		{node: memberA2, score: .8},
		{node: memberB1, score: .7},
		{node: unrelatedY, score: .6},
		{node: memberB2, score: .5},
	}
	folded := server.foldMemberOwners(context.Background(), targets, graph.LocalizationNodeScope{})
	want := []string{ownerA.ID, memberA1.ID, unrelatedX.ID, memberA2.ID, ownerB.ID, memberB1.ID, unrelatedY.ID, memberB2.ID}
	if len(folded) != len(want) {
		t.Fatalf("folded len = %d, want %d: %#v", len(folded), len(want), folded)
	}
	for index, id := range want {
		if folded[index].node == nil || folded[index].node.ID != id {
			t.Fatalf("folded[%d] = %#v, want %q; full=%#v", index, folded[index], id, folded)
		}
	}
}

func TestFoldMemberOwnersBoundedRelationCapAndKindFilter(t *testing.T) {
	for _, test := range []struct {
		name     string
		extra    int
		wantFold bool
	}{
		{name: "exact", extra: exploreOwnerFoldRelationLimit - 1, wantFold: true},
		{name: "plus one", extra: exploreOwnerFoldRelationLimit, wantFold: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, g := newOwnerFoldFixture(t)
			members := []string{
				"repo/mw/persist.ts::PersistOptions.serialize",
				"repo/mw/persist.ts::PersistOptions.hydrate",
			}
			for index := 0; index < test.extra; index++ {
				ownerID := fmt.Sprintf("repo/mw/persist.ts::OtherOwner%02d", index)
				g.AddNode(&graph.Node{ID: ownerID, Kind: graph.KindType, Name: fmt.Sprintf("OtherOwner%02d", index), FilePath: "repo/mw/persist.ts"})
				for _, memberID := range members {
					g.AddEdge(&graph.Edge{From: memberID, To: ownerID, Kind: graph.EdgeMemberOf})
				}
			}
			// Unrequested kinds must be filtered before the per-member cap.
			for index := 0; index < 32; index++ {
				for _, memberID := range members {
					g.AddEdge(&graph.Edge{From: memberID, To: fmt.Sprintf("irrelevant-%02d", index), Kind: graph.EdgeReads, Line: index + 1})
				}
			}
			targets := []exploreTarget{
				{node: g.GetNode(members[0]), score: 1},
				{node: g.GetNode(members[1]), score: .9},
			}
			folded := s.foldMemberOwners(context.Background(), targets, graph.LocalizationNodeScope{})
			gotFold := len(folded) == len(targets)+1 && folded[0].node != nil && folded[0].foldedOwner
			if gotFold != test.wantFold {
				t.Fatalf("folded=%v at %d relevant owner rows, want %v: %#v", gotFold, test.extra+1, test.wantFold, folded)
			}
		})
	}
}

func TestFoldMemberOwnersScopesHydratedOwners(t *testing.T) {
	const (
		workspace = "workspace"
		project   = "project"
		repo      = "repo"
	)
	scope := graph.LocalizationNodeScope{
		WorkspaceID: workspace, ProjectID: project,
		RepoAllow: map[string]bool{repo: true}, ExcludeTests: true,
	}
	g := graph.New()
	memberA := &graph.Node{ID: "repo/owner.go::Owner.a", Kind: graph.KindMethod, Name: "a", FilePath: "repo/owner.go", WorkspaceID: workspace, ProjectID: project, RepoPrefix: repo}
	memberB := &graph.Node{ID: "repo/owner.go::Owner.b", Kind: graph.KindMethod, Name: "b", FilePath: "repo/owner.go", WorkspaceID: workspace, ProjectID: project, RepoPrefix: repo}
	allowedOwner := &graph.Node{ID: "repo/owner.go::ZOwner", Kind: graph.KindType, Name: "ZOwner", FilePath: "repo/owner.go", WorkspaceID: workspace, ProjectID: project, RepoPrefix: repo}
	blockedOwners := []*graph.Node{
		{ID: "repo/owner.go::AWorkspace", Kind: graph.KindType, Name: "AWorkspace", FilePath: "repo/owner.go", WorkspaceID: "other", ProjectID: project, RepoPrefix: repo},
		{ID: "repo/owner.go::BProject", Kind: graph.KindType, Name: "BProject", FilePath: "repo/owner.go", WorkspaceID: workspace, ProjectID: "other", RepoPrefix: repo},
		{ID: "repo/owner.go::CRepo", Kind: graph.KindType, Name: "CRepo", FilePath: "repo/owner.go", WorkspaceID: workspace, ProjectID: project, RepoPrefix: "other"},
		{ID: "repo/owner.go::DTest", Kind: graph.KindType, Name: "DTest", FilePath: "repo/owner.go", WorkspaceID: workspace, ProjectID: project, RepoPrefix: repo, Meta: map[string]any{"is_test": true}},
	}
	g.AddBatch([]*graph.Node{memberA, memberB, allowedOwner}, nil)
	for _, owner := range append(blockedOwners, allowedOwner) {
		if owner != allowedOwner {
			g.AddNode(owner)
		}
		g.AddEdge(&graph.Edge{From: memberA.ID, To: owner.ID, Kind: graph.EdgeMemberOf})
		g.AddEdge(&graph.Edge{From: memberB.ID, To: owner.ID, Kind: graph.EdgeMemberOf})
	}
	server := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	targets := []exploreTarget{{node: memberA, score: 1}, {node: memberB, score: .9}}
	folded := server.foldMemberOwners(context.Background(), targets, scope)
	if len(folded) != 3 || folded[0].node.ID != allowedOwner.ID {
		t.Fatalf("scope-filtered owner fold = %#v", folded)
	}
	disjoint := scope
	disjoint.WorkspaceID = "other-workspace"
	unfolded := server.foldMemberOwners(context.Background(), targets, disjoint)
	if len(unfolded) != len(targets) || unfolded[0].node.ID != memberA.ID {
		t.Fatalf("out-of-scope owners changed fold: %#v", unfolded)
	}
}

func TestLimitExploreFoldedTargetsPreservesReservationsAndCap(t *testing.T) {
	s, g := newOwnerFoldFixture(t)
	targets := []exploreTarget{
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions.serialize"), score: 1.0, typedAnchorProjection: true},
		{node: g.GetNode("repo/mw/other.ts::unrelated"), score: 0.9},
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions.hydrate"), score: 0.8, sourceLiteral: true},
	}
	folded := s.foldMemberOwners(context.Background(), targets, graph.LocalizationNodeScope{})
	bounded := limitExploreFoldedTargets("", folded, len(targets), map[string]struct{}{
		targets[0].node.ID: {},
		targets[2].node.ID: {},
	})
	if len(bounded) != len(targets) {
		t.Fatalf("bounded owner fold = %d targets, want %d", len(bounded), len(targets))
	}
	for _, id := range []string{targets[0].node.ID, targets[2].node.ID} {
		found := false
		for _, target := range bounded {
			found = found || target.node != nil && target.node.ID == id
		}
		if !found {
			t.Fatalf("owner fold cap evicted reserved target %s", id)
		}
	}
}

func TestLimitExploreFoldedTargetsDropsSyntheticOwnerWhenEveryDirectTargetIsReserved(t *testing.T) {
	s, g := newOwnerFoldFixture(t)
	targets := []exploreTarget{
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions.serialize"), score: 1.0, typedAnchorProjection: true},
		{node: g.GetNode("repo/mw/other.ts::unrelated"), score: 0.9, sourceLiteral: true},
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions.hydrate"), score: 0.8, typedAnchorProjection: true},
	}
	reserved := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		reserved[target.node.ID] = struct{}{}
	}
	folded := s.foldMemberOwners(context.Background(), targets, graph.LocalizationNodeScope{})
	if len(folded) != len(targets)+1 || !folded[0].foldedOwner {
		t.Fatalf("fixture did not insert a tagged synthetic owner: %#v", folded)
	}
	bounded := limitExploreFoldedTargets(
		"Find the PersistOptions type that owns serialize and hydrate behavior.",
		folded,
		len(targets),
		reserved,
	)
	if len(bounded) != len(targets) {
		t.Fatalf("all-protected fold returned %d targets, want %d", len(bounded), len(targets))
	}
	for _, target := range bounded {
		if target.foldedOwner {
			t.Fatal("synthetic owner survived when every direct target was reserved")
		}
	}
}

func TestLimitExploreFoldedTargetsNeverEvictsDirectWindowForSyntheticOwner(t *testing.T) {
	s, g := newOwnerFoldFixture(t)
	direct := []exploreTarget{
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions.serialize"), score: 1.0},
		{node: g.GetNode("repo/mw/other.ts::unrelated"), score: 0.9},
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions.hydrate"), score: 0.8},
	}
	folded := s.foldMemberOwners(context.Background(), direct, graph.LocalizationNodeScope{})
	if len(folded) != len(direct)+1 || !folded[0].foldedOwner {
		t.Fatalf("fixture did not insert a tagged synthetic owner: %#v", folded)
	}
	bounded := limitExploreFoldedTargets("", folded, len(direct), map[string]struct{}{
		folded[0].node.ID: {}, // even a stale reservation cannot displace direct evidence
	})
	if len(bounded) != len(direct) {
		t.Fatalf("bounded owner fold = %d targets, want %d", len(bounded), len(direct))
	}
	want := make(map[string]struct{}, len(direct))
	for _, target := range direct {
		want[target.node.ID] = struct{}{}
	}
	for _, target := range bounded {
		if target.foldedOwner {
			t.Fatal("post-selection synthetic owner displaced a direct candidate")
		}
		delete(want, target.node.ID)
	}
	if len(want) != 0 {
		t.Fatalf("direct candidate window was not preserved: missing %v", want)
	}
}

func TestLimitExploreFoldedTargetsRetainsDraftSelectedTypeOwnerAtCap(t *testing.T) {
	s, g := newOwnerFoldFixture(t)
	direct := []exploreTarget{
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions.serialize"), score: 1.0},
		{node: g.GetNode("repo/mw/other.ts::unrelated"), score: 0.9},
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions.hydrate"), score: 0.8},
	}
	folded := s.foldMemberOwners(context.Background(), direct, graph.LocalizationNodeScope{})
	bounded := limitExploreFoldedTargets(
		"Find the persist middleware options type that owns serialize and hydrate behavior.",
		folded,
		len(direct),
		nil,
	)
	if len(bounded) != len(direct) {
		t.Fatalf("bounded owner fold = %d targets, want %d", len(bounded), len(direct))
	}
	want := map[string]struct{}{
		"repo/mw/persist.ts::PersistOptions":           {},
		"repo/mw/persist.ts::PersistOptions.serialize": {},
		"repo/mw/persist.ts::PersistOptions.hydrate":   {},
	}
	for _, target := range bounded {
		if target.node != nil {
			delete(want, target.node.ID)
		}
	}
	if len(want) != 0 {
		t.Fatalf("draft-selected type route was not retained: missing %v; got %#v", want, bounded)
	}
	for _, target := range bounded {
		if target.node != nil && target.node.ID == "repo/mw/other.ts::unrelated" {
			t.Fatal("draft-rejected unrelated row survived ahead of selected owner route")
		}
	}
}

func TestExploreFoldedOwnerTaskAlignedRequiresOwnerIdentity(t *testing.T) {
	tests := []struct {
		name  string
		task  string
		owner string
		want  bool
	}{
		{
			name:  "compound identity",
			task:  "Find the persist middleware options type that owns hydration.",
			owner: "PersistOptions",
			want:  true,
		},
		{
			name:  "single exact identity",
			task:  "Find the callable that matches ancestor ignore rules.",
			owner: "Ignore",
			want:  true,
		},
		{
			name:  "generic declaration words",
			task:  "Find the configuration options type that owns this behavior.",
			owner: "ConfigurationOptions",
			want:  false,
		},
		{
			name:  "literal generic compound identity",
			task:  "Find ConfigurationOptions and explain its behavior.",
			owner: "ConfigurationOptions",
			want:  true,
		},
		{
			name:  "partial compound identity",
			task:  "Find number-to-words registration for a locale.",
			owner: "NumberToWordsConverterRegistry",
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner := &graph.Node{Name: tt.owner, Kind: graph.KindType}
			if got := exploreFoldedOwnerTaskAligned(tt.task, owner); got != tt.want {
				t.Fatalf("owner alignment = %v, want %v for task %q and owner %q", got, tt.want, tt.task, tt.owner)
			}
		})
	}
}

func TestLimitExploreFoldedTargetsGenericTypeWordsDoNotRetainUnrelatedOwner(t *testing.T) {
	s, g := newOwnerFoldFixture(t)
	direct := []exploreTarget{
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions.serialize"), score: 1.0},
		{node: g.GetNode("repo/mw/other.ts::unrelated"), score: 0.9},
		{node: g.GetNode("repo/mw/persist.ts::PersistOptions.hydrate"), score: 0.8},
	}
	folded := s.foldMemberOwners(context.Background(), direct, graph.LocalizationNodeScope{})
	bounded := limitExploreFoldedTargets(
		"Find the type config options that own this behavior.",
		folded,
		len(direct),
		nil,
	)
	if len(bounded) != len(direct) {
		t.Fatalf("bounded owner fold = %d targets, want %d", len(bounded), len(direct))
	}
	want := make(map[string]struct{}, len(direct))
	for _, target := range direct {
		want[target.node.ID] = struct{}{}
	}
	for _, target := range bounded {
		if target.foldedOwner {
			t.Fatal("generic type/config/options wording retained an unrelated synthetic owner")
		}
		if target.node != nil {
			delete(want, target.node.ID)
		}
	}
	if len(want) != 0 {
		t.Fatalf("generic owner wording displaced direct candidates: missing %v", want)
	}
}

// A same-file helper named in a wrapper body is promotable into the answer
// draft as a callee of the ranked wrapper — the generic wrapper-to-helper
// shape. Verified covered by the existing draft promotion; pinned so
// selection work cannot regress it.
func TestDraftPromotesWrapperHelperCallee(t *testing.T) {
	task := "case insensitive path lookup returns the wrong file"
	wrapper := &graph.Node{ID: "repo/fs/path.go::findCaseInsensitivePath", Kind: graph.KindFunction,
		Name: "findCaseInsensitivePath", FilePath: "repo/fs/path.go"}
	helper := &graph.Node{ID: "repo/fs/path.go::findCaseInsensitivePathRec", Kind: graph.KindFunction,
		Name: "findCaseInsensitivePathRec", FilePath: "repo/fs/path.go"}
	targets := []exploreTarget{{
		node: wrapper, score: 1.0,
		source:  "func findCaseInsensitivePath(p string) string { return findCaseInsensitivePathRec(p, 0) }",
		callees: []*graph.Node{helper},
	}}
	found := false
	for _, entry := range exploreAnswerDraft(task, targets) {
		if entry.node != nil && entry.node.ID == helper.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("wrapper body helper must be promotable into the answer draft")
	}
}
