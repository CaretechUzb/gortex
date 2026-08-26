package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// ResolveIncomingForNames is the receipt-exact eviction companion: it must
// reach pending references parked under BOTH stub forms (bare and
// repo-prefixed) for a name no frontier file declares anymore.
func TestResolveIncomingForNamesRebindsBothStubForms(t *testing.T) {
	bare := &graph.Edge{From: "repo/c.go::CallerA", To: graph.UnresolvedMarker + "Target", Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: 3}
	prefixed := &graph.Edge{From: "repo/d.go::CallerB", To: "repo::" + graph.UnresolvedMarker + "Target", Kind: graph.EdgeCalls, FilePath: "repo/d.go", Line: 4}
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/c.go::CallerA", Kind: graph.KindFunction, Name: "CallerA", FilePath: "repo/c.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/d.go::CallerB", Kind: graph.KindFunction, Name: "CallerB", FilePath: "repo/d.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/b.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "repo/b.go", RepoPrefix: "repo", Language: "go"},
	}, []*graph.Edge{bare, prefixed})

	r := New(g)
	stats := r.ResolveIncomingForNames([]string{"Target"}, []string{"repo"})

	if bare.To != "repo/b.go::Target" {
		t.Fatalf("bare-stub edge target = %q, want repo/b.go::Target", bare.To)
	}
	if prefixed.To != "repo/b.go::Target" {
		t.Fatalf("prefixed-stub edge target = %q, want repo/b.go::Target", prefixed.To)
	}
	if stats == nil {
		t.Fatal("nil stats")
	}
}

func TestResolveIncomingForNamesEmptyInputsAreNoOps(t *testing.T) {
	r := New(graph.New())
	if stats := r.ResolveIncomingForNames(nil, []string{"repo"}); stats == nil {
		t.Fatal("nil stats for empty names")
	}
	if stats := r.ResolveIncomingForNames([]string{""}, nil); stats == nil {
		t.Fatal("nil stats for blank name")
	}
}

// Member references park under the wildcard stub forms
// (`unresolved::*.<Name>` and `<repo>::unresolved::*.<Name>`) — the other two
// of graph.UnresolvedNameCandidateIDs' four name-owned forms. The names pass
// must enumerate them too, or member references outside every receipt file
// frontier stay pending forever once the whole-graph fallback is gone.
func TestResolveIncomingForNamesRebindsWildcardMemberStubs(t *testing.T) {
	bare := &graph.Edge{From: "repo/c.go::CallerA", To: graph.UnresolvedMarker + "*.Target", Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: 3}
	prefixed := &graph.Edge{From: "repo/d.go::CallerB", To: "repo::" + graph.UnresolvedMarker + "*.Target", Kind: graph.EdgeCalls, FilePath: "repo/d.go", Line: 4}
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/c.go::CallerA", Kind: graph.KindFunction, Name: "CallerA", FilePath: "repo/c.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/d.go::CallerB", Kind: graph.KindFunction, Name: "CallerB", FilePath: "repo/d.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/b.go::T.Target", Kind: graph.KindMethod, Name: "Target", QualName: "T.Target", FilePath: "repo/b.go", RepoPrefix: "repo", Language: "go"},
	}, []*graph.Edge{bare, prefixed})

	r := New(g)
	r.ResolveIncomingForNames([]string{"Target"}, []string{"repo"})

	if bare.To != "repo/b.go::T.Target" {
		t.Fatalf("bare wildcard-stub edge target = %q, want repo/b.go::T.Target", bare.To)
	}
	if prefixed.To != "repo/b.go::T.Target" {
		t.Fatalf("prefixed wildcard-stub edge target = %q, want repo/b.go::T.Target", prefixed.To)
	}
}

// A wildcard member stub with two same-name method candidates on different
// receivers must stay parked: the names pass runs resolveEdge with its gates
// unchanged, so a still-ambiguous member reference binds no differently than
// it would on any other pass. Paired with the wildcard-rebind test above,
// this pins that enumerating the wildcard forms adds reach, not laxity.
func TestResolveIncomingForNamesAmbiguousWildcardStaysUnresolved(t *testing.T) {
	pending := &graph.Edge{From: "repo/c.go::Caller", To: graph.UnresolvedMarker + "*.Target", Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: 3}
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/c.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/c.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/x/a.go::X.Target", Kind: graph.KindMethod, Name: "Target", QualName: "X.Target", FilePath: "repo/x/a.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/y/b.go::Y.Target", Kind: graph.KindMethod, Name: "Target", QualName: "Y.Target", FilePath: "repo/y/b.go", RepoPrefix: "repo", Language: "go"},
	}, []*graph.Edge{pending})

	r := New(g)
	r.ResolveIncomingForNames([]string{"Target"}, []string{"repo"})

	if !graph.IsUnresolvedTarget(pending.To) {
		t.Fatalf("ambiguous member reference bound to %q, must stay parked", pending.To)
	}
}
