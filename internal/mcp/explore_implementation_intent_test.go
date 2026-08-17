package mcp

import (
	"context"
	"fmt"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// Neutral fixture: Storage.load is declared by an interface and implemented
// by DiskStorage and CloudStorage in separate files.
func newStorageFixtureServer(t *testing.T) (*Server, *graph.Graph) {
	t.Helper()
	g := graph.New()
	nodes := []*graph.Node{
		{ID: "repo/storage/storage.go::Storage", Kind: graph.KindInterface, Name: "Storage", FilePath: "repo/storage/storage.go", Language: "go"},
		{ID: "repo/storage/storage.go::Storage.load", Kind: graph.KindMethod, Name: "load", FilePath: "repo/storage/storage.go", Language: "go"},
		{ID: "repo/storage/disk.go::DiskStorage", Kind: graph.KindType, Name: "DiskStorage", FilePath: "repo/storage/disk.go", Language: "go"},
		{ID: "repo/storage/disk.go::DiskStorage.load", Kind: graph.KindMethod, Name: "load", FilePath: "repo/storage/disk.go", Language: "go"},
		{ID: "repo/storage/cloud.go::CloudStorage", Kind: graph.KindType, Name: "CloudStorage", FilePath: "repo/storage/cloud.go", Language: "go"},
		{ID: "repo/storage/cloud.go::CloudStorage.load", Kind: graph.KindMethod, Name: "load", FilePath: "repo/storage/cloud.go", Language: "go"},
	}
	edges := []*graph.Edge{
		{From: "repo/storage/storage.go::Storage.load", To: "repo/storage/storage.go::Storage", Kind: graph.EdgeMemberOf},
		{From: "repo/storage/disk.go::DiskStorage", To: "repo/storage/storage.go::Storage", Kind: graph.EdgeImplements},
		{From: "repo/storage/cloud.go::CloudStorage", To: "repo/storage/storage.go::Storage", Kind: graph.EdgeImplements},
		{From: "repo/storage/disk.go::DiskStorage.load", To: "repo/storage/disk.go::DiskStorage", Kind: graph.EdgeMemberOf},
		{From: "repo/storage/cloud.go::CloudStorage.load", To: "repo/storage/cloud.go::CloudStorage", Kind: graph.EdgeMemberOf},
	}
	g.AddBatch(nodes, edges)
	return NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil), g
}

func TestExploreImplementationIntentVocabulary(t *testing.T) {
	positives := []string{
		"where is the concrete implementation of Storage.load",
		"find the classes that implement Storage",
		"which subclasses override load",
	}
	for _, task := range positives {
		if !exploreImplementationIntent(task) {
			t.Fatalf("intent not detected: %q", task)
		}
	}
	if exploreImplementationIntent("where is Storage.load declared") {
		t.Fatal("declaration query must not carry implementation intent")
	}
}

// An interface seed expands to its implementing types; an interface-member
// seed expands to the same-name concrete members. The abstract seed is never
// evicted and new files are bounded.
func TestExpandImplementationTargets(t *testing.T) {
	s, g := newStorageFixtureServer(t)
	seed := exploreTarget{node: g.GetNode("repo/storage/storage.go::Storage.load"), score: 1.0}

	expanded := s.expandImplementationTargets(context.Background(), []exploreTarget{seed}, graph.LocalizationNodeScope{})
	if expanded[0].node.ID != "repo/storage/storage.go::Storage.load" {
		t.Fatal("abstract seed must stay at its rank")
	}
	got := map[string]bool{}
	for _, tgt := range expanded[1:] {
		got[tgt.node.ID] = true
	}
	if !got["repo/storage/disk.go::DiskStorage.load"] || !got["repo/storage/cloud.go::CloudStorage.load"] {
		t.Fatalf("member seed must expand to same-name concrete members, got %v", got)
	}

	ifaceSeed := exploreTarget{node: g.GetNode("repo/storage/storage.go::Storage"), score: 1.0}
	expanded = s.expandImplementationTargets(context.Background(), []exploreTarget{ifaceSeed}, graph.LocalizationNodeScope{})
	got = map[string]bool{}
	for _, tgt := range expanded[1:] {
		got[tgt.node.ID] = true
	}
	if !got["repo/storage/disk.go::DiskStorage"] || !got["repo/storage/cloud.go::CloudStorage"] {
		t.Fatalf("interface seed must expand to implementing types, got %v", got)
	}
}

