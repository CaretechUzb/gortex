package mcp

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type panicFileNodeStore struct {
	graph.Store
}

func (panicFileNodeStore) GetFileNodes(string) []*graph.Node {
	panic("creditFileConsumption must not read file nodes")
}

func TestCreditFileConsumptionUsesOnlyReturnedPageState(t *testing.T) {
	session := &sessionState{}
	session.recordLastSearchPage("needle", []*graph.Node{
		{ID: "repo/a.go::A", FilePath: "repo/a.go"},
		{ID: "repo/a.go::B", FilePath: "repo/a.go"},
		{ID: "repo/b.go::C", FilePath: "repo/b.go"},
	})
	server := &Server{
		session: session,
		combo:   newComboManager("", "", ModeHuman),
		graph:   panicFileNodeStore{},
	}

	server.creditFileConsumption(context.Background(), " ./repo/a.go ")
	server.creditFileConsumption(context.Background(), "repo/a.go")

	session.mu.Lock()
	defer session.mu.Unlock()
	_, gotA := session.lastSearch.consumed["repo/a.go::A"]
	_, gotB := session.lastSearch.consumed["repo/a.go::B"]
	_, gotOther := session.lastSearch.consumed["repo/b.go::C"]
	if !gotA || !gotB {
		t.Fatalf("matching page IDs were not credited once: %#v", session.lastSearch.consumed)
	}
	if gotOther {
		t.Fatalf("other-file ID was credited: %#v", session.lastSearch.consumed)
	}
}

func TestCreditFileConsumptionCanceledContextIsNoOp(t *testing.T) {
	session := &sessionState{}
	session.recordLastSearchPage("needle", []*graph.Node{{ID: "repo/a.go::A", FilePath: "repo/a.go"}})
	server := &Server{
		session: session,
		combo:   newComboManager("", "", ModeHuman),
		graph:   panicFileNodeStore{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	server.creditFileConsumption(ctx, "repo/a.go")

	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.lastSearch.consumed) != 0 {
		t.Fatalf("canceled credit mutated state: %#v", session.lastSearch.consumed)
	}
}
