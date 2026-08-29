package mcp

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// A repo narrow keyed on registry prefixes can never match a node that
// carries no prefix, so every such predicate has to decide what to do with
// one. The server used to answer that question two different ways: filterNodes
// / filterSubGraph / query.ScopeAllows ADMITTED prefix-less nodes, while the
// explore verb's post-filters REJECTED them. Under a scoped session that made
// explore's resilience ladder relax into its unscoped rungs and still return
// nothing, because the post-filter it re-applied threw the results away again.
//
// These tests pin the single answer: admit.

func TestRepoNarrowAdmitsPrefixlessNodes(t *testing.T) {
	allow := map[string]bool{"gortex": true}

	cases := []struct {
		name   string
		allow  map[string]bool
		prefix string
		want   bool
	}{
		{"in the allow set", allow, "gortex", true},
		{"outside the allow set", allow, "other", false},
		{"prefix-less node is admitted", allow, "", true},
		{"nil allow set is no narrow", nil, "anything", true},
		{"empty allow set is no narrow", map[string]bool{}, "anything", true},
		{"nil allow set admits prefix-less", nil, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := repoNarrowAdmits(tc.allow, tc.prefix); got != tc.want {
				t.Fatalf("repoNarrowAdmits(%v, %q) = %v, want %v", tc.allow, tc.prefix, got, tc.want)
			}
		})
	}
}

// TestRepoNarrowPredicatesAgree is the cross-package fence. filterNodes and
// exploreNodeWithinQueryScope live in different call paths and query.ScopeAllows
// in a different package entirely; a scoped query must get the same verdict
// from all three or a result set changes shape depending on which verb produced
// it.
func TestRepoNarrowPredicatesAgree(t *testing.T) {
	allow := map[string]bool{"gortex": true}
	scope := query.QueryOptions{RepoAllow: allow}

	global := &graph.Node{ID: "dep::go.uber.org/zap::Logger", Name: "Logger"}
	inRepo := &graph.Node{ID: "gortex/a.go::A", Name: "A", FilePath: "gortex/a.go", RepoPrefix: "gortex"}
	otherRepo := &graph.Node{ID: "other/b.go::B", Name: "B", FilePath: "other/b.go", RepoPrefix: "other"}

	for _, tc := range []struct {
		node *graph.Node
		want bool
	}{
		{global, true},
		{inRepo, true},
		{otherRepo, false},
	} {
		gotFilter := len(filterNodes([]*graph.Node{tc.node}, allow)) == 1
		gotExplore := exploreNodeWithinQueryScope(tc.node, scope)
		gotScope := scope.ScopeAllows(tc.node)

		if gotFilter != tc.want || gotExplore != tc.want || gotScope != tc.want {
			t.Errorf("node %q (repo %q): filterNodes=%v exploreNodeWithinQueryScope=%v ScopeAllows=%v, want all %v",
				tc.node.ID, tc.node.RepoPrefix, gotFilter, gotExplore, gotScope, tc.want)
		}
	}
}

// TestRepoNarrowAdmitsNode covers the node-aware form every caller
// holding a *graph.Node must use: it agrees with query.ScopeAllows that
// a shared rendezvous node survives a narrow aimed at another repo.
func TestRepoNarrowAdmitsNode(t *testing.T) {
	allow := map[string]bool{"local": true}
	tests := []struct {
		name string
		node *graph.Node
		want bool
	}{
		{"nil node is rejected", nil, false},
		{"in-scope symbol admitted", &graph.Node{Kind: graph.KindFunction, RepoPrefix: "local"}, true},
		{"out-of-scope symbol rejected", &graph.Node{Kind: graph.KindFunction, RepoPrefix: "local@aurora"}, false},
		{"prefix-less node admitted", &graph.Node{Kind: graph.KindFunction}, true},
		{"sibling-minted contract admitted", &graph.Node{Kind: graph.KindContract, RepoPrefix: "local@aurora"}, true},
		{"out-of-scope bridge rejected", &graph.Node{Kind: graph.KindContractBridge, RepoPrefix: "local@aurora"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := repoNarrowAdmitsNode(allow, tc.node); got != tc.want {
				t.Fatalf("repoNarrowAdmitsNode(%v, %v) = %v, want %v", allow, tc.node, got, tc.want)
			}
		})
	}
}