// Only-abstract evidence blocks answer_ready for implementation intent;
// the presence of one concrete implementor unblocks it, and non-intent
// queries are never touched.
func TestImplementationAnswerBlockedOnAbstractOnlyHead(t *testing.T) {
	s, g := newStorageFixtureServer(t)
	eng := s.engineFor(context.Background())
	task := "find the concrete implementation of Storage.load"
	abstractOnly := []exploreTarget{
		{node: g.GetNode("repo/storage/storage.go::Storage.load")},
		{node: g.GetNode("repo/storage/storage.go::Storage")},
	}
	if !exploreImplementationAnswerBlocked(context.Background(), task, abstractOnly, eng.Reader(), graph.LocalizationNodeScope{}) {
		t.Fatal("abstract-only head must block answer_ready for implementation intent")
	}
	withConcrete := append(abstractOnly, exploreTarget{node: g.GetNode("repo/storage/disk.go::DiskStorage.load")})
	if exploreImplementationAnswerBlocked(context.Background(), task, withConcrete, eng.Reader(), graph.LocalizationNodeScope{}) {
		t.Fatal("a concrete implementor in the head must unblock answer_ready")
	}
	if exploreImplementationAnswerBlocked(context.Background(), "where is Storage.load declared", abstractOnly, eng.Reader(), graph.LocalizationNodeScope{}) {
		t.Fatal("non-intent queries must be untouched")
	}
}

