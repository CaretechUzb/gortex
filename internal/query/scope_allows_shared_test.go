package query

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// TestScopeAllows_SharedContractNodes covers the carve-out for global
// rendezvous nodes. A contract node's RepoPrefix / ProjectID record
// which repo minted the global ID first, so narrowing to a sibling that
// merely consumes it must not hide it — while the workspace boundary
// stays hard.
//
// The concrete failure this pins: one Odoo submodule tracked at two
// branches, where `local`'s symbols consumed 1,805 contract nodes minted
// by `local@aurora-redesign`, and a session bound to `local` could not
// see any of them.
func TestScopeAllows_SharedContractNodes(t *testing.T) {
	contract := &graph.Node{
		Kind: graph.KindContract, ID: "ws::pointerup",
		WorkspaceID: "docker-env", ProjectID: "local@aurora", RepoPrefix: "local@aurora",
	}
	symbol := &graph.Node{
		Kind: graph.KindFunction, ID: "local@aurora/a.js::f",
		WorkspaceID: "docker-env", ProjectID: "local@aurora", RepoPrefix: "local@aurora",
	}
	bridge := &graph.Node{
		Kind: graph.KindContractBridge, ID: "bridge::docker-env::local@aurora::ws::pointerup",
		WorkspaceID: "docker-env", ProjectID: "local@aurora", RepoPrefix: "local@aurora",
	}

	repoNarrow := QueryOptions{WorkspaceID: "docker-env", RepoAllow: map[string]bool{"local": true}}
	assert.True(t, repoNarrow.ScopeAllows(contract),
		"a repo narrow must admit a contract minted by a sibling")
	assert.False(t, repoNarrow.ScopeAllows(symbol),
		"a repo narrow must still reject an ordinary symbol of another repo")
	assert.False(t, repoNarrow.ScopeAllows(bridge),
		"a bridge node carries its own scope and stays narrowable")

	projNarrow := QueryOptions{WorkspaceID: "docker-env", ProjectID: "local"}
	assert.True(t, projNarrow.ScopeAllows(contract),
		"the project sub-boundary must admit a shared contract too")
	assert.False(t, projNarrow.ScopeAllows(symbol),
		"the project sub-boundary still rejects another project's symbol")

	// The workspace boundary is the hard rail: sharing does not cross it.
	otherWS := QueryOptions{WorkspaceID: "gortex", RepoAllow: map[string]bool{"gortex": true}}
	assert.False(t, otherWS.ScopeAllows(contract),
		"a shared node must not escape its workspace")
}
