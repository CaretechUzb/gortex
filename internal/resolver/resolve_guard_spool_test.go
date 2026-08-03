package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestWarmGuardLookupCachePreloadsReceiverType(t *testing.T) {
	g := graph.New()
	source := &graph.Node{
		ID:         "repo::caller",
		Name:       "caller",
		Kind:       graph.KindMethod,
		Language:   "java",
		RepoPrefix: "repo",
	}
	target := &graph.Node{
		ID:         "repo::target",
		Name:       "invoke",
		Kind:       graph.KindMethod,
		Language:   "java",
		RepoPrefix: "repo",
	}
	g.AddNode(source)
	g.AddNode(target)

	r := New(g)
	r.warmGuardLookupCache([]reindexJob{{
		edge: &graph.Edge{
			From: source.ID,
			To:   target.ID,
			Kind: graph.EdgeCalls,
			Meta: map[string]any{"receiver_type": "MissingReceiver"},
		},
		newTo: target.ID,
	}})

	byName := r.nodesByRepoName[source.RepoPrefix]
	if _, warmed := byName["MissingReceiver"]; !warmed {
		t.Fatal("receiver type was not installed as an authoritative guard-cache result")
	}
}