func TestExpandImplementationTargetsBoundedRelationCaps(t *testing.T) {
	t.Run("abstract owner exact and plus one", func(t *testing.T) {
		for _, test := range []struct {
			name       string
			extra      int
			wantExpand bool
		}{
			{name: "exact", extra: exploreImplOwnerRelationLimit - 1, wantExpand: true},
			{name: "plus one", extra: exploreImplOwnerRelationLimit, wantExpand: false},
		} {
			t.Run(test.name, func(t *testing.T) {
				s, g := newStorageFixtureServer(t)
				for index := 0; index < test.extra; index++ {
					id := fmt.Sprintf("repo/storage/storage.go::Owner%02d", index)
					g.AddNode(&graph.Node{ID: id, Kind: graph.KindType, Name: fmt.Sprintf("Owner%02d", index), FilePath: "repo/storage/storage.go"})
					g.AddEdge(&graph.Edge{From: "repo/storage/storage.go::Storage.load", To: id, Kind: graph.EdgeMemberOf})
				}
				for index := 0; index < 32; index++ {
					g.AddEdge(&graph.Edge{From: "repo/storage/storage.go::Storage.load", To: fmt.Sprintf("irrelevant-owner-%02d", index), Kind: graph.EdgeReads, Line: index + 1})
				}
				seed := exploreTarget{node: g.GetNode("repo/storage/storage.go::Storage.load"), score: 1}
				expanded := s.expandImplementationTargets(context.Background(), []exploreTarget{seed}, graph.LocalizationNodeScope{})
				if got := len(expanded) > 1; got != test.wantExpand {
					t.Fatalf("expanded=%v at %d owner relations, want %v", got, test.extra+1, test.wantExpand)
				}
			})
		}
	})

	t.Run("implementors exact and plus one", func(t *testing.T) {
		for _, test := range []struct {
			name       string
			extra      int
			wantExpand bool
		}{
			{name: "exact", extra: exploreImplImplementorLimit - 2, wantExpand: true},
			{name: "plus one", extra: exploreImplImplementorLimit - 1, wantExpand: false},
		} {
			t.Run(test.name, func(t *testing.T) {
				s, g := newStorageFixtureServer(t)
				for index := 0; index < test.extra; index++ {
					id := fmt.Sprintf("repo/storage/impl.go::Impl%02d", index)
					g.AddNode(&graph.Node{ID: id, Kind: graph.KindType, Name: fmt.Sprintf("Impl%02d", index), FilePath: "repo/storage/impl.go"})
					g.AddEdge(&graph.Edge{From: id, To: "repo/storage/storage.go::Storage", Kind: graph.EdgeImplements})
				}
				for index := 0; index < 32; index++ {
					g.AddEdge(&graph.Edge{From: fmt.Sprintf("irrelevant-impl-%02d", index), To: "repo/storage/storage.go::Storage", Kind: graph.EdgeReads, Line: index + 1})
				}
				seed := exploreTarget{node: g.GetNode("repo/storage/storage.go::Storage"), score: 1}
				expanded := s.expandImplementationTargets(context.Background(), []exploreTarget{seed}, graph.LocalizationNodeScope{})
				if got := len(expanded) > 1; got != test.wantExpand {
					t.Fatalf("expanded=%v at %d implementors, want %v", got, test.extra+2, test.wantExpand)
				}
			})
		}
	})

	t.Run("member refinement exact and plus one", func(t *testing.T) {
		for _, test := range []struct {
			name       string
			members    int
			wantMember bool
		}{
			{name: "exact", members: exploreImplMemberRefinementLimit, wantMember: true},
			{name: "plus one", members: exploreImplMemberRefinementLimit + 1, wantMember: false},
		} {
			t.Run(test.name, func(t *testing.T) {
				g := graph.New()
				ownerID := "repo/api/api.go::API"
				seedID := "repo/api/api.go::API.run"
				implID := "repo/impl/impl.go::Concrete"
				g.AddBatch([]*graph.Node{
					{ID: ownerID, Kind: graph.KindInterface, Name: "API", FilePath: "repo/api/api.go"},
					{ID: seedID, Kind: graph.KindMethod, Name: "run", FilePath: "repo/api/api.go"},
					{ID: implID, Kind: graph.KindType, Name: "Concrete", FilePath: "repo/impl/impl.go"},
				}, []*graph.Edge{
					{From: seedID, To: ownerID, Kind: graph.EdgeMemberOf},
					{From: implID, To: ownerID, Kind: graph.EdgeImplements},
				})
				memberID := "repo/impl/impl.go::Concrete.000-run"
				for index := 0; index < test.members; index++ {
					id := fmt.Sprintf("repo/impl/impl.go::Concrete.member%03d", index)
					name := fmt.Sprintf("member%03d", index)
					if index == 0 {
						id, name = memberID, "run"
					}
					g.AddNode(&graph.Node{ID: id, Kind: graph.KindMethod, Name: name, FilePath: "repo/impl/impl.go"})
					g.AddEdge(&graph.Edge{From: id, To: implID, Kind: graph.EdgeMemberOf})
				}
				for index := 0; index < 32; index++ {
					g.AddEdge(&graph.Edge{From: fmt.Sprintf("irrelevant-member-%02d", index), To: implID, Kind: graph.EdgeReads, Line: index + 1})
				}
				s := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
				seed := exploreTarget{node: g.GetNode(seedID), score: 1}
				expanded := s.expandImplementationTargets(context.Background(), []exploreTarget{seed}, graph.LocalizationNodeScope{})
				if len(expanded) != 2 {
					t.Fatalf("expanded length=%d, want 2", len(expanded))
				}
				gotMember := expanded[1].node.ID == memberID
				if gotMember != test.wantMember {
					t.Fatalf("refined member=%v at %d rows, want %v (node=%s)", gotMember, test.members, test.wantMember, expanded[1].node.ID)
				}
			})
		}
	})
}

func TestExpandImplementationTargetsAppliesScopeBeforeAdmissionQuotas(t *testing.T) {
	const (
		workspace = "allowed-workspace"
		project   = "allowed-project"
		repo      = "allowed-repo"
	)
	scope := graph.LocalizationNodeScope{
		WorkspaceID:  workspace,
		ProjectID:    project,
		RepoAllow:    map[string]bool{repo: true},
		ExcludeTests: true,
	}
	g := graph.New()
	owner := &graph.Node{
		ID: "repo/api/api.go::API", Kind: graph.KindInterface, Name: "API", FilePath: "repo/api/api.go",
		WorkspaceID: workspace, ProjectID: project, RepoPrefix: repo,
	}
	g.AddNode(owner)
	blocked := []*graph.Node{
		{ID: "repo/00-workspace/impl.go::Impl", Kind: graph.KindType, Name: "BlockedWorkspace", FilePath: "repo/00-workspace/impl.go", WorkspaceID: "other", ProjectID: project, RepoPrefix: repo},
		{ID: "repo/01-project/impl.go::Impl", Kind: graph.KindType, Name: "BlockedProject", FilePath: "repo/01-project/impl.go", WorkspaceID: workspace, ProjectID: "other", RepoPrefix: repo},
		{ID: "repo/02-repo/impl.go::Impl", Kind: graph.KindType, Name: "BlockedRepo", FilePath: "repo/02-repo/impl.go", WorkspaceID: workspace, ProjectID: project, RepoPrefix: "other-repo"},
		{ID: "repo/03-test/impl.go::Impl", Kind: graph.KindType, Name: "BlockedTest", FilePath: "repo/03-test/impl.go", WorkspaceID: workspace, ProjectID: project, RepoPrefix: repo, Meta: map[string]any{"is_test": true}},
	}
	for _, node := range blocked {
		g.AddNode(node)
		g.AddEdge(&graph.Edge{From: node.ID, To: owner.ID, Kind: graph.EdgeImplements})
	}
	allowed := []*graph.Node{
		{ID: "repo/10-allowed-a/impl.go::Impl", Kind: graph.KindType, Name: "AllowedA", FilePath: "repo/10-allowed-a/impl.go", WorkspaceID: workspace, ProjectID: project, RepoPrefix: repo},
		{ID: "repo/11-allowed-b/impl.go::Impl", Kind: graph.KindType, Name: "AllowedB", FilePath: "repo/11-allowed-b/impl.go", WorkspaceID: workspace, ProjectID: project, RepoPrefix: repo},
	}
	for _, node := range allowed {
		g.AddNode(node)
		g.AddEdge(&graph.Edge{From: node.ID, To: owner.ID, Kind: graph.EdgeImplements})
	}
	s := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
	expanded := s.expandImplementationTargets(context.Background(), []exploreTarget{{node: owner, score: 1}}, scope)
	got := map[string]bool{}
	for _, target := range expanded {
		if target.node != nil {
			got[target.node.ID] = true
		}
	}
	for _, node := range allowed {
		if !got[node.ID] {
			t.Fatalf("scope-allowed implementation %s was charged behind blocked rows: %v", node.ID, got)
		}
	}
	for _, node := range blocked {
		if got[node.ID] {
			t.Fatalf("out-of-scope implementation was admitted: %s", node.ID)
		}
	}
}

func TestImplementationRecoveryScopesHydratedOwnersAndMembers(t *testing.T) {
	const (
		workspace = "workspace"
		project   = "project"
		repo      = "repo"
	)
	scope := graph.LocalizationNodeScope{
		WorkspaceID: workspace, ProjectID: project,
		RepoAllow: map[string]bool{repo: true}, ExcludeTests: true,
	}
	allowedNode := func(id string, kind graph.NodeKind, name string) *graph.Node {
		return &graph.Node{ID: id, Kind: kind, Name: name, FilePath: graph.IDFile(id), WorkspaceID: workspace, ProjectID: project, RepoPrefix: repo}
	}

	t.Run("owner classification", func(t *testing.T) {
		g := graph.New()
		seed := allowedNode("repo/api.go::API.run", graph.KindMethod, "run")
		allowedOwner := allowedNode("repo/api.go::ZAllowedAPI", graph.KindInterface, "AllowedAPI")
		implementation := allowedNode("repo/impl.go::Implementation", graph.KindType, "Implementation")
		blockedOwners := []*graph.Node{
			{ID: "repo/api.go::AWorkspace", Kind: graph.KindInterface, Name: "AWorkspace", FilePath: "repo/api.go", WorkspaceID: "other", ProjectID: project, RepoPrefix: repo},
			{ID: "repo/api.go::BProject", Kind: graph.KindInterface, Name: "BProject", FilePath: "repo/api.go", WorkspaceID: workspace, ProjectID: "other", RepoPrefix: repo},
			{ID: "repo/api.go::CRepo", Kind: graph.KindInterface, Name: "CRepo", FilePath: "repo/api.go", WorkspaceID: workspace, ProjectID: project, RepoPrefix: "other"},
			{ID: "repo/api.go::DTest", Kind: graph.KindInterface, Name: "DTest", FilePath: "repo/api.go", WorkspaceID: workspace, ProjectID: project, RepoPrefix: repo, Meta: map[string]any{"is_test": true}},
		}
		g.AddBatch([]*graph.Node{seed, allowedOwner, implementation}, []*graph.Edge{{From: implementation.ID, To: allowedOwner.ID, Kind: graph.EdgeImplements}})
		for _, owner := range blockedOwners {
			g.AddNode(owner)
			g.AddEdge(&graph.Edge{From: seed.ID, To: owner.ID, Kind: graph.EdgeMemberOf})
		}
		g.AddEdge(&graph.Edge{From: seed.ID, To: allowedOwner.ID, Kind: graph.EdgeMemberOf})
		server := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
		expanded := server.expandImplementationTargets(context.Background(), []exploreTarget{{node: seed, score: 1}}, scope)
		if len(expanded) != 2 || expanded[1].node.ID != implementation.ID {
			t.Fatalf("scope-filtered owner expansion = %#v", expanded)
		}
		if !exploreImplementationAnswerBlocked(context.Background(), "find implementation", []exploreTarget{{node: seed}}, g, scope) {
			t.Fatal("scope-allowed interface owner must classify the method as abstract")
		}
		disjoint := scope
		disjoint.WorkspaceID = "other-workspace"
		if exploreImplementationAnswerBlocked(context.Background(), "find implementation", []exploreTarget{{node: seed}}, g, disjoint) {
			t.Fatal("only out-of-scope interface owners must not classify the method as abstract")
		}
	})

	t.Run("same-name member refinement", func(t *testing.T) {
		g := graph.New()
		owner := allowedNode("repo/api.go::API", graph.KindInterface, "API")
		seed := allowedNode("repo/api.go::API.run", graph.KindMethod, "run")
		implementation := allowedNode("repo/impl.go::Implementation", graph.KindType, "Implementation")
		allowedMember := allowedNode("repo/impl.go::Implementation.zrun", graph.KindMethod, "run")
		blockedMembers := []*graph.Node{
			{ID: "repo/impl.go::Implementation.arun", Kind: graph.KindMethod, Name: "run", FilePath: "repo/impl.go", WorkspaceID: "other", ProjectID: project, RepoPrefix: repo},
			{ID: "repo/impl.go::Implementation.brun", Kind: graph.KindMethod, Name: "run", FilePath: "repo/impl.go", WorkspaceID: workspace, ProjectID: "other", RepoPrefix: repo},
			{ID: "repo/impl.go::Implementation.crun", Kind: graph.KindMethod, Name: "run", FilePath: "repo/impl.go", WorkspaceID: workspace, ProjectID: project, RepoPrefix: "other"},
			{ID: "repo/impl.go::Implementation.drun", Kind: graph.KindMethod, Name: "run", FilePath: "repo/impl.go", WorkspaceID: workspace, ProjectID: project, RepoPrefix: repo, Meta: map[string]any{"is_test": true}},
		}
		g.AddBatch([]*graph.Node{owner, seed, implementation, allowedMember}, []*graph.Edge{
			{From: seed.ID, To: owner.ID, Kind: graph.EdgeMemberOf},
			{From: implementation.ID, To: owner.ID, Kind: graph.EdgeImplements},
			{From: allowedMember.ID, To: implementation.ID, Kind: graph.EdgeMemberOf},
		})
		for _, member := range blockedMembers {
			g.AddNode(member)
			g.AddEdge(&graph.Edge{From: member.ID, To: implementation.ID, Kind: graph.EdgeMemberOf})
		}
		server := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
		expanded := server.expandImplementationTargets(context.Background(), []exploreTarget{{node: seed, score: 1}}, scope)
		if len(expanded) != 2 || expanded[1].node.ID != allowedMember.ID {
			t.Fatalf("scope-filtered member refinement = %#v", expanded)
		}
	})
}
